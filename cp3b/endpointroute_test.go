package cp3b

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/message-server/api"
	"github.com/urnetwork/message-server/endpoint"
	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/sdk/v2026/urmessage"
)

// THE SERVER AS ITS OWN HOST: THE SAME EXCHANGE, CARRIED BY A PINNED TLS SESSION INSTEAD OF THE
// PLATFORM.
//
// The server half is the real pipeline (peer + api + store) behind msgrepo/endpoint on a loopback
// port; the client half is sdk.MessageRouteClient in DIRECT mode, which is the fallback setting.
// URNETWORK mode differs only in the dialler (sdk/message_tunnel.go), and that needs live exit
// providers, so it is measured against the deployment rather than here.
//
// What this holds, beyond "a message arrives": the server's key is checked BEFORE a single frame
// leaves the device. The second case points the same client at the same server with a pin that is
// not the server's, and the endpoint's own counters show nothing ever arrived.

type endpointWorld struct {
	endpoint      *endpoint.Endpoint
	url           string
	pin           []byte
	serverId      connect.Id
	peer          *peer.Peer
	listener      *trackingListener
	subscriptions *api.Subscriptions
}

// trackingListener remembers every connection it accepts, so a case can cut them all from the
// server's side: what an exit provider leaving the mesh looks like to the server.
type trackingListener struct {
	net.Listener
	mutex sync.Mutex
	conns []net.Conn
}

func (self *trackingListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err == nil {
		self.mutex.Lock()
		self.conns = append(self.conns, conn)
		self.mutex.Unlock()
	}
	return conn, err
}

func (self *trackingListener) dropAll() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, conn := range self.conns {
		conn.Close()
	}
	dropped := len(self.conns)
	self.conns = nil
	return dropped
}

func newEndpointWorld(t *testing.T) *endpointWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	certificate := selfSignedCertificate(t)
	served, err := endpoint.New(ctx, endpoint.Config{Certificate: certificate})
	if err != nil {
		cancel()
		t.Fatalf("endpoint.New: %v", err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("listening: %v", err)
	}
	listener := &trackingListener{Listener: raw}
	go served.Serve(listener)

	connections, err := peer.NewConnections(rand.Reader, time.Now, time.Hour)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewConnections: %v", err)
	}
	checks, err := peer.NewChecks(connections, peer.DefaultMaxRequestBytes)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewChecks: %v", err)
	}
	subscriptions := api.NewSubscriptions()
	handler, err := api.New(api.Config{
		Store:         store.NewMemoryStore(store.DefaultLimits()),
		KnownGroups:   api.NewMemoryKnownGroups(),
		Front:         checks,
		Subscriptions: subscriptions,
	})
	if err != nil {
		cancel()
		t.Fatalf("api.New: %v", err)
	}
	dispatch, err := peer.New(peer.Config{
		Client:      endpoint.Join(served, nil),
		Handler:     handler,
		Connections: connections,
		Checks:      checks,
		Capabilities: &protocol.Capabilities{
			MaxRequestBytes: peer.DefaultMaxRequestBytes,
		},
		ProtocolVersion: worldProtocolVersion,
		ServerId:        bytes.Repeat([]byte{0x5A}, 16),
	})
	if err != nil {
		cancel()
		t.Fatalf("peer.New: %v", err)
	}
	subscriptions.SetPusher(dispatch)
	t.Cleanup(func() {
		dispatch.Close()
		subscriptions.Close()
		served.Close()
		cancel()
	})
	return &endpointWorld{
		endpoint:      served,
		url:           "wss://" + listener.Addr().String() + endpoint.DefaultPath,
		pin:           served.Pin(),
		serverId:      connect.NewId(),
		peer:          dispatch,
		listener:      listener,
		subscriptions: subscriptions,
	}
}

func (self *endpointWorld) route(t *testing.T, pin []byte) *sdk.MessageRouteClient {
	t.Helper()
	client, err := sdk.NewMessageRouteClient(context.Background(), &sdk.MessageRouteConfig{
		Endpoint: self.url,
		Pin:      pin,
		Mode:     sdk.MessageRouteDirect,
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageRouteClient: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func (self *endpointWorld) device(t *testing.T, name string) *urmessage.Device {
	t.Helper()
	device, _, _ := self.deviceAndRoute(t, name)
	return device
}

// deviceAndRoute is [endpointWorld.device] with the route client it speaks over, for a case that
// reads the route's own state.
func (self *endpointWorld) deviceAndRoute(t *testing.T, name string) (*urmessage.Device, *sdk.MessageRouteClient, *sdk.MessageTransport) {
	t.Helper()
	route := self.route(t, self.pin)
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          route,
		Server:          self.serverId,
		ProtocolVersion: worldProtocolVersion,
		Timeout:         30 * time.Second,
	})
	if err != nil {
		t.Fatalf("%s: sdk.NewMessageTransport: %v", name, err)
	}
	t.Cleanup(transport.Close)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("%s: sdk.OpenStreamStore: %v", name, err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport: transport,
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
	})
	if err != nil {
		t.Fatalf("%s: urmessage.NewDevice: %v", name, err)
	}
	t.Cleanup(func() { device.Close() })
	return device, route, transport
}

func TestTheExchangeCrossesTheServersOwnEndpointWithItsKeyPinned(t *testing.T) {
	world := newEndpointWorld(t)
	alice := world.device(t, "alice")
	bob := world.device(t, "bob")

	texts := exchangeBetween(t, alice, bob)

	stats := world.endpoint.Stats()
	if stats.Accepted != 2 {
		t.Errorf("the endpoint accepted %d sessions for two devices", stats.Accepted)
	}
	if stats.FramesIn == 0 || stats.FramesOut == 0 {
		t.Errorf("the exchange completed and the endpoint counts %d frames in and %d out", stats.FramesIn, stats.FramesOut)
	}
	if stats.FramesDropped != 0 {
		t.Errorf("the endpoint dropped %d frames from two well-formed clients", stats.FramesDropped)
	}
	served := world.peer.Stats()
	t.Logf("%d texts crossed; endpoint: %d sessions, %d frames in, %d out; peer served %d responses",
		len(texts), stats.Accepted, stats.FramesIn, stats.FramesOut, served.ResponsesSent)
}

func TestAServerWhoseKeyIsNotThePinnedOneIsNeverSentAFrame(t *testing.T) {
	world := newEndpointWorld(t)

	// the control first: the same server, the right pin, answers a Hello
	right := world.route(t, world.pin)
	assertHello(t, right, world.serverId, true)

	// a pin that is not the server's: one octet different
	wrongPin := append([]byte(nil), world.pin...)
	wrongPin[0] ^= 0x01
	before := world.endpoint.Stats()
	wrong := world.route(t, wrongPin)
	assertHello(t, wrong, world.serverId, false)
	after := world.endpoint.Stats()

	if after.FramesIn != before.FramesIn {
		t.Errorf("%d frames reached a server whose key is not the pinned one", after.FramesIn-before.FramesIn)
	}
	if after.Accepted != before.Accepted {
		t.Errorf("the endpoint completed %d sessions with a client that pinned another key", after.Accepted-before.Accepted)
	}
	if !strings.Contains(wrong.Status().LastError, sdk.ErrMessageRoutePinMismatch.Error()) {
		t.Errorf("the route reports %q, not the pin mismatch", wrong.Status().LastError)
	}
}

// assertHello says Hello once over a fresh transport and checks only whether it was answered.
func assertHello(t *testing.T, client *sdk.MessageRouteClient, server connect.Id, answered bool) {
	t.Helper()
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          client,
		Server:          server,
		ProtocolVersion: worldProtocolVersion,
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageTransport: %v", err)
	}
	defer transport.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	reason, _, err := transport.Hello(ctx, worldProtocolVersion)
	switch {
	case answered && err != nil:
		t.Fatalf("a Hello to the server with its own key pinned failed: %v (route: %+v)", err, client.Status())
	case answered && reason != protocol.Reason_REASON_OK:
		t.Fatalf("a Hello to the server with its own key pinned was answered %v", reason)
	case !answered && err == nil:
		t.Fatal("a Hello was answered by a server whose key is not the pinned one")
	case !answered && !errors.Is(err, context.DeadlineExceeded) && err != nil:
		t.Logf("the refused Hello ended with: %v", err)
	}
}

// selfSignedCertificate is the shape the deployment uses: a P-256 key, self-signed, pinned.
func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "urmessage"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-signing: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate back: %v", err)
	}
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if got, err := endpoint.PinOf(certificate); err != nil || !bytes.Equal(got, pin[:]) {
		t.Fatalf("endpoint.PinOf answers %x, %v; the SPKI hashes to %x", got, err, pin)
	}
	return certificate
}
