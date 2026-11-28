package hotspot

// listener.go — the screen in front of the proxy servers. A listener is bound
// to one hotspot address only; the screen also closes, before the proxy reads
// a byte, every accepted connection whose source is outside that address's
// subnet, and every connection over the proxy's connection cap. The subnet
// check matters because an address bind alone does not pin the interface: a
// host that accepts a packet for its address on any interface would also
// accept one sent to the hotspot address from elsewhere.

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// The default bind: tcp4 at exactly the hotspot address and port.
func listenTcp4(ctx context.Context, address netip.AddrPort) (net.Listener, error) {
	listenConfig := net.ListenConfig{}
	return listenConfig.Listen(ctx, "tcp4", address.String())
}

// Counts the connections the proxy serves, over all its listeners. Safe for
// concurrent use.
type hotspotProxyConnectionBudget struct {
	stateLock sync.Mutex
	// non-positive is no cap
	maxConnectionCount int
	connectionCount    int
}

func newHotspotProxyConnectionBudget(maxConnectionCount int) *hotspotProxyConnectionBudget {
	return &hotspotProxyConnectionBudget{
		maxConnectionCount: maxConnectionCount,
	}
}

// Takes one connection from the budget, false when it is spent.
func (self *hotspotProxyConnectionBudget) acquire() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < self.maxConnectionCount && self.maxConnectionCount <= self.connectionCount {
		return false
	}
	self.connectionCount += 1
	return true
}

func (self *hotspotProxyConnectionBudget) release() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectionCount -= 1
}

func (self *hotspotProxyConnectionBudget) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectionCount
}

// A listener that hands the proxy only admitted connections. The proxy server
// owns the listener; closing it closes the bound listener.
type hotspotProxyScreeningListener struct {
	net.Listener
	// the subnet of the hotspot address: the sources admitted
	sourcePrefix netip.Prefix
	budget       *hotspotProxyConnectionBudget
	// connections closed by the screen, for the status and tests
	refusedCount *atomic.Int64
}

// Returns the next admitted connection. A refused one is closed here and never
// reaches the proxy. Returns the bound listener's error, which is how a close
// ends the serve loop.
func (self *hotspotProxyScreeningListener) Accept() (net.Conn, error) {
	for {
		conn, err := self.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !self.admits(conn.RemoteAddr()) || !self.budget.acquire() {
			// counted before the close, so a peer that sees the close sees the count
			self.refusedCount.Add(1)
			conn.Close()
			continue
		}
		admitted := &hotspotProxyConn{
			Conn:   conn,
			budget: self.budget,
		}
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			// keep the half-close the relays pass a clean end of stream with
			return &hotspotProxyTcpConn{
				hotspotProxyConn: admitted,
				tcpConn:          tcpConn,
			}, nil
		}
		return admitted, nil
	}
}

// Whether the source is in the hotspot subnet. A source that is not a tcp
// address is refused.
func (self *hotspotProxyScreeningListener) admits(remoteAddr net.Addr) bool {
	tcpAddr, ok := remoteAddr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return self.sourcePrefix.Contains(tcpAddr.AddrPort().Addr().Unmap())
}

// An admitted connection, which returns its place in the budget once, at its
// first close. The proxy closes a hijacked connection itself, and the http
// server closes the others.
type hotspotProxyConn struct {
	net.Conn
	budget    *hotspotProxyConnectionBudget
	closeOnce sync.Once
}

func (self *hotspotProxyConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(self.budget.release)
	return err
}

// An admitted tcp connection, which keeps the tcp half-closes that the
// proxies' relays find by interface.
type hotspotProxyTcpConn struct {
	*hotspotProxyConn
	tcpConn *net.TCPConn
}

func (self *hotspotProxyTcpConn) CloseWrite() error {
	return self.tcpConn.CloseWrite()
}

func (self *hotspotProxyTcpConn) CloseRead() error {
	return self.tcpConn.CloseRead()
}
