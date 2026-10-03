package sdk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Dialer is the method interface of net.Dialer. Device implementations use
// their user-space packet path. Contexts bound establishment; cancellation
// after success does not close the connection. Closing the owning Device does.
//
//gomobile:noexport
type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// TLSDialer extends Device with TLS over TCP and DTLS over UDP.
//
//gomobile:noexport
type TLSDialer interface {
	DialTls(network, address string, config *tls.Config) (net.Conn, error)
	DialTlsContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error)
}

// SocketTLSOptions is the portable TLS configuration used by JS and C.
// RootCAPEM, when supplied, replaces the default trust roots. Verification is
// always enabled. Native Go callers can supply a full tls.Config for TCP.
type SocketTLSOptions struct {
	ServerName string   `json:"serverName,omitempty"`
	RootCAPEM  string   `json:"rootCAPEM,omitempty"`
	NextProtos []string `json:"nextProtos,omitempty"`
}

//gomobile:noexport
func (o *SocketTLSOptions) TLSConfig() (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if o == nil {
		return c, nil
	}
	c.ServerName = o.ServerName
	c.NextProtos = append([]string(nil), o.NextProtos...)
	if o.RootCAPEM != "" {
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM([]byte(o.RootCAPEM)) {
			return nil, errors.New("invalid root CA PEM")
		}
	}
	return c, nil
}

type deviceSockets struct {
	mu     sync.Mutex
	tun    *connect.Tun
	closed bool
	conns  map[*deviceSocketConn]struct{}
	done   chan struct{}
}

func (s *deviceSockets) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	tun := s.tun
	conns := make([]*deviceSocketConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	if tun != nil {
		_ = tun.Close()
	}
}

func (d *DeviceLocal) socketTun() (*connect.Tun, error) {
	// Use the same lock order as live resolver updates: device, then sockets.
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	s := &d.sockets
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if s.tun != nil {
		return s.tun, nil
	}
	settings := connect.DefaultTunSettings()
	settings.DialRace = 1 // address-family racing already happens inside the TUN
	settings.Log = d.logger()
	// All resolver traffic uses this same device. In particular, never use
	// the host resolver fallback while a provider is unavailable.
	resolver := socketResolverSettings(d.dnsResolverSettingsWithLock())
	tun, err := connect.CreateTunWithResolver(d.ctx, settings, resolver)
	if err != nil {
		return nil, err
	}
	s.tun, s.conns = tun, make(map[*deviceSocketConn]struct{})
	local := tun.LocalAddresses()
	unsub := d.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, path *connect.IpPath, packet []byte) {
		// Fragments have no transport ports and ParseIpPath can reject them.
		// Demultiplex on the IP header so gVisor can reassemble large datagrams.
		destination := socketPacketDestination(packet)
		for _, address := range local {
			if destination == address {
				_, _ = tun.Write(packet)
				break
			}
		}
	})
	context.AfterFunc(d.ctx, s.close)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		defer unsub()
		defer s.close()
		packets := make([][]byte, 32)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			d.SendPacketsNoCopy(packets[:n])
			clear(packets[:n])
		}
	}()
	return tun, nil
}

func socketResolverSettings(settings *DnsResolverSettings) *connect.DnsResolverSettings {
	if settings == nil {
		settings = GetDefaultDnsResolverSettings()
	}
	resolver := settings.toConnect()
	resolver.RemoteDohUrlsIpv4 = append(resolver.RemoteDohUrlsIpv4, resolver.LocalDohUrlsIpv4...)
	resolver.RemoteDohUrlsIpv6 = append(resolver.RemoteDohUrlsIpv6, resolver.LocalDohUrlsIpv6...)
	resolver.RemoteDnsIpv4 = append(resolver.RemoteDnsIpv4, resolver.LocalDnsIpv4...)
	resolver.RemoteDnsIpv6 = append(resolver.RemoteDnsIpv6, resolver.LocalDnsIpv6...)
	resolver.EnableRemoteDoh = resolver.EnableRemoteDoh || resolver.EnableLocalDoh
	resolver.EnableRemoteDns = resolver.EnableRemoteDns || resolver.EnableLocalDns
	resolver.EnableLocalDoh, resolver.EnableLocalDns = false, false
	return resolver
}

func (s *deviceSockets) setResolver(settings *DnsResolverSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tun != nil && !s.closed {
		s.tun.SetDnsResolverSettings(socketResolverSettings(settings), connect.DefaultTunSettings().DohRequestTimeout)
	}
}

func (s *deviceSockets) closeAndWait() {
	s.close()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

func socketPacketDestination(packet []byte) netip.Addr {
	if len(packet) >= 20 && packet[0]>>4 == 4 {
		return netip.AddrFrom4([4]byte(packet[16:20]))
	}
	if len(packet) >= 40 && packet[0]>>4 == 6 {
		return netip.AddrFrom16([16]byte(packet[24:40]))
	}
	return netip.Addr{}
}

//gomobile:noexport
func (d *DeviceLocal) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

//gomobile:noexport
func (d *DeviceLocal) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := socketNetwork(network); err != nil {
		return nil, err
	}
	tun, err := d.socketTun()
	if err != nil {
		return nil, err
	}
	var c net.Conn
	host, _, splitErr := net.SplitHostPort(address)
	_, literalErr := netip.ParseAddr(host)
	if network == "udp" && splitErr == nil && literalErr != nil {
		c, err = dialHappyUDP(ctx, network, address, tun.DialContext)
	} else {
		c, err = tun.DialContext(ctx, network, address)
	}
	if err != nil {
		return nil, err
	}
	s := &d.sockets
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d.ctx.Err() != nil || ctx.Err() != nil {
		_ = c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, net.ErrClosed
	}
	conn := &deviceSocketConn{Conn: c, owner: s, local: c.LocalAddr(), remote: c.RemoteAddr()}
	s.conns[conn] = struct{}{}
	return conn, nil
}

type deviceSocketConn struct {
	net.Conn
	owner         *deviceSockets
	once          sync.Once
	addrMu        sync.Mutex
	local, remote net.Addr
}

func (c *deviceSocketConn) LocalAddr() net.Addr {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	if addr := c.Conn.LocalAddr(); addr != nil {
		c.local = addr
	}
	return c.local
}
func (c *deviceSocketConn) RemoteAddr() net.Addr {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	if addr := c.Conn.RemoteAddr(); addr != nil {
		c.remote = addr
	}
	return c.remote
}

func (c *deviceSocketConn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.LocalAddr()
		_ = c.RemoteAddr()
		err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return err
}
func (c *deviceSocketConn) CloseRead() error  { return socketHalfClose(c.Conn, true) }
func (c *deviceSocketConn) CloseWrite() error { return socketHalfClose(c.Conn, false) }

func socketHalfClose(c net.Conn, read bool) error {
	if read {
		if x, ok := c.(interface{ CloseRead() error }); ok {
			return x.CloseRead()
		}
	} else {
		if x, ok := c.(interface{ CloseWrite() error }); ok {
			return x.CloseWrite()
		}
	}
	return errors.New("half-close is unsupported for this connection")
}

func socketNetwork(network string) error {
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
		return nil
	}
	return net.UnknownNetworkError(network)
}

//gomobile:noexport
func (d *DeviceLocal) DialTls(network, address string, config *tls.Config) (net.Conn, error) {
	return d.DialTlsContext(context.Background(), network, address, config)
}

//gomobile:noexport
func (d *DeviceLocal) DialTlsContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
	return dialSocketTLS(ctx, d.DialContext, network, address, config)
}

//gomobile:noexport
func (d *DeviceRemote) DialTls(network, address string, config *tls.Config) (net.Conn, error) {
	return d.DialTlsContext(context.Background(), network, address, config)
}

//gomobile:noexport
func (d *DeviceRemote) DialTlsContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
	return dialSocketTLS(ctx, d.DialContext, network, address, config)
}

func dialSocketTLS(ctx context.Context, dial connect.DialContextFunction, network, address string, config *tls.Config) (net.Conn, error) {
	if err := socketNetwork(network); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return raceSocketFamilies(ctx, network, address, func(ctx context.Context, family string) (net.Conn, error) {
		raw, err := dial(ctx, family, address)
		if err != nil {
			return nil, err
		}
		var secure net.Conn
		if strings.HasPrefix(family, "tcp") {
			c := tls.Client(raw, config)
			err = c.HandshakeContext(ctx)
			secure = c
		} else {
			if config.VerifyConnection != nil || config.GetClientCertificate != nil || len(config.CipherSuites) > 0 || config.MinVersion > tls.VersionTLS12 ||
				(config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS12) || len(config.CurvePreferences) > 0 ||
				len(config.EncryptedClientHelloConfigList) > 0 || config.EncryptedClientHelloRejectionVerify != nil || config.Time != nil || config.Rand != nil || config.ClientSessionCache != nil {
				_ = raw.Close()
				return nil, errors.New("TLS configuration contains options unsupported by DTLS 1.2")
			}
			c, createErr := dtls.Client(&connectedPacketConn{Conn: raw}, raw.RemoteAddr(), &dtls.Config{
				ServerName: config.ServerName, RootCAs: config.RootCAs,
				Certificates: config.Certificates, SupportedProtocols: config.NextProtos,
				InsecureSkipVerify: config.InsecureSkipVerify, VerifyPeerCertificate: config.VerifyPeerCertificate,
				KeyLogWriter: config.KeyLogWriter,
			})
			err = createErr
			if err == nil {
				err = c.HandshakeContext(ctx)
				secure = c
			}
		}
		if err != nil {
			_ = raw.Close()
			if secure != nil {
				_ = secure.Close()
			}
			return nil, err
		}
		return secure, nil
	}, func(c net.Conn) { _ = c.Close() })
}

// Race handshakes, not UDP connect(), which cannot establish reachability.
// Each attempt resolves its family through the device's TUN resolver.
func raceSocketFamilies[T any](ctx context.Context, network, address string, dial func(context.Context, string) (T, error), closeConn func(T)) (T, error) {
	host, _, err := net.SplitHostPort(address)
	var zero T
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if _, err := netip.ParseAddr(host); err == nil || strings.HasSuffix(network, "4") || strings.HasSuffix(network, "6") {
		return dial(ctx, network)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn T
		err  error
	}
	results := make(chan result)
	launch := func(family string) {
		go func() {
			c, err := dial(ctx, family)
			select {
			case results <- result{c, err}:
			case <-ctx.Done():
				if err == nil {
					closeConn(c)
				}
			}
		}()
	}
	launch(network + "6")
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	second, failures := false, 0
	for {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-timer.C:
			if !second {
				second = true
				launch(network + "4")
			}
		case r := <-results:
			if r.err == nil {
				if ctx.Err() != nil {
					closeConn(r.conn)
					return zero, ctx.Err()
				}
				return r.conn, nil
			}
			failures++
			if failures == 2 {
				return zero, r.err
			}
			if !second {
				timer.Stop()
				second = true
				launch(network + "4")
			}
		}
	}
}

// A connected UDP socket adapted to PacketConn without permitting writes to a
// different peer. This also prevents QUIC/DTLS from opening a host UDP socket.
type connectedPacketConn struct{ net.Conn }

func (c *connectedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}
func (c *connectedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if addr == nil || addr.String() != c.RemoteAddr().String() {
		return 0, fmt.Errorf("connected UDP peer mismatch")
	}
	return c.Write(p)
}

var _ Dialer = (*DeviceLocal)(nil)
var _ Dialer = (*DeviceRemote)(nil)
var _ Dialer = (*net.Dialer)(nil)
var _ TLSDialer = (*DeviceLocal)(nil)
var _ TLSDialer = (*DeviceRemote)(nil)
