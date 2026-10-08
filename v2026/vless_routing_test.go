package sdk

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A minimal VLESS relay for the sdk tests: raw tcp, no security, no flow. It
// reads one request header, dials the destination it names and relays both
// ways, recording each destination.
type testVlessRelay struct {
	listener     net.Listener
	stateLock    sync.Mutex
	destinations []string
}

func newTestVlessRelay(t *testing.T) *testVlessRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &testVlessRelay{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go relay.serve(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return relay
}

func (self *testVlessRelay) port() int {
	return self.listener.Addr().(*net.TCPAddr).Port
}

func (self *testVlessRelay) seen() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.destinations...)
}

func (self *testVlessRelay) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	// version, user id, addons length
	head := make([]byte, 18)
	if _, err := io.ReadFull(reader, head); err != nil || head[0] != 0 {
		return
	}
	if _, err := io.CopyN(io.Discard, reader, int64(head[17])); err != nil {
		return
	}
	// command, port, address type
	rest := make([]byte, 4)
	if _, err := io.ReadFull(reader, rest); err != nil || rest[0] != 1 {
		return
	}
	port := int(binary.BigEndian.Uint16(rest[1:3]))
	var host string
	switch rest[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 2:
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		name := make([]byte, int(length))
		if _, err := io.ReadFull(reader, name); err != nil {
			return
		}
		host = string(name)
	case 3:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		return
	}
	destination := net.JoinHostPort(host, strconv.Itoa(port))
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.destinations = append(self.destinations, destination)
	}()
	destinationConn, err := net.Dial("tcp", destination)
	if err != nil {
		return
	}
	defer destinationConn.Close()
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(destinationConn, reader)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, destinationConn)
		done <- struct{}{}
	}()
	<-done
}

// A space whose api is reachable only through a VLESS server -- no direct,
// resilient or extender dialers, nothing exposed -- reaches it once its VLESS
// settings are saved, and stops when they are turned off. The settings take
// effect in the running space, with no rebuild.
func TestNetworkSpaceRoutesThroughVless(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello api"))
	}))
	defer api.Close()
	rootCas := x509.NewCertPool()
	rootCas.AddCert(api.Certificate())
	relay := newTestVlessRelay(t)

	settings := connect.DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: rootCas}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(ctx, api.URL, "wss://127.0.0.1:1", settings)
	defer networkSpace.close()

	get := func(timeout time.Duration) (string, error) {
		requestCtx, requestCancel := context.WithTimeout(ctx, timeout)
		defer requestCancel()
		body, err := connect.HttpGetWithStrategyRaw(requestCtx, networkSpace.clientStrategy, api.URL+"/hello", "")
		return string(body), err
	}

	if body, err := get(2 * time.Second); err == nil {
		t.Fatalf("with no VLESS server the api must be unreachable, got %q", body)
	}

	vlessSettings := &VlessSettings{
		Enabled:  true,
		Address:  "127.0.0.1",
		Port:     relay.port(),
		Id:       testVlessId,
		Network:  connect.VlessNetworkTcp,
		Security: connect.VlessSecurityNone,
	}
	if errorId := networkSpace.SetVlessSettings(vlessSettings); errorId != "" {
		t.Fatal(errorId)
	}
	body, err := get(30 * time.Second)
	if err != nil {
		t.Fatalf("through the VLESS server: %s", err)
	}
	if body != "hello api" {
		t.Fatalf("body = %q", body)
	}
	apiHostPort := api.Listener.Addr().String()
	destinations := relay.seen()
	if len(destinations) == 0 || destinations[0] != apiHostPort {
		t.Fatalf("relay destinations = %v, expected %s", destinations, apiHostPort)
	}
	if settings := networkSpace.GetVlessSettings(); !settings.Enabled || settings.Port != relay.port() {
		t.Fatalf("settings read back = %+v", settings)
	}

	vlessSettings.Enabled = false
	if errorId := networkSpace.SetVlessSettings(vlessSettings); errorId != "" {
		t.Fatal(errorId)
	}
	if body, err := get(2 * time.Second); err == nil {
		t.Fatalf("with VLESS turned off the api must be unreachable again, got %q", body)
	}
}
