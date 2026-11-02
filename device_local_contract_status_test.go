package sdk

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
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

// A result that says nothing about the account does not clear an account error,
// however long ago it was reported. The window only summarizes what is still
// inside it, so a Setup result arriving after a refusal aged out summarized to "no
// error". The platform answers Setup for a failure on its own side, and an account
// that is out of balance then read as funded: the app withdrew its out of balance
// alert, and the desktop tunnels' failsafe took it as the balance having come back.
func TestDeviceLocalContractStatusUnrelatedErrorKeepsAccountError(t *testing.T) {
	for _, accountKind := range []testingContractErrorKind{testingInsufficientBalanceErrorKind, testingNoPermissionErrorKind} {
		for _, unrelated := range []protocol.ContractError{
			protocol.ContractError_Setup,
			protocol.ContractError_Reliability,
			protocol.ContractError_Trust,
			protocol.ContractError_Invalid,
		} {
			t.Run(accountKind.contractError.String()+"/"+unrelated.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					device := newContractErrorLogDevice(newTestingContractErrorLogger())
					// past the window on the bubble's clock
					ageOut := 2 * device.settings.NetContractStatusDuration

					device.updateContractStatus(testingContractErrorStatus(accountKind.contractError))
					connect.AssertEqual(t, true, accountKind.inStatus(device.GetContractStatus()))

					// the account's error is out of the window, and this is the only update
					time.Sleep(ageOut)
					device.updateContractStatus(testingContractErrorStatus(unrelated))
					connect.AssertEqual(t, 1, testing_contractStatusUpdateCount(device))
					if !accountKind.inStatus(device.GetContractStatus()) {
						t.Fatalf("%s cleared %s", unrelated, accountKind.contractError)
					}

					// only a healthy status ends it
					time.Sleep(ageOut)
					device.updateContractStatus(&connect.ContractStatus{})
					if accountKind.inStatus(device.GetContractStatus()) {
						t.Fatalf("a healthy status did not clear %s", accountKind.contractError)
					}
				})
			})
		}
	}
}

// A result that says nothing about the account adds nothing to the status either.
func TestDeviceLocalContractStatusUnrelatedErrorRaisesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device := newContractErrorLogDevice(newTestingContractErrorLogger())
		device.updateContractStatus(testingContractErrorStatus(protocol.ContractError_Setup))
		if contractStatus := device.GetContractStatus(); *contractStatus != (ContractStatus{}) {
			t.Fatalf("a setup result reads %+v", contractStatus)
		}
	})
}

// What a refused account's session reports and what it can read back: the status
// raised by the refusal, no exits and an empty window while no provider is
// admitted, the same generation as long as the destination stands, and a new
// destination resetting the status along with the generation. The desktop tunnels'
// failsafe reads these (connect's TestOutOfBalanceWindowAdmitsNoExit is the window
// behind them).
func TestDeviceLocalContractStatusOutOfBalanceSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device := testing_newContractStatusDevice(ctx, t, 10, 10*time.Minute)
	defer device.Close()

	initial := device.GetContractStatus()
	if initial == nil || *initial != (ContractStatus{}) {
		t.Fatalf("a new device reads a contract status: %+v", initial)
	}
	generation := device.GetWindowStatus().ConnectionGeneration

	insufficientBalance := protocol.ContractError_InsufficientBalance
	device.updateContractStatus(&connect.ContractStatus{Error: &insufficientBalance})
	connect.AssertEqual(t, true, device.GetContractStatus().InsufficientBalance)
	window := device.GetWindowStatus()
	connect.AssertEqual(t, generation, window.ConnectionGeneration)
	connect.AssertEqual(t, 0, window.ProviderStateAdded)
	connect.AssertEqual(t, false, window.MinSatisfied)
	connect.AssertEqual(t, 0, device.GetExits().Len())

	// the same generation: a granted contract is what clears it
	device.updateContractStatus(&connect.ContractStatus{})
	connect.AssertEqual(t, false, device.GetContractStatus().InsufficientBalance)
	connect.AssertEqual(t, generation, device.GetWindowStatus().ConnectionGeneration)

	// a new destination: cleared with the generation moved, which is how a reader
	// tells a reset from a grant
	device.updateContractStatus(&connect.ContractStatus{Error: &insufficientBalance})
	connect.AssertEqual(t, true, device.GetContractStatus().InsufficientBalance)
	device.SetConnectLocation(nil)
	connect.AssertEqual(t, false, device.GetContractStatus().InsufficientBalance)
	if next := device.GetWindowStatus().ConnectionGeneration; next <= generation {
		t.Fatalf("the reset did not move the generation: %d then %d", generation, next)
	}
}

type testingContractStatusChangeListener func(*ContractStatus)

func (self testingContractStatusChangeListener) ContractStatusChanged(contractStatus *ContractStatus) {
	self(contractStatus)
}

// TestDeviceLocalContractStatusClearsOnDisconnect asserts the contract status
// is dropped with the connection that reported it. An insufficient balance
// error previously outlived a disconnect: nothing can report a healthy status
// while disconnected, so the latched error kept the UI on the subscribe
// prompt even after the balance was refreshed.
func TestDeviceLocalContractStatusClearsOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device := testing_newContractStatusDevice(ctx, t, 10, 10*time.Minute)
	defer device.Close()

	insufficientBalance := protocol.ContractError_InsufficientBalance
	device.updateContractStatus(&connect.ContractStatus{Error: &insufficientBalance})
	connect.AssertEqual(t, true, device.GetContractStatus().InsufficientBalance)

	var changes []*ContractStatus
	sub := device.AddContractStatusChangeListener(testingContractStatusChangeListener(func(contractStatus *ContractStatus) {
		changes = append(changes, contractStatus)
	}))
	defer sub.Close()

	device.SetConnectLocation(nil)

	if device.GetContractStatus().InsufficientBalance {
		t.Fatalf("insufficient balance outlived the disconnect")
	}
	connect.AssertEqual(t, 0, testing_contractStatusUpdateCount(device))
	if len(changes) != 1 || changes[0].InsufficientBalance {
		t.Fatalf("disconnect did not publish the cleared contract status: %v", changes)
	}
}
