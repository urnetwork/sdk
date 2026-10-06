package urmessage

import (
	"crypto/rand"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/v2026/message"
	"github.com/urnetwork/connect/v2026/messagegroup"
	"github.com/urnetwork/connect/v2026/mls"
	"github.com/urnetwork/connect/v2026/mls/syntax"
	"github.com/urnetwork/connect/v2026/protocol"
)

// ── the receiving arm, end to end, through real devices and the seam's own commits ──────────
//
// WHAT IS REAL HERE. Every member is a [crossProcessDevice] -- the package's own deviceIdentity
// over a durable store, the shipped messagegroup engine, a real GroupSession -- and every
// receiver is a [Group] built the way newKindWalk builds one, driving [Group.openPageLocked] and
// [Group.commitWalkLocked] over a page of records exactly as [Group.Receive] does below the fetch.
// The HOSTILE committer is a real device too: it builds its commit through the seam's by-value
// arms (CommitRemove, CommitPolicy, CommitContextExtensions, CommitAdd), by reference through
// Propose* and Commit(nil) where no by-value arm carries the shape, or -- for the one shape both
// send doors refuse -- on the live *mls.Group behind its handle, as a hostile mls build would;
// merges it; and seals the commit record through its session the way [Group.publishCommitLocked]
// seals one -- it drives the seam directly and never a verb, so R2's send-side check (rolescommit.go)
// is not in its way and nothing stops it from committing what its role does not permit. What is NOT
// here is the server, which is the cp3b module's; the
// server accepts every commit anyway, so what a refusal costs (the receiver stays at n, the
// server has moved on) is the same with or without it.

// roleMember is one member of the role world: its device, handle, session and receiving group.
type roleMember struct {
	name    string
	dev     *crossProcessDevice
	handle  messagegroup.GroupHandle
	session *messagegroup.GroupSession
	group   *Group
	leaf    uint32
}

// roleWorld is one group of real members at a common epoch, with the server's record numbering
// assigned here because no server is present.
type roleWorld struct {
	t              *testing.T
	root           string
	groupId        []byte
	pqSecret       []byte
	groupHandleKey []byte
	members        map[string]*roleMember
	nextRecordId   uint64
}

// newRoleWorld founds a group with names[0] as its OWNER and adds every other name in ONE commit,
// so every member stands at epoch 1 with the founder the only named role -- exactly the shape
// every live group has (item 242: "every non-founder in every live group is UNNAMED").
func newRoleWorld(t *testing.T, names ...string) *roleWorld {
	t.Helper()
	world := &roleWorld{
		t:            t,
		root:         t.TempDir(),
		groupId:      make([]byte, GroupIdBytes),
		members:      map[string]*roleMember{},
		nextRecordId: 1,
	}
	if _, err := rand.Read(world.groupId); err != nil {
		t.Fatalf("drawing a group id: %v", err)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("pq_secret: %v", err)
	}
	world.pqSecret = pqSecret

	founder := world.device(names[0])
	founderHandle := founder.createGroup(t, world.groupId)
	t.Cleanup(func() { founderHandle.Close() })
	mlsSecret, err := founderHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	world.groupHandleKey = messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

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

func (self *roleWorld) device(name string) *crossProcessDevice {
	self.t.Helper()
	dev := openCrossProcessDevice(self.t, filepath.Join(self.root, name))
	self.t.Cleanup(dev.close)
	return dev
}

// enroll gives one member a session and a receiving [Group] over its handle, built field by field
// for newKindWalk's reason: everything below the fetch is what is under test.
func (self *roleWorld) enroll(name string, dev *crossProcessDevice, handle messagegroup.GroupHandle) *roleMember {
	self.t.Helper()
	session := newCrossProcessSession(self.t, handle, self.pqSecret, self.groupHandleKey, dev.reserver, name+"'s nonce")
	self.t.Cleanup(func() { session.Close() })
	group := &Group{
		device: &Device{
			stateStore:  dev.store,
			engine:      dev.engine,
			reserver:    dev.reserver,
			identityPub: append([]byte(nil), dev.identityPub...),
			nowMs:       func() int64 { return time.Now().UnixMilli() },
			random:      rand.Reader,
			groups:      map[string]*Group{},
		},
		id:             append([]byte(nil), self.groupId...),
		handle:         handle,
		groupHandleKey: self.groupHandleKey,
		pqSecrets:      map[uint64][]byte{handle.Epoch(): self.pqSecret},
		session:        session,
		epoch:          handle.Epoch(),
		opened:         true,
		reconciled:     true,
		// every member of this world is named in the FOUNDING commit, so no leaf here was
		// ever occupied by anybody else and this device's stream floor is known without
		// asking the server. See [Group.ownFloorHeld].
		ownFloorHeld: true,
	}
	group.initTables()
	// AND THE PAST EPOCH LOADER, WHICH PRODUCTION INSTALLS ON EVERY SESSION A GROUP RECEIVES
	// THROUGH (ledger item 241, [Device.pastEpochLoader]). Without it a member reads every record
	// from an epoch it has left as a loud ErrRecordOpen rather than opening it under that epoch's
	// own schedule -- which is not what a real member does, and is the road R4's capture is taken
	// on when a walk meets a commit before it meets the lines that preceded it.
	if err := session.InstallPastEpochLoader(group.device.pastEpochLoader(group.id)); err != nil {
		self.t.Fatalf("%s's past epoch loader: %v", name, err)
	}
	member := &roleMember{name: name, dev: dev, handle: handle, session: session, group: group, leaf: handle.OwnLeafIndex()}
	self.members[name] = member
	return member
}

func (self *roleWorld) member(name string) *roleMember {
	self.t.Helper()
	member, held := self.members[name]
	if !held {
		self.t.Fatalf("no member named %q", name)
	}
	return member
}

// publish seals one commit the committer has already built and MERGED as the record every other
// member walks, and moves the committer's session and group onto the epoch it opened: the (1)-(4)
// of [Group.AddMemberAndPublish] with no server in between. The record is sealed at the OLD epoch
// by the session, which is still there after the handle merged, exactly as production seals it.
func (self *roleWorld) publish(committer *roleMember, commit []byte) *sealed {
	self.t.Helper()
	record, err := committer.session.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(time.Now().UnixMilli()), commit, 0, nil)
	if err != nil {
		self.t.Fatalf("%s sealing its commit record: %v", committer.name, err)
	}
	newEpoch := committer.handle.Epoch()
	// the harness files what production's publishCommitLocked files, at the epoch it is entering.
	// This world does not rotate -- its whole subject is roles -- so the value is the same one,
	// and what the line buys is that the table covers the epoch enterEpochLocked persists.
	committer.group.filePqSecretLocked(newEpoch, self.pqSecret)
	if err := committer.session.AdvanceEpoch(self.pqSecret); err != nil {
		self.t.Fatalf("%s advancing its session to epoch %d: %v", committer.name, newEpoch, err)
	}
	if err := committer.group.crossEpochLadderLocked(newEpoch); err != nil {
		self.t.Fatalf("%s crossing the epoch: %v", committer.name, err)
	}
	if err := committer.group.enterEpochLocked(); err != nil {
		self.t.Fatalf("%s entering epoch %d: %v", committer.name, newEpoch, err)
	}
	one := &sealed{recordId: self.nextRecordId, record: record}
	self.nextRecordId += 1
	return one
}

// deliver walks one page holding the records through a receiver's group, as newKindWalk.deliver
// does, and answers what [Group.Receive] would: the walk's first failure.
func (self *roleWorld) deliver(receiver *roleMember, page ...*sealed) error {
	self.t.Helper()
	group := receiver.group
	own, err := group.session.SenderHandle()
	if err != nil {
		self.t.Fatalf("%s's sender handle: %v", receiver.name, err)
	}
	group.ownHandles[own] = true
	walk := &pageWalk{
		// the same three lines [Group.Receive] writes; the handle table is per record epoch
		// (ledger item 245) and [Group.walkLeavesLocked] fills it as the walk meets each epoch.
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
	// nil for the reason pqrotation_test.go names: no fetch, so no transport refusal.
	return group.commitWalkLocked(walk, nil)
}

// commitAndPublish runs one of the seam's by-value arms on the committer's handle, merges, and
// publishes the record.
func (self *roleWorld) commitAndPublish(committer *roleMember, what string,
	arm func() ([]byte, []byte, []byte, error)) *sealed {

	self.t.Helper()
	commit, _, _, err := arm()
	if err != nil {
		self.t.Fatalf("%s's %s: %v", committer.name, what, err)
	}
	if err := committer.handle.MergePendingCommit(); err != nil {
		self.t.Fatalf("%s's MergePendingCommit after %s: %v", committer.name, what, err)
	}
	return self.publish(committer, commit)
}

// policyOf is a member's current policy, decoded off its own group context.
func (self *roleWorld) policyOf(member *roleMember) *mls.GroupPolicyExtension {
	self.t.Helper()
	extensions, err := member.group.contextExtensionsLocked()
	if err != nil {
		self.t.Fatalf("%s's group context: %v", member.name, err)
	}
	policy, err := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
	if err != nil {
		self.t.Fatalf("%s's policy: %v", member.name, err)
	}
	return policy
}

// policyBody encodes a policy as the body CommitPolicy takes.
func (self *roleWorld) policyBody(policy *mls.GroupPolicyExtension) []byte {
	self.t.Helper()
	if err := policy.Canonicalize(); err != nil {
		self.t.Fatalf("canonicalizing: %v", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		self.t.Fatalf("encoding the policy: %v", err)
	}
	return encoded.ExtensionData
}

// refuse delivers one record to a receiver and holds the whole of what a refusal promises: the
// error wraps ErrCommitUnauthorized and the rule, the group and its handle stayed at the epoch
// they were at, nothing more was ingested, and the refusal was counted exactly once. It answers
// the error for a caller that wants to read more off it.
func (self *roleWorld) refuse(receiver *roleMember, record *sealed, rule error) error {
	self.t.Helper()
	epoch := receiver.group.Epoch()
	before := receiver.group.Stats()
	err := self.deliver(receiver, record)
	if err == nil {
		self.t.Fatalf("%s ingested the commit; it is now at epoch %d", receiver.name, receiver.group.Epoch())
	}
	if !errors.Is(err, ErrCommitUnauthorized) {
		self.t.Errorf("%s answered %v, which does not wrap ErrCommitUnauthorized", receiver.name, err)
	}
	if !errors.Is(err, rule) {
		self.t.Errorf("%s answered %v, which does not wrap the rule %v", receiver.name, err, rule)
	}
	if got := receiver.group.Epoch(); got != epoch {
		self.t.Errorf("%s's group is at epoch %d after a refusal, want %d", receiver.name, got, epoch)
	}
	if got := receiver.handle.Epoch(); got != epoch {
		self.t.Errorf("%s's handle is at epoch %d after a refusal, want %d: the commit was applied", receiver.name, got, epoch)
	}
	after := receiver.group.Stats()
	if after.CommitRefused != before.CommitRefused+1 {
		self.t.Errorf("%s's Stats.CommitRefused went %d -> %d, want one more", receiver.name, before.CommitRefused, after.CommitRefused)
	}
	if after.Ingested != before.Ingested {
		self.t.Errorf("%s's Stats.Ingested went %d -> %d over a refusal", receiver.name, before.Ingested, after.Ingested)
	}
	return err
}

// ingest delivers one record to a receiver and holds the allowed half: no error, the epoch moved
// by one, nothing was counted as refused, and the receiver's exporter agrees with the committer's
// -- which an epoch counter cannot say.
func (self *roleWorld) ingest(receiver *roleMember, committer *roleMember, record *sealed) {
	self.t.Helper()
	epoch := receiver.group.Epoch() + 1
	before := receiver.group.Stats()
	err := self.deliver(receiver, record)
	if err != nil {
		self.t.Fatalf("%s refused a commit the rules allow: %v", receiver.name, err)
	}
	if got := receiver.group.Epoch(); got != epoch {
		self.t.Fatalf("%s's group is at epoch %d, want %d", receiver.name, got, epoch)
	}
	after := receiver.group.Stats()
	if after.CommitRefused != before.CommitRefused {
		self.t.Errorf("%s counted a refusal over an allowed commit", receiver.name)
	}
	if after.Ingested != before.Ingested+1 {
		self.t.Errorf("%s's Stats.Ingested went %d -> %d, want one more", receiver.name, before.Ingested, after.Ingested)
	}
	mine, err := receiver.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's exporter: %v", receiver.name, err)
	}
	theirs, err := committer.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's exporter: %v", committer.name, err)
	}
	if string(mine) != string(theirs) {
		self.t.Errorf("%s and %s disagree about the epoch %d exporter", receiver.name, committer.name, epoch)
	}
}

// A MEMBER'S REMOVE OF THE OWNER IS REFUSED BY EVERY HONEST RECEIVER: item 242's Q2, the hole
// this arm exists to close. Before R1 every honest receiver applied it and the policy went on
// naming an identity with no leaf as owner.
func TestAMembersRemoveOfTheOwnerIsRefusedByEveryHonestReceiver(t *testing.T) {
	world := newRoleWorld(t, "owner", "mallory", "carol")
	owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")

	record := world.commitAndPublish(mallory, "CommitRemove of the owner", func() ([]byte, []byte, []byte, error) {
		return mallory.handle.CommitRemove([]uint32{owner.leaf})
	})
	if mallory.group.Epoch() != 2 {
		t.Fatalf("the hostile committer is at epoch %d after its own merge, want 2", mallory.group.Epoch())
	}
	for _, honest := range []*roleMember{owner, carol} {
		world.refuse(honest, record, mls.ErrAdminRemovedByNonOwner)
	}
	// THE RECORD IS RETRIED LIKE ANY INGEST FAILURE AND THE RETRY NEVER REACHES THE RULES: the
	// first Process opened the commit's MLS frame and spent that generation of the committer's
	// ratchet, so the second walk is refused by mls at Process, as an ingest failure and not as
	// a second refusal. One refusal is counted once; the receiver is still at the epoch before.
	err := world.deliver(carol, record)
	if !errors.Is(err, ErrCommitIngest) || errors.Is(err, ErrCommitUnauthorized) {
		t.Errorf("the second walk over the refused commit answered %v, want an ingest failure at Process", err)
	}
	if got := carol.group.Stats().CommitRefused; got != 1 {
		t.Errorf("carol's Stats.CommitRefused is %d after two walks, want 1", got)
	}
	if carol.group.Epoch() != 1 {
		t.Errorf("carol moved to epoch %d", carol.group.Epoch())
	}
}

// A MEMBER'S POLICY NAMING ITSELF OWNER IS REFUSED: item 242's P2, R5.
func TestAMembersPolicyNamingItselfOwnerIsRefused(t *testing.T) {
	world := newRoleWorld(t, "owner", "mallory", "carol")
	owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")

	coup := world.policyOf(mallory)
	coup.SetRole(owner.dev.identityPub, mls.RoleAdmin)
	coup.SetRole(mallory.dev.identityPub, mls.RoleOwner)
	record := world.commitAndPublish(mallory, "CommitPolicy naming itself owner", func() ([]byte, []byte, []byte, error) {
		return mallory.handle.CommitPolicy(world.policyBody(coup))
	})
	for _, honest := range []*roleMember{owner, carol} {
		world.refuse(honest, record, ErrCommitOwnerTransfer)
	}
}

// A CONTEXT EXTENSIONS COMMIT THAT DROPS 0xF001 IS REFUSED (R0a, item 242's P3), AND ONE THAT
// DROPS 0x0003 IS REFUSED (R0b, item 242's P4) -- each by every honest receiver, and each a
// commit the seam builds and mls accepts.
func TestAContextExtensionsCommitDroppingThePolicyOrTheCapabilitiesIsRefused(t *testing.T) {
	without := func(extensions []messagegroup.ExtensionBytes, dropped uint16) []messagegroup.ExtensionBytes {
		out := []messagegroup.ExtensionBytes{}
		for _, extension := range extensions {
			if extension.Type != dropped {
				out = append(out, extension)
			}
		}
		return out
	}
	rows := []struct {
		name    string
		dropped uint16
		rule    error
	}{
		{"dropping urmessage_group_policy", uint16(mls.ExtensionTypeUrmessageGroupPolicy), ErrCommitPolicyInvalid},
		{"dropping required_capabilities", 0x0003, ErrCommitExtensionChanged},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			world := newRoleWorld(t, "owner", "mallory", "carol")
			owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")
			current, err := mallory.group.contextExtensionsLocked()
			if err != nil {
				t.Fatal(err)
			}
			// THE POSITIVE CONTROL: the list carries what is about to be dropped
			stripped := without(current, row.dropped)
			if len(stripped) != len(current)-1 {
				t.Fatalf("the group context carries %d extension(s) and none of type %#04x: %v", len(current), row.dropped, current)
			}
			record := world.commitAndPublish(mallory, "CommitContextExtensions", func() ([]byte, []byte, []byte, error) {
				return mallory.handle.CommitContextExtensions(stripped)
			})
			for _, honest := range []*roleMember{owner, carol} {
				err := world.refuse(honest, record, row.rule)
				if row.dropped == uint16(mls.ExtensionTypeUrmessageGroupPolicy) && !errors.Is(err, mls.ErrNoGroupPolicy) {
					t.Errorf("%s's refusal does not carry mls.ErrNoGroupPolicy: %v", honest.name, err)
				}
			}
		})
	}
}

// A POLICY NAMING AN IDENTITY WITH NO LEAF IS REFUSED (R0c), even from the owner.
func TestAPolicyNamingAnIdentityWithNoLeafIsRefused(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	phantom := world.policyOf(owner)
	phantom.SetRole(roleIdentity("nobody"), mls.RoleMember)
	record := world.commitAndPublish(owner, "CommitPolicy naming a phantom", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(phantom))
	})
	for _, honest := range []*roleMember{bob, carol} {
		world.refuse(honest, record, ErrCommitPolicyPhantom)
	}
}

// THE OWNER'S PROMOTION IS ALLOWED, AND AFTERWARDS ONLY THE ADMIN MAY ADD: R4 lets the owner seat
// an admin, R1 lets that admin add a new identity, and R1 refuses the same add from a member. The
// allowed commits move every receiver and their exporters agree with the committer's.
func TestAnOwnersPromotionIsAllowedAndOnlyTheAdminMayThenAdd(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	// epoch 1 -> 2: the owner makes bob an admin
	promotion := world.policyOf(owner)
	promotion.SetRole(bob.dev.identityPub, mls.RoleAdmin)
	record := world.commitAndPublish(owner, "CommitPolicy promoting bob", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(promotion))
	})
	for _, honest := range []*roleMember{bob, carol} {
		world.ingest(honest, owner, record)
	}
	if role, _ := world.policyOf(carol).RoleOf(bob.dev.identityPub); role != mls.RoleAdmin {
		t.Fatalf("carol reads bob as %s after the promotion", role)
	}

	// epoch 2 -> 3: bob, now an admin, adds dave
	dave := world.device("dave")
	daveKeyPackage, err := dave.engine.NewKeyPackage()
	if err != nil {
		t.Fatal(err)
	}
	record = world.commitAndPublish(bob, "CommitAdd of dave", func() ([]byte, []byte, []byte, error) {
		return bob.handle.CommitAdd([][]byte{daveKeyPackage})
	})
	for _, honest := range []*roleMember{owner, carol} {
		world.ingest(honest, bob, record)
	}

	// epoch 3 -> refused: carol, a member, adds erin
	erin := world.device("erin")
	erinKeyPackage, err := erin.engine.NewKeyPackage()
	if err != nil {
		t.Fatal(err)
	}
	record = world.commitAndPublish(carol, "CommitAdd of erin", func() ([]byte, []byte, []byte, error) {
		return carol.handle.CommitAdd([][]byte{erinKeyPackage})
	})
	for _, honest := range []*roleMember{owner, bob} {
		world.refuse(honest, record, ErrCommitAddByNonAdmin)
		if honest.group.Epoch() != 3 {
			t.Errorf("%s is at epoch %d, want 3", honest.name, honest.group.Epoch())
		}
	}
}

// claimingKeyPackage is a key package whose credential CLAIMS an identity it does not sign with:
// a fresh signer under another member's identity, which nothing in mls refuses (item 242's M7).
// It is also, with the claimed identity's own device committing it, what a second device of that
// identity looks like in this build.
func claimingKeyPackage(t *testing.T, root string, claimed []byte) []byte {
	t.Helper()
	store, err := OpenDurableStateStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatal(err)
	}
	signer, _, leafKeys, _, err := deviceIdentity(crypto, store, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, store, signer, mls.BasicCredential(claimed), leafKeys)
	if err != nil {
		t.Fatal(err)
	}
	keyPackage, err := engine.NewKeyPackage()
	if err != nil {
		t.Fatal(err)
	}
	return keyPackage
}

// AN ADD CLAIMING THE OWNER'S IDENTITY, COMMITTED BY AN ADMIN, IS REFUSED (R6a): the credential
// would otherwise inherit the owner's role at every member.
func TestAnAddClaimingTheOwnersIdentityByAnAdminIsRefused(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	promotion := world.policyOf(owner)
	promotion.SetRole(bob.dev.identityPub, mls.RoleAdmin)
	record := world.commitAndPublish(owner, "CommitPolicy promoting bob", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(promotion))
	})
	for _, honest := range []*roleMember{bob, carol} {
		world.ingest(honest, owner, record)
	}

	claim := claimingKeyPackage(t, filepath.Join(world.root, "claimant"), owner.dev.identityPub)
	record = world.commitAndPublish(bob, "CommitAdd of a leaf claiming the owner", func() ([]byte, []byte, []byte, error) {
		return bob.handle.CommitAdd([][]byte{claim})
	})
	for _, honest := range []*roleMember{owner, carol} {
		world.refuse(honest, record, ErrCommitIdentityClaimed)
	}
}

// THE OWNER ADDING ITS OWN SECOND DEVICE IS ALLOWED (R6a's other arm), and afterwards the owner's
// identity holds two leaves at every receiver.
func TestTheOwnerAddingItsOwnSecondDeviceIsAllowed(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	second := claimingKeyPackage(t, filepath.Join(world.root, "owner-laptop"), owner.dev.identityPub)
	record := world.commitAndPublish(owner, "CommitAdd of its own second device", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitAdd([][]byte{second})
	})
	for _, honest := range []*roleMember{bob, carol} {
		world.ingest(honest, owner, record)
		ownerLeaves := 0
		for at := 0; at < honest.handle.MemberCount(); at += 1 {
			_, identity, _, err := honest.handle.MemberAt(at)
			if err != nil {
				t.Fatal(err)
			}
			if string(identity) == string(owner.dev.identityPub) {
				ownerLeaves += 1
			}
		}
		if ownerLeaves != 2 {
			t.Errorf("%s holds %d leaf(s) for the owner's identity after the add, want 2", honest.name, ownerLeaves)
		}
	}
}

// A MEMBER OR AN OBSERVER MAY COMMIT ITS OWN DEVICE LEAVES, OR NOTHING, AND NO MORE (R7): §11's
// table gives "commit epochs" to ADMIN and OWNER, and ruling 5 gives the OBSERVER "its own device
// add / remove and nothing else" -- and ruling 12 makes the PATH-ONLY commit every role's, since
// RFC 9420 §12.4 forbids a committer carrying its own Update and a bare commit is a member's only
// way to refresh its own leaf keys. So two commits the seam builds are INGESTED by every honest
// receiver, with every exporter agreeing at the new epoch: a member's own second device, and a
// member's or an observer's Commit(nil), which carries no proposal and the group's own extension
// list. And the Update by reference is still refused: the owner's own key rotation, committed by
// an observer, is a change to a leaf that is not the committer's own.
func TestAMemberOrAnObserverMayCommitOnlyItsOwnDeviceLeaves(t *testing.T) {
	t.Run("a member's own second device and its bare commit are both ingested", func(t *testing.T) {
		world := newRoleWorld(t, "owner", "bob", "carol")
		owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

		// epoch 1 -> 2: the positive control, through the real seam
		laptop := claimingKeyPackage(t, filepath.Join(world.root, "bob-laptop"), bob.dev.identityPub)
		record := world.commitAndPublish(bob, "CommitAdd of its own second device", func() ([]byte, []byte, []byte, error) {
			return bob.handle.CommitAdd([][]byte{laptop})
		})
		for _, honest := range []*roleMember{owner, carol} {
			world.ingest(honest, bob, record)
		}

		// epoch 2 -> 3: the same member's path-only commit, ruling 12. THE POSITIVE CONTROL that
		// it is bare: the staged value at a receiver declares no add, remove or update.
		record = world.commitAndPublish(bob, "Commit(nil)", func() ([]byte, []byte, []byte, error) {
			return bob.handle.Commit(nil)
		})
		world.ingestBare(carol, bob, record)
		world.ingest(owner, bob, record)
		for _, honest := range []*roleMember{owner, carol} {
			if honest.group.Epoch() != 3 {
				t.Errorf("%s is at epoch %d after the member's bare commit, want 3", honest.name, honest.group.Epoch())
			}
		}
	})

	t.Run("an observer's bare commit is ingested (ruling 12)", func(t *testing.T) {
		world := newRoleWorld(t, "owner", "olive", "carol")
		owner, olive, carol := world.member("owner"), world.member("olive"), world.member("carol")
		world.demoteToObserver(owner, olive, carol)

		record := world.commitAndPublish(olive, "Commit(nil)", func() ([]byte, []byte, []byte, error) {
			return olive.handle.Commit(nil)
		})
		world.ingestBare(carol, olive, record)
		world.ingest(owner, olive, record)
		if role, _ := world.policyOf(carol).RoleOf(olive.dev.identityPub); role != mls.RoleObserver {
			t.Errorf("carol reads olive as %s after olive's bare commit, want observer: the commit changed a policy", role)
		}
	})

	t.Run("an observer committing the owner's update by reference is refused", func(t *testing.T) {
		world := newRoleWorld(t, "owner", "olive", "carol")
		owner, olive, carol := world.member("owner"), world.member("olive"), world.member("carol")
		world.demoteToObserver(owner, olive, carol)

		// the owner's own key rotation, cached at every member as a proposal record would leave it
		proposal, err := owner.handle.ProposeUpdate()
		if err != nil {
			t.Fatalf("the owner's ProposeUpdate: %v", err)
		}
		for _, receiver := range []*roleMember{olive, carol} {
			processed, err := receiver.handle.Process(proposal)
			if err != nil {
				t.Fatalf("%s processing the owner's update proposal: %v", receiver.name, err)
			}
			if processed.Kind != messagegroup.EngineProcessedProposal {
				t.Fatalf("%s processed the owner's update as kind %d, want a proposal", receiver.name, processed.Kind)
			}
		}
		// Commit(nil) folds in every cached proposal: the commit carries the owner's Update by
		// reference and nothing else
		record := world.commitAndPublish(olive, "Commit(nil) over the owner's cached update", func() ([]byte, []byte, []byte, error) {
			return olive.handle.Commit(nil)
		})
		for _, honest := range []*roleMember{owner, carol} {
			world.refuse(honest, record, ErrCommitBeyondOwnDevices)
		}
	})
}

// ingestBare is [roleWorld.ingest] with the positive control that the commit IS the path-only
// one ruling 12 names: the receiver's configured authorizer sees a decision declaring no added,
// removed or updated leaf, the same membership on both sides, and an extension list byte
// identical before and after. Without it, a "bare" commit that quietly carried a cached
// proposal would pass as the self-heal.
func (self *roleWorld) ingestBare(receiver *roleMember, committer *roleMember, record *sealed) {
	self.t.Helper()
	var seen *CommitAuthorization
	receiver.group.device.commitAuthorizer = func(decision *CommitAuthorization) error {
		seen = decision
		return nil
	}
	defer func() { receiver.group.device.commitAuthorizer = nil }()
	self.ingest(receiver, committer, record)
	if seen == nil {
		self.t.Fatalf("%s's authorizer was never asked about %s's bare commit", receiver.name, committer.name)
	}
	if len(seen.AddedLeaves)+len(seen.RemovedLeaves)+len(seen.UpdatedLeaves) != 0 {
		self.t.Fatalf("%s's bare commit declares added %v removed %v updated %v: it is not path-only",
			committer.name, seen.AddedLeaves, seen.RemovedLeaves, seen.UpdatedLeaves)
	}
	if len(seen.Members) != len(seen.MembersAfter) {
		self.t.Fatalf("%s's bare commit moves the membership from %d to %d leaves", committer.name, len(seen.Members), len(seen.MembersAfter))
	}
	if !extensionsEqual(seen.ExtensionsBefore, seen.ExtensionsAfter) {
		self.t.Fatalf("%s's bare commit changes the group context extension list", committer.name)
	}
	if string(seen.CommitterIdentity) != string(committer.dev.identityPub) {
		self.t.Fatalf("the bare commit's committer is %x, want %s's %x", seen.CommitterIdentity, committer.name, committer.dev.identityPub)
	}
}

// demoteToObserver has the owner commit a policy making one member an OBSERVER, and every member
// ingest it -- R4's "set MEMBER/OBSERVER" arm, which the owner holds.
func (self *roleWorld) demoteToObserver(owner *roleMember, who *roleMember, others ...*roleMember) {
	self.t.Helper()
	demotion := self.policyOf(owner)
	demotion.SetRole(who.dev.identityPub, mls.RoleObserver)
	record := self.commitAndPublish(owner, "CommitPolicy demoting "+who.name, func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(self.policyBody(demotion))
	})
	for _, receiver := range append([]*roleMember{who}, others...) {
		self.ingest(receiver, owner, record)
	}
	if role, _ := self.policyOf(who).RoleOf(who.dev.identityPub); role != mls.RoleObserver {
		self.t.Fatalf("%s reads itself as %s after the demotion, want observer", who.name, role)
	}
}

// A CONFIGURED AUTHORIZER THAT ALLOWS CANNOT ALLOW WHAT THE RULES REFUSE, and one that refuses
// refuses what the rules allow: the two compose, the rules first.
func TestAConfiguredAuthorizerComposesWithTheRulesAndCannotLoosenThem(t *testing.T) {
	world := newRoleWorld(t, "owner", "mallory", "carol")
	owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")

	asked := 0
	carol.group.device.commitAuthorizer = func(*CommitAuthorization) error {
		asked += 1
		return nil
	}
	record := world.commitAndPublish(mallory, "CommitRemove of the owner", func() ([]byte, []byte, []byte, error) {
		return mallory.handle.CommitRemove([]uint32{owner.leaf})
	})
	world.refuse(carol, record, mls.ErrAdminRemovedByNonOwner)
	if asked != 0 {
		t.Errorf("the configured authorizer was asked %d time(s) about a commit the rules refused", asked)
	}

	// the other direction, on a fresh world: an allowed commit reaches the hook, and the hook's
	// refusal is carried
	world = newRoleWorld(t, "owner", "bob", "carol")
	bob := world.member("bob")
	owner, carol = world.member("owner"), world.member("carol")
	productRule := errors.New("this product refuses every promotion")
	var seen *CommitAuthorization
	carol.group.device.commitAuthorizer = func(decision *CommitAuthorization) error {
		seen = decision
		return productRule
	}
	promotion := world.policyOf(owner)
	promotion.SetRole(bob.dev.identityPub, mls.RoleAdmin)
	record = world.commitAndPublish(owner, "CommitPolicy promoting bob", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(promotion))
	})
	world.ingest(bob, owner, record)
	world.refuse(carol, record, productRule)
	if seen == nil {
		t.Fatal("the configured authorizer was never asked about an allowed commit")
	}
	// and it was handed the filled value: committer, roles on both sides, both policies
	if string(seen.CommitterIdentity) != string(owner.dev.identityPub) || seen.CommitterRole != mls.RoleOwner.String() {
		t.Errorf("the hook saw committer %x as %q, want the owner", seen.CommitterIdentity, seen.CommitterRole)
	}
	if seen.PolicyBefore == nil || seen.PolicyAfter == nil {
		t.Fatalf("the hook saw policies before=%v after=%v", seen.PolicyBefore, seen.PolicyAfter)
	}
	if role, _ := seen.PolicyAfter.RoleOf(bob.dev.identityPub); role != mls.RoleAdmin {
		t.Errorf("the hook's PolicyAfter reads bob as %s", role)
	}
	if len(seen.Members) != 3 || len(seen.MembersAfter) != 3 {
		t.Errorf("the hook saw %d members before and %d after, want 3 and 3", len(seen.Members), len(seen.MembersAfter))
	}
	for _, member := range seen.MembersAfter {
		if string(member.IdentityPub) == string(bob.dev.identityPub) && member.Role != mls.RoleAdmin.String() {
			t.Errorf("MembersAfter reads bob as %q", member.Role)
		}
	}
}

// THE OWNER ADDING A STRANGER AND CROWNING IT IN ONE COMMIT IS REFUSED (R5, ruling 10): "a
// current member" is the PRE-commit membership, so the new owner must have held a leaf before the
// commit. No by-value arm of the seam carries an Add and a policy together, so the commit is built
// by reference through the seam's own doors -- ProposeAdd, ProposeGroupPolicy, each processed at
// every receiver as a proposal record would leave it, then Commit(nil) folding both in -- which is
// exactly the fold ruling 13 keeps out of production and the receiving arm must still judge. The
// refusal is R5's and not R0c's: dave holds a leaf after the commit, so the policy names no
// phantom. THE CONTROL is the same transfer in two commits, both allowed: the owner adds dave,
// dave joins, and the owner crowns dave in the next commit; every receiver reads dave as owner
// and the old owner as admin afterwards.
func TestTheOwnerAddingAStrangerAndCrowningItInOneCommitIsRefused(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	dave := world.device("dave")
	daveKeyPackage, err := dave.engine.NewKeyPackage()
	if err != nil {
		t.Fatal(err)
	}
	crown := world.policyOf(owner)
	crown.SetRole(dave.identityPub, mls.RoleOwner)
	crown.SetRole(owner.dev.identityPub, mls.RoleAdmin)

	// the one commit: the add and the transfer, by reference, cached at every member
	addProposal, err := owner.handle.ProposeAdd(daveKeyPackage)
	if err != nil {
		t.Fatalf("the owner's ProposeAdd of dave: %v", err)
	}
	crownProposal, err := owner.handle.ProposeGroupPolicy(world.policyBody(crown))
	if err != nil {
		t.Fatalf("the owner's ProposeGroupPolicy crowning dave: %v", err)
	}
	for _, receiver := range []*roleMember{bob, carol} {
		for _, proposal := range [][]byte{addProposal, crownProposal} {
			processed, err := receiver.handle.Process(proposal)
			if err != nil {
				t.Fatalf("%s processing the owner's proposal: %v", receiver.name, err)
			}
			if processed.Kind != messagegroup.EngineProcessedProposal {
				t.Fatalf("%s processed the owner's proposal as kind %d, want a proposal", receiver.name, processed.Kind)
			}
		}
	}
	record := world.commitAndPublish(owner, "Commit(nil) over the cached add and crowning", func() ([]byte, []byte, []byte, error) {
		return owner.handle.Commit(nil)
	})
	for _, honest := range []*roleMember{bob, carol} {
		err := world.refuse(honest, record, ErrCommitOwnerTransfer)
		if errors.Is(err, ErrCommitPolicyPhantom) {
			t.Errorf("%s's refusal is R0c's phantom and not R5's: %v", honest.name, err)
		}
	}
	if owner.group.Epoch() != 2 {
		t.Fatalf("the owner is at epoch %d after its own merge, want 2", owner.group.Epoch())
	}

	// THE CONTROL, on a fresh world: the add and the transfer as two commits
	world = newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol = world.member("owner"), world.member("bob"), world.member("carol")
	dave = world.device("dave")
	daveKeyPackage, err = dave.engine.NewKeyPackage()
	if err != nil {
		t.Fatal(err)
	}
	commit, welcome, ratchetTree, err := owner.handle.CommitAdd([][]byte{daveKeyPackage})
	if err != nil {
		t.Fatalf("the owner's CommitAdd of dave: %v", err)
	}
	if err := owner.handle.MergePendingCommit(); err != nil {
		t.Fatalf("the owner's MergePendingCommit: %v", err)
	}
	record = world.publish(owner, commit)
	for _, honest := range []*roleMember{bob, carol} {
		world.ingest(honest, owner, record)
	}
	daveHandle, err := dave.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("dave's JoinFromWelcome: %v", err)
	}
	t.Cleanup(func() { daveHandle.Close() })
	daveMember := world.enroll("dave", dave, daveHandle)

	crown = world.policyOf(owner)
	crown.SetRole(dave.identityPub, mls.RoleOwner)
	crown.SetRole(owner.dev.identityPub, mls.RoleAdmin)
	record = world.commitAndPublish(owner, "CommitPolicy crowning dave", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(crown))
	})
	for _, honest := range []*roleMember{bob, carol, daveMember} {
		world.ingest(honest, owner, record)
		policy := world.policyOf(honest)
		if role, _ := policy.RoleOf(dave.identityPub); role != mls.RoleOwner {
			t.Errorf("%s reads dave as %s after the transfer, want owner", honest.name, role)
		}
		if role, _ := policy.RoleOf(owner.dev.identityPub); role != mls.RoleAdmin {
			t.Errorf("%s reads the old owner as %s after the transfer, want admin", honest.name, role)
		}
	}
}

// keylessKeyPackage is a key package that LISTS urmessage_leaf_keys among its capabilities, as
// ValSem106 requires, and does not CARRY the extension: what a hostile mls build hands a group,
// and what both of the seam's send doors refuse. It is connect/messagegroup's own
// commitAddKeyPackageWithoutLeafKeys spelled from this module: mls.NewKeyPackageWithSigner over the
// device's signer and credential, with the engine's capability list and no extensions.
func keylessKeyPackage(t *testing.T, dev *crossProcessDevice) []byte {
	t.Helper()
	capabilities := mls.Capabilities{
		Versions:     []mls.ProtocolVersion{mls.ProtocolVersionMls10},
		CipherSuites: mls.Suites(),
		Extensions: []mls.ExtensionType{
			mls.ExtensionTypeUrmessageGroupPolicy,
			mls.ExtensionTypeUrmessageLeafKeys,
		},
		Proposals:   []mls.ProposalType{},
		Credentials: []mls.CredentialType{mls.CredentialTypeBasic},
	}
	keyPackage, initPrivate, encryptPrivate, err := mls.NewKeyPackageWithSigner(dev.crypto, dev.crypto.Suite(),
		dev.signer, mls.BasicCredential(dev.identityPub), capabilities, nil)
	if err != nil {
		t.Fatalf("a key package with no leaf keys: %v", err)
	}
	defer keyPackage.Zeroize()
	defer clear(initPrivate)
	defer clear(encryptPrivate)
	encoded, err := syntax.Marshal(keyPackage)
	if err != nil {
		t.Fatalf("encoding the keyless key package: %v", err)
	}
	return encoded
}

// liveGroupOf is the *mls.Group behind a handle of connect/messagegroup's adapter, reached through
// the field that package keeps unexported, so that a case can build the one commit R6d exists for:
// an Add of a key package with no urmessage_leaf_keys, which both of the seam's send doors refuse
// and a hostile mls build would not. It is connect's own liveTreeOf technique one module out --
// unsafe IN A TEST AND NOWHERE ELSE, the field found by name and checked by type, so a rename or
// a retype in connect fails here with a sentence rather than reading past the end of the struct.
func liveGroupOf(t *testing.T, handle messagegroup.GroupHandle) *mls.Group {
	t.Helper()
	value := reflect.ValueOf(handle)
	if value.Kind() != reflect.Pointer || value.Type().String() != "*messagegroup.connectMlsHandle" {
		t.Fatalf("the handle is a %s, not connect/messagegroup's adapter over a live *mls.Group", value.Type())
	}
	field, found := value.Type().Elem().FieldByName("group")
	if !found {
		t.Fatal("connect/messagegroup's adapter declares no field named group; the keyless fixture reaches the live *mls.Group through it, and this is the line to move")
	}
	if field.Type != reflect.TypeOf((*mls.Group)(nil)) {
		t.Fatalf("the adapter's group field is a %s, want *mls.Group; the keyless fixture is written over the wrong storage", field.Type)
	}
	group := *(**mls.Group)(unsafe.Add(value.UnsafePointer(), field.Offset))
	if group == nil {
		t.Fatal("the adapter holds no live group")
	}
	return group
}

// A COMMIT ADMITTING A LEAF WITHOUT LEAF KEYS IS REFUSED BY EVERY HONEST RECEIVER (R6d): the
// hardening R1 carried into this pass. Both send doors refuse the key package -- the control that
// it is keyless for the reason this case names, and the reason the commit is built past them, on
// the owner's live *mls.Group as a hostile build would build it. mls accepts it at Process (connect
// pins that acceptance in TestTheStagedTreeAnswersWhetherEachLeafCarriesLeafKeysAndMlsAdmitsALeafWithout)
// and the seam reports the leaf with HasLeafKeys == false, which is what the rule reads. The
// committer is the OWNER, so no authority rule names this commit: the refusal is R6d's alone.
func TestACommitAdmittingALeafWithoutLeafKeysIsRefusedByEveryHonestReceiver(t *testing.T) {
	world := newRoleWorld(t, "owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	stranger := world.device("keyless")
	encoded := keylessKeyPackage(t, stranger)
	if _, _, _, err := owner.handle.CommitAdd([][]byte{encoded}); !errors.Is(err, messagegroup.ErrEngineCommitAddKeyPackage) {
		t.Fatalf("CommitAdd over the keyless package answered %v, want ErrEngineCommitAddKeyPackage: the send door has stopped asking", err)
	}
	if _, err := owner.handle.ProposeAdd(encoded); !errors.Is(err, mls.ErrMalformedExtension) {
		t.Fatalf("ProposeAdd over the keyless package answered %v, want mls.ErrMalformedExtension: the send door has stopped asking", err)
	}
	var keyless mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &keyless); err != nil {
		t.Fatalf("decoding the keyless package: %v", err)
	}
	result, err := liveGroupOf(t, owner.handle).CreateCommit([][]byte{}, []mls.Proposal{{
		ProposalType: mls.ProposalTypeAdd,
		Add:          &mls.Add{KeyPackage: keyless},
	}}, nil)
	if err != nil {
		t.Fatalf("mls's CreateCommit over a by-value Add of the keyless package: %v; mls has grown a send-side leaf keys rule, and this case is the line to move", err)
	}
	if err := owner.handle.MergePendingCommit(); err != nil {
		t.Fatalf("the owner's MergePendingCommit: %v", err)
	}
	record := world.publish(owner, result.Commit)

	for _, honest := range []*roleMember{bob, carol} {
		// the seam's own reading, which is what the rule was handed: one keyless leaf, the
		// stranger's, and every other leaf keyed
		var seen *CommitAuthorization
		honest.group.device.commitAuthorizer = func(decision *CommitAuthorization) error {
			seen = decision
			return nil
		}
		world.refuse(honest, record, ErrCommitLeafWithoutKeys)
		if seen != nil {
			t.Fatalf("%s's configured authorizer was asked about a commit the rules refused", honest.name)
		}
		if honest.group.Epoch() != 1 {
			t.Errorf("%s is at epoch %d, want 1", honest.name, honest.group.Epoch())
		}
	}
}

// ── the staged epoch is erased through the seam, held at runtime ────────────────────────────

// handleCall is one call the ingest path made on the seam that touches a staged value.
type handleCall struct {
	method    string
	processed *messagegroup.EngineProcessed
	answered  error
}

// countingHandle is the seam's own GroupHandle with the three doors a staged epoch passes through
// counted: Process, where it is born; ApplyCommit, where it is installed; and DiscardProcessed,
// where it is erased. Every other method is the real handle's. It can fail one ApplyCommit
// without asking the real handle, and it can answer one DiscardProcessed with an injected error
// AFTER the real erase ran, so the group underneath stays consistent with what the test observes.
type countingHandle struct {
	messagegroup.GroupHandle
	calls       []handleCall
	applyOnce   error
	discardOnce error
}

func (self *countingHandle) Process(message []byte) (*messagegroup.EngineProcessed, error) {
	processed, err := self.GroupHandle.Process(message)
	self.calls = append(self.calls, handleCall{"Process", processed, err})
	return processed, err
}

func (self *countingHandle) ApplyCommit(processed *messagegroup.EngineProcessed) error {
	if self.applyOnce != nil {
		err := self.applyOnce
		self.applyOnce = nil
		self.calls = append(self.calls, handleCall{"ApplyCommit", processed, err})
		return err
	}
	err := self.GroupHandle.ApplyCommit(processed)
	self.calls = append(self.calls, handleCall{"ApplyCommit", processed, err})
	return err
}

func (self *countingHandle) DiscardProcessed(processed *messagegroup.EngineProcessed) error {
	err := self.GroupHandle.DiscardProcessed(processed)
	self.calls = append(self.calls, handleCall{"DiscardProcessed", processed, err})
	if self.discardOnce != nil {
		injected := self.discardOnce
		self.discardOnce = nil
		return injected
	}
	return err
}

// take answers the calls since the last take.
func (self *countingHandle) take() []handleCall {
	calls := self.calls
	self.calls = nil
	return calls
}

// expectCalls holds that one ingest made exactly these calls, in this order, every one over the
// value Process answered.
func expectCalls(t *testing.T, what string, calls []handleCall, methods ...string) {
	t.Helper()
	names := []string{}
	for _, call := range calls {
		names = append(names, call.method)
	}
	if len(calls) != len(methods) {
		t.Fatalf("%s made the seam calls %v, want %v", what, names, methods)
	}
	for at, method := range methods {
		if calls[at].method != method {
			t.Fatalf("%s made the seam calls %v, want %v", what, names, methods)
		}
	}
	for _, call := range calls[1:] {
		if call.processed != calls[0].processed {
			t.Fatalf("%s's %s was over a value other than the one its Process answered", what, call.method)
		}
	}
}

// THE INGEST PATH ERASES EVERY STAGED EPOCH IT DOES NOT INSTALL, THROUGH THE SEAM'S OWN DOOR, AND
// SURFACES A FAILED ERASE -- held at RUNTIME, over real devices and the real ingest path, with the
// seam's three doors counted. A processed commit is a fully derived second epoch that nothing
// else would erase; the seam's door for it is DiscardProcessed, and the property is what the path
// DOES with it on each of its exits, not where a defer sits in the source:
//
//   - a REFUSED commit sees exactly one DiscardProcessed after its Process, and no ApplyCommit;
//   - an ALLOWED commit sees ApplyCommit and then a DiscardProcessed that finds nothing and answers
//     nil (the seam releases the staged half on the install);
//   - a DiscardProcessed error is SURFACED through Receive, not swallowed;
//   - an ApplyCommit that FAILS sees the same erase, and the erase is real: the value it erased is
//     no longer installable through the real handle.
//
// This replaced a source pin over ingestCommitLocked's statement positions. That pin passed a
// mutant that put a return in the ELSE branch of the Process error check -- an exit after a
// successful Process and before the defer, which the pin's position reading never inspected --
// and every refused commit's staged epoch then stayed in the heap with the pin green. Here that
// mutant is the first case going red: the refused commit's calls are [Process] and no erase.
func TestEveryStagedEpochTheIngestPathDoesNotInstallIsErasedThroughTheSeam(t *testing.T) {
	world := newRoleWorld(t, "owner", "mallory", "carol")
	owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")
	counting := &countingHandle{GroupHandle: carol.handle}
	carol.group.handle = counting

	// (1) a refused commit: Process, then one erase of that value, and no install
	record := world.commitAndPublish(mallory, "CommitRemove of the owner", func() ([]byte, []byte, []byte, error) {
		return mallory.handle.CommitRemove([]uint32{owner.leaf})
	})
	world.refuse(carol, record, mls.ErrAdminRemovedByNonOwner)
	calls := counting.take()
	expectCalls(t, "the refused commit", calls, "Process", "DiscardProcessed")
	if calls[1].answered != nil {
		t.Errorf("erasing the refused commit's staged epoch answered %v", calls[1].answered)
	}

	// (2) an allowed commit: Process, the install, then the erase that finds nothing
	promotion := world.policyOf(owner)
	promotion.SetRole(carol.dev.identityPub, mls.RoleAdmin)
	record = world.commitAndPublish(owner, "CommitPolicy promoting carol", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(promotion))
	})
	world.ingest(carol, owner, record)
	calls = counting.take()
	expectCalls(t, "the allowed commit", calls, "Process", "ApplyCommit", "DiscardProcessed")
	if calls[1].answered != nil || calls[2].answered != nil {
		t.Errorf("the allowed commit's install answered %v and its erase %v, want nil and nil", calls[1].answered, calls[2].answered)
	}

	// (3) an erase that fails is reported by the walk, beside the install that preceded it
	failedErase := errors.New("the seam refused to erase")
	counting.discardOnce = failedErase
	retention := world.policyOf(owner)
	retention.RetentionPolicy.DurableMs += 1
	record = world.commitAndPublish(owner, "CommitPolicy changing retention", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(retention))
	})
	epoch := carol.group.Epoch()
	err := world.deliver(carol, record)
	if !errors.Is(err, failedErase) || !errors.Is(err, ErrCommitIngest) {
		t.Fatalf("the walk over a commit whose erase failed answered %v, want ErrCommitIngest wrapping the erase's own error", err)
	}
	expectCalls(t, "the commit whose erase failed", counting.take(), "Process", "ApplyCommit", "DiscardProcessed")
	if carol.group.Epoch() != epoch+1 || carol.handle.Epoch() != epoch+1 {
		t.Errorf("carol is at epoch %d (handle %d) after the install that preceded the failed erase, want %d", carol.group.Epoch(), carol.handle.Epoch(), epoch+1)
	}

	// (4) an install that fails: the same erase, and a real one
	failedInstall := errors.New("the seam refused to install")
	counting.applyOnce = failedInstall
	retention.RetentionPolicy.DurableMs += 1
	record = world.commitAndPublish(owner, "CommitPolicy changing retention again", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(retention))
	})
	epoch = carol.group.Epoch()
	before := carol.group.Stats()
	err = world.deliver(carol, record)
	if !errors.Is(err, failedInstall) || !errors.Is(err, ErrCommitIngest) || errors.Is(err, ErrCommitUnauthorized) {
		t.Fatalf("the walk over a commit whose install failed answered %v, want ErrCommitIngest wrapping the install's own error and no refusal", err)
	}
	calls = counting.take()
	expectCalls(t, "the commit whose install failed", calls, "Process", "ApplyCommit", "DiscardProcessed")
	if calls[2].answered != nil {
		t.Errorf("erasing the uninstalled commit's staged epoch answered %v", calls[2].answered)
	}
	if carol.group.Epoch() != epoch || carol.handle.Epoch() != epoch {
		t.Errorf("carol moved to epoch %d (handle %d) over a commit that did not install, want %d", carol.group.Epoch(), carol.handle.Epoch(), epoch)
	}
	if after := carol.group.Stats(); after.CommitRefused != before.CommitRefused || after.Ingested != before.Ingested {
		t.Errorf("an install failure was counted as a refusal or an ingest: refused %d -> %d, ingested %d -> %d",
			before.CommitRefused, after.CommitRefused, before.Ingested, after.Ingested)
	}
	// THE ERASE WAS REAL: the real handle refuses to install the value the path discarded, and
	// the group stays where it was
	if err := carol.handle.ApplyCommit(calls[0].processed); err == nil {
		t.Fatal("the staged epoch the ingest path discarded was still installable through the real handle: the erase did not happen")
	}
	if carol.handle.Epoch() != epoch {
		t.Errorf("the real handle moved to epoch %d over an erased value", carol.handle.Epoch())
	}
}

// A ROLE-MODEL REFUSAL IS NEITHER RULING 52's REMOVAL NOR RULING 41's HALT, AND THE WALKS SAY WHICH.
//
// WHAT LEDGER ITEM 259's ADVERSARY PASS FILED, and it reproduces here exactly as filed: a commit
// [authorizeCommit] refuses leaves NO STICKY STATE, so the same four-walk-then-silence sequence
// ruling 52 was written to end is still what a refusing receiver gets. Measured, walk by walk, at
// the commit this case landed in:
//
//	1st Receive: ErrCommitUnauthorized, wrapping the rule that refused it
//	2nd Receive: ErrCommitIngest: processing the commit: mls: ratchet generation already consumed
//	3rd Receive: ErrRecordAbandoned, after maxRecordAttempts attempts, cursor resolved PAST it
//	4th Receive: nil. 5th: nil. Every later one: nil.
//
// THE HALF THAT IS CORRECT AND IS THE REASON THIS IS NOT A BUG REPORT. It is not [Group.removed],
// and that is ruling 41's sentence read literally: a commit this device REFUSED removed it from
// nothing. Its leaf is still in the tree it is standing in, it is still a member at epoch n, and a
// state saying "you are no longer in this group" would be reporting a membership change this device
// did not accept and its own MLS state does not carry. [Group.ingestCommitLocked]'s step (3) takes
// the refusal BEFORE ApplyCommit, so the removed arm is not even reachable from it.
//
// AND THE HALF THAT IS A RESIDUAL, NAMED RATHER THAN DRESSED UP. It is not [Group.halted] either.
// [Group.halted] is [ErrRemovalWithoutRotation] and nothing else -- ruling 41's ONE invalid shape --
// and the 2026-09-24 repair that made it sticky and persisted was made for exactly the sequence
// above, one refusal site along. The role model's refusal site did not get it. What that costs in
// the field is item 242's own accounting and is not new: the server has already accepted the commit
// and moved current_epoch, so a refusing receiver stays at n, can no longer write, and recovery is a
// new group. What is new here is only that the SENTENCE is lost after walk one -- step (0) has
// consumed the committer's ratchet generation by then, so the sentinel cannot be re-derived -- and
// the client is left with a group that reads healthy. Extending the halt to this site is ruling 41's
// track and not X4's: it needs a second persisted kind, a projection, and a ruling on whether an
// unauthorized commit and an unrotated removal are one state or two.
//
// SO THIS CASE PINS THE STATE OF AFFAIRS AND NOT A DESIDERATUM, and says so out loud because a case
// that pins a residual can be misread as blessing it. The day the halt reaches this site, THIS is
// the case that goes red, and its header is what has to be read: the two clauses to rewrite are the
// `halted == nil` reading and the silence at walk four.
//
// THE CONTROL IS INLINE AND FIRES FOR ITS OWN REASON: the same receiver, in the same case, FOLLOWS
// an honest policy commit from the owner before the coup arrives. Without it "the walk went silent"
// is satisfied by a walk that never followed anything.
//
// WHAT WOULD GO RED: set [Group.removed] on an authorization refusal (a refused commit would then
// claim to have removed the refuser); route step (3)'s refusal through [Group.haltLocked] (the
// residual is closed and this header is stale); take the refusal AFTER ApplyCommit (walk one's
// epoch check).
func TestARoleModelRefusalIsNeitherTheRemovalNorTheHaltAndTheWalksSayWhich(t *testing.T) {
	world := newRoleWorld(t, "owner", "mallory", "carol")
	owner, mallory, carol := world.member("owner"), world.member("mallory"), world.member("carol")

	standingAt := carol.group.Epoch()

	// ── THE COUP, AND THE FOUR WALKS OVER IT ────────────────────────────────────────────────────
	coup := world.policyOf(mallory)
	coup.SetRole(owner.dev.identityPub, mls.RoleAdmin)
	coup.SetRole(mallory.dev.identityPub, mls.RoleOwner)
	record := world.commitAndPublish(mallory, "CommitPolicy naming itself owner", func() ([]byte, []byte, []byte, error) {
		return mallory.handle.CommitPolicy(world.policyBody(coup))
	})

	first := world.refuse(carol, record, ErrCommitOwnerTransfer)
	refusedOnce := carol.group.Stats().CommitRefused
	if !errors.Is(first, ErrCommitUnauthorized) {
		t.Fatalf("walk 1 answered %v, want ErrCommitUnauthorized", first)
	}

	// walks two and three: the sentence is gone, then the record is abandoned
	second := world.deliver(carol, record)
	if second == nil {
		t.Fatalf("walk 2 over the refused commit answered nil")
	}
	if errors.Is(second, ErrCommitUnauthorized) {
		t.Errorf("walk 2 still answers ErrCommitUnauthorized (%v). That would be the residual above "+
			"CLOSED, which is a change this case's header has to be rewritten for rather than a "+
			"result to pass over", second)
	}
	if got := carol.group.Stats().CommitRefused; got != refusedOnce {
		t.Errorf("Stats.CommitRefused went %d -> %d on walk 2: the refusal is being re-taken over a "+
			"commit mls can no longer be asked about", refusedOnce, got)
	}
	third := world.deliver(carol, record)
	if !errors.Is(third, ErrRecordAbandoned) {
		t.Fatalf("walk 3 answered %v, want ErrRecordAbandoned after maxRecordAttempts attempts", third)
	}
	stats := carol.group.Stats()
	if stats.Unopened != 1 || stats.FailedOpen != uint64(maxRecordAttempts) {
		t.Errorf("after walk 3 Unopened is %d and FailedOpen %d, want 1 and %d",
			stats.Unopened, stats.FailedOpen, maxRecordAttempts)
	}
	if carol.group.cursor < record.recordId {
		t.Errorf("the cursor stands at %d, still below the refused commit %d",
			carol.group.cursor, record.recordId)
	}
	for walk := 4; walk <= 5; walk += 1 {
		if err := world.deliver(carol, record); err != nil {
			t.Errorf("walk %d answered %v, want nil. A sentence here would be the residual above "+
				"CLOSED and this header stale", walk, err)
		}
	}

	// ── AND NEITHER STICKY STATE, WHICH IS THE WHOLE POINT OF THE CASE ───────────────────────────
	if epoch, state := carol.group.Removal(); state != nil || epoch != 0 {
		t.Errorf("the refusing receiver reads (%d, %v) from Removal. A commit this device REFUSED "+
			"removed it from nothing: its leaf is in the tree it is standing in and it is still a "+
			"member at epoch %d", epoch, state, standingAt)
	}
	if carol.group.halted != nil {
		t.Errorf("the refusing receiver is halted (%v). [Group.halted] is ErrRemovalWithoutRotation "+
			"and nothing else; if the halt now reaches the authorization site, this case's header is "+
			"what has to be rewritten", carol.group.halted)
	}
	if got := carol.group.Epoch(); got != standingAt {
		t.Errorf("the refusing receiver stands at epoch %d, want %d", got, standingAt)
	}
	if got := carol.handle.Epoch(); got != standingAt {
		t.Errorf("the refusing receiver's MLS handle stands at epoch %d, want %d: the commit was "+
			"applied and the refusal taken after it", got, standingAt)
	}

	// ── THE CONTROL, AFTERWARDS, WHICH IS WHERE IT BELONGS ──────────────────────────────────────
	//
	// The same receiver FOLLOWS an honest commit from the owner, built against the epoch the owner
	// is still standing at -- because the owner refuses the coup too. So the silence above is the
	// REFUSED RECORD's and not the walk's. What this does NOT show, and what item 242 already
	// accounts for, is the field: there the server accepted the coup and moved current_epoch, so
	// every honest receiver is stale and the owner's next commit is refused as EPOCH_STALE. This
	// harness has no server, and the control is scoped to the record.
	promotion := world.policyOf(owner)
	promotion.SetRole(carol.dev.identityPub, mls.RoleMember)
	honest := world.commitAndPublish(owner, "CommitPolicy naming carol", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(promotion))
	})
	world.ingest(carol, owner, honest)
	if got := carol.group.Epoch(); got != standingAt+1 {
		t.Fatalf("CONTROL FAILED: carol stands at epoch %d after an honest commit, want %d; the "+
			"silence above would then be a walk that follows nothing", got, standingAt+1)
	}
	t.Logf("one refused commit: walk 1 names the rule, walk 2 loses the sentence, walk 3 abandons "+
		"the record and resolves the cursor past it, walks 4 and 5 are silent -- and the group is "+
		"neither removed nor halted, standing at epoch %d until it follows an honest commit to %d",
		standingAt, carol.group.Epoch())
}
