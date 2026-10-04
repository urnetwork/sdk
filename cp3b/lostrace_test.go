package cp3b

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// AN HONEST COMMITTER THAT LOSES THE EPOCH RACE STAYS WHERE IT WAS, FOLLOWS THE WINNER, AND
// RETRIES (MASTER §9.3, spec B §6.2, ledger item 242's R2).
//
// R2 is what makes two committers routine: an owner and its admins act on one group, and two of
// them acting within one fetch interval is ordinary honest use rather than an attack. The server
// takes exactly one commit per (group, epoch) and answers the other REASON_COMMIT_LOST; what the
// loser does next is the whole of this case, and before 2026-09-22 it was the wrong thing.
// Measured then, by this shape: the owner's commit was MERGED before it was submitted, so on the
// refusal her handle stood at a private 4 while her group stood at 3 -- Receive over the winner's
// commit answered "mls: message does not decrypt", Send was answered EPOCH_STALE, and Members()
// showed the transfer that never landed. Dead until the app restarted.
//
// TWO RACES, ONE PER ROAD, AND THE LOSER IS THE SAME DEVICE BOTH TIMES. In the first an admin's
// Add wins and the owner's TransferOwnership loses -- the policy road loses. In the second the
// owner's SetRole wins and an admin's AddMemberAndPublish loses -- the add road loses, with a key
// package that is then offered again. Each time the loser is answered ErrCommitLost (wrapping
// ErrSubmitRefused, so the old reading holds), stands at the epoch it was at, reads the LIVE
// policy and not the one it built, ingests the winner's commit on its next Receive (which is the
// one thing the forked device could not do), retries the same verb, and the group converges: one
// epoch, one roster, every line at the final epoch opened by every member.
//
// AND THE RACE IS SETTLED WITHOUT S2-2'S RECOVERY. COMMIT_LOST is answered only after write_auth
// verified (spec B §4.5), so it is not a nonce fact and a Hello, a rebind and a re-MAC cannot change
// it; the sentence the old order produced on every lost race, "REASON_COMMIT_LOST again after a
// fresh Hello and a re-MAC", is asserted absent.
//
// WHAT WOULD GO RED: merge before submit again, and the loser's Receive fails with the decrypt
// refusal and her epoch reads 3 with a roster nobody else holds. Drop the epoch-race arm and the
// error is the plain ErrSubmitRefused with the re-MAC sentence in it. Skip ClearPendingCommit on
// the refusal and the retry is refused by mls with "a pending commit is already staged".
func TestALostEpochRaceLeavesTheHonestCommitterWhereItWasAndItRetries(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice, _, _ := world.device(t, "alice")
	bob, _, _ := world.device(t, "bob")
	carol, _, _ := world.device(t, "carol")
	dave, _, _ := world.device(t, "dave")
	erin, _, _ := world.device(t, "erin")
	for _, who := range []*urmessage.Device{alice, bob, carol, dave, erin} {
		if err := who.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}
	groupId := newGroupId(t)
	serverEpoch := func() uint64 {
		state, err := world.store.GroupState(ctx, groupId)
		if err != nil {
			t.Fatalf("the server's group state: %v", err)
		}
		return state.CurrentEpoch
	}

	// ── epochs 1 to 3: alice founds with bob, promotes him, and he adds carol ─────────────────
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
	identities := map[string][]byte{"alice": aliceId, "bob": bobId}
	if err := aliceGroup.SetRole(ctx, bobId, "admin"); err != nil {
		t.Fatalf("alice's SetRole promoting bob: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	carolKeyPackage, err := carol.KeyPackage()
	if err != nil {
		t.Fatalf("carol's KeyPackage: %v", err)
	}
	invite, err = bobGroup.AddMemberAndPublish(ctx, carolKeyPackage)
	if err != nil {
		t.Fatalf("bob's AddMemberAndPublish of carol: %v", err)
	}
	carolGroup, err := carol.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("carol's Join: %v", err)
	}
	groups["carol"] = carolGroup
	identities["carol"] = rolesIdentityOf(t, carolGroup)
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 3, groups)
	rosterAtThree := map[string]string{"alice": "owner", "bob": "admin", "carol": "member"}
	rolesAssertRoster(t, 3, groups, identities, rosterAtThree)
	if got := serverEpoch(); got != 3 {
		t.Fatalf("the server is at epoch %d before the first race, want 3", got)
	}

	// ── race 1: bob's Add of dave wins; alice's transfer, built at 3 unfetched, loses ──────────
	daveKeyPackage, err := dave.KeyPackage()
	if err != nil {
		t.Fatalf("dave's KeyPackage: %v", err)
	}
	invite, err = bobGroup.AddMemberAndPublish(ctx, daveKeyPackage)
	if err != nil {
		t.Fatalf("bob's AddMemberAndPublish of dave: %v", err)
	}
	daveGroup, err := dave.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("dave's Join: %v", err)
	}
	identities["dave"] = rolesIdentityOf(t, daveGroup)
	if got := serverEpoch(); got != 4 {
		t.Fatalf("the server is at epoch %d after bob's add, want 4", got)
	}
	ingestedBefore := aliceGroup.Stats().Ingested
	err = aliceGroup.TransferOwnership(ctx, bobId)
	assertLostRace(t, "alice's TransferOwnership built at 3 against a server at 4", err)
	if aliceGroup.Epoch() != 3 {
		t.Fatalf("alice is at epoch %d after losing the race, want 3: the loser moved", aliceGroup.Epoch())
	}
	if got := serverEpoch(); got != 4 {
		t.Fatalf("the server is at epoch %d after alice's refused commit, want 4", got)
	}
	// THE LIVE POLICY AND NOT THE ONE SHE BUILT: alice still reads herself as the owner
	rolesAssertRoster(t, 3, map[string]*urmessage.Group{"alice": aliceGroup}, identities, rosterAtThree)
	if role, err := aliceGroup.MyRole(); err != nil || role != "owner" {
		t.Fatalf("alice's MyRole after losing the race is %q, %v; want owner, the live policy", role, err)
	}
	// THE ONE THING THE FORKED DEVICE COULD NOT DO: follow the winner
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive over the commit that beat hers: %v", err)
	}
	if aliceGroup.Epoch() != 4 {
		t.Fatalf("alice is at epoch %d after following the winner, want 4", aliceGroup.Epoch())
	}
	if got := aliceGroup.Stats().Ingested; got != ingestedBefore+1 {
		t.Fatalf("alice ingested %d commit(s) following the winner, want exactly one", got-ingestedBefore)
	}
	groups["dave"] = daveGroup
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 4, groups)
	rosterAtFour := map[string]string{"alice": "owner", "bob": "admin", "carol": "member", "dave": "member"}
	rolesAssertRoster(t, 4, groups, identities, rosterAtFour)
	// THE RETRY, re-derived against the winner
	if err := aliceGroup.TransferOwnership(ctx, bobId); err != nil {
		t.Fatalf("alice's TransferOwnership retried at 4: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 5, groups)
	rosterAtFive := map[string]string{"alice": "admin", "bob": "owner", "carol": "member", "dave": "member"}
	rolesAssertRoster(t, 5, groups, identities, rosterAtFive)
	rolesAssertMesh(t, ctx, 5, groups, rosterAtFive)

	// ── race 2: bob's SetRole wins; alice's Add of erin, built at 5 unfetched, loses ───────────
	if err := bobGroup.SetRole(ctx, identities["carol"], "admin"); err != nil {
		t.Fatalf("bob's SetRole promoting carol, as the owner: %v", err)
	}
	if got := serverEpoch(); got != 6 {
		t.Fatalf("the server is at epoch %d after bob's promotion, want 6", got)
	}
	erinKeyPackage, err := erin.KeyPackage()
	if err != nil {
		t.Fatalf("erin's KeyPackage: %v", err)
	}
	ingestedBefore = aliceGroup.Stats().Ingested
	invite, err = aliceGroup.AddMemberAndPublish(ctx, erinKeyPackage)
	assertLostRace(t, "alice's AddMemberAndPublish built at 5 against a server at 6", err)
	if invite != nil {
		t.Fatal("a lost add answered an Invite; a joiner handed it would join an epoch nobody entered")
	}
	if aliceGroup.Epoch() != 5 {
		t.Fatalf("alice is at epoch %d after losing the second race, want 5", aliceGroup.Epoch())
	}
	rolesAssertRoster(t, 5, map[string]*urmessage.Group{"alice": aliceGroup}, identities, rosterAtFive)
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive over the promotion that beat her add: %v", err)
	}
	if aliceGroup.Epoch() != 6 || aliceGroup.Stats().Ingested != ingestedBefore+1 {
		t.Fatalf("alice is at epoch %d having ingested %d commit(s) after following the winner, want 6 and one",
			aliceGroup.Epoch(), aliceGroup.Stats().Ingested-ingestedBefore)
	}
	// THE SAME KEY PACKAGE, offered again: nothing consumed it
	invite, err = aliceGroup.AddMemberAndPublish(ctx, erinKeyPackage)
	if err != nil {
		t.Fatalf("alice's AddMemberAndPublish of erin retried at 6: %v", err)
	}
	erinGroup, err := erin.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("erin's Join: %v", err)
	}
	groups["erin"] = erinGroup
	identities["erin"] = rolesIdentityOf(t, erinGroup)
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 7, groups)
	if got := serverEpoch(); got != 7 {
		t.Fatalf("the server is at epoch %d at the end, want 7", got)
	}
	rosterAtSeven := map[string]string{"alice": "admin", "bob": "owner", "carol": "admin", "dave": "member", "erin": "member"}
	rolesAssertRoster(t, 7, groups, identities, rosterAtSeven)
	rolesAssertMesh(t, ctx, 7, groups, rosterAtSeven)
	t.Logf("lost race: the owner lost a transfer to an admin's add and an admin lost an add to the owner's promotion; both times she stayed at her epoch, followed the winner, retried, and five devices converged at 7")
}

// assertLostRace holds a verb's answer to the shape a losing committer is owed: ErrCommitLost,
// wrapping ErrSubmitRefused, settled without S2-2's recovery.
func assertLostRace(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s answered nil; the server accepts one commit per epoch and this one was second", what)
	}
	if !errors.Is(err, urmessage.ErrCommitLost) {
		t.Fatalf("%s answered %v, want ErrCommitLost", what, err)
	}
	if !errors.Is(err, urmessage.ErrSubmitRefused) {
		t.Fatalf("%s answered ErrCommitLost without ErrSubmitRefused under it: %v", what, err)
	}
	if strings.Contains(err.Error(), "after a fresh Hello") {
		t.Fatalf("%s spent S2-2's recovery on a reason that is not a nonce fact: %v", what, err)
	}
	t.Logf("%s: %v", what, err)
}
