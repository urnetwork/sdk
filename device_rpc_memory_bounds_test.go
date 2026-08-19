package sdk

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/rpc"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestDeviceRpcMemoryDefaultsAreBounded(t *testing.T) {
	settings := defaultDeviceRpcSettings()
	if settings.HttpMaxConcurrent != 2 {
		t.Fatalf("http concurrency = %d, want 2", settings.HttpMaxConcurrent)
	}
	if settings.httpMaxBodyBytes() != deviceRpcDefaultHttpMaxBodyBytes {
		t.Fatalf("http body limit = %d", settings.httpMaxBodyBytes())
	}
	if settings.maxFrameBytes() <= int64(settings.httpMaxBodyBytes()) {
		t.Fatalf("frame limit %d must leave encoding headroom above body limit %d", settings.maxFrameBytes(), settings.httpMaxBodyBytes())
	}
	if settings.maxQueuedBytes() < settings.maxFrameBytes() {
		t.Fatalf("queued byte budget %d cannot hold one frame of %d", settings.maxQueuedBytes(), settings.maxFrameBytes())
	}
}

func TestDeviceRemoteRejectsOversizedHttpRequestBeforeRpc(t *testing.T) {
	settings := defaultDeviceRpcSettings()
	settings.HttpMaxBodyBytes = 8
	remote := &DeviceRemote{settings: settings}

	_, err := remote.httpPostRaw(context.Background(), "https://example.invalid", make([]byte, 9), "")
	if err == nil {
		t.Fatal("oversized http request was accepted")
	}
}

func TestDeviceRemoteHttpAdmissionBackpressuresBeforeRpc(t *testing.T) {
	settings := defaultDeviceRpcSettings()
	settings.HttpMaxConcurrent = 1
	remoteCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	remote := &DeviceRemote{ctx: remoteCtx, settings: settings}

	release, ok := remote.acquireHttp(t.Context())
	if !ok {
		t.Fatal("initial http admission failed")
	}
	acquired := make(chan bool, 1)
	go func() {
		secondRelease, secondOK := remote.acquireHttp(t.Context())
		if secondOK {
			secondRelease()
		}
		acquired <- secondOK
	}()
	select {
	case <-acquired:
		t.Fatal("second http request bypassed admission control")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case secondOK := <-acquired:
		if !secondOK {
			t.Fatal("blocked http admission did not resume")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked http admission deadlocked")
	}
}

func TestDeviceLocalHttpAdmissionRejectsWithoutQueueing(t *testing.T) {
	localCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	local := &DeviceLocalRpc{
		ctx:             localCtx,
		httpSem:         make(chan struct{}, 1),
		httpDeliverySem: make(chan struct{}, 1),
	}
	release, ok := local.tryAcquireHttp()
	if !ok {
		t.Fatal("initial local http admission failed")
	}
	if _, secondOK := local.tryAcquireHttp(); secondOK {
		t.Fatal("second local http request was queued past the limit")
	}
	release()
	secondRelease, secondOK := local.tryAcquireHttp()
	if !secondOK {
		t.Fatal("local http admission did not recover after release")
	}
	secondRelease()
}

func TestHttpRpcServerErrorDoesNotRequireSessionTeardown(t *testing.T) {
	var serverError error = rpc.ServerError("device rpc http concurrency limit reached")
	if httpRpcErrorRequiresCleanup(serverError) {
		t.Fatal("http application error required rpc session teardown")
	}
	if !httpRpcErrorRequiresCleanup(io.ErrClosedPipe) {
		t.Fatal("http transport error did not require rpc session teardown")
	}
}

func TestDeviceRemoteHttpResponseDropsOversizedBody(t *testing.T) {
	response := newDeviceRemoteHttpResponseWithLimit(connect.NewId(), make([]byte, 9), nil, 8)
	if response.Error == nil {
		t.Fatal("oversized response has no error")
	}
	if response.BodyBytes != nil {
		t.Fatal("oversized response retained its body")
	}

	response = newDeviceRemoteHttpResponseWithLimit(connect.NewId(), make([]byte, 9), io.ErrUnexpectedEOF, 8)
	if response.BodyBytes != nil {
		t.Fatal("oversized errored response retained its body")
	}
}

func TestDeviceRpcMuxRejectsOversizedInboundFrame(t *testing.T) {
	settings := boundedDeviceRpcTestSettings()
	ws := newBoundedDeviceRpcTestWs(bytes.NewReader(make([]byte, settings.MuxMaxFrameBytes+1)))
	newDeviceRpcMux(t.Context(), ws, settings)

	select {
	case <-ws.closed:
	case <-time.After(time.Second):
		t.Fatal("oversized inbound frame did not close the mux")
	}
	if ws.getReadLimit() != settings.MuxMaxFrameBytes {
		t.Fatalf("websocket read limit = %d, want %d", ws.getReadLimit(), settings.MuxMaxFrameBytes)
	}
}

func TestDeviceRpcMuxRejectsOversizedOutboundFrameBeforeCopy(t *testing.T) {
	settings := boundedDeviceRpcTestSettings()
	ws := newBoundedDeviceRpcTestWs(nil)
	mux := newDeviceRpcMux(t.Context(), ws, settings)

	// The stream tag consumes one byte, so a payload equal to the frame limit
	// is one byte too large.
	if _, err := mux.conns[deviceRpcStreamForward].Write(make([]byte, settings.MuxMaxFrameBytes)); err == nil {
		t.Fatal("oversized outbound frame was accepted")
	}
	select {
	case <-ws.closed:
	case <-time.After(time.Second):
		t.Fatal("oversized outbound frame did not close the mux")
	}
}

func TestDeviceRpcQueuedByteBudgetBackpressuresAtomically(t *testing.T) {
	budget := newDeviceRpcByteBudget(10)
	if !budget.acquire(t.Context(), 8) {
		t.Fatal("initial budget acquisition failed")
	}

	acquired := make(chan bool, 1)
	go func() {
		acquired <- budget.acquire(t.Context(), 8)
	}()
	select {
	case <-acquired:
		t.Fatal("second acquisition bypassed the byte budget")
	case <-time.After(20 * time.Millisecond):
	}

	budget.release(8)
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("blocked acquisition did not resume")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked acquisition deadlocked")
	}
	budget.release(8)
}

func TestDeviceRpcReceiveByteBudgetRefusesWithoutWaiting(t *testing.T) {
	budget := newDeviceRpcByteBudget(10)
	if !budget.tryAcquire(8) {
		t.Fatal("initial receive budget acquisition failed")
	}
	if budget.tryAcquire(8) {
		t.Fatal("receive budget exceeded its byte limit")
	}
	budget.release(8)
}

// A reliable stream fragment cannot be dropped and skipped, but a saturated
// logical stream must not park the websocket reader and starve the other RPC
// direction. The mux closes promptly so DeviceRemote can reconnect cleanly.
func TestDeviceRpcMuxReceiveQueueSaturationClosesWithoutBlocking(t *testing.T) {
	settings := boundedDeviceRpcTestSettings()
	settings.MuxMaxQueuedBytes = 4 * settings.MuxMaxFrameBytes
	ws := newScriptedDeviceRpcTestWs([]io.Reader{
		bytes.NewReader([]byte{deviceRpcStreamReverse, 1}),
		bytes.NewReader([]byte{deviceRpcStreamReverse, 2}),
	})
	newDeviceRpcMux(t.Context(), ws, settings)

	select {
	case <-ws.closed:
	case <-time.After(time.Second):
		t.Fatal("full RPC receive queue blocked the shared websocket reader")
	}
}

func TestDeviceRpcMuxKeepsIndependentDirectionBudgets(t *testing.T) {
	settings := boundedDeviceRpcTestSettings()
	settings.MuxMaxQueuedBytes = settings.MuxMaxFrameBytes
	ws := newBoundedDeviceRpcTestWs(nil)
	mux := newDeviceRpcMux(t.Context(), ws, settings)
	defer mux.close()

	frameBytes := int(settings.MuxMaxFrameBytes)
	if !mux.receiveBytes.acquire(t.Context(), frameBytes) {
		t.Fatal("receive direction could not reserve one maximum frame")
	}
	defer mux.receiveBytes.release(frameBytes)
	if !mux.sendBytes.acquire(t.Context(), frameBytes) {
		t.Fatal("send direction was blocked by receive queue occupancy")
	}
	mux.sendBytes.release(frameBytes)
}

func boundedDeviceRpcTestSettings() *deviceRpcSettings {
	settings := defaultDeviceRpcSettings()
	settings.KeepAliveTimeout = 0
	settings.MuxMaxFrameBytes = 16
	settings.MuxMaxQueuedBytes = 32
	settings.MuxSendBufferSize = 1
	settings.MuxReceiveBufferSize = 1
	return settings
}

type boundedDeviceRpcTestWs struct {
	reader io.Reader

	nextOnce  sync.Once
	closeOnce sync.Once
	closed    chan struct{}

	mu        sync.Mutex
	readLimit int64
}

func newBoundedDeviceRpcTestWs(reader io.Reader) *boundedDeviceRpcTestWs {
	return &boundedDeviceRpcTestWs{reader: reader, closed: make(chan struct{})}
}

func (self *boundedDeviceRpcTestWs) WriteMessage(int, []byte) error            { return nil }
func (self *boundedDeviceRpcTestWs) WriteControl(int, []byte, time.Time) error { return nil }
func (self *boundedDeviceRpcTestWs) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}
func (self *boundedDeviceRpcTestWs) NextReader() (int, io.Reader, error) {
	var reader io.Reader
	self.nextOnce.Do(func() { reader = self.reader })
	if reader != nil {
		return DeviceRpcWsBinary, reader, nil
	}
	<-self.closed
	return 0, nil, io.EOF
}
func (self *boundedDeviceRpcTestWs) SetReadLimit(limit int64) {
	self.mu.Lock()
	self.readLimit = limit
	self.mu.Unlock()
}
func (self *boundedDeviceRpcTestWs) getReadLimit() int64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.readLimit
}
func (self *boundedDeviceRpcTestWs) SetReadDeadline(time.Time) error   { return nil }
func (self *boundedDeviceRpcTestWs) SetWriteDeadline(time.Time) error  { return nil }
func (self *boundedDeviceRpcTestWs) SetPongHandler(func(string) error) {}
func (self *boundedDeviceRpcTestWs) LocalAddr() net.Addr               { return &net.TCPAddr{} }
func (self *boundedDeviceRpcTestWs) RemoteAddr() net.Addr              { return &net.TCPAddr{} }

var _ deviceRpcWs = (*boundedDeviceRpcTestWs)(nil)

// scriptedDeviceRpcTestWs supplies complete websocket messages in a fixed
// order, then waits for the mux to close it.
type scriptedDeviceRpcTestWs struct {
	readers   chan io.Reader
	closeOnce sync.Once
	closed    chan struct{}
}

func newScriptedDeviceRpcTestWs(readers []io.Reader) *scriptedDeviceRpcTestWs {
	self := &scriptedDeviceRpcTestWs{
		readers: make(chan io.Reader, len(readers)),
		closed:  make(chan struct{}),
	}
	for _, reader := range readers {
		self.readers <- reader
	}
	return self
}

func (self *scriptedDeviceRpcTestWs) WriteMessage(int, []byte) error { return nil }
func (self *scriptedDeviceRpcTestWs) WriteControl(int, []byte, time.Time) error {
	return nil
}
func (self *scriptedDeviceRpcTestWs) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}
func (self *scriptedDeviceRpcTestWs) NextReader() (int, io.Reader, error) {
	select {
	case reader := <-self.readers:
		return DeviceRpcWsBinary, reader, nil
	case <-self.closed:
		return 0, nil, io.EOF
	}
}
func (self *scriptedDeviceRpcTestWs) SetReadLimit(int64)                {}
func (self *scriptedDeviceRpcTestWs) SetReadDeadline(time.Time) error   { return nil }
func (self *scriptedDeviceRpcTestWs) SetWriteDeadline(time.Time) error  { return nil }
func (self *scriptedDeviceRpcTestWs) SetPongHandler(func(string) error) {}
func (self *scriptedDeviceRpcTestWs) LocalAddr() net.Addr               { return &net.TCPAddr{} }
func (self *scriptedDeviceRpcTestWs) RemoteAddr() net.Addr              { return &net.TCPAddr{} }

var _ deviceRpcWs = (*scriptedDeviceRpcTestWs)(nil)
