package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// TestMain points the default device rpc address at a per-process ephemeral
// port. Every rpc test — including the DeviceLocal default rpc manager, which
// reads the default settings internally — then shares a port unique to this
// test process, so a suite run never collides with an orphaned earlier run,
// another checkout's suite, or a locally running app on the fixed production
// default (127.0.0.1:12025). A genuinely shared port is separately rejected by
// the sync instance check, so a collision fails loud instead of pairing a
// remote with the wrong device.
// It also turns off the extender network client, the member role's gossip node
// and the provider extender role for the whole suite: a space built with a
// production host name would otherwise resolve its extender dns name and reach
// the operator's hello on every construction, every such space would stand up a
// libp2p host, and every test that turns providing on would bind the extender's
// carrier ports. The extender tests that need any of them turn it back on for
// their own space.
// Every space built from a key resolves in the process (testOfflineConnectSettings).
func TestMain(m *testing.M) {
	deviceRpcDefaultAddress = testing_freeHostPort()
	SetExtenderNetworkClientEnabled(false)
	extenderNodeEnabled = false
	extenderProvideRoleEnabled = false
	newNetworkSpaceConnectSettings = testOfflineConnectSettings
	os.Exit(m.Run())
}

// testOfflineResolver answers every name in the process, with nothing: the
// hosts of the suite's spaces (`test`, `space.example`, `.test.invalid`) never
// resolve, and no lookup may reach the system resolver, which forwards off the
// machine. The hosts file still answers, so localhost resolves.
var testOfflineResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, errors.New("the test resolves no name off the machine")
	},
}

// testOfflineConnectSettings are the connect settings of the suite's spaces:
// the defaults with the offline resolver, which also takes the place of the
// strategy's protected-domain DoH, so no space queries a public DoH server,
// and with dials and packet sockets toward loopback only, so no space, device
// or node built on it reaches the documentation addresses its urls, records
// and peers carry.
func testOfflineConnectSettings() *connect.ConnectSettings {
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.Resolver = testOfflineResolver
	connectSettings.DialContextSettings = testLoopbackDialContextSettings()
	return connectSettings
}

// testOfflineClientStrategySettings are client strategy settings with the
// offline connect settings, for a test that builds a strategy for test hosts
// itself.
func testOfflineClientStrategySettings() *connect.ClientStrategySettings {
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.ConnectSettings = *testOfflineConnectSettings()
	return strategySettings
}

// testLoopbackDialContextSettings dial and send to loopback only. A test whose
// in-process servers are all on loopback uses them where its records, peers or
// spaces name documentation addresses. Nothing is sent to those: they answer
// nothing in the process, as they do on a real network, so a tcp dial to one
// waits out its deadline (at most testBlackholeDialTimeout) and a datagram
// to one is dropped. A host name is refused, as the offline resolver refuses
// it.
func testLoopbackDialContextSettings() *connect.DialContextSettings {
	return &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ip := net.ParseIP(host)
			if host == "localhost" || (ip != nil && ip.IsLoopback()) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			if ip != nil && strings.HasPrefix(network, "tcp") {
				timer := time.NewTimer(testBlackholeDialTimeout)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
					return nil, fmt.Errorf("the test dial of %s %s timed out", network, address)
				}
			}
			return nil, fmt.Errorf("the test dials nothing off the machine, refused %s %s", network, address)
		},
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			packetConn, err := net.ListenUDP("udp", nil)
			if err != nil {
				return nil, err
			}
			return &testLoopbackPacketConn{PacketConn: packetConn}, nil
		},
	}
}

// The longest a test tcp dial to a documentation address waits, the connect
// default (ConnectSettings.ConnectTimeout).
const testBlackholeDialTimeout = 15 * time.Second

// Only the net.PacketConn methods, so quic-go writes through WriteTo rather
// than the udp socket's own message writes.
type testLoopbackPacketConn struct {
	net.PacketConn
}

func (self *testLoopbackPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if udpAddr, ok := addr.(*net.UDPAddr); !ok || !udpAddr.IP.IsLoopback() {
		// dropped, as a documentation address drops it
		return len(b), nil
	}
	return self.PacketConn.WriteTo(b, addr)
}

// testing_freeHostPort reserves a free loopback port from the kernel (bind
// 127.0.0.1:0, read the assigned port, close) and returns "127.0.0.1:<port>".
// The kernel cycles ephemeral assignments, so successive calls return distinct
// ports and nothing rebinds a returned port for the lifetime of a test run.
func testing_freeHostPort() string {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	return fmt.Sprintf("127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
}
