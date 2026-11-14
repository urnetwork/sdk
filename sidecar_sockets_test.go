//go:build !ios_extension && !js

package sdk

// SidecarSockets (SDK-M8-03): Dial and Listen through a fake socket service whose streams are in-memory pipes, so a
// dialed Socket and an accepted one carry bytes both ways; the timeouts' bounds; Accept's timeout and its close; the
// gate's pending answer named as SidecarResult names it; ListenContext as a Go net.Listener; and the component, which
// waits for the core's enrollment and answers unavailable meanwhile. Every address is synthetic.

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A socket service in memory: each address a listener of pipes, a dial reaching the listener its address names.
type testSidecarSocketsBackend struct {
	stateLock sync.Mutex
	listeners map[string]*testSidecarSocketListener
	// each Listen's weight, in order
	weights []int
	// answered by Listen in place of a listener, when set
	listenErr error
}

func newTestSidecarSocketsBackend() *testSidecarSocketsBackend {
	return &testSidecarSocketsBackend{listeners: map[string]*testSidecarSocketListener{}}
}

func (self *testSidecarSocketsBackend) Dial(ctx context.Context, address string) (net.Conn, error) {
	self.stateLock.Lock()
	listener := self.listeners[address]
	self.stateLock.Unlock()
	if listener == nil {
		return nil, &net.OpError{Op: "dial", Net: "ur", Err: sidecar.ErrSocketUnavailable}
	}
	dialed, accepted := net.Pipe()
	select {
	case listener.accepted <- accepted:
		return dialed, nil
	case <-listener.closed:
		dialed.Close()
		accepted.Close()
		return nil, &net.OpError{Op: "dial", Net: "ur", Err: sidecar.ErrSocketUnavailable}
	case <-ctx.Done():
		dialed.Close()
		accepted.Close()
		return nil, ctx.Err()
	}
}

func (self *testSidecarSocketsBackend) Listen(ctx context.Context, address string, weight int) (sidecarSocketListenerBackend, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.listenErr != nil {
		return nil, self.listenErr
	}
	self.weights = append(self.weights, weight)
	listener := &testSidecarSocketListener{
		backend:  self,
		address:  address,
		accepted: make(chan net.Conn),
		closed:   make(chan struct{}),
	}
	self.listeners[address] = listener
	return listener, nil
}

// One listen of the fake's.
type testSidecarSocketListener struct {
	backend   *testSidecarSocketsBackend
	address   string
	accepted  chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	closeSeen int
}

func (self *testSidecarSocketListener) AcceptContext(ctx context.Context) (net.Conn, error) {
	select {
	case conn := <-self.accepted:
		return conn, nil
	case <-self.closed:
		return nil, &net.OpError{Op: "accept", Net: "ur", Err: net.ErrClosed}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (self *testSidecarSocketListener) Accept() (net.Conn, error) {
	return self.AcceptContext(context.Background())
}

func (self *testSidecarSocketListener) Close() error {
	self.backend.stateLock.Lock()
	self.closeSeen += 1
	self.backend.stateLock.Unlock()
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

func (self *testSidecarSocketListener) Addr() net.Addr {
	return &sidecar.SocketAddr{}
}

// SidecarSockets on backend until the test ends.
func newTestSidecarSockets(t *testing.T, backend sidecarSocketsBackend) (*SidecarSockets, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newSidecarSockets(ctx, backend, defaultSidecarSocketsSettings()), cancel
}

// Writes data on from and reads it whole on to.
func requireTestSocketCarries(t *testing.T, from *Socket, to *Socket, data string) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := from.Write([]byte(data))
		written <- err
	}()
	got := []byte{}
	for len(got) < len(data) {
		read, err := to.Read(1024)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, read.Data...)
	}
	if err := <-written; err != nil || string(got) != data {
		t.Fatalf("carried %q, %v", got, err)
	}
}

// A Dial reaches the listener its address names, and the dialed and accepted Sockets carry bytes both ways; the
// listen's weight reaches the core; a closed listener stops accepting, a second Close doing nothing.
func TestSidecarSocketsDialAndListenThroughAFakeService(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, _ := newTestSidecarSockets(t, backend)
	listener, err := sockets.Listen("chat.sockettest", 250)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *Socket, 1)
	go func() {
		socket, err := listener.Accept(0)
		if err != nil {
			t.Error(err)
		}
		accepted <- socket
	}()
	dialed, err := sockets.Dial("chat.sockettest", 0)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	requireTestSocketCarries(t, dialed, server, "a synthetic hello")
	requireTestSocketCarries(t, server, dialed, "a synthetic answer")
	dialed.Close()
	server.Close()
	if len(backend.weights) != 1 || backend.weights[0] != 250 {
		t.Errorf("weights %v", backend.weights)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	listener.Close()
	if _, err := listener.Accept(0); err == nil || !errors.Is(err, net.ErrClosed) {
		t.Errorf("an accept after Close: %v", err)
	}
	if _, err := sockets.Dial("chat.sockettest", 0); err == nil || !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Errorf("a dial of a closed listen: %v", err)
	}
	// a name no one listens on is the core's refusal, as it came
	if _, err := sockets.Dial("nobody.sockettest", 1000); err == nil || !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Errorf("a dial of no listener: %v", err)
	}
}

// A timeout outside 0 and the int32 range is refused before the core sees the call; an Accept's timeout that passes
// with no stream answers its own error; and closing the Sidecar ends a held Accept.
func TestSidecarSocketsTimeouts(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, cancel := newTestSidecarSockets(t, backend)
	for _, timeoutMillis := range []int64{-1, 2147483648} {
		if _, err := sockets.Dial("chat.sockettest", timeoutMillis); !errors.Is(err, errSidecarSocketTimeout) {
			t.Errorf("dial %d: %v", timeoutMillis, err)
		}
	}
	listener, err := sockets.Listen("chat.sockettest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(-5); !errors.Is(err, errSidecarSocketTimeout) {
		t.Errorf("accept -5: %v", err)
	}
	if _, err := listener.Accept(1); !errors.Is(err, errSidecarSocketAcceptTimeout) {
		t.Errorf("an accept whose timeout passed: %v", err)
	}
	held := make(chan error, 1)
	go func() {
		_, err := listener.Accept(0)
		held <- err
	}()
	cancel()
	if err := <-held; !errors.Is(err, errSidecarSocketsClosed) {
		t.Errorf("an accept the close ended: %v", err)
	}
	if _, err := sockets.Listen("other.sockettest", 0); !errors.Is(err, errSidecarSocketsClosed) {
		t.Errorf("a listen after the close: %v", err)
	}
}

// The gate's pending answer to a tls/ or smtp/ listen comes back naming approval_pending:<id>, as SidecarResult does,
// and the approval is still the core's error underneath.
func TestSidecarSocketsListenPendingNamesTheApproval(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	approvalId := sidecar.Id{0x81}
	backend.listenErr = &sidecar.ApprovalPendingError{ApprovalId: approvalId}
	sockets, _ := newTestSidecarSockets(t, backend)
	_, err := sockets.Listen("tls/www", 0)
	var pending *sidecar.ApprovalPendingError
	if err == nil || !strings.HasPrefix(err.Error(), sidecarApprovalPendingPrefix+approvalId.String()) || !errors.As(err, &pending) {
		t.Fatalf("%v", err)
	}
}

// ListenContext is a net.Listener an app's Go server accepts on, its streams the dialer's.
func TestSidecarSocketsListenContextIsANetListener(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, _ := newTestSidecarSockets(t, backend)
	var listener net.Listener
	listener, err := sockets.ListenContext(context.Background(), "web.sockettest")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err.Error()
			return
		}
		defer conn.Close()
		line := make([]byte, 5)
		if _, err := io.ReadFull(conn, line); err != nil {
			served <- err.Error()
			return
		}
		served <- string(line)
	}()
	dialed, err := sockets.Dial("web.sockettest", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	if _, err := dialed.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if line := <-served; line != "hello" {
		t.Fatalf("served %q", line)
	}
}

// GetSockets is nil unless sockets is named. Named, the component waits for the core's enrollment rather than refuse
// the Sidecar, its Dial answering the core's unavailable meanwhile; once the enrollment comes the waiting component is
// built and attached, and one that still refuses waits on.
func TestGetSocketsAndTheComponentWaitingForEnrollment(t *testing.T) {
	world := newTestSidecarWorld(t)
	if bare := world.open(t); bare.GetSockets() != nil {
		t.Error("GetSockets without sockets named")
	}

	// the registered factory: the core is not enrolled, so the component waits
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentSockets)
	second := newTestSidecarWorld(t)
	local := second.openWith(t, settings, &sidecarLocalOptions{})
	sockets := local.GetSockets()
	if sockets == nil || local.GetSockets() != sockets || local.component(SidecarComponentSockets) != nil {
		t.Fatalf("GetSockets %v, component %v", sockets, local.component(SidecarComponentSockets))
	}
	local.stateLock.Lock()
	deferred := append([]string{}, local.deferredComponents...)
	local.stateLock.Unlock()
	if len(deferred) != 1 || deferred[0] != SidecarComponentSockets {
		t.Fatalf("deferred %v", deferred)
	}
	if _, err := sockets.Dial("chat.sockettest", 1000); err == nil || !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Errorf("a dial before the component: %v", err)
	}
	if _, err := sockets.Listen("chat.sockettest", 0); err == nil || !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Errorf("a listen before the component: %v", err)
	}
	// still unenrolled, the enrollment's step leaves it waiting
	if waiting := local.addDeferredComponents(local.Core()); len(waiting) != 1 {
		t.Errorf("waiting %v", waiting)
	}
	second.close(t, local)
	if local.GetSockets() != nil {
		t.Error("GetSockets after Close")
	}

	// a factory that waits once and then builds: the enrollment's step attaches it
	enrolled := false
	component := &testSidecarComponent{name: SidecarComponentSockets, closed: make(chan struct{})}
	third := newTestSidecarWorld(t)
	local = third.openWith(t, settings, &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{
		SidecarComponentSockets: func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
			if !enrolled {
				return nil, errSidecarComponentNotEnrolled
			}
			return component, nil
		},
	}})
	if local.component(SidecarComponentSockets) != nil {
		t.Fatal("built before the enrollment")
	}
	enrolled = true
	if waiting := local.addDeferredComponents(local.Core()); len(waiting) != 0 || local.component(SidecarComponentSockets) != component {
		t.Fatalf("waiting %v, component %v", waiting, local.component(SidecarComponentSockets))
	}
	third.close(t, local)
	select {
	case <-component.closed:
	default:
		t.Error("the component outlived the Sidecar")
	}
}
