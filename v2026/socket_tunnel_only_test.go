package sdk

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A remote route that accepts every packet and counts it. It owns each
// packet it accepts, so it returns it to the pool.
type tunnelOnlyTestRemote struct {
	packetCount atomic.Int64
}

func (self *tunnelOnlyTestRemote) SendPacket(source connect.TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	self.packetCount.Add(1)
	connect.MessagePoolReturn(packet)
	return true
}

func (self *tunnelOnlyTestRemote) SendPacketBatch(source connect.TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	for _, packet := range packets {
		self.SendPacket(source, provideMode, packet, timeout)
	}
	return len(packets)
}

func (self *tunnelOnlyTestRemote) Close()   {}
func (self *tunnelOnlyTestRemote) Shuffle() {}
func (self *tunnelOnlyTestRemote) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}
func (self *tunnelOnlyTestRemote) SetLocalSecurityBypass(localSecurityBypass bool) {}

// A remote route with only the original singular send contract.
type tunnelOnlyTestSingularRemote struct {
	remote tunnelOnlyTestRemote
}

func (self *tunnelOnlyTestSingularRemote) SendPacket(source connect.TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	return self.remote.SendPacket(source, provideMode, packet, timeout)
}
func (self *tunnelOnlyTestSingularRemote) Close()   {}
func (self *tunnelOnlyTestSingularRemote) Shuffle() {}
func (self *tunnelOnlyTestSingularRemote) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}
func (self *tunnelOnlyTestSingularRemote) SetLocalSecurityBypass(localSecurityBypass bool) {}

// A udp datagram to a documentation address.
func tunnelOnlyTestPacket(t *testing.T) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version: 4, TTL: 64, Protocol: layers.IPProtocolUDP,
		SrcIP: net.IPv4(192, 0, 2, 10), DstIP: net.IPv4(198, 51, 100, 20),
	}
	udp := &layers.UDP{SrcPort: 40000, DstPort: 9}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, udp, gopacket.Payload([]byte("tunnel only"))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// The tunnel-only send never takes the local route: with local routing on and
// no remote route it drops and counts the burst, where the developer sockets'
// send would leave through the local nat; with a remote route only the remote
// carries it; and when the remote route ends it drops again.
func TestDeviceLocalTunnelOnlySendNeverTakesTheLocalRoute(t *testing.T) {
	connect.MessagePoolReturn(connect.MessagePoolGet(256))
	connect.GetMessagePoolAggregateStats()
	baselineRoots := connect.MessagePoolOutstandingCount()
	baselineBytes := connect.MessagePoolOutstandingByteCount()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	natSettings := connect.DefaultLocalUserNatSettings()
	natSettings.Log = connect.NewNoopLogger()
	nat := connect.NewLocalUserNat(ctx, "tunnel only send", natSettings)
	defer func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := nat.CloseAndWait(join); err != nil {
			t.Errorf("nat did not join: %v", err)
		}
	}()
	provider := &deviceLocalProvider{localUserNat: nat}

	device := newPacketBatchTestDevice(nil)
	device.sendRoute.Store(&deviceLocalSendRoute{routeLocal: true, provider: provider})
	packet := tunnelOnlyTestPacket(t)

	// the developer sockets follow local routing out of the host's network
	if sent := device.sendPacketsNoCopy([][]byte{connect.MessagePoolCopy(packet)}); sent != 1 {
		t.Fatalf("the route's send sent %d, want 1 through the local nat", sent)
	}
	if localCount := device.localFallbackEgressPacketCount.Load(); localCount != 1 {
		t.Fatalf("local egress %d, want 1", localCount)
	}

	if device.tunnelRouteConnected() {
		t.Fatal("local routing counts as a tunnel")
	}
	if sent := device.sendPacketsTunnelOnly([][]byte{connect.MessagePoolCopy(packet), connect.MessagePoolCopy(packet)}); sent != 0 {
		t.Fatalf("the tunnel-only send sent %d with no remote route, want 0", sent)
	}
	if localCount, dropCount := device.localFallbackEgressPacketCount.Load(), device.tunnelOnlyDropPacketCount.Load(); localCount != 1 || dropCount != 2 {
		t.Fatalf("local egress %d drops %d, want the local egress unchanged at 1 and 2 drops", localCount, dropCount)
	}

	remote := &tunnelOnlyTestRemote{}
	device.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: remote, routeLocal: true, provider: provider})
	if !device.tunnelRouteConnected() {
		t.Fatal("the remote route does not count as a tunnel")
	}
	if sent := device.sendPacketsTunnelOnly([][]byte{connect.MessagePoolCopy(packet)}); sent != 1 || remote.packetCount.Load() != 1 {
		t.Fatalf("the tunnel-only send sent %d, the remote got %d; want both 1", sent, remote.packetCount.Load())
	}

	// a remote route with only the singular contract still carries it, where
	// the route's own send falls back to the local nat
	singular := &tunnelOnlyTestSingularRemote{}
	device.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: singular, routeLocal: true, provider: provider})
	if sent := device.sendPacketsTunnelOnly([][]byte{connect.MessagePoolCopy(packet)}); sent != 1 || singular.remote.packetCount.Load() != 1 {
		t.Fatalf("the tunnel-only send sent %d, the singular remote got %d; want both 1", sent, singular.remote.packetCount.Load())
	}
	if localCount := device.localFallbackEgressPacketCount.Load(); localCount != 1 {
		t.Fatalf("local egress %d with a remote route, want it unchanged at 1", localCount)
	}

	// the remote route ends under an open connection
	device.sendRoute.Store(&deviceLocalSendRoute{routeLocal: true, provider: provider})
	if sent := device.sendPacketsTunnelOnly([][]byte{connect.MessagePoolCopy(packet)}); sent != 0 {
		t.Fatalf("the tunnel-only send sent %d after the remote route ended, want 0", sent)
	}
	if localCount, dropCount := device.localFallbackEgressPacketCount.Load(), device.tunnelOnlyDropPacketCount.Load(); localCount != 1 || dropCount != 3 {
		t.Fatalf("local egress %d drops %d, want 1 and 3", localCount, dropCount)
	}

	cancel()
	join, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := nat.CloseAndWait(join); err != nil {
		t.Fatalf("nat did not join: %v", err)
	}
	if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
		t.Fatalf("packets retained: roots=%d/%d bytes=%d/%d", roots, baselineRoots, bytes, baselineBytes)
	}
}

// On a real device with local routing on and no destination, the developer
// socket reaches a loopback origin through the local nat, the host's own
// network; the tunnel-only dial refuses at once and the origin sees nothing.
func TestDeviceLocalTunnelOnlyDialRefusesTheLocalRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()
	device.SetRouteLocal(true)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	origin := listener.Addr().String()

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	conn, err := device.DialContext(dialCtx, "tcp", origin)
	if err != nil {
		t.Fatalf("the developer socket did not reach the origin through the local route: %v", err)
	}
	defer conn.Close()
	select {
	case originConn := <-accepted:
		originConn.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("the origin did not see the developer socket")
	}

	if _, err := device.DialTunnelOnlyContext(dialCtx, "tcp", origin); !errors.Is(err, ErrTunnelNotConnected) {
		t.Fatalf("the tunnel-only dial returned %v, want ErrTunnelNotConnected", err)
	}
	if _, err := device.DialTunnelOnlyContext(dialCtx, "udp", origin); !errors.Is(err, ErrTunnelNotConnected) {
		t.Fatalf("the tunnel-only udp dial returned %v, want ErrTunnelNotConnected", err)
	}
	select {
	case originConn := <-accepted:
		originConn.Close()
		t.Fatal("the origin saw a connection from the tunnel-only dial")
	default:
	}
	// a refused dial makes no tunnel-only tun
	device.tunnelOnlySockets.mu.Lock()
	tun := device.tunnelOnlySockets.tun
	device.tunnelOnlySockets.mu.Unlock()
	if tun != nil {
		t.Fatal("a refused tunnel-only dial created its tun")
	}
}
