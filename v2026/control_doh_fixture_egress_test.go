// Keep the control-DoH fixture's native sockets inside its owned listeners.
// The injected DoH dial and bootstrap plain-DNS dial are separate paths.
package sdk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

var errControlDohFixtureHostSocket = errors.New("control DoH fixture refused a host socket")

// Only the exact API listener may receive a native TCP connect. An empty
// listener refuses every host socket, including bootstrap plain DNS.
func controlDohFixtureSocketControl(listener string) func(string, string, syscall.RawConn) error {
	return func(network string, address string, _ syscall.RawConn) error {
		if listener != "" && address == listener && (network == "tcp4" || network == "tcp6") {
			return nil
		}
		return errControlDohFixtureHostSocket
	}
}

// Keep the existing DoH TLS trust and blackhole behavior, but refuse native
// host egress: bootstrap plain DNS does not use the injected DoH dial.
func controlDohFixtureConfigure(roots *x509.CertPool, dohAddress string) func(*connect.DohSettings) {
	return func(settings *connect.DohSettings) {
		settings.RequestTimeout = time.Second
		settings.DnsResolverSettings.TlsConfig = &tls.Config{RootCAs: roots}
		settings.DialControl = controlDohFixtureSocketControl("")
		settings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				if address == dohAddress {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
	}
}

// Exercise the actual bootstrap resolver with encrypted paths unavailable.
// The outer tripwire always refuses before connect, even in the old-behavior
// control, so reproducing the missing fixture guard cannot contact DNS.
func TestControlDohFixtureBootstrapStaysOffHostNetwork(t *testing.T) {
	settings := connect.ControlDohSettings(nil, nil)
	controlDohFixtureConfigure(x509.NewCertPool(), "127.0.0.1:1")(settings)
	settings.IpVersion = 4
	settings.BootstrapPlainDnsDomains = []string{"space.example"}
	settings.DnsResolverSettings.EnableRemoteDoh = false
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.LocalDnsIpv4 = []string{"192.0.2.53"}
	settings.DnsResolverSettings.LocalDnsIpv6 = nil

	guard := settings.DialControl
	var attempts atomic.Int32
	var escaped atomic.Int32
	settings.DialControl = func(network string, address string, raw syscall.RawConn) error {
		attempts.Add(1)
		if guard == nil || !errors.Is(guard(network, address, raw), errControlDohFixtureHostSocket) {
			escaped.Add(1)
		}
		return errors.New("bootstrap fixture outer socket tripwire")
	}
	cache := connect.NewDohCache(settings)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "api.space.example")
	if ctx.Err() != nil {
		t.Fatalf("bootstrap refusal exceeded its watchdog: %v", ctx.Err())
	}
	if attempts.Load() == 0 {
		t.Fatal("bootstrap native socket boundary was not exercised")
	}
	if escaped.Load() != 0 {
		t.Fatalf("bootstrap escaped the fixture guard before the outer tripwire: %d attempts", escaped.Load())
	}
	if len(addrs) != 0 || authoritative {
		t.Fatalf("refused bootstrap manufactured an answer: %v authoritative=%v", addrs, authoritative)
	}
}

// The API retains its actual local TCP path, while both a nonlocal answer
// and an unowned loopback/UDP destination fail before a connect can occur.
func TestControlDohFixtureApiDialStaysAtItsListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings := connect.DefaultConnectSettings()
	settings.DialControl = controlDohFixtureSocketControl(listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := settings.NetDialer().DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatalf("owned API listener refused: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	guard := settings.DialControl
	for _, target := range []struct{ network, address string }{
		{network: "tcp4", address: "192.0.2.91:443"},
		{network: "tcp4", address: "127.0.0.1:1"},
		{network: "udp4", address: "192.0.2.53:53"},
	} {
		var attempts atomic.Int32
		var escaped atomic.Int32
		settings.DialControl = func(network string, address string, raw syscall.RawConn) error {
			attempts.Add(1)
			if !errors.Is(guard(network, address, raw), errControlDohFixtureHostSocket) {
				escaped.Add(1)
			}
			return errors.New("API fixture outer socket tripwire")
		}
		conn, err := settings.NetDialer().DialContext(ctx, target.network, target.address)
		if conn != nil {
			conn.Close()
			t.Fatalf("unowned %s socket was connected", target.network)
		}
		if err == nil || attempts.Load() == 0 || escaped.Load() != 0 {
			t.Fatalf("API socket escaped fixture refusal: network=%s attempts=%d escaped=%d err=%v", target.network, attempts.Load(), escaped.Load(), err)
		}
	}
}
