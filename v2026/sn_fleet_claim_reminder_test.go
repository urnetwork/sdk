//go:build !ios_extension && !js

package sdk

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The one Top 200 notification (owner, 2026-11-04): the hotkey is on this device, so a
// claim that waits on it waits for the app, and RemindAtMillis says when the claim can
// next go on with this device. The clock is fixed and blocks are the chain's 12 s.

var testingFleetRemindNow = time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)

func testingFleetBlocksMillis(blocks int64) int64 {
	return blocks * snBlockSeconds * 1000
}

func newTestingFleetRemindClaim(t *testing.T) *testingFleetClaim {
	t.Helper()
	fixture := newTestingFleetClaim(t)
	fixture.claim.now = func() time.Time { return testingFleetRemindNow }
	return fixture
}

func testingFleetExpectRemindAt(t *testing.T, name string, state *SnFleetClaimState, want int64) {
	t.Helper()
	if state.RemindAtMillis != want {
		t.Fatalf("%s: stage %s (%s), remind at %d, want %d (%+d ms)", name, state.Stage, state.ErrorCode, state.RemindAtMillis, want, state.RemindAtMillis-want)
	}
}

// A tapped claim can wait days for the window that keeps its new UID safe. The
// notification comes when the window opens, counted from the chain's own head, which the
// server's read may trail, and the app run again names the same moment.
func TestSnFleetClaimRemindsWhenTheRegisterWindowOpens(t *testing.T) {
	ctx := context.Background()
	nowMillis := testingFleetRemindNow.UnixMilli()
	window := func(fixture *testingFleetClaim) {
		fixture.chain.block = 1000
		fixture.server.status.CurrentBlock = 990
		fixture.server.status.RegisterAfterBlock = 1000 + 7200
		fixture.server.status.RegisterBeforeBlock = 1000 + 7200 + 16200
	}

	// the tap a day before the window: the authorization goes, registering waits
	early := newTestingFleetRemindClaim(t)
	window(early)
	state := early.claim.TapSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingRegisterWindow)
	testingFleetExpectRemindAt(t, "a tap a day early", state, nowMillis+testingFleetBlocksMillis(7200))
	if len(early.server.authorizations) != 1 || early.chain.wrote() {
		t.Fatal("the authorization should go, and nothing be spent")
	}

	// the app run again finishes the tap, and waits for the same moment
	tapped := newTestingFleetRemindClaim(t)
	tapped.authorized(t)
	tapped.chain.registered, tapped.server.status.Registered = false, false
	window(tapped)
	state = tapped.claim.AdvanceSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingRegisterWindow)
	testingFleetExpectRemindAt(t, "advance on the tapped claim", state, nowMillis+testingFleetBlocksMillis(7200))
	tapped.claim.now = func() time.Time { return testingFleetRemindNow.Add(time.Hour) }
	tapped.chain.block += 300
	testingFleetExpectRemindAt(t, "an hour later", tapped.claim.AdvanceSync(ctx), nowMillis+testingFleetBlocksMillis(7200))

	// past the window's end, until the server names the next one: an hour on
	late := newTestingFleetRemindClaim(t)
	late.chain.block = 2001
	state = late.claim.TapSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingRegisterWindow)
	testingFleetExpectRemindAt(t, "past the window", state, nowMillis+time.Hour.Milliseconds())
}

// A top-up or a conversion landing, a burn spike passing and a wallet's registration
// take minutes to about an hour, and registering comes next: an hour on, or six hours
// before the window closes when that's sooner and still ahead.
func TestSnFleetClaimRemindsWhileRegisteringWaitsOnTheChain(t *testing.T) {
	ctx := context.Background()
	nowMillis := testingFleetRemindNow.UnixMilli()
	soon := nowMillis + time.Hour.Milliseconds()
	stages := []struct {
		stage string
		set   func(*testingFleetClaim)
		pass  func(*testingFleetClaim) *SnFleetClaimState
	}{
		{SnFleetClaimStageConverting, func(f *testingFleetClaim) {
			f.chain.free = 500_000
			f.chain.canConvert = true
			f.server.status.TopUp.Available = true
		}, func(f *testingFleetClaim) *SnFleetClaimState { return f.claim.TapSync(ctx) }},
		{SnFleetClaimStageWaitingTopUp, func(f *testingFleetClaim) {
			f.chain.free = 0
			f.server.status.TopUp.Available = true
		}, func(f *testingFleetClaim) *SnFleetClaimState { return f.claim.TapSync(ctx) }},
		{SnFleetClaimStageWaitingBurn, func(f *testingFleetClaim) {
			f.chain.burn = uint64(SnFleetClaimMaxBurnRao) + 1
		}, func(f *testingFleetClaim) *SnFleetClaimState { return f.claim.TapSync(ctx) }},
		{SnFleetClaimStageNeedsWalletRegistration, func(f *testingFleetClaim) {
			f.authorized(t)
			f.chain.registered, f.server.status.Registered = false, false
			f.store.phrase = ""
		}, func(f *testingFleetClaim) *SnFleetClaimState { return f.claim.AdvanceSync(ctx) }},
	}
	ends := []struct {
		name   string
		blocks int64
		want   int64
	}{
		{"the window closes in three days", 21600, soon},
		{"the window closes in six and a half hours", 1950, nowMillis + 30*time.Minute.Milliseconds()},
		{"the window closes in five hours", 1500, soon},
	}
	for _, stage := range stages {
		for _, end := range ends {
			fixture := newTestingFleetRemindClaim(t)
			stage.set(fixture)
			fixture.server.status.RegisterBeforeBlock = int64(fixture.chain.block) + end.blocks
			state := stage.pass(fixture)
			testingFleetExpectStage(t, state, stage.stage)
			testingFleetExpectRemindAt(t, stage.stage+", "+end.name, state, end.want)
		}
	}
}

// Once registered, the server chooses the devices after they have had 12 hours to ask,
// and then this device publishes and signs: twelve hours on, or six hours before the
// next epoch starts when that's sooner and still ahead.
func TestSnFleetClaimRemindsWhileTheServerChoosesDevices(t *testing.T) {
	ctx := context.Background()
	nowMillis := testingFleetRemindNow.UnixMilli()
	later := nowMillis + 12*time.Hour.Milliseconds()
	epochs := []struct {
		name   string
		blocks int64
		want   int64
	}{
		{"the epoch starts in three days", 21600, later},
		{"the epoch starts in fifteen hours", 4500, nowMillis + 9*time.Hour.Milliseconds()},
		{"the epoch starts in three hours", 900, later},
	}
	for _, epoch := range epochs {
		// the tap registers: the chain's head counts
		registering := newTestingFleetRemindClaim(t)
		registering.server.status.NextEpochStartBlock = int64(registering.chain.block) + epoch.blocks
		state := registering.claim.TapSync(ctx)
		testingFleetExpectStage(t, state, SnFleetClaimStageRegistering)
		testingFleetExpectRemindAt(t, "registering, "+epoch.name, state, epoch.want)

		// registered, no manifest yet: the server's head counts
		choosing := newTestingFleetRemindClaim(t)
		choosing.authorized(t)
		choosing.server.status.CurrentBlock = 5000
		choosing.server.status.NextEpochStartBlock = 5000 + epoch.blocks
		state = choosing.claim.AdvanceSync(ctx)
		testingFleetExpectStage(t, state, SnFleetClaimStageChoosingDevices)
		testingFleetExpectRemindAt(t, "choosing, "+epoch.name, state, epoch.want)
	}

	// with no schedule from the server, twelve hours on
	unknown := newTestingFleetRemindClaim(t)
	unknown.authorized(t)
	state := unknown.claim.AdvanceSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageChoosingDevices)
	testingFleetExpectRemindAt(t, "choosing, no schedule", state, later)
}

// Nothing to come back for where nothing this device does moves the claim: before the
// tap, with the hotkey elsewhere, a wallet's approval or funds to come, once it has
// published and signed, when bound, and on a failure, which the app retries while it
// runs.
func TestSnFleetClaimRemindsNothingThisDeviceCannotMove(t *testing.T) {
	ctx := context.Background()
	check := func(name string, state *SnFleetClaimState, stage string) {
		t.Helper()
		testingFleetExpectStage(t, state, stage)
		testingFleetExpectRemindAt(t, name, state, 0)
	}

	fixture := newTestingFleetRemindClaim(t)
	fixture.server.status.BindingOpen = false
	check("closed", fixture.claim.TapSync(ctx), SnFleetClaimStageClosed)

	fixture = newTestingFleetRemindClaim(t)
	fixture.server.status.ConsentColdkey = ""
	check("no consent", fixture.claim.TapSync(ctx), SnFleetClaimStageNoConsent)

	fixture = newTestingFleetRemindClaim(t)
	check("before the tap", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageNeedsTap)

	fixture = newTestingFleetRemindClaim(t)
	fixture.authorized(t)
	fixture.chain.registered, fixture.server.status.Registered = false, false
	fixture.manifested(t, 3, true)
	check("a lost UID takes a new tap", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageNeedsTap)

	fixture = newTestingFleetRemindClaim(t)
	fixture.store.phrase = ""
	check("a wallet's approval", fixture.claim.TapSync(ctx), SnFleetClaimStageNeedsWalletAuthorization)

	fixture = newTestingFleetRemindClaim(t)
	fixture.server.status.AuthorizationMessage = testingFleetAuthorizationMessage(t, testingFleetColdkeySeed, testingFleetPublic(t, [32]byte{0x77}), testingFleetNetworkId)
	check("the hotkey on another device", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageHotkeyElsewhere)

	fixture = newTestingFleetRemindClaim(t)
	fixture.chain.free = 0
	check("funds to add", fixture.claim.TapSync(ctx), SnFleetClaimStageNeedsFunds)

	// this device publishes and signs in one pass; the oracle and the other devices go on
	fixture = newTestingFleetRemindClaim(t)
	fixture.authorized(t)
	fixture.server.status.CurrentBlock = 5000
	fixture.server.status.NextEpochStartBlock = 5000 + 21600
	manifest := fixture.manifested(t, 3, false)
	binding, err := manifest.Binding(manifest.Members[0], 5, 16)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
	check("published and signed", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageWaitingMirror)
	if len(fixture.chain.commitments) != 1 || len(fixture.server.hotkeySigned) != 1 {
		t.Fatal("the pass should publish and sign")
	}
	fixture.server.status.Manifest.Mirrored = true
	fixture.server.status.HotkeyWork = nil
	fixture.server.status.Counts = snFleetDeviceCounts{Chosen: 2, Ready: 1}
	check("the other devices sign", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageWaitingDevices)
	fixture.server.status.Counts = snFleetDeviceCounts{Chosen: 2, Ready: 2, Bound: 2}
	check("bound", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageBound)

	fixture = newTestingFleetRemindClaim(t)
	fixture.authorized(t)
	fixture.chain.registered, fixture.server.status.Registered = false, false
	fixture.chain.readErr = errors.New("the rpc is down")
	check("a failed read", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageFailed)

	fixture = newTestingFleetRemindClaim(t)
	fixture.authorized(t)
	fixture.manifested(t, 3, false)
	fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: "{"}}
	check("a binding it can't sign", fixture.claim.AdvanceSync(ctx), SnFleetClaimStageFailed)
}

// Refresh only reads. It can't tell a tapped claim waiting to register from one never
// tapped, so it never names a moment, even where it reads the same stage as Advance.
func TestSnFleetClaimRefreshNeverReminds(t *testing.T) {
	ctx := context.Background()
	nowMillis := testingFleetRemindNow.UnixMilli()

	tapped := newTestingFleetRemindClaim(t)
	tapped.authorized(t)
	tapped.chain.registered, tapped.server.status.Registered = false, false
	tapped.chain.block = 999
	state := tapped.claim.RefreshSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageNeedsTap)
	testingFleetExpectRemindAt(t, "refresh, a tapped claim", state, 0)
	state = tapped.claim.AdvanceSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingRegisterWindow)
	testingFleetExpectRemindAt(t, "advance, a tapped claim", state, nowMillis+testingFleetBlocksMillis(1))

	choosing := newTestingFleetRemindClaim(t)
	choosing.authorized(t)
	choosing.server.status.CurrentBlock = 5000
	choosing.server.status.NextEpochStartBlock = 5000 + 21600
	state = choosing.claim.RefreshSync(ctx)
	testingFleetExpectStage(t, state, SnFleetClaimStageChoosingDevices)
	testingFleetExpectRemindAt(t, "refresh, choosing", state, 0)
	if choosing.chain.wrote() || len(choosing.server.hotkeySigned) != 0 {
		t.Fatal("refresh wrote or signed")
	}
}

// The moment itself: a delay, pulled in to the lead before a deadline that comes sooner
// and is still ahead; a block the server didn't name is no moment.
func TestSnFleetClaimRemindBy(t *testing.T) {
	now := int64(1_800_000_000_000)
	hour := time.Hour.Milliseconds()
	for _, c := range []struct {
		name     string
		delay    time.Duration
		deadline int64
		want     int64
	}{
		{"no deadline", time.Hour, 0, now + hour},
		{"a deadline far off", time.Hour, now + 72*hour, now + hour},
		{"the last good moment sooner", 12 * time.Hour, now + 15*hour, now + 9*hour},
		{"the last good moment now", time.Hour, now + 6*hour, now + hour},
		{"the last good moment passed", time.Hour, now + 5*hour, now + hour},
		{"the deadline passed", 12 * time.Hour, now - hour, now + 12*hour},
	} {
		if got := snFleetClaimRemindBy(now, c.delay, c.deadline); got != c.want {
			t.Fatalf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if (snFleetClaimSchedule{}).blockMillis(10, now) != 0 || (snFleetClaimSchedule{CurrentBlock: 10}).blockMillis(0, now) != 0 {
		t.Fatal("a block not known made a moment")
	}
	if got := (snFleetClaimSchedule{CurrentBlock: 10}).blockMillis(310, now); got != now+hour {
		t.Fatalf("300 blocks: %d, want %d", got, now+hour)
	}
	for _, stage := range []string{
		SnFleetClaimStageClosed,
		SnFleetClaimStageNoConsent,
		SnFleetClaimStageNeedsTap,
		SnFleetClaimStageHotkeyElsewhere,
		SnFleetClaimStageNeedsWalletAuthorization,
		SnFleetClaimStageNeedsFunds,
		SnFleetClaimStageWaitingMirror,
		SnFleetClaimStageWaitingDevices,
		SnFleetClaimStageBound,
		SnFleetClaimStageFailed,
	} {
		if got := snFleetClaimRemindAt(stage, snFleetClaimSchedule{CurrentBlock: 10, RegisterAfterBlock: 20, RegisterBeforeBlock: 30, NextEpochStartBlock: 40}, now); got != 0 {
			t.Fatalf("%s: %d", stage, got)
		}
	}
}
