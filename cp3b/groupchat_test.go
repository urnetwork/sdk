package cp3b

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/urmessage"
)

// THE MILESTONE (ledger item 239, steps A4+A5): THREE REAL DEVICES, ONE ADDS A THIRD, ALL THREE
// CONVERGE TO EPOCH TWO AND EXCHANGE MESSAGES IN EVERY DIRECTION -- THROUGH A RUNNING SERVER, WITH
// EVERY KEY REAL. This is the step where group chats start to exist.
//
// WHAT IS NEW HERE OVER TestATextTypedOnOneClientComesBackOnASecondThroughARunningServer, which is
// the two-member CP3b: a SECOND epoch. Alice founds and adds Bob (epoch 1), the two chat, then Alice
// adds Carol -- which is a commit that OPENS EPOCH 2, publishes the wrap fan-out for it, and hands
// Carol a Welcome. Carol joins at epoch 2. Bob, who authored nothing, INGESTS Alice's commit on his
// next Receive and follows the group into epoch 2. Then a message from each device opens on both
// others, which is the assertion that says all three share one epoch-2 key schedule rather than
// merely agreeing on the number 2.
//
// THE STRINGS. Typed once each, compared against the variable and never a repeated literal.
const (
	gcEpochOneAliceToBob = "epoch one: alice to bob, before anyone was added"
	gcEpochOneBobToAlice = "epoch one: bob to alice, the answer under the joiner's leaf"
	gcAliceAtEpochTwo    = "epoch two: alice, to a group that now has three members"
	gcBobAtEpochTwo      = "epoch two: bob, who followed a commit he did not author"
	gcCarolAtEpochTwo    = "epoch two: carol, the third member, sealing under her own new leaf"
)

func TestThreeDevicesConvergeToEpochTwoAndExchangeMessagesEveryDirection(t *testing.T) {
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

	// ── epoch one: alice founds, adds bob, and the two exchange a line each ────────────────────
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if aliceGroup.Epoch() != 1 || bobGroup.Epoch() != 1 {
		t.Fatalf("epoch one founding left alice at %d and bob at %d", aliceGroup.Epoch(), bobGroup.Epoch())
	}
	if _, err := aliceGroup.Send(ctx, gcEpochOneAliceToBob); err != nil {
		t.Fatalf("alice's epoch-one Send: %v", err)
	}
	gcReceiveText(t, ctx, "bob", bobGroup, gcEpochOneAliceToBob)
	if _, err := bobGroup.Send(ctx, gcEpochOneBobToAlice); err != nil {
		t.Fatalf("bob's epoch-one Send: %v", err)
	}
	gcReceiveText(t, ctx, "alice", aliceGroup, gcEpochOneBobToAlice)

	// ── alice adds carol: the commit that OPENS EPOCH TWO, published to the server ─────────────
	carolKeyPackage, err := carol.device.KeyPackage()
	if err != nil {
		t.Fatalf("carol's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMemberAndPublish(ctx, carolKeyPackage)
	if err != nil {
		t.Fatalf("alice's AddMemberAndPublish: %v", err)
	}
	if aliceGroup.Epoch() != 2 {
		t.Fatalf("the commit that added carol left alice at epoch %d, want 2", aliceGroup.Epoch())
	}
	// the invite crosses as octets, as a carrier would move it
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	carolGroup, err := carol.device.Join(ctx, carried)
	if err != nil {
		t.Fatalf("carol's Join: %v", err)
	}
	if carolGroup.Epoch() != 2 {
		t.Fatalf("carol joined at epoch %d, want 2", carolGroup.Epoch())
	}

	// ── bob INGESTS the commit and follows the group into epoch two ────────────────────────────
	//
	// Bob authored nothing: he learns of the membership change only by fetching the commit record
	// and processing it. This is the A5 ingest path, over a real server, and it is the one arm of
	// the milestone that neither the committer (who merges its own commit) nor the joiner (who gets
	// a Welcome) exercises.
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive that must ingest the commit: %v", err)
	}
	if bobGroup.Epoch() != 2 {
		t.Fatalf("bob did not follow the commit into epoch two: he is at epoch %d", bobGroup.Epoch())
	}
	if ingested := bobGroup.Stats().Ingested; ingested != 1 {
		t.Errorf("bob's Stats.Ingested is %d after following one commit, want 1", ingested)
	}

	// carol drains the pre-join history into gaps and gets her cursor current. Alice's and bob's
	// epoch-one lines are records carol cannot open (she joined at epoch two), so they are
	// out_of_window gaps -- item 241's history-for-new-members is a later step -- not failures.
	if _, err := carolGroup.Receive(ctx); err != nil {
		t.Fatalf("carol's draining Receive: %v", err)
	}

	// ── all three at epoch two ─────────────────────────────────────────────────────────────────
	if aliceGroup.Epoch() != 2 || bobGroup.Epoch() != 2 || carolGroup.Epoch() != 2 {
		t.Fatalf("epochs did not converge: alice %d, bob %d, carol %d",
			aliceGroup.Epoch(), bobGroup.Epoch(), carolGroup.Epoch())
	}

	// ── messages in every direction, every device opening every one ────────────────────────────
	//
	// One line from each device, received by both others: six directions across the three pairs.
	if _, err := aliceGroup.Send(ctx, gcAliceAtEpochTwo); err != nil {
		t.Fatalf("alice's epoch-two Send: %v", err)
	}
	gcReceiveText(t, ctx, "bob", bobGroup, gcAliceAtEpochTwo)
	gcReceiveText(t, ctx, "carol", carolGroup, gcAliceAtEpochTwo)

	if _, err := bobGroup.Send(ctx, gcBobAtEpochTwo); err != nil {
		t.Fatalf("bob's epoch-two Send: %v", err)
	}
	gcReceiveText(t, ctx, "alice", aliceGroup, gcBobAtEpochTwo)
	gcReceiveText(t, ctx, "carol", carolGroup, gcBobAtEpochTwo)

	if _, err := carolGroup.Send(ctx, gcCarolAtEpochTwo); err != nil {
		t.Fatalf("carol's epoch-two Send: %v", err)
	}
	gcReceiveText(t, ctx, "alice", aliceGroup, gcCarolAtEpochTwo)
	gcReceiveText(t, ctx, "bob", bobGroup, gcCarolAtEpochTwo)

	// nothing from a member of the group failed to open, on any of the three
	assertNothingFailedToOpen(t, "alice", aliceGroup)
	assertNothingFailedToOpen(t, "bob", bobGroup)
	assertNothingFailedToOpen(t, "carol", carolGroup)

	// ── restart bob over his directory: back at epoch two, still reading ───────────────────────
	//
	// The only thing that crosses is the two directories. Bob comes back, restores, and re-walks his
	// whole history at epoch two: his epoch-one records refuse under the single-epoch session and
	// surface as out_of_window GAPS (not failures), and his epoch-two records open. The count of
	// those gaps is reported -- it is what "history across a membership change" (item 241) will one
	// day carry instead.
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
	if bobRestored.Epoch() != 2 {
		t.Fatalf("the restarted bob came back at epoch %d, want 2", bobRestored.Epoch())
	}
	got, err := bobRestored.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}
	// the restarted bob reads at least the two epoch-two lines the others sent
	readAtEpochTwo := gcTextsPresent(got)
	for _, want := range []string{gcAliceAtEpochTwo, gcCarolAtEpochTwo} {
		if !readAtEpochTwo[want] {
			t.Errorf("the restarted bob did not read %q back; it read %v", want, gcTexts(got))
		}
	}
	// its own epoch-two line comes back from the copy it kept
	if !readAtEpochTwo[gcBobAtEpochTwo] {
		t.Errorf("the restarted bob did not read its OWN epoch-two line %q back", gcBobAtEpochTwo)
	}
	if bobRestored.Epoch() != 2 {
		t.Fatalf("the restarted bob's Receive left it at epoch %d, want 2", bobRestored.Epoch())
	}
	stats := bobRestored.Stats()
	if stats.FailedOpen != 0 {
		t.Errorf("the restarted bob failed to open %d records; pre-change records must be gaps, not failures", stats.FailedOpen)
	}
	t.Logf("MILESTONE: three devices converged to epoch 2 and messaged in every direction; the restart produced %d out_of_window gap(s) (bob's own and alice's epoch-one lines now open under the rebuilt epoch-one schedule, item 241) and opened %d record(s) with 0 failures",
		stats.GapOutOfWindow, stats.Opened+stats.OpenedOwn)
}

// A4, THE CASE THAT MATTERS: A PEER WHOSE STREAM INDEX IS PAST THE WINDOW STILL OPENS AFTER AN
// EPOCH CHANGE. This is the D3 starvation the group-chat survey named, driven end to end.
//
// The receiver ratchets are zeroized at every epoch install and §5.6's stream index is continuous
// across epochs, so a peer that had sent more than [messagegroup.DefaultRecordWindowSize] (1024)
// records by the epoch that closed is at index > 1024 now. A ladder re-tracked at head 0 answers
// 1024 rungs and then ErrOutOfWindow -- so that peer's very next message goes dark. Re-tracked at
// the head this device AUTHENTICATED for it ([Group.peerHeads], carried across the change by
// [Group.crossEpochLadderLocked]), it opens. This case sends 1025 messages from bob at epoch one,
// has alice open every one, then changes the epoch and has bob send ONE more -- at an index past the
// window -- which alice must open.
//
// THE MUTATION THAT PROVES IT: re-track at 0 instead of peerHeads in crossEpochLadderLocked (and in
// trackLocked), and this case goes red at alice's final Receive with ErrOutOfWindow. The two-member
// and three-member cases above stay GREEN under that mutation, because their indices never leave the
// first window -- which is exactly why this case exists and why it sends 1025.
func TestAPeerPastTheWindowStillOpensAfterAnEpochChange(t *testing.T) {
	if testing.Short() {
		t.Skip("this case sends 1025 records to cross the receiver window; skipped under -short")
	}
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

	// bob sends more than one window's worth at epoch one; §5.6's counter is one per sender, so his
	// last record's stream index is past DefaultRecordWindowSize.
	const past = 1025
	for i := 0; i < past; i++ {
		if _, err := bobGroup.Send(ctx, fmt.Sprintf("bob epoch-one line %d of %d", i, past)); err != nil {
			t.Fatalf("bob's Send %d: %v", i, err)
		}
	}
	// alice opens every one of them, which is what raises her authenticated head for bob's ladder to
	// the top of his epoch-one stream.
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive of bob's %d lines: %v", past, err)
	}
	assertNothingFailedToOpen(t, "alice at epoch one", aliceGroup)

	bobHandle := senderHandleOf(t, aliceGroup)
	epochOneIndices := streamIndicesOf(t, world, groupId, bobHandle)
	if len(epochOneIndices) == 0 || epochOneIndices[len(epochOneIndices)-1] <= 1024 {
		t.Fatalf("bob's last epoch-one stream index is %v; the case needs it past the window (1024)", epochOneIndices)
	}

	// the epoch change: alice adds carol.
	carolKeyPackage, err := carol.device.KeyPackage()
	if err != nil {
		t.Fatalf("carol's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMemberAndPublish(ctx, carolKeyPackage)
	if err != nil {
		t.Fatalf("alice's AddMemberAndPublish: %v", err)
	}
	if aliceGroup.Epoch() != 2 {
		t.Fatalf("alice is at epoch %d after adding carol, want 2", aliceGroup.Epoch())
	}
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	if _, err := carol.device.Join(ctx, carried); err != nil {
		t.Fatalf("carol's Join: %v", err)
	}

	// bob follows the commit into epoch two, then sends ONE more line -- at an index past the window.
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive that ingests the commit: %v", err)
	}
	if bobGroup.Epoch() != 2 {
		t.Fatalf("bob did not reach epoch 2: he is at %d", bobGroup.Epoch())
	}
	const pastWindowLine = "bob's first epoch-two line, sealed at a stream index past the receiver window"
	if _, err := bobGroup.Send(ctx, pastWindowLine); err != nil {
		t.Fatalf("bob's epoch-two Send: %v", err)
	}
	epochTwoIndices := streamIndicesOf(t, world, groupId, bobHandle)
	last := epochTwoIndices[len(epochTwoIndices)-1]
	if last <= 1024 {
		t.Fatalf("bob's epoch-two record is at stream index %d, which is not past the window", last)
	}

	// THE ASSERTION: alice, at epoch two, opens bob's record at an index past the window. Under the
	// mutation (re-track at 0) this Receive fails ErrOutOfWindow and the text never arrives.
	gcReceiveText(t, ctx, "alice", aliceGroup, pastWindowLine)
	assertNothingFailedToOpen(t, "alice at epoch two", aliceGroup)
	t.Logf("A4: alice opened bob's record at stream index %d after an epoch change, %d past the window",
		last, last-1024)
}

// A5's AUTHORIZATION HOOK IS ON THE INGEST PATH. This is the case the "remove the hook call"
// mutation goes red on: a receiving device whose [urmessage.CommitAuthorizer] REFUSES a commit does
// not follow it, and the hook is handed the committer, what the commit does, and the pre-commit
// membership -- MASTER §11's receiving-client validation, shaped now and filled later.
//
// The role model itself is not built; this test supplies its own authorizer to prove the CALL is
// there and fed. Remove the call in [Group.ingestCommitLocked] and bob ingests the commit anyway,
// reaching epoch two -- and this case fails on both the missing refusal and the epoch.
func TestAReceivedCommitRunsThroughTheAuthorizationHook(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	roleModelRefuses := errors.New("cp3b: this device's authorizer refuses the commit")
	var seen *urmessage.CommitAuthorization
	calls := 0

	alice, _, _ := world.device(t, "alice")
	bob := world.deviceWithAuthorizer(t, "bob", func(decision *urmessage.CommitAuthorization) error {
		calls++
		seen = decision
		return roleModelRefuses
	})
	carol, _, _ := world.device(t, "carol")
	for _, who := range []*urmessage.Device{alice, bob, carol} {
		if err := who.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}

	// alice founds, adds bob, opens; bob joins.
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
	bobCarried := gcReencodeInvite(t, invite)
	bobGroup, err := bob.Join(ctx, bobCarried)
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}

	// alice adds carol: the commit bob's authorizer will be asked about.
	carolKeyPackage, err := carol.KeyPackage()
	if err != nil {
		t.Fatalf("carol's KeyPackage: %v", err)
	}
	if _, err := aliceGroup.AddMemberAndPublish(ctx, carolKeyPackage); err != nil {
		t.Fatalf("alice's AddMemberAndPublish: %v", err)
	}

	// bob receives the commit. His authorizer refuses it, so the ingest is refused and he stays put.
	_, err = bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrCommitUnauthorized) {
		t.Fatalf("bob's Receive over a refused commit answered %v, want it to wrap ErrCommitUnauthorized", err)
	}
	if !errors.Is(err, roleModelRefuses) {
		t.Errorf("the authorizer's own cause is not carried out through the refusal: %v", err)
	}
	if bobGroup.Epoch() != 1 {
		t.Fatalf("bob followed a commit his authorizer refused, to epoch %d", bobGroup.Epoch())
	}
	if ingested := bobGroup.Stats().Ingested; ingested != 0 {
		t.Errorf("bob's Stats.Ingested is %d after a refused commit, want 0", ingested)
	}

	// the hook was called, and fed the inputs a role check needs.
	if calls == 0 || seen == nil {
		t.Fatal("the authorizer was never called; the hook is not on the ingest path")
	}
	if seen.Epoch != 2 {
		t.Errorf("the hook saw the commit opening epoch %d, want 2", seen.Epoch)
	}
	if len(seen.AddedLeaves) != 1 {
		t.Errorf("the commit added carol; the hook saw %d added leaves: %v", len(seen.AddedLeaves), seen.AddedLeaves)
	}
	if len(seen.RemovedLeaves) != 0 || len(seen.UpdatedLeaves) != 0 {
		t.Errorf("the commit only added; the hook saw %d removed and %d updated", len(seen.RemovedLeaves), len(seen.UpdatedLeaves))
	}
	if len(seen.Members) != 2 {
		t.Errorf("the PRE-commit membership is alice and bob; the hook saw %d members", len(seen.Members))
	}
	committerIsAMember := false
	for _, member := range seen.Members {
		if member.Leaf == seen.CommitterLeaf {
			committerIsAMember = true
		}
	}
	if !committerIsAMember {
		t.Error("the committer leaf the hook saw is not among the members it was handed, so a role check could not read the committer's role")
	}
	t.Logf("A5 hook: called %d time(s), committer leaf %d, %d added, %d pre-commit members, refusal carried",
		calls, seen.CommitterLeaf, len(seen.AddedLeaves), len(seen.Members))
}

// deviceWithAuthorizer is a non-durable device with a [urmessage.CommitAuthorizer], for the hook
// case. It mirrors [world.device] and adds the one config field; no restart, so no state store.
func (self *world) deviceWithAuthorizer(t *testing.T, name string, authorizer urmessage.CommitAuthorizer) *urmessage.Device {
	t.Helper()
	client := self.connectClient(t)
	transport := self.transport(t, client)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("%s: sdk.OpenStreamStore: %v", name, err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport:        transport,
		Reserver:         sdk.NewStreamIndexReserver(streamStore),
		CommitAuthorizer: authorizer,
	})
	if err != nil {
		t.Fatalf("%s: urmessage.NewDevice: %v", name, err)
	}
	t.Cleanup(func() { device.Close() })
	return device
}

// gcReencodeInvite round-trips an invite through its octets, as a carrier would move it.
func gcReencodeInvite(t *testing.T, invite *urmessage.Invite) *urmessage.Invite {
	t.Helper()
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	return carried
}

// gcReceiveText fetches on one group and asserts a text is among the OPENED (non-gap) messages it
// hands back. It fails the test if the text is missing or came back as a gap.
func gcReceiveText(t *testing.T, ctx context.Context, who string, group *urmessage.Group, want string) {
	t.Helper()
	gcReceiveTextMessage(t, ctx, who, group, want)
}

// gcReceiveTextMessage is gcReceiveText with the [urmessage.Message] answered, for a caller that
// wants to read more off it than that it arrived.
func gcReceiveTextMessage(t *testing.T, ctx context.Context, who string, group *urmessage.Group,
	want string) *urmessage.Message {

	t.Helper()
	received, err := group.Receive(ctx)
	if err != nil {
		t.Fatalf("%s's Receive (looking for %q): %v", who, want, err)
	}
	for _, one := range received {
		if one.Gap == "" && one.Text == want {
			return one
		}
	}
	t.Fatalf("%s did not open %q; it received %v", who, want, gcTexts(received))
	return nil
}

// gcTextsPresent is the set of OPENED (non-gap) texts in a Receive result.
func gcTextsPresent(messages []*urmessage.Message) map[string]bool {
	present := map[string]bool{}
	for _, one := range messages {
		if one.Gap == "" {
			present[one.Text] = true
		}
	}
	return present
}

// gcTexts renders a Receive result for a failure message, marking gaps as such.
func gcTexts(messages []*urmessage.Message) string {
	out := []string{}
	for _, one := range messages {
		if one.Gap != "" {
			out = append(out, fmt.Sprintf("<gap:%s>", one.Gap))
			continue
		}
		out = append(out, fmt.Sprintf("%q", one.Text))
	}
	return fmt.Sprintf("%v", out)
}
