package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A route that takes each pooled send, returns it to its pool, and reports it
// without ever waiting, so a packet the test did not count cannot hold the
// probe harness's pump, its only caller.
type probeHarnessTestRoute struct {
	sent chan struct{}
}

func (self *probeHarnessTestRoute) SendPacket(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	connect.MessagePoolReturn(packet)
	select {
	case self.sent <- struct{}{}:
	default:
	}
	return true
}

func (self *probeHarnessTestRoute) SendPacketBatch(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	packets [][]byte,
	timeout time.Duration,
) int {
	for _, packet := range packets {
		self.SendPacket(source, provideMode, packet, timeout)
	}
	return len(packets)
}

func (self *probeHarnessTestRoute) Close() {
}

func (self *probeHarnessTestRoute) Shuffle() {
}

func (self *probeHarnessTestRoute) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}

func (self *probeHarnessTestRoute) SetLocalSecurityBypass(localSecurityBypass bool) {
}

// The probe harness's pump reads pooled packets from its tun and hands the
// device a copy of each, so every read is the pump's to return. It returned
// none, and each packet a probe sent stayed outstanding in the pools. Three
// datagrams leave the harness's stack toward a documentation address; once
// the route has taken their copies and the harness has closed, the process's
// pools hold no root they did not hold before. The pool count is the
// process's, so the test runs in a process of its own.
func TestProbeHarnessReturnsEveryPacketItReads(t *testing.T) {
	if runIsolatedLoadTest(t) {
		return
	}
	route := &probeHarnessTestRoute{sent: make(chan struct{}, 16)}
	device := newPacketBatchTestDevice(route)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device.ctx, device.cancel, device.log = ctx, cancel, connect.NewNoopLogger()
	baselineRoots, baselineBytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount()

	harness, err := newProbeHarness(ctx, device)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := harness.tun.DialContext(ctx, "udp", "192.0.2.53:53")
	if err != nil {
		harness.close()
		t.Fatal(err)
	}
	for index := range 3 {
		if _, err := conn.Write([]byte{byte(index)}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		select {
		case <-route.sent:
		case <-time.After(10 * time.Second):
			// bounds a missing premise only
			t.Fatal("the probe pump did not send the datagrams its stack emitted")
		}
	}
	conn.Close()
	harness.close()
	if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
		t.Fatalf("the probe harness left pooled roots outstanding: roots=%d/%d bytes=%d/%d", roots, baselineRoots, bytes, baselineBytes)
	}
}
