package sdk

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/rpc"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Both servers receive the initial datagram, but only IPv4 replies. A later
// IPv6 reply must not change the selected peer or duplicate another write.
func TestSocketUDPInitialDatagramRace(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "rpc"}[remote], func(t *testing.T) {
			n := newSocketTestNetwork(t)
			type receipt struct {
				family int
				data   []byte
				addr   net.Addr
			}
			packets := make(chan receipt, 8)
			servers := make(map[int]net.PacketConn)
			for _, family := range []int{4, 6} {
				pc, err := n.peer.ListenUDP(&net.UDPAddr{IP: n.ip(family == 6), Port: 8123})
				if err != nil {
					t.Fatal(err)
				}
				servers[family] = pc
				t.Cleanup(func() { _ = pc.Close() })
				go func() {
					for {
						p := make([]byte, 65535)
						size, addr, err := pc.ReadFrom(p)
						if err != nil {
							return
						}
						select {
						case packets <- receipt{family, p[:size], addr}:
						case <-t.Context().Done():
							return
						}
					}
				}()
			}
			var device Dialer = n.device
			if remote {
				device, _, _ = socketTestRemote(t, n)
			}
			c, err := device.Dial("udp", "socket.test:8123")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			initial := []byte("original")
			if _, err := c.Write(initial); err != nil {
				t.Fatal(err)
			}
			initial[0] = 'X'
			next := make(chan error, 1)
			go func() { _, err := c.Write([]byte("second")); next <- err }()
			receive := func() receipt {
				select {
				case p := <-packets:
					return p
				case <-time.After(2 * time.Second):
					t.Fatal("missing family datagram")
					return receipt{}
				}
			}
			v6, v4 := receive(), receive()
			if v6.family != 6 || v4.family != 4 || string(v6.data) != "original" || string(v4.data) != "original" {
				t.Fatalf("initial race: %+v %+v", v6, v4)
			}
			select {
			case <-next:
				t.Fatal("later write completed before a reply")
			default:
			}
			_, _ = servers[4].WriteTo([]byte("winner"), v4.addr)
			buf := make([]byte, 100)
			if size, err := c.Read(buf); err != nil || string(buf[:size]) != "winner" {
				t.Fatal(size, err)
			}
			if err := <-next; err != nil {
				t.Fatal(err)
			}
			second := receive()
			if second.family != 4 || string(second.data) != "second" {
				t.Fatalf("later write: %+v", second)
			}
			_, _ = servers[6].WriteTo([]byte("late loser"), v6.addr)
			_, _ = servers[4].WriteTo([]byte("selected"), v4.addr)
			if size, err := c.Read(buf); err != nil || string(buf[:size]) != "selected" {
				t.Fatal(size, err)
			}
			if addr := c.RemoteAddr().(*net.UDPAddr); addr.IP.To4() == nil {
				t.Fatal("winner address not exposed")
			}
			select {
			case unexpected := <-packets:
				t.Fatalf("unexpected duplicate: %+v", unexpected)
			default:
			}
		})
	}
}

func TestSocketUDPRaceDeadlineRecoveryAndClose(t *testing.T) {
	n := newSocketTestNetwork(t)
	pc, err := n.peer.ListenUDP(&net.UDPAddr{IP: n.ip(true), Port: 8124})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	c, err := n.device.Dial("udp", "socket.test:8124")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Millisecond))
	_, _ = c.Write(nil)
	p := make([]byte, 1)
	if _, err := c.Read(p); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("wait")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, addr, err := pc.ReadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pc.WriteTo(nil, addr)
	if size, err := c.Read(p); err != nil || size != 0 {
		t.Fatal("empty winning datagram", size, err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Read(p); done <- err }()
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock")
	}
}

func TestSocketFragmentedDatagrams(t *testing.T) {
	n := newSocketTestNetwork(t)
	for _, family := range []string{"udp4", "udp6"} {
		c, err := n.device.Dial(family, n.echo(t, family, 0))
		if err != nil {
			t.Fatal(err)
		}
		socketRoundTrip(t, c, bytes.Repeat([]byte{0x7b}, 16000), true)
		_ = c.Close()
	}
}

func TestSocketRPCLimitsAndPendingDialCancellation(t *testing.T) {
	n := newSocketTestNetwork(t)
	_, server, _ := socketTestRemote(t, n)
	call := func(req DeviceSocketRequest) DeviceSocketResponse {
		var reply DeviceSocketResponse
		_ = server.Socket(&req, &reply)
		return reply
	}
	// A blackholed TCP handshake must not occupy the RPC dispatcher.
	n.nat.drop.Store(6)
	id := connect.NewId()
	if r := call(DeviceSocketRequest{ID: id, Op: "open", Start: true, Network: "tcp6", Address: net.JoinHostPort(n.ip(true).String(), "8125")}); !r.Pending {
		t.Fatal(r)
	}
	if r := call(DeviceSocketRequest{ID: id, Op: "close"}); r.err() != nil {
		t.Fatal(r.err())
	}
	server.sockets.workers.Wait()
	server.sockets.mu.Lock()
	count := len(server.sockets.entries)
	for range socketRPCMaxConnections {
		server.sockets.entries[connect.NewId()] = &socketRpcEntry{cancel: func() {}}
	}
	server.sockets.mu.Unlock()
	if count != 0 {
		t.Fatal("canceled dial retained a handle")
	}
	if r := call(DeviceSocketRequest{ID: connect.NewId(), Op: "open", Start: true, Network: "udp4", Address: "127.0.0.1:1"}); r.err() == nil {
		t.Fatal("connection limit ignored")
	}
}

func TestSocketPortableBindings(t *testing.T) {
	n := newSocketTestNetwork(t)
	s, err := n.device.OpenSocket("udp6", n.echo(t, "udp6", 0), 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.SetDeadlineMillis(time.Now().Add(time.Second).UnixMilli())
	if _, err := s.Write(nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(10)
	if err != nil || r.Eof || len(r.Data) != 0 {
		t.Fatal(r, err)
	}
	if _, err := s.Read(0); err == nil {
		t.Fatal("invalid read size accepted")
	}
	if err := s.CloseWrite(); err == nil {
		t.Fatal("UDP half-close accepted")
	}
	if _, err := n.device.OpenSocket("tcp", "test:1", -1, nil); err == nil {
		t.Fatal("invalid timeout accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := n.device.DialContext(ctx, "udp", "socket.test:1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSocketResolverUpdateRetiresCachedAnswers(t *testing.T) {
	n := newSocketTestNetwork(t)
	n.echo(t, "tcp4", 8126)
	c, err := n.device.Dial("tcp", "socket.test:8126")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	settings := connect.DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &connect.DnsResolverSettings{}
	n.device.SetUpgradeMuxSettings(settings)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if extra, err := n.device.DialContext(ctx, "tcp", "socket.test:8126"); err == nil {
		extra.Close()
		t.Fatal("socket reused a retired resolver answer")
	}
	socketRoundTrip(t, c, []byte("existing connection"), false)
}

type socketReorderedRPC struct {
	*DeviceLocalRpc
	started, allow, created, closed chan struct{}
}

func (s *socketReorderedRPC) Socket(req *DeviceSocketRequest, reply *DeviceSocketResponse) error {
	if req.Op == "open" && req.Start {
		close(s.started)
		select {
		case <-s.allow:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
		defer close(s.created)
	}
	err := s.DeviceLocalRpc.Socket(req, reply)
	if req.Op == "close" {
		select {
		case s.closed <- struct{}{}:
		default:
		}
	}
	return err
}

func TestSocketRemoteCanceledOpenArrivesAfterClose(t *testing.T) {
	n := newSocketTestNetwork(t)
	ctx, cancel := context.WithCancel(t.Context())
	local := &DeviceLocalRpc{ctx: ctx, cancel: cancel, deviceLocal: n.device}
	slow := &socketReorderedRPC{DeviceLocalRpc: local, started: make(chan struct{}), allow: make(chan struct{}), created: make(chan struct{}), closed: make(chan struct{}, 2)}
	client, server := net.Pipe()
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("DeviceLocalRpc", slow); err != nil {
		t.Fatal(err)
	}
	go rpcServer.ServeConn(server)
	remote := &DeviceRemote{ctx: ctx, cancel: cancel, settings: defaultDeviceRpcSettings(), service: &rpcClient{ctx: ctx, client: rpc.NewClient(client), timeout: time.Second, closeClient: client.Close}}
	t.Cleanup(func() { cancel(); client.Close(); server.Close(); local.sockets.close(); local.sockets.workers.Wait() })
	dialCtx, stopDial := context.WithCancel(ctx)
	defer stopDial()
	result := make(chan error, 1)
	go func() {
		c, err := remote.DialContext(dialCtx, "udp4", net.JoinHostPort(n.ip(false).String(), "8127"))
		if c != nil {
			c.Close()
		}
		result <- err
	}()
	<-slow.started
	stopDial()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("canceled dial waited for the RPC")
	}
	select {
	case <-slow.closed:
	case <-time.After(time.Second):
		t.Fatal("first close not sent")
	}
	close(slow.allow)
	<-slow.created
	select {
	case <-slow.closed:
	case <-time.After(time.Second):
		t.Fatal("late open was not closed")
	}
	local.sockets.workers.Wait()
	local.sockets.mu.Lock()
	count := len(local.sockets.entries)
	local.sockets.mu.Unlock()
	if count != 0 {
		t.Fatal("late open leaked a socket")
	}
}
