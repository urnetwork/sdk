//go:build !js

package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/v2026"
)

// Synthetic authorization tests the SDK's header boundary only; production
// signed-id verification is covered by server/proxy's endpoint tests.
const h1PlusRpcTestBearer = "h1plus-test-only-signed-id+/="

type h1PlusRpcRequest struct {
	protocol, remote string
	headerAuth       bool
	queryEmpty       bool
}

type h1PlusRpcFixture struct {
	server   *httptest.Server
	accepted chan *deviceRpcMux
	mu       sync.Mutex
	requests []h1PlusRpcRequest
}

func h1PlusRpcSettings() *deviceRpcSettings {
	s := defaultDeviceRpcSettings()
	s.EnableH1Plus = true
	s.H1PlusStats = &connect.H1PlusStats{}
	s.RpcConnectTimeout = 3 * time.Second
	s.KeepAliveTimeout = 0
	s.MuxWriteTimeout = 2 * time.Second
	s.DisableLogging = true
	return s
}

func newH1PlusRpcFixture(t *testing.T, custom bool, rejectStatus int) *h1PlusRpcFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f := &h1PlusRpcFixture{accepted: make(chan *deviceRpcMux, 8)}
	s := h1PlusRpcSettings()
	upgrader := websocket.Upgrader{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation := h1PlusRpcRequest{
			protocol: r.Header.Get("Upgrade"), remote: r.RemoteAddr,
			headerAuth: r.Header.Get("Authorization") == "Bearer "+h1PlusRpcTestBearer,
			queryEmpty: r.URL.RawQuery == "",
		}
		f.mu.Lock()
		f.requests = append(f.requests, observation)
		f.mu.Unlock()
		if r.URL.Path != "/device-rpc" || !observation.headerAuth || !observation.queryEmpty {
			http.Error(w, "authorization required", http.StatusUnauthorized)
			return
		}
		if rejectStatus != 0 {
			http.Error(w, "test authorization denial", rejectStatus)
			return
		}
		var ws deviceRpcWs
		if connect.IsFramedUpgrade(r, connect.H1FramerXlProtocol) {
			if !custom {
				http.Error(w, "upgrade unavailable", http.StatusUpgradeRequired)
				return
			}
			raw, err := connect.AcceptFramedUpgrade(w, r, connect.H1FramerXlProtocol, time.Second)
			if err != nil {
				t.Errorf("accept custom upgrade: %v", err)
				return
			}
			framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerXlProtocol, int(s.maxFrameBytes()), nil)
			if err != nil {
				raw.Close()
				t.Errorf("create XL carrier: %v", err)
				return
			}
			ws = framed
		} else {
			var err error
			ws, err = upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("accept websocket: %v", err)
				return
			}
		}
		mux := newDeviceRpcMux(ctx, ws, s)
		defer mux.close()
		select {
		case f.accepted <- mux:
		case <-ctx.Done():
			return
		}
		<-mux.ctx.Done()
	}))
	t.Cleanup(func() {
		cancel()
		f.server.Close()
	})
	return f
}

func (f *h1PlusRpcFixture) dial(t *testing.T, s *deviceRpcSettings) (client, server *deviceRpcMux) {
	t.Helper()
	dialer := NewPlatformDeviceRpcDialer(f.server.URL, h1PlusRpcTestBearer, s)
	forward, reverse, err := dialer.Dial(t.Context())
	if err != nil {
		t.Fatalf("native RPC dial: %v", err)
	}
	t.Cleanup(func() { forward.Close(); reverse.Close() })
	client = forward.(*deviceRpcMuxConn).mux
	select {
	case server = <-f.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("native RPC session was not accepted")
	}
	t.Cleanup(func() { server.close() })
	return client, server
}

func (f *h1PlusRpcFixture) requestSnapshot() []h1PlusRpcRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]h1PlusRpcRequest(nil), f.requests...)
}

func TestDeviceRpcH1PlusNativeHeaderAuthAndOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			fixture := newH1PlusRpcFixture(t, true, 0)
			s := h1PlusRpcSettings()
			s.EnableH1Plus = enabled
			client, server := fixture.dial(t, s)
			_, clientFramed := client.ws.(*connect.FramedMessageConn)
			_, serverFramed := server.ws.(*connect.FramedMessageConn)
			if clientFramed != enabled || serverFramed != enabled {
				t.Fatalf("enabled=%t selected client=%T server=%T", enabled, client.ws, server.ws)
			}
			requests := fixture.requestSnapshot()
			if len(requests) != 1 || !requests[0].headerAuth || !requests[0].queryEmpty {
				t.Fatalf("native authorization crossed the query boundary: %+v", requests)
			}
			wantProtocol := "websocket"
			if enabled {
				wantProtocol = connect.H1FramerXlProtocol
			}
			if requests[0].protocol != wantProtocol {
				t.Fatalf("upgrade=%q want=%q", requests[0].protocol, wantProtocol)
			}
			snapshot := s.H1PlusStats.Snapshot()
			if enabled && (snapshot.Attempts != 1 || snapshot.Accepted != 1 || snapshot.Fallbacks != 0) {
				t.Fatalf("custom selection counters: %+v", snapshot)
			}
			if !enabled && (snapshot.Attempts != 0 || snapshot.Accepted != 0) {
				t.Fatalf("disabled custom path attempted upgrade: %+v", snapshot)
			}
		})
	}
}

func TestDeviceRpcH1PlusOldProviderFreshFallbackAndReconnect(t *testing.T) {
	fixture := newH1PlusRpcFixture(t, false, 0)
	s := h1PlusRpcSettings()
	for range 2 {
		client, server := fixture.dial(t, s)
		if _, ok := client.ws.(*websocket.Conn); !ok {
			t.Fatalf("old-provider fallback carrier=%T", client.ws)
		}
		h1PlusRpcTransfer(t, client.conns[0], server.conns[0], h1PlusRpcPayload(96*1024))
		client.close()
		server.close()
		h1PlusRpcWaitBudgetsReleased(t, client, server)
	}
	requests := fixture.requestSnapshot()
	if len(requests) != 3 || requests[0].protocol != connect.H1FramerXlProtocol || requests[1].protocol != "websocket" || requests[2].protocol != "websocket" {
		t.Fatalf("old-provider attempts/cached reconnect=%+v", requests)
	}
	if requests[0].remote == requests[1].remote || requests[1].remote == requests[2].remote {
		t.Fatal("fallback or reconnect reused a failed/closed transport socket")
	}
	for _, r := range requests {
		if !r.headerAuth || !r.queryEmpty {
			t.Fatal("fallback changed header-only authorization")
		}
	}
	if snapshot := s.H1PlusStats.Snapshot(); snapshot.Attempts != 1 || snapshot.Fallbacks != 1 || snapshot.Accepted != 0 {
		t.Fatalf("old-provider capability cache/counters=%+v", snapshot)
	}
}

func TestDeviceRpcH1PlusAuthorizationDenialDoesNotFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fixture := newH1PlusRpcFixture(t, true, status)
			s := h1PlusRpcSettings()
			ws, err := dialDeviceRpcWs(t.Context(), fixture.server.URL, h1PlusRpcTestBearer, s)
			if ws != nil || err == nil || connect.HTTPUpgradeAllowsFallback(err) {
				t.Fatalf("authorization denial permitted carrier/fallback: %T %v", ws, err)
			}
			if len(fixture.requestSnapshot()) != 1 {
				t.Fatal("authorization denial triggered another authenticated attempt")
			}
			if stats := s.H1PlusStats.Snapshot(); stats.AuthFailures != 1 || stats.Fallbacks != 0 {
				t.Fatalf("authorization counters=%+v", stats)
			}
		})
	}
}

type H1PlusRpcEchoArgs struct{ Payload []byte }
type h1PlusRpcEcho struct{}

func (*h1PlusRpcEcho) Echo(args H1PlusRpcEchoArgs, reply *[]byte) error {
	*reply = args.Payload
	return nil
}

func TestDeviceRpcH1PlusRealForwardReverseRpc(t *testing.T) {
	fixture := newH1PlusRpcFixture(t, true, 0)
	clientMux, serverMux := fixture.dial(t, h1PlusRpcSettings())
	forwardServer, reverseServer := rpc.NewServer(), rpc.NewServer()
	for _, s := range []*rpc.Server{forwardServer, reverseServer} {
		if err := s.RegisterName("Echo", &h1PlusRpcEcho{}); err != nil {
			t.Fatal(err)
		}
	}
	served := make(chan struct{}, 2)
	go func() { forwardServer.ServeConn(serverMux.conns[0]); served <- struct{}{} }()
	go func() { reverseServer.ServeConn(clientMux.conns[1]); served <- struct{}{} }()
	forwardClient := rpc.NewClient(clientMux.conns[0])
	reverseClient := rpc.NewClient(serverMux.conns[1])
	defer func() {
		forwardClient.Close()
		reverseClient.Close()
		for range 2 {
			select {
			case <-served:
			case <-time.After(3 * time.Second):
				t.Error("RPC server did not stop after mux close")
			}
		}
	}()
	// Both logical directions carry actual gob/net-rpc calls above uint16.
	// Reserve encoding/tag headroom for the large near-3MiB call.
	for _, size := range []int{96 * 1024, int(deviceRpcDefaultMaxFrameBytes) - 4096} {
		payload := h1PlusRpcPayload(size)
		errs := make(chan error, 2)
		for _, c := range []*rpc.Client{forwardClient, reverseClient} {
			go func() {
				var reply []byte
				if err := c.Call("Echo.Echo", H1PlusRpcEchoArgs{payload}, &reply); err != nil {
					errs <- err
				} else if !bytes.Equal(reply, payload) {
					errs <- errors.New("RPC reply payload changed")
				} else {
					errs <- nil
				}
			}()
		}
		for range 2 {
			select {
			case err := <-errs:
				if err != nil {
					t.Fatalf("%d-byte forward/reverse RPC: %v", size, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("forward/reverse RPC made no progress")
			}
		}
	}
}

func TestDeviceRpcH1PlusExactThreeMiBEnvelopeAndReconnect(t *testing.T) {
	fixture := newH1PlusRpcFixture(t, true, 0)
	s := h1PlusRpcSettings()
	for generation := range 2 {
		client, server := fixture.dial(t, s)
		// One stream-tag byte plus this payload is the exact unchanged 3MiB
		// envelope. Both independent directions remain able to make progress.
		p := h1PlusRpcPayload(int(s.maxFrameBytes()) - 1)
		h1PlusRpcTransfer(t, client.conns[0], server.conns[0], p)
		h1PlusRpcTransfer(t, server.conns[1], client.conns[1], p)
		if generation == 0 {
			if n, err := client.conns[0].Write(make([]byte, s.maxFrameBytes())); n != 0 || err == nil {
				t.Fatal("one-byte oversize envelope was accepted")
			}
		}
		client.close()
		server.close()
		h1PlusRpcWaitBudgetsReleased(t, client, server)
	}
	if stats := s.H1PlusStats.Snapshot(); stats.Attempts != 2 || stats.Accepted != 2 || stats.Fallbacks != 0 {
		t.Fatalf("reconnect changed custom carrier selection: %+v", stats)
	}
}

func TestDeviceRpcH1PlusLocalMutualTlsAndFallback(t *testing.T) {
	material, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                         string
		client, server, mtls, wantXl bool
	}{
		{"mutual-tls", true, true, true, true},
		{"old-listener", true, false, true, false},
		{"client-disabled", false, true, true, false},
		{"plain-local", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientSettings, serverSettings := h1PlusRpcSettings(), h1PlusRpcSettings()
			clientSettings.EnableH1Plus, serverSettings.EnableH1Plus = tc.client, tc.server
			address := requireRemoteAddress(testing_freeHostPort())
			var serverPem, clientCert, clientPem, serverCert string
			if tc.mtls {
				serverPem, clientCert = material.GetServerPem(), material.GetClientCertPem()
				clientPem, serverCert = material.GetClientPem(), material.GetServerCertPem()
			}
			listener := NewWebsocketDeviceRpcListener(address, serverPem, clientCert, serverSettings)
			defer listener.Close()
			if err := listener.ensureStarted(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			dialer := NewWebsocketDeviceRpcDialer(address, clientPem, serverCert, clientSettings)
			cf, cr, err := dialer.Dial(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer cf.Close()
			defer cr.Close()
			sf, sr, err := listener.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.Close()
			defer sr.Close()
			cm, sm := cf.(*deviceRpcMuxConn).mux, sf.(*deviceRpcMuxConn).mux
			_, custom := cm.ws.(*connect.FramedMessageConn)
			_, customServer := sm.ws.(*connect.FramedMessageConn)
			if custom != tc.wantXl || customServer != tc.wantXl {
				t.Fatalf("local selection client=%T server=%T wantXL=%t", cm.ws, sm.ws, tc.wantXl)
			}
			h1PlusRpcTransfer(t, cf, sf, h1PlusRpcPayload(96*1024))
			h1PlusRpcTransfer(t, sr, cr, []byte("reverse-tls-confirmed"))
			stats := clientSettings.H1PlusStats.Snapshot()
			if !tc.mtls && stats.Attempts != 0 {
				t.Fatal("unauthenticated local connection attempted a custom upgrade")
			}
			if tc.name == "old-listener" && (stats.Fallbacks != 1 || stats.Accepted != 0) {
				t.Fatalf("old TLS listener fallback=%+v", stats)
			}
		})
	}
}

// A blocked real FramerXl writer holds one frame while two later frames queue.
// Cancellation must release every reservation and wake the next producer.
func TestDeviceRpcH1PlusCancellationDrainsByteBudget(t *testing.T) {
	s := h1PlusRpcSettings()
	s.MuxMaxFrameBytes, s.MuxMaxQueuedBytes = 256, 3*256
	s.MuxSendBufferSize = 2
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	started := make(chan struct{})
	raw := &h1PlusRpcWriteObserver{Conn: left, started: started}
	framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerXlProtocol, 256, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mux := newDeviceRpcMux(ctx, framed, s)
	defer mux.close()
	observed := [][]byte{}
	for i := range 3 {
		if _, err := mux.conns[i%2].write(make([]byte, 255), func(frame []byte) {
			observed = append(observed, connect.MessagePoolShareReadOnly(frame))
		}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("XL writer never started")
			}
		}
	}
	blocked := make(chan error, 1)
	go func() { _, err := mux.conns[1].Write([]byte{1}); blocked <- err }()
	h1PlusRpcEventually(t, "producer reaches exhausted byte budget", func() bool {
		mux.sendBytes.mu.Lock()
		defer mux.sendBytes.mu.Unlock()
		return mux.sendBytes.used == 3*256 && mux.sendBytes.waiters == 1
	})
	// Send and receive admission must remain independent, even with a writer
	// parked in an actual XL stream Write.
	if !mux.receiveBytes.tryAcquire(256) {
		t.Fatal("send pressure consumed receive progress capacity")
	}
	mux.receiveBytes.release(256)
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("blocked producer cancellation=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation left the producer blocked")
	}
	h1PlusRpcWaitBudgetsReleased(t, mux)
	for _, frame := range observed {
		assertDeviceRpcObservedFrameReturned(t, frame)
	}
}

func TestDeviceRpcH1PlusHeartbeatsKeepBothDirectionsAlive(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		settings := h1PlusRpcSettings()
		settings.KeepAliveTimeout = time.Second
		settings.KeepAliveRetryCount = 2
		stats := []*connect.H1PlusStats{{}, {}}
		muxes := make([]*deviceRpcMux, 0, 2)
		for i, raw := range []net.Conn{left, right} {
			framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerXlProtocol, int(settings.maxFrameBytes()), stats[i])
			if err != nil {
				t.Fatal(err)
			}
			mux := newDeviceRpcMux(t.Context(), framed, settings)
			defer mux.close()
			muxes = append(muxes, mux)
		}
		// Fake time exceeds the three-second read deadline. Each serialized
		// zero-length heartbeat refreshes liveness without becoming RPC data.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		for i, mux := range muxes {
			if mux.ctx.Err() != nil {
				t.Fatal("XL heartbeat failed to refresh peer liveness")
			}
			if got := stats[i].Snapshot(); got.Messages < 4 || got.Bytes != 0 {
				t.Fatalf("heartbeat carrier counters=%+v", got)
			}
			if mux.receiveBytes.used != 0 || len(mux.conns[0].receive) != 0 || len(mux.conns[1].receive) != 0 {
				t.Fatal("transport heartbeats consumed RPC receive capacity")
			}
		}
		for _, mux := range muxes {
			mux.close()
		}
		synctest.Wait()
		for _, mux := range muxes {
			assertDeviceRpcSendDrained(t, mux)
		}
	})
}

type h1PlusRpcWriteObserver struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *h1PlusRpcWriteObserver) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func h1PlusRpcPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*17 + 3)
	}
	return p
}

func h1PlusRpcTransfer(t *testing.T, from, to net.Conn, payload []byte) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		actual := make([]byte, len(payload))
		_, err := io.ReadFull(to, actual)
		if err == nil && !bytes.Equal(actual, payload) {
			err = errors.New("tagged stream payload mismatch")
		}
		result <- err
	}()
	if n, err := from.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("stream write: n=%d err=%v", n, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tagged stream read did not complete")
	}
}

func h1PlusRpcWaitBudgetsReleased(t *testing.T, muxes ...*deviceRpcMux) {
	t.Helper()
	h1PlusRpcEventually(t, "all mux byte ownership released", func() bool {
		for _, mux := range muxes {
			for _, budget := range []*deviceRpcByteBudget{mux.sendBytes, mux.receiveBytes} {
				budget.mu.Lock()
				used, waiters := budget.used, budget.waiters
				budget.mu.Unlock()
				if used != 0 || waiters != 0 {
					return false
				}
			}
		}
		return true
	})
}

func h1PlusRpcEventually(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

// Native dial errors must not expose the synthetic authorization token even
// though both the custom and ordinary paths authenticate with that header.
func TestDeviceRpcH1PlusErrorsDoNotContainCredential(t *testing.T) {
	fixture := newH1PlusRpcFixture(t, true, http.StatusForbidden)
	_, err := dialDeviceRpcWs(t.Context(), fixture.server.URL, h1PlusRpcTestBearer, h1PlusRpcSettings())
	if err == nil || strings.Contains(err.Error(), h1PlusRpcTestBearer) {
		t.Fatalf("unsafe or missing native dial error: %v", err)
	}
}

// H1+ is opt-out for device RPC: new sessions offer XL unless the process opts
// out, and opting back in restores the default.
func TestDeviceRpcH1PlusEnabledByDefault(t *testing.T) {
	if !defaultDeviceRpcSettings().EnableH1Plus {
		t.Fatal("device RPC opted out of H1+ by default")
	}
	SetDeviceRpcH1PlusEnabled(false)
	defer SetDeviceRpcH1PlusEnabled(true)
	if defaultDeviceRpcSettings().EnableH1Plus {
		t.Fatal("device RPC ignored the opt-out")
	}
}
