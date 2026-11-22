//go:build !ios

package sdk

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Discovery is held by the owning context, before any identity, carrier or
// provider can be created. All changes below enter the real SDK destination
// constructor and live policy setter; callbacks witness resize completion.
type heldH3PolicyDiscovery struct{}

func (*heldH3PolicyDiscovery) FindProviders2(ctx context.Context, _ string, _ *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func newH3PolicyDevice(t *testing.T, target ByteCount) *DeviceLocal {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	return device
}

// Monitor callbacks, not sleeps, witness the actual production resize loop.
func requireH3PolicyTarget(t *testing.T, device *DeviceLocal, want int) *connect.RemoteUserNatMultiClient {
	t.Helper()
	device.stateLock.Lock()
	multi, ok := device.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
	device.stateLock.Unlock()
	if !ok {
		t.Fatal("real SDK destination did not construct a multi-client")
	}
	changed := connect.NewMonitor()
	unsubscribe := multi.Monitor().AddMonitorEventCallback(func(*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
		changed.NotifyAll()
	})
	defer unsubscribe()
	// An already owned expansion retains its normal 15-second acquisition
	// budget before the next resize observes a setter; do not shorten it.
	watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
	defer stop()
	for {
		update := changed.NotifyChannel()
		status := device.GetWindowStatus()
		if status.TargetSize == want {
			if status.MinSatisfied || status.ProviderStateAdded != 0 {
				t.Fatal("held discovery was reported as a usable provider")
			}
			return multi
		}
		select {
		case <-update:
		case <-watchdog.Done():
			t.Fatalf("resize watchdog: target=%d want=%d", status.TargetSize, want)
		}
	}
}

// The window target of a device with no provider and held discovery under
// its h3 limit. Since R20 the limit is the claims its carrier budget fits,
// nine at 32 MiB and eighteen at 64 (one before, for a target of two), which
// no longer narrows the windows: the quality window keeps its 5 at 32 MiB and
// 10 at 64, the speed window its 1 and 2.
func h3PolicyWindowTarget(target ByteCount) int {
	switch target {
	case 32 * 1024 * 1024:
		return 5 + 1
	case 64 * 1024 * 1024:
		return 10 + 2
	default:
		panic(fmt.Sprintf("no h3 window target pinned for %d", target))
	}
}

func TestDeviceH3WindowPolicyTracksLiveModeAndColdReconnect(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		device := newH3PolicyDevice(t, target)
		device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
		if err := device.SetConnectLocationChecked(location); err != nil {
			t.Fatal(err)
		}
		multi := requireH3PolicyTarget(t, device, h3PolicyWindowTarget(target))
		generation := device.GetWindowStatus().ConnectionGeneration
		device.stateLock.Lock()
		generator, owner := device.apiMultiClientGenerator, device.platformTransportBudget
		device.stateLock.Unlock()
		if generator == nil {
			t.Fatal("fixture bypassed the built-in API generator")
		}
		baseline := mobileTargetScaledCount(mobileQualityWindowSize, target) +
			mobileTargetScaledCount(mobileSpeedWindowSize, target)
		for _, mode := range []TransportMode{TransportModeH1, TransportModeAuto, TransportModeH3} {
			device.SetTransportSettings(&TransportSettings{Mode: mode})
			want := baseline
			if mode == TransportModeH3 {
				want = h3PolicyWindowTarget(target)
			}
			if requireH3PolicyTarget(t, device, want) != multi ||
				device.GetWindowStatus().ConnectionGeneration != generation ||
				device.apiMultiClientGenerator != generator || device.platformTransportBudget != owner {
				t.Fatal("mode migration replaced identity, generation or the carrier owner")
			}
		}
		if err := device.SetConnectLocationChecked(nil); err != nil {
			t.Fatal(err)
		}
		join, stop := context.WithTimeout(t.Context(), 5*time.Second)
		if err := multi.CloseAndWait(join); err != nil {
			t.Fatal(err)
		}
		stop()
		if stats := owner.Stats(); stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
			stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("disconnected owner retained capacity: %+v", stats)
		}
		if err := device.SetConnectLocationChecked(location); err != nil {
			t.Fatal(err)
		}
		if requireH3PolicyTarget(t, device, h3PolicyWindowTarget(target)) == multi ||
			device.GetWindowStatus().ConnectionGeneration <= generation {
			t.Fatal("cold reconnect reused a retired window generation")
		}
	}
}

func TestDeviceH3WindowPolicyPreservesExplicitProfileAndOtherOwner(t *testing.T) {
	first := newH3PolicyDevice(t, 32*1024*1024)
	second := newH3PolicyDevice(t, 64*1024*1024)
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	for _, device := range []*DeviceLocal{first, second} {
		device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		if err := device.SetConnectLocationChecked(location); err != nil {
			t.Fatal(err)
		}
		requireH3PolicyTarget(t, device, h3PolicyWindowTarget(device.settings.MemoryTargetByteCount))
	}
	secondGeneration := second.GetWindowStatus().ConnectionGeneration
	for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		profile := &PerformanceProfile{WindowType: kind, WindowSize: &WindowSizeSettings{
			WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3, WindowSizeReconnectScale: 1,
		}}
		first.SetPerformanceProfile(profile)
		requireH3PolicyTarget(t, first, 3)
		if !performanceProfileValuesEqual(first.GetPerformanceProfile(), profile) {
			t.Fatal("H3 capacity silently rewrote an explicit performance profile")
		}
		first.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
		requireH3PolicyTarget(t, first, 3)
		first.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		requireH3PolicyTarget(t, first, 3)
		if requireH3PolicyTarget(t, second, h3PolicyWindowTarget(64*1024*1024)) == nil ||
			second.GetWindowStatus().ConnectionGeneration != secondGeneration {
			t.Fatal("one owner's profile/mode change affected another")
		}
	}
	first.SetPerformanceProfile(nil)
	requireH3PolicyTarget(t, first, h3PolicyWindowTarget(32*1024*1024))
	if err := first.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.ctx.Err() != nil {
		t.Fatal("closing one owner canceled another")
	}
	requireH3PolicyTarget(t, second, h3PolicyWindowTarget(64*1024*1024))
}

// The persistent control reserve is derived from configured family topology,
// never the instantaneous Used/Free counters. Test capacities here are owned
// constructor fixtures, not a request to raise either production profile.
func TestDeviceH3WindowPolicyReservesConfiguredControlGraphs(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.DisableLogging = true
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	settings.ProviderDialContextSettings = &connect.DialContextSettings{
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return nil, context.Canceled },
		PacketConnFactory: func(context.Context) (net.PacketConn, error) { return nil, context.Canceled },
	}
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	device.SetProvideMode(ProvideModeNone)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
	if device.provider == nil {
		t.Fatal("real constructor omitted the provider control owner")
	}
	if device.GetProviderTransportSettings().Mode != TransportModeAuto {
		t.Fatal("client-only H3 selection changed the fresh provider Auto policy")
	}
	transferCapacity := device.transferMemory.root.TotalByteCount()
	// Join all actual producer work before substituting finite capacities for
	// this calculation-only boundary; no destination or dial uses these roots.
	if err := device.provider.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The constructor used only loopback. The joined producer now provides
	// the configured three-path shape for this pure accounting boundary.
	device.provider.platformUrlV4 = "wss://control-v4.example"
	device.provider.platformUrlV6 = "wss://control-v6.example"
	originalOwner := device.platformTransportBudget
	defer func() {
		device.stateLock.Lock()
		device.platformTransportBudget = originalOwner
		device.stateLock.Unlock()
	}()
	base := newDeviceLocalPlatformTransportSettingsForProfile(settings.MemoryTargetByteCount,
		nil, nil, "", "", true, nil)
	claim := base.H3MemoryReservationByteCount(connect.TransportModeH3, device.clientStrategy)
	control := 3 * base.H1MemoryReservationByteCount()
	for _, c := range []struct {
		bytes        connect.ByteCount
		slots        int
		want         int
		providerMode TransportMode
	}{
		{bytes: 3*claim + control - 1, slots: 16, want: 2, providerMode: TransportModeAuto},
		{bytes: 3*claim + control, slots: 16, want: 3, providerMode: TransportModeAuto},
		{bytes: 3*claim + control, slots: 5, want: 2, providerMode: TransportModeH1},
		{bytes: claim - 1, slots: 16, want: 1, providerMode: TransportModeAuto},
		// Explicit provider H3 is not a reclaimable Auto lease: all three
		// persistent control graphs remain reserved before public windows.
		{bytes: 6*claim - 1, slots: 16, want: 2, providerMode: TransportModeH3},
		{bytes: 6 * claim, slots: 16, want: 3, providerMode: TransportModeH3},
		// No public graph fits here. One remains a positive target, which
		// the budget may still admit beside a held standby or carries on h1,
		// never a claim that an empty class can satisfy readiness.
		{bytes: 4*claim - 1, slots: 16, want: 1, providerMode: TransportModeH3},
	} {
		device.preferenceMutationLock.Lock()
		device.stateLock.Lock()
		device.providerTransportSettings = normalizeTransportSettings(&TransportSettings{Mode: c.providerMode}, true)
		device.platformTransportBudget = connect.NewPlatformTransportBudget(c.bytes, c.slots)
		device.stateLock.Unlock()
		got := device.publicH3WindowSizeLimitWithLock()
		device.preferenceMutationLock.Unlock()
		if got != c.want {
			t.Fatalf("control reserve bytes=%d slots=%d got=%d want=%d", c.bytes, c.slots, got, c.want)
		}
	}
	if device.transferMemory.root.TotalByteCount() != transferCapacity {
		t.Fatal("carrier sizing changed the transfer memory root")
	}
}

// R20: the window limit at phone targets, from the phone policy's own claims
// and each target's real carrier budget, with no provider, a provider on Auto
// (h1 control paths) and a provider on h3, each with its v4, v6 and standby
// paths. Before R20 every 20- to 64-MiB target composed a 3- to 9.6-MiB claim
// that only one transport fit beside no control path (two at 24 MiB), and
// none beside a provider on h3. Every claim is now the phone sizes', 923,532
// bytes, so the budget holds a carrier for each of six to ten peers beside
// the provider's three h3 paths from 32 MiB, and a phone's budget has the
// slots for them: 32 at 64 MiB.
func TestDeviceH3WindowLimitAtPhoneTargets(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	for _, c := range []struct {
		target ByteCount
		// limits with no provider, the provider on Auto and the provider on h3
		none, auto, h3 int
	}{
		{20 * 1024 * 1024, 5, 3, 2},
		{24 * 1024 * 1024, 6, 4, 3},
		{32 * 1024 * 1024, 9, 6, 6},
		{48 * 1024 * 1024, 13, 10, 10},
		{64 * 1024 * 1024, 18, 14, 15},
	} {
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.MemoryTargetByteCount = c.target
		settings.DisableLogging = true
		settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
		settings.ProviderDialContextSettings = &connect.DialContextSettings{
			DialContext:       func(context.Context, string, string) (net.Conn, error) { return nil, context.Canceled },
			PacketConnFactory: func(context.Context) (net.PacketConn, error) { return nil, context.Canceled },
		}
		device := newSettingsOwnershipDevice(t, fixture, settings, true)
		device.SetProvideMode(ProvideModeNone)
		device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		provider := device.provider
		if provider == nil {
			t.Fatal("real constructor omitted the provider control owner")
		}
		// join the provider's producers before giving it the three-path shape
		// this pure accounting boundary reads
		if err := provider.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		provider.platformUrlV4 = "wss://control-v4.example"
		provider.platformUrlV6 = "wss://control-v6.example"

		base := newDeviceLocalPlatformTransportSettingsForProfile(c.target, nil, nil, "", "", true, nil)
		claim := base.H3MemoryReservationByteCount(connect.TransportModeH3, device.clientStrategy)
		h1 := base.H1MemoryReservationByteCount()
		total := device.platformTransportBudget.Stats().TotalByteCount
		slots := device.platformTransportBudget.Stats().MaxTransportCount
		if total != c.target/4 || claim != 923532 || slots != max(16, int(total/(512*1024))) {
			t.Fatalf("target %d: budget %d with %d slots and claim %d, want a quarter of the target, a slot a 512 KiB and the phone sizes' claim",
				c.target, total, slots, claim)
		}
		// the arithmetic the limit must agree with: what fits beside the
		// configured control paths, and one where nothing fits
		want := func(controlBytes connect.ByteCount) int {
			return max(1, int(max(0, total-controlBytes)/claim))
		}
		if want(0) != c.none || want(3*h1) != c.auto || want(3*claim) != c.h3 {
			t.Fatalf("target %d: claims fit %d/%d/%d, want %d/%d/%d", c.target,
				want(0), want(3*h1), want(3*claim), c.none, c.auto, c.h3)
		}
		limit := func(providerMode TransportMode, withProvider bool) int {
			device.preferenceMutationLock.Lock()
			defer device.preferenceMutationLock.Unlock()
			func() {
				device.stateLock.Lock()
				defer device.stateLock.Unlock()
				device.providerTransportSettings = normalizeTransportSettings(&TransportSettings{Mode: providerMode}, true)
				device.provider = nil
				if withProvider {
					device.provider = provider
				}
			}()
			return device.publicH3WindowSizeLimitWithLock()
		}
		none := limit(TransportModeAuto, false)
		auto := limit(TransportModeAuto, true)
		h3 := limit(TransportModeH3, true)
		if none != c.none || auto != c.auto || h3 != c.h3 {
			t.Fatalf("target %d: window limits %d/%d/%d, want %d/%d/%d", c.target, none, auto, h3, c.none, c.auto, c.h3)
		}
		t.Logf("target %d: budget %d, h3 claim %d, h1 claim %d, limits none %d, auto %d, h3 %d",
			c.target, total, claim, h1, none, auto, h3)
	}
}

// A public getter must finish before composition resumes. Unlike TryLock,
// completion tolerates unrelated state owners and still detects a publisher
// retaining stateLock across the real external-query boundary.
func TestDeviceH3WindowCompositionReleasesDeviceStateLock(t *testing.T) {
	device := newH3PolicyDevice(t, 32*1024*1024)
	watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
	defer stop()
	var getterDones []chan *TransportSettings
	t.Cleanup(func() {
		for _, done := range getterDones {
			for range done {
			}
		}
		device.stateLock.Lock()
		device.testingBeforeH3WindowComposition = nil
		device.stateLock.Unlock()
	})
	observations := 0
	device.stateLock.Lock()
	device.testingBeforeH3WindowComposition = func() {
		observations++
		done := make(chan *TransportSettings, 1)
		getterDones = append(getterDones, done)
		go func() {
			defer close(done)
			done <- device.GetTransportSettings()
		}()
		select {
		case settings := <-done:
			if settings.Mode != TransportModeH3 {
				t.Error("composition getter did not observe the current H3 policy")
			}
			for range done {
			}
		case <-watchdog.Done():
			t.Error("H3 composition prevented a public device-state getter from completing")
		}
		if device.preferenceMutationLock.TryLock() {
			device.preferenceMutationLock.Unlock()
			t.Error("H3 graph composition lost preference publication ownership")
		}
	}
	device.stateLock.Unlock()
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
	if err := device.SetConnectLocationChecked(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}); err != nil {
		t.Fatal(err)
	}
	device.SetProviderTransportSettings(&TransportSettings{Mode: TransportModeH3})
	if observations != 3 {
		t.Fatalf("client/destination/provider composition observations=%d want=3", observations)
	}
	requireH3PolicyTarget(t, device, h3PolicyWindowTarget(32*1024*1024))
}

// Public setters and initial destination construction share the existing
// preference lock through the real multi-client cap and carrier publication.
// A foreign state owner makes TryLock fail at the first held composition;
// getter completion still proves that composition released its own state lock.
func TestDeviceH3WindowCompositionSerializesPolicyPublication(t *testing.T) {
	for _, kind := range []string{"client", "provider", "destination"} {
		device := newH3PolicyDevice(t, 32*1024*1024)
		location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
		if kind != "client" {
			device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		}
		if kind != "destination" {
			if err := device.SetConnectLocationChecked(location); err != nil {
				t.Fatal(err)
			}
		}
		var priorMulti *connect.RemoteUserNatMultiClient
		var priorGeneration int64
		if kind != "destination" {
			want := h3PolicyWindowTarget(32 * 1024 * 1024)
			if kind == "client" {
				want = mobileTargetScaledCount(mobileQualityWindowSize, 32*1024*1024) +
					mobileTargetScaledCount(mobileSpeedWindowSize, 32*1024*1024)
			}
			priorMulti = requireH3PolicyTarget(t, device, want)
			priorGeneration = device.GetWindowStatus().ConnectionGeneration
		}
		type boundary struct {
			stateAvailable bool
			preferenceHeld bool
		}
		entered := make(chan boundary, 1)
		compositionStarted := make(chan struct{})
		probeState := make(chan struct{})
		releaseState := make(chan struct{})
		release := make(chan struct{})
		var probeOnce, stateOnce, releaseOnce sync.Once
		probeFirst := func() { probeOnce.Do(func() { close(probeState) }) }
		releaseOtherState := func() { stateOnce.Do(func() { close(releaseState) }) }
		releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
		firstDone := make(chan error, 1)
		var secondDone chan error
		var stateDone chan struct{}
		var getterDone chan *TransportSettings
		t.Cleanup(func() {
			probeFirst()
			releaseOtherState()
			releaseFirst()
			for range firstDone {
			}
			if secondDone != nil {
				for range secondDone {
				}
			}
			if stateDone != nil {
				<-stateDone
			}
			if getterDone != nil {
				for range getterDone {
				}
			}
			device.stateLock.Lock()
			device.testingBeforeH3WindowComposition = nil
			device.stateLock.Unlock()
		})
		observations := 0
		device.stateLock.Lock()
		device.testingBeforeH3WindowComposition = func() {
			observations++
			if observations != 1 {
				return
			}
			close(compositionStarted)
			<-probeState
			stateAvailable := device.stateLock.TryLock()
			if stateAvailable {
				device.stateLock.Unlock()
			}
			preferenceAvailable := device.preferenceMutationLock.TryLock()
			if preferenceAvailable {
				device.preferenceMutationLock.Unlock()
			}
			entered <- boundary{stateAvailable: stateAvailable, preferenceHeld: !preferenceAvailable}
			<-release
		}
		device.stateLock.Unlock()
		go func() {
			defer close(firstDone)
			switch kind {
			case "client":
				device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
			case "provider":
				device.SetProviderTransportSettings(&TransportSettings{Mode: TransportModeH3})
			case "destination":
				firstDone <- device.SetConnectLocationChecked(location)
				return
			}
			firstDone <- nil
		}()
		watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
		defer stop()
		select {
		case <-compositionStarted:
		case <-watchdog.Done():
			t.Fatalf("%s never started the actual composition boundary", kind)
		}
		stateHeld := make(chan struct{})
		stateDone = make(chan struct{})
		go func() {
			defer close(stateDone)
			device.stateLock.Lock()
			close(stateHeld)
			<-releaseState
			device.stateLock.Unlock()
		}()
		select {
		case <-stateHeld:
		case <-watchdog.Done():
			t.Fatalf("%s composition prevented the competing state owner from entering", kind)
		}
		// The old one-shot observation now sees a known foreign owner. Its
		// false result cannot identify which goroutine holds the state lock.
		probeFirst()
		var observation boundary
		select {
		case observation = <-entered:
		case <-watchdog.Done():
			releaseFirst()
			stop()
			t.Fatalf("%s never reached the actual composition boundary", kind)
		}
		releaseOtherState()
		select {
		case <-stateDone:
		case <-watchdog.Done():
			t.Fatalf("%s competing state owner did not join", kind)
		}
		getterDone = make(chan *TransportSettings, 1)
		go func() {
			defer close(getterDone)
			getterDone <- device.GetTransportSettings()
		}()
		select {
		case settings := <-getterDone:
			if settings.Mode != TransportModeH3 {
				t.Fatalf("%s getter did not observe the held H3 publication", kind)
			}
			for range getterDone {
			}
		case <-watchdog.Done():
			t.Fatalf("%s state getter did not finish while composition remained held", kind)
		}
		secondStarted := make(chan struct{})
		secondDone = make(chan error, 1)
		go func() {
			defer close(secondDone)
			close(secondStarted)
			device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
			secondDone <- nil
		}()
		<-secondStarted
		// Ownership, not a short negative timeout, proves the second operation
		// cannot pass its public preference admission before the first leaves.
		releaseFirst()
		for _, done := range []chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
				for range done {
				}
			case <-watchdog.Done():
				stop()
				t.Fatalf("%s setter did not join", kind)
			}
		}
		stop()
		device.stateLock.Lock()
		device.testingBeforeH3WindowComposition = nil
		device.stateLock.Unlock()
		baseline := mobileTargetScaledCount(mobileQualityWindowSize, 32*1024*1024) +
			mobileTargetScaledCount(mobileSpeedWindowSize, 32*1024*1024)
		multi := requireH3PolicyTarget(t, device, baseline)
		if device.GetTransportSettings().Mode != TransportModeH1 {
			t.Fatalf("%s did not retain the later client policy", kind)
		}
		generation := device.GetWindowStatus().ConnectionGeneration
		if priorMulti != nil && (multi != priorMulti || generation != priorGeneration) {
			t.Fatalf("%s migration replaced the current destination", kind)
		}
		if priorMulti == nil && generation == 0 {
			t.Fatal("initial destination did not publish its own generation")
		}
		if err := device.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Logf("h3_composition kind=%s stateAvailable=%t preferenceHeld=%t getterCompletedWhilePublicationHeld=true observations=%d deviceAndTestWorkersJoined=true", kind, observation.stateAvailable, observation.preferenceHeld, observations)
		if observation.stateAvailable || !observation.preferenceHeld || observations != 2 {
			t.Fatalf("H3_COMPOSITION_STATE_WITNESS: %s boundary=%+v observations=%d want=2 with a competing state owner", kind, observation, observations)
		}
	}
}

// Only a phone's carrier budget takes a slot a 512 KiB (R20): a desktop
// device at the same target keeps its 16 slots, with H3 claims too large for
// them to bind.
func TestDeviceDesktopCarrierBudgetKeepsSixteenSlots(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 64 * 1024 * 1024
	settings.LowMemoryProfile = false
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	device := newSettingsOwnershipDevice(t, fixture, settings, false)
	if stats := device.platformTransportBudget.Stats(); stats.TotalByteCount != 16*1024*1024 || stats.MaxTransportCount != 16 {
		t.Fatalf("desktop carrier budget %+v, want 16 MiB and 16 slots", stats)
	}
}
