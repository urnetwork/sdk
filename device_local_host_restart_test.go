// A host that runs its DeviceLocal in its own process, over the space's local
// state, as the android app does (android DeviceManager.initDevice): it turns
// on autosave and seeds each setting it saves itself from the local state. A
// setting it writes only through the device, which autosaves it in the same
// store, is restored at the next start only by DeviceLocal.Load. The client
// and provider transport settings are such settings, and android called no
// Load, so after every app restart, update or reboot they came back as the
// default carrier policies while the store still held the user's choice.
package sdk

import (
	"testing"
)

// The settings the android app seeds from its local state, in its order.
func testingSeedLikeAndroid(device *DeviceLocal, localState *LocalState) {
	device.SetRouteLocal(localState.GetRouteLocal())
	device.SetProvideMode(localState.GetProvideMode())
	device.SetCanShowRatingDialog(localState.GetCanShowRatingDialog())
	device.SetProvideControlMode(localState.GetProvideControlMode())
	device.SetVpnInterfaceWhileOffline(localState.GetVpnInterfaceWhileOffline())
	device.SetCanRefer(localState.GetCanRefer())
	device.SetAllowForeground(localState.GetAllowForeground())
	device.SetProvideNetworkMode(localState.GetProvideNetworkMode())
	device.SetCanPromptIntroFunnel(localState.GetCanPromptIntroFunnel())
	device.SetPerformanceProfile(localState.GetPerformanceProfile())
	device.SetBlockerEnabled(localState.GetBlockerEnabled())
	if overrides := localState.GetBlockActionOverrides(); overrides != nil {
		device.SetBlockActionOverrides(overrides)
	}
	if dnsResolverSettings := localState.GetDnsResolverSettings(); dnsResolverSettings != nil {
		device.SetDnsResolverSettings(dnsResolverSettings)
	}
}

func TestDeviceLocalHostRestartKeepsTheCarrierPoliciesThroughLoad(t *testing.T) {
	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)

	// before the restart: the user picked h3 for both directions, which the
	// app writes only through the device, and turned post-quantum on, which
	// the app also saves itself
	before := testingPreferenceDevice(t, fixture)
	if err := before.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	before.SetTransportSettings(&TransportSettings{Mode: TransportModeH3})
	before.SetProviderTransportSettings(&TransportSettings{Mode: TransportModeH3})
	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	if err := fixture.localState.SetPerformanceProfile(postQuantum); err != nil {
		t.Fatal(err)
	}
	before.SetPerformanceProfile(postQuantum)
	testingJoinPreferenceDevice(t, before)
	manager.Close()

	// the restart as android started its device before: autosave and the seeds
	// alone. The store keeps h3, the device runs the defaults
	unloadedManager, unloaded := testingPreferenceSpaceAt(t, directory)
	withoutLoad := testingPreferenceDevice(t, unloaded)
	if err := withoutLoad.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	withoutLoad.SetProvidePaused(true)
	testingSeedLikeAndroid(withoutLoad, unloaded.localState)
	if withoutLoad.GetTransportSettings().Mode == TransportModeH3 ||
		withoutLoad.GetProviderTransportSettings().Mode == TransportModeH3 {
		t.Fatal("a device started without Load restored its carrier policies: the case this test covers is gone")
	}
	if saved := unloaded.localState.GetTransportSettings(); saved == nil || saved.Mode != TransportModeH3 {
		t.Fatalf("the seeds rewrote the saved client carrier policy: %+v", saved)
	}
	testingJoinPreferenceDevice(t, withoutLoad)
	unloadedManager.Close()

	// the restart as android starts its device now: autosave, Load, then the
	// same seeds
	_, restarted := testingPreferenceSpaceAt(t, directory)
	device := testingPreferenceDevice(t, restarted)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetProvidePaused(true)
	load, err := device.Load()
	if err != nil || !load.GetLoaded() {
		t.Fatalf("load: %v", err)
	}
	testingSeedLikeAndroid(device, restarted.localState)

	if mode := device.GetTransportSettings().Mode; mode != TransportModeH3 {
		t.Errorf("after the restart the client carrier policy is %s, want the saved h3", mode)
	}
	if mode := device.GetProviderTransportSettings().Mode; mode != TransportModeH3 {
		t.Errorf("after the restart the provider carrier policy is %s, want the saved h3", mode)
	}
	if profile := device.GetPerformanceProfile(); profile == nil || !profile.PostQuantumEncryption {
		t.Errorf("after the restart the profile is %+v, want post-quantum on", profile)
	}
	if saved := restarted.localState.GetTransportSettings(); saved == nil || saved.Mode != TransportModeH3 {
		t.Errorf("the start rewrote the saved client carrier policy: %+v", saved)
	}
	if saved := restarted.localState.GetProviderTransportSettings(); saved == nil || saved.Mode != TransportModeH3 {
		t.Errorf("the start rewrote the saved provider carrier policy: %+v", saved)
	}
}
