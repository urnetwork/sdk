package sdk

// Fast DNS on connect with a preset (connect ADBLOCK3RD decision 5, C7):
// during the connect window, while no exit is added, a lookup the preset
// cannot answer yet gets the host fallback's answer. The mux races that
// fallback only while the tunnel is not ready, and with no readiness
// predicate it never races, so the answer also proves the device installs
// the predicate when it builds the mux. A local DoH server stands in for
// URnetwork's default resolver on the host network; nothing leaves the
// machine.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A DoH server that answers every A query with addr.
func testingFastDnsDohServer(t *testing.T, addr [4]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		var parser dnsmessage.Parser
		header, err := parser.Start(query)
		if err != nil {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		question, err := parser.Question()
		if err != nil {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
			ID: header.ID, Response: true, RecursionDesired: true, RecursionAvailable: true,
		})
		builder.StartQuestions()
		builder.Question(question)
		builder.StartAnswers()
		if question.Type == dnsmessage.TypeA {
			builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: addr})
		}
		response, err := builder.Finish()
		if err != nil {
			http.Error(w, "bad response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(response)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDnsPresetFastDnsAnswersDuringTheConnectWindow(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	answer := [4]byte{192, 0, 2, 77}
	server := testingFastDnsDohServer(t, answer)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	fallback := func() *connect.DnsResolverSettings {
		return &connect.DnsResolverSettings{
			EnableLocalDoh:   true,
			LocalDohUrlsIpv4: []string{server.URL + "/dns-query"},
			TlsConfig:        &tls.Config{RootCAs: roots},
		}
	}
	dnsPresetFastDnsFallbackOverride.Store(&fallback)
	t.Cleanup(func() { dnsPresetFastDnsFallbackOverride.Store(nil) })

	device := testingDnsPresetDevice(t, nil)
	device.SetDnsResolverSettings(DnsResolverSettingsWithFallback(testingPresetSettings(t, "alpha-family", ""), true))
	device.SetConnectLocation(testingStoredLocation("connect-window"))
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("no live dns interceptor")
	}
	device.stateLock.Lock()
	dns := device.upgradeMuxLiveSettings.Dns
	device.stateLock.Unlock()
	if dns.Fallback == nil || !dns.FallbackUntilTunnelReady || dns.Failover != nil {
		t.Fatalf("the preset's mux settings: fallback %+v, until ready %t, failover %+v", dns.Fallback, dns.FallbackUntilTunnelReady, dns.Failover)
	}

	server4 := net.ParseIP(device.TunnelDnsAddressesIpv4().Get(0)).To4()
	answers := make(chan []byte, 4)
	unsubscribe := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		_, payload, err := connect.ParseIpPathWithPayload(packet)
		if err != nil {
			return
		}
		select {
		case answers <- append([]byte(nil), payload...):
		default:
		}
	})
	t.Cleanup(unsubscribe)
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.IPv4(10, 0, 0, 20), DstIP: server4, Protocol: layers.IPProtocolUDP}
	udp := &layers.UDP{SrcPort: 43124, DstPort: 53}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	query := &layers.DNS{ID: 4242, RD: true, Questions: []layers.DNSQuestion{{Name: []byte("window.fast-dns.example"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}}}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, udp, query); err != nil {
		t.Fatal(err)
	}
	packet := buffer.Bytes()
	if !device.SendPacket(packet, int32(len(packet))) {
		t.Fatal("the mux did not claim the lookup")
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case payload := <-answers:
			var parser dnsmessage.Parser
			header, err := parser.Start(payload)
			if err != nil || header.ID != 4242 || !header.Response {
				continue
			}
			parser.SkipAllQuestions()
			for {
				answerHeader, err := parser.AnswerHeader()
				if err != nil {
					t.Fatalf("the connect window's answer has no A record: %v", err)
				}
				if answerHeader.Type != dnsmessage.TypeA {
					parser.SkipAnswer()
					continue
				}
				resource, err := parser.AResource()
				if err != nil || resource.A != answer {
					t.Fatalf("the connect window answered %v, want the host fallback's %v", resource.A, answer)
				}
				return
			}
		case <-deadline:
			t.Fatal("no answer during the connect window: the host fallback did not race")
		}
	}
}
