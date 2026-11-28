package sdk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The carrier policy lines (support inbox 8667). After a disconnect a Windows
// service dialed only QUIC on an IPv4-only network and never H1, most likely
// under a Transports choice the person had made, but no log named the policy
// in force, so it could not be told. The device logs the client and the
// provider policy at default verbosity when it is built and on every change,
// with only mode names and priorities.

// Records the formatted transport policy lines a device logs.
type testingTransportPolicyLogger struct {
	connect.Logger
	stateLock sync.Mutex
	lines     []string
}

func newTestingTransportPolicyLogger() *testingTransportPolicyLogger {
	return &testingTransportPolicyLogger{
		Logger: connect.NewNoopLogger(),
	}
}

func (self *testingTransportPolicyLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(line, "[device]transport policy") &&
		!strings.HasPrefix(line, "[device]provider transport policy") {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testingTransportPolicyLogger) policyLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func TestDeviceLocalLogsTheTransportPolicyOnBuildAndChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	logger := newTestingTransportPolicyLogger()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.ClientSettings.Log = logger
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	expectedLines := []string{
		"[device]transport policy = mode=auto auto=h1:1,h3:2,dns:3,dnspump:4 (built)\n",
		"[device]provider transport policy = mode=auto auto=h1:1,h3:2,dns:3,dnspump:4 (built)\n",
	}
	if lines := logger.policyLines(); !slices.Equal(lines, expectedLines) {
		t.Fatalf("built: logged %q, expected %q", lines, expectedLines)
	}

	// an Auto policy without h1, which dials only quic, and a provider pinned to h1
	device.SetTransportSettings(testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 4},
		&TransportModePriority{Mode: TransportModeH3, Priority: 2},
	))
	device.SetProviderTransportSettings(testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	))
	expectedLines = append(
		expectedLines,
		"[device]transport policy = mode=auto auto=h3:2,dnspump:4 (changed)\n",
		"[device]provider transport policy = mode=h1 auto=h1:1 (changed)\n",
	)
	if lines := logger.policyLines(); !slices.Equal(lines, expectedLines) {
		t.Fatalf("changed: logged %q, expected %q", lines, expectedLines)
	}

	// a write equal to the policy in force changes nothing and logs nothing
	device.SetTransportSettings(device.GetTransportSettings())
	device.SetProviderTransportSettings(device.GetProviderTransportSettings())
	if lines := logger.policyLines(); !slices.Equal(lines, expectedLines) {
		t.Fatalf("unchanged: logged %q, expected %q", lines, expectedLines)
	}
}
