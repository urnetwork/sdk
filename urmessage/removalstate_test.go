package urmessage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// LEDGER RULING 52: THE STATE A REMOVED MEMBER IS LEFT IN
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHAT THIS FILE IS ABOUT, AS MEASURED AT sdk ca89760 BEFORE ANY OF IT EXISTED. A device a commit
// removed was told once, generically, and then went quiet for ever:
//
//	1st Receive: ErrCommitIngest: applying the commit: mls: this client was removed by the commit
//	2nd Receive: ErrCommitIngest: processing the commit: mls: the group is closed and its epoch secrets have been zeroized
//	3rd Receive: ErrRecordAbandoned: record 7, after 3 attempts: <the 2nd sentence>
//	4th, 5th, 6th Receive: nil, with Stats.Omitted at 0 and a live composer.
//
// So there are four properties here and they are four different mechanisms: the sentinel has to be
// NAMED rather than generic; it has to survive the ONE walk mls can be asked on; the walk must not
// spend maxRecordAttempts on the record and resolve past it; and the whole thing has to survive a
// restart, which is what part TEN of [GroupRecord] is for. The Send half and the restart half are
// driven end to end over a real server by cp3b's
// TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart; what is here is the walk's
// own behaviour, the store's codec, and the compatibility question a new part always raises.

// A REMOVED DEVICE IS TOLD BY NAME ON EVERY WALK, AND THE RECORD IS NEVER ABANDONED.
//
// THE FOUR CLAUSES, each against a different one of the four defects above: the FIRST walk answers
// [ErrRemovedFromGroup] and NOT [ErrCommitIngest] (named, not generic) while still carrying mls's own
// sentinel so the cause is readable; the SECOND, THIRD and FOURTH walks answer the same sentinel,
// which is the state being sticky rather than re-derived -- mls cannot be asked twice, it closes the
// group as it answers; [Stats.Unopened], [Stats.FailedOpen] and [Group.UnopenedRecords] stay EMPTY
// across all four, which is the walk not treating a removal as a record that did not open; and the
// group stays at the epoch it was removed at, so nothing was half-applied.
//
// AND IT IS TOLD APART FROM THE OTHER THREE STATES BY NAME, in the same assertion block: not
// [ErrRemovalWithoutRotation] (ruling 41's halt, a commit this device REFUSED), not
// [ErrNoWrapForEpoch]/[ErrWrapUnreadable]/[ErrOrphanWrap] (ruling 38's dark states, a commit it
// FOLLOWED without keys), and not [ErrFetchRefused] or [ErrRecordAbandoned]. That list is the
// ruling's own text turned into a predicate.
//
// THE INLINE CONTROL IS THE SURVIVOR OF THE SAME COMMIT, in the same case and over the same page: bob
// walks the identical removal, follows it into the epoch it opens, and reads (0, nil) from
// [Group.Removal]. Without it every clause above is satisfied by a field set on every ingest.
//
// WHAT WOULD GO RED: drop the mls.ErrRemovedFromGroup arm in [Group.ingestCommitLocked] step (4)
// (the first walk answers ErrCommitIngest); drop the `self.removed != nil` clause in
// [Group.openPageLocked]'s is_commit arm (the second walk answers `the group is closed`, the third
// [ErrRecordAbandoned] with Unopened at 1, the fourth nil); set the state on any ApplyCommit failure
// (the control's survivor is removed too); set it for every ingest (the same).
func TestARemovedDeviceAnswersTheRemovalOnEveryWalkAndTheRecordIsNeverAbandoned(t *testing.T) {
	ctx := context.Background()
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// NAME EVERY MEMBER IN THE POLICY, so the verb has an entry to drop and the commit it derives
	// is not an R0c phantom every receiver refuses before it can remove anybody.
	named := rotPolicyOf(t, alice)
	named.SetRole(bob.dev.identityPub, mls.RoleAdmin)
	named.SetRole(carol.dev.identityPub, mls.RoleMember)
	naming := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitPolicy(rotPolicyBody(t, named))
	})
	for _, who := range []*rotMember{bob, carol} {
		if err := world.deliver(who, naming.page()...); err != nil {
			t.Fatalf("%s's walk over the policy that names everybody: %v", who.name, err)
		}
	}

	// BEFORE THE REMOVAL BOTH RECEIVERS ARE MEMBERS AND SAY SO: the control that makes the two
	// different answers below two answers rather than one constant.
	for _, who := range []*rotMember{bob, carol} {
		if epoch, removal := who.group.Removal(); removal != nil || epoch != 0 {
			t.Fatalf("CONTROL FAILED: %s reads (%d, %v) from Removal while it is still a member",
				who.name, epoch, removal)
		}
	}
	removedAt := carol.group.Epoch()

	// THE VERB'S OWN DERIVATION, captured and refused so nothing is published, then committed
	// through the seam: the vector under test is the one [Group.RemoveMember] computed.
	capture := captureOutgoingOn(t, alice.group)
	if err := alice.group.RemoveMember(ctx, carol.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("alice's RemoveMember answered %v, want this case's authorizer refusal", err)
	}
	alice.group.device.commitAuthorizer = nil
	if capture.calls != 1 {
		t.Fatalf("the configured authorizer saw %d decision(s), want 1", capture.calls)
	}
	removal := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemoveWithExtensions(capture.decision.RemovedLeaves,
			capture.decision.ExtensionsAfter)
	})

	// ── THE SURVIVOR, FIRST, SO THE CONTROL IS TAKEN OVER THE SAME PAGE ─────────────────────────
	if err := world.deliver(bob, removal.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: the survivor's walk over the removal answered %v; every clause "+
			"below would then be about a page nobody could follow", err)
	}
	if got := bob.group.Epoch(); got != removal.opens {
		t.Fatalf("CONTROL FAILED: the survivor is at epoch %d after the removal, want %d", got, removal.opens)
	}
	if epoch, state := bob.group.Removal(); state != nil || epoch != 0 {
		t.Errorf("the SURVIVOR of the removal reads (%d, %v) from Removal: the state is being set "+
			"for a member the commit left in the group", epoch, state)
	}

	// ── AND THE REMOVED DEVICE, FOUR WALKS OVER THE SAME PAGE ───────────────────────────────────
	for walk := 1; walk <= 4; walk += 1 {
		err := world.deliver(carol, removal.page()...)
		if !errors.Is(err, ErrRemovedFromGroup) {
			t.Fatalf("walk %d over the removal answered %v, want ErrRemovedFromGroup. Before ruling "+
				"52 walk 1 answered ErrCommitIngest, walk 2 `the group is closed and its epoch "+
				"secrets have been zeroized`, walk 3 ErrRecordAbandoned and walk 4 NIL", walk, err)
		}
		if !errors.Is(err, mls.ErrRemovedFromGroup) {
			t.Errorf("walk %d does not carry mls.ErrRemovedFromGroup, which is the cause and is a "+
				"value rather than state: %v", walk, err)
		}
		// NAMED AND NOT GENERIC, which is the ruling's second clause: ErrCommitIngest is what a
		// bent ciphertext and a commit whose exporter failed both answer, and a caller that saw it
		// here would read a membership that ended as a transient it should retry.
		if errors.Is(err, ErrCommitIngest) {
			t.Errorf("walk %d answers ErrCommitIngest as well, so a caller cannot tell a removal "+
				"from a commit that did not open: %v", walk, err)
		}
		for _, other := range []struct {
			name string
			err  error
		}{
			{"ErrRemovalWithoutRotation (ruling 41's halt: a commit this device REFUSED)", ErrRemovalWithoutRotation},
			{"ErrNoWrapForEpoch (ruling 38: a commit it FOLLOWED with no keys)", ErrNoWrapForEpoch},
			{"ErrWrapUnreadable", ErrWrapUnreadable},
			{"ErrOrphanWrap", ErrOrphanWrap},
			{"ErrRecordAbandoned (a record that did not open)", ErrRecordAbandoned},
			{"ErrFetchRefused (the transport)", ErrFetchRefused},
			{"ErrIdentityInUse", ErrIdentityInUse},
		} {
			if errors.Is(err, other.err) {
				t.Errorf("walk %d also answers %s; ruling 52's whole content is that this state is "+
					"distinguishable from that one", walk, other.name)
			}
		}
		// THE WALK DID NOT TREAT IT AS A RECORD THAT DID NOT OPEN: no attempt spent, nothing
		// abandoned, and the cursor still below the commit -- which is what makes walk 2 possible.
		stats := carol.group.Stats()
		if stats.FailedOpen != 0 || stats.Unopened != 0 || len(carol.group.UnopenedRecords()) != 0 {
			t.Errorf("after walk %d the removed device has FailedOpen %d, Unopened %d and unopened "+
				"records %v: three attempts and a cursor bump is the shape of a transient, and a "+
				"removal is the one record a device cannot open and must not retry",
				walk, stats.FailedOpen, stats.Unopened, carol.group.UnopenedRecords())
		}
		if epoch, state := carol.group.Removal(); state == nil || epoch != removedAt {
			t.Errorf("after walk %d Removal answers (%d, %v), want (%d, non-nil)",
				walk, epoch, state, removedAt)
		}
		if got := carol.group.Epoch(); got != removedAt {
			t.Errorf("after walk %d the removed device is at epoch %d, want %d", walk, got, removedAt)
		}
	}

	// AND THE SEND DOOR ANSWERS THE SAME STATE. It is the door and not [Group.Send] because this
	// package has no transport -- the real verb is driven in cp3b -- and it is the door every send
	// kind goes through.
	if _, err := carol.group.sendableLocked(KindText); !errors.Is(err, ErrRemovedFromGroup) {
		t.Errorf("the removed device's send door answered %v, want ErrRemovedFromGroup. Measured "+
			"before ruling 52: `sealing a message: messagegroup: an application record's inner MLS "+
			"frame did not open: mls: the group is closed and its epoch secrets have been zeroized`, "+
			"which carries no sentinel a composer could branch on", err)
	}
	if err := carol.group.committableLocked(); !errors.Is(err, ErrRemovedFromGroup) {
		t.Errorf("the removed device's commit door answered %v, want ErrRemovedFromGroup", err)
	}
	// the control beside them: the survivor's own doors are open
	if _, err := bob.group.sendableLocked(KindText); err != nil {
		t.Errorf("CONTROL FAILED: the SURVIVOR's send door answered %v, so the two refusals above "+
			"are satisfied by a door that is shut for everybody", err)
	}
	t.Logf("four walks over one removal: every one answers ErrRemovedFromGroup carrying mls's own "+
		"sentinel, nothing was abandoned, the group stands at epoch %d, and the survivor of the "+
		"same commit is at %d and unaffected", removedAt, removal.opens)
}

// RULING 52's STATE SURVIVES A RESTART, THROUGH PART TEN, AND THE ROUND TRIP IS THE STORE'S OWN.
//
// WHY THIS IS THE CASE THAT DECIDES THE CARRIER. The state is derivable from the wire exactly ONCE
// per MLS handle: mls closes the group and zeroizes its epoch secrets as it answers
// mls.ErrRemovedFromGroup, so the next Process answers `the group is closed`. A device that came back
// without the state would therefore be relying on a re-derivation that only works because the cursor
// is not persisted -- and would go quiet the moment anything above the removal was abandoned. So the
// state goes on the disk, and this asks the disk.
//
// WHAT IS ASSERTED: a removed group's record carries TEN parts with a non-empty tenth; the reader
// hands back the kind and the epoch it was written with; and [Device.restoreOne] rebuilds a group
// that answers [ErrRemovedFromGroup] from [Group.Removal] and from both doors, at the same epoch,
// with mls's own sentinel still in the chain -- which is [removedErrorOf] re-wrapping a VALUE rather
// than inventing a diagnosis.
//
// WHAT WOULD GO RED: drop RemovedKind/RemovedEpoch from [Group.groupRecordLocked]; stop writing part
// ten in [DurableStateStore.PutGroupRecord]; drop the two fields from [Device.restoreOne]'s literal;
// have [removedErrorOf] answer a bare sentence with no sentinel.
func TestTheRemovalStateIsOnTheDiskAndARestoredGroupComesBackKnowingIt(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	removed := world.member("bob")
	removedAt := removed.group.Epoch()

	// the state, set through the one writer rather than by assigning the field: it persists itself.
	if err := removed.group.removedLocked(mls.ErrRemovedFromGroup); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("removedLocked answered %v", err)
	}

	// ── THE DISK ────────────────────────────────────────────────────────────────────────────────
	store := removed.dev.store
	parts, err := store.readRecord(store.groupRecordPath(removed.group.id), stateKindGroupRecord)
	if err != nil {
		t.Fatalf("reading the group record back: %v", err)
	}
	if len(parts) != 10 {
		t.Fatalf("the group record carries %d parts, want 10: part ten is the removal", len(parts))
	}
	if len(parts[9]) != 1+8 {
		t.Fatalf("the removal part is %d octets, want %d (u8 kind, u64 epoch)", len(parts[9]), 1+8)
	}
	if parts[9][0] != removedByCommit {
		t.Errorf("the removal part names kind %d, want %d", parts[9][0], removedByCommit)
	}
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the store holds %d group record(s), want 1", len(records))
	}
	if records[0].RemovedKind != removedByCommit || records[0].RemovedEpoch != removedAt {
		t.Fatalf("the record decodes to kind %d at epoch %d, want %d at %d",
			records[0].RemovedKind, records[0].RemovedEpoch, removedByCommit, removedAt)
	}

	// ── THE RESTORE ─────────────────────────────────────────────────────────────────────────────
	revived := restoredRotDevice(t, removed)
	group, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), removedAt)
	if err != nil {
		t.Fatalf("the restore of a removed group: %v. A group whose device was removed is still a "+
			"group whose history this device may read, so a restore that refused it would take the "+
			"transcript away as well as the membership", err)
	}
	defer group.Close()

	epoch, state := group.Removal()
	if state == nil {
		t.Fatalf("the restored group reads (%d, nil) from Removal: the state did not survive the "+
			"process, and the device is back to reading as caught up and silent", epoch)
	}
	if epoch != removedAt {
		t.Errorf("the restored group was removed at epoch %d, want %d", epoch, removedAt)
	}
	if !errors.Is(state, ErrRemovedFromGroup) {
		t.Errorf("the restored state does not carry ErrRemovedFromGroup: %v", state)
	}
	if !errors.Is(state, mls.ErrRemovedFromGroup) {
		t.Errorf("the restored state does not carry mls.ErrRemovedFromGroup: %v. The cause is a "+
			"VALUE and not state a restart can invalidate, so a caller branching on that name must "+
			"not read true before a restart and false after it", state)
	}
	if _, err := group.sendableLocked(KindText); !errors.Is(err, ErrRemovedFromGroup) {
		t.Errorf("the restored group's send door answered %v, want ErrRemovedFromGroup", err)
	}
	if err := group.committableLocked(); !errors.Is(err, ErrRemovedFromGroup) {
		t.Errorf("the restored group's commit door answered %v, want ErrRemovedFromGroup", err)
	}
	t.Logf("the removal was written as part ten (kind %d, epoch %d) and came back as a group whose "+
		"every door answers by name", removedByCommit, removedAt)
}

// A STORE WRITTEN BEFORE PART TEN STILL STARTS, AND WHAT IT LOSES IS ONE WALK RATHER THAN ONE DEVICE.
//
// WHY THIS IS THE CASE THAT DECIDES THE SHAPE OF PART TEN. A restore that REFUSED a record written by
// an older build is a device that can never start again: the deployed alpha's disk is FIVE parts, the
// build before this one wrote NINE, and none of them carries a removal. So the reader takes every
// arity and a missing part means "this disk says nothing about a removal", which is exactly the state
// every build before this one was in.
//
// THE FIXTURE IS A REAL NINE-PART RECORD, written through the store's own framing with part ten
// dropped -- not a ten-part record with an empty part, which is what a device that is still a member
// writes and would prove nothing. The control is that the tenth part this build wrote is NOT empty,
// so stripping it changes something.
//
// AND THE SECOND CLAUSE IS THE PRICE, MEASURED RATHER THAN CLAIMED: such a device comes back reading
// as a MEMBER, and its next walk over the removing commit -- which is still the first record above a
// cursor that is not persisted, in a group whose MLS state on the disk still stands at the epoch
// before the removal -- re-derives the state and files part ten. So the loss is bounded to the window
// between the restore and the first Receive.
//
// WHAT WOULD GO RED: make [groupRecordOf] refuse a nine-part record (the restore fails and the device
// never starts); invent a removal at the read from anything other than part ten (the first clause);
// drop the mls.ErrRemovedFromGroup arm at ApplyCommit (the second clause cannot re-derive).
func TestAStoreWrittenBeforeTheRemovalPartStillStartsAndTheFirstWalkFilesTheRemoval(t *testing.T) {
	ctx := context.Background()
	world := newRotWorld(t, "alice", "bob")
	alice, bob := world.member("alice"), world.member("bob")

	named := rotPolicyOf(t, alice)
	named.SetRole(bob.dev.identityPub, mls.RoleMember)
	naming := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitPolicy(rotPolicyBody(t, named))
	})
	if err := world.deliver(bob, naming.page()...); err != nil {
		t.Fatalf("bob's walk over the policy that names him: %v", err)
	}
	removedAt := bob.group.Epoch()

	capture := captureOutgoingOn(t, alice.group)
	if err := alice.group.RemoveMember(ctx, bob.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("alice's RemoveMember answered %v, want this case's authorizer refusal", err)
	}
	alice.group.device.commitAuthorizer = nil
	removal := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemoveWithExtensions(capture.decision.RemovedLeaves,
			capture.decision.ExtensionsAfter)
	})
	if err := world.deliver(bob, removal.page()...); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("bob's walk over his own removal answered %v", err)
	}

	// ── THE DISK, REWRITTEN AS A BUILD BEFORE PART TEN WROTE IT ─────────────────────────────────
	store := bob.dev.store
	path := store.groupRecordPath(bob.group.id)
	parts, err := store.readRecord(path, stateKindGroupRecord)
	if err != nil {
		t.Fatalf("reading the group record back: %v", err)
	}
	if len(parts) != 10 {
		t.Fatalf("this build wrote %d parts and this case strips the tenth; if the arity has moved, "+
			"the fixture is no longer an old store", len(parts))
	}
	if len(parts[9]) == 0 {
		t.Fatalf("CONTROL FAILED: the tenth part this build wrote is EMPTY over a group whose device " +
			"was removed, so stripping it changes nothing and this case would pass against a build " +
			"that never wrote one")
	}
	if err := store.writeRecord(path, stateKindGroupRecord, parts[:9]...); err != nil {
		t.Fatalf("rewriting the group record with nine parts: %v", err)
	}
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords over the nine-part disk: %v", err)
	}
	if len(records) != 1 || records[0].RemovedKind != removedNone || records[0].RemovedEpoch != 0 {
		t.Fatalf("the nine-part record decoded to kind %d at epoch %d; a record written before part "+
			"ten must come back as removedNone, which is a disk that says nothing rather than a "+
			"device that is certainly still a member",
			records[0].RemovedKind, records[0].RemovedEpoch)
	}

	// ── THE RESTORE: IT STARTS, AND IT READS AS A MEMBER ────────────────────────────────────────
	revived := restoredRotDevice(t, bob)
	group, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), removedAt)
	if err != nil {
		t.Fatalf("a device whose store predates part ten did not start: %v. A restore that refuses "+
			"an older record is a device that can never start again, which is the one outcome no "+
			"compatibility question may reach", err)
	}
	defer group.Close()
	if epoch, state := group.Removal(); state != nil || epoch != 0 {
		t.Fatalf("the nine-part restore came back with (%d, %v); part ten is the only place this "+
			"state is written, so a record without it must read as a member", epoch, state)
	}

	// ── AND THE FIRST WALK RE-DERIVES IT AND FILES IT, WHICH IS THE BOUND ON THE LOSS ───────────
	restoredMember := &rotMember{
		name: bob.name, root: bob.root,
		handle: group.handle, session: group.session, group: group,
		leaf: group.handle.OwnLeafIndex(),
	}
	group.ownFloorHeld, group.reconciled = true, true
	if err := world.deliver(restoredMember, removal.page()...); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("the restored device's first walk over the commit that removed it answered %v, "+
			"want ErrRemovedFromGroup: the removing commit is still the first record above a cursor "+
			"nothing persists, so an old store's loss is one walk wide", err)
	}
	if epoch, state := group.Removal(); state == nil || epoch != removedAt {
		t.Fatalf("after the first walk the restored group reads (%d, %v), want (%d, non-nil)",
			epoch, state, removedAt)
	}
	after, err := revived.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords after the walk: %v", err)
	}
	if len(after) != 1 || after[0].RemovedKind != removedByCommit || after[0].RemovedEpoch != removedAt {
		t.Errorf("after the re-derivation the disk holds kind %d at epoch %d, want %d at %d: the "+
			"walk that learned it did not write it down, so the NEXT restart loses it again",
			after[0].RemovedKind, after[0].RemovedEpoch, removedByCommit, removedAt)
	}
	t.Logf("a nine-part store started, read as a member, and its first walk over the removing "+
		"commit re-derived the state and wrote part ten (kind %d, epoch %d)", removedByCommit, removedAt)
}

// PART TEN's CODEC REFUSES EVERY SHAPE THIS BUILD DID NOT WRITE, AND THE ONE SHORT SHAPE IT ADMITS IS
// THE EMPTY ONE.
//
// IT IS [decodeWrapDark]'s DISCIPLINE ONE PART OVER, and the two are separate functions because they
// carry separate KIND SPACES -- but what this case MEASURED is that the codec cannot police that, and
// the finding is kept rather than dropped. A first draft asserted "a wrap_dark kind is refused by this
// part" with wrapDarkNoWrap as its instance, and it FAILED: wrapDarkNoWrap and removedByCommit are
// BOTH 1, so the one value a confused caller is most likely to hand the wrong encoder is the one value
// neither encoder can tell apart. The four wrap_dark kinds that do not collide (2, 3, 4, 5) ARE
// refused and that is asserted below; the collision at 1 is asserted as a FACT so the day either
// constant moves this case says so. What actually prevents the mix is structural and not checkable
// here: two parts, two encoders, two writers, and no call site that hands one field's kind to the
// other's function.
//
// AND EPOCH ZERO ROUND-TRIPS, which is why the kind octet is carried at all: this field holds the
// epoch a device was STANDING at, and a founder stands at epoch zero, so zero cannot be the "not
// removed" sentinel the way it is for [LeafOccupancy.DepartedEpoch].
//
// WHAT WOULD GO RED: accept a kind this build does not name; answer removedNone for a part of the
// wrong length; drop the kind octet and key "not removed" off a zero epoch; renumber either kind
// space so the collision at 1 stops being a collision (which is a disk format change and must be
// noticed).
func TestTheRemovalPartRefusesEveryShapeThisBuildDidNotWrite(t *testing.T) {
	if encoded, err := encodeRemoval(removedNone, 0); err != nil || encoded != nil {
		t.Errorf("encodeRemoval(removedNone) answered %v, %v; a member's part is EMPTY", encoded, err)
	}
	// AND removedNone WITH AN EPOCH IS STILL EMPTY, because the epoch is meaningless without a kind
	// and writing it would give two records of one member two shapes.
	if encoded, err := encodeRemoval(removedNone, 9); err != nil || encoded != nil {
		t.Errorf("encodeRemoval(removedNone, 9) answered %v, %v", encoded, err)
	}
	if _, err := encodeRemoval(removedUnnamed, 1); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("encodeRemoval(removedUnnamed) answered %v, want ErrStateStoreFormat: a state this "+
			"build cannot name must fail the persist rather than be written as `still a member`", err)
	}
	// THE COLLISION, STATED AS THE FACT IT IS. This is the one wrap_dark kind part ten cannot refuse,
	// because the two constants are the same octet; every other one it can.
	if wrapDarkNoWrap != removedByCommit {
		t.Errorf("wrapDarkNoWrap is %d and removedByCommit is %d: they used to be the same octet, "+
			"which is why the loop below carves the first one out. If a kind space has been "+
			"renumbered, that is a disk format change and this case is the notice",
			wrapDarkNoWrap, removedByCommit)
	}
	for _, kind := range []uint8{wrapDarkUnreadable, wrapDarkOrphan, wrapDarkRemoval, wrapDarkUnfollowable} {
		if _, err := encodeRemoval(kind, 1); !errors.Is(err, ErrStateStoreFormat) {
			t.Errorf("encodeRemoval accepted wrap_dark kind %d, a kind of the part NEXT DOOR, and "+
				"answered %v", kind, err)
		}
		if _, _, err := decodeRemoval(append([]byte{kind}, bytes.Repeat([]byte{0x00}, 8)...)); !errors.Is(err, ErrStateStoreFormat) {
			t.Errorf("decodeRemoval read wrap_dark kind %d as a removal", kind)
		}
	}
	for _, epoch := range []uint64{0, 1, 7, ^uint64(0)} {
		encoded, err := encodeRemoval(removedByCommit, epoch)
		if err != nil {
			t.Fatalf("encodeRemoval at epoch %d: %v", epoch, err)
		}
		kind, got, err := decodeRemoval(encoded)
		if err != nil || kind != removedByCommit || got != epoch {
			t.Errorf("the removal part round-trips epoch %d as (%d, %d, %v)", epoch, kind, got, err)
		}
	}
	if kind, epoch, err := decodeRemoval(nil); err != nil || kind != removedNone || epoch != 0 {
		t.Errorf("decodeRemoval(nil) answered (%d, %d, %v), want removedNone", kind, epoch, err)
	}
	for _, part := range [][]byte{
		{removedByCommit},
		{removedByCommit, 0, 0, 0, 0, 0, 0, 0},
		{removedByCommit, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		bytes.Repeat([]byte{0x00}, 9),
		append([]byte{removedUnnamed}, bytes.Repeat([]byte{0x00}, 8)...),
	} {
		if _, _, err := decodeRemoval(part); !errors.Is(err, ErrStateStoreFormat) {
			t.Errorf("decodeRemoval(% x) answered %v, want ErrStateStoreFormat: answering `still a "+
				"member` for a file that has been altered is answering the most comfortable thing",
				part, err)
		}
	}
	// AND THE PROJECTION OFF AN ERROR IS errors.Is AND NOT A STRING, held both ways.
	if got := removedKindOf(nil); got != removedNone {
		t.Errorf("removedKindOf(nil) is %d, want removedNone", got)
	}
	if got := removedKindOf(errors.New("some other refusal entirely")); got != removedUnnamed {
		t.Errorf("removedKindOf of an unrelated error is %d, want removedUnnamed: mapping it to "+
			"removedNone would persist the ABSENCE of a state that is present", got)
	}
	for _, wrapped := range []error{
		ErrRemovedFromGroup,
		errors.Join(ErrRemovedFromGroup, mls.ErrRemovedFromGroup),
		removedErrorOf(removedByCommit, 4),
	} {
		if got := removedKindOf(wrapped); got != removedByCommit {
			t.Errorf("removedKindOf(%v) is %d, want removedByCommit", wrapped, got)
		}
	}
	if removedErrorOf(removedNone, 4) != nil {
		t.Error("removedErrorOf(removedNone) answered a state")
	}
}

// readmit is the ONE repair [ErrRemovedFromGroup] names, as a door: the identity a commit removed
// is added back, in the SAME PROCESS and on the SAME DEVICE, which is the shape production has.
//
// IT REUSES THE DEVICE RATHER THAN THE NAME, AND THAT IS FORCED RATHER THAN CHOSEN. [rotWorld.admit]
// opens a second [DurableStateStore] over the joiner's state directory; a removed device is still
// holding its own, and the single-writer exclusion refuses the second opener by name. Production
// cannot do it either: [Device.Join] runs on the device that is already there, over the store that
// is already open, and it OVERWRITES that group's record -- part ten included, which is what takes
// the removal off the disk.
//
// WHAT IT MODELS OF [Device.Join] IS WHAT [rotWorld.enrollAt] MODELS: a fresh [Group] at the epoch
// the welcome admitted it at, holding that epoch's pq_secret and nothing below it, with
// [Group.ownFloorHeld] FALSE because a joiner cannot know whose leaf it landed on. The one thing
// this door does not model is the store write, which [rotWorld.enrollAt] makes for its own reasons.
func (self *rotWorld) readmit(committer *rotMember, removed *rotMember, receivers ...*rotMember) *rotMember {
	self.t.Helper()
	keyPackage, err := removed.dev.engine.NewKeyPackage()
	if err != nil {
		self.t.Fatalf("%s's key package for the re-add: %v", removed.name, err)
	}
	var welcome, ratchetTree []byte
	published := self.rotate(committer, func() ([]byte, []byte, []byte, error) {
		commit, admission, tree, err := committer.handle.CommitAdd([][]byte{keyPackage})
		welcome, ratchetTree = admission, tree
		return commit, admission, tree, err
	})
	for _, receiver := range receivers {
		if err := self.deliver(receiver, published.page()...); err != nil {
			self.t.Fatalf("%s's walk over the commit that adds %s back: %v", receiver.name,
				removed.name, err)
		}
	}
	handle, err := removed.dev.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		self.t.Fatalf("%s's JoinFromWelcome on the way back in: %v", removed.name, err)
	}
	self.t.Cleanup(func() { handle.Close() })
	return self.enrollAt(removed.name, removed.dev, handle, published.pqSecret)
}

// THE ONE REPAIR THE REMOVED SENTINEL NAMES, DRIVEN -- AND WHAT IT COSTS, MEASURED.
//
// WHY THIS CASE EXISTS. [ErrRemovedFromGroup]'s own sentence ends "until it is added back", and
// ledger item 259 filed that nothing ran it: a sentinel that names exactly one repair and never
// exercises it is a promise rather than a road, and what the repair COSTS was a sentence nobody had
// measured. Both halves are here.
//
// WHAT THE REPAIR BUYS, AND IT IS THE WHOLE OF THE GOOD NEWS: the state is GONE. The re-added
// device's group answers (0, nil) from [Group.Removal] -- a fresh [Group] at the epoch the welcome
// admitted it at, with no removal on it -- and the record on the disk agrees, because
// [Device.Join]'s own persist rewrites part ten with the new group's `removedNone`.
//
// AND WHAT IT COSTS, THREE THINGS, EACH MEASURED HERE RATHER THAN REASONED:
//
//  1. BOTH DOORS ARE STILL SHUT, and they answer [ErrStreamFloorUnheld] rather than the removal. A
//     re-add lands on the leftmost BLANK leaf (RFC 9420 §7.7), so the server may already hold stream
//     claims under the very handle this device is about to seal at -- including its OWN earlier ones
//     -- and [Group.seedOwnStreamLocked] has to look before [Group.Send] may seal. One clean walk is
//     the price, and it is [Device.Join]'s standing price for every joiner rather than a removal's.
//  2. ITS OWN PRE-REMOVAL LINES COST [maxRecordAttempts] FAILED WALKS EACH AND ARE THEN LOST. A
//     re-added device holds state from its admission ON -- [Device.Join] files one pq_secret row and
//     mls keeps no schedule below the epoch the welcome names -- so a record it wrote ITSELF before
//     the removal does not authenticate. The walk cannot tell that from a transient: it spends
//     [maxRecordAttempts] attempts, answers [ErrRecordAbandoned], counts [Stats.Unopened], puts the
//     record in [Group.UnopenedRecords] and resolves the cursor PAST it. Nothing later repairs it.
//  3. AND THE HISTORY IS NOT RECOVERED BY THE RE-ADD, which is the same fact from the user's side:
//     what the removed device could still read while it held the keys of the epoch it was removed at
//     is exactly what the re-added device can no longer read at all.
//
// THE TWO ROWS ARE THE LEAF, which every reading of this before it was driven left as a silent
// premise. §7.7 refills the leftmost blank leaf, so a device removed and added back with nobody else
// joining in between lands on its OWN old leaf and derives its OWN old sender_handle; a newcomer
// admitted first takes that leaf and pushes the re-add one along. Both rows are built and the cost
// is the SAME in both -- which is the finding, because it says the loss is the EPOCH's and not the
// handle's, and no amount of [Group.ownHandles] bookkeeping can buy any of it back.
//
// THE CONTROLS ARE INLINE AND EACH FIRES FOR ITS OWN REASON. The survivor OPENS the very record the
// re-added device cannot, over the same octets, which is what makes "did not authenticate" a fact
// about this device rather than about the record. And the re-added device's [Group.Removal] is read
// BEFORE the failing walks, so the two are not one assertion.
//
// WHAT WOULD GO RED: have [Device.Join] carry the removal forward (the repair repairs nothing);
// seed a re-added group with the epochs below its admission (the cost paragraph is wrong and the
// record opens); raise [Group.ownFloorHeld] for a joiner above epoch one (door 1 opens and the
// server's claims are met with a seal instead of a look).
func TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory(t *testing.T) {
	for _, one := range []struct {
		name      string
		newcomer  bool
		sameLeaf  bool
		receivers []string
	}{
		{name: "added straight back, onto the blank leaf that is its own old one", sameLeaf: true},
		{name: "a newcomer took the blank leaf first, so the re-add lands one along", newcomer: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			ctx := context.Background()
			world := newRotWorld(t, "alice", "bob", "carol")
			alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

			// NAME EVERY MEMBER, so the verb has an entry to drop and its commit is not an R0c
			// phantom every receiver refuses before it can remove anybody.
			named := rotPolicyOf(t, alice)
			named.SetRole(bob.dev.identityPub, mls.RoleAdmin)
			named.SetRole(carol.dev.identityPub, mls.RoleMember)
			naming := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
				return alice.handle.CommitPolicy(rotPolicyBody(t, named))
			})
			for _, who := range []*rotMember{bob, carol} {
				if err := world.deliver(who, naming.page()...); err != nil {
					t.Fatalf("%s's walk over the policy that names everybody: %v", who.name, err)
				}
			}

			// carol's own line, written while it was a member: the thing the repair does not give
			// back, and the control's subject.
			mine := world.sealDurable(carol, "carol, before the removal")
			opened := bob.group.Stats().Opened
			if err := world.deliver(bob, mine); err != nil {
				t.Fatalf("CONTROL FAILED: the survivor's walk over carol's line answered %v", err)
			}
			if got := bob.group.Stats().Opened; got != opened+1 {
				t.Fatalf("CONTROL FAILED: the survivor's Stats.Opened went %d -> %d over carol's "+
					"line; the record has to OPEN for somebody, or the re-added device failing to "+
					"open it below is a fact about the record and not about the re-add",
					opened, got)
			}
			leafBefore := carol.leaf

			// ── THE REMOVAL ─────────────────────────────────────────────────────────────────────
			capture := captureOutgoingOn(t, alice.group)
			if err := alice.group.RemoveMember(ctx, carol.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
				t.Fatalf("alice's RemoveMember answered %v, want this case's authorizer refusal", err)
			}
			alice.group.device.commitAuthorizer = nil
			removal := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
				return alice.handle.CommitRemoveWithExtensions(capture.decision.RemovedLeaves,
					capture.decision.ExtensionsAfter)
			})
			if err := world.deliver(bob, removal.page()...); err != nil {
				t.Fatalf("CONTROL FAILED: the survivor's walk over the removal answered %v", err)
			}
			if err := world.deliver(carol, removal.page()...); !errors.Is(err, ErrRemovedFromGroup) {
				t.Fatalf("carol's walk over her own removal answered %v, want ErrRemovedFromGroup", err)
			}
			if _, state := carol.group.Removal(); state == nil {
				t.Fatalf("CONTROL FAILED: carol reads no removal after being removed, so the repair " +
					"below repairs nothing")
			}
			removed := carol

			// ── THE REPAIR ──────────────────────────────────────────────────────────────────────
			if one.newcomer {
				world.admit(alice, "dave", bob)
			}
			again := world.readmit(alice, removed, bob)
			if one.sameLeaf && again.leaf != leafBefore {
				t.Fatalf("the re-add landed at leaf %d and this row is the one where §7.7 refills "+
					"its own old leaf %d", again.leaf, leafBefore)
			}
			if !one.sameLeaf && again.leaf == leafBefore {
				t.Fatalf("the re-add landed back at leaf %d although a newcomer was admitted first; "+
					"this row exists to move the handle and it did not", again.leaf)
			}
			if epoch, state := again.group.Removal(); state != nil || epoch != 0 {
				t.Fatalf("the re-added group reads (%d, %v) from Removal: the ONE repair the "+
					"sentinel names does not clear the state it names it for", epoch, state)
			}

			// ── COST 1: BOTH DOORS, AND THEY NAME THE JOINER'S PRICE AND NOT THE REMOVAL ────────
			//
			// THE HARNESS'S `false` IS TIED TO PRODUCTION'S RULE HERE RATHER THAN LEFT PARALLEL TO
			// IT: [Device.Join] raises [Group.ownFloorHeld] only for `handle.Epoch() <= 1`, whose
			// argument is that epoch one's leaf is fresh by construction. A re-add is necessarily
			// above that -- it takes a commit to remove and another to add back -- so the door below
			// is the one production would answer too, and this reading says so instead of assuming.
			if again.group.Epoch() <= 1 {
				t.Fatalf("the re-add landed at epoch %d; Device.Join raises ownFloorHeld at or below "+
					"epoch one, so the refusals below would not be the ones production answers",
					again.group.Epoch())
			}
			for _, door := range []struct {
				what string
				err  error
			}{
				{"the send door", func() error { _, err := again.group.sendableLocked(KindText); return err }()},
				{"the commit door", again.group.committableLocked()},
			} {
				if !errors.Is(door.err, ErrStreamFloorUnheld) {
					t.Errorf("%s of the re-added group answered %v, want ErrStreamFloorUnheld: a "+
						"re-add lands on the leftmost BLANK leaf and the server may already hold "+
						"claims under the handle it derives there, so one clean walk is owed before "+
						"a seal", door.what, door.err)
				}
				if errors.Is(door.err, ErrRemovedFromGroup) {
					t.Errorf("%s of the re-added group still answers the removal: %v", door.what, door.err)
				}
			}

			// ── COST 2: ITS OWN PRE-REMOVAL LINE, WALK BY WALK ──────────────────────────────────
			for walk := 1; walk <= maxRecordAttempts; walk += 1 {
				err := world.deliver(again, mine)
				if walk < maxRecordAttempts {
					if !errors.Is(err, ErrRecordOpen) || errors.Is(err, ErrRecordAbandoned) {
						t.Fatalf("walk %d over its own pre-removal line answered %v, want "+
							"ErrRecordOpen: a re-added device holds state from its admission on and "+
							"cannot authenticate a record it wrote itself below that", walk, err)
					}
					continue
				}
				if !errors.Is(err, ErrRecordAbandoned) {
					t.Fatalf("walk %d over its own pre-removal line answered %v, want "+
						"ErrRecordAbandoned after maxRecordAttempts attempts", walk, err)
				}
			}
			stats := again.group.Stats()
			if stats.FailedOpen != uint64(maxRecordAttempts) || stats.Unopened != 1 {
				t.Errorf("the re-added device spent FailedOpen %d and Unopened %d over one line of "+
					"its own, want %d and 1", stats.FailedOpen, stats.Unopened, maxRecordAttempts)
			}
			if stats.Opened != 0 || stats.OpenedOwn != 0 {
				t.Errorf("the re-added device read %d record(s) and %d of its own over a page it "+
					"holds no keys for", stats.Opened, stats.OpenedOwn)
			}
			if unopened := again.group.UnopenedRecords(); len(unopened) != 1 || unopened[0] != mine.recordId {
				t.Errorf("the re-added device's unopened records are %v, want [%d]",
					unopened, mine.recordId)
			}
			if again.group.cursor < mine.recordId {
				t.Errorf("the cursor stands at %d, below the abandoned record %d: the walk keeps "+
					"coming back to a line it can never open", again.group.cursor, mine.recordId)
			}
			// AND IT IS PERMANENT: the next walk over the same page is silent, which is what makes
			// this a loss rather than a delay.
			if err := world.deliver(again, mine); err != nil {
				t.Errorf("the walk after the abandonment answered %v, want nil", err)
			}
			t.Logf("leaf %d -> %d, re-added at epoch %d: the removal state is gone, both doors "+
				"answer ErrStreamFloorUnheld, and one line of its own cost %d failed walks and is "+
				"in UnopenedRecords for ever", leafBefore, again.leaf, again.group.Epoch(),
				maxRecordAttempts)
		})
	}
}

// THE VICTIM OF A DIGEST-LESS REMOVAL IS HALTED AND IS NOT REMOVED, AND THAT IS THE DECISION.
//
// WHAT WAS FILED. Ledger item 259's adversary pass found that the device a DIGEST-LESS removal names
// answers `removed: false` through cgo's urnet_message_group_removal, so neither of Spec C's two
// screen states is reachable for it: not "you are no longer in this group", because [Group.removed]
// is nil, and not the halt, because this boundary carries no projection of [Group.halted] at all.
//
// AND THE DECISION IS THAT `removed: false` IS CORRECT, WHICH IS RULING 41 READ LITERALLY. A removal
// carrying no epoch digest is an INVALID commit -- there is no authenticator for anything it
// delivers -- and [Group.ingestCommitLocked]'s step (3a) refuses it BEFORE ApplyCommit, for the
// victim exactly as for every survivor. A commit this device refused removed it from nothing: its
// leaf is still in the tree it is standing in, it is still a member at epoch n, and a boundary that
// said "you were removed" would be reporting a membership change that this device did not accept and
// that its own MLS state does not carry. The state it IS in is the halt, and the halt is ruling 41's
// own track: giving it a projection is an ABI addition with a header key, a ctest reader and a
// ruling behind it, and ledger ruling 50's precedent is that a second track does not ride in on
// this one. What X4 owes is that the answer is not an accident, and this case is that.
//
// THE THREE READINGS, all over one page: the victim answers [ErrRemovalWithoutRotation] and NOT
// [ErrRemovedFromGroup]; [Group.Removal] -- which is exactly what the cgo projection reads --
// answers (0, nil), so `removed: false` is what the boundary would carry; and it stands at the epoch
// it refused from with its own MLS handle unmoved, which is the halt and not a removal that half
// happened.
//
// THE CONTROL IS THE SAME COMMIT MADE PROPERLY, in the same case and against the same victim: a
// removal that DOES carry its digest, walked by the same device in a fresh world, answers
// [ErrRemovedFromGroup] and reads a removal off [Group.Removal]. Without it every clause above is
// satisfied by a build that never sets the removed state at all.
//
// WHAT WOULD GO RED: move step (3a)'s refusal after ApplyCommit (the victim is then removed by a
// commit this build judged invalid, which is the outcome ruling 41 took away); set [Group.removed]
// on the halt (the two states stop being two).
func TestTheVictimOfADigestLessRemovalIsHaltedAndReadsNoRemoval(t *testing.T) {
	ctx := context.Background()
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, carol := world.member("alice"), world.member("carol")
	refusedAt := carol.group.Epoch()

	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{noDigest: true})
	if digest, err := epochDigestOf(&published.commit.record.Header); err != nil || digest != nil {
		t.Fatalf("CONTROL FAILED: this commit carries a digest (%v, %v), so it is not the shape "+
			"this case is named for", digest, err)
	}

	err := world.deliver(carol, published.page()...)
	if !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the VICTIM's walk over a digest-less removal of its own leaf answered %v, want "+
			"ErrRemovalWithoutRotation", err)
	}
	if errors.Is(err, ErrRemovedFromGroup) {
		t.Errorf("the victim's walk answers the removal as well: %v. A commit this device REFUSED "+
			"removed it from nothing, and the two states are two", err)
	}
	if epoch, state := carol.group.Removal(); state != nil || epoch != 0 {
		t.Errorf("the victim reads (%d, %v) from Removal, which is what cgo's "+
			"urnet_message_group_removal projects; `removed` must be false for a commit this "+
			"device did not follow", epoch, state)
	}
	if carol.group.halted == nil {
		t.Errorf("the victim is not halted either, so it is in neither state and the refusal left " +
			"nothing behind")
	}
	if got := carol.group.Epoch(); got != refusedAt {
		t.Errorf("the victim's group stands at epoch %d, want %d", got, refusedAt)
	}
	if got := carol.handle.Epoch(); got != refusedAt {
		t.Errorf("the victim's MLS handle stands at epoch %d, want %d: the commit was applied and "+
			"the refusal was taken after it", got, refusedAt)
	}

	// ── THE CONTROL: THE SAME REMOVAL MADE PROPERLY IS A REMOVAL ────────────────────────────────
	honest := newRotWorld(t, "alice", "bob", "carol")
	honestAlice, honestBob, honestCarol := honest.member("alice"), honest.member("bob"), honest.member("carol")
	named := rotPolicyOf(t, honestAlice)
	named.SetRole(honestBob.dev.identityPub, mls.RoleAdmin)
	named.SetRole(honestCarol.dev.identityPub, mls.RoleMember)
	naming := honest.rotate(honestAlice, func() ([]byte, []byte, []byte, error) {
		return honestAlice.handle.CommitPolicy(rotPolicyBody(t, named))
	})
	for _, who := range []*rotMember{honestBob, honestCarol} {
		if err := honest.deliver(who, naming.page()...); err != nil {
			t.Fatalf("CONTROL FAILED: %s's walk over the naming: %v", who.name, err)
		}
	}
	capture := captureOutgoingOn(t, honestAlice.group)
	if err := honestAlice.group.RemoveMember(ctx, honestCarol.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("CONTROL FAILED: RemoveMember answered %v", err)
	}
	honestAlice.group.device.commitAuthorizer = nil
	removal := honest.rotate(honestAlice, func() ([]byte, []byte, []byte, error) {
		return honestAlice.handle.CommitRemoveWithExtensions(capture.decision.RemovedLeaves,
			capture.decision.ExtensionsAfter)
	})
	if err := honest.deliver(honestCarol, removal.page()...); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("CONTROL FAILED: the victim of a ROTATED removal answered %v, want "+
			"ErrRemovedFromGroup; without this the readings above are satisfied by a build that "+
			"never sets the removed state", err)
	}
	if _, state := honestCarol.group.Removal(); state == nil {
		t.Fatalf("CONTROL FAILED: the victim of a rotated removal reads no removal")
	}
	t.Logf("the victim of a digest-less removal is halted at epoch %d with Removal() (0, nil) -- "+
		"`removed: false` at the cgo boundary, deliberately -- while the victim of the same removal "+
		"made properly reads the removal", refusedAt)
}
