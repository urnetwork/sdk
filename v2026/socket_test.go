package sdk

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/rpc"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

type socketTestNAT struct {
	peer    *connect.Tun
	drop    atomic.Int32
	packets atomic.Int64
}

func (n *socketTestNAT) SendPacket(_ connect.TransferPath, _ protocol.ProvideMode, p []byte, _ time.Duration) bool {
	defer connect.MessagePoolReturn(p)
	n.packets.Add(1)
	// Keep DNS available when blackholing application traffic in one family.
	path, _ := connect.ParseIpPath(p)
	if int32(p[0]>>4) != n.drop.Load() || (path != nil && path.DestinationPort == 53) {
		_, _ = n.peer.Write(p)
	}
	return true
}
func (*socketTestNAT) Close()                                               {}
func (*socketTestNAT) Shuffle()                                             {}
func (*socketTestNAT) SecurityPolicyStats(bool) connect.SecurityPolicyStats { return nil }
func (*socketTestNAT) SetLocalSecurityBypass(bool)                          {}

type socketTestNetwork struct {
	device *DeviceLocal
	peer   *connect.Tun
	nat    *socketTestNAT
	cancel context.CancelFunc
}

func newSocketTestNetwork(t *testing.T) *socketTestNetwork {
	t.Helper()
	return newSocketTestNetworkWithPacketReply(t, nil)
}

// The optional reply hook owns its packet copy and can queue delivery without
// blocking the shared packet reader.
func newSocketTestNetworkWithPacketReply(t *testing.T, replyPacket func(*connect.IpPath, func())) *socketTestNetwork {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	settings := connect.DefaultTunSettings()
	settings.DialRace = 1
	peer, err := connect.CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	nat := &socketTestNAT{peer: peer}
	localSettings := DefaultDeviceLocalSettings()
	localSettings.DisableLogging = true
	mux := connect.DefaultUpgradeMuxSettings()
	addrs := peer.LocalAddresses()
	mux.Dns.Resolver = &connect.DnsResolverSettings{EnableRemoteDns: true, RemoteDnsIpv4: []string{addrs[0].String()}, RemoteDnsIpv6: []string{addrs[1].String()}}
	d := &DeviceLocal{ctx: ctx, cancel: cancel, settings: localSettings, log: localSettings.logger(), clientId: connect.NewId(), stats: newDeviceStats(), upgradeMuxSettings: mux,
		receiveCallbacks: connect.NewCallbackList[connect.ReceivePacketFunction](), receivePacketsCallbacks: connect.NewCallbackList[connect.ReceivePacketsFunction](), receivePacketBatchCallbacks: connect.NewCallbackList[receivePacketBatchFunction]()}
	d.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: nat})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			p, err := peer.Read()
			if err != nil {
				return
			}
			path, _ := connect.ParseIpPath(p)
			if replyPacket == nil {
				d.receive(connect.TransferPath{}, protocol.ProvideMode_Network, path, p)
			} else {
				packet := append([]byte(nil), p...)
				replyPacket(path, func() {
					d.receive(connect.TransferPath{}, protocol.ProvideMode_Network, path, packet)
				})
			}
			connect.MessagePoolReturn(p)
		}
	}()
	for _, ip := range addrs {
		dns, err := peer.ListenUDP(&net.UDPAddr{IP: net.IP(ip.AsSlice()), Port: 53})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			p := make([]byte, 65535)
			for {
				n, addr, err := dns.ReadFrom(p)
				if err != nil {
					return
				}
				var q dnsmessage.Message
				if q.Unpack(p[:n]) != nil {
					continue
				}
				answer := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true}, Questions: q.Questions}
				for _, question := range q.Questions {
					for _, a := range addrs {
						h := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}
						if a.Is4() && question.Type == dnsmessage.TypeA {
							answer.Answers = append(answer.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: a.As4()}})
						}
						if a.Is6() && question.Type == dnsmessage.TypeAAAA {
							answer.Answers = append(answer.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
						}
					}
				}
				out, _ := answer.Pack()
				_, _ = dns.WriteTo(out, addr)
			}
		}()
	}
	t.Cleanup(func() { cancel(); d.sockets.close(); _ = peer.Close(); <-done })
	return &socketTestNetwork{device: d, peer: peer, nat: nat, cancel: cancel}
}
func (n *socketTestNetwork) ip(v6 bool) net.IP {
	for _, ip := range n.peer.LocalAddresses() {
		if ip.Is6() == v6 {
			return net.IP(ip.AsSlice())
		}
	}
	panic("missing address")
}
func (n *socketTestNetwork) echo(t *testing.T, network string, port int) string {
	t.Helper()
	if strings.HasPrefix(network, "tcp") {
		ln, err := n.peer.ListenTCP(&net.TCPAddr{IP: n.ip(strings.HasSuffix(network, "6")), Port: port})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
			}
		}()
		return ln.Addr().String()
	}
	c, err := n.peer.ListenUDP(&net.UDPAddr{IP: n.ip(strings.HasSuffix(network, "6")), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		p := make([]byte, 65535)
		for {
			size, a, err := c.ReadFrom(p)
			if err != nil {
				return
			}
			_, _ = c.WriteTo(p[:size], a)
		}
	}()
	return c.LocalAddr().String()
}
func socketRoundTrip(t *testing.T, c net.Conn, p []byte, datagram bool) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Write(p); err != nil || n != len(p) {
		t.Fatalf("write %d: %v", n, err)
	}
	out := make([]byte, len(p)+1)
	var n int
	var err error
	if datagram {
		n, err = c.Read(out)
	} else {
		n, err = io.ReadFull(c, out[:len(p)])
	}
	if err != nil || !bytes.Equal(out[:n], p) {
		t.Fatalf("read %d %q: %v", n, out[:n], err)
	}
}

func TestSocketFamilyMatrix(t *testing.T) {
	n := newSocketTestNetwork(t)
	for _, network := range []string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			addr := n.echo(t, network, 0)
			c, err := n.device.DialContext(t.Context(), network, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			socketRoundTrip(t, c, []byte("user space socket"), strings.HasPrefix(network, "udp"))
			if c.LocalAddr().String() == "" || c.RemoteAddr().String() != addr {
				t.Fatal("addresses missing")
			}
		})
	}
	if n.nat.packets.Load() == 0 {
		t.Fatal("socket bypassed device packet path")
	}
}
func TestSocketUDPBoundariesEmptyAndTruncation(t *testing.T) {
	n := newSocketTestNetwork(t)
	c, err := n.device.Dial("udp6", n.echo(t, "udp6", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	socketRoundTrip(t, c, []byte{}, true)
	socketRoundTrip(t, c, []byte("next"), true)
	_, _ = c.Write([]byte("truncate"))
	p := make([]byte, 3)
	if size, err := c.Read(p); err != nil || size != 3 || string(p) != "tru" {
		t.Fatalf("truncation: %d %v %q", size, err, p)
	}
	socketRoundTrip(t, c, []byte("following"), true)
}
func TestSocketDeadlinesHalfCloseAndCancellation(t *testing.T) {
	n := newSocketTestNetwork(t)
	addr := n.echo(t, "tcp4", 0)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := n.device.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	socketRoundTrip(t, c, bytes.Repeat([]byte("x"), 128<<10), false)
	_ = c.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("timeout: %v", err)
		}
	}
	_ = c.SetDeadline(time.Time{})
	socketRoundTrip(t, c, []byte("after timeout"), false)
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("half-close EOF: %v", err)
	}
	cancelCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := n.device.DialContext(cancelCtx, "tcp", addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dial: %v", err)
	}
}
func TestSocketCloseUnblocksReadAndDeviceOwnsConnections(t *testing.T) {
	n := newSocketTestNetwork(t)
	c, err := n.device.Dial("udp4", n.echo(t, "udp4", 0))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
	n.cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("device cancellation did not unblock read")
	}
	if _, err := n.device.Dial("udp4", "127.0.0.1:9"); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
func TestSocketInvalidNetworkAddressAndFamily(t *testing.T) {
	n := newSocketTestNetwork(t)
	for _, test := range []struct{ network, addr string }{{"unix", "/tmp/sock"}, {"tcp", "no-port"}, {"tcp4", "[::1]:9"}, {"udp6", "127.0.0.1:9"}, {"udp", "127.0.0.1:65536"}} {
		c, err := n.device.Dial(test.network, test.addr)
		if c != nil {
			c.Close()
			t.Fatal("invalid dial returned connection")
		}
		if err == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
}
func TestSocketNamedHappyEyeballs(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		for _, dead := range []int32{0, 6, 4} {
			t.Run(network+strconv.Itoa(int(dead)), func(t *testing.T) {
				n := newSocketTestNetwork(t)
				livePeers := map[string]bool{
					n.echo(t, network+"4", 8443): dead != 4,
					n.echo(t, network+"6", 8443): dead != 6,
				}
				n.nat.drop.Store(dead)
				start := time.Now()
				c, err := n.device.Dial(network, "socket.test:8443")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				socketRoundTrip(t, c, []byte("first"), network == "udp")
				if time.Since(start) > 2*time.Second {
					t.Fatal("family fallback too slow")
				}
				// The first completed TCP handshake or UDP reply wins.
				// When both paths work, either family can win the race.
				if !livePeers[c.RemoteAddr().String()] {
					t.Fatalf("winner=%s is not a live configured peer", c.RemoteAddr())
				}
				socketRoundTrip(t, c, []byte("second"), network == "udp")
			})
		}
	}
}

func socketTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "socket.test"}, DNSNames: []string{"socket.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}
func TestSocketTLSAndDTLS(t *testing.T) {
	for _, network := range []string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			n := newSocketTestNetwork(t)
			cert, roots := socketTestCertificate(t)
			var addr string
			if strings.HasPrefix(network, "tcp") {
				ln, err := n.peer.ListenTCP(&net.TCPAddr{IP: n.ip(strings.HasSuffix(network, "6"))})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				addr = ln.Addr().String()
				go func() {
					for {
						raw, err := ln.Accept()
						if err != nil {
							return
						}
						go func() {
							c := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
							defer c.Close()
							_, _ = io.Copy(c, c)
						}()
					}
				}()
			} else {
				pc, err := n.peer.ListenUDP(&net.UDPAddr{IP: n.ip(strings.HasSuffix(network, "6"))})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pc.Close() })
				addr = pc.LocalAddr().String()
				go func() { // Connected DTLS peer is learned from the first ClientHello without dropping it.
					p := make([]byte, 65535)
					size, remote, err := pc.ReadFrom(p)
					if err != nil {
						return
					}
					wrapped := &socketFirstPacket{PacketConn: pc, first: p[:size], remote: remote}
					c, err := dtls.Server(wrapped, remote, &dtls.Config{Certificates: []tls.Certificate{cert}})
					if err != nil {
						return
					}
					defer c.Close()
					if c.HandshakeContext(t.Context()) != nil {
						return
					}
					for {
						size, err := c.Read(p)
						if err != nil {
							return
						}
						_, _ = c.Write(p[:size])
					}
				}()
			}
			config := &tls.Config{RootCAs: roots, ServerName: "socket.test"}
			if network == "tcp" || network == "udp" {
				n.nat.drop.Store(6)
				_, port, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort("socket.test", port)
				config.ServerName = "" // inferred from the hostname, not the winning IP
			}
			c, err := n.device.DialTlsContext(t.Context(), network, addr, config)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			socketRoundTrip(t, c, []byte("encrypted"), strings.HasPrefix(network, "udp"))
			if config.MinVersion != 0 {
				t.Fatal("mutated caller config")
			}
		})
	}
}

type socketFirstPacket struct {
	net.PacketConn
	first  []byte
	remote net.Addr
}

func (c *socketFirstPacket) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.first != nil {
		n := copy(p, c.first)
		c.first = nil
		return n, c.remote, nil
	}
	return c.PacketConn.ReadFrom(p)
}

func socketTestRemote(t *testing.T, n *socketTestNetwork) (*DeviceRemote, *DeviceLocalRpc, net.Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	client, server := net.Pipe()
	local := &DeviceLocalRpc{ctx: ctx, cancel: cancel, deviceLocal: n.device}
	srv := rpc.NewServer()
	if err := srv.RegisterName("DeviceLocalRpc", local); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		codec := newGobServerCodec(server)
		for {
			if srv.ServeRequest(codec) != nil {
				return
			}
		}
	}()
	remote := &DeviceRemote{ctx: ctx, cancel: cancel, settings: defaultDeviceRpcSettings(), service: &rpcClient{ctx: ctx, client: rpc.NewClient(client), timeout: time.Second, closeClient: client.Close}}
	t.Cleanup(func() {
		cancel()
		local.sockets.close()
		client.Close()
		server.Close()
		<-done
		local.sockets.workers.Wait()
	})
	return remote, local, client
}
func TestSocketRemoteTCPUDPAndConcurrentControls(t *testing.T) {
	n := newSocketTestNetwork(t)
	d, _, _ := socketTestRemote(t, n)
	for _, network := range []string{"tcp4", "udp6"} {
		c, err := d.Dial(network, n.echo(t, network, 0))
		if err != nil {
			t.Fatal(err)
		}
		socketRoundTrip(t, c, []byte("over rpc"), strings.HasPrefix(network, "udp"))
		_ = c.SetDeadline(time.Time{})
		done := make(chan error, 1)
		go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
		_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("read blocked sequential RPC controls")
		}
		_ = c.SetReadDeadline(time.Time{})
		socketRoundTrip(t, c, []byte("after timeout"), strings.HasPrefix(network, "udp"))
		_ = c.Close()
	}
}
func TestSocketRemoteDisconnectAndSessionIsolation(t *testing.T) {
	n := newSocketTestNetwork(t)
	d, local, transport := socketTestRemote(t, n)
	c, err := d.Dial("udp4", n.echo(t, "udp4", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	other := &DeviceLocalRpc{ctx: t.Context(), deviceLocal: n.device}
	var result DeviceSocketResponse
	_ = other.Socket(&DeviceSocketRequest{ID: c.(*remoteSocketConn).id, Op: "read", Start: true, Size: 1}, &result)
	if !errors.Is(result.err(), net.ErrClosed) {
		t.Fatal("another session accessed socket")
	}
	transport.Close()
	local.sockets.close()
	if _, err := c.Write([]byte("closed")); err == nil {
		t.Fatal("write replayed after disconnect")
	}
}

// The family-race helper must close late successful losers, including a dialer
// that returns success after cancellation.
func TestSocketFamilyRaceClosesLoser(t *testing.T) {
	var closed atomic.Int32
	block := make(chan struct{})
	done := make(chan struct{})
	got, err := raceSocketFamilies(t.Context(), "udp", "socket.test:1", func(ctx context.Context, family string) (int, error) {
		if family == "udp6" {
			<-block
			close(done)
			return 6, nil
		}
		return 4, nil
	}, func(int) { closed.Add(1) })
	if err != nil || got != 4 {
		t.Fatal(got, err)
	}
	close(block)
	<-done
	limit := time.After(time.Second)
	for closed.Load() != 1 {
		select {
		case <-limit:
			t.Fatal("loser leaked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
