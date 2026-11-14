// The security reason of a block action reaches the apps: from connect through
// DeviceLocal, across the device rpc, and the verbose reason diagnostics.
package sdk

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestDeviceLocalBlockActionCarriesReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	settings.BlockActionWindowDuration = time.Minute
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	now := time.Now()
	device.updateBlockActions([]*connect.BlockAction{
		{
			Time:   now,
			Ips:    []netip.Addr{netip.MustParseAddr("203.0.113.80")},
			Block:  true,
			Reason: connect.BlockActionReasonSecurityEncrypted,
		},
		{
			Time:    now,
			Ips:     []netip.Addr{netip.MustParseAddr("203.0.113.81")},
			Block:   true,
			Blocker: true,
			Reason:  connect.BlockActionReasonBlocker,
		},
	})

	window := device.GetBlockActions()
	if window.BlockActions.Len() != 2 {
		t.Fatalf("windowed actions = %d, want 2", window.BlockActions.Len())
	}
	reasons := map[string]*BlockAction{}
	for i := 0; i < window.BlockActions.Len(); i += 1 {
		blockAction := window.BlockActions.Get(i)
		reasons[blockAction.Reason] = blockAction
	}
	security := reasons[BlockActionReasonSecurityEncrypted]
	if security == nil || !security.IsSecurity() || !security.RouteLocalOverridable() {
		t.Fatalf("security action = %+v, want a route-local overridable security block", security)
	}
	blocker := reasons[BlockActionReasonBlocker]
	if blocker == nil || blocker.IsSecurity() || blocker.RouteLocalOverridable() {
		t.Fatalf("blocker action = %+v, want a non-security block", blocker)
	}

	// the device rpc carries the reason
	roundTrip := newBlockActionRpc(security).toBlockAction()
	if roundTrip.Reason != BlockActionReasonSecurityEncrypted {
		t.Fatalf("rpc reason = %q", roundTrip.Reason)
	}
}

func TestBlockActionReasonClassification(t *testing.T) {
	cases := []struct {
		reason      string
		security    bool
		overridable bool
	}{
		{reason: BlockActionReasonSecurityEncrypted, security: true, overridable: true},
		{reason: BlockActionReasonSecurityPort, security: true, overridable: true},
		{reason: BlockActionReasonSecurityBittorrent, security: true},
		{reason: BlockActionReasonSecurityIp, security: true},
		{reason: BlockActionReasonSecuritySmtp, security: true},
		{reason: BlockActionReasonSecurity, security: true},
		{reason: BlockActionReasonBlocker},
		{reason: BlockActionReasonOverride},
		{reason: ""},
	}
	for _, c := range cases {
		blockAction := &BlockAction{Reason: c.reason}
		if blockAction.IsSecurity() != c.security || blockAction.RouteLocalOverridable() != c.overridable {
			t.Errorf("%q: security=%t overridable=%t, want %t %t", c.reason, blockAction.IsSecurity(), blockAction.RouteLocalOverridable(), c.security, c.overridable)
		}
	}
}

type testingSecurityPolicyReasonLogger struct {
	testingSecurityPolicyMonitorLogger
	lines []string
}

func (self *testingSecurityPolicyReasonLogger) Infof(format string, args ...any) {
	self.testingSecurityPolicyMonitorLogger.Infof(format, args...)
	self.lines = append(self.lines, format)
}

// TestSecurityPolicyMonitorReasonsAreBounded verifies the reason diagnostics
// print one line per reason regardless of port cardinality.
func TestSecurityPolicyMonitorReasonsAreBounded(t *testing.T) {
	logger := &testingSecurityPolicyReasonLogger{}
	reasons := connect.SecurityPolicyReasonStats{
		connect.SecurityPolicyReasonDropEncrypted:  map[connect.SecurityDestination]uint64{},
		connect.SecurityPolicyReasonAllowWireGuard: map[connect.SecurityDestination]uint64{},
	}
	for port := 1; port <= 4096; port++ {
		destination := connect.SecurityDestination{Version: 4, Protocol: connect.IpProtocolUdp, Port: port}
		reasons[connect.SecurityPolicyReasonDropEncrypted][destination] = uint64(port)
		reasons[connect.SecurityPolicyReasonAllowWireGuard][destination] = 1
	}
	printSecurityPolicyReasons(logger, "egress", reasons)
	if count := logger.infoCount.Load(); count != 3 {
		t.Fatalf("reason log count = %d, want header plus one line per reason", count)
	}
	if !strings.Contains(logger.lines[1]+logger.lines[2], "across %d ports") {
		t.Fatalf("reason lines = %v", logger.lines)
	}
}
