package cp3b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/sdk/urmessage"
)

// [urmessage.Group.RemoveMember] OVER A RUNNING SERVER (ledger item 258, ruling 49). The feature
// every gate in the removal track exists to protect, asked of the whole seam: four real devices, a
// fifth leaf that is one of them a second time, and a real message server that takes what it is
// handed.
//
// WHAT THIS FILE HAS THAT urmessage's OWN SUITE CANNOT. No test in that package can run a publishing
// verb to completion -- the submit goes through `*sdk.MessageTransport`, a concrete type with no stub
// -- so the derivation, the refusals and the fan-out are held there and the SUBMIT is held here:
// a removal that is accepted, an epoch the server moves to, survivors that converge through real
// fetches, and a removed member that fetches successfully and cannot follow. The server's own
// `current_epoch` is read off its store, which is the only place "nothing was published" is a fact
// rather than a claim.

// TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow is the verb end to end.
//
// THE SHAPE, and every epoch is a real commit through a real submit: alice founds with bob and
// promotes him ADMIN; bob adds carol and dave; CAROL ADDS HER OWN SECOND DEVICE, so one identity
// holds two leaves -- the case a per-leaf removal would get wrong and a per-identity one cannot;
// alice promotes carol ADMIN; then alice, the OWNER, removes carol in ONE call.
//
// THE REFUSALS ARE TAKEN WHILE CAROL IS STILL A MEMBER, and the assertion that matters is the
// SERVER'S: its `current_epoch` does not move and nobody ingests anything, so the refusal happened
// before the connection was consulted. R2 (a member may not remove another member), R3 (only the
// owner may remove an admin) and the two by-name doors are each driven from the verb.
//
// THE REMOVAL'S OWN PROPERTIES, and "it returned nil" is none of them: both of carol's leaves are
// gone at every survivor; carol's identity is gone from the roster, which is the policy entry too
// (a removal that left it is an R0c phantom every receiver refuses); the three survivors converge
// at the new epoch and every pair opens the other's line, which a shared storage root is the only
// way to do; and carol's own device fetches successfully -- it holds the keys of the epoch it was
// removed AT -- and cannot apply the commit, stays where it was, and can no longer write.
//
// WHAT WOULD GO RED: remove one of carol's two leaves and the survivors' rosters still carry her
// identity; skip the send-side decision and carol's refused verbs move the SERVER's epoch; forget
// the policy in the removal commit and every survivor refuses it on receipt, so the convergence
// block fails; leave the removed leaves in the fan-out and carol follows the epoch.
func TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice, _, _ := world.device(t, "alice")
	bob, _, _ := world.device(t, "bob")
	carol, _, _ := world.device(t, "carol")
	dave, _, _ := world.device(t, "dave")
	for _, who := range []*urmessage.Device{alice, bob, carol, dave} {
		if err := who.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}
	// ── epoch 1: alice founds with bob ──────────────────────────────────────────────────────────
	groupId := newGroupId(t)
	aliceGroup, err := alice.CreateGroup(ctx, groupId)
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	bobKeyPackage, err := bob.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMember(bobKeyPackage)
	if err != nil {
		t.Fatalf("alice's AddMember: %v", err)
	}
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	bobGroup, err := bob.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	groups := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup}
	aliceId, bobId := rolesIdentityOf(t, aliceGroup), rolesIdentityOf(t, bobGroup)

	// ── epoch 2: alice promotes bob, so an ADMIN exists for R3 to be about ──────────────────────
	if err := aliceGroup.SetRole(ctx, bobId, "admin"); err != nil {
		t.Fatalf("alice's SetRole promoting bob: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 2, groups)

	// ── epoch 3 and 4: bob, an admin, adds carol and dave ──────────────────────────────────────
	carolGroup := removalAdd(t, ctx, "bob", bobGroup, carol, "carol")
	groups["carol"] = carolGroup
	rolesReceiveAll(t, ctx, groups)
	daveGroup := removalAdd(t, ctx, "bob", bobGroup, dave, "dave")
	groups["dave"] = daveGroup
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 4, groups)
	carolId, daveId := rolesIdentityOf(t, carolGroup), rolesIdentityOf(t, daveGroup)

	// ── epoch 5: CAROL ADDS HER OWN SECOND DEVICE ───────────────────────────────────────────────
	//
	// A MEMBER may add its own device leaves and nothing else (MASTER section 11, R7), and R6a
	// requires an Add claiming an identity already in the group to be committed by that identity --
	// so this is carol's commit and nobody else's. The second leaf is a seam member because an
	// urmessage.Device mints one identity per state store; what it is for is that carol's identity
	// now holds TWO leaves.
	laptop := world.seamMemberClaiming(t, ctx, "carol-laptop", carolId)
	invite, err = carolGroup.AddMemberAndPublish(ctx, laptop.keyPackage(t))
	if err != nil {
		t.Fatalf("carol's AddMemberAndPublish of her own second device: %v", err)
	}
	laptop.join(t, gcReencodeInvite(t, invite))
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 5, groups)

	// THE CONTROL THE WHOLE CASE RESTS ON: carol's identity holds two leaves, at every reader.
	for name, group := range groups {
		if got := removalLeavesOf(t, group, carolId); len(got) != 2 {
			t.Fatalf("CONTROL FAILED: %s reads carol's identity at %d leaf/leaves (%v), and this "+
				"case is about ONE call that takes EVERY leaf of an identity", name, len(got), got)
		}
		if got := len(removalRoster(t, group)); got != 5 {
			t.Fatalf("CONTROL FAILED: %s reads %d members, want 5 (alice, bob, carol, carol's "+
				"laptop, dave)", name, got)
		}
	}

	// ── THE REFUSALS, WHILE CAROL IS STILL A MEMBER, MEASURED AT THE SERVER ─────────────────────
	epochAtServer := func() uint64 {
		state, err := world.store.GroupState(ctx, groupId)
		if err != nil {
			t.Fatalf("the server's group state: %v", err)
		}
		return state.CurrentEpoch
	}
	if got := epochAtServer(); got != 5 {
		t.Fatalf("the server is at epoch %d before the refused verbs, want 5", got)
	}
	ingestedBefore := map[string]uint64{}
	for name, group := range groups {
		ingestedBefore[name] = group.Stats().Ingested
	}
	refusedBefore := carolGroup.Stats().CommitRefusedOwn

	// R2: a MEMBER may not remove another member
	if err := carolGroup.RemoveMember(ctx, daveId); !errors.Is(err, urmessage.ErrCommitUnauthorized) ||
		!errors.Is(err, urmessage.ErrCommitRemoveByNonAdmin) {
		t.Errorf("carol, a member, removing dave answered %v; want ErrCommitUnauthorized wrapping "+
			"ErrCommitRemoveByNonAdmin (R2)", err)
	}
	// R3: only the OWNER may remove an ADMIN
	if err := carolGroup.RemoveMember(ctx, bobId); !errors.Is(err, urmessage.ErrCommitUnauthorized) ||
		!errors.Is(err, mls.ErrAdminRemovedByNonOwner) {
		t.Errorf("carol removing bob, an admin, answered %v; want ErrCommitUnauthorized wrapping "+
			"mls.ErrAdminRemovedByNonOwner (R3)", err)
	}
	if got := carolGroup.Stats().CommitRefusedOwn; got != refusedBefore+2 {
		t.Errorf("carol's Stats.CommitRefusedOwn is %d after two rule refusals, want %d",
			got, refusedBefore+2)
	}
	// AND THE TWO BY-NAME DOORS, which are a caller bug and not a role answer: neither is counted
	if err := carolGroup.RemoveMember(ctx, carolId); !errors.Is(err, urmessage.ErrRemoveSelf) {
		t.Errorf("carol removing her own identity answered %v, want ErrRemoveSelf", err)
	}
	if err := bobGroup.RemoveMember(ctx, aliceId); !errors.Is(err, urmessage.ErrRemoveOwner) {
		t.Errorf("bob, an admin, removing the owner answered %v, want ErrRemoveOwner", err)
	}
	if err := aliceGroup.RemoveMember(ctx, bytes.Repeat([]byte{0x11}, len(aliceId))); !errors.Is(err, urmessage.ErrNoSuchMember) {
		t.Errorf("alice removing an identity that holds no leaf answered %v, want ErrNoSuchMember", err)
	}
	if got := carolGroup.Stats().CommitRefusedOwn; got != refusedBefore+2 {
		t.Errorf("a by-name refusal was counted as a role refusal: carol's CommitRefusedOwn is %d, want %d",
			got, refusedBefore+2)
	}
	// NOTHING REACHED THE WIRE: the SERVER's epoch, which is the only place this is a fact
	if got := epochAtServer(); got != 5 {
		t.Fatalf("the server is at epoch %d after five refused removals, want 5: something was published", got)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 5, groups)
	for name, group := range groups {
		if got := group.Stats().Ingested; got != ingestedBefore[name] {
			t.Errorf("%s ingested a commit after the refused removals (%d -> %d)", name, ingestedBefore[name], got)
		}
		if got := group.Stats().CommitRefused; got != 0 {
			t.Errorf("%s counted %d receiving-side refusal(s); nothing should have reached the wire", name, got)
		}
	}

	// ── epoch 6: alice promotes carol, so the removal below is the OWNER removing an ADMIN ──────
	if err := aliceGroup.SetRole(ctx, carolId, "admin"); err != nil {
		t.Fatalf("alice's SetRole promoting carol: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 6, groups)
	if role, err := carolGroup.MyRole(); err != nil || role != "admin" {
		t.Fatalf("carol reads her own role as %q, %v; want admin", role, err)
	}

	// ── epoch 7: THE OWNER REMOVES THE ADMIN, IN ONE CALL ───────────────────────────────────────
	carolLeavesBefore := removalLeavesOf(t, aliceGroup, carolId)
	if err := aliceGroup.RemoveMember(ctx, carolId); err != nil {
		t.Fatalf("alice's RemoveMember of carol, as the owner removing an admin: %v", err)
	}
	if got := aliceGroup.Epoch(); got != 7 {
		t.Fatalf("alice is at epoch %d after ONE removal commit, want 7", got)
	}
	if got := epochAtServer(); got != 7 {
		t.Fatalf("the server is at epoch %d after the removal, want 7", got)
	}

	// ── EVERY SURVIVOR FOLLOWS, AND BOTH LEAVES ARE GONE ───────────────────────────────────────
	survivors := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup, "dave": daveGroup}
	rolesReceiveAll(t, ctx, survivors)
	rolesAssertEpoch(t, 7, survivors)
	for name, group := range survivors {
		roster := removalRoster(t, group)
		if len(roster) != 3 {
			t.Errorf("%s reads %d members after one call removed one identity's two leaves out of "+
				"five, want 3: %v", name, len(roster), roster)
		}
		if got := removalLeavesOf(t, group, carolId); len(got) != 0 {
			t.Errorf("%s still reads carol's identity at leaves %v after the removal; she held %v "+
				"and a removal that leaves one of somebody's devices in the group has removed nobody",
				name, got, carolLeavesBefore)
		}
		// the positive control in the same loop: the survivors are all still there, with their roles
		for who, identity := range map[string][]byte{"alice": aliceId, "bob": bobId, "dave": daveId} {
			if got := removalLeavesOf(t, group, identity); len(got) != 1 {
				t.Errorf("CONTROL FAILED: %s reads %s at %d leaf/leaves after the removal, want 1: "+
					"the absence above is satisfied by a roster that lost everybody", name, who, len(got))
			}
		}
	}
	// AND THE POLICY ENTRY WENT WITH THE LEAVES, which is what the roster's roles say: a commit that
	// left carol named would have been refused by every survivor as an R0c phantom, so reaching
	// epoch 7 at all is the first half; the second is that the survivors' own roles are intact.
	rolesAssertRoster(t, 7, survivors,
		map[string][]byte{"alice": aliceId, "bob": bobId, "dave": daveId},
		map[string]string{"alice": "owner", "bob": "admin", "dave": "member"})
	// AND THEY CONVERGE: every pair opens the other's line at the new epoch, which a shared
	// storage_root is the only way to do.
	rolesAssertMesh(t, ctx, 7, survivors,
		map[string]string{"alice": "owner", "bob": "admin", "dave": "member"})

	// ── AND THE REMOVED MEMBER CANNOT FOLLOW THE EPOCH ITS OWN REMOVAL OPENED ───────────────────
	//
	// Its FETCH still works, which is the honest half: it holds the keys of the epoch it was
	// removed at, and item 246's ceiling serves it the rows at and below that epoch. What it cannot
	// do is apply the commit -- mls answers ErrRemovedFromGroup -- so it stays at 6 and can no
	// longer write. A carrier that would let the product say "you were removed" in so many words is
	// ledger item 257's ruling 52 and is NOT in this step.
	if _, err := carolGroup.Receive(ctx); err == nil {
		t.Errorf("the removed member's Receive came back clean and it is now at epoch %d", carolGroup.Epoch())
	} else if !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Errorf("the removed member's Receive answered %v, want it to carry mls.ErrRemovedFromGroup", err)
	}
	if got := carolGroup.Epoch(); got != 6 {
		t.Errorf("the removed member is at epoch %d, want 6: it followed its own removal", got)
	}
	if _, err := carolGroup.Send(ctx, "a line from somebody who is not in this group any more"); err == nil {
		t.Errorf("the removed member's Send was accepted")
	}
	t.Logf("removal: one call took carol's %d leaves and her policy entry out of the group; the "+
		"three survivors converge at epoch 7 with a mesh, and the removed member fetches, cannot "+
		"apply and cannot write", len(carolLeavesBefore))
}

// removalAdd has an admin add one device and answers the joiner's group.
func removalAdd(t *testing.T, ctx context.Context, by string, committer *urmessage.Group,
	joiner *urmessage.Device, name string) *urmessage.Group {

	t.Helper()
	keyPackage, err := joiner.KeyPackage()
	if err != nil {
		t.Fatalf("%s's KeyPackage: %v", name, err)
	}
	invite, err := committer.AddMemberAndPublish(ctx, keyPackage)
	if err != nil {
		t.Fatalf("%s's AddMemberAndPublish of %s: %v", by, name, err)
	}
	group, err := joiner.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("%s's Join: %v", name, err)
	}
	return group
}

// removalRoster is a group's roster, failing the test rather than answering an error.
func removalRoster(t *testing.T, group *urmessage.Group) []urmessage.Member {
	t.Helper()
	members, err := group.Members()
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	return members
}

// removalLeavesOf is every leaf one identity holds in a group's roster, in leaf order.
func removalLeavesOf(t *testing.T, group *urmessage.Group, identity []byte) []uint32 {
	t.Helper()
	leaves := []uint32{}
	for _, member := range removalRoster(t, group) {
		if bytes.Equal(member.IdentityPub, identity) {
			leaves = append(leaves, member.LeafIndex)
		}
	}
	return leaves
}

// THE ADMIN THAT REMOVED SOMEBODY STILL READS THEIR HISTORY AFTER A RESTART.
//
// WHY THIS CASE EXISTS, AND IT IS A DEFECT THE REMOVAL VERB WOULD HAVE SHIPPED WITH.
// urmessage's ingest arm has filed [urmessage.Group]'s departed-leaf table and pruned its
// receiver-ladder heads since ledger item 245, on the arm that follows SOMEBODY ELSE'S commit. The
// publishing arm had nothing to file, because until RemoveMember no verb could put a leaf in a
// staged commit's removed set -- and the admin who removes somebody is the ONE device in the group
// that does not learn the removal from an ingest.
//
// WHAT THAT COSTS, and the cursor is why it is not academic: the receive cursor is not persisted, so
// a restarted device re-walks its whole history. Without the table the removed member's records --
// which sit BELOW the removing commit and are its whole half of the conversation -- resolve to no
// leaf at all and take the failure road; three walks later each is abandoned as a `malformed` gap.
// The removing admin would be the only member of the group to lose them.
//
// MEASURED RATHER THAN REASONED, by deleting the two lines: the FIRST Receive after the restart does
// not come back quietly, it answers `urmessage: a record from a member of this group did not open:
// record 5 names sender_handle <16 octets>, which is no leaf of this group at epoch 1`. So the first
// symptom is a loud refusal of the whole page and the malformed gap is the second, three walks later.
//
// THE DISCRIMINATOR IS THEREFORE BOTH: the Receive is clean AND the removed member's lines come back
// OPENED with their text, with `Stats.GapMalformed` zero.
//
// THE CONTROL IS THE SAME LINES BEFORE THE RESTART: alice opens both of bob's lines while he is
// still a member, so the case cannot pass by asserting something about records that never arrived.
//
// WHAT WOULD GO RED: delete either of the two lines the publish path runs after its merge.
func TestTheAdminThatRemovedSomebodyStillReadsTheirHistoryAfterARestart(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	// bob's own two lines, sent while he is a member and opened by alice: the control
	lines := []string{bobsFirstLine, bobsSecondLine}
	for _, text := range lines {
		if _, err := bobGroup.Send(ctx, text); err != nil {
			t.Fatalf("bob's Send %q: %v", text, err)
		}
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive before the removal: %v", err)
	}
	beforeRemoval := gcTextsPresent(aliceGroup.Messages())
	for _, text := range lines {
		if !beforeRemoval[text] {
			t.Fatalf("CONTROL FAILED: alice did not open %q while bob was still a member, so this "+
				"case would be about records that never arrived: %s", text, textsOf(aliceGroup.Messages()))
		}
	}
	bobId := rolesIdentityOf(t, bobGroup)

	// alice, the owner, removes bob. Her own publish path is the only thing that can file what
	// this commit took out of the group.
	if err := aliceGroup.RemoveMember(ctx, bobId); err != nil {
		t.Fatalf("alice's RemoveMember of bob: %v", err)
	}
	if got := aliceGroup.Epoch(); got != 2 {
		t.Fatalf("alice is at epoch %d after the removal, want 2", got)
	}

	// ── THE RESTART: everything in memory dropped, two directories reopened ─────────────────────
	alice = world.restart(t, alice)
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted alice's Connect: %v", err)
	}
	restored, err := alice.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the restarted alice restored %d group(s), want 1", len(restored))
	}
	aliceGroup = restored[0]
	if got := aliceGroup.Epoch(); got != 2 {
		t.Fatalf("the restored group is at epoch %d, want 2", got)
	}

	// ── AND THE WHOLE HISTORY COMES BACK, THE REMOVED MEMBER'S HALF INCLUDED ────────────────────
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted alice's Receive: %v", err)
	}
	present := gcTextsPresent(aliceGroup.Messages())
	for _, text := range lines {
		if !present[text] {
			t.Errorf("after removing bob and restarting, alice's log does not hold %q as an opened "+
				"line. His records sit BELOW the removing commit and the cursor is not persisted, so "+
				"a device whose publish path did not file what its own commit removed cannot resolve "+
				"the sender_handle they carry and abandons every one of them: %s",
				text, textsOf(aliceGroup.Messages()))
		}
	}
	if got := aliceGroup.Stats().GapMalformed; got != 0 {
		t.Errorf("the restarted alice answered %d malformed gap(s) over the removed member's own "+
			"records, want 0: that is the abandonment ledger item 245's third piece is about", got)
	}
	if got := aliceGroup.Stats().Unopened; got != 0 {
		t.Errorf("the restarted alice holds %d unopened record(s) after the re-walk, want 0", got)
	}
	t.Logf("the removing admin restarted, re-walked its whole history and still opens both of the " +
		"removed member's lines, with no malformed gap")
}

// ── LEDGER RULING 52: WHAT THE REMOVED DEVICE IS LEFT WITH, OVER A REAL SERVER AND A REAL RESTART ──

// A REMOVED DEVICE IS TOLD BY NAME ON EVERY WALK AND ON EVERY SEND, AND STILL IS AFTER A RESTART.
//
// WHAT WAS THERE BEFORE THIS, MEASURED THROUGH THIS HARNESS AT sdk ca89760 AND NOT REASONED. The
// removed device's walks answered, in order: the mls sentence wrapped in the generic
// ErrCommitIngest; `the group is closed and its epoch secrets have been zeroized`; ErrRecordAbandoned
// with Stats.Unopened at 1; and then NIL, for ever, with Stats.Omitted at zero -- because item 246's
// ceiling serves the rows at and below the epoch it was removed at and calls the page COMPLETE with a
// ceiling-relative high water, so the omission predicate had nothing to report. Its Send answered
// `sealing a message: messagegroup: an application record's inner MLS frame did not open: mls: the
// group is closed…`, which carries no sentinel at all. A device thrown out of a group was
// indistinguishable from one that was caught up and quiet, with a live composer, for ever.
//
// WHY THIS CASE IS IN THIS MODULE AND NOT IN urmessage's OWN SUITE. Three of its four clauses need
// things that package cannot reach: a real [urmessage.Group.Send] (the submit goes through
// `*sdk.MessageTransport`, a concrete type with no stub), a real [urmessage.Group.Receive] over a
// FETCH, and a real restart -- two directories reopened with nothing crossing but the disk. The
// walk's own behaviour and the store's codec are held one module over in
// urmessage/removalstate_test.go.
//
// THE FOUR CLAUSES. (1) Four consecutive Receives answer [urmessage.ErrRemovedFromGroup], carrying
// mls.ErrRemovedFromGroup and NOT the generic ErrCommitIngest, with Stats.Unopened and
// UnopenedRecords empty throughout -- the walk does not spend three attempts on the record and
// resolve past it. (2) Send answers the same name. (3) The device still READS: both of its own lines
// and the survivor's are in its log, because it holds the keys of the epoch it was removed at, which
// is what makes a read-only transcript renderable. (4) After a restart it answers the state BEFORE
// any walk -- [urmessage.Group.Removal] on the restored group, with no Receive yet -- which is the
// only assertion that can tell a persisted state from one re-derived off the wire, since the
// removing commit is still the first record above a cursor nothing persists.
//
// THE INLINE CONTROL IS THE SURVIVOR OF THE SAME COMMIT: carol receives cleanly, follows to the new
// epoch, sends, and reads (0, nil) from Removal -- in this case, over this commit. Without it every
// clause above is satisfied by a state set on every ingest.
//
// WHAT WOULD GO RED: drop the mls.ErrRemovedFromGroup arm at ApplyCommit (clause 1's first walk);
// drop the `self.removed` clause in the walk's is_commit arm (walks 2 to 4); drop the send door's
// clause (clause 2); stop writing part ten, or drop it from the restore (clause 4, which is the ONLY
// clause a re-derivation cannot fake); set the state on any ingest (the control).
func TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	carol := world.newPersona(t, "carol")
	for _, who := range []*persona{alice, bob, carol} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	// the third member, so the SURVIVOR control is somebody other than the committer
	carolGroup := removalAdd(t, ctx, "alice", aliceGroup, carol.device, "carol")
	groups := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup, "carol": carolGroup}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 2, groups)

	// the line the removed device must still be able to read afterwards, and the survivor's
	if _, err := bobGroup.Send(ctx, bobsFirstLine); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := carolGroup.Send(ctx, carolsLineBeforeTheRemoval); err != nil {
		t.Fatalf("carol's Send: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	held := gcTextsPresent(bobGroup.Messages())
	for _, text := range []string{bobsFirstLine, carolsLineBeforeTheRemoval} {
		if !held[text] {
			t.Fatalf("CONTROL FAILED: before the removal bob's log does not hold %q, so clause 3 "+
				"below would be about records that never arrived: %s", text, textsOf(bobGroup.Messages()))
		}
	}
	bobId := rolesIdentityOf(t, bobGroup)
	removedAt := bobGroup.Epoch()

	// ── THE REMOVAL ─────────────────────────────────────────────────────────────────────────────
	if err := aliceGroup.RemoveMember(ctx, bobId); err != nil {
		t.Fatalf("alice's RemoveMember of bob: %v", err)
	}
	opened := removedAt + 1
	if got := aliceGroup.Epoch(); got != opened {
		t.Fatalf("alice is at epoch %d after the removal, want %d", got, opened)
	}

	// ── THE CONTROL FIRST, OVER THE SAME COMMIT: THE SURVIVOR IS UNAFFECTED ─────────────────────
	if _, err := carolGroup.Receive(ctx); err != nil {
		t.Fatalf("CONTROL FAILED: the survivor's Receive over the removal answered %v; every clause "+
			"below would then be about a commit nobody could follow", err)
	}
	if got := carolGroup.Epoch(); got != opened {
		t.Fatalf("CONTROL FAILED: the survivor is at epoch %d after the removal, want %d", got, opened)
	}
	if epoch, state := carolGroup.Removal(); state != nil || epoch != 0 {
		t.Errorf("the SURVIVOR of the removal reads (%d, %v) from Removal: the state is being set for "+
			"a member the commit left in the group", epoch, state)
	}
	if _, err := carolGroup.Send(ctx, carolsLineAfterTheRemoval); err != nil {
		t.Errorf("CONTROL FAILED: the survivor's Send after the removal answered %v, so the send "+
			"refusal below is satisfied by a group nobody can write to", err)
	}

	// ── CLAUSE 1: FOUR WALKS, EVERY ONE BY NAME, AND NOT ONE ATTEMPT SPENT ──────────────────────
	//
	// [Stats.FailedOpen] IS THE DISCRIMINATOR AND [Stats.Unopened] IS NOT, which was MEASURED here
	// rather than reasoned: a mutant that sent the removing commit through `fail()` was caught by
	// this file's urmessage twin and PASSED here, because the sticky clause one level out means only
	// ONE attempt is ever spent and [maxRecordAttempts] is never reached -- so nothing is ever
	// abandoned and `Unopened` stays 0 over a walk that did treat the removal as a record that did
	// not open. The counter that moves on the FIRST attempt is the one that has to be asserted.
	failedOpenBefore := bobGroup.Stats().FailedOpen
	for walk := 1; walk <= 4; walk += 1 {
		_, err := bobGroup.Receive(ctx)
		removalAssertRemoved(t, fmt.Sprintf("walk %d", walk), err)
		stats := bobGroup.Stats()
		if stats.FailedOpen != failedOpenBefore {
			t.Errorf("after walk %d the removed device has spent %d open attempt(s) on the removing "+
				"commit (FailedOpen %d -> %d): a removal is the one record a device cannot open and "+
				"must not retry, and three attempts plus a cursor bump is what made walk 4 answer nil "+
				"before ruling 52", walk, stats.FailedOpen-failedOpenBefore, failedOpenBefore, stats.FailedOpen)
		}
		if stats.Unopened != 0 || len(bobGroup.UnopenedRecords()) != 0 {
			t.Errorf("after walk %d the removed device holds %d unopened record(s) %v: the removing "+
				"commit was abandoned", walk, stats.Unopened, bobGroup.UnopenedRecords())
		}
		if got := bobGroup.Epoch(); got != removedAt {
			t.Errorf("after walk %d the removed device is at epoch %d, want %d", walk, got, removedAt)
		}
		if epoch, state := bobGroup.Removal(); state == nil || epoch != removedAt {
			t.Errorf("after walk %d Removal answers (%d, %v), want (%d, non-nil)", walk, epoch, state, removedAt)
		}
	}

	// ── CLAUSE 2: SEND ─────────────────────────────────────────────────────────────────────────
	_, sendErr := bobGroup.Send(ctx, "a line from somebody who is not in this group any more")
	removalAssertRemoved(t, "Send", sendErr)

	// ── CLAUSE 3: IT STILL READS WHAT IT IS ENTITLED TO ────────────────────────────────────────
	held = gcTextsPresent(bobGroup.Messages())
	for _, text := range []string{bobsFirstLine, carolsLineBeforeTheRemoval} {
		if !held[text] {
			t.Errorf("the removed device's log has lost %q. It holds the keys of the epoch it was "+
				"removed at, so the transcript up to that epoch is exactly what Spec C screen 10's "+
				"read-only variant renders: %s", text, textsOf(bobGroup.Messages()))
		}
	}
	if held[carolsLineAfterTheRemoval] {
		t.Errorf("the removed device opened a line sealed at the epoch its own removal opened, which "+
			"it holds no keys for: %s", textsOf(bobGroup.Messages()))
	}

	// ── CLAUSE 4: THE RESTART, AND THE STATE IS READ BEFORE ANY WALK ────────────────────────────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v. A device a commit removed is still a device whose "+
			"own history is on this disk, so a restore that refused the group would take the "+
			"transcript away with the membership", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the restarted bob restored %d group(s), want 1", len(restored))
	}
	bobGroup = restored[0]
	// THE ONE ASSERTION A RE-DERIVATION CANNOT FAKE: no Receive has run in this process, and the
	// removing commit is still sitting above a cursor nothing persists, so a state read here came
	// off part ten of the group record and from nowhere else.
	epoch, state := bobGroup.Removal()
	if state == nil {
		t.Fatalf("the restored group reads (%d, nil) from Removal BEFORE its first Receive: the state "+
			"did not survive the process. Such a device comes back reading as caught up and silent "+
			"until some walk happens to re-derive it, which is the state ruling 52 exists to end", epoch)
	}
	if epoch != removedAt {
		t.Errorf("the restored group was removed at epoch %d, want %d", epoch, removedAt)
	}
	if !errors.Is(state, urmessage.ErrRemovedFromGroup) || !errors.Is(state, mls.ErrRemovedFromGroup) {
		t.Errorf("the restored state is %v; it must carry urmessage.ErrRemovedFromGroup AND mls's own "+
			"sentinel, because the cause is a value and not state a restart can invalidate", state)
	}
	if got := bobGroup.Epoch(); got != removedAt {
		t.Errorf("the restored group is at epoch %d, want %d", got, removedAt)
	}
	// and then its walks and its sends, after the restart, still answer by name
	for walk := 1; walk <= 2; walk += 1 {
		_, err := bobGroup.Receive(ctx)
		removalAssertRemoved(t, fmt.Sprintf("walk %d after the restart", walk), err)
	}
	_, sendErr = bobGroup.Send(ctx, "a line from somebody who is not in this group any more, after a restart")
	removalAssertRemoved(t, "Send after the restart", sendErr)
	if got := bobGroup.Stats().Unopened; got != 0 {
		t.Errorf("the restarted removed device holds %d unopened record(s) after re-walking its whole "+
			"history, want 0", got)
	}
	t.Logf("the removed device answered ErrRemovedFromGroup on four walks and a send, kept both "+
		"lines of the conversation up to epoch %d, came back from a restart already knowing before "+
		"its first fetch, and the survivor of the same commit is at epoch %d and still sending",
		removedAt, opened)
}

const (
	carolsLineBeforeTheRemoval = "carol's line, sealed while the device this case removes was still a member"
	carolsLineAfterTheRemoval  = "carol's line at the epoch the removal opened, which the removed device holds no keys for"
)

// removalAssertRemoved holds RULING 52's whole predicate over one answer: it IS the removal, it
// carries mls's own cause, and it is none of the three states the ruling says it must be
// distinguishable from.
//
// IT IS A HELPER BECAUSE THE PREDICATE IS THE POINT AND IT IS ASKED SEVEN TIMES in one case; a
// clause spelled seven times is six chances for one of them to be the weaker spelling.
func removalAssertRemoved(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, urmessage.ErrRemovedFromGroup) {
		t.Errorf("%s answered %v, want urmessage.ErrRemovedFromGroup", what, err)
		return
	}
	if !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Errorf("%s does not carry mls.ErrRemovedFromGroup, which is the cause: %v", what, err)
	}
	// NAMED AND NOT GENERIC: ErrCommitIngest is what a bent ciphertext answers too, and a caller
	// that saw it here would read a membership that ended as a transient worth retrying.
	if errors.Is(err, urmessage.ErrCommitIngest) {
		t.Errorf("%s also answers ErrCommitIngest, so a removal cannot be told from a commit that "+
			"did not open: %v", what, err)
	}
	for _, other := range []struct {
		name string
		err  error
	}{
		{"ErrRemovalWithoutRotation (ruling 41's halt: a commit this device REFUSED)", urmessage.ErrRemovalWithoutRotation},
		{"ErrNoWrapForEpoch (ruling 38: a commit it FOLLOWED with no keys)", urmessage.ErrNoWrapForEpoch},
		{"ErrWrapUnreadable", urmessage.ErrWrapUnreadable},
		{"ErrOrphanWrap", urmessage.ErrOrphanWrap},
		{"ErrRecordAbandoned (a record that did not open)", urmessage.ErrRecordAbandoned},
		{"ErrFetchRefused (the transport)", urmessage.ErrFetchRefused},
		{"ErrNotReconciled", urmessage.ErrNotReconciled},
		{"ErrStreamFloorUnheld", urmessage.ErrStreamFloorUnheld},
	} {
		if errors.Is(err, other.err) {
			t.Errorf("%s also answers %s; ruling 52's whole content is that this state is "+
				"distinguishable from that one: %v", what, other.name, err)
		}
	}
}

// ── THE ONE REPAIR THE REMOVED SENTINEL NAMES, THROUGH THE VERB THAT PERFORMS IT ────────────────

// A DEVICE ADDED BACK THROUGH A REAL [urmessage.Device.Join] READS NO REMOVAL, AND THE DISK AGREES
// ACROSS A RESTART.
//
// WHY THIS CASE EXISTS, AND IT IS A HOLE IN A SIBLING CASE RATHER THAN A NEW PROPERTY. The re-add
// is driven one module over by
// urmessage.TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory,
// whose subject is what the repair COSTS -- three failed walks, the abandoned line, the cursor --
// and all of that runs the real walk machinery. What it cannot run is the verb: no test in that
// package can drive a publishing verb to completion, so its re-add is a `&Group{…}` the harness
// composes and the "the state is GONE" half of the sentinel's promise is a reading of a struct
// literal. That case listed "have [urmessage.Device.Join] carry the removal forward" as a mutant
// it would catch and it CANNOT: the mutant leaves that whole module green. This case is where that
// clause is held.
//
// THE TWO CLAUSES, AND THE SECOND IS THE ONE A HARNESS CANNOT FAKE. (1) After a real
// AddMemberAndPublish and a real Join, [urmessage.Group.Removal] on the re-added group answers (0,
// nil) -- the device that read a removal one epoch ago reads none now. (2) The device is then
// KILLED and restarted, and the state is read off the restored group BEFORE any Receive, which is
// the only reading that can tell a cleared part ten from one that happens to look clear in memory.
// The disk held a removal before the re-add -- ruling 52 persists it, and this case asserts that
// first -- so a Join that did not rewrite part ten leaves a device that comes back removed from a
// group it is a member of, with a dead composer and no way back.
//
// THE CONTROL IS THE REMOVAL ITSELF, IN THIS CASE AND OVER THIS COMMIT: the removed device answers
// [urmessage.ErrRemovedFromGroup] by name and reads a non-nil removal at the epoch it was removed
// at, before anything is repaired. Without it both clauses are satisfied by a build that never
// sets the removal state at all, which is exactly the build ruling 52 replaced.
//
// WHAT WOULD GO RED: have Join carry the persisted removal forward (clause 1, and clause 2 after
// the restart); stop persisting the cleared state in Join's own persist (clause 2 only, which is
// why the restart is here and not a flourish); drop the removal state altogether (the control).
func TestADeviceAddedBackThroughARealJoinReadsNoRemovalAndTheDiskAgreesAfterARestart(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	// bob's own line, sealed while he is a member: what the re-add does NOT give back, and the
	// reason the disk has something to rewrite.
	if _, err := bobGroup.Send(ctx, bobsFirstLine); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive before the removal: %v", err)
	}
	bobId := rolesIdentityOf(t, bobGroup)
	removedAt := bobGroup.Epoch()

	// ── THE CONTROL: THE REMOVAL, AND THE STATE IT SETS ─────────────────────────────────────────
	if err := aliceGroup.RemoveMember(ctx, bobId); err != nil {
		t.Fatalf("alice's RemoveMember of bob: %v", err)
	}
	_, receiveErr := bobGroup.Receive(ctx)
	removalAssertRemoved(t, "CONTROL: the removed device's Receive", receiveErr)
	if epoch, state := bobGroup.Removal(); state == nil || epoch != removedAt {
		t.Fatalf("CONTROL FAILED: the removed device reads (%d, %v) from Removal, want (%d, "+
			"non-nil). Nothing below clears a state that was never set", epoch, state, removedAt)
	}

	// ── THE REPAIR, THROUGH THE VERBS: AddMemberAndPublish ON ONE DEVICE, Join ON THE OTHER ─────
	rejoined := removalAdd(t, ctx, "alice", aliceGroup, bob.device, "bob")
	if got := rejoined.Epoch(); got <= removedAt {
		t.Fatalf("the re-added group is at epoch %d and the removal opened epoch %d: a re-add takes "+
			"a commit of its own, so this is not the group the welcome admitted", got, removedAt+1)
	}

	// ── CLAUSE 1: THE STATE IS GONE ─────────────────────────────────────────────────────────────
	if epoch, state := rejoined.Removal(); state != nil || epoch != 0 {
		t.Fatalf("the group a real Join built reads (%d, %v) from Removal: the ONE repair "+
			"ErrRemovedFromGroup's own sentence names does not clear the state it names it for, so "+
			"a device added back to a group is still told it was thrown out of it", epoch, state)
	}

	// ── CLAUSE 2: THE DISK, READ BEFORE ANY WALK IN A NEW PROCESS ───────────────────────────────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the restarted bob restored %d group(s), want 1", len(restored))
	}
	// NO Receive HAS RUN IN THIS PROCESS. A state read here came off part ten of the group record
	// on the disk and from nowhere else, so this is the clause that holds Join's own persist.
	if epoch, state := restored[0].Removal(); state != nil || epoch != 0 {
		t.Fatalf("the RESTARTED re-added device reads (%d, %v) from Removal before its first "+
			"Receive: Join cleared the state in memory and left the removal on the disk, so this "+
			"device comes back reading as removed from a group it is a member of", epoch, state)
	}
	t.Logf("removed at epoch %d, added back through a real Join at epoch %d: Removal answers "+
		"(0, nil) and still does after the process was killed and the group restored off the disk",
		removedAt, rejoined.Epoch())
}
