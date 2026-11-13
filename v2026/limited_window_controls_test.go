//go:build !ios

// The developer menu's limited window fraction (server FP2-PROTOCOL.md §7.8,
// §10): read, set and reset on the device, applied to the live window
// without a reconnect, and built into every window after it; and the window
// status and developer exits that report the limited window.
package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Disconnects the device and joins the window it had, so a reconnect builds
// the next one.
func disconnectLimitedWindowTestDevice(t *testing.T, device *DeviceLocal, multi *connect.RemoteUserNatMultiClient) {
	t.Helper()
	if err := device.SetConnectLocationChecked(nil); err != nil {
		t.Fatal(err)
	}
	join, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := multi.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
}

// The current window of a connected device.
func requireLimitedWindowTestMulti(t *testing.T, device *DeviceLocal) *connect.RemoteUserNatMultiClient {
	t.Helper()
	multi, ok := device.multiClient()
	if !ok {
		t.Fatal("the device built no multi client")
	}
	return multi
}

// While disconnected the fraction is the one the next window takes: the
// override when set, else connect's.
func TestLimitedWindowSizeFractionWhileDisconnected(t *testing.T) {
	device := newH3PolicyDevice(t, LowMemoryDeviceTargetByteCount)
	defaultFraction := connect.DefaultMultiClientSettings().LimitedWindowSizeFraction
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), defaultFraction)
	device.SetLimitedWindowSizeFraction(0.25)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.25)
	device.SetLimitedWindowSizeFraction(0)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.0)
	device.ResetLimitedWindowSizeFraction()
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), defaultFraction)
}

// An override set before connecting builds the first window; one set while
// connected reaches the live window and survives the reconnect; a reset
// returns the live window and the next to connect's fraction.
func TestLimitedWindowSizeFractionReachesTheLiveAndTheNextWindow(t *testing.T) {
	device := newH3PolicyDevice(t, LowMemoryDeviceTargetByteCount)
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	defaultFraction := connect.DefaultMultiClientSettings().LimitedWindowSizeFraction

	device.SetLimitedWindowSizeFraction(0.25)
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	first := requireLimitedWindowTestMulti(t, device)
	connect.AssertEqual(t, first.LimitedWindowSizeFraction(), 0.25)

	device.SetLimitedWindowSizeFraction(0.75)
	connect.AssertEqual(t, first.LimitedWindowSizeFraction(), 0.75)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), 0.75)

	disconnectLimitedWindowTestDevice(t, device, first)
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	second := requireLimitedWindowTestMulti(t, device)
	if second == first {
		t.Fatal("the reconnect kept the retired window")
	}
	connect.AssertEqual(t, second.LimitedWindowSizeFraction(), 0.75)

	device.ResetLimitedWindowSizeFraction()
	connect.AssertEqual(t, second.LimitedWindowSizeFraction(), defaultFraction)
	connect.AssertEqual(t, device.GetLimitedWindowSizeFraction(), defaultFraction)

	disconnectLimitedWindowTestDevice(t, device, second)
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, requireLimitedWindowTestMulti(t, device).LimitedWindowSizeFraction(), defaultFraction)
}

// A connected device reports its limited window beside the normal windows:
// none held while discovery answers nothing, and no UDP route without a
// dualproto client.
func TestWindowStatusReportsTheLimitedWindow(t *testing.T) {
	device := newH3PolicyDevice(t, LowMemoryDeviceTargetByteCount)
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	multi := requireLimitedWindowTestMulti(t, device)
	windowStatus := device.GetWindowStatus()
	connect.AssertEqual(t, windowStatus.LimitedWindowSize, multi.LimitedWindowSize())
	connect.AssertEqual(t, windowStatus.LimitedWindowSize, 0)
	connect.AssertEqual(t, windowStatus.UdpAvailable, false)
}

// A limited exit is named by its window, which has no rank mode of its own,
// and carries the labels and overflow step the server gave its provider.
func TestExitFromConnectNamesTheLimitedWindow(t *testing.T) {
	exit := exitFromConnect(&connect.ExitInfo{
		ClientId:           connect.NewId(),
		WindowType:         connect.WindowTypeLimited,
		CapabilityIp:       "ipv4",
		CapabilityProtocol: "tcp",
		OverflowStep:       1,
	})
	connect.AssertEqual(t, exit.WindowType, ExitWindowTypeLimited)
	connect.AssertEqual(t, exit.CapabilityIp, "ipv4")
	connect.AssertEqual(t, exit.CapabilityProtocol, "tcp")
	connect.AssertEqual(t, exit.OverflowStep, int32(1))
	for windowType, expectedWindowType := range map[connect.WindowType]string{
		connect.WindowTypeQuality: "quality",
		connect.WindowTypeSpeed:   "speed",
		connect.WindowTypeAuto:    "",
	} {
		connect.AssertEqual(t, exitFromConnect(&connect.ExitInfo{WindowType: windowType}).WindowType, expectedWindowType)
	}
}
