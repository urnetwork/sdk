package cp3b

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/sdk/urmessage"
)

// LEDGER ITEM 241, STEP A7: HISTORY SURVIVES A MEMBERSHIP CHANGE FOR THE MEMBERS WHO WERE THERE, AND A
// NEW MEMBER GETS NONE OF IT. Owner's words: "Message history should always be readable to the
// people who were always in the group chat." The milestone before this step measured the gap it
// closes -- 602 pre-change records draining as out_of_window on a member who WAS there -- and the
// cases here are that measurement re-taken with the multi-epoch open in place, over three and then
// four real devices and a running server.
//
// THE THREE CASES THE RULING RESTS ON, each with the mutation that turns it red:
//
//  1. THE MEMBERS WHO WERE THERE OPEN EVERYTHING; THE JOINER OPENS NOTHING FROM BEFORE. Alice, the
//     committer, opens Bob's epoch-one lines she had not fetched when she added Carol; Bob, the
//     follower, opens Alice's; Carol drains every pre-admission record as a gap and opens none. Then
//     Alice PROMOTES Bob to admin -- one policy commit through SetRole, ledger item 242's R2, since
//     under MASTER §11 and ruling 1 an Add is an ADMIN's or the OWNER's to commit and Bob, an
//     unnamed non-founder, is a MEMBER whose Add every honest receiver would refuse
//     (urmessage.ErrCommitAddByNonAdmin) and whose own client now refuses to build -- and BOB
//     commits (adds Dave) with Alice's epoch-three lines unfetched, and opens them at epoch four,
//     so BOTH members that were there have exercised the prior-epoch open live, not only the
//     founder. Restore `header.Epoch != self.epoch` in connect's openRecordOnLoop: Alice's and
//     Bob's assertions go red with ErrRecordOpen. Route a joiner's pre-admission records to the
//     CURRENT handle when the prior epoch's state is missing: Carol's gap count goes red. Remove
//     the promotion: Bob's add is refused on the send side and the case stops there.
//  2. THE RESTART, with the >1024 discriminator at a PRIOR epoch and the window edge, is the
//     second case below.
//  3. THE WINDOW, exactly, is the same case's last stage.
func TestHistorySurvivesAMembershipChangeForTheMembersWhoWereThereAndNotForTheJoiner(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	carol := world.newPersona(t, "carol")
	dave := world.newPersona(t, "dave")
	for _, who := range []*persona{alice, bob, carol, dave} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}

	// ── epoch one: alice and bob each send three lines and NEITHER fetches ──────────────────────
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	aliceEpochOne := hsLines("alice at epoch one", 3)
	bobEpochOne := hsLines("bob at epoch one", 3)
	hsSendAll(t, ctx, "alice", aliceGroup, aliceEpochOne)
	hsSendAll(t, ctx, "bob", bobGroup, bobEpochOne)

	// ── alice adds carol: epoch two, with bob's three lines still unfetched at alice ────────────
	carolGroup := hsAddAndJoin(t, ctx, aliceGroup, carol)
	if aliceGroup.Epoch() != 2 || carolGroup.Epoch() != 2 {
		t.Fatalf("after the add alice is at %d and carol at %d, want 2 and 2", aliceGroup.Epoch(), carolGroup.Epoch())
	}

	// ALICE, THE COMMITTER, OPENS BOB'S EPOCH-ONE LINES AT EPOCH TWO. This is the live shape of the
	// multi-epoch open: the records precede her own commit in the server's order, her session has
	// already moved on, and before this step every one of them was an out_of_window gap.
	got, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive at epoch two: %v", err)
	}
	hsAssertOpened(t, "alice at epoch two", got, bobEpochOne)
	aliceStats := aliceGroup.Stats()
	if aliceStats.GapOutOfWindow != 0 {
		t.Errorf("alice, a member at epoch one, has %d out_of_window gap(s) after the change; a member who was there keeps her history", aliceStats.GapOutOfWindow)
	}
	if aliceStats.OpenedPastEpoch != 3 {
		t.Errorf("alice opened %d record(s) under a prior epoch's schedule, want 3 (bob's epoch-one lines)", aliceStats.OpenedPastEpoch)
	}
	assertNothingFailedToOpen(t, "alice at epoch two", aliceGroup)

	// BOB, THE FOLLOWER, OPENS ALICE'S EPOCH-ONE LINES AND FOLLOWS THE COMMIT. His walk meets them
	// at epoch one, before the commit, so they open live -- the point of asserting him is the count:
	// zero gaps for a member who was there, whichever road the records took.
	got, err = bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive that follows the commit: %v", err)
	}
	hsAssertOpened(t, "bob following the commit", got, aliceEpochOne)
	if bobGroup.Epoch() != 2 {
		t.Fatalf("bob is at epoch %d after ingesting the commit, want 2", bobGroup.Epoch())
	}
	if gaps := bobGroup.Stats().GapOutOfWindow; gaps != 0 {
		t.Errorf("bob, a member at epoch one, has %d out_of_window gap(s) after the change", gaps)
	}
	assertNothingFailedToOpen(t, "bob at epoch two", bobGroup)

	// CAROL, ADMITTED AT EPOCH TWO, DRAINS THE SIX PRE-ADMISSION LINES AS GAPS AND OPENS NONE.
	// Her device holds no epoch-one state, so the loader answers the store's not-found and the walk
	// renders a gap; that is MLS's own answer for a later joiner and item 241 keeps it.
	got, err = carolGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("carol's draining Receive: %v", err)
	}
	carolStats := carolGroup.Stats()
	const preAdmission = 6
	if carolStats.GapOutOfWindow != preAdmission {
		t.Errorf("carol, admitted at epoch two, has %d out_of_window gap(s), want %d: every line from before her admission and nothing else",
			carolStats.GapOutOfWindow, preAdmission)
	}
	if carolStats.Opened != 0 || carolStats.OpenedPastEpoch != 0 {
		t.Errorf("carol opened %d record(s) (%d under a prior epoch) from before her admission; a joiner gets none of it unless the group grants it",
			carolStats.Opened, carolStats.OpenedPastEpoch)
	}
	if opened := hsOpenedTexts(got); len(opened) != 0 {
		t.Errorf("carol received text from before her admission: %v", opened)
	}
	if carolStats.FailedOpen != 0 {
		t.Errorf("carol's pre-admission records failed to open %d time(s); they must be gaps, not failures", carolStats.FailedOpen)
	}

	// ── epoch three: alice promotes bob to ADMIN, the role model's committing arm (R2) ──────────
	//
	// One policy commit through the verb, with nothing unfetched at alice: what it exercises is
	// that a promotion made through SetRole is ingested by every member and read back the same at
	// every one, which is what lets bob be the next committer under ruling 1.
	bobId := rolesIdentityOf(t, bobGroup)
	if err := aliceGroup.SetRole(ctx, bobId, "admin"); err != nil {
		t.Fatalf("alice's SetRole promoting bob: %v", err)
	}
	if aliceGroup.Epoch() != 3 {
		t.Fatalf("alice is at epoch %d after the promotion commit, want 3", aliceGroup.Epoch())
	}
	for _, who := range []struct {
		name  string
		group *urmessage.Group
	}{{"bob", bobGroup}, {"carol", carolGroup}} {
		if _, err := who.group.Receive(ctx); err != nil {
			t.Fatalf("%s's Receive that follows the promotion: %v", who.name, err)
		}
		if who.group.Epoch() != 3 {
			t.Fatalf("%s is at epoch %d after ingesting the promotion, want 3", who.name, who.group.Epoch())
		}
	}
	rolesAssertRoster(t, 3, map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup, "carol": carolGroup},
		map[string][]byte{"alice": rolesIdentityOf(t, aliceGroup), "bob": bobId, "carol": rolesIdentityOf(t, carolGroup)},
		map[string]string{"alice": "owner", "bob": "admin", "carol": "member"})
	if role, err := bobGroup.MyRole(); err != nil || role != "admin" {
		t.Fatalf("bob's MyRole after the promotion is %q, %v; want admin", role, err)
	}

	// ── epoch three: alice sends two lines; BOB, now an admin, commits (adds dave) without fetching them ──
	aliceEpochThree := hsLines("alice at epoch three", 2)
	hsSendAll(t, ctx, "alice", aliceGroup, aliceEpochThree)
	daveGroup := hsAddAndJoin(t, ctx, bobGroup, dave)
	if bobGroup.Epoch() != 4 || daveGroup.Epoch() != 4 {
		t.Fatalf("after bob's add bob is at %d and dave at %d, want 4 and 4", bobGroup.Epoch(), daveGroup.Epoch())
	}
	// BOB, NOW THE COMMITTER, OPENS ALICE'S EPOCH-THREE LINES AT EPOCH FOUR: the non-founder's
	// prior-epoch open, live, under epoch three's schedule.
	got, err = bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive at epoch four: %v", err)
	}
	hsAssertOpened(t, "bob at epoch four", got, aliceEpochThree)
	bobStats := bobGroup.Stats()
	if bobStats.GapOutOfWindow != 0 {
		t.Errorf("bob has %d out_of_window gap(s) after his own commit", bobStats.GapOutOfWindow)
	}
	if bobStats.OpenedPastEpoch != 2 {
		t.Errorf("bob opened %d record(s) under a prior epoch's schedule, want 2 (alice's epoch-three lines)", bobStats.OpenedPastEpoch)
	}
	// alice and carol follow bob's commit; carol's gap count does NOT move, because the lines she
	// meets now are from an epoch she was in.
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive that follows bob's commit: %v", err)
	}
	if aliceGroup.Epoch() != 4 {
		t.Fatalf("alice is at epoch %d after ingesting bob's commit, want 4", aliceGroup.Epoch())
	}
	aliceStats = aliceGroup.Stats()
	if aliceStats.GapOutOfWindow != 0 {
		t.Errorf("alice has %d out_of_window gap(s) after following bob's commit", aliceStats.GapOutOfWindow)
	}
	got, err = carolGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("carol's Receive that follows bob's commit: %v", err)
	}
	hsAssertOpened(t, "carol at epoch three", got, aliceEpochThree)
	if gaps := carolGroup.Stats().GapOutOfWindow; gaps != preAdmission {
		t.Errorf("carol's gap count moved from %d to %d over records from an epoch she was in", preAdmission, gaps)
	}
	// dave, admitted at epoch four, drains all eight earlier lines as gaps.
	if _, err := daveGroup.Receive(ctx); err != nil {
		t.Fatalf("dave's draining Receive: %v", err)
	}
	if gaps, opened := daveGroup.Stats().GapOutOfWindow, daveGroup.Stats().Opened; gaps != 8 || opened != 0 {
		t.Errorf("dave, admitted at epoch four, has %d gap(s) and opened %d, want 8 and 0", gaps, opened)
	}
	for _, who := range []struct {
		name  string
		group *urmessage.Group
	}{{"alice", aliceGroup}, {"bob", bobGroup}, {"carol", carolGroup}, {"dave", daveGroup}} {
		assertNothingFailedToOpen(t, who.name+" at epoch four", who.group)
	}

	// ── the plain restart: bob re-walks his whole history at epoch four and opens all of it ─────
	//
	// Every line he ever read comes back: alice's epoch-one lines under epoch one's schedule,
	// alice's epoch-three lines under epoch three's, his own three from his copies. Zero gaps. The
	// head persistence is not what makes THIS restart pass -- every epoch is inside the window and
	// every record re-opens from the first, so the ladders walk from zero -- and the case that
	// needs it is the next test.
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("bob was in one group and %d came back", len(restored))
	}
	bobRestored := restored[0]
	got, err = bobRestored.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}
	hsAssertOpened(t, "the restarted bob", got, append(append([]string{}, aliceEpochOne...), aliceEpochThree...))
	hsAssertOpened(t, "the restarted bob, from his copies", got, bobEpochOne)
	restoredStats := bobRestored.Stats()
	if restoredStats.GapOutOfWindow != 0 {
		t.Errorf("the restarted bob has %d out_of_window gap(s); the milestone counted 2 here and item 241 makes it 0", restoredStats.GapOutOfWindow)
	}
	if restoredStats.OpenedPastEpoch != 5 {
		t.Errorf("the restarted bob opened %d record(s) under prior epochs' schedules, want 5 (alice's three at epoch one and two at epoch three)", restoredStats.OpenedPastEpoch)
	}
	assertNothingFailedToOpen(t, "the restarted bob", bobRestored)
	t.Logf("A7: alice opened %d and bob %d record(s) under prior epochs live, the restarted bob %d; carol's and dave's pre-admission gaps stayed at %d and %d",
		aliceStats.OpenedPastEpoch, bobStats.OpenedPastEpoch, restoredStats.OpenedPastEpoch,
		carolGroup.Stats().GapOutOfWindow, daveGroup.Stats().GapOutOfWindow)
}

// THE RESTART THAT NEEDS THE PERSISTED HEADS, THE >1024 DISCRIMINATOR AT A PRIOR EPOCH, AND THE
// WINDOW'S EXACT EDGE, in one long history because each stage is the setup of the next.
//
// Bob sends more than one receiver window at epoch one and one more line at epoch two; alice reads
// all of it live, then adds members until she stands exactly PastEpochWindow epochs past epoch one.
//
//   - STAGE ONE, a restart at the edge: the restarted alice re-walks everything. Bob's epoch-one
//     lines are exactly 32 epochs behind and every one of them opens -- 1,030 of them, through ONE
//     rebuilt epoch-one schedule whose ratchet walks past MaxGenerationSkip as a live group's would.
//     A schedule rebuilt per record would refuse the 1,026th. Then bob's epoch-two line, at a stream
//     index past the receiver window, opens under epoch two's schedule with the ladder tracked at
//     the head the epoch-one opens just raised. Zero gaps. THE MUTATION: rebuild per record in
//     connect's pastEpochOnLoop and this stage goes red at record 1,026.
//   - STAGE TWO, one epoch further: epoch one is now 33 behind and every one of its 1,030 lines is
//     a gap -- not 1,029 and not 1,031, and not one of them a failure. Bob's epoch-two line still
//     opens, and THIS is the persisted head proving itself: nothing in this process has opened an
//     epoch-one record, so the only thing that can put the epoch-two ladder at index 1,030 rather
//     than 0 is the table the previous run wrote. THE MUTATION: skip the PeerHeads read in
//     restoreOne, or write nothing in persistPeerHeadsLocked, and this stage goes red on bob's
//     epoch-two line with ErrOutOfWindow. Move the window bound by one in either direction and
//     one of the two stages goes red.
func TestARestartedMemberOpensItsBacklogPastTheWindowAndTheWindowClosesExactlyAtThirtyTwo(t *testing.T) {
	if testing.Short() {
		t.Skip("this case sends past the receiver window and drives 33 epochs; skipped under -short")
	}
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

	// ── epoch one: bob sends past the receiver window, alice reads all of it ───────────────────
	const pastTheWindow = int(messagegroup.DefaultRecordWindowSize) + 6
	bobEpochOne := hsLines("bob at epoch one", pastTheWindow)
	hsSendAll(t, ctx, "bob", bobGroup, bobEpochOne)
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive of bob's %d lines: %v", pastTheWindow, err)
	}
	assertNothingFailedToOpen(t, "alice at epoch one", aliceGroup)
	bobHandle := senderHandleOf(t, aliceGroup)

	// ── epoch two: alice adds a member, bob follows and sends ONE more line, alice reads it ─────
	hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, "member-2"))
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive that follows the commit: %v", err)
	}
	if bobGroup.Epoch() != 2 {
		t.Fatalf("bob is at epoch %d, want 2", bobGroup.Epoch())
	}
	const bobEpochTwo = "bob at epoch two, at a stream index past the receiver window"
	if _, err := bobGroup.Send(ctx, bobEpochTwo); err != nil {
		t.Fatalf("bob's epoch-two Send: %v", err)
	}
	indices := streamIndicesOf(t, world, groupId, bobHandle)
	if last := indices[len(indices)-1]; last <= uint64(messagegroup.DefaultRecordWindowSize) {
		t.Fatalf("bob's epoch-two line is at stream index %d, which is not past the receiver window", last)
	}
	gcReceiveText(t, ctx, "alice", aliceGroup, bobEpochTwo)

	// ── alice adds members until she is exactly PastEpochWindow epochs past epoch one ──────────
	for aliceGroup.Epoch() < 1+messagegroup.PastEpochWindow {
		hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, fmt.Sprintf("member-%d", aliceGroup.Epoch()+1)))
	}
	if aliceGroup.Epoch() != 1+messagegroup.PastEpochWindow {
		t.Fatalf("alice is at epoch %d, want %d", aliceGroup.Epoch(), 1+messagegroup.PastEpochWindow)
	}

	// ── STAGE ONE: restart at the edge ─────────────────────────────────────────────────────────
	alice = world.restart(t, alice)
	aliceGroup = hsRestoreOne(t, ctx, alice)
	if aliceGroup.Epoch() != 1+messagegroup.PastEpochWindow {
		t.Fatalf("the restarted alice came back at epoch %d, want %d", aliceGroup.Epoch(), 1+messagegroup.PastEpochWindow)
	}
	got, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Receive at the edge of the window: %v", err)
	}
	hsAssertOpened(t, "the restarted alice at the edge", got, append(append([]string{}, bobEpochOne...), bobEpochTwo))
	stats := aliceGroup.Stats()
	if stats.GapOutOfWindow != 0 {
		t.Errorf("at epoch %d, exactly PastEpochWindow past epoch one, the restarted alice has %d out_of_window gap(s); the window is one epoch too short",
			aliceGroup.Epoch(), stats.GapOutOfWindow)
	}
	if want := uint64(pastTheWindow) + 1; stats.OpenedPastEpoch != want {
		t.Errorf("the restarted alice opened %d record(s) under prior epochs, want %d", stats.OpenedPastEpoch, want)
	}
	assertNothingFailedToOpen(t, "the restarted alice at the edge", aliceGroup)

	// ── STAGE TWO: one more epoch, and epoch one is out of the window ──────────────────────────
	hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, "member-last"))
	if aliceGroup.Epoch() != 2+messagegroup.PastEpochWindow {
		t.Fatalf("alice is at epoch %d, want %d", aliceGroup.Epoch(), 2+messagegroup.PastEpochWindow)
	}
	alice = world.restart(t, alice)
	aliceGroup = hsRestoreOne(t, ctx, alice)
	got, err = aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Receive past the window: %v", err)
	}
	stats = aliceGroup.Stats()
	if stats.GapOutOfWindow != uint64(pastTheWindow) {
		t.Errorf("at epoch %d, PastEpochWindow+1 past epoch one, the restarted alice has %d out_of_window gap(s), want exactly %d: every epoch-one line and nothing else",
			aliceGroup.Epoch(), stats.GapOutOfWindow, pastTheWindow)
	}
	if opened := hsOpenedTexts(got); opened[bobEpochOne[0]] || opened[bobEpochOne[len(bobEpochOne)-1]] {
		t.Errorf("an epoch-one line opened at epoch %d, PastEpochWindow+1 past it; the window is one epoch too long", aliceGroup.Epoch())
	}
	// THE PERSISTED HEAD: bob's epoch-two line is at index 1,030 and no epoch-one record opened in
	// this process to walk the ladder there.
	hsAssertOpened(t, "the restarted alice past the window", got, []string{bobEpochTwo})
	if stats.OpenedPastEpoch != 1 {
		t.Errorf("the restarted alice opened %d record(s) under prior epochs, want 1 (bob's epoch-two line, positioned by the persisted head)", stats.OpenedPastEpoch)
	}
	assertNothingFailedToOpen(t, "the restarted alice past the window", aliceGroup)
	t.Logf("A7: at epoch %d every one of bob's %d epoch-one lines opened and at epoch %d every one was a gap; bob's epoch-two line at stream index %d opened both times",
		1+messagegroup.PastEpochWindow, pastTheWindow, 2+messagegroup.PastEpochWindow, indices[len(indices)-1])
}

// ── helpers ──────────────────────────────────────────────────────────────────────────────────

// hsLines is n distinct lines under one prefix.
func hsLines(prefix string, n int) []string {
	lines := make([]string, 0, n)
	for i := range n {
		lines = append(lines, fmt.Sprintf("%s: line %d of %d", prefix, i+1, n))
	}
	return lines
}

// hsSendAll sends every line, in order, and fails on the first refusal.
func hsSendAll(t *testing.T, ctx context.Context, who string, group *urmessage.Group, lines []string) {
	t.Helper()
	for i, line := range lines {
		if _, err := group.Send(ctx, line); err != nil {
			t.Fatalf("%s's Send %d: %v", who, i, err)
		}
	}
}

// hsAddAndJoin has one group add a persona and the persona join, and answers the joined group.
func hsAddAndJoin(t *testing.T, ctx context.Context, adder *urmessage.Group, who *persona) *urmessage.Group {
	t.Helper()
	if err := who.device.Connect(ctx); err != nil {
		t.Fatalf("%s's Connect: %v", who.name, err)
	}
	keyPackage, err := who.device.KeyPackage()
	if err != nil {
		t.Fatalf("%s's KeyPackage: %v", who.name, err)
	}
	invite, err := adder.AddMemberAndPublish(ctx, keyPackage)
	if err != nil {
		t.Fatalf("adding %s: %v", who.name, err)
	}
	joined, err := who.device.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("%s's Join: %v", who.name, err)
	}
	return joined
}

// hsRestoreOne connects a restarted persona and restores its one group.
func hsRestoreOne(t *testing.T, ctx context.Context, who *persona) *urmessage.Group {
	t.Helper()
	if err := who.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted %s's Connect: %v", who.name, err)
	}
	restored, err := who.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted %s's Restore: %v", who.name, err)
	}
	if len(restored) != 1 {
		t.Fatalf("%s was in one group and %d came back", who.name, len(restored))
	}
	return restored[0]
}

// hsOpenedTexts is the set of texts that came back as MESSAGES and not as gaps.
func hsOpenedTexts(messages []*urmessage.Message) map[string]bool {
	return gcTextsPresent(messages)
}

// hsAssertOpened requires every wanted line to be among the OPENED texts of one Receive.
func hsAssertOpened(t *testing.T, who string, got []*urmessage.Message, want []string) {
	t.Helper()
	opened := hsOpenedTexts(got)
	missing := 0
	for _, line := range want {
		if !opened[line] {
			missing += 1
			if missing <= 3 {
				t.Errorf("%s did not open %q", who, line)
			}
		}
	}
	if missing > 3 {
		t.Errorf("%s: %d of %d wanted lines did not open", who, missing, len(want))
	}
}
