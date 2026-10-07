package cp3b

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/sdk/v2026/urmessage"
)

// DIAGNOSTIC, written 2026-09-29 to reproduce liveprobe step 11's red on the deployed alpha:
// `C removed, two leaves: 1204 record(s) from a member of this group did not open`, where the SAME
// 602 pre-join records had been counted `602 out_of_window gaps, opened 0, failed 0` earlier in the
// same run. The removed device in-process today is a FOUNDER
// (TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart removes bob, who is in
// openPair), so nothing in this module has ever removed a LATE JOINER that has records sealed below
// its own admission. That is the variable this case adds.
func TestARemovedLateJoinerReWalkingItsPreJoinHistoryCountsGapsAndNotFailures(t *testing.T) {
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

	// ── THE PRE-JOIN HISTORY, which is the whole point of this case ─────────────────────────────
	// ABOVE api.DefaultMaxRecordsPerFetch (512), which is the variable the live run has and the
	// 12-line first cut of this case did not: liveprobe step 4 sends 600 lines for exactly this
	// reason, so the joiner's drain of its pre-join history crosses a TRUNCATED fetch page.
	const preJoinLines = 600
	for i := 0; i < preJoinLines; i += 1 {
		if _, err := aliceGroup.Send(ctx, fmt.Sprintf("a line at epoch 1, before carol joined, %d", i)); err != nil {
			t.Fatalf("alice's pre-join Send %d: %v", i, err)
		}
	}
	pair := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup}
	rolesReceiveAll(t, ctx, pair)
	rolesAssertEpoch(t, 1, pair)

	// carol joins at epoch 2, with preJoinLines records already sealed below its admission
	carolGroup := removalAdd(t, ctx, "alice", aliceGroup, carol.device, "carol")
	groups := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup, "carol": carolGroup}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 2, groups)
	// the joiner's drain: a 600-record history does not cross a 512-record page in one Receive
	for round := 1; round <= 8; round += 1 {
		before := carolGroup.Stats().Fetched
		if _, err := carolGroup.Receive(ctx); err != nil {
			t.Fatalf("carol's drain round %d: %v", round, err)
		}
		if carolGroup.Stats().Fetched == before {
			break
		}
	}

	report := func(stage string) urmessage.Stats {
		t.Helper()
		s := carolGroup.Stats()
		t.Logf("carol %-34s epoch=%d fetched=%d opened=%d unopened=%d FailedOpen=%d outOfWindow=%d",
			stage, carolGroup.Epoch(), s.Fetched, s.Opened, s.Unopened, s.FailedOpen, s.GapOutOfWindow)
		return s
	}

	// ── MEASUREMENT 1, the live run's step 5: the joiner drains what it was not there for ───────
	joined := report("after joining at epoch 2")
	if joined.GapOutOfWindow != preJoinLines {
		t.Errorf("CONTROL FAILED: carol counted %d out_of_window gap(s) for %d record(s) sealed below "+
			"its admission; this case is about how those are classified, so it must first see them",
			joined.GapOutOfWindow, preJoinLines)
	}
	if joined.FailedOpen != 0 {
		t.Errorf("CONTROL FAILED: carol counted %d failed open(s) BEFORE any removal, so the red below "+
			"would not be the removal's", joined.FailedOpen)
	}

	// a line each way at epoch 2, so the removed device has something of its own to keep
	if _, err := carolGroup.Send(ctx, "carol's line before the removal"); err != nil {
		t.Fatalf("carol's Send: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)

	// ── THE EPOCH DEPTH, which the live run has and the 12-line and 600-line cuts did not ───────
	// liveprobe step 11 removes C at epoch 8, not at epoch 2. pastEpochOpenableLocked answers
	// `openable` whenever self.epoch-epoch <= messagegroup.PastEpochWindow, so at epoch 2 a record
	// at epoch 1 is one rung down and at epoch 8 it is seven; the live failure is a derivation for
	// epoch 1 asked by a session standing at 8 that holds pq_secrets "from epoch 2 up".
	bobId := rolesIdentityOf(t, bobGroup)
	for _, role := range []string{"admin", "member", "admin", "member", "admin", "member"} {
		if err := aliceGroup.SetRole(ctx, bobId, role); err != nil {
			t.Fatalf("alice's SetRole(bob, %q): %v", role, err)
		}
		rolesReceiveAll(t, ctx, groups)
	}
	report("after the role churn")

	carolId := rolesIdentityOf(t, carolGroup)
	removedAt := carolGroup.Epoch()

	// ── THE REMOVAL ─────────────────────────────────────────────────────────────────────────────
	if err := aliceGroup.RemoveMember(ctx, carolId); err != nil {
		t.Fatalf("alice's RemoveMember of carol: %v", err)
	}
	if got := aliceGroup.Epoch(); got != removedAt+1 {
		t.Fatalf("alice is at epoch %d after the removal, want %d", got, removedAt+1)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("CONTROL FAILED: the survivor's Receive over the removal answered %v", err)
	}

	// ── MEASUREMENT 2: the removed late joiner's walks, still in this process ────────────────────
	for walk := 1; walk <= 2; walk += 1 {
		_, err := carolGroup.Receive(ctx)
		removalAssertRemoved(t, fmt.Sprintf("walk %d", walk), err)
	}
	beforeRestart := report("after 2 walks, removed, same process")

	// ── MEASUREMENT 3: THE RESTART, which is where the live run went red ─────────────────────────
	carol = world.restart(t, carol)
	if err := carol.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted carol's Connect: %v", err)
	}
	restored, err := carol.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted carol's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the restarted carol restored %d group(s), want 1", len(restored))
	}
	carolGroup = restored[0]
	fresh := carolGroup.Stats()
	t.Logf("carol RESTORED, BEFORE ANY WALK        epoch=%d fetched=%d opened=%d unopened=%d FailedOpen=%d outOfWindow=%d",
		carolGroup.Epoch(), fresh.Fetched, fresh.Opened, fresh.Unopened, fresh.FailedOpen, fresh.GapOutOfWindow)
	for walk := 1; walk <= 2; walk += 1 {
		_, err := carolGroup.Receive(ctx)
		removalAssertRemoved(t, fmt.Sprintf("walk %d after the restart", walk), err)
	}
	afterRestart := report("after 2 walks, removed, RESTARTED")

	// ── THE ASSERTION liveprobe step 11 makes and this module has never made ────────────────────
	if afterRestart.FailedOpen != 0 {
		t.Errorf("THE LIVE RED REPRODUCES: a removed LATE JOINER that re-walks its history after a "+
			"restart counts %d failed open(s), against %d in the same process before the restart and "+
			"%d out_of_window gap(s) for the same %d pre-join record(s) when it joined. A record "+
			"sealed below this device's own admission is history it was never there for, which is a "+
			"GAP; counting it as a failure is what liveprobe step 11 went red on",
			afterRestart.FailedOpen, beforeRestart.FailedOpen, joined.GapOutOfWindow, preJoinLines)
	}
	if afterRestart.GapOutOfWindow == 0 && afterRestart.Fetched > 0 {
		t.Errorf("the restarted removed device re-walked %d record(s) and counted ZERO out_of_window "+
			"gaps, though %d of them are sealed below its admission: the classification that held "+
			"before the restart is not holding after it", afterRestart.Fetched, preJoinLines)
	}
}
