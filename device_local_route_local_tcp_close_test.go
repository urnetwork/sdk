package sdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A TCP client's FIN through routeLocal closes its direction and nothing more (RFC 9293
// 3.6): the LocalUserNat half-closes the upstream, whatever the upstream sends still
// reaches the client, and the NAT's own FIN follows once the upstream closes, with no
// reset. A client that half-closed after its request used to read nothing until its own
// deadline, and a client's close drew no FIN, only a reset after its retransmit.
//
// The client's retransmission floor is above the test's bound, so no timer fires: what
// the client reads is what the NAT sent the first time.

// How long the client waits for what the NAT owes it. Only a failure takes this long.
const routeLocalTcpCloseTimeout = 30 * time.Second

// A device on routeLocal with a gvisor client bridged to it, as the production IoLoop
// bridges a tun, and a record of the NAT's TCP controls to the client.
type routeLocalTcpCloseHarness struct {
	ctx    context.Context
	cancel context.CancelFunc
	tun    *connect.Tun
	close  func()

	mutex sync.Mutex
	// the client port's FIN from the NAT
	natFins map[int]*routeLocalTcpCloseSignal
	// resets the NAT sent to the client
	natResets atomic.Int64
}

// Closed the first time; a retransmission of the same control signals nothing more.
type routeLocalTcpCloseSignal struct {
	once sync.Once
	done chan struct{}
}

func (self *routeLocalTcpCloseSignal) signal() {
	self.once.Do(func() { close(self.done) })
}

func newRouteLocalTcpCloseHarness(t *testing.T) *routeLocalTcpCloseHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		cancel()
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		cancel()
		t.Fatalf("new device: %v", err)
	}
	device.SetRouteLocal(true)

	tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
	tunSettings.Log = connect.NewNoopLogger()
	tunSettings.TcpMinRto = 5 * time.Minute
	tunSettings.TcpMaxRto = 5 * time.Minute
	tun, err := connect.CreateTun(ctx, tunSettings)
	if err != nil {
		device.Close()
		cancel()
		t.Fatalf("create tun: %v", err)
	}

	h := &routeLocalTcpCloseHarness{
		ctx:     ctx,
		cancel:  cancel,
		tun:     tun,
		natFins: map[int]*routeLocalTcpCloseSignal{},
	}
	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		if path, err := connect.ParseIpPath(packet); err == nil && path.Protocol == connect.IpProtocolTcp {
			if path.Rst {
				h.natResets.Add(1)
			}
			if path.Fin {
				h.natFin(path.DestinationPort).signal()
			}
		}
		tun.Write(packet)
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				if !device.SendPacketNoCopy(packet, int32(len(packet))) {
					MessagePoolReturn(packet)
				}
			}
		}
	}()
	h.close = func() {
		unsub()
		device.Close()
		tun.Close()
		wg.Wait()
		cancel()
	}
	return h
}

// Signaled by the NAT's FIN to the client port.
func (self *routeLocalTcpCloseHarness) natFin(clientPort int) *routeLocalTcpCloseSignal {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	fin, ok := self.natFins[clientPort]
	if !ok {
		fin = &routeLocalTcpCloseSignal{done: make(chan struct{})}
		self.natFins[clientPort] = fin
	}
	return fin
}

// Serves one connection on loopback; its result arrives on the returned channel.
func (self *routeLocalTcpCloseHarness) serveOne(t *testing.T, serve func(conn net.Conn) error) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			result <- fmt.Errorf("accept: %w", err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(routeLocalTcpCloseTimeout))
		result <- serve(conn)
	}()
	return listener.Addr().String(), result
}

func (self *routeLocalTcpCloseHarness) dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := self.tun.DialContext(self.ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func (self *routeLocalTcpCloseHarness) requireNoReset(t *testing.T) {
	t.Helper()
	if resets := self.natResets.Load(); resets != 0 {
		t.Fatalf("the NAT sent the client %d resets", resets)
	}
}

// A client that half-closes after its request reads the response the server sends once
// it has read that request to the end, then EOF.
func TestDeviceLocalRouteLocalTcpHalfCloseReadsTheResponse(t *testing.T) {
	h := newRouteLocalTcpCloseHarness(t)
	defer h.close()

	const request = "request"
	const response = "response to request"
	addr, served := h.serveOne(t, func(conn net.Conn) error {
		read, err := io.ReadAll(conn)
		if err != nil {
			return fmt.Errorf("server read: %w", err)
		}
		if string(read) != request {
			return fmt.Errorf("server read %q, want %q", read, request)
		}
		if _, err := conn.Write([]byte(response)); err != nil {
			return fmt.Errorf("server write: %w", err)
		}
		return nil
	})

	conn := h.dial(t, addr)
	defer conn.Close()
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	closeWriter, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("client conn %T cannot half-close", conn)
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(routeLocalTcpCloseTimeout))
	read, err := io.ReadAll(conn)
	if err != nil || string(read) != response {
		t.Fatalf("client read %q, err=%v, want %q then EOF", read, err, response)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	h.requireNoReset(t)
}

// A client's close draws the NAT's FIN once the server, which reads EOF, closes too,
// and no reset.
func TestDeviceLocalRouteLocalTcpCloseDrawsTheNatFin(t *testing.T) {
	h := newRouteLocalTcpCloseHarness(t)
	defer h.close()

	const payload = "echo this"
	addr, served := h.serveOne(t, func(conn net.Conn) error {
		_, err := io.Copy(conn, conn)
		return err
	})

	conn := h.dial(t, addr)
	clientPort := conn.LocalAddr().(*net.TCPAddr).Port
	natFin := h.natFin(clientPort)
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(routeLocalTcpCloseTimeout))
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != payload {
		t.Fatalf("echo %q, err=%v", echo, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-natFin.done:
	case <-time.After(routeLocalTcpCloseTimeout):
		t.Fatal("the NAT sent no FIN after the client's close")
	}
	if err := <-served; err != nil {
		t.Fatalf("server: %v", err)
	}
	h.requireNoReset(t)
}
