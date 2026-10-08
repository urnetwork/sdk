package urmessage

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/urnetwork/connect/v2026/messagegroup"
)

// ── S2-14's other half: coming back ──────────────────────────────────────────────────────────

// Restore re-enters every group this device's durable store holds a record for.
//
// IT IS THE POINT OF THE DURABLE STORE AND IT IS NOT AUTOMATIC. A caller says Hello first -- every
// authenticator is a MAC over the connection's nonce and a restored session is bound to the nonce
// this transport holds now -- and then calls this. The two are separate because [Device.Connect]
// rebinds the groups a device ALREADY holds, and until this runs a restarted device holds none.
//
// WHAT A RESTORED GROUP CAN AND CANNOT DO, said here because the list is short and the absence of
// it would be read as "everything".
//
//   - It can SEND, AFTER IT HAS RECEIVED ONCE: the session is rebuilt at the record's epoch off
//     the MLS exporter, the stream index continues from the durable reserver, and the server takes
//     the record -- but the first [Group.Send] on a restored group is refused with
//     [ErrNotReconciled] until one [Group.Receive] has completed. That ordering is the CLONE
//     CHECK and it is stated below.
//   - It can RECEIVE, including records sealed BEFORE the restart AND THE ONES IT SEALED ITSELF: a
//     receiver ladder is followed from its root (`trackLocked` passes head index 0), and the
//     cursor is not persisted, so a restored device re-fetches this group's history and
//     re-derives every key it needs for it. Its own records are SHOWN rather than skipped, which
//     is what makes a restored conversation the whole conversation -- and since connect 4c030dc
//     they are shown from the copies [Group.Send] persisted ([DeviceStore.SentRecords]) and never
//     opened, because a member cannot open its own application record (MG-4). An own record the
//     store holds no copy of is counted in [Stats.OwnWithoutCopy]; see [Group.openPageLocked].
//   - It cannot ADD a member or OPEN: both need the epoch-zero founding session, which exists on
//     the founder before the first commit and is not persisted. A restored group answers
//     [ErrAlphaOneAdd] and [ErrNoMemberAdded] by name, which is the same answer a joiner gets.
//   - It CAN INGEST A COMMIT, and that sentence is new. It used to read the other way: a restored
//     group could not ingest one, so it could not follow its group into a later epoch, and that was
//     open item J1-8. The cause was structural rather than an omission -- this package carried its
//     own [messagegroup.GroupHandle] over `mls.LoadGroup`, and two of that interface's twenty six
//     methods cannot be implemented outside `connect/messagegroup` at all, because
//     `messagegroup.EngineProcessed` carries its staged commit in an unexported field. So `Process`
//     and `ApplyCommit` were refused by name. `messagegroup.GroupEngine.LoadGroup` closed it: a
//     restored group is now the SAME handle type a founded or a joined one is, reaching both
//     methods through the same body. WHAT THIS DOES NOT DO is make a second epoch reachable from
//     this package's own API -- [Group.AddMember] still answers [ErrAlphaOneAdd], and nothing here
//     yet drives an ingest on a restored group -- so what changed is that the floor under a second
//     epoch exists, not that the alpha has one. Ledger item 239 is where the rest of it is owed.
//
// A GROUP THAT WILL NOT RESTORE IS NAMED AND THE REST STILL COME BACK. One unreadable epoch state
// must not cost a device every other conversation it is in, so the refusals are collected and
// returned together with whatever did restore.
//
// ---------------------------------------------------------------------------------------------
// ONE COPY OF THE APP-DATA FOLDER IS TWO DEVICES ON ONE IDENTITY, AND THIS IS WHERE THAT IS MET.
// ---------------------------------------------------------------------------------------------
//
// A DURABLE IDENTITY IS WHAT MAKES A RESTART A RESTORE AND IT IS ALSO WHAT MAKES A COPY DANGEROUS.
// Before the store existed, a restarted device drew a FRESH Ed25519 identity and a fresh MLS
// group, so a copied directory was harmless: the copy was a different leaf with a different
// sender_handle and it collided with nothing. With the identity on the disk, a copied folder is a
// second device at the SAME leaf, the SAME sender_handle, the SAME epoch and the SAME stream
// counter -- and two records under one (epoch, sender_handle, stream_index) are one record_key and
// one nonce, which spec A section 5.6 calls a total break of both AEADs for that record. The
// single-writer exclusion does NOT reach this: it is held per DIRECTORY, and a copy is a second
// directory, so both opens are granted and neither knows about the other.
//
// THE SERVER REFUSING THE DUPLICATE SUBMISSION IS NOT A DEFENCE AND IS NOT TREATED AS ONE. The
// message server answers REASON_STREAM_INDEX_REUSED to the second record -- NOT
// REASON_STREAM_INDEX_REGRESSED, which this paragraph said until it was read out of the server's
// source. The two are different gates at different depths and the difference is the whole of why
// the name matters here:
//
//	msgrepo store/memory.go:442-452   step (0), the idempotency probe, BEFORE any gate, any
//	                                  allocation and the row lock: a claim already standing at
//	                                  this (group_id, sender_handle, stream_index) whose body_hash
//	                                  or head hash differs is `probeDiffers`, and
//	                                  `return self.refuseBatch(..., protocol.Reason_REASON_STREAM_INDEX_REUSED), nil`
//	msgrepo store/memory.go:610       step (3), stream monotonicity, INSIDE the transaction:
//	                                  `if seen && record.StreamIndex <= last { return protocol.Reason_REASON_STREAM_INDEX_REGRESSED }`
//
// A clone submits at an index the server has a DIFFERENT record at, so it is answered at step (0)
// and never reaches step (3). A reader chasing "when does the refusal happen?" to the monotonicity
// gate would be reading a path this case does not take. `connect/protocol/message.proto:568-569`
// carries both names; neither is invented.
//
// AND THE SEALING HAS ALREADY HAPPENED BY THEN, which is why none of this is the defence. Two
// ciphertexts under one keystream exist on this disk and on the wire whether or not the server
// stores the second. What the reason IS good for is the seal path's half of the clone check: see
// [Group.cloneRefusalLocked], which reads it as evidence rather than as text.
//
// SO THE CHECK IS BEFORE THE SEAL WHERE IT CAN BE, AND IT IS THREE CLAUSES.
//
//  1. A RESTORED GROUP WILL NOT SEND UNTIL IT HAS RECEIVED A CLEAN, COMPLETE WALK.
//     [Group.Receive] walks the group's whole history -- the cursor is not persisted --
//     authenticates every record of this device's own (since MG-4 without opening it; see
//     ownFrameAlreadySpent for what that establishes), and holds the highest stream index it has
//     found, over every walk since the restore, against
//     [messagegroup.StreamIndexReserver]'s HighWater for this stream. Every index on the server
//     under this sender_handle was allocated by this device's reserver and a reserver never
//     rewinds, so an index ABOVE the high water was sealed by something else holding these keys.
//     There is no second reading of it. The group then refuses to seal, for the life of the
//     process, with [ErrIdentityInUse].
//
//     "CLEAN AND COMPLETE" IS LOAD-BEARING AND IT USED TO BE ONLY "COMPLETE", which made the two
//     cheapest ways past this check failures the client had already detected and PRINTED: a page
//     the server itself said was short ([ErrFetchOmitted]) and a walk that lost a record
//     ([ErrRecordOpen]) both set `reconciled` anyway. See [Group.walkReconcilesLocked].
//
//  2. AFTER IT HAS RECONCILED, an own record that opens under this device's sender_handle and is
//     not a record this device sealed is the same finding, refused the same way. TWO SHAPES, and
//     the second is the one an index-only check could not see: an index this device never sealed
//     at, and an index it DID seal at carrying a body_hash that is not the one it sealed. The
//     hash is why [Group.ownIndices] is a map and not a set; §3.1's body_hash is authenticated by
//     both AEADs, so a party without this group's keys cannot produce a record that opens at all.
//
//  3. AT THE SUBMIT, a REASON_STREAM_INDEX_REUSED is the same finding a third time and is made
//     sticky there. Clauses 1 and 2 both live in [Group.Receive], and a user types the next line
//     rather than fetching first -- so until this clause existed, two level copies that kept
//     sending collided on EVERY index rather than once. See [Group.cloneRefusalLocked] for what
//     the reason means in the server's own source and for why an honest device cannot provoke it.
//
// WHAT THIS COVERS: every copy that is BEHIND the original -- a phone backup, a folder copied last
// week, a partial restore that brought the state directory and not the stream directory -- is
// caught at its first Receive, BEFORE it has sealed anything. So is a copy that cannot reach the
// server at all, because clause 1 refuses the send when the reconciliation has not run. And a copy
// that is exactly level but LISTENS before it speaks is caught by clause 2 with no ciphertext
// produced at all.
//
// WHAT IT DOES NOT COVER, said plainly because a half-stated defence is worse than none. TWO
// COPIES THAT ARE EXACTLY LEVEL and both SEAL before either fetches again agree with the server
// and with each other at the moment of the seal, so nothing on either side has any evidence the
// other exists. Both seal at that index, and that index's two ciphertexts exist before any server
// has said anything at all. NO CLIENT-SIDE CHECK CAN PREVENT IT and none of what follows is a
// mitigation of it.
//
// AND HERE IS THE BOUND ON WHAT FOLLOWS, WHICH IS THE SENTENCE A PREVIOUS VERSION OF THIS
// PARAGRAPH GOT WRONG. It said "the count is one record, not a stream of them". That was FALSE and
// was falsified by measurement -- four typed messages produced four collided indices, because the
// only check was in [Group.Receive] and neither copy fetched. The bound now has a PRECONDITION,
// and the precondition is the half that was missing:
//
//	FOR A COPY WHOSE SUBMISSIONS ARE ANSWERED:
//	  ONE CONTESTED STREAM INDEX PER GROUP, PER PROCESS LIFETIME OF THE LOSING COPY.
//
// The server accepts one submission and answers the other REASON_STREAM_INDEX_REUSED; clause 3
// makes that [ErrIdentityInUse] AT THE SUBMIT, sticky for the life of the process, so the losing
// copy seals nothing further however many lines are typed. The winner is then the only writer of
// that stream and produces no further collision.
//
// IT IS NOT "ONE FOR EVER" AND SAYING SO WOULD BE THE SAME MISTAKE AGAIN. The sticky bit is
// in-process. A losing copy that is RESTARTED comes back unreconciled, must Receive before it may
// Send, and pays at most one more contested index before clause 1 or clause 3 stops it again -- so
// a user who keeps reopening a copied folder keeps paying, once per launch. Measured:
// `cp3b.TestARestartedLosingCopyCostsAtMostOneMoreIndex`, which in the ordinary shape (the winner
// has gone on sending) costs ZERO more, because clause 1's high-water comparison fires; the case
// asserts the bound rather than the lucky number.
//
// ────────────────────────────────────────────────────────────────────────────────────────────
// AND THE PRECONDITION IS LOAD-BEARING: A COPY THAT SEALS WHILE IT CANNOT REACH THE SERVER IS
// BOUNDED BY NOTHING. This is stated here, in the same paragraph as the bound, because a bound
// whose exception lives somewhere else is how the last two of these sentences came to be false.
//
// EVERY CLAUSE OF THIS CHECK IS FED BY SOMETHING THE SERVER SAID -- clauses 1 and 2 by records it
// handed over, clause 3 by the reason it answered. [Group.Send] consumes a stream index and
// produces a ciphertext BEFORE it submits, deliberately -- [Group.Send] notes the index AT THE
// SEAL and says why: a submit whose answer never arrived is still a record this device SEALED, and
// a device that recorded only acknowledged indices would meet its own lost record on a later fetch
// and read it as a second writer. So a copy that
// RECONCILED while it had a connection and then lost it goes on sealing, no evidence ever reaches
// it, and it collides ONCE PER SEND. Measured at four contested indices from four sends, with
// IdentityInUse nil on both sides:
// `cp3b.TestACopyThatSealsWhileItCannotReachTheServerIsBoundedByNothing`.
//
// IT IS ONE WORD AWAY FROM A CASE THAT IS COVERED, and the two must not be read as one. "A copy
// that cannot reach the server AT ALL" is caught, because clause 1 refuses the send of a group
// that has never reconciled. This is a copy that DID reconcile, and then went dark.
//
// NOTHING IN THIS BUILD CLOSES IT AND NO DETECTION CAN: the evidence does not exist on this side of
// the wire. The two things that would are S2-28's new LEAF, and a send path that will not seal at a
// new index while an older one is unacknowledged -- which is a protocol decision, would break the
// lost-answer recovery this package depends on, and is not taken here.
// ────────────────────────────────────────────────────────────────────────────────────────────
//
// AND THE COUNT IS TAKEN OFF THE CIPHERTEXTS SUBMITTED, NOT OFF THE SERVER'S STORED ROWS. The
// losing record is refused, so it is never a row; a measurement on rows would report "no index
// used twice" over a genuine two-time pad. `cp3b`'s captureStore records every ct_body the client
// asks to write, which is the only place the event is visible.
//
// WHAT WOULD CLOSE IT PROPERLY: a copy that came back under a DIFFERENT leaf, which is an MLS
// Update commit. THE HANDLE-LEVEL BLOCKER ON THAT IS GONE AND THE REST OF IT IS NOT: J1-8 is closed
// -- a restored group ingests a commit, because it is the same handle a live one is -- and what is
// still missing is everything above the handle. [Group.AddMember] answers [ErrAlphaOneAdd], nothing
// in this package drives a second epoch, and the ladders do not survive one (ledger item 239).
// **FILED AS S2-28: a copied app-data folder
// needs a new leaf, not a detection.** Until it is ruled, the three clauses above are the whole
// answer and the sentences you are reading are the rest of it. Every case in this paragraph is
// driven in `sdk/cp3b`: `clone_test.go` for the copy that is behind and the level copy that
// listens, `reconcilegate_test.go` for clause 1's two short-walk routes and the clean-walk
// control, and `sealgate_test.go` for clause 3, the residual's bound, the restart, the honest
// resubmission that must NOT be read as a clone, and the partitioned copy that nothing bounds.
func (self *Device) Restore(ctx context.Context) ([]*Group, error) {
	store, durable := self.stateStore.(DeviceStore)
	if !durable {
		return nil, ErrNoDeviceStore
	}
	// A LEAVE THAT DID NOT FINISH ERASING IS FINISHED FIRST, and before the nonce is asked for: it
	// needs no connection, and a device that cannot connect should still not keep a group it left
	// on the disk. A group being left is never restored ([DurableStateStore.GroupRecords] skips
	// it), and an erase that fails again is reported and tried again at the next Restore.
	// one mark it cannot read is reported, and does not stop the others being finished
	leaving, unfinished := store.GroupsBeingForgotten()
	for _, groupId := range leaving {
		if self.holdsGroup(groupId) {
			continue
		}
		if err := store.DeleteGroupRecord(groupId); err != nil && unfinished == nil {
			unfinished = fmt.Errorf("%w: group %x: %w", ErrForgetUnfinished, groupId, err)
		}
	}
	nonce, nonceEpoch, err := self.nonce()
	if err != nil {
		return nil, err
	}
	records, err := store.GroupRecords()
	if err != nil {
		return nil, err
	}
	restored := []*Group{}
	var firstFailure error
	for _, record := range records {
		if self.holdsGroup(record.GroupId) {
			continue
		}
		group, err := self.restoreOne(store, record, nonce, nonceEpoch)
		if err != nil {
			if firstFailure == nil {
				firstFailure = err
			}
			continue
		}
		restored = append(restored, group)
	}
	if firstFailure == nil {
		firstFailure = unfinished
	}
	return restored, firstFailure
}

// restoreOne rebuilds one group: the MLS state at the record's epoch, then the session over it.
//
// THE HANDLE COMES OUT OF THE ENGINE AND NOT OUT OF mls, WHICH IS J1-8 CLOSED. This package used to
// call `mls.LoadGroup` directly and wrap the result in a handle of its own, because
// `messagegroup.GroupEngine` had four methods and none of them opened a persisted group. That copy
// could not implement two of [messagegroup.GroupHandle]'s twenty six methods -- `EngineProcessed`
// carries its staged commit in an unexported field, so the only value an implementation outside
// that package can build is one `ApplyCommit` refuses -- so it refused `Process` and `ApplyCommit`
// by name and a restored group could not ingest a commit. `GroupEngine.LoadGroup` now answers the
// SAME handle type a founded or a joined group is seen through, so a restored group reaches every
// method through the same body a live one does and the refusal is gone rather than relocated.
//
// THE EPOCH COMPARISON MOVED WITH IT and is not repeated here. It used to be this function's, three
// lines below the load; it is now [messagegroup.GroupEngine.LoadGroup]'s own, refusing with
// `messagegroup.ErrEngineLoadedEpoch`, which is where it reaches every caller of that interface
// rather than the one caller that remembered to make it. The wrap below carries the group id and
// the epoch this record named, so an operator reading the refusal still sees which row asked.
func (self *Device) restoreOne(store DeviceStore, record *GroupRecord, nonce []byte, nonceEpoch uint64) (*Group, error) {
	handle, err := self.engine.LoadGroup(record.GroupId, record.Epoch)
	if err != nil {
		return nil, fmt.Errorf("%w: group %x at epoch %d: %w", ErrRestore, record.GroupId, record.Epoch, err)
	}
	// THE TABLE THIS RECORD CAME BACK WITH, AND WHAT IT IS WHEN THERE IS NONE. A record written
	// before item 251's ruling 40 has five parts and nil here; one written by this build has six
	// and a table, possibly empty. [restoredPqSecrets] turns either into the map this group runs
	// on, and it is where the old store's single scalar becomes pq_secret at the epoch the record
	// names.
	table, current, err := restoredPqSecrets(record)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("%w: group %x at epoch %d: %w", ErrRestore, record.GroupId, record.Epoch, err)
	}
	session, err := messagegroup.NewGroupSession(handle, current, record.GroupHandleKey,
		self.reserver, self.nowMs, nonce)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("%w: group %x: the session at epoch %d: %w",
			ErrRestore, record.GroupId, record.Epoch, err)
	}
	if err := session.InstallPastEpochLoader(self.pastEpochLoader(record.GroupId)); err != nil {
		session.Close()
		return nil, fmt.Errorf("%w: group %x: the past epoch loader: %w", ErrRestore, record.GroupId, err)
	}
	// THE PAST EPOCHS GO BACK INTO THE SESSION, AND THE PREMISE IS DROPPED ONLY IF THE TABLE
	// REFUTES IT. connect's [messagegroup.GroupSession] starts every session holding the
	// group-lifetime premise -- "the one secret I have answers every epoch" -- which is correct
	// for every group written before rotation and WRONG for a group that has rotated, where it
	// re-derives a past epoch's storage root out of today's secret and every record of that epoch
	// stops opening at the AEAD tag with nothing saying why. Rotation is a property of the GROUP
	// and is durable; the refutation a session makes by comparing octets is a property of ONE
	// PROCESS and dies with it. This is where the durable fact is handed back.
	//
	// IT IS DECIDED ON THE OCTETS AND NOT ON THE ARITY, which is the same rule connect uses one
	// layer down. A six-part record whose rows all carry ONE value is a group this build wrote
	// and that has not rotated -- a founder that never committed, or a joiner holding one epoch --
	// and declaring it rotated would cost it every epoch it holds no separate row for, refusing
	// history it can in fact still read. So the declaration follows two different values and
	// nothing else.
	//
	// THE RESIDUAL, NAMED: a device that joined a ROTATING group at epoch n holds one row, so the
	// premise stands and an epoch below n would be answered with pq_secret[n]. It holds no MLS
	// state below n either, so that epoch refuses at the past-epoch store before a secret is ever
	// asked for -- the bound is real, and it is the same residual connect's own pqsecret.go header
	// names from the other side.
	for _, row := range table {
		if row.epoch == record.Epoch {
			continue
		}
		// A ROW OUTSIDE THE WINDOW IS SKIPPED AND NOT REFUSED, and the difference is a device that
		// starts against one that does not. messagegroup.InstallPqSecret answers
		// ErrEpochOutOfWindow for a row more than PastEpochWindow behind -- correctly, since no
		// open at that epoch would be admitted -- and a restore that made that an error would
		// refuse the whole group over a row nothing can use. The writer bounds the table at
		// [Group.enterEpochLocked], so a row here is one a store written before that bound landed
		// still holds, or one a hand-edited file carries.
		if row.epoch < record.Epoch && record.Epoch-row.epoch > messagegroup.PastEpochWindow {
			continue
		}
		if err := session.InstallPqSecret(row.epoch, row.secret); err != nil {
			session.Close()
			handle.Close()
			return nil, fmt.Errorf("%w: group %x: pq_secret[%d]: %w", ErrRestore, record.GroupId, row.epoch, err)
		}
	}
	if pqSecretsShowRotation(table) {
		if err := session.DeclarePqSecretRotated(); err != nil {
			session.Close()
			handle.Close()
			return nil, fmt.Errorf("%w: group %x: %w", ErrRestore, record.GroupId, err)
		}
	}
	restoredDark, restoredHalt := restoredDiagnosisOf(record.WrapDarkKind, record.WrapDarkEpoch)
	restored := &Group{
		device:         self,
		id:             append([]byte(nil), record.GroupId...),
		handle:         handle,
		groupHandleKey: append([]byte(nil), record.GroupHandleKey...),
		pqSecrets:      pqSecretsMapOf(table),
		session:        session,
		sessionBound:   nonceEpoch,
		epoch:          record.Epoch,
		opened:         record.Opened,
		// AND THE DIAGNOSIS THIS GROUP CAME BACK DARK WITH, which is ruling 38 surviving the
		// process that took it. Without this the restored group holds a pq_secret no peer agrees
		// with, a table that reads as healthy -- [pqSecretsShowRotation] compares octets and the
		// fallback wrote the same octets as the epoch below -- and no sentence anywhere, so Send
		// and Receive answer the server's generic refusal instead of naming the wrap. nil when
		// the record is not dark, and nil for a five- or six-part record, which is a disk with
		// no diagnosis on it rather than a healthy group; [GroupRecord.WrapDarkKind] says what
		// that costs.
		// AND RULING 41's HALT COMES BACK AS A HALT AND NOT AS A DARK STATE. One persisted kind
		// column, two fields, and [restoredDiagnosisOf] is the one place the octet decides which:
		// a halted group REFUSED a commit and never entered the epoch it opens, and restoring
		// that as [Group.wrapDark] would be this build persisting one diagnosis and reading back
		// another.
		wrapDark:      restoredDark,
		wrapDarkEpoch: record.WrapDarkEpoch,
		halted:        restoredHalt,
		haltedEpoch:   record.WrapDarkEpoch,
		// AND RULING 52's STATE, OUT OF ITS OWN PART AND NOT OUT OF THE COLUMN ABOVE. A device a
		// commit removed comes back knowing it, which is the whole of why part ten exists: mls can be
		// asked only ONCE -- it closes the group and zeroizes its epoch secrets as it answers -- and
		// what the walk did with that answer before this field existed was spend [maxRecordAttempts]
		// on the record and then go quiet for ever. [removedErrorOf] answers nil for a record that is
		// not removed AND for a record written before part ten, which is a disk carrying nothing about
		// this rather than a device that is certainly still a member; [GroupRecord.RemovedKind] prices
		// that, and the price is one WALK and not one device, because the removing commit is still the
		// first record above a cursor that is not persisted.
		removed:      removedErrorOf(record.RemovedKind, record.RemovedEpoch),
		removedEpoch: record.RemovedEpoch,
		// AND NOT RECONCILED. This is the one place a [Group] is built over an identity that
		// existed before this process did, so it is the one place a SECOND copy of that
		// identity is possible. [Group.Send] refuses until [Group.Receive] has walked this
		// group's history once and held the stream indices it finds against this device's own
		// durable reserver. See Restore's header for what that covers and what it does not.
		reconciled: false,
	}
	restored.initTables()
	// AND THE WITNESS THE WINDOW DOES NOT PRUNE, WHICH IS ITEM 243's RULE SURVIVING THE PROCESS
	// THAT LEARNED IT. [Group.initTables] has just seeded a witness row for every secret in the
	// restored table; these are the rows for the epochs the table no longer covers -- the ones the
	// window moved past before the record was written -- and without them a restart would put the
	// removal rule's subject back inside the window that defeated it.
	//
	// AN EMPTY PART IS NOT A DEFECT AND IS NOT INVENTED AROUND, and it is the residual
	// [Group.pqSecretWitness] names: a record written before this part existed carries no rows, so
	// what comes back is the witness of the table it did carry. That device follows a removal
	// fanned out on a secret it once held and has since evicted AND forgotten, and the only repair
	// for it is on the wire.
	for _, row := range record.PqSecretWitness {
		if len(row.Digest) != sha256.Size {
			return nil, fmt.Errorf("%w: group %x: a pq_secret witness row for epoch %d is %d octets and a digest is %d",
				ErrRestore, record.GroupId, row.Epoch, len(row.Digest), sha256.Size)
		}
		witness := [sha256.Size]byte{}
		copy(witness[:], row.Digest)
		if _, already := restored.pqSecretWitness[row.Epoch]; !already {
			restored.pqSecretWitness[row.Epoch] = witness
		}
	}
	// AND THE LEAF LEDGER, WHICH IS LEDGER ITEM 245 SURVIVING THE PROCESS THAT LEARNED IT.
	// [Group.initTables] has just seeded the handle set with the ONE leaf this device stands at
	// today; these are the rows the record carries -- leaves this device stood at BEFORE the one it
	// stands at now, and leaves whose occupants this device watched a commit remove.
	//
	// AN EMPTY PART IS NOT A DEFECT AND IS NOT INVENTED AROUND, and it is the residual
	// [GroupRecord.Leaves] names: a record written before part nine carries no rows, so what comes
	// back is the one leaf the tree says. That device is in the state every build before this one
	// was in -- it resolves a departed leaf's records only while the leaf has been REFILLED, and it
	// shows lines it wrote from an earlier leaf as a stranger's -- and it STARTS, which is the
	// property that decides how a compatibility question is answered here.
	//
	// THE HANDLE IS DERIVED AND NEVER STORED, so a ledger written by a build with a different
	// derivation could not put a handle in this set at all: what it carries is a leaf index, and the
	// expansion is this build's own.
	for _, row := range record.Leaves {
		if row.Own {
			restored.ownHandles[messagegroup.SenderHandle(restored.groupHandleKey, row.Leaf)] = true
		}
		if row.DepartedEpoch != 0 {
			restored.departedAt[row.Leaf] = row.DepartedEpoch
		}
	}
	// AND THE COPIES OF WHAT THIS DEVICE SAID IN IT, which since connect 4c030dc are the only
	// place its own half of the conversation can be read from (MG-4: a member cannot open its own
	// application record). A store that will not answer refuses THIS group by name rather than
	// bringing it back without them: a restored conversation missing every line the user typed,
	// with a nil error, is the defect S2-14's own-half clause exists to keep out.
	sent, err := store.SentRecords(record.GroupId)
	if err != nil {
		session.Close()
		handle.Close()
		return nil, fmt.Errorf("%w: group %x: the copies of this device's own records: %w", ErrRestore, record.GroupId, err)
	}
	for _, one := range sent {
		restored.ownIndices[one.StreamIndex] = &ownSealed{
			bodyHash: one.BodyHash,
			hasCopy:  true,
			body:     one.Body,
			sentAtMs: one.SentAtMs,
		}
	}
	// AND THE HEADS THIS DEVICE HAD AUTHENTICATED, ledger item 241's restart half. They go into
	// TWO places and the split is the whole of what makes the re-walk correct. The table itself
	// goes to [Group.persistedHeads], read only, and positions a PRIOR epoch's ladder at the head
	// as of the START of that epoch. And the CURRENT epoch's ladders are seeded from the same
	// table at the highest head of any epoch BELOW the current one -- never the current epoch's
	// own head, because the re-walk opens the current epoch's records again from the first and a
	// ladder above them refuses every one. A store that answers no table -- a directory the build
	// before this one wrote -- leaves both empty, which is what that build did. A store that will
	// not READ refuses this group by name, for SentRecords' reason one paragraph up.
	heads, err := store.PeerHeads(record.GroupId)
	if err != nil {
		session.Close()
		handle.Close()
		return nil, fmt.Errorf("%w: group %x: the authenticated receiver heads: %w", ErrRestore, record.GroupId, err)
	}
	for _, head := range heads {
		ladder := ladderKey{leaf: head.Leaf, retentionWire: head.RetentionWire, ephWindow: head.EphWindow}
		restored.persistedHeads[epochLadderKey{epoch: head.Epoch, ladderKey: ladder}] = head.Head
		if head.Epoch < record.Epoch && restored.peerHeads[ladder] < head.Head {
			restored.peerHeads[ladder] = head.Head
		}
	}
	self.hold(restored)
	return restored, nil
}

// persistSent writes the copy of one record this device sealed, when the store can hold one.
//
// A NO-OP ON A STORE THAT IS NOT DURABLE, for persistGroup's reason: a device over a map loses its
// groups at exit, and with them any use for a copy of what it said in them. The in-memory copy on
// [Group.ownIndices] is what shows a lost answer's record in THIS process on either store.
func (self *Device) persistSent(groupId []byte, streamIndex uint64, sealed *ownSealed) error {
	store, durable := self.stateStore.(DeviceStore)
	if !durable {
		return nil
	}
	return store.PutSentRecord(groupId, &SentRecord{
		StreamIndex: streamIndex,
		BodyHash:    sealed.bodyHash,
		SentAtMs:    sealed.sentAtMs,
		Body:        sealed.body,
	})
}

// persistPeerHeads writes one group's [PeerHead] table, when the store can hold one.
//
// A NO-OP ON A STORE THAT IS NOT DURABLE, for persistSent's reason: a device over a map loses its
// groups at exit, and with them any use for the heads a restart would track them at.
func (self *Device) persistPeerHeads(groupId []byte, heads []PeerHead) error {
	store, durable := self.stateStore.(DeviceStore)
	if !durable {
		return nil
	}
	return store.PutPeerHeads(groupId, heads)
}

// pastEpochLoader is the door a group's session rebuilds a PRIOR epoch's schedule through: the
// engine's own LoadGroup over this device's store, at the epoch asked for, and nothing wider.
// Ledger item 241; the whole account of what the session does with the handle is connect's
// messagegroup/pastepoch.go.
//
// IT IS INSTALLED ON EVERY SESSION A GROUP RECEIVES THROUGH -- the founder's after its first
// commit, the joiner's, and the restored one -- and never on the founding session at epoch zero,
// which seals one commit and opens nothing. A session it is not installed on is the single-epoch
// session this package shipped before item 241, and the walk would see every prior-epoch record
// as [ErrRecordOpen] carrying messagegroup.ErrRecordNotForThisSession: a loud failure and not a
// quiet gap, which is the right shape for a wiring mistake.
func (self *Device) pastEpochLoader(groupId []byte) messagegroup.PastEpochLoader {
	id := append([]byte(nil), groupId...)
	return func(epoch uint64) (messagegroup.GroupHandle, error) {
		return self.engine.LoadGroup(id, epoch)
	}
}

// holdsGroup reports whether this device already has a live view of that group, so that a second
// Restore is a no-op rather than a second session over one durable reserver row.
func (self *Device) holdsGroup(groupId []byte) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	_, held := self.groups[string(groupId)]
	return held
}

// persistGroup writes the urmessage-side record of one group, when the store can hold one.
//
// IT IS A NO-OP ON A STORE THAT IS NOT DURABLE, which is what keeps [MemoryStateStore] the exact
// behaviour it had: a device over a map goes on losing everything at exit and does not pay for a
// record nothing will read.
func (self *Device) persistGroup(record *GroupRecord) error {
	store, durable := self.stateStore.(DeviceStore)
	if !durable {
		return nil
	}
	return store.PutGroupRecord(record)
}
