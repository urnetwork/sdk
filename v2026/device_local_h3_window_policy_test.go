//go:build !ios

package sdk

import (
	"context"
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

func TestDeviceH3WindowPolicyTracksLiveModeAndColdReconnect(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		device := newH3PolicyDevice(t, target)
		device.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
		location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
		if err := device.SetConnectLocationChecked(location); err != nil {
			t.Fatal(err)
		}
		multi := requireH3PolicyTarget(t, device, 2)
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
				want = 2
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
		if requireH3PolicyTarget(t, device, 2) == multi ||
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
		requireH3PolicyTarget(t, device, 2)
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
		if requireH3PolicyTarget(t, second, 2) == nil ||
			second.GetWindowStatus().ConnectionGeneration != secondGeneration {
			t.Fatal("one owner's profile/mode change affected another")
		}
	}
	first.SetPerformanceProfile(nil)
	requireH3PolicyTarget(t, first, 2)
	if err := first.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.ctx.Err() != nil {
		t.Fatal("closing one owner canceled another")
	}
	requireH3PolicyTarget(t, second, 2)
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
		// No public graph fits here. One remains a positive unready target,
		// never a claim that an empty class can satisfy readiness.
		{bytes: 8 * 1024 * 1024, slots: 16, want: 1, providerMode: TransportModeH3},
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

// The matched v1 control adds only this nil-by-default observation seam at
// its old query boundary. TryLock witnesses the held lock without deadlocking
// the test; no query or callback is replaced with a model of the policy.
func TestDeviceH3WindowCompositionReleasesDeviceStateLock(t *testing.T) {
	device := newH3PolicyDevice(t, 32*1024*1024)
	observations := 0
	device.testingBeforeH3WindowComposition = func() {
		observations++
		if device.stateLock.TryLock() {
			device.stateLock.Unlock()
		} else {
			t.Error("H3 graph composition still holds DeviceLocal.stateLock")
		}
		if device.preferenceMutationLock.TryLock() {
			device.preferenceMutationLock.Unlock()
			t.Error("H3 graph composition lost preference publication ownership")
		}
	}
	defer func() { device.testingBeforeH3WindowComposition = nil }()
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
	requireH3PolicyTarget(t, device, 2)
}

// Public setters and initial destination construction share the existing
// preference lock through the real multi-client cap and carrier publication.
// Holding the first composition forces a later setter to publish last.
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
			want := 2
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
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseFirst)
		observations := 0
		device.testingBeforeH3WindowComposition = func() {
			observations++
			if observations != 1 {
				return
			}
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
		firstDone := make(chan error, 1)
		go func() {
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
		var observation boundary
		select {
		case observation = <-entered:
		case <-watchdog.Done():
			releaseFirst()
			stop()
			t.Fatalf("%s never reached the actual composition boundary", kind)
		}
		secondStarted := make(chan struct{})
		secondDone := make(chan error, 1)
		go func() {
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
			case <-watchdog.Done():
				stop()
				t.Fatalf("%s setter did not join", kind)
			}
		}
		stop()
		device.testingBeforeH3WindowComposition = nil
		if !observation.stateAvailable || !observation.preferenceHeld || observations != 2 {
			t.Fatalf("%s boundary=%+v observations=%d want=2", kind, observation, observations)
		}
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
	}
}
