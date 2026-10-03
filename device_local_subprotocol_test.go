package sdk

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Two connect clients wired over the in-process gateway transports with no
// contracts (the mux security test's pairing), so a device-side registry can
// be attached to one and exercised from the other.
type subprotocolTestPair struct {
	ctx    context.Context
	cancel context.CancelFunc
	a      *connect.Client
	b      *connect.Client
}

func newSubprotocolTestPair(t *testing.T) *subprotocolTestPair {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), subprotocolTestClientSettings())
	b := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), subprotocolTestClientSettings())
	wireSubprotocolTestClients(a, b)
	return &subprotocolTestPair{ctx: ctx, cancel: cancel, a: a, b: b}
}

// No end-to-end encryption (no peer certificates in this harness). The send
// buffers keep their defaults: a query answer is enqueued with a zero timeout
// (the provider ping echo's rule), which an unbuffered sequence refuses.
func subprotocolTestClientSettings() *connect.ClientSettings {
	s := connect.DefaultClientSettings()
	s.EncryptionSettings.Mode = connect.EncryptionModeOff
	return s
}

// Routes a's sends to b and b's sends to a (a client send transport bound to
// the peer on each side, the peer's gateway receive on the other end), no
// contracts either way.
func wireSubprotocolTestClients(a *connect.Client, b *connect.Client) {
	aToB := make(chan []byte, 8)
	bToA := make(chan []byte, 8)
	a.RouteManager().UpdateTransport(connect.NewSendClientTransport(connect.DestinationId(b.ClientId())), []connect.Route{aToB})
	a.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{bToA})
	b.RouteManager().UpdateTransport(connect.NewSendClientTransport(connect.DestinationId(a.ClientId())), []connect.Route{bToA})
	b.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{aToB})
	a.ContractManager().AddNoContractPeer(b.ClientId())
	b.ContractManager().AddNoContractPeer(a.ClientId())
}

func (self *subprotocolTestPair) close() {
	self.a.Cancel()
	self.b.Cancel()
	self.cancel()
}

// A listener that records every delivery.
type recordingSubprotocolListener struct {
	stateLock sync.Mutex
	events    []recordedSubprotocolMessage
	received  chan struct{}
}

type recordedSubprotocolMessage struct {
	subprotocolId int32
	sourceId      string
	messageBytes  []byte
	delivered     []byte
}

func newRecordingSubprotocolListener() *recordingSubprotocolListener {
	return &recordingSubprotocolListener{received: make(chan struct{}, 64)}
}

func (self *recordingSubprotocolListener) SubprotocolMessage(subprotocolId int32, sourceClientId *Id, messageBytes []byte) {
	self.stateLock.Lock()
	self.events = append(self.events, recordedSubprotocolMessage{
		subprotocolId: subprotocolId,
		sourceId:      sourceClientId.String(),
		messageBytes:  slices.Clone(messageBytes),
		delivered:     messageBytes,
	})
	self.stateLock.Unlock()
	self.received <- struct{}{}
}

func (self *recordingSubprotocolListener) wait(t *testing.T, timeout time.Duration) recordedSubprotocolMessage {
	t.Helper()
	select {
	case <-self.received:
	case <-time.After(timeout):
		t.Fatal("no subprotocol message delivered")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.events[len(self.events)-1]
}

func (self *recordingSubprotocolListener) last() recordedSubprotocolMessage {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.events[len(self.events)-1]
}

func (self *recordingSubprotocolListener) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.events)
}

func newTestDeviceLocalSubprotocols(ctx context.Context) *deviceLocalSubprotocols {
	return newDeviceLocalSubprotocols(ctx, connect.DefaultLogger())
}

func sendSubprotocolFrom(t *testing.T, from *connect.Client, to *connect.Client, subprotocolId int32, messageBytes []byte) {
	t.Helper()
	sent := from.SendSubprotocolBytes(
		connect.SubprotocolId(subprotocolId),
		slices.Clone(messageBytes),
		to.ClientId(),
		func(err error) {},
	)
	if !sent {
		t.Fatalf("send of subprotocol %d not enqueued", subprotocolId)
	}
}

func waitForSubprotocolStat(t *testing.T, client *connect.Client, read func(connect.ClientSubprotocolStatsSnapshot) uint64, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if read(client.SubprotocolStats()) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subprotocol stat did not reach %d: %+v", want, client.SubprotocolStats())
}

func TestDeviceLocalSubprotocolsEnableDisable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subprotocols := newTestDeviceLocalSubprotocols(ctx)
	listener := newRecordingSubprotocolListener()

	// refused ids: zero, reserved, out of range, and a nil listener
	for _, id := range []int32{0, 1, 1023, 65536, -5} {
		if _, err := subprotocols.enable(id, listener); err == nil {
			t.Errorf("id %d must be refused", id)
		}
	}
	if _, err := subprotocols.enable(4000, nil); err == nil {
		t.Error("a nil listener must be refused")
	}

	// no client attached yet: enabling still records the id
	sub1, err := subprotocols.enable(4000, listener)
	connect.AssertEqual(t, err, nil)
	sub2, err := subprotocols.enable(4000, newRecordingSubprotocolListener())
	connect.AssertEqual(t, err, nil)
	sub3, err := subprotocols.enable(5000, listener)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{4000, 5000})

	// the id stays enabled while one listener remains
	sub1.Close()
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{4000, 5000})
	sub2.Close()
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{5000})
	// closing twice is harmless
	sub2.Close()
	sub3.Close()
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{})

	// disable removes every listener of an id at once
	_, err = subprotocols.enable(4000, listener)
	connect.AssertEqual(t, err, nil)
	_, err = subprotocols.enable(4000, newRecordingSubprotocolListener())
	connect.AssertEqual(t, err, nil)
	subprotocols.disable(4000)
	subprotocols.disable(4000)
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{})
}

func TestDeviceLocalSubprotocolsDelivery(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()

	subprotocols := newTestDeviceLocalSubprotocols(pair.ctx)
	subprotocols.attach(pair.b)

	first := newRecordingSubprotocolListener()
	second := newRecordingSubprotocolListener()
	sub, err := subprotocols.enable(4000, first)
	connect.AssertEqual(t, err, nil)
	_, err = subprotocols.enable(4000, second)
	connect.AssertEqual(t, err, nil)

	message := []byte("hello subprotocol")
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)

	// both listeners see the id, the source and the bytes
	for _, listener := range []*recordingSubprotocolListener{first, second} {
		got := listener.wait(t, 5*time.Second)
		connect.AssertEqual(t, got.subprotocolId, int32(4000))
		connect.AssertEqual(t, got.sourceId, connect.Id(pair.a.ClientId()).String())
		connect.AssertEqual(t, got.messageBytes, message)
	}
	// the delivered slice is the listener's copy: scribbling on it after the
	// callback neither affects the pool nor the next delivery
	firstDelivered := first.last()
	for i := range firstDelivered.delivered {
		firstDelivered.delivered[i] = 0xff
	}
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)
	got := first.wait(t, 5*time.Second)
	connect.AssertEqual(t, got.messageBytes, message)
	second.wait(t, 5*time.Second)
	waitForSubprotocolStat(t, pair.b, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.Received }, 2)

	// an id with no listener is dropped and counted, nothing fires
	before := first.count() + second.count()
	sendSubprotocolFrom(t, pair.a, pair.b, 4001, message)
	waitForSubprotocolStat(t, pair.b, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.DroppedUnregistered }, 1)
	connect.AssertEqual(t, first.count()+second.count(), before)

	// removing one listener keeps the other; removing the last drops the id
	sub.Close()
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)
	got = second.wait(t, 5*time.Second)
	connect.AssertEqual(t, got.messageBytes, message)
	subprotocols.disable(4000)
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)
	waitForSubprotocolStat(t, pair.b, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.DroppedUnregistered }, 2)
	connect.AssertEqual(t, first.count(), 2)
	connect.AssertEqual(t, second.count(), 3)
}

func TestDeviceLocalSubprotocolsSendBytes(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()

	device := newTestDeviceLocalSubprotocols(pair.ctx)
	// nothing attached: the send is refused
	connect.AssertEqual(t, device.sendBytes(4000, newId([16]byte(pair.a.ClientId())), []byte("x")), false)
	device.attach(pair.b)

	received := make(chan []byte, 4)
	remove, err := pair.a.AddSubprotocolRawCallback(4000, func(source connect.TransferPath, id connect.SubprotocolId, messageBytes []byte, peer connect.Peer) {
		received <- slices.Clone(messageBytes)
	})
	connect.AssertEqual(t, err, nil)
	defer remove()

	message := []byte("from the device")
	sent := device.sendBytes(4000, newId([16]byte(pair.a.ClientId())), message)
	connect.AssertEqual(t, sent, true)
	// the caller keeps its slice: reusing it after the send cannot change
	// what was sent
	message[0] = 'X'
	select {
	case got := <-received:
		connect.AssertEqual(t, got, []byte("from the device"))
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not receive the device's message")
	}

	// refused sends: the control id, a nil destination, invalid ids
	connect.AssertEqual(t, device.sendBytes(4000, newId([16]byte(connect.ControlId)), message), false)
	connect.AssertEqual(t, device.sendBytes(4000, nil, message), false)
	connect.AssertEqual(t, device.sendBytes(0, newId([16]byte(pair.a.ClientId())), message), false)
	connect.AssertEqual(t, device.sendBytes(70000, newId([16]byte(pair.a.ClientId())), message), false)
	// a reserved id may be sent (the network's own subprotocols), so 1000 is
	// not refused here; it is only refused for enabling
	connect.AssertEqual(t, device.stats().Sent, int64(1))
}

// The registrations belong to the device, not to a client: attaching a
// replacement client re-applies every enabled id there and drops them on the
// old client.
func TestDeviceLocalSubprotocolsReattach(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()

	subprotocols := newTestDeviceLocalSubprotocols(pair.ctx)
	subprotocols.attach(pair.b)
	listener := newRecordingSubprotocolListener()
	_, err := subprotocols.enable(4000, listener)
	connect.AssertEqual(t, err, nil)
	_, err = subprotocols.enable(4100, listener)
	connect.AssertEqual(t, err, nil)

	message := []byte("before")
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)
	connect.AssertEqual(t, listener.wait(t, 5*time.Second).messageBytes, message)

	// a replacement client, wired to a like the first
	replacement := connect.NewClient(pair.ctx, connect.NewId(), connect.NewNoContractClientOob(), subprotocolTestClientSettings())
	defer replacement.Cancel()
	wireSubprotocolTestClients(pair.a, replacement)
	subprotocols.attach(replacement)

	// the replacement delivers every enabled id
	sendSubprotocolFrom(t, pair.a, replacement, 4100, []byte("after"))
	got := listener.wait(t, 5*time.Second)
	connect.AssertEqual(t, got.subprotocolId, int32(4100))
	connect.AssertEqual(t, got.messageBytes, []byte("after"))

	// the old client no longer has the registrations
	sendSubprotocolFrom(t, pair.a, pair.b, 4000, message)
	waitForSubprotocolStat(t, pair.b, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.DroppedUnregistered }, 1)
	connect.AssertEqual(t, listener.count(), 2)

	// an id enabled after the swap lands on the replacement only
	_, err = subprotocols.enable(4200, listener)
	connect.AssertEqual(t, err, nil)
	sendSubprotocolFrom(t, pair.a, replacement, 4200, []byte("late"))
	connect.AssertEqual(t, listener.wait(t, 5*time.Second).subprotocolId, int32(4200))

	// detaching leaves the ids enabled but delivers nothing
	subprotocols.attach(nil)
	connect.AssertEqual(t, subprotocols.enabledIds(), []int32{4000, 4100, 4200})
	sendSubprotocolFrom(t, pair.a, replacement, 4000, message)
	waitForSubprotocolStat(t, replacement, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.DroppedUnregistered }, 1)
	connect.AssertEqual(t, listener.count(), 3)
}

type recordingSubprotocolsQueryCallback struct {
	results chan recordedSubprotocolsQueryResult
}

type recordedSubprotocolsQueryResult struct {
	ids []int
	ok  bool
}

func newRecordingSubprotocolsQueryCallback() *recordingSubprotocolsQueryCallback {
	return &recordingSubprotocolsQueryCallback{results: make(chan recordedSubprotocolsQueryResult, 4)}
}

func (self *recordingSubprotocolsQueryCallback) Result(subprotocolIds *IntList, ok bool) {
	var ids []int
	for i := 0; i < subprotocolIds.Len(); i++ {
		ids = append(ids, subprotocolIds.Get(i))
	}
	self.results <- recordedSubprotocolsQueryResult{ids: ids, ok: ok}
}

func (self *recordingSubprotocolsQueryCallback) wait(t *testing.T, timeout time.Duration) recordedSubprotocolsQueryResult {
	t.Helper()
	select {
	case result := <-self.results:
		return result
	case <-time.After(timeout):
		t.Fatal("no query result")
		return recordedSubprotocolsQueryResult{}
	}
}

func TestDeviceLocalSubprotocolsQuery(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()

	peer := newTestDeviceLocalSubprotocols(pair.ctx)
	peer.attach(pair.b)
	listener := newRecordingSubprotocolListener()
	for _, id := range []int32{4100, 4000, 4100} {
		_, err := peer.enable(id, listener)
		connect.AssertEqual(t, err, nil)
	}

	device := newTestDeviceLocalSubprotocols(pair.ctx)
	callback := newRecordingSubprotocolsQueryCallback()

	// no client: the callback answers ok=false without blocking
	device.query(newId([16]byte(pair.b.ClientId())), 1000, callback)
	connect.AssertEqual(t, callback.wait(t, time.Second).ok, false)

	device.attach(pair.a)
	// a nil destination answers ok=false
	device.query(nil, 1000, callback)
	connect.AssertEqual(t, callback.wait(t, time.Second).ok, false)

	// the peer answers with its enabled ids, sorted and deduplicated; the
	// caller returns before the answer
	started := time.Now()
	device.query(newId([16]byte(pair.b.ClientId())), 5000, callback)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("QuerySubprotocols blocked the caller")
	}
	result := callback.wait(t, 5*time.Second)
	connect.AssertEqual(t, result.ok, true)
	connect.AssertEqual(t, result.ids, []int{4000, 4100})
	waitForSubprotocolStat(t, pair.b, func(s connect.ClientSubprotocolStatsSnapshot) uint64 { return s.QueriesAnswered }, 1)

	// an unreachable peer times out within the given bound, ok=false
	started = time.Now()
	device.query(NewId(), 300, callback)
	result = callback.wait(t, 5*time.Second)
	connect.AssertEqual(t, result.ok, false)
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond || 3*time.Second < elapsed {
		t.Fatalf("timeout not honoured: %s", elapsed)
	}
}

// Enabling, sending and querying create no lingering goroutines once the
// clients are gone (the churn tests' baseline pattern).
func TestDeviceLocalSubprotocolsNoGoroutineLeak(t *testing.T) {
	const tolerance = 8
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		pair := newSubprotocolTestPair(t)
		subprotocols := newTestDeviceLocalSubprotocols(pair.ctx)
		subprotocols.attach(pair.b)
		listener := newRecordingSubprotocolListener()
		_, err := subprotocols.enable(4000, listener)
		connect.AssertEqual(t, err, nil)
		sendSubprotocolFrom(t, pair.a, pair.b, 4000, []byte("round"))
		listener.wait(t, 5*time.Second)
		callback := newRecordingSubprotocolsQueryCallback()
		device := newTestDeviceLocalSubprotocols(pair.ctx)
		device.attach(pair.a)
		device.query(newId([16]byte(pair.b.ClientId())), 5000, callback)
		callback.wait(t, 5*time.Second)
		pair.close()
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= base+tolerance {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: final=%d baseline=%d (tol +%d)", runtime.NumGoroutine(), base, tolerance)
}

// The gomobile surface on DeviceLocal, on a device built without a network.
func TestDeviceLocalSubprotocolSurface(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, networkSpace.GetApi().CloseAndWait(ctx), nil)

	deviceLocal, err := NewDeviceLocalWithDefaults(
		networkSpace,
		testingByJwt(connect.NewId()),
		"",
		"",
		"",
		NewId(),
		false,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	listener := newRecordingSubprotocolListener()
	if _, err := deviceLocal.EnableSubprotocol(100, listener); err == nil {
		t.Fatal("a reserved id must be refused on the device surface")
	}
	sub, err := deviceLocal.EnableSubprotocol(4000, listener)
	connect.AssertEqual(t, err, nil)
	enabled := deviceLocal.EnabledSubprotocols()
	connect.AssertEqual(t, enabled.Len(), 1)
	connect.AssertEqual(t, enabled.Get(0), 4000)

	// the enabled id is registered on the device's own client when there is one
	if client := deviceLocal.providerClientSnapshot(); client != nil {
		if deviceLocal.subprotocols.attachedClient() != client {
			t.Fatal("the enabled ids must be registered on the device's own client")
		}
	}

	// the control id is never a subprotocol destination
	connect.AssertEqual(t, deviceLocal.SendSubprotocolBytes(4000, newId([16]byte(connect.ControlId)), []byte("x")), false)
	stats := deviceLocal.SubprotocolStats()
	connect.AssertEqual(t, stats.Sent, int64(0))

	callback := newRecordingSubprotocolsQueryCallback()
	deviceLocal.QuerySubprotocols(nil, 100, callback)
	connect.AssertEqual(t, callback.wait(t, time.Second).ok, false)

	sub.Close()
	connect.AssertEqual(t, deviceLocal.EnabledSubprotocols().Len(), 0)
	deviceLocal.DisableSubprotocol(4000)
}
