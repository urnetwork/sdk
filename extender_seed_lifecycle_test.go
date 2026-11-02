package sdk

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The seed bootstrap's lifecycle (extender_seed.go): one pass at a time for
// every caller, the backoff that bounds the mirrors, the cancellation an
// account change and a path change bring, the reset and the close, and the
// automatic trigger of a cold start with the dns bootstrap and the control
// route blocked (EXTENDER-UPDATE2.md §2.3, §2.4).

// One mirror entry for the lifecycle tests; its address is never dialed by
// them, whose mirror pass is a seam.
var testSeedMirrorConfig = ExtenderSeedMirrorValue{
	Url:     "https://mirror.seed.example",
	Address: "127.0.0.1:9",
}

// A mirror pass that counts its calls and answers what `answer` says, with
// the request and the pass's context.
type testSeedMirrorPass struct {
	calls  atomic.Int32
	answer func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult
}

func (self *testSeedMirrorPass) run(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
	self.calls.Add(1)
	return self.answer(ctx, request)
}

// A mirror pass answer that accepts and applies `envelope`, as connect's does.
func testSeedMirrorServes(t *testing.T, envelopeFunc func() []byte) func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
	return func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		envelope, err := connect.ParseExtenderSeedEnvelope(envelopeFunc())
		if err != nil {
			return &extenderSeedMirrorResult{code: connect.ExtenderSeedErrorCode(err)}
		}
		bundle, err := connect.VerifyExtenderSeed(envelope, request.rootKeys, request.networkHosts, request.now())
		if err != nil {
			return &extenderSeedMirrorResult{code: connect.ExtenderSeedErrorCode(err)}
		}
		if _, err := request.acceptor.Accept(bundle, request.rootKeys); err != nil {
			return &extenderSeedMirrorResult{code: connect.ExtenderSeedErrorCode(err)}
		}
		applyResult, _ := connect.ApplyExtenderSeed(request.directory, bundle)
		return &extenderSeedMirrorResult{bundle: bundle, apply: applyResult}
	}
}

// A seed test space whose mirror pass is `mirrorPass`, with no embedded bundle.
func newTestSeedLifecycleSpace(t *testing.T, mirrorPass *testSeedMirrorPass, configure func(*extenderSeedSettings)) (*NetworkSpace, *testSeedClock) {
	t.Helper()
	clock := newTestSeedClock()
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)
	testSeedInstallEmbedded(t, "")
	testSeedInstallMirrors(t, testSeedMirrorConfig)
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
		settings.MirrorPass = mirrorPass.run
		if configure != nil {
			configure(settings)
		}
	})
	return newTestSeedSpace(t, testSeedStoragePath(t)), clock
}

// Concurrent callers share one pass, and each sees its result.
func TestExtenderSeedConcurrentCallersShareOnePass(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var envelopeBytes []byte
	serve := testSeedMirrorServes(t, func() []byte { return envelopeBytes })
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		enterOnce.Do(func() { close(entered) })
		<-release
		return serve(ctx, request)
	}}
	space, clock := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	envelopeBytes = testSeedBytes(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{}))
	seed := space.getExtenderSeed()

	callerCount := 4
	results := make(chan bool, callerCount)
	go func() {
		results <- seed.obtainExtenderControlReach(context.Background())
	}()
	<-entered
	for range callerCount - 1 {
		go func() {
			results <- seed.obtainExtenderControlReach(context.Background())
		}()
	}
	// the joiners are waiting on the pass in flight; let them all arrive
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range callerCount {
		if !<-results {
			t.Error("a caller of the shared pass saw no reach")
		}
	}
	if calls := mirrorPass.calls.Load(); calls != 1 {
		t.Errorf("%d callers ran %d mirror passes", callerCount, calls)
	}
	// a later call is a new pass, with nothing new to add
	if seed.obtainExtenderControlReach(context.Background()) {
		t.Error("the same bundle again was reported as new reach")
	}
	if calls := mirrorPass.calls.Load(); calls != 2 {
		t.Errorf("the later call ran %d mirror passes in all", calls)
	}
}

// A mirror pass that found nothing leaves the mirrors in backoff, doubling,
// so a caller that retries does not refetch them; local sources are judged
// on every pass regardless.
func TestExtenderSeedMirrorBackoff(t *testing.T) {
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	space, clock := newTestSeedLifecycleSpace(t, mirrorPass, func(settings *extenderSeedSettings) {
		settings.MinBackoff = time.Minute
		settings.MaxBackoff = 4 * time.Minute
	})
	seed := space.getExtenderSeed()
	expectCalls := func(calls int32) {
		t.Helper()
		result := seed.awaitPass(context.Background())
		if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeFetch {
			t.Errorf("a pass with no bundle anywhere: %+v", result)
		}
		if got := mirrorPass.calls.Load(); got != calls {
			t.Fatalf("the mirrors were fetched %d times, not %d", got, calls)
		}
	}
	expectCalls(1)
	expectCalls(1)
	clock.advance(time.Minute)
	expectCalls(2)
	// the backoff doubled
	clock.advance(time.Minute)
	expectCalls(2)
	clock.advance(time.Minute)
	expectCalls(3)
	// and stops at the max
	clock.advance(4 * time.Minute)
	expectCalls(4)
	clock.advance(4 * time.Minute)
	expectCalls(5)
	// a path change starts over
	seed.networkChanged()
	expectCalls(6)
}

// A pass in flight ends when the bound identity changes, and its caller sees
// no reach; nothing it fetched is applied (§2.3). The change comes through the
// release binding as a device's does (extender_release_space.go): a token of
// another identity moves the account monitor, and a refresh of the same
// identity's does not.
func TestExtenderSeedAccountChangeCancelsThePass(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan error, 1)
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		close(entered)
		<-ctx.Done()
		ended <- ctx.Err()
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeCanceled}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	seed := space.getExtenderSeed()

	networkId := connect.NewId()
	clientId := connect.NewId()
	var byJwt atomic.Pointer[string]
	setByJwt := func(networkId connect.Id, clientId connect.Id) {
		value := testReleaseByJwt(t, networkId, clientId)
		byJwt.Store(&value)
	}
	setByJwt(networkId, clientId)
	unbind := space.bindExtenderRelease(func() string {
		return *byJwt.Load()
	})
	t.Cleanup(unbind)

	result := make(chan *extenderSeedPassResult, 1)
	go func() {
		result <- seed.awaitPass(context.Background())
	}()
	<-entered

	// a refresh of the same identity's token is no account change
	setByJwt(networkId, clientId)
	space.extenderReleaseCredentialChanged()
	select {
	case err := <-ended:
		t.Fatalf("a token refresh ended the pass with %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	setByJwt(networkId, connect.NewId())
	space.extenderReleaseCredentialChanged()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the account change ended the pass with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an account change did not cancel the pass")
	}
	if r := <-result; r.usable || r.outcome != ExtenderSeedPassOutcomeCanceled {
		t.Errorf("the canceled pass answered %+v", r)
	}
	if status := space.GetExtenderSeedStatus(); status.LastPassOutcome != ExtenderSeedPassOutcomeCanceled {
		t.Errorf("the status after the canceled pass is %+v", status)
	}
	testSeedRequireNothingApplied(t, space)
}

// A path change of the physical network ends the pass in flight; connect's
// process-wide notification reaches the bootstrap of every live space.
func TestExtenderSeedPathChangeCancelsThePass(t *testing.T) {
	entered := make(chan struct{}, 1)
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		entered <- struct{}{}
		<-ctx.Done()
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeCanceled}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	seed := space.getExtenderSeed()

	for _, change := range []func(){seed.networkChanged, connect.NetworkChanged} {
		result := make(chan *extenderSeedPassResult, 1)
		go func() {
			result <- seed.awaitPass(context.Background())
		}()
		<-entered
		change()
		select {
		case r := <-result:
			if r.usable || r.outcome != ExtenderSeedPassOutcomeCanceled {
				t.Errorf("the pass a path change ended answered %+v", r)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a path change did not cancel the pass")
		}
	}
}

// A caller whose own context ends gives up waiting; the pass goes on for the
// others, and the next caller joins it.
func TestExtenderSeedCallerDeadlineLeavesThePassRunning(t *testing.T) {
	release := make(chan struct{})
	var envelopeBytes []byte
	serve := testSeedMirrorServes(t, func() []byte { return envelopeBytes })
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		<-release
		return serve(ctx, request)
	}}
	space, clock := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	envelopeBytes = testSeedBytes(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{}))
	seed := space.getExtenderSeed()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if seed.obtainExtenderControlReach(ctx) {
		t.Fatal("a caller past its deadline saw reach")
	}
	joined := make(chan bool, 1)
	go func() {
		joined <- seed.obtainExtenderControlReach(context.Background())
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if !<-joined {
		t.Error("the caller that joined the pass saw no reach")
	}
	if calls := mirrorPass.calls.Load(); calls != 1 {
		t.Errorf("the two callers ran %d mirror passes", calls)
	}
}

// Puts the fixture key in the bundled table for the fixture network, as a
// production host's key is, so a reset that clears the configured keys
// returns the space to it.
func testSeedInstallBundledRootKey(t *testing.T) {
	t.Helper()
	previous, ok := bundledExtenderRootPublicKeyHexes[testSeedNetworkHost]
	bundledExtenderRootPublicKeyHexes[testSeedNetworkHost] = []string{testSeedRootPublicKeyHex()}
	t.Cleanup(func() {
		if ok {
			bundledExtenderRootPublicKeyHexes[testSeedNetworkHost] = previous
		} else {
			delete(bundledExtenderRootPublicKeyHexes, testSeedNetworkHost)
		}
	})
}

// "Reset extenders" clears the seed store with the directory: the floor goes,
// so an older bundle is accepted again, as on a fresh install (E7).
func TestExtenderSeedResetClearsTheStore(t *testing.T) {
	testSeedInstallBundledRootKey(t)
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	space, clock := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	seed := space.getExtenderSeed()
	newer := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 8})
	older := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 7})
	if result := space.ImportExtenderSeed(testSeedText(t, newer)); !result.Ok {
		t.Fatalf("the newer bundle: %+v", result)
	}
	if result := space.ImportExtenderSeed(testSeedText(t, older)); result.Error != ExtenderSeedImportErrorRollback {
		t.Fatalf("the older bundle before the reset: %+v", result)
	}

	space.ResetExtenders()

	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 0 {
		t.Errorf("after the reset the floor is %d, %v", sequence, err)
	}
	if status := space.GetExtenderSeedStatus(); status.Sequence != 0 || status.Source != "" {
		t.Errorf("after the reset the status is %+v", status)
	}
	if entry := testSeedDirectoryEntry(space, 0); entry != nil {
		t.Errorf("after the reset the directory holds %+v", entry)
	}
	if result := space.ImportExtenderSeed(testSeedText(t, older)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("the older bundle after the reset: %+v", result)
	}
}

// A reset that a constructor applies -- values that bring a reset this
// storage has not applied -- clears the store before the bootstrap starts.
func TestExtenderSeedStoredResetClearsTheStore(t *testing.T) {
	storagePath := testSeedStoragePath(t)
	space, clock, _ := newTestSeedImportSpace(t, storagePath)
	if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 4}))); !result.Ok {
		t.Fatalf("the bundle: %+v", result)
	}
	space.close()

	values := testSeedSpaceValues()
	values.ExtenderResetId = connect.NewId().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := newNetworkSpace(ctx, NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"}, values, storagePath)
	defer restarted.close()
	seed := restarted.getExtenderSeed()
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, restarted.extenderDirectory.RootKeys()); err != nil || sequence != 0 {
		t.Errorf("after a stored reset the floor is %d, %v", sequence, err)
	}
}

// A closed bootstrap runs no pass, its control reach answers at once, and its
// status stays readable.
func TestExtenderSeedClose(t *testing.T) {
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	seed := space.getExtenderSeed()
	space.close()
	if seed.obtainExtenderControlReach(context.Background()) {
		t.Error("a closed bootstrap reported reach")
	}
	if calls := mirrorPass.calls.Load(); calls != 0 {
		t.Errorf("a closed bootstrap ran %d mirror passes", calls)
	}
	seed.Close()
	_ = space.GetExtenderSeedStatus()
}

// With no trust anchor in force nothing can verify, and a pass says so rather
// than fetching what it could not judge.
func TestExtenderSeedWithoutRootKeys(t *testing.T) {
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	clock := newTestSeedClock()
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)
	testSeedInstallEmbedded(t, "")
	testSeedInstallMirrors(t, testSeedMirrorConfig)
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
		settings.MirrorPass = mirrorPass.run
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// a host the bundled table does not name, with no configured keys, and a
	// mirror, so a fetch would have somewhere to go
	space := newNetworkSpace(
		ctx,
		NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"},
		NetworkSpaceValues{ExtenderSeedMirrors: []ExtenderSeedMirrorValue{testSeedMirrorConfig}},
		testSeedStoragePath(t),
	)
	defer space.close()
	seed := space.getExtenderSeed()
	if status := space.GetExtenderSeedStatus(); status.MirrorCount != 1 {
		t.Fatalf("the space counts %d mirrors", status.MirrorCount)
	}
	result := seed.awaitPass(context.Background())
	if result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeNoRootKeys {
		t.Errorf("a pass with no root keys: %+v", result)
	}
	if calls := mirrorPass.calls.Load(); calls != 0 {
		t.Errorf("a pass with no root keys fetched %d times", calls)
	}
	envelope := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{})
	if r := space.ImportExtenderSeed(testSeedText(t, envelope)); r.Error != ExtenderSeedImportErrorNoRootKeys {
		t.Errorf("an import with no root keys: %+v", r)
	}
}

// Turns the network client on for the spaces a test builds, with the dns
// bootstrap blocked: every name fails to resolve, and only ipv4 has a host
// route. With `realHello` the client reads the operator's hello through the
// space's client strategy, which reaches nothing until a verified extender
// carries it; without, every hello fails.
func testSeedEnableBlockedNetworkClient(t *testing.T, realHello bool) {
	t.Helper()
	previousEnabled := SetExtenderNetworkClientEnabled(true)
	previousConfigure := extenderNetworkClientConfigure
	previousLoopDisabled := extenderSeedLoopDisabled.Swap(false)
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		blocked := errors.New("the dns bootstrap is blocked")
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, blocked
		}
		settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
			return nil, blocked
		}
		if !realHello {
			settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
				return nil, errors.New("the control route is blocked")
			}
		}
		settings.Hint = func(ctx context.Context) (*connect.ExtenderHintResult, error) {
			return nil, errors.New("the control route is blocked")
		}
		settings.ProbeWindowCount = 0
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	}
	t.Cleanup(func() {
		SetExtenderNetworkClientEnabled(previousEnabled)
		extenderNetworkClientConfigure = previousConfigure
		extenderSeedLoopDisabled.Store(previousLoopDisabled)
	})
}

// A cold start with the dns bootstrap blocked and the control route down: once
// the network client's first attempt ends with nothing usable, the trigger
// asks the control route, runs a pass when it does not answer, asks again
// through the seed's extender, and requests the gated release (§2.3).
func TestExtenderSeedTriggerRecoversAColdStart(t *testing.T) {
	testSeedEnableBlockedNetworkClient(t, false)
	var reachable atomic.Bool
	probeResults := make(chan bool, 16)
	// the trigger may act as soon as the space is built, so what the mirror
	// serves exists first
	envelopeBytes := testSeedBytes(t, testSeedEnvelope(t, time.Now(), testSeedBundleSpec{sequence: 21}))
	serve := testSeedMirrorServes(t, func() []byte { return envelopeBytes })
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		result := serve(ctx, request)
		// the seed's extender carries the control route from here
		reachable.Store(true)
		return result
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, func(settings *extenderSeedSettings) {
		settings.ControlProbe = func(ctx context.Context) error {
			answered := reachable.Load()
			select {
			case probeResults <- answered:
			default:
			}
			if answered {
				return nil
			}
			return errors.New("the control route does not answer")
		}
	})
	testSeedWaitFor(t, func() bool {
		return 0 < space.extenderReleaseRequestCount()
	})
	// asked once before the pass, which did not answer, and once after it,
	// through the seed, which did; the woken network client may hold the
	// seed's address afterwards (nothing serves it here) and the trigger may
	// ask again, which is not this test's concern
	if first, second := <-probeResults, <-probeResults; first || !second {
		t.Errorf("the control route was asked %v then %v, not unanswered then answered", first, second)
	}
	if calls := mirrorPass.calls.Load(); calls < 1 {
		t.Errorf("the cold start ran %d mirror passes", calls)
	}
	status := space.GetExtenderSeedStatus()
	if status.Sequence != 21 || status.Source != ExtenderSeedSourceMirror {
		t.Errorf("the status after the cold start is %+v", status)
	}
	if entry := testSeedDirectoryEntry(space, 0); entry == nil || entry.Source != connect.ExtenderSourceSeed {
		t.Errorf("the seed's record landed as %+v", entry)
	}
}

// When the operator answers directly the space needs no seed, and the trigger
// runs no pass however empty the directory is.
func TestExtenderSeedTriggerRunsNoPassWhileControlAnswers(t *testing.T) {
	testSeedEnableBlockedNetworkClient(t, false)
	var probes atomic.Int32
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, func(settings *extenderSeedSettings) {
		settings.ControlProbe = func(ctx context.Context) error {
			probes.Add(1)
			return nil
		}
	})
	testSeedWaitFor(t, func() bool {
		return 0 < probes.Load()
	})
	// the next probe is a recheck period away; nothing else runs meanwhile
	time.Sleep(100 * time.Millisecond)
	if calls := mirrorPass.calls.Load(); calls != 0 {
		t.Errorf("the trigger ran %d mirror passes while the operator answered", calls)
	}
	if count := probes.Load(); count != 1 {
		t.Errorf("the trigger asked the control route %d times", count)
	}
	if status := space.GetExtenderSeedStatus(); status.LastPassTime != 0 {
		t.Errorf("a pass ran: %+v", status)
	}
}

// Waits up to ten seconds for `done`.
func testSeedWaitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if deadline.Before(time.Now()) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A pass that runs into its own deadline failed, as one whose mirrors refused
// would have: the mirrors back off, so a caller that asks again does not fetch
// again at once, and the trigger backs off too. Only an account change, a path
// change, a reset or the close cancels a pass.
func TestExtenderSeedPassDeadlineIsAFailure(t *testing.T) {
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		// every mirror stalls until the pass ends, as connect's pass reports it
		<-ctx.Done()
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeCanceled}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, func(settings *extenderSeedSettings) {
		settings.PassTimeout = 100 * time.Millisecond
	})
	seed := space.getExtenderSeed()

	result := seed.awaitPass(context.Background())
	if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeFetch {
		t.Fatalf("a pass that ran into its deadline: %+v", result)
	}
	if status := space.GetExtenderSeedStatus(); status.LastPassOutcome != ExtenderSeedPassOutcomeFailed ||
		status.LastPassError != connect.ExtenderSeedCodeFetch {
		t.Errorf("the status after the deadline is %+v", status)
	}
	seed.awaitPass(context.Background())
	if calls := mirrorPass.calls.Load(); calls != 1 {
		t.Errorf("the mirrors were fetched %d times: the deadline left no backoff", calls)
	}
}

// A reset of the extender section holds passes off until the directory and the
// seed store are both reset: the pass in flight is ended and waited out first,
// so what it had in hand -- here a bundle it applies even after its cancel, as
// connect's pass does with one it already accepted -- never lands after the
// reset (E7). The operator's key is the host's bundled default here, so the
// reset directory still verifies the bundle: what a late apply would land.
func TestExtenderSeedResetHoldsThePassInFlight(t *testing.T) {
	testSeedInstallBundledRootKey(t)
	entered := make(chan struct{})
	var enterOnce sync.Once
	var envelopeBytes []byte
	serve := testSeedMirrorServes(t, func() []byte { return envelopeBytes })
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		enterOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return serve(context.Background(), request)
	}}
	space, clock := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	envelopeBytes = testSeedBytes(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 4}))
	seed := space.getExtenderSeed()

	result := make(chan *extenderSeedPassResult, 1)
	go func() {
		result <- seed.awaitPass(context.Background())
	}()
	<-entered
	space.ResetExtenders()
	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("the reset did not end the pass in flight")
	}

	if rootKeys := space.extenderDirectory.RootKeys(); rootKeys == nil || rootKeys.Len() == 0 {
		t.Fatal("the reset directory verifies nothing, so no late apply could land")
	}
	testSeedRequireNothingApplied(t, space)
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 0 {
		t.Errorf("after the reset the floor is %d, %v", sequence, err)
	}
	if seed.suspended() {
		t.Error("the reset left the bootstrap held")
	}
	if calls := mirrorPass.calls.Load(); calls != 1 {
		t.Errorf("the mirrors were fetched %d times", calls)
	}
}

// The seed bootstrap is one of the space's control reaches
// (addExtenderControlReach): the release fetch's walk reaches it when nothing
// cached gives reach, and no longer once the bootstrap is closed.
func TestExtenderSeedIsAControlReachOfTheSpace(t *testing.T) {
	envelope := testSeedEnvelope(t, time.Now(), testSeedBundleSpec{sequence: 6})
	space, _, _ := newTestSeedEmbeddedSpace(t, testSeedText(t, envelope)+"\n")
	if !space.obtainExtenderControlReach(context.Background()) {
		t.Fatal("the walk of the space's control reaches did not reach the seed")
	}
	if entry := testSeedDirectoryEntry(space, 0); entry == nil || entry.Source != connect.ExtenderSourceSeed {
		t.Errorf("the seed's record landed as %+v", entry)
	}
	// nothing new a second time
	if space.obtainExtenderControlReach(context.Background()) {
		t.Error("the walk reported the same record as new reach")
	}
	space.closeExtenderSeed()
	if space.obtainExtenderControlReach(context.Background()) {
		t.Error("the walk reached a closed bootstrap")
	}
}
