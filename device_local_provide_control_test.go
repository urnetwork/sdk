package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
)

// TestDeviceLocalProvideControlModes asserts the control mode → effective
// provide mode mapping (ProvideMode is a bit set — assert per-case, never
// with ranges):
//   - always  → Public
//   - network → Network (the private provider: always on, peers only)
//   - never   → None (no providing at all)
//   - auto    → Network while idle (Public while connected is covered by the
//     connect flow tests)
//
// and that SetProvideControlMode ENFORCES its mapping even when the control
// mode value is unchanged, correcting a provide mode set independently (the
// stale-persisted-mode migration path).
func TestDeviceLocalProvideControlModes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	connect.AssertEqual(t, err, nil)
	defer device.Close()

	assertMode := func(provideMode ProvideMode, provideEnabled bool) {
		connect.AssertEqual(t, device.GetProvideMode(), provideMode)
		connect.AssertEqual(t, device.GetProvideEnabled(), provideEnabled)
	}

	device.SetProvideControlMode(ProvideControlModeAlways)
	assertMode(ProvideModePublic, true)

	// the private provider: always on, but only for same-network peers
	device.SetProvideControlMode(ProvideControlModeNetwork)
	assertMode(ProvideModeNetwork, true)

	// never means never: the provider is torn down entirely
	device.SetProvideControlMode(ProvideControlModeNever)
	assertMode(ProvideModeNone, false)

	// auto while idle keeps Network provide, so the device stays a
	// reachable/discoverable peer
	device.SetProvideControlMode(ProvideControlModeAuto)
	assertMode(ProvideModeNetwork, true)

	// enforcement: a provide mode set independently of the control mode is
	// corrected by a redundant SetProvideControlMode with the SAME value —
	// the control mode owns the provide mode. This is the migration path for
	// a stale persisted provide mode applied at init.
	device.SetProvideControlMode(ProvideControlModeNever)
	assertMode(ProvideModeNone, false)
	device.SetProvideMode(ProvideModeNetwork)
	assertMode(ProvideModeNetwork, true)
	device.SetProvideControlMode(ProvideControlModeNever)
	assertMode(ProvideModeNone, false)
}
