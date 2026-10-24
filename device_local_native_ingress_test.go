// Native adapters must return an unavailable remote offer before reading the
// next independently serviceable packet. No fake route implements this policy.

//go:build darwin || linux || android || ios

package sdk

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

// The actual empty remote window and actual local resolver are independent:
// no remote packet is admitted, while the resolver owns its ordinary reply.
type nativeIngressDeviceFixture struct {
	device     *DeviceLocal
	cancel     context.CancelFunc
	remote     *connect.RemoteUserNatMultiClient
	mux        *connect.UpgradeMux
	replies    chan []byte
	workerDone <-chan struct{}
}

func newNativeIngressDeviceFixture(t *testing.T, observePolicies ...func(connect.SecurityPolicy) connect.SecurityPolicy) *nativeIngressDeviceFixture {
	t.Helper()
	for _, size := range []int{60, 256, 2048} {
		connect.MessagePoolReturn(connect.MessagePoolGet(size))
	}
	baselineRoots := connect.MessagePoolOutstandingCount()
	baselineBytes := connect.MessagePoolOutstandingByteCount()
	previousViolation := connect.SetMessagePoolViolationHandler(func(err error, _ []byte) {
		t.Errorf("native adapter pool custody: %v", err)
	})
	t.Cleanup(func() { connect.SetMessagePoolViolationHandler(previousViolation) })
	ctx, cancel := context.WithCancel(context.Background())
	settings := connect.DefaultMultiClientSettings()
	settings.Log = connect.NewNoopLogger()
	settings.ProviderProbe = false
	settings.SecurityPolicyGenerator = func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy {
		policy := connect.DisableSecurityPolicy()
		for _, observePolicy := range observePolicies {
			policy = observePolicy(policy)
		}
		return policy
	}
	remote := connect.NewRemoteUserNatMultiClient(ctx, &testingDnsOwnerGenerator{}, nil, protocol.ProvideMode_Network, settings)
	device := newPacketBatchTestDevice(remote)
	device.ctx, device.cancel, device.log = ctx, cancel, connect.NewNoopLogger()
	f := &nativeIngressDeviceFixture{device: device, cancel: cancel, remote: remote, replies: make(chan []byte, 2)}
	mux, err := connect.NewUpgradeMux(ctx, connect.SourceId(device.clientId), protocol.ProvideMode_Network,
		device.settings.SendTimeout,
		func(source connect.TransferPath, mode protocol.ProvideMode, path *connect.IpPath, packet []byte) {
			device.receivePackets(source, mode, path, [][]byte{packet})
		}, testingDnsOwnerSettings(), device.log)
	if err != nil {
		cancel()
		remote.Close()
		t.Fatal(err)
	}
	f.mux = mux
	mux.SetUpstreamBatchClient(remote)
	device.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: remote, upgradeMux: mux})
	unsubscribe := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		f.replies <- append([]byte(nil), packet...)
	})
	t.Cleanup(func() {
		cancel()
		mux.Close()
		if f.workerDone != nil {
			select {
			case <-f.workerDone:
			case <-time.After(5 * time.Second):
				t.Error("packed native caller did not join after cancellation")
			}
		}
		unsubscribe()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		if err := remote.CloseAndWait(joinCtx); err != nil {
			t.Errorf("join actual native remote owner: %v", err)
		}
		if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
			t.Errorf("native adapter retained pooled roots after joins: roots=%d/%d bytes=%d/%d", roots, baselineRoots, bytes, baselineBytes)
		}
	})
	return f
}

// Synthetic addresses and resolver.arpa are local fixture inputs, not hosted
// DNS or provider requests. The TCP packet cannot borrow the resolver's route.
func nativeIngressDevicePackets(t *testing.T) (remotePacket, dnsPacket []byte) {
	t.Helper()
	remotePacket = craftIpv4Packet(connect.IpProtocolTcp, net.IPv4(10, 0, 0, 20), 43122,
		net.IPv4(203, 0, 113, 44), 443, true, nil)
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.IPv4(10, 0, 0, 20),
		DstIP: net.ParseIP(connect.DefaultDnsUpgradeMaskAddress).To4(), Protocol: layers.IPProtocolUDP}
	udp := &layers.UDP{SrcPort: 43123, DstPort: 53}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	dns := &layers.DNS{ID: 31337, RD: true, Questions: []layers.DNSQuestion{
		{Name: []byte("_dns.resolver.arpa"), Type: layers.DNSTypeA, Class: layers.DNSClassIN},
	}}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, udp, dns); err != nil {
		t.Fatal(err)
	}
	return remotePacket, buffer.Bytes()
}

func requireNativeIngressDnsReply(t *testing.T, packet []byte) {
	t.Helper()
	path, payload, err := connect.ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(payload)
	if err != nil || !header.Response || header.ID != 31337 ||
		!path.SourceIp.Equal(net.ParseIP(connect.DefaultDnsUpgradeMaskAddress)) {
		t.Fatal("later independently ready packet did not produce its exact local DNS response")
	}
}

// The Apple entry's serialized next read is represented by a second packed
// call made only after the first has returned, not by a parallel send worker.
func TestDeviceLocalNativePackedNextReadDoesNotWaitForRemote(t *testing.T) {
	f := newNativeIngressDeviceFixture(t)
	remotePacket, dnsPacket := nativeIngressDevicePackets(t)
	first := packetBatchTestEncode([][]byte{remotePacket})
	second := packetBatchTestEncode([][]byte{dnsPacket})
	done := make(chan [2]int32, 1)
	workerDone := make(chan struct{})
	f.workerDone = workerDone
	go func() {
		defer close(workerDone)
		firstCount := f.device.SendPacketBatch(first)
		secondCount := f.device.SendPacketBatch(second)
		done <- [2]int32{firstCount, secondCount}
	}()
	// gVisor's custom park is not a synctest durable wait. Keep the real
	// production stack and bound this caller independently of its unchanged
	// five-second remote budget; never extend or fake that budget.
	select {
	case counts := <-done:
		if counts != [2]int32{0, 1} {
			t.Fatalf("packed native counts=%v; want independent refusal/admission", counts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NATIVE_PACKED_NEXT_READ_WAITED_FOR_REMOTE: five-second provider admission owns the native caller")
	}
	select {
	case reply := <-f.replies:
		requireNativeIngressDnsReply(t, reply)
	case <-time.After(2 * time.Second):
		t.Fatal("later native packed DNS request did not complete before remote readiness")
	}
}

// The existing observation clock supplies a barrier after the first native
// read/drain, before its send. Thus the second datagram is provably a later
// native batch. The two-second watchdog is shorter than the unchanged remote
// five-second wait; it bounds test failure, not production admission policy.
func TestDeviceLocalNativeFdNextReadDoesNotWaitForRemote(t *testing.T) {
	f := newNativeIngressDeviceFixture(t)
	remotePacket, dnsPacket := nativeIngressDevicePackets(t)
	fd, peer := testingIoLoopSocketPair(t)
	if count, err := peer.Write(remotePacket); err != nil || count != len(remotePacket) {
		peer.Close()
		syscall.Close(int(fd))
		t.Fatalf("preload first native packet: n=%d err=%v", count, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	observation := newIoLoopObservation(func() time.Time {
		if calls.Add(1) == 3 {
			close(entered)
			select {
			case <-release:
			case <-f.device.ctx.Done():
			}
		}
		return time.Now()
	})
	loop := newIoLoopObserved(f.device, fd, nil, observation)
	t.Cleanup(func() {
		unpark()
		f.cancel()
		loop.Close()
		for _, done := range []<-chan struct{}{observation.runDone, observation.fileClosed} {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("native descriptor owner did not join after cancellation")
			}
		}
		peer.Close()
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first descriptor read did not reach its exact send boundary")
	}
	if count, err := peer.Write(dnsPacket); err != nil || count != len(dnsPacket) {
		t.Fatalf("write later native datagram: n=%d err=%v", count, err)
	}
	unpark()
	select {
	case reply := <-f.replies:
		requireNativeIngressDnsReply(t, reply)
	case <-time.After(2 * time.Second):
		t.Fatal("NATIVE_FD_NEXT_READ_WAITED_FOR_REMOTE: later local DNS packet stayed behind unrelated remote admission")
	}
	loop.Close()
	for _, done := range []<-chan struct{}{observation.runDone, observation.fileClosed} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("native descriptor owner did not join after the local response")
		}
	}
	if observation.send.Count != 2 || observation.offeredPacketCount != 2 ||
		observation.acceptedPacketCount != 1 || observation.refusedPacketCount != 1 || observation.invalidResultCount != 0 {
		t.Fatalf("native next-read custody: sends=%d offered=%d accepted=%d refused=%d invalid=%d",
			observation.send.Count, observation.offeredPacketCount, observation.acceptedPacketCount,
			observation.refusedPacketCount, observation.invalidResultCount)
	}
}
