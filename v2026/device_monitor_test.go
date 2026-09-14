// Security-policy monitor tests guard background-poll ownership and bounded
// diagnostic logging.
package sdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// testingSecurityPolicyMonitorLogger counts emitted records without writing to
// the process log.
type testingSecurityPolicyMonitorLogger struct {
	infoCount atomic.Int64
}

// TestSecurityPolicyMonitorCloseAndWaitJoinsRun verifies that an explicitly
// enabled diagnostic poll has a deterministic owner-visible retirement edge.
func TestSecurityPolicyMonitorCloseAndWaitJoinsRun(t *testing.T) {
	monitor := newSecurityPolicyMonitor(context.Background(), nil, true)
	select {
	case <-monitor.started:
	case <-time.After(time.Second):
		t.Fatal("security-policy monitor did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := monitor.CloseAndWait(ctx); err != nil {
		t.Fatalf("close security-policy monitor: %v", err)
	}
	select {
	case <-monitor.done:
	default:
		t.Fatal("security-policy monitor returned before its run loop exited")
	}
}

func (self *testingSecurityPolicyMonitorLogger) Info(args ...any) {
	self.infoCount.Add(1)
}

func (self *testingSecurityPolicyMonitorLogger) Infof(format string, args ...any) {
	self.infoCount.Add(1)
}

func (self *testingSecurityPolicyMonitorLogger) Warningf(format string, args ...any) {
}

func (self *testingSecurityPolicyMonitorLogger) Errorf(format string, args ...any) {
}

func (self *testingSecurityPolicyMonitorLogger) V(level int32) connect.Verbose {
	return testingSecurityPolicyMonitorVerbose{}
}

// testingSecurityPolicyMonitorVerbose keeps verbose logger calls disabled.
type testingSecurityPolicyMonitorVerbose struct {
}

func (self testingSecurityPolicyMonitorVerbose) Enabled() bool {
	return false
}

func (self testingSecurityPolicyMonitorVerbose) Info(args ...any) {
}

func (self testingSecurityPolicyMonitorVerbose) Infof(format string, args ...any) {
}

// TestDefaultDeviceSettingsDoNotPollSecurityPolicy verifies that ordinary app
// construction cannot start the process-global diagnostic poll.
func TestDefaultDeviceSettingsDoNotPollSecurityPolicy(t *testing.T) {
	if DefaultDeviceLocalSettings().Verbose {
		t.Fatal("default device settings enable background security-policy polling")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if monitor := newSecurityPolicyMonitor(ctx, nil, false); monitor != nil {
		t.Fatal("disabled security-policy monitor was started")
	}
}

// TestSecurityPolicyMonitorLoggingIsResultBounded verifies that a large
// per-port snapshot produces constant-sized logging rather than one log call
// per destination.
func TestSecurityPolicyMonitorLoggingIsResultBounded(t *testing.T) {
	logger := &testingSecurityPolicyMonitorLogger{}
	stats := connect.SecurityPolicyStats{
		connect.SecurityPolicyResultAllow:    map[connect.SecurityDestination]uint64{},
		connect.SecurityPolicyResultIncident: map[connect.SecurityDestination]uint64{},
	}
	for port := 1; port <= 4096; port++ {
		destination := connect.SecurityDestination{
			Version:  4,
			Protocol: connect.IpProtocolTcp,
			Port:     port,
		}
		stats[connect.SecurityPolicyResultAllow][destination] = 1
		stats[connect.SecurityPolicyResultIncident][destination] = 2
	}

	printSecurityPolicyStats(logger, "ingress", stats)
	const expectedLogCount = 3
	if count := logger.infoCount.Load(); count != expectedLogCount {
		t.Fatalf(
			"security-policy log count = %d, want header plus one line per result (%d)",
			count,
			expectedLogCount,
		)
	}
}
