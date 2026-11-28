package hotspot

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Bounds a read or write that only a failure would wait out.
const testIoTimeout = 10 * time.Second

// A source address on the synthetic hotspot subnet below.
var testHotspotSource = netip.MustParseAddrPort("192.168.43.20:40000")

func testHotspotInterfaces() *HotspotInterfaceList {
	return newTestHotspotInterfaceList(
		newTestHotspotInterface("rmnet_data0", false, true, "10.20.30.40/24"),
		newTestHotspotInterface("wlan1", true, false, "192.168.43.1/24"),
	)
}

// Listens in place of the hotspot bind, since the test host has no hotspot
// interface: it records each address the proxy asked for and binds a loopback
// listener instead. With a source set, every accepted connection reports it as
// its remote address, standing in for a device on the hotspot subnet.
type testListenRecorder struct {
	source netip.AddrPort

	stateLock sync.Mutex
	requested []netip.AddrPort
	actual    map[netip.AddrPort]string
}

func (self *testListenRecorder) listen(ctx context.Context, address netip.AddrPort) (net.Listener, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requested = append(self.requested, address)
	if self.actual == nil {
		self.actual = map[netip.AddrPort]string{}
	}
	self.actual[address] = listener.Addr().String()
	if self.source.IsValid() {
		return &sourceRewritingListener{
			Listener: listener,
			source:   net.TCPAddrFromAddrPort(self.source),
		}, nil
	}
	return listener, nil
}

func (self *testListenRecorder) requestedAddresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	values := []string{}
	for _, address := range self.requested {
		values = append(values, address.String())
	}
	return values
}

// The loopback address serving the latest listen of the hotspot address.
func (self *testListenRecorder) actualAddress(t *testing.T, requested string) string {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	actual, ok := self.actual[netip.MustParseAddrPort(requested)]
	if !ok {
		t.Fatalf("the proxy never listened on %s", requested)
	}
	return actual
}

type sourceRewritingListener struct {
	net.Listener
	source *net.TCPAddr
}

func (self *sourceRewritingListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sourceRewritingConn{Conn: conn, source: self.source}, nil
}

type sourceRewritingConn struct {
	net.Conn
	source *net.TCPAddr
}

func (self *sourceRewritingConn) RemoteAddr() net.Addr {
	return self.source
}

// The tunnel the proxy dials. Not connected, it refuses as a device with no
// remote route does; connected, it reaches the test origin, standing in for
// the tunnel's exit. It is the proxy's only way out, so the origin seeing a
// connection the tunnel did not dial would be a direct fallback.
type testTunnel struct {
	connected atomic.Bool
	target    string
	dialCount atomic.Int64
}

func (self *testTunnel) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	self.dialCount.Add(1)
	if !self.connected.Load() {
		return nil, sdk.ErrTunnelNotConnected
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, self.target)
}

// A tcp origin that answers ping with pong on each connection and holds the
// connection until the peer closes, counting the connections it accepted.
type testOrigin struct {
	address       string
	acceptedCount atomic.Int64
}

func newTestOrigin(t *testing.T) *testOrigin {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	origin := &testOrigin{address: listener.Addr().String()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			origin.acceptedCount.Add(1)
			go func() {
				defer conn.Close()
				buf := make([]byte, 4)
				for {
					if _, err := io.ReadFull(conn, buf); err != nil {
						return
					}
					if string(buf) == "ping" {
						if _, err := conn.Write([]byte("pong")); err != nil {
							return
						}
					}
				}
			}()
		}
	}()
	return origin
}

func newTestHotspotProxy(
	t *testing.T,
	recorder *testListenRecorder,
	tunnel *testTunnel,
	store hotspotProxyStore,
	adjust func(settings *hotspotProxySettings),
) *HotspotProxy {
	t.Helper()
	settings := defaultHotspotProxySettings()
	settings.listen = recorder.listen
	settings.httpProxySettings.StatsLogInterval = 0
	settings.socksProxySettings.StatsLogInterval = 0
	if adjust != nil {
		adjust(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hotspotProxy := newHotspotProxy(ctx, settings, tunnel.dial, store, connect.NewNoopLogger())
	t.Cleanup(func() {
		hotspotProxy.Close()
		cancel()
	})
	return hotspotProxy
}

func basicProxyAuthorization(username string, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// Sends one CONNECT and reads the answer. On 200 the connection is the
// tunnel and stays open; otherwise it is closed.
func httpProxyConnect(t *testing.T, proxyAddress string, target string, authorization string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddress, testIoTimeout)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	conn.SetDeadline(time.Now().Add(testIoTimeout))
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if authorization != "" {
		request += "Proxy-Authorization: " + authorization + "\r\n"
	}
	request += "\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		t.Fatalf("write connect: %v", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		t.Fatalf("read connect response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		conn.Close()
		return response, nil, nil
	}
	return response, conn, reader
}

// Sends one plain http request for an absolute url through the proxy.
func httpProxyGet(t *testing.T, proxyAddress string, target string, authorization string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddress, testIoTimeout)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testIoTimeout))
	targetUrl, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %q: %v", target, err)
	}
	request := "GET " + target + " HTTP/1.1\r\nHost: " + targetUrl.Host + "\r\nConnection: close\r\n"
	if authorization != "" {
		request += "Proxy-Authorization: " + authorization + "\r\n"
	}
	request += "\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write get: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read get response: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

func pingPong(t *testing.T, conn net.Conn, reader io.Reader) {
	t.Helper()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(reader, pong); err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if string(pong) != "pong" {
		t.Fatalf("read %q, want pong", pong)
	}
}

// The socks5 method selection: the methods offered, then the method chosen.
func socksSelectMethod(t *testing.T, conn net.Conn, methods ...byte) byte {
	t.Helper()
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		t.Fatalf("write socks greeting: %v", err)
	}
	selection := make([]byte, 2)
	if _, err := io.ReadFull(conn, selection); err != nil {
		t.Fatalf("read socks method: %v", err)
	}
	if selection[0] != 0x05 {
		t.Fatalf("socks version %#x", selection[0])
	}
	return selection[1]
}

// The RFC 1929 user/password exchange; returns the status, 0 on success.
func socksAuthenticate(t *testing.T, conn net.Conn, username string, password string) byte {
	t.Helper()
	request := []byte{0x01, byte(len(username))}
	request = append(request, username...)
	request = append(request, byte(len(password)))
	request = append(request, password...)
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write socks credentials: %v", err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("read socks auth: %v", err)
	}
	return response[1]
}

// A CONNECT to an ipv4 address; returns the reply code, 0 on success.
func socksConnectIpv4(t *testing.T, conn net.Conn, target string) byte {
	t.Helper()
	addrPort := netip.MustParseAddrPort(target)
	request := []byte{0x05, 0x01, 0x00, 0x01}
	ip := addrPort.Addr().As4()
	request = append(request, ip[:]...)
	request = binary.BigEndian.AppendUint16(request, addrPort.Port())
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write socks connect: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read socks reply: %v", err)
	}
	var rest int
	switch reply[3] {
	case 0x01:
		rest = 4 + 2
	case 0x04:
		rest = 16 + 2
	default:
		t.Fatalf("socks reply address type %#x", reply[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, rest)); err != nil {
		t.Fatalf("read socks reply address: %v", err)
	}
	return reply[1]
}

func dialSocks(t *testing.T, proxyAddress string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddress, testIoTimeout)
	if err != nil {
		t.Fatalf("dial socks proxy: %v", err)
	}
	conn.SetDeadline(time.Now().Add(testIoTimeout))
	t.Cleanup(func() {
		conn.Close()
	})
	return conn
}

// The connection was closed without a byte of answer.
func expectClosedWithoutAnswer(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(testIoTimeout))
	n, err := conn.Read(make([]byte, 1))
	if n != 0 || err == nil {
		t.Fatalf("%s: read %d bytes, err %v; want the connection closed without an answer", what, n, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("%s: the connection stayed open; want it closed without an answer", what)
	}
}

func expectNotListening(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, testIoTimeout)
	if err == nil {
		conn.Close()
		t.Fatalf("%s still accepts connections", address)
	}
}

func TestHotspotProxyNothingListensUntilEnabled(t *testing.T) {
	recorder := &testListenRecorder{source: testHotspotSource}
	tunnel := &testTunnel{}
	store := &memoryHotspotProxyStore{}
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, store, nil)

	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	if requested := recorder.requestedAddresses(); len(requested) != 0 {
		t.Fatalf("listened on %v before the proxy was turned on", requested)
	}
	status := hotspotProxy.GetStatus()
	if status.Enabled || status.Listening || status.ErrorId != "" || status.Endpoints.Len() != 0 ||
		status.Username != "" || status.Password != "" {
		t.Fatalf("status before the proxy was turned on = %+v", status)
	}
	if hotspotProxy.GetEnabled() || store.GetHotspotProxyPreferences().Enabled {
		t.Fatal("the proxy reads as on before it was turned on")
	}

	hotspotProxy.SetEnabled(true)
	wantRequested := []string{"192.168.43.1:8080", "192.168.43.1:1080"}
	if requested := recorder.requestedAddresses(); !slices.Equal(requested, wantRequested) {
		t.Fatalf("listened on %v, want exactly %v", requested, wantRequested)
	}
	status = hotspotProxy.GetStatus()
	if !status.Enabled || !status.Listening || status.ErrorId != "" || status.Endpoints.Len() != 1 {
		t.Fatalf("status after the proxy was turned on = %+v", status)
	}
	if !strings.HasPrefix(status.Username, hotspotProxyUsernamePrefix) || len(status.Username) != len(hotspotProxyUsernamePrefix)+8 || len(status.Password) != 20 {
		t.Fatalf("generated credentials %q %q", status.Username, status.Password)
	}
	endpoint := status.Endpoints.Get(0)
	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")
	socksAddress := recorder.actualAddress(t, "192.168.43.1:1080")
	httpPort := netip.MustParseAddrPort(httpAddress).Port()
	socksPort := netip.MustParseAddrPort(socksAddress).Port()
	if endpoint.InterfaceName != "wlan1" || endpoint.Host != "192.168.43.1" ||
		endpoint.HttpPort != int(httpPort) || endpoint.SocksPort != int(socksPort) {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	credentials := status.Username + ":" + status.Password + "@192.168.43.1:"
	if endpoint.HttpProxyUrl != "http://"+credentials+netip.MustParseAddrPort(httpAddress).String()[len("127.0.0.1:"):] ||
		endpoint.SocksProxyUrl != "socks5h://"+credentials+netip.MustParseAddrPort(socksAddress).String()[len("127.0.0.1:"):] {
		t.Fatalf("endpoint urls %q %q", endpoint.HttpProxyUrl, endpoint.SocksProxyUrl)
	}
	stored := store.GetHotspotProxyPreferences()
	if !stored.Enabled || stored.Username != status.Username || stored.Password != status.Password {
		t.Fatalf("stored %+v, want on with the status credentials", stored)
	}

	hotspotProxy.SetEnabled(false)
	expectNotListening(t, httpAddress)
	expectNotListening(t, socksAddress)
	offStatus := hotspotProxy.GetStatus()
	if offStatus.Enabled || offStatus.Listening || offStatus.Endpoints.Len() != 0 || offStatus.ErrorId != "" {
		t.Fatalf("status after the proxy was turned off = %+v", offStatus)
	}
	// the credentials stay, so a shared device's settings stay valid
	if offStatus.Username != status.Username || offStatus.Password != status.Password {
		t.Fatalf("credentials changed when the proxy was turned off: %+v", offStatus)
	}
	if stored := store.GetHotspotProxyPreferences(); stored.Enabled || stored.Password != status.Password {
		t.Fatalf("stored %+v after the proxy was turned off", stored)
	}

	hotspotProxy.SetEnabled(true)
	if again := hotspotProxy.GetStatus(); !again.Listening || again.Password != status.Password {
		t.Fatalf("status after turning the proxy on again = %+v", again)
	}
}

// The proxy listens on the hotspot and tethering interfaces only, follows them
// as they come and go, and never listens on an interface of the device's own
// networks.
func TestHotspotProxyListensOnlyOnTheHotspotInterfaces(t *testing.T) {
	recorder := &testListenRecorder{source: testHotspotSource}
	hotspotProxy := newTestHotspotProxy(t, recorder, &testTunnel{}, &memoryHotspotProxyStore{}, nil)

	hotspotProxy.SetEnabled(true)
	if status := hotspotProxy.GetStatus(); status.Listening || status.ErrorId != HotspotProxyErrorNoHotspot {
		t.Fatalf("status with no hotspot = %+v", status)
	}
	if requested := recorder.requestedAddresses(); len(requested) != 0 {
		t.Fatalf("listened on %v with no hotspot", requested)
	}

	upstreams := []*HotspotInterface{
		newTestHotspotInterface("rmnet_data0", false, true, "10.20.30.40/24"),
		// the phone's own wi-fi, even when a report calls it tethered
		newTestHotspotInterface("wlan0", true, true, "192.168.1.20/24"),
		newTestHotspotInterface("tun0", false, true, "10.0.0.2/32"),
	}
	hotspotProxy.SetInterfaces(newTestHotspotInterfaceList(append(slices.Clone(upstreams),
		newTestHotspotInterface("wlan1", true, false, "192.168.43.1/24", "2001:db8:2::1/64"),
		newTestHotspotInterface("rndis0", true, false, "192.168.42.129/24"),
	)...))
	wantRequested := []string{
		"192.168.42.129:8080", "192.168.42.129:1080",
		"192.168.43.1:8080", "192.168.43.1:1080",
	}
	if requested := recorder.requestedAddresses(); !slices.Equal(requested, wantRequested) {
		t.Fatalf("listened on %v, want exactly %v", requested, wantRequested)
	}
	status := hotspotProxy.GetStatus()
	if !status.Listening || status.ErrorId != "" || status.Endpoints.Len() != 2 ||
		status.Endpoints.Get(0).InterfaceName != "rndis0" || status.Endpoints.Get(1).InterfaceName != "wlan1" {
		t.Fatalf("status with two tethering interfaces = %+v", status)
	}

	// the hotspot and tethering go off
	hotspotProxy.SetInterfaces(newTestHotspotInterfaceList(upstreams...))
	for _, requested := range wantRequested {
		expectNotListening(t, recorder.actualAddress(t, requested))
	}
	if status := hotspotProxy.GetStatus(); status.Listening || status.ErrorId != HotspotProxyErrorNoHotspot || status.Endpoints.Len() != 0 {
		t.Fatalf("status after the hotspot went off = %+v", status)
	}
	if requested := recorder.requestedAddresses(); len(requested) != len(wantRequested) {
		t.Fatalf("listened on %v, want nothing more after the hotspot went off", requested)
	}
}

// A listen that fails, e.g. on ports another app holds, shows in the status
// and is tried again at the next change.
func TestHotspotProxyReportsAFailedListen(t *testing.T) {
	recorder := &testListenRecorder{source: testHotspotSource}
	refuse := atomic.Bool{}
	refuse.Store(true)
	hotspotProxy := newTestHotspotProxy(t, recorder, &testTunnel{}, &memoryHotspotProxyStore{}, func(settings *hotspotProxySettings) {
		settings.listen = func(ctx context.Context, address netip.AddrPort) (net.Listener, error) {
			if refuse.Load() {
				return nil, errors.New("address already in use")
			}
			return recorder.listen(ctx, address)
		}
	})
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	if status := hotspotProxy.GetStatus(); status.Listening || status.ErrorId != HotspotProxyErrorListenFailed {
		t.Fatalf("status after a failed listen = %+v", status)
	}

	refuse.Store(false)
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	if status := hotspotProxy.GetStatus(); !status.Listening || status.ErrorId != "" {
		t.Fatalf("status after the listen was tried again = %+v", status)
	}
}

// A connection from outside the hotspot subnet is closed before the proxy
// reads a byte, even with the right credentials, and nothing is dialed.
func TestHotspotProxyClosesConnectionsFromOutsideTheHotspotSubnet(t *testing.T) {
	// no source rewriting: the client's source is loopback, outside the subnet
	recorder := &testListenRecorder{}
	tunnel := &testTunnel{}
	tunnel.connected.Store(true)
	tunnel.target = newTestOrigin(t).address
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, &memoryHotspotProxyStore{}, nil)
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	status := hotspotProxy.GetStatus()

	httpConn, err := net.DialTimeout("tcp", recorder.actualAddress(t, "192.168.43.1:8080"), testIoTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer httpConn.Close()
	io.WriteString(httpConn, "CONNECT origin.example:443 HTTP/1.1\r\nHost: origin.example:443\r\nProxy-Authorization: "+
		basicProxyAuthorization(status.Username, status.Password)+"\r\n\r\n")
	expectClosedWithoutAnswer(t, httpConn, "http from outside the subnet")

	socksConn, err := net.DialTimeout("tcp", recorder.actualAddress(t, "192.168.43.1:1080"), testIoTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer socksConn.Close()
	socksConn.Write([]byte{0x05, 0x01, 0x02})
	expectClosedWithoutAnswer(t, socksConn, "socks from outside the subnet")

	if dialCount := tunnel.dialCount.Load(); dialCount != 0 {
		t.Fatalf("dialed %d times for connections from outside the subnet", dialCount)
	}
	if refusedCount := hotspotProxy.refusedCount.Load(); refusedCount != 2 {
		t.Fatalf("screen refused %d connections, want 2", refusedCount)
	}
}

func TestHotspotProxyScreenAdmitsOnlyTheHotspotSubnet(t *testing.T) {
	screen := &hotspotProxyScreeningListener{sourcePrefix: netip.MustParsePrefix("192.168.43.0/24")}
	cases := []struct {
		remoteAddr net.Addr
		admitted   bool
	}{
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.168.43.20"), Port: 40000}, admitted: true},
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.168.43.1"), Port: 40000}, admitted: true},
		// the ipv4-mapped form of a subnet source
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("::ffff:192.168.43.20"), Port: 40000}, admitted: true},
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.168.42.20"), Port: 40000}, admitted: false},
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("10.20.30.41"), Port: 40000}, admitted: false},
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 40000}, admitted: false},
		{remoteAddr: &net.TCPAddr{IP: net.ParseIP("2001:db8::9"), Port: 40000}, admitted: false},
		{remoteAddr: &net.UDPAddr{IP: net.ParseIP("192.168.43.20"), Port: 40000}, admitted: false},
		{remoteAddr: nil, admitted: false},
	}
	for _, c := range cases {
		if admitted := screen.admits(c.remoteAddr); admitted != c.admitted {
			t.Errorf("admits(%v) = %v, want %v", c.remoteAddr, admitted, c.admitted)
		}
	}
}

// Every request needs the generated credentials, and one without them is
// refused before anything is dialed.
func TestHotspotProxyRefusesRequestsWithoutCredentials(t *testing.T) {
	origin := newTestOrigin(t)
	recorder := &testListenRecorder{source: testHotspotSource}
	tunnel := &testTunnel{target: origin.address}
	tunnel.connected.Store(true)
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, &memoryHotspotProxyStore{}, nil)
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	status := hotspotProxy.GetStatus()
	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")
	socksAddress := recorder.actualAddress(t, "192.168.43.1:1080")

	for _, authorization := range []string{
		"",
		basicProxyAuthorization(status.Username, "wrong-password"),
		basicProxyAuthorization("ur-wrong", status.Password),
		basicProxyAuthorization(status.Username, status.Password+"x"),
		basicProxyAuthorization("", ""),
		"Basic !!!",
		"Bearer " + status.Password,
	} {
		response, _, _ := httpProxyConnect(t, httpAddress, "origin.example:443", authorization)
		if response.StatusCode != http.StatusProxyAuthRequired {
			t.Errorf("CONNECT with authorization %q = %d, want 407", authorization, response.StatusCode)
		}
		if challenge := response.Header.Get("Proxy-Authenticate"); !strings.HasPrefix(challenge, "Basic") {
			t.Errorf("CONNECT with authorization %q has challenge %q", authorization, challenge)
		}
	}
	if response := httpProxyGet(t, httpAddress, "http://origin.example/", ""); response.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("GET without credentials = %d, want 407", response.StatusCode)
	}

	// socks: no "no authentication" method, and wrong credentials fail
	if method := socksSelectMethod(t, dialSocks(t, socksAddress), 0x00); method != 0xFF {
		t.Errorf("socks offered only no authentication: method %#x, want 0xff (none acceptable)", method)
	}
	for _, credentials := range [][2]string{
		{status.Username, "wrong-password"},
		{"ur-wrong", status.Password},
		{"", ""},
	} {
		conn := dialSocks(t, socksAddress)
		if method := socksSelectMethod(t, conn, 0x00, 0x02); method != 0x02 {
			t.Fatalf("socks method %#x, want user/password", method)
		}
		if authStatus := socksAuthenticate(t, conn, credentials[0], credentials[1]); authStatus == 0x00 {
			t.Errorf("socks accepted credentials %q", credentials)
		}
	}

	if dialCount := tunnel.dialCount.Load(); dialCount != 0 {
		t.Fatalf("dialed %d times for requests without the credentials", dialCount)
	}
	if acceptedCount := origin.acceptedCount.Load(); acceptedCount != 0 {
		t.Fatalf("the origin accepted %d connections from requests without the credentials", acceptedCount)
	}

	// the credentials sign in, over http and socks
	response, conn, reader := httpProxyConnect(t, httpAddress, "origin.example:443", basicProxyAuthorization(status.Username, status.Password))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT with the credentials = %d, want 200", response.StatusCode)
	}
	pingPong(t, conn, reader)
	conn.Close()

	socksConn := dialSocks(t, socksAddress)
	if method := socksSelectMethod(t, socksConn, 0x02); method != 0x02 {
		t.Fatalf("socks method %#x, want user/password", method)
	}
	if authStatus := socksAuthenticate(t, socksConn, status.Username, status.Password); authStatus != 0x00 {
		t.Fatalf("socks refused the credentials: %#x", authStatus)
	}
	if reply := socksConnectIpv4(t, socksConn, "192.0.2.10:443"); reply != 0x00 {
		t.Fatalf("socks connect with the credentials: reply %#x", reply)
	}
	pingPong(t, socksConn, socksConn)

	if dialCount := tunnel.dialCount.Load(); dialCount != 2 {
		t.Fatalf("dialed %d times, want once for each signed-in request", dialCount)
	}
}

// The tunnel is the proxy's only way out. A device that is not connected
// refuses every request at once, and no request reaches a destination by any
// other path: the origin, which a direct dial of its literal address would
// reach, sees nothing until the tunnel connects.
func TestHotspotProxyDialsOnlyThroughTheTunnel(t *testing.T) {
	origin := newTestOrigin(t)
	var webRequestCount atomic.Int64
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webRequestCount.Add(1)
		io.WriteString(w, "direct")
	}))
	defer web.Close()

	recorder := &testListenRecorder{source: testHotspotSource}
	tunnel := &testTunnel{target: origin.address}
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, &memoryHotspotProxyStore{}, nil)
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	status := hotspotProxy.GetStatus()
	authorization := basicProxyAuthorization(status.Username, status.Password)
	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")
	socksAddress := recorder.actualAddress(t, "192.168.43.1:1080")

	// refused at once with 502, not retried through the connect budget
	response, _, _ := httpProxyConnect(t, httpAddress, origin.address, authorization)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT while not connected = %d, want 502", response.StatusCode)
	}
	if response := httpProxyGet(t, httpAddress, web.URL+"/", authorization); response.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET while not connected = %d, want 502", response.StatusCode)
	}
	socksConn := dialSocks(t, socksAddress)
	socksSelectMethod(t, socksConn, 0x02)
	if authStatus := socksAuthenticate(t, socksConn, status.Username, status.Password); authStatus != 0x00 {
		t.Fatalf("socks auth %#x", authStatus)
	}
	if reply := socksConnectIpv4(t, socksConn, origin.address); reply != 0x03 {
		t.Fatalf("socks connect while not connected: reply %#x, want 0x03 (network unreachable)", reply)
	}

	if dialCount := tunnel.dialCount.Load(); dialCount != 3 {
		t.Fatalf("the tunnel was asked %d times, want once for each request", dialCount)
	}
	if acceptedCount := origin.acceptedCount.Load(); acceptedCount != 0 {
		t.Fatalf("the origin accepted %d connections while the tunnel was not connected", acceptedCount)
	}
	if requestCount := webRequestCount.Load(); requestCount != 0 {
		t.Fatalf("the web origin served %d requests while the tunnel was not connected", requestCount)
	}

	// connected, a request goes through the tunnel
	tunnel.connected.Store(true)
	response, conn, reader := httpProxyConnect(t, httpAddress, origin.address, authorization)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT while connected = %d, want 200", response.StatusCode)
	}
	defer conn.Close()
	pingPong(t, conn, reader)
	if acceptedCount, dialCount := origin.acceptedCount.Load(), tunnel.dialCount.Load(); acceptedCount != 1 || dialCount != 4 {
		t.Fatalf("origin accepted %d with %d tunnel dials, want 1 through the 4th dial", acceptedCount, dialCount)
	}
}

// New credentials close every connection signed in with the old ones, and
// only the new ones sign in after.
func TestHotspotProxyNewCredentialsCloseOldConnections(t *testing.T) {
	origin := newTestOrigin(t)
	recorder := &testListenRecorder{source: testHotspotSource}
	tunnel := &testTunnel{target: origin.address}
	tunnel.connected.Store(true)
	store := &memoryHotspotProxyStore{}
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, store, nil)
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	old := hotspotProxy.GetStatus()

	response, conn, reader := httpProxyConnect(t, recorder.actualAddress(t, "192.168.43.1:8080"), "origin.example:443", basicProxyAuthorization(old.Username, old.Password))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d", response.StatusCode)
	}
	defer conn.Close()
	pingPong(t, conn, reader)

	hotspotProxy.RegenerateCredentials()
	current := hotspotProxy.GetStatus()
	if current.Username == old.Username || current.Password == old.Password || !current.Listening {
		t.Fatalf("status after new credentials = %+v, old %+v", current, old)
	}
	if stored := store.GetHotspotProxyPreferences(); stored.Password != current.Password || !stored.Enabled {
		t.Fatalf("stored %+v after new credentials", stored)
	}
	// the old tunnel is closed
	conn.SetReadDeadline(time.Now().Add(testIoTimeout))
	if _, err := conn.Write([]byte("ping")); err == nil {
		if _, err := io.ReadFull(reader, make([]byte, 4)); err == nil {
			t.Fatal("a connection signed in with the old credentials still relays")
		}
	}

	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")
	if response, _, _ := httpProxyConnect(t, httpAddress, "origin.example:443", basicProxyAuthorization(old.Username, old.Password)); response.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("CONNECT with the old credentials = %d, want 407", response.StatusCode)
	}
	response, newConn, newReader := httpProxyConnect(t, httpAddress, "origin.example:443", basicProxyAuthorization(current.Username, current.Password))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT with the new credentials = %d, want 200", response.StatusCode)
	}
	defer newConn.Close()
	pingPong(t, newConn, newReader)
}

// The proxy closes with its device: the device's context ends every listener
// and connection, and the choice stays stored for the next device's proxy.
func TestHotspotProxyClosesWithItsDevice(t *testing.T) {
	recorder := &testListenRecorder{source: testHotspotSource}
	store := &memoryHotspotProxyStore{}
	settings := defaultHotspotProxySettings()
	settings.listen = recorder.listen
	deviceCtx, deviceCancel := context.WithCancel(context.Background())
	defer deviceCancel()
	hotspotProxy := newHotspotProxy(deviceCtx, settings, (&testTunnel{}).dial, store, connect.NewNoopLogger())
	defer hotspotProxy.Close()

	changes := make(chan struct{}, 16)
	sub := hotspotProxy.AddStatusChangeListener(testStatusChangeListener(func() {
		changes <- struct{}{}
	}))
	defer sub.Close()

	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")

	deviceCancel()
	select {
	case <-hotspotProxy.runDone:
	case <-time.After(testIoTimeout):
		t.Fatal("the proxy did not close with its device")
	}
	expectNotListening(t, httpAddress)
	status := hotspotProxy.GetStatus()
	if status.Listening || status.ErrorId != HotspotProxyErrorClosed {
		t.Fatalf("status after the device closed = %+v", status)
	}
	// a closed proxy changes nothing, so the next device's proxy is still on
	hotspotProxy.SetEnabled(false)
	if !store.GetHotspotProxyPreferences().Enabled {
		t.Fatal("a closed proxy changed the stored choice")
	}
	if len(changes) == 0 {
		t.Fatal("no status change was reported")
	}
}

type testStatusChangeListener func()

func (self testStatusChangeListener) HotspotProxyStatusChanged() {
	self()
}

// The choice and the credentials are the space's: the next proxy of the same
// local state is on with the same credentials.
func TestHotspotProxyKeepsTheChoiceInLocalState(t *testing.T) {
	asyncLocalState := sdk.NewAsyncLocalState(t.TempDir())
	defer asyncLocalState.CloseAndWait(context.Background())
	localState := asyncLocalState.GetLocalState()

	first := newTestHotspotProxy(t, &testListenRecorder{source: testHotspotSource}, &testTunnel{}, localState, nil)
	first.SetEnabled(true)
	firstStatus := first.GetStatus()
	first.Close()

	stored := localState.GetHotspotProxyPreferences()
	if !stored.Enabled || stored.Username != firstStatus.Username || stored.Password != firstStatus.Password {
		t.Fatalf("stored %+v, want on with %q", stored, firstStatus.Username)
	}

	recorder := &testListenRecorder{source: testHotspotSource}
	second := newTestHotspotProxy(t, recorder, &testTunnel{}, localState, nil)
	if status := second.GetStatus(); !status.Enabled || status.Listening || status.Password != firstStatus.Password {
		t.Fatalf("the next proxy's status = %+v", status)
	}
	second.SetInterfaces(testHotspotInterfaces())
	if status := second.GetStatus(); !status.Listening {
		t.Fatalf("the next proxy does not listen on the hotspot: %+v", status)
	}
}

// At the connection cap, a new connection is closed as it is accepted.
func TestHotspotProxyClosesConnectionsOverTheCap(t *testing.T) {
	origin := newTestOrigin(t)
	recorder := &testListenRecorder{source: testHotspotSource}
	tunnel := &testTunnel{target: origin.address}
	tunnel.connected.Store(true)
	hotspotProxy := newTestHotspotProxy(t, recorder, tunnel, &memoryHotspotProxyStore{}, func(settings *hotspotProxySettings) {
		settings.maxConnectionCount = 1
	})
	hotspotProxy.SetInterfaces(testHotspotInterfaces())
	hotspotProxy.SetEnabled(true)
	status := hotspotProxy.GetStatus()
	httpAddress := recorder.actualAddress(t, "192.168.43.1:8080")

	response, conn, reader := httpProxyConnect(t, httpAddress, "origin.example:443", basicProxyAuthorization(status.Username, status.Password))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d", response.StatusCode)
	}
	defer conn.Close()
	pingPong(t, conn, reader)

	over, err := net.DialTimeout("tcp", httpAddress, testIoTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	expectClosedWithoutAnswer(t, over, "a connection over the cap")
	if refusedCount := hotspotProxy.refusedCount.Load(); refusedCount != 1 {
		t.Fatalf("screen refused %d connections, want 1", refusedCount)
	}
}

func TestHotspotProxyConnectionBudget(t *testing.T) {
	budget := newHotspotProxyConnectionBudget(1)
	if !budget.acquire() {
		t.Fatal("the first connection was refused")
	}
	if budget.acquire() {
		t.Fatal("a connection over the cap was admitted")
	}
	client, server := net.Pipe()
	defer client.Close()
	conn := &hotspotProxyConn{Conn: server, budget: budget}
	conn.Close()
	conn.Close()
	if count := budget.count(); count != 0 {
		t.Fatalf("two closes left %d connections counted, want 0", count)
	}
	if !budget.acquire() {
		t.Fatal("a connection after a close was refused")
	}

	unlimited := newHotspotProxyConnectionBudget(0)
	for range 1000 {
		if !unlimited.acquire() {
			t.Fatal("a budget without a cap refused a connection")
		}
	}
}

func TestHotspotProxyCredentialsMatch(t *testing.T) {
	if !hotspotProxyCredentialsMatch("ur-abc", "secret", "ur-abc", "secret") {
		t.Fatal("the credentials did not match themselves")
	}
	for _, c := range [][4]string{
		{"ur-abc", "secreT", "ur-abc", "secret"},
		{"ur-abd", "secret", "ur-abc", "secret"},
		{"ur-abc", "secret ", "ur-abc", "secret"},
		{"", "", "", ""},
		{"ur-abc", "", "ur-abc", ""},
		{"", "secret", "", "secret"},
	} {
		if hotspotProxyCredentialsMatch(c[0], c[1], c[2], c[3]) {
			t.Errorf("credentials %q matched", c)
		}
	}
}

func TestHotspotProxyGeneratedCredentials(t *testing.T) {
	settings := defaultHotspotProxySettings()
	seen := map[string]bool{}
	for range 64 {
		username, password, err := newHotspotProxyCredentials(settings)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(username, hotspotProxyUsernamePrefix) || len(username) != len(hotspotProxyUsernamePrefix)+settings.usernameLength {
			t.Fatalf("user name %q", username)
		}
		if len(password) != settings.passwordLength {
			t.Fatalf("password %q", password)
		}
		for _, r := range strings.TrimPrefix(username, hotspotProxyUsernamePrefix) {
			if !strings.ContainsRune(hotspotProxyUsernameAlphabet, r) {
				t.Fatalf("user name %q has %q", username, r)
			}
		}
		for _, r := range password {
			if !strings.ContainsRune(hotspotProxyPasswordAlphabet, r) {
				t.Fatalf("password %q has %q", password, r)
			}
		}
		if seen[password] {
			t.Fatalf("password %q generated twice", password)
		}
		seen[password] = true
	}
}
