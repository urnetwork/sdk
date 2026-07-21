package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// testing_newContractStatusDevice builds a DeviceLocal with a small contract
// status window so the sliding-window behavior is testable without long waits.
func testing_newContractStatusDevice(ctx context.Context, t *testing.T, count int, duration time.Duration) *DeviceLocal {
	t.Helper()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.Verbose = false
	settings.DisableLogging = true
	settings.NetContractStatusCount = count
	settings.NetContractStatusDuration = duration
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	return device
}

func testing_contractStatusUpdateCount(device *DeviceLocal) int {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return len(device.orderedContractStatusUpdates)
}

// TestDeviceLocalContractStatusWindowExpires asserts the summarized status is
// built only from updates inside the window: a premium status must stop being
// reported once it ages out. The window trim previously kept exactly the
// expired updates (slicing [:i] instead of [i:]), which latched Premium on for
// the life of the device.
func TestDeviceLocalContractStatusWindowExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	duration := 200 * time.Millisecond
	device := testing_newContractStatusDevice(ctx, t, 10, duration)
	defer device.Close()

	device.updateContractStatus(&connect.ContractStatus{Premium: true})
	connect.AssertEqual(t, true, device.GetContractStatus().Premium)

	// age the premium update out of the window, then report a non-premium
	// status: the expired update must no longer contribute
	select {
	case <-time.After(2 * duration):
	}
	device.updateContractStatus(&connect.ContractStatus{Premium: false})

	if device.GetContractStatus().Premium {
		t.Fatalf("premium latched on after the update aged out of the window")
	}
	// only the in-window update is retained
	connect.AssertEqual(t, 1, testing_contractStatusUpdateCount(device))
}

// TestDeviceLocalContractStatusWindowBounded asserts the retained window is
// bounded by NetContractStatusCount. Slicing the wrong side of the window grew
// this slice without bound (one entry per update, forever).
func TestDeviceLocalContractStatusWindowBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	count := 3
	device := testing_newContractStatusDevice(ctx, t, count, 10*time.Second)
	defer device.Close()

	// a burst well inside the duration window: the count bound is what trims
	for range 50 {
		device.updateContractStatus(&connect.ContractStatus{Premium: false})
	}
	if got := testing_contractStatusUpdateCount(device); count < got {
		t.Fatalf("retained %d contract status updates, want at most %d", got, count)
	}
}

// TestDeviceLocalContractStatusErrorClears asserts an error state reported
// inside the window clears once a healthy status arrives, and that the
// summarized status reflects the newest update rather than a stale one.
func TestDeviceLocalContractStatusErrorClears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device := testing_newContractStatusDevice(ctx, t, 10, 10*time.Second)
	defer device.Close()

	insufficientBalance := protocol.ContractError_InsufficientBalance
	device.updateContractStatus(&connect.ContractStatus{Error: &insufficientBalance})
	connect.AssertEqual(t, true, device.GetContractStatus().InsufficientBalance)

	// a healthy status inside the same window resets the error state
	device.updateContractStatus(&connect.ContractStatus{})
	connect.AssertEqual(t, false, device.GetContractStatus().InsufficientBalance)
}
