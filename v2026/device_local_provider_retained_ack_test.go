package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A provider's socket-owned returns are retained past the ordinary
// acknowledgement lifetime, and connect bounds that retention with
// SendBufferSettings.RetainedAckTimeout. Its zero value keeps the unbounded
// retention that rewrote a gone destination's whole window every
// MaxResendInterval until the provider restarted. Every provider in the field
// builds its client from these settings, the apps through DeviceLocal and the
// sn miner through DefaultDeviceLocalSettings, so each copy and memory sizing
// step between connect's defaults and the provider client must keep the bound.
func TestDeviceLocalProviderClientKeepsRetainedAckBound(t *testing.T) {
	bound := connect.DefaultSendBufferSettings().RetainedAckTimeout
	if bound <= 0 {
		t.Fatal("connect sets no default retained acknowledgement bound, so this proves nothing")
	}

	// a memory-targeted mobile device, sized the way device construction does
	mobile, mobileProvider := transferMemoryTestDevice(t, defaultDeviceLocalMemoryTargetByteCountForPlatform(true))
	applyMobileH1PerformanceClientSettingsForPlatform(mobileProvider, mobile.settings.MemoryTargetByteCount, true, true)

	// a desktop device, which keeps the connect sizing
	desktop := DefaultDeviceLocalSettings()
	desktop.MemoryTargetByteCount = defaultDeviceLocalMemoryTargetByteCountForPlatform(false)
	desktopMemory := newDeviceLocalTransferMemoryForPlatform(desktop, false)
	configureDeviceLocalClientMemoryForPlatform(desktop, desktopMemory, false)
	_, _, _, desktopProviderShare := deviceMemoryShares(desktop)
	desktopProvider := newDeviceClientSettings(&desktop.ClientSettings, "", nil)
	configureDeviceLocalProviderMemory(desktopProvider, desktopProviderShare)

	// a caller's partial client settings, completed from connect's defaults
	partialProvider := newDeviceClientSettings(&connect.ClientSettings{
		ForwardBufferSettings:   connect.DefaultForwardBufferSettings(),
		ContractManagerSettings: connect.DefaultContractManagerSettings(),
	}, "", nil)

	for _, c := range []struct {
		name     string
		settings *connect.ClientSettings
	}{
		{name: "mobile device client", settings: &mobile.settings.ClientSettings},
		{name: "mobile provider client", settings: mobileProvider},
		{name: "desktop device client", settings: &desktop.ClientSettings},
		{name: "desktop provider client", settings: desktopProvider},
		{name: "partial provider client", settings: partialProvider},
	} {
		if retained := c.settings.SendBufferSettings.RetainedAckTimeout; retained != bound {
			t.Errorf("%s retained acknowledgement bound = %s, want connect's %s", c.name, retained, bound)
		}
	}
}
