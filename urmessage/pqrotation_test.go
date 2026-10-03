// pq_secret ROTATES PER EPOCH, AND THE MEMBER A COMMIT REMOVES DOES NOT GET THE NEXT ONE.
//
// WHAT THIS FILE MEASURES, and why each case is written as a PROPERTY rather than as a count.
// Ledger item 243 ruled pq_secret a group-lifetime value "on the explicit condition that rotating
// it is a prerequisite of shipping REMOVAL"; item 251's rulings 36-40 are that condition coming
// due, and the thing that has to be TRUE at the end of it is one sentence: every member a commit
// leaves in the group derives the same storage_root for the epoch it opens, and the member it
// takes out does not. Everything else here -- the table, the wrap, the three sentinels -- is
// machinery in service of that sentence, and a suite that counted the machinery would pass over a
// rotation that delivered the wrong secret to everybody equally.
//
// WHAT IS REAL. Every member is a [crossProcessDevice]: this package's own deviceIdentity over a
// durable store, the shipped messagegroup engine, a real GroupSession, and -- which is new and is
// what makes a wrap openable at all -- the X-Wing seed under the public half its leaf publishes
// (S2-26, sdk 48ee76e). Commits are the seam's by-value arms on real handles. The fan-out is
// built by the production functions [Group.wrapTargetsAtLocked] and [Group.sealEpochWrapLocked],
// and the receive leg is [Group.openPageLocked] driving [Group.ingestWrapLocked] and
// [Group.ingestCommitLocked], exactly as [Group.Receive] does below the fetch.
//
// WHAT IS NOT HERE IS THE SERVER, which is the cp3b module's. What that costs is stated rather
// than glossed: the ORDER of the records on the wire is a server-side fact -- ruling 37 has the
// wraps submitted at epoch n, before the commit, because a write is accepted only at the current
// epoch -- and no page assembled here can refuse a wrap for being late. cp3b's
// TestTheEpochFanOutIsSubmittedBeforeTheCommitAndCarriesTheEpochItOpens is where that half is
// measured, against a real server that answers REASON_EPOCH_STALE.
package urmessage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/protocol"
)

// ── the world ────────────────────────────────────────────────────────────────────────────────

type rotMember struct {
	name    string
	root    string
	dev     *crossProcessDevice
	handle  messagegroup.GroupHandle
	session *messagegroup.GroupSession
	group   *Group
	leaf    uint32
}

type rotWorld struct {
	t              *testing.T
	root           string
	groupId        []byte
	groupHandleKey []byte
	founding       []byte
	members        map[string]*rotMember
	order          []string
	nextRecordId   uint64
}

// newRotWorld founds a group with names[0] and admits every other name in ONE commit, so every
// member stands at epoch 1 holding the founding pq_secret -- the shape every live group has, and
// the shape the compatibility path is defined on.
func newRotWorld(t *testing.T, names ...string) *rotWorld {
	t.Helper()
	world := &rotWorld{
		t:            t,
		root:         t.TempDir(),
		groupId:      make([]byte, GroupIdBytes),
		members:      map[string]*rotMember{},
		nextRecordId: 1,
	}
	if _, err := rand.Read(world.groupId); err != nil {
		t.Fatalf("drawing a group id: %v", err)
	}
	founding, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("pq_secret: %v", err)
	}
	world.founding = founding

	founder := world.device(names[0])
	founderHandle := founder.createGroup(t, world.groupId)
	t.Cleanup(func() { founderHandle.Close() })
	mlsSecret, err := founderHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	world.groupHandleKey = messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, founding))

	joiners := []*crossProcessDevice{}
	keyPackages := [][]byte{}
	for _, name := range names[1:] {
		joiner := world.device(name)
		keyPackage, err := joiner.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("%s's key package: %v", name, err)
		}
		joiners = append(joiners, joiner)
		keyPackages = append(keyPackages, keyPackage)
	}
	_, welcome, ratchetTree, err := founderHandle.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("the founding CommitAdd: %v", err)
	}
	if err := founderHandle.MergePendingCommit(); err != nil {
		t.Fatalf("the founding MergePendingCommit: %v", err)
	}
	world.enroll(names[0], founder, founderHandle)
	for at, joiner := range joiners {
		handle, err := joiner.engine.JoinFromWelcome(welcome, ratchetTree)
		if err != nil {
			t.Fatalf("%s's JoinFromWelcome: %v", names[at+1], err)
		}
		t.Cleanup(func() { handle.Close() })
		world.enroll(names[at+1], joiner, handle)
	}
	return world
}

func (self *rotWorld) device(name string) *crossProcessDevice {
	self.t.Helper()
	dev := openCrossProcessDevice(self.t, filepath.Join(self.root, name))
	self.t.Cleanup(dev.close)
	return dev
}

func (self *rotWorld) rootOf(name string) string {
	return filepath.Join(self.root, name)
}

// enroll gives one member a session and a receiving [Group]. THE WRAP SEED IS ON THE DEVICE, and
// that is the one field this world sets that the role world does not: without it every
// [Device.openWrapToOwnLeaf] answers [ErrNoDeviceWrapKey] and every case here would measure a
// device that cannot open its own wrap rather than a wrap that is wrong.
func (self *rotWorld) enroll(name string, dev *crossProcessDevice, handle messagegroup.GroupHandle) *rotMember {
	self.t.Helper()
	member := self.enrollAt(name, dev, handle, self.founding)
	// AND THE FOUNDING COHORT'S OWN STREAM FLOOR IS KNOWN. Every member of a world built by
	// [newRotWorld] is named in the FOUNDING commit, so no leaf here has ever been occupied
	// by anybody else and there is no previous occupant's claim to hold a floor against. A
	// member admitted LATER, through [rotWorld.admit], is the one that cannot say that.
	member.group.ownFloorHeld = true
	return member
}

// enrollAt is enroll with the ONE pq_secret row this member starts life holding, and that row is
// the whole of what separates a founder from a member admitted later. [Device.Join] files exactly
// one row -- `pqSecrets: {handle.Epoch(): invite.PqSecret}` -- so a device admitted at epoch k
// knows nothing of the values drawn below it, which is the subject of ledger ruling 43's first
// residual. [rotWorld.admit] is the door that uses it; `enroll` passes the founding value because
// every member of a world built by [newRotWorld] is named in the founding commit.
func (self *rotWorld) enrollAt(name string, dev *crossProcessDevice, handle messagegroup.GroupHandle,
	pqSecret []byte) *rotMember {

	self.t.Helper()
	session := newCrossProcessSession(self.t, handle, pqSecret, self.groupHandleKey, dev.reserver, name+"'s nonce")
	self.t.Cleanup(func() { session.Close() })
	group := &Group{
		device: &Device{
			stateStore:  dev.store,
			engine:      dev.engine,
			reserver:    dev.reserver,
			identityPub: append([]byte(nil), dev.identityPub...),
			wrapSeed:    dev.wrapSeed,
			nowMs:       func() int64 { return time.Now().UnixMilli() },
			random:      rand.Reader,
			groups:      map[string]*Group{},
		},
		id:             append([]byte(nil), self.groupId...),
		handle:         handle,
		groupHandleKey: self.groupHandleKey,
		pqSecrets:      map[uint64][]byte{handle.Epoch(): append([]byte(nil), pqSecret...)},
		session:        session,
		epoch:          handle.Epoch(),
		opened:         true,
		reconciled:     true,
		// FALSE, BECAUSE THIS DOOR MODELS [Device.Join] AND A JOINER CANNOT KNOW WHOSE LEAF IT
		// LANDED ON. [rotWorld.enroll] raises it for the FOUNDING cohort, whose leaves were never
		// anybody else's; a member admitted through [rotWorld.admit] keeps it false, which is
		// what production does and what [Group.ownFloorHeld] is for.
		ownFloorHeld: false,
	}
	group.initTables()
	if err := session.InstallPastEpochLoader(group.device.pastEpochLoader(group.id)); err != nil {
		self.t.Fatalf("%s's past epoch loader: %v", name, err)
	}
	if err := group.device.persistGroup(group.groupRecordLocked(true)); err != nil {
		self.t.Fatalf("%s's group record: %v", name, err)
	}
	member := &rotMember{name: name, root: self.rootOf(name), dev: dev, handle: handle,
		session: session, group: group, leaf: handle.OwnLeafIndex()}
	self.members[name] = member
	self.order = append(self.order, name)
	return member
}

func (self *rotWorld) member(name string) *rotMember {
	self.t.Helper()
	held, found := self.members[name]
	if !found {
		self.t.Fatalf("no member named %q", name)
	}
	return held
}

// rotation is one publication: the wrap rows in the order they go on the wire, then the commit.
//
// `wraps` STAYS INDEX-PARALLEL TO `targets` and a decoy never goes in it. A case that omits one
// member's wrap finds it by `targets[at].leaf`, so a list that sometimes holds an extra row would
// silently omit the wrong leaf; [rotBend.decoy]'s rows live in `decoys` for that reason alone.
type rotation struct {
	wraps    []*sealed
	decoys   []*sealed
	commit   *sealed
	opens    uint64
	pqSecret []byte
	targets  []wrapTarget
}

// rotBend is one leaf's wrap built wrong on purpose: sealed to a key nobody holds, carrying a
// secret this epoch was not opened with, or landed BESIDE the honest one. All three are states the
// field produces -- a committer that encapsulated to the wrong leaf, a committer that lost its CAS
// race, and a committer that lost its CAS race while the WINNER's wrap arrived too -- and all are
// built by the production sealer rather than by editing octets, so what each case measures is the
// OPENER.
type rotBend struct {
	leaf        uint32
	toAStranger bool
	payload     []byte
	// decoy ADDS the bent row instead of replacing the honest one, which is the only way to put
	// TWO candidates for one epoch in front of one device. It is the reading [Stats.WrapOrphaned]'s
	// own doc calls healthy -- "two committers raced, this device opened both wraps and used the
	// winner's" -- and until the counter moved it was the reading that could never show, because
	// the orphan total was added only on the arm where NO candidate won.
	decoy bool
}

// page is the publication as a receiver meets it, wraps first -- ruling 37's order. The decoys go
// FIRST, because a loser's fan-out is written before the winner's commit is accepted and a walk in
// record-id order meets it first; putting them last would test the easy direction.
func (self *rotation) page() []*sealed {
	page := append([]*sealed{}, self.decoys...)
	page = append(page, self.wraps...)
	return append(page, self.commit)
}

// rotate runs [Group.publishCommitLocked]'s (2a) through (4) with no server in between: draw the
// epoch's own pq_secret, enumerate the targets off the live tree minus what the commit removes,
// seal one wrap per target PRE-MERGE, seal the commit that announces the epoch, then merge, file
// and advance.
//
// IT CALLS THE PRODUCTION FUNCTIONS FOR EVERY STEP THAT HAS ONE. What is re-spelled here is only
// what [Group.submitLocked] would have done -- handing a record to a transport and numbering it --
// because no transport exists in this package's tests.
//
// AND IT TAKES NO `removing` VECTOR, WHICH IS THE POINT AS OF 2026-09-25. Until ledger item 257's
// ruling 51 was consumed this harness passed the removed leaves down beside the commit, exactly as
// production's arms did -- so every case here that held a removal out of a fan-out was holding a
// fact about what THE CASE passed. The exclusion is now [Group.wrapTargetsAtLocked]'s own read off
// the staged commit, so a case that builds a removing commit and asserts the removed leaf is not a
// target is measuring the derivation. A `removing` parameter restored here would make every such
// case vacuous again without changing a line of production code, which is why there is none.
func (self *rotWorld) rotate(committer *rotMember,
	arm func() ([]byte, []byte, []byte, error), bends ...rotBend) *rotation {

	self.t.Helper()
	group := committer.group
	commit, _, _, err := arm()
	if err != nil {
		self.t.Fatalf("%s's commit: %v", committer.name, err)
	}
	pending, err := committer.handle.PendingEpoch()
	if err != nil {
		self.t.Fatalf("%s's pending epoch: %v", committer.name, err)
	}
	// THE PRODUCTION DECISION, THROUGH THE PRODUCTION CALL. The draw, the target enumeration
	// and the wrap records are [Group.stageEpochRotationLocked]'s and are not re-spelled here:
	// a harness that drew its own secret measures its own arithmetic, and it did -- a mutant
	// that replaced the draw with the group's existing secret passed this whole file.
	staged, err := group.stageEpochRotationLocked(pending)
	if err != nil {
		self.t.Fatalf("%s's staged rotation: %v", committer.name, err)
	}
	pqNext, targets := staged.pqSecret, staged.targets
	newMlsSecret, err := committer.handle.PendingExport(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's staged exporter: %v", committer.name, err)
	}
	newRoot := messagegroup.StorageRoot(newMlsSecret, pqNext)
	writeKey, readKey := message.WriteKey(newRoot), message.ReadKey(newRoot)

	one := &rotation{opens: pending.Epoch, pqSecret: pqNext, targets: targets}
	// THE BENDS ARE APPLIED BY RE-SEALING ONE ROW, WHILE THE COMMITTER IS STILL AT THE OLD
	// EPOCH, which is forced rather than tidy: a GroupSession seals at the epoch it was
	// constructed over, so a row rebuilt after the advance below would carry epoch n+1 in its
	// header and every receiver would refuse it as a record from an epoch it is not in -- a
	// different failure from the one each case is about. The UNBENT rows are production's own,
	// sealed by [Group.stageEpochRotationLocked] and not rebuilt here.
	for at, target := range targets {
		record := staged.wraps[at]
		for _, bend := range bends {
			if bend.leaf != target.leaf {
				continue
			}
			bent := target
			payload := pqNext
			if bend.toAStranger {
				stranger, err := messagegroup.XwingGenerateKey(rand.Reader)
				if err != nil {
					self.t.Fatalf("a stranger's x-wing key: %v", err)
				}
				bent.xwingPub = stranger.Public().Bytes()
			}
			if bend.payload != nil {
				payload = bend.payload
			}
			replacement, err := group.sealEpochWrapLocked(pending.Epoch, bent, payload)
			if err != nil {
				self.t.Fatalf("%s's bent wrap for leaf %d: %v", committer.name, target.leaf, err)
			}
			if bend.decoy {
				// THE LOSER'S ROW, BESIDE THE WINNER'S. The honest record is left alone and this
				// one is numbered ahead of it by [rotation.page].
				one.decoys = append(one.decoys, self.number(replacement))
				continue
			}
			record = replacement
		}
		one.wraps = append(one.wraps, self.number(record))
	}

	groupId, err := epochDigestGroupId(group.id)
	if err != nil {
		self.t.Fatalf("the group id: %v", err)
	}
	contextHash := rotSha256(pending.GroupContext)
	digest, err := message.NewEpochDigestAttachment(groupId, message.EpochDigestAttachment{
		Epoch:             pending.Epoch,
		AlgId:             epochAttachmentAlgId,
		GroupContextHash:  contextHash[:],
		ExpectedWrapCount: uint32(len(targets)),
	}, writeKey, readKey)
	if err != nil {
		self.t.Fatalf("the epoch digest: %v", err)
	}
	record, err := group.session.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(time.Now().UnixMilli()), commit, 0, &message.ServerAttachment{
			Kind:        message.AttachmentEpochDigest,
			EpochDigest: digest,
		})
	if err != nil {
		self.t.Fatalf("%s sealing its commit record: %v", committer.name, err)
	}
	one.commit = self.number(record)

	if err := committer.handle.MergePendingCommit(); err != nil {
		self.t.Fatalf("%s's MergePendingCommit: %v", committer.name, err)
	}
	group.filePqSecretLocked(pending.Epoch, pqNext)
	if err := group.session.AdvanceEpoch(pqNext); err != nil {
		self.t.Fatalf("%s advancing to epoch %d: %v", committer.name, pending.Epoch, err)
	}
	// production's step (4a), which arrived with [Group.RemoveMember]: a committer files what
	// its own commit took out of the group and prunes the departed leaves' ladder heads,
	// BEFORE the re-track below, exactly as the ingest arm's step (5a) does. It is here so a
	// removal case in this file runs against the state production leaves; no case here may
	// read [Group.departedAt] at a committer as evidence about the publish path, because this
	// line is the harness's. The functions are held in removemember_test.go and the call site
	// end to end in cp3b.
	group.noteDepartedLeavesLocked(pending.RemovedLeaves, pending.Epoch)
	group.pruneRemovedLaddersLocked(pending.RemovedLeaves)
	if err := group.crossEpochLadderLocked(pending.Epoch); err != nil {
		self.t.Fatalf("%s crossing the epoch: %v", committer.name, err)
	}
	if err := group.enterEpochLocked(); err != nil {
		self.t.Fatalf("%s entering epoch %d: %v", committer.name, pending.Epoch, err)
	}
	return one
}

// advanceWithoutRotating is the epoch change EVERY BUILD BEFORE THIS COMMIT MADE: a commit that
// opens the next epoch on the secret the group already has, with no draw and no fan-out.
//
// IT IS NOT A DEGRADED [rotWorld.rotate] AND IT IS NOT A MUTANT. It is the shape that wrote every
// group record on the deployed alpha, and it is the only way to build a group whose HISTORY ran
// group-lifetime -- which is the precondition of the one case that needs it, the five-part restore
// with a backlog UNDER it. A world built with `rotate` has a different past: every epoch below the
// restore point ran on its own secret, and a five-part record naming that epoch would then be a
// record no build ever wrote.
//
// The receiver's compatibility arm is what opens it: the digest is computed over the keys the HELD
// secret descends from, so [Group.resolvePqSecretLocked] reproduces it from the value every member
// already has and no wrap is looked for. ExpectedWrapCount is zero because there is no fan-out.
//
// IT TAKES THE COMMIT ARM RATHER THAN ALWAYS BUILDING A BARE ONE, because the shape ledger item
// 243 is about is an old build's REMOVAL -- a CommitRemove whose digest is computed over the held
// secret -- and a harness that could only build `Commit(nil)` could not produce it. The two
// callers are the bare epoch change and that removal, and both go through this one body, so what
// the removal case measures is this build's receive side and not a second fixture.
func (self *rotWorld) advanceWithoutRotating(committer *rotMember,
	arm func() ([]byte, []byte, []byte, error)) *rotation {

	self.t.Helper()
	group := committer.group
	commit, _, _, err := arm()
	if err != nil {
		self.t.Fatalf("%s's commit: %v", committer.name, err)
	}
	pending, err := committer.handle.PendingEpoch()
	if err != nil {
		self.t.Fatalf("%s's pending epoch: %v", committer.name, err)
	}
	held := group.pqSecretLocked()
	newMlsSecret, err := committer.handle.PendingExport(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's staged exporter: %v", committer.name, err)
	}
	newRoot := messagegroup.StorageRoot(newMlsSecret, held)
	writeKey, readKey := message.WriteKey(newRoot), message.ReadKey(newRoot)
	groupId, err := epochDigestGroupId(group.id)
	if err != nil {
		self.t.Fatalf("the group id: %v", err)
	}
	contextHash := rotSha256(pending.GroupContext)
	digest, err := message.NewEpochDigestAttachment(groupId, message.EpochDigestAttachment{
		Epoch:            pending.Epoch,
		AlgId:            epochAttachmentAlgId,
		GroupContextHash: contextHash[:],
		// ONE AND NOT ZERO: the attachment builder refuses a count of zero by name -- MASTER
		// section 8.2's `+1` is the epoch's own snapshot, which every commit owes whether or not
		// it fans out. This is the count a build before the rotation emitted.
		ExpectedWrapCount: 1,
	}, writeKey, readKey)
	if err != nil {
		self.t.Fatalf("the epoch digest: %v", err)
	}
	record, err := group.session.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(time.Now().UnixMilli()), commit, 0, &message.ServerAttachment{
			Kind:        message.AttachmentEpochDigest,
			EpochDigest: digest,
		})
	if err != nil {
		self.t.Fatalf("%s sealing its commit record: %v", committer.name, err)
	}
	one := &rotation{opens: pending.Epoch, pqSecret: append([]byte(nil), held...)}
	one.commit = self.number(record)
	if err := committer.handle.MergePendingCommit(); err != nil {
		self.t.Fatalf("%s's MergePendingCommit: %v", committer.name, err)
	}
	group.filePqSecretLocked(pending.Epoch, one.pqSecret)
	if err := group.session.AdvanceEpoch(one.pqSecret); err != nil {
		self.t.Fatalf("%s advancing to epoch %d: %v", committer.name, pending.Epoch, err)
	}
	if err := group.crossEpochLadderLocked(pending.Epoch); err != nil {
		self.t.Fatalf("%s crossing the epoch: %v", committer.name, err)
	}
	if err := group.enterEpochLocked(); err != nil {
		self.t.Fatalf("%s entering epoch %d: %v", committer.name, pending.Epoch, err)
	}
	return one
}

// fanOutOnTheHeldSecret is THE SHAPE NEITHER OF THE TWO ARMS ABOVE CAN BUILD, and it is the one
// item 251's ruling 41 was taken over: a committer that REMOVES a leaf, writes a COMPLETE,
// well-formed, openable fan-out to every survivor, and puts in every wrap the pq_secret the group
// ALREADY HOLDS.
//
// WHY IT HAS TO BE A THIRD ARM. [rotWorld.advanceWithoutRotating] writes no wrap at all, so the
// receiver has no candidate and never reaches the resolution's first arm;
// [rotWorld.rotate] goes through [Group.stageEpochRotationLocked], whose
// FIRST statement is the draw, so it cannot be made to deliver a stale value. Between them sits
// the commit that defeats both -- FANNED, so a candidate exists, and UNROTATED, so that candidate
// is the value the removed member also holds -- and it is the only way to reach
// [Group.resolvePqSecretLocked]'s wrap-candidate arm with a secret the removal was supposed to
// take away.
//
// `how` IS THE ONE KNOB AND IT IS NOT A SECOND FIXTURE: see [unrotatedFanOut]. The zero value is
// the reproduced blocker, and the two fields are the two other shapes an old or hostile client can
// put on the wire with a fan-out behind it.
//
// EVERY RECORD IS BUILT BY A PRODUCTION SEALER: the targets are [Group.wrapTargetsAtLocked]'s and
// each wrap is [Group.sealEpochWrapLocked]'s, so what a case built on this measures is the
// RECEIVER and not a hand-encoded octet string.
func (self *rotWorld) fanOutOnTheHeldSecret(committer *rotMember,
	arm func() ([]byte, []byte, []byte, error), how unrotatedFanOut) *rotation {

	self.t.Helper()
	group := committer.group
	commit, _, _, err := arm()
	if err != nil {
		self.t.Fatalf("%s's commit: %v", committer.name, err)
	}
	pending, err := committer.handle.PendingEpoch()
	if err != nil {
		self.t.Fatalf("%s's pending epoch: %v", committer.name, err)
	}
	held := append([]byte(nil), group.pqSecretLocked()...)
	if how.opensOn != nil {
		held = append([]byte(nil), how.opensOn...)
	}
	wrapped := held
	if how.payload != nil {
		wrapped = how.payload
	}
	targets, err := group.wrapTargetsAtLocked(pending)
	if err != nil {
		self.t.Fatalf("%s's targets: %v", committer.name, err)
	}
	if len(targets) == 0 {
		self.t.Fatalf("%s's removal addresses no survivor", committer.name)
	}
	one := &rotation{opens: pending.Epoch, pqSecret: held, targets: targets}
	for _, target := range targets {
		record, err := group.sealEpochWrapLocked(pending.Epoch, target, wrapped)
		if err != nil {
			self.t.Fatalf("%s's wrap for leaf %d: %v", committer.name, target.leaf, err)
		}
		one.wraps = append(one.wraps, self.number(record))
		if how.decoy == nil {
			continue
		}
		// THE EXTRA ROW, BESIDE THE FAN-OUT'S OWN AND SEALED HERE RATHER THAN BY THE CALLER. A
		// GroupSession seals at the epoch it was constructed over, and this function ADVANCES the
		// committer before it returns -- so a row a caller sealed afterwards would carry epoch n+1
		// in its header and every receiver would refuse it as a record from an epoch it is not in,
		// which is a different failure from the one any case using this is about.
		decoy, err := group.sealEpochWrapLocked(pending.Epoch, target, how.decoy)
		if err != nil {
			self.t.Fatalf("%s's decoy for leaf %d: %v", committer.name, target.leaf, err)
		}
		one.decoys = append(one.decoys, self.number(decoy))
	}

	// THE DIGEST IS OVER THE HELD SECRET, which is what makes this the unrotated shape: the epoch
	// really is opened on the value every member -- including the one being removed -- already has.
	var attachment *message.ServerAttachment
	if !how.noDigest {
		newMlsSecret, err := committer.handle.PendingExport(storageExporterLabel, nil, storageExporterBytes)
		if err != nil {
			self.t.Fatalf("%s's staged exporter: %v", committer.name, err)
		}
		newRoot := messagegroup.StorageRoot(newMlsSecret, held)
		writeKey, readKey := message.WriteKey(newRoot), message.ReadKey(newRoot)
		groupId, err := epochDigestGroupId(group.id)
		if err != nil {
			self.t.Fatalf("the group id: %v", err)
		}
		contextHash := rotSha256(pending.GroupContext)
		digest, err := message.NewEpochDigestAttachment(groupId, message.EpochDigestAttachment{
			Epoch:             pending.Epoch,
			AlgId:             epochAttachmentAlgId,
			GroupContextHash:  contextHash[:],
			ExpectedWrapCount: uint32(len(targets)),
		}, writeKey, readKey)
		if err != nil {
			self.t.Fatalf("the epoch digest: %v", err)
		}
		attachment = &message.ServerAttachment{Kind: message.AttachmentEpochDigest, EpochDigest: digest}
	}
	record, err := group.session.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(time.Now().UnixMilli()), commit, 0, attachment)
	if err != nil {
		self.t.Fatalf("%s sealing its commit record: %v", committer.name, err)
	}
	one.commit = self.number(record)

	if err := committer.handle.MergePendingCommit(); err != nil {
		self.t.Fatalf("%s's MergePendingCommit: %v", committer.name, err)
	}
	group.filePqSecretLocked(pending.Epoch, held)
	if err := group.session.AdvanceEpoch(held); err != nil {
		self.t.Fatalf("%s advancing to epoch %d: %v", committer.name, pending.Epoch, err)
	}
	if err := group.crossEpochLadderLocked(pending.Epoch); err != nil {
		self.t.Fatalf("%s crossing the epoch: %v", committer.name, err)
	}
	if err := group.enterEpochLocked(); err != nil {
		self.t.Fatalf("%s entering epoch %d: %v", committer.name, pending.Epoch, err)
	}
	return one
}

// unrotatedFanOut is how one hand-rolled adversarial committer differs from the honest one, in the
// two ways that change WHICH refusal the receiver takes. The zero value is the reproduced blocker:
// a complete, openable fan-out of the secret the group already holds, under a digest that names
// it.
type unrotatedFanOut struct {
	// opensOn is the value the epoch is actually opened on -- what the wraps carry AND what the
	// digest is computed over. nil is the group's CURRENT secret, which is the ordinary
	// unrotated removal. A value from an EARLIER epoch is the shape that separates the rule
	// from its old spelling: the removed member keeps every row of the window it was a member
	// for, so replaying pq_secret[n-1] delivers a different octet string to the same adversary.
	opensOn []byte
	// payload replaces the pq_secret the wraps carry while the DIGEST still names `opensOn`. It
	// is the shape no check that reads the CANDIDATES could ever have refused: they are fresh,
	// and the digest then says the epoch was opened on a held value after all.
	payload []byte
	// noDigest seals the commit with NO server attachment, which is what
	// [Group.epochDigestOf] answers nil for -- a commit the resolution cannot ask the question
	// of. It is the shape that reached the resolution's no-digest arm through a walk, on the
	// first try, while a comment in pqdarkgate_test.go said no page could.
	noDigest bool
	// decoy adds ONE EXTRA wrap row per target, beside the fan-out's own and carrying these
	// octets instead of `payload`. It exists to put a candidate the removal rule does NOT
	// recognise beside candidates it does, which is the shape the deleted pre-apply clause could
	// not refuse: "every candidate carries a held value" is defeated by one fresh row, while the
	// digest comparison at the resolution does not have that shape and refuses either page.
	decoy []byte
}

// fanOutOnAFreshSecret is the residual arm: fresh octets in the wraps, the held secret in the
// digest. Spelled once here so no caller has to decide for itself what "fresh" means.
func (self *rotWorld) fanOutOnAFreshSecret(committer *rotMember,
	arm func() ([]byte, []byte, []byte, error)) *rotation {

	self.t.Helper()
	decoy := make([]byte, messagegroup.PqSecretBytes)
	if _, err := rand.Read(decoy); err != nil {
		self.t.Fatalf("the decoy payload: %v", err)
	}
	return self.fanOutOnTheHeldSecret(committer, arm, unrotatedFanOut{payload: decoy})
}

func (self *rotWorld) number(record *message.Record) *sealed {
	one := &sealed{recordId: self.nextRecordId, record: record}
	self.nextRecordId += 1
	return one
}

// deliver walks one page through a receiver's group, as [Group.Receive] does below the fetch.
func (self *rotWorld) deliver(receiver *rotMember, page ...*sealed) error {
	self.t.Helper()
	group := receiver.group
	own, err := group.session.SenderHandle()
	if err != nil {
		self.t.Fatalf("%s's sender handle: %v", receiver.name, err)
	}
	group.ownHandles[own] = true
	walk := &pageWalk{
		// THE SAME THREE LINES [Group.Receive] WRITES, and the handle table is deliberately
		// EMPTY rather than prebuilt: it is per record epoch now (ledger item 245) and
		// [Group.walkLeavesLocked] fills it on the first record of each epoch. A harness that
		// handed the walk one prebuilt table would be the very defect this file's removal rows
		// drive.
		own:          group.ownHandles,
		ownNow:       own,
		leaves:       map[uint64]map[[16]byte]uint32{},
		opened:       []*Message{},
		from:         group.cursor,
		reached:      group.cursor,
		resolvedTo:   group.cursor,
		reconciled:   group.reconciled,
		complete:     true,
		unobtainable: map[uint64]bool{},
	}
	rows := []*protocol.Record{}
	for _, one := range page {
		encoded, err := message.EncodeRecord(one.record)
		if err != nil {
			self.t.Fatalf("encoding record %d: %v", one.recordId, err)
		}
		rows = append(rows, &protocol.Record{RecordId: one.recordId, RecordBytes: encoded})
	}
	group.openPageLocked(&protocol.FetchResponse{Records: rows}, walk)
	// nil: this harness hands a page straight to openPageLocked and never fetches, so there
	// is no transport refusal to weigh. The arm that HAS one is only reachable against a real
	// server -- see cp3b/pqrotation_test.go.
	return group.commitWalkLocked(walk, nil)
}

// storageRootOf is the value the whole of item 243 is about: this member's own storage root at the
// epoch it stands in, from its OWN exporter and its OWN table.
func (self *rotWorld) storageRootOf(member *rotMember) []byte {
	self.t.Helper()
	mlsSecret, err := member.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's exporter at epoch %d: %v", member.name, member.handle.Epoch(), err)
	}
	secret := member.group.pqSecretLocked()
	if len(secret) == 0 {
		self.t.Fatalf("%s holds no pq_secret at epoch %d", member.name, member.group.epoch)
	}
	return messagegroup.StorageRoot(mlsSecret, secret)
}

func rotSha256(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// ── 1. THE PROPERTY ──────────────────────────────────────────────────────────────────────────

// EVERY MEMBER A COMMIT LEAVES IN THE GROUP DERIVES THE SAME storage_root FOR THE EPOCH IT OPENS,
// AND THE MEMBER IT REMOVES CANNOT -- ACROSS TWO ROTATIONS.
//
// THE INLINE CONTROL IS THE PRE-ROTATION AGREEMENT AND IT FIRES FOR ITS OWN REASON: before the
// removal, the member about to be removed derives the SAME root as the survivors. Without it a
// build in which nobody ever agreed about anything would pass the second half of this property
// trivially, and the case would be measuring a broken group rather than a removal.
//
// THE COUNTERFACTUAL IS THE FINDING, and it is item 251's own measurement read from this side.
// A removed member keeps pq_secret[n] forever -- that is what item 243 ruled and what nothing can
// take back. So the question is not what it holds but what that holding BUYS, and the answer is
// measured directly: mls_secret[n+1] (which MLS's own post-compromise security already denies it,
// but which is GRANTED here so the pq half is the only variable) mixed with the RETAINED secret
// does NOT reproduce the survivors' root; mixed with the ROTATED one it does, exactly. That second
// clause is the second control: it pins that the difference is the post-quantum half and not the
// exporter, which is the one substitution a broken rotation could hide behind.
//
// WHAT WOULD GO RED: reuse one secret across epochs (the survivors still agree, and the
// counterfactual reproduces their root -- the removal buys nothing); leave the removed leaf in the
// fan-out (its wrap arrives and the "no wrap" assertion fails); seal every wrap to one leaf's key
// (the other survivors go dark and their roots diverge).
func TestThreeMembersRotateAcrossTwoEpochsAndAMemberRemovedByThatCommitCannotFollow(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// ── THE CONTROL, FIRST AND ON ITS OWN TERMS: at epoch 1 all three agree ─────────────────
	atOne := world.storageRootOf(alice)
	for _, who := range []*rotMember{bob, carol} {
		if !bytes.Equal(atOne, world.storageRootOf(who)) {
			t.Fatalf("CONTROL FAILED: at epoch 1 %s's storage root is not alice's, so this group "+
				"never agreed about anything and the removal below would prove nothing", who.name)
		}
	}
	t.Logf("CONTROL HELD: at epoch 1 all three members derive one storage root")

	// ── ROTATION ONE: a bare commit that removes nobody. Everyone follows. ───────────────────
	first := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	if first.opens != 2 {
		t.Fatalf("the first rotation opens epoch %d, want 2", first.opens)
	}
	for _, who := range []*rotMember{bob, carol} {
		if err := world.deliver(who, first.page()...); err != nil {
			t.Fatalf("%s's walk over the first rotation: %v", who.name, err)
		}
	}
	atTwo := world.storageRootOf(alice)
	if bytes.Equal(atOne, atTwo) {
		t.Fatalf("epoch 2's storage root is epoch 1's; nothing rotated")
	}
	for _, who := range []*rotMember{bob, carol} {
		if got := world.storageRootOf(who); !bytes.Equal(atTwo, got) {
			t.Fatalf("after the first rotation %s's storage root at epoch 2 is not alice's; "+
				"a member that followed the commit did not follow the secret", who.name)
		}
		if !bytes.Equal(who.group.pqSecretLocked(), first.pqSecret) {
			t.Fatalf("%s's pq_secret at epoch 2 is not the one the fan-out carried", who.name)
		}
		if got := who.group.Stats().WrapOpened; got != 1 {
			t.Errorf("%s opened %d wrap(s) across one rotation, want 1", who.name, got)
		}
	}

	// ── ROTATION TWO: the commit that REMOVES carol ─────────────────────────────────────────
	//
	// The retained secret is read BEFORE the commit, because that is all a removed member can
	// keep: pq_secret at the epoch it was removed at.
	retained := append([]byte(nil), carol.group.pqSecretLocked()...)
	if !bytes.Equal(retained, first.pqSecret) {
		t.Fatalf("carol's retained secret is not epoch 2's; the counterfactual would be about the wrong value")
	}
	second := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	})
	if second.opens != 3 {
		t.Fatalf("the second rotation opens epoch %d, want 3", second.opens)
	}

	// THE FAN-OUT LEFT CAROL OUT, and the positive control is in the same loop: every surviving
	// leaf IS a target. A bare "carol is not a target" would pass against an empty fan-out.
	survivors, removedIsTarget := 0, false
	for _, target := range second.targets {
		if target.leaf == carol.leaf {
			removedIsTarget = true
			continue
		}
		survivors += 1
	}
	if removedIsTarget {
		t.Fatalf("the fan-out for epoch 3 addresses leaf %d, which is the leaf the commit removes", carol.leaf)
	}
	if survivors != 2 {
		t.Fatalf("the fan-out for epoch 3 addresses %d surviving leaves, want 2 (alice and bob); "+
			"the exclusion cannot be told from an empty fan-out", survivors)
	}

	if err := world.deliver(bob, second.page()...); err != nil {
		t.Fatalf("bob's walk over the removal: %v", err)
	}
	atThree := world.storageRootOf(alice)
	if got := world.storageRootOf(bob); !bytes.Equal(atThree, got) {
		t.Fatalf("after the removal bob's storage root at epoch 3 is not alice's")
	}
	if bytes.Equal(atThree, atTwo) {
		t.Fatalf("epoch 3's storage root is epoch 2's; the removal rotated nothing")
	}

	// ── THE COUNTERFACTUAL, WITH ITS OWN CONTROL BESIDE IT ──────────────────────────────────
	//
	// The exporter of the epoch the removal opened is GRANTED to the removed member here. That is
	// strictly more than MLS gives it -- a Remove forces an UpdatePath and blanks the path, so it
	// cannot derive this at all -- and granting it is the point: it leaves the post-quantum half
	// as the only variable, which is the half item 243 is about.
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-3 exporter: %v", err)
	}
	withRetained := messagegroup.StorageRoot(granted, retained)
	withRotated := messagegroup.StorageRoot(granted, second.pqSecret)
	if !bytes.Equal(withRotated, atThree) {
		t.Fatalf("CONTROL FAILED: the epoch-3 exporter mixed with the epoch's OWN secret is not the " +
			"root the survivors derived, so the counterfactual below is not about the pq half")
	}
	if bytes.Equal(withRetained, atThree) {
		t.Fatalf("the removed member's RETAINED pq_secret, mixed with the epoch it was removed from, " +
			"reproduces the survivors' storage_root[3]. Item 243's whole subject: the removal removed nothing")
	}
	t.Logf("the removed member's retained secret does not reproduce storage_root[3]; the epoch's own secret does")

	// AND CAROL WAS HANDED NOTHING. Her table still stops at the epoch she was removed from.
	if _, held := carol.group.pqSecretAtLocked(3); held {
		t.Fatalf("the removed member holds a pq_secret for epoch 3")
	}
}

// THE FAN-OUT LEAVES A REMOVED LEAF OUT EVEN WHEN THE SAME COMMIT REFILLS IT, WHICH IS THE ONE
// INPUT ON WHICH EVERY COMPARISON OF THE TWO TREES AND THE TRUTH DISAGREE.
//
// WHY THIS CASE EXISTS BESIDE THE ONE ABOVE. The case above removes the group's HIGHEST leaf out of
// three, so it is satisfied by any derivation that happens to name that leaf -- "the leaves that no
// longer fit under pending.MemberCount", "the highest live leaf when the staged count is smaller",
// a live-versus-staged set difference. Here one commit removes leaf 2 AND admits a newcomer: RFC
// 9420 §12.3 applies Removes before Adds and an Add fills the leftmost blank, so the newcomer lands
// on the leaf the removal just blanked and the two trees' occupied leaves -- and BOTH member
// counts -- are EQUAL while a member was removed. Every one of those comparisons answers "nothing
// removed" here, the removed member is still standing in the live tree the fan-out is built off
// pre-merge, and the epoch's post-quantum secret is sealed straight to the X-Wing key of the member
// the commit exists to shut out: ledger item 243 arriving inverted, through a commit that looks
// balanced from every angle except the staged commit's own answer.
//
// THE EQUALITY IS ASSERTED FIRST AND THE EXCLUSION SECOND, because the second says nothing without
// the first: against a build where the refill did not land, this case would be an ordinary removal
// and would convict nothing that the case above does not. The equality is a MEASUREMENT of what
// this MLS build does, so it fatals with its own sentence rather than being relaxed.
//
// AND IT IS EMPHATICALLY NOT `len(targets) == pending.MemberCount`. That equality is false on every
// Add -- the added leaf is not in the live tree and gets no wrap -- and here it is false by two:
// three members before, three after, two wraps. connect states the same refusal on
// [messagegroup.PendingEpoch] itself, and holds the layer below this one in
// TestARemovalWhoseLeafIsRefilledInTheSameCommitIsStillNamedByTheStagedCommit.
func TestARemovalTheSameCommitRefillsIsStillLeftOutOfTheFanOut(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// the newcomer exists only to be admitted: it never joins, because what this case measures is
	// the COMMITTER's fan-out over the tree the commit has not been merged into yet.
	dave := world.device("dave")
	keyPackage, err := dave.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the newcomer's key package: %v", err)
	}

	liveOccupied := map[uint32]bool{}
	for at := 0; at < alice.handle.MemberCount(); at += 1 {
		leaf, _, _, err := alice.handle.MemberAt(at)
		if err != nil {
			t.Fatalf("the live tree's member %d: %v", at, err)
		}
		liveOccupied[leaf] = true
	}
	if !liveOccupied[carol.leaf] {
		t.Fatalf("CONTROL FAILED: the live tree does not hold leaf %d, which is the leaf this case "+
			"removes; occupied %v", carol.leaf, liveOccupied)
	}
	liveCount := alice.handle.MemberCount()

	// THE COMMIT: one Remove and one Add, folded together. The seam has no by-value arm that
	// carries both, so the two proposals are generated and committed by reference -- which is
	// ruling 13's forbidden shape IN PRODUCTION and is the only door this package has onto a
	// multi-proposal commit from a test. What is measured below is the derivation, not the door.
	if _, err := alice.handle.ProposeRemove(carol.leaf); err != nil {
		t.Fatalf("proposing the removal of leaf %d: %v", carol.leaf, err)
	}
	if _, err := alice.handle.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("proposing the newcomer's add: %v", err)
	}
	if _, _, _, err := alice.handle.Commit(nil); err != nil {
		t.Fatalf("the remove-and-refill commit: %v", err)
	}
	defer alice.handle.ClearPendingCommit()

	pending, err := alice.handle.PendingEpoch()
	if err != nil {
		t.Fatalf("the staged commit's facts: %v", err)
	}

	// ── THE MEASUREMENT: the refill landed, so nothing about the two trees can see the removal ──
	if pending.MemberCount != liveCount {
		t.Fatalf("MEASUREMENT FAILED: the staged tree holds %d members and the live tree holds %d. "+
			"This case exists because an Add refills the leaf a Remove blanked in the SAME commit, "+
			"which makes every count- and set-comparison blind; if that has stopped being true in "+
			"this MLS build, the reason [Group.wrapTargetsAtLocked] reads the staged commit has to "+
			"be re-derived rather than this assertion relaxed", pending.MemberCount, liveCount)
	}
	if alice.handle.MemberCount() != liveCount {
		t.Fatalf("MEASUREMENT FAILED: the live tree moved under a staged commit: %d, want %d",
			alice.handle.MemberCount(), liveCount)
	}

	// ── THE PROPERTY: and the removed leaf is left out of the fan-out anyway ────────────────────
	//
	// THROUGH [Group.stageEpochRotationLocked] AND NOT STRAIGHT INTO [Group.wrapTargetsAtLocked],
	// which is the frame [Group.publishCommitLocked] actually calls. Measured: a mutant that sent
	// the commit path through [Group.foundingWrapTargetsLocked] -- the door with no exclusion at
	// all, which is what an arm reaching past the derivation looks like -- is invisible to a case
	// that calls the enumerator itself, and this frame convicts it.
	staged, err := alice.group.stageEpochRotationLocked(pending)
	if err != nil {
		t.Fatalf("the rotation for the epoch the remove-and-refill opens: %v", err)
	}
	targets := staged.targets
	addressed := map[uint32]bool{}
	for _, target := range targets {
		addressed[target.leaf] = true
	}
	if addressed[carol.leaf] {
		t.Fatalf("the fan-out for epoch %d addresses leaf %d, which is the leaf the commit REMOVES "+
			"and which the same commit refilled with a newcomer. The two trees agree exactly here "+
			"(%d members before and after), so every derivation taken by comparing them answers the "+
			"empty set -- and this epoch's pq_secret is now sealed to the removed member's own "+
			"X-Wing key, which is ledger item 243 inverted. The exclusion must come off the staged "+
			"commit's own RemovedLeaves", pending.Epoch, carol.leaf, liveCount)
	}
	// THE POSITIVE CONTROL, IN THE SAME LOOP: both survivors ARE addressed. Without it an empty
	// fan-out, or one this build refused to build at all, would satisfy the clause above.
	for _, who := range []*rotMember{alice, bob} {
		if !addressed[who.leaf] {
			t.Fatalf("CONTROL FAILED: the fan-out does not address %s at leaf %d, so the exclusion "+
				"above cannot be told from a fan-out that addresses nobody; addressed %v",
				who.name, who.leaf, addressed)
		}
	}
	if len(targets) != liveCount-1 {
		t.Fatalf("the fan-out addresses %d leaves over a live tree of %d with one removal; want %d",
			len(targets), liveCount, liveCount-1)
	}
	// AND THE SIZE IS NOT pending.MemberCount, which is the one-line invariant two passes offered
	// for this function. It is false here by one and false on every Add by one the other way.
	if len(targets) == pending.MemberCount {
		t.Fatalf("the fan-out's size equals the staged member count (%d). That equality has never "+
			"held -- it is false on every Add -- and if it holds here the removed leaf is back in "+
			"the fan-out", pending.MemberCount)
	}
	t.Logf("live and staged member counts both %d, staged RemovedLeaves %v, fan-out addresses %d leaves",
		liveCount, pending.RemovedLeaves, len(targets))
}

// ── 2. THE THREE FAILURE STATES ──────────────────────────────────────────────────────────────

// EACH OF THE THREE WAYS A WRAP FAILS IS REACHED, NAMED BY ITS OWN SENTINEL, AND COUNTED APART.
//
// RULING 38 IS WHY THIS IS THREE CASES AND NOT ONE. The day a wrap carries key material a member
// that never opens a readable one goes dark in both directions and permanently, with an
// undiagnosable REASON_REJECTED -- and the orphan case, a fan-out from a committer that lost its
// CAS race, "must be a typed refusal separable from this one, or the two are indistinguishable in
// the field". They have three different repairs, so they are three sentences.
//
// THE POSITIVE CONTROL IS THE FIRST SUBTEST: the same page, unmutated, installs the secret and
// moves [Stats.WrapOpened]. Without it every case below would be satisfiable by a build that
// refused every wrap.
func TestTheThreeWaysADeviceWrapFailsAreThreeSentinelsAndThreeCounters(t *testing.T) {
	for _, one := range []struct {
		what    string
		bend    func(victim *rotMember) []rotBend
		omit    bool
		want    error
		counter func(Stats) uint64
	}{
		{
			what:    "the control: an intact fan-out installs the epoch's secret",
			bend:    func(victim *rotMember) []rotBend { return nil },
			want:    nil,
			counter: func(stats Stats) uint64 { return stats.WrapOpened },
		},
		{
			// ITEM 132's OMISSION, built the way a lying committer builds it: the victim's row is
			// simply not on the wire, and expected_wrap_count is untouched, so the server and
			// every other member see a fan-out that adds up.
			what:    "no wrap arrived: the committer left this member out of the fan-out",
			bend:    func(victim *rotMember) []rotBend { return nil },
			omit:    true,
			want:    ErrNoWrapForEpoch,
			counter: func(stats Stats) uint64 { return stats.WrapMissing },
		},
		{
			// SEALED TO A KEY NOBODY HOLDS. The KEM does not say no -- ML-KEM-768 uses implicit
			// rejection, so this decapsulates SUCCESSFULLY to a pseudorandom secret -- and the
			// only thing that separates "mine" from "not mine" is the Poly1305 tag. That is why
			// this case is built by sealing to a stranger rather than by editing a ciphertext: the
			// record's own AEAD still authenticates, so the refusal comes from where this case
			// says it does.
			what: "a wrap arrived and did not open: it was sealed to a key this device does not hold",
			bend: func(victim *rotMember) []rotBend {
				return []rotBend{{leaf: victim.leaf, toAStranger: true}}
			},
			want:    ErrWrapUnreadable,
			counter: func(stats Stats) uint64 { return stats.WrapUnreadable },
		},
		{
			// THE ORPHAN. A second committer built the same epoch, wrote its fan-out, and lost the
			// race -- so what reaches this member is a perfectly READABLE wrap for epoch n+1
			// carrying a secret that epoch was never opened with. It is a state ruling 37's
			// pre-merge submit PRODUCES, which is why the detector ships in the same commit.
			what: "wraps arrived for an epoch that never opened: a committer lost its CAS race",
			bend: func(victim *rotMember) []rotBend {
				loser := make([]byte, messagegroup.PqSecretBytes)
				for at := range loser {
					loser[at] = 0x7C
				}
				return []rotBend{{leaf: victim.leaf, payload: loser}}
			},
			want:    ErrOrphanWrap,
			counter: func(stats Stats) uint64 { return stats.WrapOrphaned },
		},
	} {
		t.Run(one.what, func(t *testing.T) {
			world := newRotWorld(t, "alice", "bob")
			alice, bob := world.member("alice"), world.member("bob")
			published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
				return alice.handle.Commit(nil)
			}, one.bend(bob)...)

			page := published.page()
			if one.omit {
				kept := []*sealed{}
				dropped := false
				for at, wrap := range published.wraps {
					if published.targets[at].leaf == bob.leaf {
						dropped = true
						continue
					}
					kept = append(kept, wrap)
				}
				if !dropped {
					t.Fatalf("the victim's wrap was not in the fan-out, so nothing was omitted")
				}
				page = append(kept, published.commit)
			}

			err := world.deliver(bob, page...)
			if one.want == nil {
				if err != nil {
					t.Fatalf("the control page was refused: %v", err)
				}
				if !bytes.Equal(bob.group.pqSecretLocked(), published.pqSecret) {
					t.Fatalf("the control page did not install the epoch's secret")
				}
				if bob.group.wrapDark != nil {
					t.Fatalf("the control page left the group dark: %v", bob.group.wrapDark)
				}
			} else {
				if !errors.Is(err, one.want) {
					t.Fatalf("the walk answered %v, want %v", err, one.want)
				}
				// AND THE STICKY COPY, which is what a caller meets on every LATER walk: the
				// diagnosis has to outlive the one page that produced it, or the next walk reports
				// an AEAD failure on an arbitrary record instead.
				if !errors.Is(bob.group.wrapDark, one.want) {
					t.Fatalf("the group's sticky diagnosis is %v, want %v", bob.group.wrapDark, one.want)
				}
				if _, refused := bob.group.sendableLocked(KindText); !errors.Is(refused, one.want) {
					t.Fatalf("a send from the dark group was refused %v, want %v", refused, one.want)
				}
				// AND THE GROUP STILL FOLLOWED THE COMMIT, which is what makes this a diagnosis
				// rather than a halt: the handle, the session and the persisted record all moved,
				// so nothing is left one epoch behind the tree it is standing in.
				if bob.group.epoch != published.opens {
					t.Fatalf("the group stands at epoch %d and the commit opened %d", bob.group.epoch, published.opens)
				}
			}
			if got := one.counter(bob.group.Stats()); got != 1 {
				t.Fatalf("the counter for this state is %d, want 1; a sentinel with no number "+
					"beside it cannot answer whether this is happening", got)
			}
			// AND NO OTHER STATE'S COUNTER MOVED, which is what makes the three SEPARABLE rather
			// than three names for one number. The control's own counter is excluded by value,
			// not by name, so a build that moved two of them at once is caught here.
			stats := bob.group.Stats()
			mine := one.counter(stats)
			for what, got := range map[string]uint64{
				"WrapMissing":    stats.WrapMissing,
				"WrapUnreadable": stats.WrapUnreadable,
				"WrapOrphaned":   stats.WrapOrphaned,
			} {
				if got != 0 && got != mine {
					t.Errorf("Stats.%s is %d and this case is about a different state", what, got)
				}
			}
		})
	}
}

// AN ORPHAN IS COUNTED ON EVERY ARM OF THE RESOLUTION AND IN BOTH RACE ORDERINGS, WHICH IS THE
// READING [Stats.WrapOrphaned]'s OWN DOC CALLS HEALTHY AND WHICH USED TO READ ZERO.
//
// THE DEFECT. `orphans` was accumulated in the candidate loop and added to the counter only on the
// arm reached when NO candidate won. So a wrap that OPENED and lost the digest -- to a later
// candidate, to the held secret, or to a removal refusal -- moved nothing, and the one state the
// counter's own documentation describes as the ordinary healthy one ("two committers raced, this
// device opened both wraps and used the winner's") was the one state it could never report.
//
// AND THE FIRST REPAIR WAS THE DEFECT'S OWN SHAPE ONE LEVEL DOWN, WHICH IS WHY (c) AND (d) ARE
// HERE. Moving the increment out of the refusal fixed the two arms REACHED THROUGH THE CANDIDATE
// LOOP and left the two that return BEFORE it reading zero -- the digest-less commit (which is
// every group on the deployed alpha) and the commit whose digest names another epoch. "Every arm"
// was asserted for two arms of four. The increment is in a DEFER now, which is what makes "every
// arm" true by construction rather than by four sites agreeing, and (c) drives the digest-less one
// through a page while (d) drives the epoch-mismatch one directly.
//
// THE CASE. A loser's fan-out reaches bob FIRST -- ruling 37 puts wraps on the wire before the
// commit, so the loser's rows carry the lower record ids -- and the winner's own wrap for bob is
// in the same page behind it. bob opens both, the digest picks the winner, and bob follows the
// commit with no error at all.
//
// THE CONTROLS ARE THE ASSERTIONS AROUND THE NUMBER, and each fires for its own reason: bob is NOT
// dark, bob stands at the new epoch, bob holds the epoch's own secret, and WrapOpened is 2 -- so
// what the counter is reporting is two wraps that really did open and one of them really did lose,
// rather than a group that failed in some other way. Without WrapOpened == 2 a decoy that never
// opened would produce the same zero this case exists to refuse.
func TestAnOrphanIsCountedOnEveryArmOfTheResolutionAndInBothRaceOrderings(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")

	loser := make([]byte, messagegroup.PqSecretBytes)
	for at := range loser {
		loser[at] = 0x5A
	}
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	}, rotBend{leaf: bob.leaf, payload: loser, decoy: true})
	if len(published.decoys) != 1 {
		t.Fatalf("CONTROL FAILED: the fixture built %d decoy row(s), want 1", len(published.decoys))
	}
	if bytes.Equal(loser, published.pqSecret) {
		t.Fatalf("CONTROL FAILED: the decoy carries the epoch's own secret, so nothing in this page loses")
	}

	if err := world.deliver(bob, published.page()...); err != nil {
		t.Fatalf("bob's walk over a winner-and-loser page answered %v; this is the HEALTHY reading "+
			"and the group is supposed to follow it", err)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark although the winner's wrap was in the same page: %v", bob.group.wrapDark)
	}
	if bob.group.epoch != published.opens {
		t.Fatalf("bob stands at epoch %d, want %d", bob.group.epoch, published.opens)
	}
	if !bytes.Equal(bob.group.pqSecretLocked(), published.pqSecret) {
		t.Fatalf("bob followed onto a secret that is not the epoch's own")
	}
	stats := bob.group.Stats()
	if stats.WrapOpened != 2 {
		t.Fatalf("CONTROL FAILED: bob opened %d wrap(s) and this case needs 2 -- the winner's and "+
			"the loser's. With fewer, a zero below would mean the decoy never opened", stats.WrapOpened)
	}
	if stats.WrapOrphaned != 1 {
		t.Fatalf("Stats.WrapOrphaned is %d after a wrap opened and lost the digest, want 1. An "+
			"orphan counted only where it is REPORTED and not where it is FOUND reads as zero on "+
			"every arm that answers a secret, which is every arm a healthy group takes", stats.WrapOrphaned)
	}
	if stats.WrapMissing != 0 || stats.WrapUnreadable != 0 {
		t.Fatalf("Stats.WrapMissing=%d WrapUnreadable=%d; this case is about neither, and a number "+
			"in either would mean the page failed for a reason this case is not measuring",
			stats.WrapMissing, stats.WrapUnreadable)
	}
	t.Logf("CONTROL HELD, loser first: WrapOpened=%d WrapOrphaned=%d", stats.WrapOpened, stats.WrapOrphaned)

	// ── AND THE OTHER ORDERING, WHICH IS HALF OF THE RACE AND READ ZERO ─────────────────────
	//
	// WHY IT IS A SECOND CASE AND NOT A SECOND ASSERTION. [rotation.page] builds the loser's row
	// FIRST, because a losing committer's fan-out is written before the winner's commit is
	// accepted -- which is true of one of the two orderings and arbitrary between them. With the
	// counter inside the candidate loop the loop RETURNED at the winner, so an orphan numbered
	// AFTER it was never reached and read as 0: exactly the reading [Stats.WrapOrphaned]'s own doc
	// calls healthy, missing on half the race orderings, with the case above green the whole time.
	// The repair is that every candidate is judged before any is answered, so this page is
	// delivered winner-first and must report the same number.
	second := newRotWorld(t, "alice", "bob")
	secondAlice, secondBob := second.member("alice"), second.member("bob")
	secondLoser := make([]byte, messagegroup.PqSecretBytes)
	for at := range secondLoser {
		secondLoser[at] = 0xA5
	}
	secondPublished := second.rotate(secondAlice, func() ([]byte, []byte, []byte, error) {
		return secondAlice.handle.Commit(nil)
	}, rotBend{leaf: secondBob.leaf, payload: secondLoser, decoy: true})
	if len(secondPublished.decoys) != 1 || len(secondPublished.wraps) == 0 {
		t.Fatalf("CONTROL FAILED: the fixture built %d decoy(s) and %d wrap(s), want 1 and at least 1",
			len(secondPublished.decoys), len(secondPublished.wraps))
	}
	if bytes.Equal(secondLoser, secondPublished.pqSecret) {
		t.Fatalf("CONTROL FAILED: the decoy carries the epoch's own secret, so nothing in this page loses")
	}
	// THE WINNER FIRST, THE LOSER SECOND, THE COMMIT LAST -- spelled here rather than through
	// [rotation.page], which hardcodes the other order and is the reason this could not be seen.
	winnerFirst := []*sealed{}
	winnerFirst = append(winnerFirst, secondPublished.wraps...)
	winnerFirst = append(winnerFirst, secondPublished.decoys...)
	winnerFirst = append(winnerFirst, secondPublished.commit)
	if err := second.deliver(secondBob, winnerFirst...); err != nil {
		t.Fatalf("bob's walk over a winner-first page answered %v; this is the same HEALTHY reading "+
			"in the other order", err)
	}
	secondStats := secondBob.group.Stats()
	if secondStats.WrapOpened != 2 {
		t.Fatalf("CONTROL FAILED: bob opened %d wrap(s) in the winner-first ordering and this case "+
			"needs 2, or a zero below would mean the loser's row never opened", secondStats.WrapOpened)
	}
	if secondBob.group.epoch != secondPublished.opens || secondBob.group.wrapDark != nil {
		t.Fatalf("bob stands at epoch %d dark=%v after a winner-first page",
			secondBob.group.epoch, secondBob.group.wrapDark)
	}
	if secondStats.WrapOrphaned != 1 {
		t.Fatalf("Stats.WrapOrphaned is %d in the WINNER-FIRST ordering and %d in the loser-first "+
			"one, want 1 in both. Which of two racing committers wrote its fan-out first is "+
			"arbitrary, so a counter that only sees the loser when it is numbered BEFORE the winner "+
			"reads zero on half the races", secondStats.WrapOrphaned, stats.WrapOrphaned)
	}

	// ── (c) THE ARM THAT RETURNS BEFORE THE CANDIDATE LOOP: THE DIGEST-LESS COMMIT ──────────
	//
	// WHY THIS ARM AND NOT ANOTHER. Every group on the deployed alpha is on it -- a kind 0x0001
	// commit inside Spec B section 5.4's open acceptance window carries no digest, so the epoch it
	// opens runs on the secret the group already has and the resolution answers it at the TOP of
	// the function, before any candidate is looked at. A wrap that opened for that epoch and
	// carries something else is an orphan by [Stats.WrapOrphaned]'s own definition -- "wraps that
	// opened and were not the epoch's own secret" -- and it read 0, because the increment sat below
	// the loop this arm never reaches.
	//
	// THE CONTROLS ARE THE CLAUSES AROUND THE NUMBER: the commit really is digest-less, the walk
	// really is followed with nil, bob really did open the row (WrapOpened), and bob really did
	// follow onto the HELD secret rather than onto the row's own octets -- so the number below is
	// about a wrap that opened and lost, and not about a page that failed some other way.
	third := newRotWorld(t, "alice", "bob")
	thirdAlice, thirdBob := third.member("alice"), third.member("bob")
	thirdHeld := append([]byte(nil), thirdBob.group.pqSecretLocked()...)
	stray := make([]byte, messagegroup.PqSecretBytes)
	for at := range stray {
		stray[at] = 0xC3
	}
	thirdPublished := third.fanOutOnTheHeldSecret(thirdAlice, func() ([]byte, []byte, []byte, error) {
		return thirdAlice.handle.Commit(nil)
	}, unrotatedFanOut{noDigest: true, payload: stray})
	if digest, err := epochDigestOf(&thirdPublished.commit.record.Header); err != nil || digest != nil {
		t.Fatalf("CONTROL FAILED: this commit carries a digest (%v, %v), so it is not the arm this "+
			"clause is named for", digest, err)
	}
	if bytes.Equal(stray, thirdHeld) {
		t.Fatalf("CONTROL FAILED: the stray row carries the held secret, so nothing about it is an orphan")
	}
	if err := third.deliver(thirdBob, thirdPublished.page()...); err != nil {
		t.Fatalf("bob's walk over a digest-less commit with one stray wrap beside it answered %v; "+
			"that is the compatibility path and every group on the deployed alpha is on it", err)
	}
	thirdStats := thirdBob.group.Stats()
	if thirdStats.WrapOpened != 1 {
		t.Fatalf("CONTROL FAILED: bob opened %d wrap(s) and this clause needs 1, or the zero below "+
			"would mean the stray row never opened", thirdStats.WrapOpened)
	}
	if !bytes.Equal(thirdBob.group.pqSecretLocked(), thirdHeld) {
		t.Fatalf("CONTROL FAILED: bob followed the digest-less commit onto something that is not the " +
			"held secret, so it did not take the arm this clause is about")
	}
	if thirdStats.WrapOrphaned != 1 {
		t.Fatalf("Stats.WrapOrphaned is %d on the DIGEST-LESS arm after a wrap opened and was not "+
			"the epoch's own secret, want 1. That arm returns before the candidate loop, so a counter "+
			"added inside the loop's aftermath is 'every arm' asserted for the arms the loop reaches",
			thirdStats.WrapOrphaned)
	}

	// ── (d) THE OTHER ARM THAT RETURNS BEFORE THE LOOP: A DIGEST FOR ANOTHER EPOCH ──────────
	//
	// IT IS DRIVEN DIRECTLY AND NOT THROUGH A PAGE, and the reason is in the arm's own comment: the
	// server refuses a commit whose attachment does not open current_epoch + 1, so no page a
	// receiver is served can carry it and a case that built one would be measuring a record this
	// system does not produce. The arm exists anyway, because the two epochs are read from two
	// different places, and a counter that reads zero there is the same defect as (c).
	fourth := newRotWorld(t, "alice", "bob")
	fourthAlice, fourthBob := fourth.member("alice"), fourth.member("bob")
	fourthPublished := fourth.rotate(fourthAlice, func() ([]byte, []byte, []byte, error) {
		return fourthAlice.handle.Commit(nil)
	})
	if err := fourth.deliver(fourthBob, fourthPublished.wraps...); err != nil {
		t.Fatalf("CONTROL FAILED: bob's walk over the fan-out alone answered %v", err)
	}
	if staged := fourthBob.group.wrapsFor[fourthPublished.opens]; len(staged) != 1 {
		t.Fatalf("CONTROL FAILED: bob staged %d candidate(s) for epoch %d, want 1",
			len(staged), fourthPublished.opens)
	}
	fourthDigest, err := epochDigestOf(&fourthPublished.commit.record.Header)
	if err != nil || fourthDigest == nil {
		t.Fatalf("the digest on the rotation commit: %v %v", fourthDigest, err)
	}
	before := fourthBob.group.Stats().WrapOrphaned
	mlsSecret, err := fourthAlice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-%d exporter: %v", fourthPublished.opens, err)
	}
	// THE MISMATCH: the digest names the epoch it really opens, and the resolution is asked about
	// the one ABOVE it. That is the comparison the arm exists for.
	mismatched, err := fourthBob.group.resolvePqSecretLocked(mlsSecret, fourthPublished.opens+1,
		fourthDigest, nil)
	if err == nil {
		t.Fatalf("CONTROL FAILED: the epoch-mismatch arm answered %d octets instead of refusing, so "+
			"this clause is not driving it", len(mismatched))
	}
	if !errors.Is(err, ErrCommitIngest) {
		t.Fatalf("CONTROL FAILED: the epoch-mismatch arm answered %v, want ErrCommitIngest", err)
	}
	// NOTHING IS STAGED FOR THE EPOCH ASKED ABOUT, which is what makes this measure the arm and not
	// the loop: the candidates are filed under `opens`, the question is about `opens+1`.
	if staged := fourthBob.group.wrapsFor[fourthPublished.opens+1]; len(staged) != 0 {
		t.Fatalf("CONTROL FAILED: bob has %d candidate(s) staged for epoch %d", len(staged),
			fourthPublished.opens+1)
	}
	if got := fourthBob.group.Stats().WrapOrphaned - before; got != 0 {
		t.Fatalf("Stats.WrapOrphaned moved by %d on an epoch with no candidates at all, want 0; the "+
			"counter is supposed to be the candidates for the epoch ASKED ABOUT", got)
	}
	// AND NOW THE SAME ARM WITH CANDIDATES UNDER THE EPOCH IT IS ASKED ABOUT.
	fourthBob.group.wrapsFor[fourthPublished.opens+1] = []wrapCandidate{
		{recordId: 1, secret: append([]byte(nil), stray...)},
	}
	if _, err := fourthBob.group.resolvePqSecretLocked(mlsSecret, fourthPublished.opens+1,
		fourthDigest, nil); !errors.Is(err, ErrCommitIngest) {
		t.Fatalf("the epoch-mismatch arm answered %v, want ErrCommitIngest", err)
	}
	if got := fourthBob.group.Stats().WrapOrphaned - before; got != 1 {
		t.Fatalf("Stats.WrapOrphaned moved by %d on the epoch-mismatch arm with one staged "+
			"candidate, want 1. This arm returns before the candidate loop too, and a wrap that "+
			"opened for an epoch nothing used is exactly what this counter names", got)
	}
}

// ── 3. THE RESTART ───────────────────────────────────────────────────────────────────────────

// A DEVICE THAT RESTARTS AFTER A ROTATION COMES BACK WITH THE WHOLE TABLE AND OPENS ITS BACKLOG.
//
// WHAT WOULD GO RED: persist the epoch without the table (the restore refuses the group, because
// the record's own epoch has no secret); persist the table and not the past rows (the restored
// member re-derives a prior epoch's storage root out of today's secret, which is ruling 40's
// defect, and the record of that epoch fails at the AEAD tag); forget
// [GroupSession.DeclarePqSecretRotated] (the same, through connect's compatibility path).
func TestARestartAfterARotationComesBackWithTheTableAndOpensItsBacklog(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")

	atOne := world.storageRootOf(bob)
	first := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	if err := world.deliver(bob, first.page()...); err != nil {
		t.Fatalf("bob's walk over the first rotation: %v", err)
	}
	if bytes.Equal(atOne, world.storageRootOf(bob)) {
		t.Fatalf("nothing rotated")
	}
	second := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	if err := world.deliver(bob, second.page()...); err != nil {
		t.Fatalf("bob's walk over the second rotation: %v", err)
	}

	// ── THE DISK, READ BACK ─────────────────────────────────────────────────────────────────
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the disk holds %d group record(s), want 1", len(records))
	}
	record := records[0]
	if record.Epoch != 3 {
		t.Fatalf("the persisted record names epoch %d, want 3", record.Epoch)
	}
	if len(record.PqSecrets) != 3 {
		t.Fatalf("the persisted table holds %d row(s), want 3 (epochs 1, 2 and 3)", len(record.PqSecrets))
	}
	for at, row := range record.PqSecrets {
		if row.Epoch != uint64(at)+1 {
			t.Fatalf("the persisted table's row %d names epoch %d; the table is not ascending from 1", at, row.Epoch)
		}
	}
	if !bytes.Equal(record.PqSecrets[1].PqSecret, first.pqSecret) ||
		!bytes.Equal(record.PqSecrets[2].PqSecret, second.pqSecret) {
		t.Fatalf("the persisted rows are not the secrets the two fan-outs delivered")
	}
	if bytes.Equal(record.PqSecrets[0].PqSecret, record.PqSecrets[1].PqSecret) {
		t.Fatalf("two of the persisted rows carry one value; a table of one secret is the scalar again")
	}

	// ── THE RESTORE, IN A DEVICE THAT DID NOT EXIST WHEN ANY OF IT HAPPENED ──────────────────
	revived := restoredRotDevice(t, bob)
	restored, err := revived.device.restoreOne(revived.store, record, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne: %v", err)
	}
	defer restored.Close()
	if restored.Epoch() != 3 {
		t.Fatalf("the restored group stands at epoch %d, want 3", restored.Epoch())
	}
	for _, row := range record.PqSecrets {
		held, found := restored.pqSecretAtLocked(row.Epoch)
		if !found {
			t.Fatalf("the restored group holds no pq_secret for epoch %d", row.Epoch)
		}
		if !bytes.Equal(held, row.PqSecret) {
			t.Fatalf("the restored group's pq_secret for epoch %d is not the one on the disk", row.Epoch)
		}
	}

	// AND THE BACKLOG. Epoch ONE's whole key schedule is rebuilt by the restored device, which
	// needs pq_secret[1] and cannot be done with pq_secret[3]: this is the assertion the table
	// exists for, and it is the one a restore that came back with the scalar alone cannot make.
	if err := rotPastEpochReachable(restored.session, 1, alice.leaf); err != nil {
		t.Fatalf("the restored device cannot rebuild epoch 1: %v", err)
	}

	// THE PLANTED NEGATIVE CONTROL, in the same case and on the same door: the same restore with
	// epoch one's ROW REMOVED must refuse that epoch BY NAME. Without it the clause above is
	// satisfiable by a session that answers every epoch out of whatever it holds -- which is
	// exactly connect's group-lifetime premise, and exactly the reading a rotated group must not
	// take. The two rows left behind carry two different values, so the premise is refuted by the
	// OCTETS and the refusal is the table's rather than a version flag's.
	trimmed := &GroupRecord{
		GroupId:        record.GroupId,
		PqSecret:       record.PqSecret,
		PqSecrets:      []EpochPqSecret{record.PqSecrets[1], record.PqSecrets[2]},
		GroupHandleKey: record.GroupHandleKey,
		Epoch:          record.Epoch,
		Opened:         true,
	}
	blind, err := revived.device.restoreOne(revived.store, trimmed, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over a trimmed table: %v", err)
	}
	defer blind.Close()
	if err := rotPastEpochReachable(blind.session, 1, alice.leaf); !errors.Is(err, messagegroup.ErrPqSecretUnknownEpoch) {
		t.Fatalf("CONTROL FAILED: a restore whose table has no row for epoch 1 answered %v for that "+
			"epoch, want ErrPqSecretUnknownEpoch. The clause above is then measuring a door that "+
			"answers every epoch out of today's secret", err)
	}
}

// THE WALK'S STICKY REFUSALS OUTRANK THE TRANSPORT'S OWN, AND THE TRANSPORT'S OUTRANKS THE
// WALK'S PER-WALK ANSWERS. That ordering is [Group.commitWalkLocked]'s whole contract with
// [Group.Receive] since the transport error became a parameter, and it is driven here directly
// because the arm it exists for -- a server REFUSING a dark group's fetch -- cannot be reached
// from this package: there is no transport in it.
//
// IT IS HELD IN BOTH DIRECTIONS, over the same walk and the same group, because "the sticky one
// wins" is satisfiable by a function that ALWAYS answers the sticky one and has stopped reporting
// the transport at all. The second clause is what refuses that.
//
// WHY THE ORDER IS THIS WAY ROUND, in one sentence, since it is a decision and not a discovery: a
// dark group's read_key is wrong, so the server refuses req_auth before it reaches any AEAD --
// the refusal is the SYMPTOM and [Group.wrapDark] is the cause, and a caller told the symptom
// retries for ever.
func TestTheWalksStickyRefusalsOutrankTheTransportsOwn(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	bob := world.member("bob")
	group := bob.group

	refused := fmt.Errorf("%w: %v", ErrFetchRefused, protocol.Reason_REASON_REJECTED)
	fresh := func() *pageWalk {
		return &pageWalk{
			own: map[[16]byte]bool{}, leaves: map[uint64]map[[16]byte]uint32{},
			opened: []*Message{},
			from:   group.cursor, reached: group.cursor, resolvedTo: group.cursor,
			reconciled: group.reconciled, complete: true, unobtainable: map[uint64]bool{},
		}
	}

	// ── ONE: with no sticky refusal standing, the transport's error IS the answer. This is the
	// control, and it fires for its own reason: it is the behaviour the repair must not have
	// replaced with a blanket "always answer the walk".
	if group.wrapDark != nil || group.identityInUse != nil {
		t.Fatalf("this case's fixture is already refusing, so clause one measures nothing")
	}
	if err := group.commitWalkLocked(fresh(), refused); !errors.Is(err, ErrFetchRefused) {
		t.Fatalf("with nothing sticky standing, a refused fetch answered %v, want ErrFetchRefused", err)
	}

	// ── TWO: the sticky diagnosis, and now the SAME refusal must not be what the caller is told.
	group.wrapDark = fmt.Errorf("%w: 1 wrap(s) at this device's own wrap_target_handle for epoch "+
		"2 did not open", ErrWrapUnreadable)
	err := group.commitWalkLocked(fresh(), refused)
	if !errors.Is(err, ErrWrapUnreadable) {
		t.Fatalf("a dark group answered %v for a refused fetch, want the sticky ErrWrapUnreadable. "+
			"That is the undiagnosable REASON_REJECTED ruling 38 exists to prevent, and it is the "+
			"arm a dark group takes on EVERY fetch after the first", err)
	}
	if errors.Is(err, ErrFetchRefused) {
		t.Fatalf("a dark group's answer still matches ErrFetchRefused, so a caller that reads the " +
			"refusal as transport will retry a group that cannot recover by being retried")
	}

	// ── THREE: it is STICKY, so the next walk says it again -- with no fetch error at all, which
	// is the arm the urmessage harness reaches and the one mutant M9 of the previous commit drove.
	if err := group.commitWalkLocked(fresh(), nil); !errors.Is(err, ErrWrapUnreadable) {
		t.Fatalf("the second walk of a dark group answered %v, want the sticky ErrWrapUnreadable", err)
	}

	// ── FOUR: the identity refusal outranks BOTH, which is the order the function's own doc
	// states and the only one that keeps a device from carrying on producing a collision.
	group.identityInUse = fmt.Errorf("%w: a second copy of this identity", ErrIdentityInUse)
	if err := group.commitWalkLocked(fresh(), refused); !errors.Is(err, ErrIdentityInUse) {
		t.Fatalf("with both sticky refusals standing, the answer was %v, want ErrIdentityInUse", err)
	}
	group.identityInUse = nil
	group.wrapDark = nil
}

// ── 4. AN OLD STORE ──────────────────────────────────────────────────────────────────────────

// A GROUP RECORD WRITTEN BEFORE THE TABLE RESTORES, AND THE DEVICE GOES ON WORKING -- INCLUDING
// THROUGH THE FIRST ROTATION IT MEETS.
//
// EVERY GROUP ON THE DEPLOYED ALPHA IS THIS SHAPE: five parts, one pq_secret scalar, no table. A
// restore that refused one is a device that can never start again, so the read path takes both
// arities and the scalar becomes the one row it is evidence for -- this record's own epoch.
//
// AND "KEEPS WORKING" IS MEASURED AS FOLLOWING THE NEXT ROTATION, not as a restore that returned
// nil. A device that came back perfectly and then could not open the wrap addressed to it at the
// next commit would have been dark ten seconds later, which is the failure this whole commit is
// about.
func TestAGroupRecordWrittenBeforeTheTableRestoresAndFollowsTheNextRotation(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")
	bobLeaf := bob.leaf

	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the disk holds %d group record(s), want 1", len(records))
	}

	// THE OLD SHAPE, BUILT THE WAY THE OLD BUILD BUILT IT: PqSecrets nil is what a five-part
	// record decodes to, and it is the signal -- a six-part record with an empty table decodes to
	// an EMPTY SLICE, which is a different state and must not be answered the same way.
	old := &GroupRecord{
		GroupId:        records[0].GroupId,
		PqSecret:       append([]byte(nil), world.founding...),
		GroupHandleKey: records[0].GroupHandleKey,
		Epoch:          records[0].Epoch,
		Opened:         true,
	}
	if old.PqSecrets != nil {
		t.Fatalf("this case's fixture is not the old shape")
	}

	revived := restoredRotDevice(t, bob)
	restored, err := revived.device.restoreOne(revived.store, old, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over a store written before the table: %v", err)
	}
	defer restored.Close()
	if restored.Epoch() != old.Epoch {
		t.Fatalf("the restored group stands at epoch %d, want %d", restored.Epoch(), old.Epoch)
	}
	held, found := restored.pqSecretAtLocked(old.Epoch)
	if !found || !bytes.Equal(held, world.founding) {
		t.Fatalf("the restored group does not hold the scalar at the epoch the record names")
	}
	// THE TABLE IS EXACTLY THE WINDOW AT OR BELOW THE EPOCH THE RECORD NAMES, AND EVERY ROW IS THE
	// SCALAR. This clause read `len(restored.pqSecrets) != 1` until the case below it was written,
	// on the reasoning that "the scalar is evidence for exactly one epoch". That reasoning was
	// wrong in the direction that loses history -- see
	// TestAFivePartRecordRestoredAboveABacklogKeepsItAcrossTheFirstRotation, where one row cost a
	// restored device every epoch under it at its first rotation -- and this replacement is
	// strictly stronger than the count it removed: it pins WHICH epochs are filed, in both
	// directions, and the OCTETS of every row, so a reader that invented a value, filed an epoch
	// the record is no evidence for, or stopped filing one is red here.
	lowest := uint64(0)
	if messagegroup.PastEpochWindow < old.Epoch {
		lowest = old.Epoch - messagegroup.PastEpochWindow
	}
	for epoch := lowest; epoch <= old.Epoch; epoch += 1 {
		row, filed := restored.pqSecretAtLocked(epoch)
		if !filed || !bytes.Equal(row, world.founding) {
			t.Fatalf("the restored table has no row carrying the scalar at epoch %d, and a "+
				"five-part record is evidence for every epoch in the window at or below the one "+
				"it names", epoch)
		}
	}
	if uint64(len(restored.pqSecrets)) != old.Epoch-lowest+1 {
		t.Fatalf("the restored table holds %d row(s) and the window at or below epoch %d is %d "+
			"epochs wide; a row outside it is a value nobody wrote",
			len(restored.pqSecrets), old.Epoch, old.Epoch-lowest+1)
	}
	if pqSecretsShowRotation([]restoredPqSecret{
		{epoch: lowest, secret: restored.pqSecrets[lowest]},
		{epoch: old.Epoch, secret: restored.pqSecrets[old.Epoch]},
	}) {
		t.Fatalf("the filled window reads as a ROTATION, which would take the compatibility path " +
			"away from every group on the alpha")
	}
	// A RESTORED GROUP IS NOT RECONCILED, which is this package's standing rule and not this
	// case's subject; a Receive is what reconciles one, and the walk below is what a Receive does
	// under the fetch.
	restored.reconciled = true

	// ── AND NOW THE ROTATION IT MEETS ───────────────────────────────────────────────────────
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	back := &rotMember{name: "bob after the restart", root: bob.root, dev: bob.dev,
		handle: restored.handle, session: restored.session, group: restored, leaf: bobLeaf}
	if err := world.deliver(back, published.page()...); err != nil {
		t.Fatalf("the restored device's walk over the first rotation it meets: %v", err)
	}
	if restored.Epoch() != published.opens {
		t.Fatalf("the restored device stands at epoch %d and the rotation opened %d",
			restored.Epoch(), published.opens)
	}
	if !bytes.Equal(restored.pqSecretLocked(), published.pqSecret) {
		t.Fatalf("the restored device did not install the secret the fan-out carried")
	}
	if restored.wrapDark != nil {
		t.Fatalf("the restored device went dark at the first rotation it met: %v", restored.wrapDark)
	}
	if got := world.storageRootOf(back); !bytes.Equal(got, world.storageRootOf(alice)) {
		t.Fatalf("after the rotation the restored device's storage root is not the committer's")
	}

	// AND IT WRITES THE NEW SHAPE BACK, so the compatibility path is a one-way door rather than a
	// state a group is stuck in: the record this device has now persisted carries the table.
	after, err := revived.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords after the rotation: %v", err)
	}
	if len(after) != 1 || after[0].PqSecrets == nil {
		t.Fatalf("the record written after the rotation carries no table")
	}
	if len(after[0].PqSecrets) != len(restored.pqSecrets) {
		t.Fatalf("the record written after the rotation holds %d row(s) and the group holds %d",
			len(after[0].PqSecrets), len(restored.pqSecrets))
	}
}

// A FIVE-PART RECORD RESTORED **ABOVE A BACKLOG** KEEPS THAT BACKLOG ACROSS ITS FIRST ROTATION.
//
// THE CASE ABOVE CANNOT SEE THIS AND THAT IS THE FINDING. It restores at epoch one, which has no
// history under it, so "the scalar is evidence for exactly one epoch" and "the scalar is evidence
// for every epoch this group has lived through" are the same sentence there. They are not the same
// sentence for a long-lived device, and the difference costs it everything below its restore epoch
// the moment it follows one rotation:
//
//   - `restoredPqSecrets` filed the scalar for ONE epoch, the one the record names;
//   - connect's group-lifetime premise answered every epoch below it -- correctly, and only while
//     the premise stood;
//   - the first rotation installs a DIFFERENT value, `installPqSecretOnLoop` refutes the premise on
//     the octets, and the epochs below the restore point now have neither a row nor the premise.
//
// Reproduced before it was repaired, with this exact case: epochs 1 and 2 reachable BEFORE the
// rotation and `ErrPqSecretUnknownEpoch` after it. It is not a corner: [Group.cursor] is in-memory
// only, so every restart re-walks the group from record zero, and a walk that cannot open the
// epochs below its restore point retries each of those records [maxRecordAttempts] times and
// abandons them. THE WORSE OUTCOME OF THE TWO the task names -- the device starts, and fails later.
//
// THE PREMISE THE REPAIR RESTS ON, stated so it can be argued with: a FIVE-PART record was written
// by a build that could not rotate, so that group ran group-lifetime for the whole of its life up
// to the epoch the record names. Filing the scalar across the window is therefore not inventing
// evidence -- it is writing down, as rows that survive a refutation, exactly the answers connect's
// premise was already giving for exactly those epochs.
//
// THE HISTORY HAS TO BE BUILT THE OLD WAY OR THE CASE IS A FICTION: see
// [rotWorld.advanceWithoutRotating]. A backlog built with [rotWorld.rotate] would be a past in
// which every epoch had its own secret, and a five-part record naming its top is a record no build
// ever wrote.
func TestAFivePartRecordRestoredAboveABacklogKeepsItAcrossTheFirstRotation(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")
	bobLeaf := bob.leaf
	restoreAt := bob.group.epoch

	// TWO EPOCH CHANGES THE OLD WAY, so the group's whole history ran on the founding scalar.
	for at := 0; at < 2; at += 1 {
		published := world.advanceWithoutRotating(alice, func() ([]byte, []byte, []byte, error) {
			return alice.handle.Commit(nil)
		})
		if err := world.deliver(bob, published.page()...); err != nil {
			t.Fatalf("bob's walk over a non-rotating epoch change: %v", err)
		}
		if !bytes.Equal(bob.group.pqSecretLocked(), world.founding) {
			t.Fatalf("a non-rotating epoch change changed the secret, so this case's history is " +
				"not the one a build before this commit produced")
		}
	}
	backlog := []uint64{restoreAt, restoreAt + 1}
	top := bob.group.epoch
	if top != restoreAt+2 {
		t.Fatalf("this case's fixture stands at epoch %d, want %d", top, restoreAt+2)
	}

	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the disk holds %d group record(s), want 1", len(records))
	}
	old := &GroupRecord{
		GroupId:        records[0].GroupId,
		PqSecret:       append([]byte(nil), world.founding...),
		GroupHandleKey: records[0].GroupHandleKey,
		Epoch:          top,
		Opened:         true,
	}
	if old.PqSecrets != nil {
		t.Fatalf("this case's fixture is not the old shape")
	}

	revived := restoredRotDevice(t, bob)
	restored, err := revived.device.restoreOne(revived.store, old, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over a five-part record above a backlog: %v", err)
	}
	defer restored.Close()
	restored.reconciled = true

	// ── BEFORE, WHICH IS THE CONTROL: the backlog is reachable, so the comparison below is
	// about the ROTATION and not about a restore that never had those epochs at all.
	for _, epoch := range backlog {
		if err := rotPastEpochReachable(restored.session, epoch, alice.leaf); err != nil {
			t.Fatalf("CONTROL FAILED: epoch %d is not reachable BEFORE the rotation (%v), so this "+
				"case cannot show a rotation taking it away", epoch, err)
		}
	}

	// ── ONE ORDINARY ROTATION ───────────────────────────────────────────────────────────────
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	back := &rotMember{name: "bob after the restart", root: bob.root, dev: bob.dev,
		handle: restored.handle, session: restored.session, group: restored, leaf: bobLeaf}
	if err := world.deliver(back, published.page()...); err != nil {
		t.Fatalf("the restored device's walk over the first rotation it meets: %v", err)
	}
	if restored.wrapDark != nil {
		t.Fatalf("the restored device went dark at the first rotation it met: %v", restored.wrapDark)
	}
	if !bytes.Equal(restored.pqSecretLocked(), published.pqSecret) {
		t.Fatalf("the restored device did not install the secret the fan-out carried, so the " +
			"premise was never refuted and this case is measuring nothing")
	}

	// ── AFTER, WHICH IS THE PROPERTY ────────────────────────────────────────────────────────
	for _, epoch := range backlog {
		if err := rotPastEpochReachable(restored.session, epoch, alice.leaf); err != nil {
			t.Fatalf("epoch %d became unreachable the moment this device followed its first "+
				"rotation: %v. The premise that answered it has been refuted, and the row that "+
				"should have replaced the premise was never filed", epoch, err)
		}
	}

	// AND THE WINDOW IS THE BOUND, not the whole history: an epoch below it is refused BY NAME,
	// which is the second direction and is what keeps this repair from claiming a history it
	// cannot serve. It is asserted against the same door, so a session that answered everything
	// would fail here instead.
	if messagegroup.PastEpochWindow < top {
		t.Fatalf("this case's fixture stands above the window, so the clause below is vacuous")
	}
	beyond := top + messagegroup.PastEpochWindow + 1
	if err := rotPastEpochReachable(restored.session, beyond, alice.leaf); err == nil {
		t.Fatalf("epoch %d is above this session's own epoch by more than the window and was "+
			"answered anyway", beyond)
	}
}

// restoredRotDevice closes one member's process and opens a SECOND over the same directories, the
// way a restart does: a device that did not exist when anything above happened, reading the same
// disk.
func restoredRotDevice(t *testing.T, member *rotMember) *restoreDevice {
	t.Helper()
	member.session.Close()
	member.handle.Close()
	member.dev.close()
	revived := openRestoreDevice(t, member.root)
	t.Cleanup(revived.close)
	return revived
}

// rotPastEpochReachable is whether a session can rebuild a PRIOR epoch's whole key schedule, which
// is the one observable that needs pq_secret AT THAT EPOCH and nothing else. connect exports no
// storage root, so this is the question asked through the door that consumes one: TrackSenderAt
// routes through pastEpochOnLoop, which asks pqSecretForOnLoop for the epoch and then extracts
// that epoch's root from it.
func rotPastEpochReachable(session *messagegroup.GroupSession, epoch uint64, leaf uint32) error {
	return session.TrackSenderAt(epoch, leaf, message.RetentionDurable, 0, 0, 0)
}

// ── 5. THE TWIN THIS TASK COULD NOT BUILD, MEASURED RATHER THAN EXCUSED ──────────────────────

// m1 TASK 14 PROPERTY 1 IS "EXACTLY TWO" AND connect REFUSES THE SECOND, AND THAT REFUSAL IS
// MEASURED HERE RATHER THAN DESCRIBED IN A COMMENT.
//
// The device wrap is ruled as two records per target -- a PERMANENT one carrying pq_secret[k] and
// an EPH(5) one carrying eph_root[k] -- which is what makes MASTER §8.1's disappearing-message
// promise cryptographic rather than behavioural, and it is what MASTER §8.2's
// expected_wrap_count = 2 x device_leaves + 1 counts. This commit ships ONE record per target,
// because connect's sealer answers ErrEphWrapWindowUnruled for any EPH record carrying a wrap tag:
// ledger open item 185 leaves that record's eph_window unstated, Spec A S19 and Spec B §5.1 check
// 3 refuse an implausible one, and a wrap head has no sent_at to divide.
//
// AN EXCUSE CAN BE FALSE AND NOT MERELY UNMEASURED, so this case runs the seal and holds the
// refusal by sentinel. IT GOES RED THE DAY ITEM 185 IS RULED AND THE REFUSAL LIFTS, which is
// exactly when the second record becomes due and expected_wrap_count has to change with it.
func TestTheEphRootTwinOfTheDeviceWrapIsRefusedByConnectAndNotByThisPackage(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice := world.member("alice")

	// THE CONTROL, IN THE SAME QUERY: the PERMANENT half of the same pair, at the same target,
	// through the same door, is sealed. Without it a refusal here would be satisfiable by a
	// session that seals nothing at all.
	//
	// THROUGH THE FOUNDING DOOR, because this world has staged no commit: every member of a
	// [newRotWorld] cohort was admitted by the founding CommitAdd and merged, so
	// [Group.wrapTargetsAtLocked] would have no staged commit to read its exclusion off. What this
	// case needs is one target's handle and nothing about a removal.
	targets, err := alice.group.foundingWrapTargetsLocked(alice.group.epoch)
	if err != nil {
		t.Fatalf("the wrap targets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatalf("this group has no wrap target")
	}
	if _, err := alice.group.sealEpochWrapLocked(alice.group.epoch, targets[0], world.founding); err != nil {
		t.Fatalf("CONTROL FAILED: the PERMANENT half of the pair did not seal either: %v", err)
	}

	_, err = alice.group.session.SealRecord(message.RetentionEph, 5, false,
		encodeHead(time.Now().UnixMilli()), []byte("an eph_root device wrap this build cannot publish"), 0,
		&message.ServerAttachment{
			Kind: message.AttachmentWrap,
			Wrap: &message.WrapTag{WrapTargetHandle: append([]byte(nil), targets[0].handle[:]...), Epoch: alice.group.epoch},
		})
	if !errors.Is(err, messagegroup.ErrEphWrapWindowUnruled) {
		t.Fatalf("connect answered %v to an EPH(5) record carrying a wrap tag, want ErrEphWrapWindowUnruled. "+
			"If ledger item 185 has been ruled and the refusal lifted, this package now owes the SECOND "+
			"record of m1 Task 14 Property 1 and expected_wrap_count owes MASTER §8.2's 2 x device_leaves + 1", err)
	}
	t.Logf("the eph_root twin is refused by connect: %v", err)
}

// ── 6. THE TABLE'S OWN DISCIPLINE ────────────────────────────────────────────────────────────

// THE TABLE IS BOUNDED BY PastEpochWindow AND AN EVICTED ENTRY IS ERASED, NOT DROPPED.
//
// An entry more than [messagegroup.PastEpochWindow] behind can serve no open connect would admit,
// so holding it is holding a retired epoch's post-quantum secret for nothing. The erase is the
// half a `delete` alone would miss, and it is measured on the OCTETS of the array that was
// evicted -- a map entry nobody blanked is a live secret with no owner.
func TestThePqSecretTableIsBoundedByTheWindowAndAnEvictedEntryIsErased(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice := world.member("alice")
	group := alice.group

	const standing = uint64(messagegroup.PastEpochWindow) + 7
	evicted := append([]byte(nil), world.founding...)
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.pqSecrets[standing] = evicted
	group.epoch = standing
	group.dropPqSecretsBelowWindowLocked()
	if _, held := group.pqSecretAtLocked(standing); !held {
		t.Fatalf("CONTROL FAILED: an entry at the session's own epoch was dropped, so the " +
			"assertion below measures a function that drops everything")
	}

	// the entry the window moves past
	far := make([]byte, messagegroup.PqSecretBytes)
	for at := range far {
		far[at] = 0xA5
	}
	group.pqSecrets[standing-messagegroup.PastEpochWindow-1] = far
	group.dropPqSecretsBelowWindowLocked()
	if _, held := group.pqSecretAtLocked(standing - messagegroup.PastEpochWindow - 1); held {
		t.Fatalf("an entry %d epochs behind survived a window of %d",
			messagegroup.PastEpochWindow+1, messagegroup.PastEpochWindow)
	}
	if containsNonZeroOctet(far) {
		t.Fatalf("the evicted entry was dropped and not erased: %d non-zero octet(s) remain",
			countNonZeroOctets(far))
	}

	// AND THE EDGE IS EXACTLY THE WINDOW, held in the other direction so the bound is not simply
	// "drops everything old".
	edge := make([]byte, messagegroup.PqSecretBytes)
	for at := range edge {
		edge[at] = 0x5A
	}
	group.pqSecrets[standing-messagegroup.PastEpochWindow] = edge
	group.dropPqSecretsBelowWindowLocked()
	if _, held := group.pqSecretAtLocked(standing - messagegroup.PastEpochWindow); !held {
		t.Fatalf("an entry exactly %d epochs behind was dropped; the window is one short",
			messagegroup.PastEpochWindow)
	}
	if !containsNonZeroOctet(edge) {
		t.Fatalf("the entry at the window's edge was erased while it was still held")
	}
	_ = fmt.Sprint(evicted)
}

// THE WITNESS THE WINDOW DOES NOT PRUNE SURVIVES A RESTART, AND THE RECORD THAT CARRIES NO WITNESS
// IS THE RESIDUAL, MEASURED RATHER THAN NAMED.
//
// WHY IT IS A SEPARATE CASE FROM THE TABLE'S OWN RESTART. [GroupRecord.PqSecrets] is bounded on
// purpose and must stay bounded -- a secret further behind than [messagegroup.PastEpochWindow] can
// serve no open, so persisting it is persisting a retired epoch's post-quantum half for nothing.
// The removal rule needs the opposite thing: whether this group has EVER held a value, which the
// removed member's own copy does not forget. So the answer is persisted and the secret is not, and
// this case asserts the two DISAGREE on the disk -- the witness names an epoch whose secret the
// table no longer carries.
//
// THE CONTROL IS A VALUE NOBODY EVER HELD, inline: it answers false through the same call. Without
// it a witness that answered true to everything would pass this, which is the failure mode that
// would refuse every honest rotation in the package.
//
// AND THE RESIDUAL IS THE SAME RECORD WITH THE WITNESS PART TAKEN OFF -- a store written by a build
// before that part existed. It comes back witnessing only what its table carries, and that device
// follows a removal fanned out on a secret it once held, evicted and forgot. The only repair is on
// the wire: the wrap payload authenticated as drawn FOR the epoch it opens. That is a `connect`
// change and is filed rather than taken here, so what this case does is MEASURE the hole instead of
// claiming it closed.
func TestARestartKeepsThePqSecretWitnessTheWindowDoesNotPrune(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")
	retained := append([]byte(nil), bob.group.pqSecretLocked()...)

	const rotations = int(messagegroup.PastEpochWindow) + 1
	for at := 0; at < rotations; at += 1 {
		published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
			return alice.handle.Commit(nil)
		})
		if err := world.deliver(bob, published.page()...); err != nil {
			t.Fatalf("CONTROL FAILED: bob's walk over honest rotation %d answered %v", at+1, err)
		}
	}

	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("bob's disk holds %d group record(s), want 1", len(records))
	}
	record := records[0]
	// ── THE DISK DISAGREES WITH ITSELF, ON PURPOSE ──────────────────────────────────────────
	for _, row := range record.PqSecrets {
		if bytes.Equal(row.PqSecret, retained) {
			t.Fatalf("CONTROL FAILED: the persisted TABLE still carries epoch 1's secret at epoch "+
				"%d after %d rotations, so the witness is not carrying anything the table does not",
				row.Epoch, rotations)
		}
	}
	witnessed := sha256.Sum256(retained)
	found := false
	for _, row := range record.PqSecretWitness {
		if bytes.Equal(row.Digest, witnessed[:]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the persisted witness holds %d row(s) and none of them is epoch 1's; a restart "+
			"would put the removal rule's subject back inside the window that defeated it",
			len(record.PqSecretWitness))
	}
	t.Logf("the record carries %d secret row(s) and %d witness row(s): the secrets are bounded by "+
		"the window and the answer to 'have I ever held this' is not",
		len(record.PqSecrets), len(record.PqSecretWitness))

	// ── THE RESTART ─────────────────────────────────────────────────────────────────────────
	revived := restoredRotDevice(t, bob)
	restored, err := revived.device.restoreOne(revived.store, record, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne: %v", err)
	}
	if _, everHeld := restored.pqSecretHeldAtLocked(retained); !everHeld {
		t.Fatalf("the restored group has no record that it ever held epoch 1's secret, so a " +
			"removal fanned out on that value is followed after every restart")
	}
	for _, secret := range restored.pqSecrets {
		if bytes.Equal(secret, retained) {
			t.Fatalf("CONTROL FAILED: the restored LIVE table carries epoch 1's secret, so the " +
				"clause above is answered by the table and not by the witness")
		}
	}
	stranger := make([]byte, messagegroup.PqSecretBytes)
	for at := range stranger {
		stranger[at] = 0x6E
	}
	if _, everHeld := restored.pqSecretHeldAtLocked(stranger); everHeld {
		t.Fatalf("CONTROL FAILED: the restored witness answers 'already held' for a value this " +
			"group has never seen, so it would refuse every honest rotation")
	}
	restored.Close()

	// ── THE RESIDUAL: THE SAME RECORD AS A BUILD BEFORE THE WITNESS PART WROTE IT ───────────
	older := *record
	older.PqSecretWitness = nil
	olderGroup, err := revived.device.restoreOne(revived.store, &older, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over a record with no witness part: %v", err)
	}
	defer olderGroup.Close()
	if _, everHeld := olderGroup.pqSecretHeldAtLocked(retained); everHeld {
		t.Fatalf("a record with NO witness part came back witnessing an epoch its table does not " +
			"carry. That would mean the witness is being invented from somewhere, and the honest " +
			"answer for that disk is that the pre-window history is gone")
	}
	t.Logf("THE RESIDUAL, MEASURED AND NOT CLAIMED CLOSED: a group restored from a record written "+
		"before the witness part witnesses only the %d row(s) its table carries, so it follows a "+
		"removal fanned out on a value it held before the window moved. The repair is on the wire: "+
		"the wrap payload authenticated as drawn FOR the epoch it opens, which is a connect change",
		len(older.PqSecrets))
}

func containsNonZeroOctet(octets []byte) bool {
	return countNonZeroOctets(octets) != 0
}

func countNonZeroOctets(octets []byte) int {
	count := 0
	for _, one := range octets {
		if one != 0 {
			count += 1
		}
	}
	return count
}

// mls is imported for the seam's own types in the harness above.
var _ = mls.RoleOwner
