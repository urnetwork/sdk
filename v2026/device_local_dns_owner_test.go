// A real DeviceLocal builds/replaces the actual mux and sends an intercepted
// DNS query. Only candidate enumeration is empty; no public network is needed.
package sdk

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

type testingDnsOwnerGenerator struct{ rpcLeakTestGenerator }

func (self *testingDnsOwnerGenerator) NextDestinations(count int, excluded []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	return map[connect.MultiHopId]connect.DestinationStats{}, nil
}

func testingDnsOwnerDevice(t *testing.T) *DeviceLocal {
	t.Helper()
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} }
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, fixture.initialJwt, "dns-owner", "test", "0",
		fixture.instanceId, settings, connect.NewId())
	if err != nil {
		t.Fatal("could not build actual DNS owner device")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("DNS owner device did not join")
		}
	})
	return device
}

// Resolver existence controls interception, independent of configured upstream
// transports. Locally served resolver.arpa proves the actual claim/reply path.
func testingDnsOwnerSettings() *connect.UpgradeMuxSettings {
	settings := connect.DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &connect.DnsResolverSettings{DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress}
	settings.Dns.Fallback = nil
	return settings
}

func TestDeviceLocalDnsInterceptorRequiresSelectedLiveMux(t *testing.T) {
	device := testingDnsOwnerDevice(t)
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	if device.GetTunnelDnsInterceptorActive() {
		t.Fatal("configured DNS without a destination claimed interception")
	}
	device.SetConnectLocation(testingStoredLocation("live-destination"))
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("real selected DNS mux was not reported active")
	}
	device.stateLock.Lock()
	first := device.upgradeMux
	device.stateLock.Unlock()
	device.Reconnect(testingStoredLocation("live-destination"))
	device.stateLock.Lock()
	replacement := device.upgradeMux
	device.stateLock.Unlock()
	if first == nil || replacement == nil || first == replacement || !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("replacement did not report only the newly selected mux")
	}
	device.RemoveDestination()
	if device.GetTunnelDnsInterceptorActive() {
		t.Fatal("removed destination retained DNS ownership")
	}
	device.SetConnectLocation(testingStoredLocation("another-live-destination"))
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("healthy destination could not reacquire DNS ownership")
	}
	device.Cancel()
	if device.GetTunnelDnsInterceptorActive() {
		t.Fatal("canceled context retained DNS ownership before Close")
	}
	device.Close()
	if device.GetTunnelDnsInterceptorActive() {
		t.Fatal("closed device retained DNS ownership")
	}
}

func TestDeviceLocalDnsInterceptorTracksAppliedNotFutureSettings(t *testing.T) {
	device := testingDnsOwnerDevice(t)
	disabled := testingDnsOwnerSettings()
	disabled.Dns = nil
	device.SetUpgradeMuxSettings(disabled)
	device.SetConnectLocation(testingStoredLocation("http-only-mux"))
	if !device.GetConnectEnabled() || device.GetTunnelDnsInterceptorActive() {
		t.Fatal("a live non-DNS mux was confused with DNS ownership")
	}
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("live settings did not enable interception")
	}
	disabled = testingDnsOwnerSettings()
	disabled.Dns.Resolver = nil
	device.SetUpgradeMuxSettings(disabled)
	if device.GetTunnelDnsInterceptorActive() {
		t.Fatal("nil resolver retained interception")
	}
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	device.SetUpgradeMuxSettings(nil)
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("future disable hid the still-intercepting live mux")
	}
	device.Reconnect(testingStoredLocation("http-only-mux"))
	if !device.GetConnectEnabled() || device.GetTunnelDnsInterceptorActive() {
		t.Fatal("rebuild without a mux retained DNS ownership")
	}
}

func TestDeviceLocalActiveDnsOwnerAnswersAdvertisedMaskThroughPacketPath(t *testing.T) {
	device := testingDnsOwnerDevice(t)
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	device.SetConnectLocation(testingStoredLocation("dns-packet-path"))
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("no actual selected DNS interceptor")
	}
	addresses := device.TunnelDnsAddressesIpv4()
	if addresses == nil || addresses.Len() == 0 {
		t.Fatal("live mux has no advertised DNS address")
	}
	server := net.ParseIP(addresses.Get(0)).To4()
	if server == nil {
		t.Fatal("advertised synthetic address is not IPv4")
	}
	received := make(chan bool, 1)
	unsubscribe := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		path, payload, err := connect.ParseIpPathWithPayload(packet)
		if err != nil {
			received <- false
			return
		}
		var parser dnsmessage.Parser
		header, err := parser.Start(payload)
		received <- err == nil && header.Response && header.ID == 31337 && path.SourceIp.Equal(server)
	})
	t.Cleanup(unsubscribe)
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.IPv4(10, 0, 0, 20), DstIP: server, Protocol: layers.IPProtocolUDP}
	udp := &layers.UDP{SrcPort: 43123, DstPort: 53}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal("could not prepare DNS checksum")
	}
	dns := &layers.DNS{ID: 31337, RD: true, Questions: []layers.DNSQuestion{{Name: []byte("_dns.resolver.arpa"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}}}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, udp, dns); err != nil {
		t.Fatal("could not prepare DNS packet")
	}
	packet := buffer.Bytes()
	if !device.SendPacket(packet, int32(len(packet))) {
		t.Fatal("selected mux did not claim the DNS packet")
	}
	select {
	case correct := <-received:
		if !correct {
			t.Fatal("selected mux did not answer as the advertised DNS identity")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("actual interceptor produced no DNS response")
	}
}
