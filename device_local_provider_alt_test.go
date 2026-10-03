package sdk

// The provider's carriers on alt (EXTENDER.md L3, L4). The space derives one
// alt url, the device installs it on every platform transport settings value,
// and the split is then visible on the wire: the H3 carrier sends its packets
// to the alt address while presenting the connect name as sni, and the H1
// websocket still dials the platform host.

import (
	"crypto/tls"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
)

// The space these tests run under. Nothing resolves: the H3 carrier is aimed
// at the loopback alt fixture by the alt url, and the websocket dial is
// observed before its name is resolved.
const (
	testAltProviderPlatformUrl  = "wss://connect.space.example"
	testAltProviderPlatformHost = "connect.space.example"
)

// One in-process alt: a quic listener on loopback that reports the sni of
// every client hello it saw. It never completes a handshake -- the client
// verifies the platform certificate chain and this one is self signed -- which
// is all the address and sni assertions need.
func newTestAltProviderListener(t *testing.T) (int, chan string) {
	t.Helper()
	certPem, keyPem, err := generateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	serverNames := make(chan string, 8)
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
				select {
				case serverNames <- clientHello.ServerName:
				default:
				}
				return nil, nil
			},
		},
		&quic.Config{MaxIdleTimeout: 10 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().(*net.UDPAddr).Port, serverNames
}

// One provider over the phase 6 provider client fixture, with the transport
// settings the device builds for a space whose alt url is `altUrl`. No family
// urls, so the group is the single standby carrier and one target mode decides
// what it dials.
func newTestAltProvider(
	t *testing.T,
	altUrl string,
	targetMode connect.TransportMode,
	dialNetworkHook func(network string, addr string),
) *deviceLocalProvider {
	t.Helper()
	client, _ := newTestProviderClient(t)

	// one dialer, so a websocket dial is one observation rather than a race
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ConnectSettings.DialNetworkHook = dialNetworkHook
	clientStrategy := connect.NewClientStrategy(client.Ctx(), strategySettings)
	t.Cleanup(clientStrategy.Close)

	memoryTargetByteCount := ByteCount(24 * 1024 * 1024)
	return &deviceLocalProvider{
		ctx:                    client.Ctx(),
		client:                 client,
		clientStrategy:         clientStrategy,
		clientStrategySettings: strategySettings,
		platformUrl:            testAltProviderPlatformUrl,
		platformTransportSettings: newDeviceLocalPlatformTransportSettings(
			memoryTargetByteCount,
			connect.NewPlatformTransportBudgetForMemoryTarget(memoryTargetByteCount),
			nil,
			altUrl,
			"",
		),
		targetMode:              targetMode,
		modePreferences:         connect.DefaultTransportModePreferences(),
		transportPolicyVersion:  1,
		migrateConnectTimeout:   platformTransportMigrateConnectTimeout,
		migrateMaxScheduleDelay: platformTransportMigrateMaxScheduleDelay,
		auth: &connect.ClientAuth{
			ByJwt:      "test",
			InstanceId: connect.NewId(),
			AppVersion: "0.0.0",
		},
	}
}

// The provider's H3 carrier reaches the in-process alt at the alt address,
// presenting the platform's connect name as sni: alt dispatches on that name
// and holds the connect certificate for it, so nothing about the request moves
// to an alt name (L1, L4).
func TestDeviceLocalProviderAltUrlDialsH3ToAlt(t *testing.T) {
	altPort, serverNames := newTestAltProviderListener(t)
	altUrl := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(altPort))

	provider := newTestAltProvider(t, altUrl, connect.TransportModeH3, nil)
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)

	select {
	case serverName := <-serverNames:
		if serverName != testAltProviderPlatformHost {
			t.Fatalf("sni = %q, expected the platform host", serverName)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the h3 carrier never reached the alt address")
	}
}

// The H1 websocket keeps the platform url while the alt url is in force: alt
// serves no websocket at all, so a carrier that followed it would never
// connect (L1, L4).
func TestDeviceLocalProviderAltUrlKeepsH1OnThePlatformHost(t *testing.T) {
	altPort, _ := newTestAltProviderListener(t)
	altUrl := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(altPort))

	var stateLock sync.Mutex
	wsAddrs := []string{}
	dialed := make(chan struct{}, 8)
	provider := newTestAltProvider(t, altUrl, connect.TransportModeH1,
		func(network string, addr string) {
			stateLock.Lock()
			defer stateLock.Unlock()
			wsAddrs = append(wsAddrs, addr)
			select {
			case dialed <- struct{}{}:
			default:
			}
		})
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)

	deadline := time.After(60 * time.Second)
	for {
		stateLock.Lock()
		haveWs := 0 < len(wsAddrs)
		stateLock.Unlock()
		if haveWs {
			break
		}
		select {
		case <-dialed:
		case <-deadline:
			t.Fatal("the h1 carrier never dialed")
		}
	}

	stateLock.Lock()
	defer stateLock.Unlock()
	for _, addr := range wsAddrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("the websocket dialed %q", addr)
		}
		if host != testAltProviderPlatformHost {
			t.Fatalf("the websocket dialed %q, expected the platform host", addr)
		}
	}
}
