// A real SDK socket's TUN reader must reach a later local control packet while
// the actual remote window is empty. No route, TUN read, or send is substituted.

//go:build darwin || linux || android || ios

package sdk

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

// The existing public security-policy boundary is an observation barrier only.
// The embedded real policy still makes the decision and retains no packet data.
type privateNativeGoReadPolicy struct {
	connect.SecurityPolicy
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

// Signals that the real socket reader drained its first remote SYN before the
// second packet is generated. Provider admission is still the real empty window.
func (self *privateNativeGoReadPolicy) InspectEgress(mode protocol.ProvideMode, path *connect.IpPath, payload []byte) (connect.SecurityPolicyResult, error) {
	if path.Protocol == connect.IpProtocolTcp && path.DestinationPort == 443 && path.DestinationIp.Equal(net.IPv4(203, 0, 113, 44)) {
		self.once.Do(func() {
			close(self.entered)
			<-self.release
		})
	}
	return self.SecurityPolicy.InspectEgress(mode, path, payload)
}

// The first real socket dial emits a remote SYN. Only after its actual policy
// visit does a second real UDP socket emit the locally served resolver.arpa
// question. OLD leaves that later native read behind its five-second send;
// FIX refuses remote admission and returns the exact local response immediately.
func TestPrivateNativeGoSocketNextReadPrecedesRemoteReadiness(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	f := newNativeIngressDeviceFixture(t, func(policy connect.SecurityPolicy) connect.SecurityPolicy {
		return &privateNativeGoReadPolicy{SecurityPolicy: policy, entered: entered, release: release}
	})
	t.Cleanup(func() {
		unpark()
		f.cancel()
		f.device.sockets.closeAndWait()
	})
	if f.device.settings.SendTimeout != 5*time.Second {
		t.Fatal("native socket proof changed the generic five-second send contract")
	}
	// Both addresses below are literals; no external resolver or host dial runs.
	dialDone := make(chan struct{})
	f.workerDone = dialDone
	go func() {
		defer close(dialDone)
		conn, _ := f.device.DialContext(f.device.ctx, "tcp4", "203.0.113.44:443")
		if conn != nil {
			_ = conn.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("real socket reader did not reach the first remote offer")
	}
	control, err := f.device.DialContext(context.Background(), "udp4", net.JoinHostPort(connect.DefaultDnsUpgradeMaskAddress, "53"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_, dnsPacket := nativeIngressDevicePackets(t)
	_, question, err := connect.ParseIpPathWithPayload(dnsPacket)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := control.Write(question); err != nil || count != len(question) {
		t.Fatalf("later native control write failed: count=%d", count)
	}
	released := time.Now()
	unpark()
	// gVisor's custom park is not a synctest durable wait. As in the existing
	// native FD/packed roots, this watchdog is below the unchanged remote budget;
	// the exact policy barrier proves that this question is a later native batch.
	select {
	case reply := <-f.replies:
		requireNativeIngressDnsReply(t, reply)
		if f.device.settings.SendTimeout <= time.Since(released) {
			t.Fatal("later local control response waited through the generic send budget")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NATIVE_GO_SOCKET_NEXT_READ_WAITED_FOR_REMOTE: later local control packet stayed behind generic remote admission")
	}
	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 2048)
	count, err := control.Read(response)
	if err != nil {
		t.Fatal("actual local control response did not reach its socket")
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(response[:count])
	if err != nil || !header.Response || header.ID != 31337 {
		t.Fatal("actual socket received a different control response")
	}
}
