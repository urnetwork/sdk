package sdk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The `[contract]error` lines of updateContractStatus (urnetwork/connect#227).
// Under sustained contract refusals every status update reports the error, and
// the line was logged inside the status summary, once for every update still
// in the window: up to ten lines per update. In support inbox 8118 a VPN
// window fed about five NoPermission updates a second, and the line was most
// of the app's log.
//
// Time moves inside a synctest bubble: a time.Sleep there advances the
// bubble's fake clock at once, so these tests hold the production one-minute
// interval and wait for nothing.

// Records the formatted `[contract]error` lines a device logs.
type testingContractErrorLogger struct {
	connect.Logger
	stateLock sync.Mutex
	lines     []string
}

func newTestingContractErrorLogger() *testingContractErrorLogger {
	return &testingContractErrorLogger{
		Logger: connect.NewNoopLogger(),
	}
}

func (self *testingContractErrorLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(line, "[contract]error") {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testingContractErrorLogger) contractErrorLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

// A device with only what updateContractStatus touches, and the default
// settings, so it runs inside a synctest bubble.
func newContractErrorLogDevice(log connect.Logger) *DeviceLocal {
	return &DeviceLocal{
		settings:                      DefaultDeviceLocalSettings(),
		log:                           log,
		orderedContractStatusUpdates:  []*contractStatusUpdate{},
		netContractStatus:             &ContractStatus{},
		contractStatusChangeListeners: connect.NewCallbackList[ContractStatusChangeListener](),
	}
}

func testingContractErrorStatus(contractError protocol.ContractError) *connect.ContractStatus {
	return &connect.ContractStatus{Error: &contractError}
}

type testingContractErrorKind struct {
	contractError protocol.ContractError
	line          string
	inStatus      func(*ContractStatus) bool
}

var testingNoPermissionErrorKind = testingContractErrorKind{
	contractError: protocol.ContractError_NoPermission,
	line:          "[contract]error no permission",
	inStatus: func(contractStatus *ContractStatus) bool {
		return contractStatus.NoPermission
	},
}

// the message keeps its misspelling, which log searches match
var testingInsufficientBalanceErrorKind = testingContractErrorKind{
	contractError: protocol.ContractError_InsufficientBalance,
	line:          "[contract]error insufficent balance",
	inStatus: func(contractStatus *ContractStatus) bool {
		return contractStatus.InsufficientBalance
	},
}

func testingFirstContractErrorLines(lines []string) []string {
	return lines[:min(3, len(lines))]
}

// Repeats of an error the status keeps log one line per interval, and that
// line counts the repeats suppressed since the line before. The status itself
// still reports every error.
func TestDeviceLocalContractErrorLogRateLimitsRepeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, kind := range []testingContractErrorKind{testingNoPermissionErrorKind, testingInsufficientBalanceErrorKind} {
			logger := newTestingContractErrorLogger()
			device := newContractErrorLogDevice(logger)

			for range 100 {
				device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			}
			lines := logger.contractErrorLines()
			if !slices.Equal(lines, []string{kind.line + "\n"}) {
				t.Errorf("%s: 100 errors in one interval logged %d lines, want 1; first lines %q", kind.line, len(lines), testingFirstContractErrorLines(lines))
				continue
			}
			if !kind.inStatus(device.GetContractStatus()) {
				t.Errorf("%s: the contract status lost the error", kind.line)
				continue
			}

			// still inside the interval of the first line
			time.Sleep(59 * time.Second)
			device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			if lines := logger.contractErrorLines(); len(lines) != 1 {
				t.Errorf("%s: a repeat 59s after the first line logged; lines %q", kind.line, lines)
				continue
			}

			// the interval has passed: one line with the count of the 100
			// errors suppressed since the first line, then limited again
			time.Sleep(time.Second)
			device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			wantLines := []string{
				kind.line + "\n",
				kind.line + " (100 suppressed)\n",
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("%s: after the interval logged %q, want %q", kind.line, lines, wantLines)
				continue
			}
			if !kind.inStatus(device.GetContractStatus()) {
				t.Errorf("%s: the contract status lost the error", kind.line)
			}
		}
	})
}

// An error of a kind the status does not have logs at once, while the status
// keeps another kind that is being rate-limited. Interleaved repeats of both
// stay limited. When one kind fills the window the other leaves the status,
// and its next error logs at once with the count suppressed meanwhile.
func TestDeviceLocalContractErrorLogOtherKindLogsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pairs := [][2]testingContractErrorKind{
			{testingNoPermissionErrorKind, testingInsufficientBalanceErrorKind},
			{testingInsufficientBalanceErrorKind, testingNoPermissionErrorKind},
		}
		for _, pair := range pairs {
			first, other := pair[0], pair[1]
			logger := newTestingContractErrorLogger()
			device := newContractErrorLogDevice(logger)

			for range 100 {
				device.updateContractStatus(testingContractErrorStatus(first.contractError))
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, []string{first.line + "\n"}) {
				t.Errorf("%s: 100 errors in one interval logged %d lines, want 1; first lines %q", first.line, len(lines), testingFirstContractErrorLines(lines))
				continue
			}

			device.updateContractStatus(testingContractErrorStatus(other.contractError))
			wantLines := []string{first.line + "\n", other.line + "\n"}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("%s after %s logged %q, want %q", other.line, first.line, lines, wantLines)
				continue
			}

			for range 50 {
				device.updateContractStatus(testingContractErrorStatus(first.contractError))
				device.updateContractStatus(testingContractErrorStatus(other.contractError))
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("%s and %s interleaved logged %q, want %q", first.line, other.line, lines, wantLines)
				continue
			}
			if contractStatus := device.GetContractStatus(); !first.inStatus(contractStatus) || !other.inStatus(contractStatus) {
				t.Errorf("the contract status %+v does not keep both kinds", contractStatus)
				continue
			}

			// a full window of the other kind, which stays limited
			for range device.settings.NetContractStatusCount {
				device.updateContractStatus(testingContractErrorStatus(other.contractError))
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("a window of %s logged %q, want %q", other.line, lines, wantLines)
				continue
			}
			if contractStatus := device.GetContractStatus(); first.inStatus(contractStatus) || !other.inStatus(contractStatus) {
				t.Errorf("the contract status %+v still has %s", contractStatus, first.line)
				continue
			}

			// 99 of the burst and 50 interleaved were suppressed
			device.updateContractStatus(testingContractErrorStatus(first.contractError))
			wantLines = append(wantLines, first.line+" (149 suppressed)\n")
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("%s after it left the status logged %q, want %q", first.line, lines, wantLines)
			}
		}
	})
}

// A healthy status clears the error, and the next error logs at once with the
// count suppressed before the clear.
func TestDeviceLocalContractErrorLogClearedStatusLogsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, kind := range []testingContractErrorKind{testingNoPermissionErrorKind, testingInsufficientBalanceErrorKind} {
			logger := newTestingContractErrorLogger()
			device := newContractErrorLogDevice(logger)

			for range 100 {
				device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, []string{kind.line + "\n"}) {
				t.Errorf("%s: 100 errors in one interval logged %d lines, want 1; first lines %q", kind.line, len(lines), testingFirstContractErrorLines(lines))
				continue
			}

			device.updateContractStatus(&connect.ContractStatus{})
			if kind.inStatus(device.GetContractStatus()) {
				t.Errorf("%s: a healthy status did not clear the error", kind.line)
				continue
			}

			for range 10 {
				device.updateContractStatus(testingContractErrorStatus(kind.contractError))
			}
			wantLines := []string{
				kind.line + "\n",
				kind.line + " (99 suppressed)\n",
			}
			if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
				t.Errorf("%s: errors after the clear logged %q, want %q", kind.line, lines, wantLines)
			}
		}
	})
}

// A disconnect clears the contract status with the connection that reported
// it, so the first error after it logs at once. This runs the real device and
// SetConnectLocation outside a bubble. The one-minute interval does not pass
// between these calls, so the second line can only come from the clear.
func TestDeviceLocalContractErrorLogClearedByDisconnectLogsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	logger := newTestingContractErrorLogger()
	settings := DefaultDeviceLocalSettings()
	settings.Verbose = false
	settings.DisableLogging = false
	settings.ClientSettings.Log = logger
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	line := testingNoPermissionErrorKind.line
	for range 100 {
		device.updateContractStatus(testingContractErrorStatus(protocol.ContractError_NoPermission))
	}
	if lines := logger.contractErrorLines(); !slices.Equal(lines, []string{line + "\n"}) {
		t.Fatalf("100 errors in one interval logged %d lines, want 1; first lines %q", len(lines), testingFirstContractErrorLines(lines))
	}

	device.SetConnectLocation(nil)
	if device.GetContractStatus().NoPermission {
		t.Fatalf("the disconnect did not clear the contract status")
	}

	device.updateContractStatus(testingContractErrorStatus(protocol.ContractError_NoPermission))
	wantLines := []string{line + "\n", line + " (99 suppressed)\n"}
	if lines := logger.contractErrorLines(); !slices.Equal(lines, wantLines) {
		t.Fatalf("the error after the disconnect logged %q, want %q", lines, wantLines)
	}
}
