//go:build !ios

// The hosted window policy (owner, 2026-11-13: "hosted proxies should use
// main window as the limited window, so that dualproto and dualstack
// requirements are not applied on the main window for hosted proxies, and
// there is no need for a separate limited window"): the device settings that
// carry it into every multi client the device builds, and the developer
// override of the limited window's fraction, which must not give such a
// device the window back.
package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The selectors the proxy service sets for its hosted devices.
func hostedWindowSelectorsTestSelectors() *connect.ProviderSelectors {
	return &connect.ProviderSelectors{
		Ip:       connect.ProviderIpCapabilityIpv4,
		Protocol: connect.ProviderProtocolCapabilityAny,
	}
}

// Records each find-providers2 request and holds it until its context ends,
// so a window asks once and waits, as against a platform that has not
// answered yet.
type hostedWindowSelectorsTestDiscovery struct {
	stateLock sync.Mutex
	requests  []connect.FindProviders2Args
	selects   []connect.FindProvidersSelect
	asked     *connect.Monitor
}

func newHostedWindowSelectorsTestDiscovery() *hostedWindowSelectorsTestDiscovery {
	return &hostedWindowSelectorsTestDiscovery{
		asked: connect.NewMonitor(),
	}
}

func (self *hostedWindowSelectorsTestDiscovery) FindProviders2(ctx context.Context, _ string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, *args)
		providerSelect := connect.FindProvidersSelect{}
		if args.Select != nil {
			providerSelect = *args.Select
		}
		self.selects = append(self.selects, providerSelect)
		self.asked.NotifyAll()
	}()
	<-ctx.Done()
	return nil, ctx.Err()
}

// The requests recorded so far, each with its select (zero for none).
func (self *hostedWindowSelectorsTestDiscovery) recorded() ([]connect.FindProviders2Args, []connect.FindProvidersSelect) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]connect.FindProviders2Args{}, self.requests...), append([]connect.FindProvidersSelect{}, self.selects...)
}

// No device setting changes a window's selectors or limited window by
// default, so apps keep connect's; the hosted settings carry ipv4 and any and
// a fraction of 0 into the multi client's settings, applied after the
// developer override so that they outrank it.
func TestDeviceLocalWindowClientSettingsCarryTheHostedPolicy(t *testing.T) {
	for _, deviceLocalSettings := range []*DeviceLocalSettings{DefaultDeviceLocalSettings(), DefaultLowMemoryDeviceLocalSettings()} {
		if deviceLocalSettings.NormalWindowSelectors != nil || deviceLocalSettings.DisableLimitedWindow {
			t.Errorf("default device settings change the windows: selectors %v, limited window disabled %t", deviceLocalSettings.NormalWindowSelectors, deviceLocalSettings.DisableLimitedWindow)
		}
		settings := connect.DefaultMultiClientSettings()
		// a developer override applied before the host's policy
		settings.LimitedWindowSizeFraction = 0.25
		applyDeviceLocalWindowClientSettings(settings, deviceLocalSettings)
		connect.AssertEqual(t, settings.NormalWindowSelectors, connect.DefaultMultiClientSettings().NormalWindowSelectors)
		connect.AssertEqual(t, settings.LimitedWindowSizeFraction, 0.25)
	}

	hosted := DefaultLowMemoryDeviceLocalSettings()
	hosted.NormalWindowSelectors = hostedWindowSelectorsTestSelectors()
	hosted.DisableLimitedWindow = true
	settings := connect.DefaultMultiClientSettings()
	settings.LimitedWindowSizeFraction = 0.75
	applyDeviceLocalWindowClientSettings(settings, hosted)
	connect.AssertEqual(t, settings.NormalWindowSelectors, *hostedWindowSelectorsTestSelectors())
	connect.AssertEqual(t, settings.LimitedWindowSizeFraction, 0.0)
}

// A device whose host disabled the limited window reads a fraction of 0,
// disconnected and connected, and the developer override's set and reset
// change neither the live window nor the next. Every window it builds asks
// find-providers2 with ipv4 and any and no ipv6 minimum.
func TestDisabledLimitedWindowIgnoresTheDeveloperOverride(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	discovery := newHostedWindowSelectorsTestDiscovery()
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = discovery
	settings.NormalWindowSelectors = hostedWindowSelectorsTestSelectors()
	settings.DisableLimitedWindow = true
	device := newSettingsOwnershipDevice(t, fixture, settings, true)

	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.0)
	device.SetLimitedWindowSizeFraction(1)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.0)
	device.ResetLimitedWindowSizeFraction()
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.0)

	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	first := requireLimitedWindowTestMulti(t, device)
	connect.AssertEqual(t, first.LimitedWindowSizeFraction(), 0.0)
	device.SetLimitedWindowSizeFraction(1)
	connect.AssertEqual(t, first.LimitedWindowSizeFraction(), 0.0)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.0)
	device.ResetLimitedWindowSizeFraction()
	connect.AssertEqual(t, first.LimitedWindowSizeFraction(), 0.0)
	connect.AssertEqual(t, first.LimitedWindowTargetSize(), 0)

	// both normal windows ask, each with the hosted selectors
	watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
	defer stop()
	for {
		update := discovery.asked.NotifyChannel()
		requests, _ := discovery.recorded()
		rankModes := map[string]bool{}
		for _, request := range requests {
			rankModes[request.RankMode] = true
		}
		if rankModes["quality"] && rankModes["speed"] {
			break
		}
		select {
		case <-update:
		case <-watchdog.Done():
			t.Fatalf("the windows asked with rank modes %v, want quality and speed", rankModes)
		}
	}
	requests, selects := discovery.recorded()
	for i, request := range requests {
		if request.Select == nil {
			t.Errorf("request %d (%s) asked in legacy mode", i, request.RankMode)
			continue
		}
		providerSelect := selects[i]
		if providerSelect.Ip != connect.ProviderIpCapabilityIpv4 || providerSelect.Protocol != connect.ProviderProtocolCapabilityAny || providerSelect.DualstackTargetCount != 0 {
			t.Errorf("request %d (%s) asked %+v, want ipv4 and any with no ipv6 minimum", i, request.RankMode, providerSelect)
		}
	}

	// the next window too
	disconnectLimitedWindowTestDevice(t, device, first)
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	second := requireLimitedWindowTestMulti(t, device)
	if second == first {
		t.Fatal("the reconnect kept the retired window")
	}
	connect.AssertEqual(t, second.LimitedWindowSizeFraction(), 0.0)
	connect.AssertEqual(t, second.LimitedWindowTargetSize(), 0)
}
