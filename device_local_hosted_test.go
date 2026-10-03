package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// A hosted (platform-embedded, e.g. cloud proxy) device must never route
// traffic locally: local egress would leave the host's real network interface.
// connectBlockActionOverrides is the single point every override passes through
// on its way to the live multi client, so when hostedIncompatible is set it must
// force RouteOverride.Local to false no matter what the client requested — even
// for a raw private subnet, which needs no DNS. Block overrides are unaffected.
func TestConnectBlockActionOverridesHostedForcesRemote(t *testing.T) {
	overrides := []*BlockActionOverride{
		{
			OverrideId:    NewId(),
			Hosts:         testing_stringList("10.0.0.0/8"),
			RouteOverride: &RouteOverride{Local: true},
		},
		{
			OverrideId:    NewId(),
			Hosts:         testing_stringList("blocked.example.com"),
			BlockOverride: &BlockOverride{Block: true},
		},
	}

	// not hosted: a local route override is honored (baseline behavior)
	notHosted := connectBlockActionOverrides(overrides, false)
	connect.AssertEqual(t, 2, len(notHosted))
	connect.AssertEqual(t, true, notHosted[0].RouteOverride.Local)

	// hosted: the local route override is forced to remote; the block override
	// passes through unchanged
	hosted := connectBlockActionOverrides(overrides, true)
	connect.AssertEqual(t, 2, len(hosted))
	connect.AssertEqual(t, false, hosted[0].RouteOverride.Local)
	connect.AssertEqual(t, true, hosted[1].BlockOverride.Block)
}

// hostedSafeBlockActionOverride neutralizes a local route override on the way into
// a hosted device's stored state, so GetBlockActionOverrides is truthful (never
// reports a local route) and the input override is not mutated in place.
func TestHostedSafeBlockActionOverride(t *testing.T) {
	hosted := &DeviceLocal{settings: &DeviceLocalSettings{HostedIncompatible: true}}
	notHosted := &DeviceLocal{settings: &DeviceLocalSettings{HostedIncompatible: false}}

	localOverride := func() *BlockActionOverride {
		return &BlockActionOverride{
			OverrideId:    NewId(),
			Hosts:         testing_stringList("example.com"),
			RouteOverride: &RouteOverride{Local: true},
		}
	}

	// hosted: the local route is neutralized to remote
	if got := hosted.hostedSafeBlockActionOverride(localOverride()); got.RouteOverride.Local {
		t.Error("hosted device must neutralize a local route override to Local=false")
	}

	// non-hosted: the override is stored unchanged
	if got := notHosted.hostedSafeBlockActionOverride(localOverride()); !got.RouteOverride.Local {
		t.Error("non-hosted device must keep a local route override")
	}

	// hosted: a block override is unaffected, and the caller's override is not
	// mutated in place (a corrected copy is returned)
	in := localOverride()
	in.BlockOverride = &BlockOverride{Block: true}
	got := hosted.hostedSafeBlockActionOverride(in)
	connect.AssertEqual(t, got.RouteOverride.Local, false)
	connect.AssertEqual(t, got.BlockOverride.Block, true)
	connect.AssertEqual(t, in.RouteOverride.Local, true) // original untouched

	// hosted: an override with no route override is returned as-is
	blockOnly := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         testing_stringList("blocked.example.com"),
		BlockOverride: &BlockOverride{Block: true},
	}
	if got := hosted.hostedSafeBlockActionOverride(blockOnly); got.BlockOverride.Block != true {
		t.Error("hosted device must preserve a block-only override")
	}
}

// A hosted device must never allow direct mode: a direct connection would leak
// that the device is hosted, and where it is hosted, via the host addresses in
// the direct connection setup. hostedSafePerformanceProfile forces AllowDirect
// off on the way into a hosted device's stored state, so GetPerformanceProfile
// is truthful and the input profile is not mutated in place.
func TestHostedSafePerformanceProfile(t *testing.T) {
	hosted := &DeviceLocal{settings: &DeviceLocalSettings{HostedIncompatible: true}}
	notHosted := &DeviceLocal{settings: &DeviceLocalSettings{HostedIncompatible: false}}

	allowDirectProfile := func() *PerformanceProfile {
		return &PerformanceProfile{
			WindowType:  WindowTypeQuality,
			AllowDirect: true,
		}
	}

	// hosted: direct mode is forced off; the rest of the profile is preserved
	got := hosted.hostedSafePerformanceProfile(allowDirectProfile())
	connect.AssertEqual(t, false, got.AllowDirect)
	connect.AssertEqual(t, WindowTypeQuality, got.WindowType)

	// non-hosted: the profile is unchanged
	got = notHosted.hostedSafePerformanceProfile(allowDirectProfile())
	connect.AssertEqual(t, true, got.AllowDirect)

	// hosted: the caller's profile is not mutated in place
	in := allowDirectProfile()
	got = hosted.hostedSafePerformanceProfile(in)
	connect.AssertEqual(t, false, got.AllowDirect)
	connect.AssertEqual(t, true, in.AllowDirect)

	// hosted: nil and non-direct profiles pass through as-is
	connect.AssertEqual(t, true, hosted.hostedSafePerformanceProfile(nil) == nil)
	nonDirect := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
	}
	connect.AssertEqual(t, true, nonDirect == hosted.hostedSafePerformanceProfile(nonDirect))
}

// A hosted remote must discard every platform-owned mutation before it can be
// queued for sync or accepted into the last-known cache. The hosted rpc returns
// success for these deliberate noops, so allowing a call through would make a
// getter report a value the device never applied.
func TestDeviceRemoteHostedIncompatibleSettersDoNotChangeState(t *testing.T) {
	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.DisableLogging = true
	deviceRemote := &DeviceRemote{
		settings: settings,
		log:      settings.logger(),
	}

	deviceRemote.SetTunnelStarted(true)
	deviceRemote.SetRouteLocal(!settings.DefaultRouteLocal)
	deviceRemote.SetProvideControlMode(ProvideControlModeAlways)
	deviceRemote.SetProvideMode(ProvideModePublic)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetProvideNetworkMode(ProvideNetworkModeAll)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.SetTransportSettings(DefaultTransportSettings())
	deviceRemote.SetProviderTransportSettings(DefaultProviderTransportSettings())

	fields := []struct {
		name      string
		pending   bool
		lastKnown bool
	}{
		{name: "tunnel started", pending: deviceRemote.state.TunnelStarted.IsSet, lastKnown: deviceRemote.lastKnownState.TunnelStarted.IsSet},
		{name: "route local", pending: deviceRemote.state.RouteLocal.IsSet, lastKnown: deviceRemote.lastKnownState.RouteLocal.IsSet},
		{name: "provide control mode", pending: deviceRemote.state.ProvideControlMode.IsSet, lastKnown: deviceRemote.lastKnownState.ProvideControlMode.IsSet},
		{name: "provide mode", pending: deviceRemote.state.ProvideMode.IsSet, lastKnown: deviceRemote.lastKnownState.ProvideMode.IsSet},
		{name: "provide paused", pending: deviceRemote.state.ProvidePaused.IsSet, lastKnown: deviceRemote.lastKnownState.ProvidePaused.IsSet},
		{name: "provide network mode", pending: deviceRemote.state.ProvideNetworkMode.IsSet, lastKnown: deviceRemote.lastKnownState.ProvideNetworkMode.IsSet},
		{name: "vpn interface while offline", pending: deviceRemote.state.VpnInterfaceWhileOffline.IsSet, lastKnown: deviceRemote.lastKnownState.VpnInterfaceWhileOffline.IsSet},
		{name: "transport settings", pending: deviceRemote.state.TransportSettings.IsSet, lastKnown: deviceRemote.lastKnownState.TransportSettings.IsSet},
		{name: "provider transport settings", pending: deviceRemote.state.ProviderTransportSettings.IsSet, lastKnown: deviceRemote.lastKnownState.ProviderTransportSettings.IsSet},
	}
	for _, field := range fields {
		if field.pending {
			t.Errorf("%s was queued for hosted sync", field.name)
		}
		if field.lastKnown {
			t.Errorf("%s poisoned the hosted last-known cache", field.name)
		}
	}
}
