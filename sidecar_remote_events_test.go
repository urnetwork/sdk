//go:build !ios_extension && !js && !ios && !android

package sdk

// The remote backend's events long-poll (QD-821) against a daemon of the root over a real Unix socket: the daemon's
// own events method over its own ring, a status method answering what a test sets, and a source attached to the ring
// raising the network's sidecar's events as a Sidecar does. Each event kind reaches a listener in order and as the
// local backend's would; a reader the ring passed gets a gap and then the state a status read finds; a session that
// ends is dialed again after its backoff; and state events stand for the status poll while they flow. Every wait is
// on a channel, the daemon's waits on its own bound, and nothing sleeps.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// A source the daemon's ring is attached to: one network's sidecar, raising what a test gives it.
type testEventsSource struct {
	callbacks *sidecar.CallbackList[sidecar.SidecarEventFunction]
	networkId sidecar.Id
	sidecarId sidecar.Id
}

func newTestEventsSource() *testEventsSource {
	return &testEventsSource{
		callbacks: sidecar.NewCallbackList[sidecar.SidecarEventFunction](),
		networkId: sidecar.NewId(),
		sidecarId: sidecar.NewId(),
	}
}

func (self *testEventsSource) AddEventCallback(callback sidecar.SidecarEventFunction) func() {
	callbackId := self.callbacks.Add(callback)
	return func() {
		self.callbacks.Remove(callbackId)
	}
}

func (self *testEventsSource) NetworkId() *sidecar.Id {
	networkId := self.networkId
	return &networkId
}

func (self *testEventsSource) Identity() *sidecar.SidecarIdentity {
	return &sidecar.SidecarIdentity{NetworkId: self.networkId, SidecarId: self.sidecarId, Generation: 1}
}

// Raises event to every callback, as a Sidecar's own delivery does.
func (self *testEventsSource) raise(event sidecar.SidecarEvent) {
	for _, callback := range self.callbacks.Get() {
		callback(event)
	}
}

// A daemon of the root: its events method over its ring, which source is attached to, and a status method answering
// the state a test sets for source's network.
type testEventsDaemon struct {
	daemon   *sidecar.Daemon
	ran      chan error
	stopOnce sync.Once

	stateLock   sync.Mutex
	source      *testEventsSource
	state       string
	reason      string
	generation  int64
	statusCount int
}

// A daemon serving on socketPath, online at generation, with edit applied to its local settings, stopped at the
// test's end.
func startTestEventsDaemon(t *testing.T, socketPath string, source *testEventsSource, generation int64, edit func(local *sidecar.SidecarLocalSettings)) *testEventsDaemon {
	t.Helper()
	stateDirectory, err := os.MkdirTemp("", "ur")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDirectory) })
	local := sidecar.DefaultSidecarSettings().LocalSettings
	if edit != nil {
		edit(local)
	}
	daemon, err := sidecar.NewDaemon(context.Background(), &sidecar.DaemonSettings{
		Instance:       "default",
		StateDirectory: filepath.Join(stateDirectory, "state"),
		SocketPath:     socketPath,
		Local:          local,
		Version:        testRemoteDaemonVersion,
		Log:            sidecar.NewNoopLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	self := &testEventsDaemon{daemon: daemon, ran: make(chan error, 1), source: source, state: "online", generation: generation}
	if err := daemon.RegisterMethod(&sidecar.DaemonMethod{Name: sidecarControlMethodStatus, Handler: func(ctx context.Context, call *sidecar.DaemonCall) (any, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.statusCount += 1
		return map[string]any{
			"instance":   "default",
			"state":      self.state,
			"reason":     self.reason,
			"generation": self.generation,
			"key_store":  "keychain",
			"networks": []map[string]any{{
				"network_id": self.source.networkId,
				"sidecar_id": self.source.sidecarId,
				"state":      self.state,
				"reason":     self.reason,
				"generation": self.generation,
			}},
		}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.RegisterMethod(sidecar.DaemonEventsMethod(daemon.Events())); err != nil {
		t.Fatal(err)
	}
	daemon.Events().Attach(source)
	go func() {
		self.ran <- daemon.Run()
	}()
	t.Cleanup(self.stop)
	return self
}

// Ends the daemon and waits for its run. Safe to call again.
func (self *testEventsDaemon) stop() {
	self.stopOnce.Do(func() {
		self.daemon.Close()
		<-self.ran
	})
}

// Sets what status answers, raising no event.
func (self *testEventsDaemon) set(state string, reason string, generation int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.state, self.reason, self.generation = state, reason, generation
}

// How many status reads it answered.
func (self *testEventsDaemon) statusReads() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.statusCount
}

// A dial whose sessions open once ready closes, so a listener is added before the first event, and whose next events
// call past a cursor waits for the test once it holds one.
type testEventsDial struct {
	base  sidecarControlDial
	ready chan struct{}

	stateLock sync.Mutex
	// closed by the call it holds when it does, and by the test to let it go
	held    chan struct{}
	release chan struct{}
	// which call's after it holds, nil for any
	match func(after string) bool
}

func newTestEventsDial(base sidecarControlDial) *testEventsDial {
	return &testEventsDial{base: base, ready: make(chan struct{})}
}

func (self *testEventsDial) dial(ctx context.Context) (sidecarControlChannel, error) {
	select {
	case <-self.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	channel, err := self.base(ctx)
	if err != nil {
		return nil, err
	}
	return &testEventsChannel{channel: channel, dial: self}, nil
}

// Holds the next events call past a cursor: held closes once it waits, and it goes on once release closes.
func (self *testEventsDial) holdNext() (held chan struct{}, release chan struct{}) {
	return self.holdNextMatching(nil)
}

// Holds the next events call past a cursor match takes, as holdNext does.
func (self *testEventsDial) holdNextMatching(match func(after string) bool) (held chan struct{}, release chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held, self.release, self.match = make(chan struct{}), make(chan struct{}), match
	return self.held, self.release
}

// The hold an events call past after takes, once, or nils.
func (self *testEventsDial) takeHold(after string) (chan struct{}, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.held == nil || (self.match != nil && !self.match(after)) {
		return nil, nil
	}
	held, release := self.held, self.release
	self.held, self.release, self.match = nil, nil, nil
	return held, release
}

// A session through testEventsDial.
type testEventsChannel struct {
	channel sidecarControlChannel
	dial    *testEventsDial
}

func (self *testEventsChannel) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	if events, ok := params.(*sidecarControlEventsParams); ok && method == sidecarControlMethodEvents && events.After != "" {
		if held, release := self.dial.takeHold(events.After); held != nil {
			close(held)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return self.channel.call(ctx, method, params, confirmed, result)
}

func (self *testEventsChannel) close() {
	self.channel.close()
}

// A remote Sidecar on dial whose status reads are its events long-poll's alone, with its listener, closed at the
// test's end with the long-poll joined. edit changes its settings.
func openTestEventsSidecar(t *testing.T, dial sidecarControlDial, edit func(settings *sidecarRemoteSettings)) (*Sidecar, *sidecarRemoteBackend, *testRemoteEvents) {
	t.Helper()
	remoteSettings := defaultSidecarRemoteSettings()
	remoteSettings.CallTimeout = testSidecarTimeout
	remoteSettings.EventReconnectMinTimeout = time.Millisecond
	remoteSettings.EventReconnectMaxTimeout = 4 * time.Millisecond
	if edit != nil {
		edit(remoteSettings)
	}
	remote, err := newSidecarRemote(context.Background(), nil, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:           dial,
		remoteSettings: remoteSettings,
		noStatusPoll:   true,
		followEvents:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := remote.remote.(*sidecarRemoteBackend)
	t.Cleanup(func() {
		testingCloseAndJoin(t, "remote", remote)
		select {
		case <-backend.eventsDone:
		case <-time.After(testSidecarTimeout):
			t.Error("the events long-poll outlived its Sidecar")
		}
	})
	events := &testRemoteEvents{events: make(chan *SidecarEvent, 256)}
	remote.AddEventListener(events)
	return remote, backend, events
}

// Waits for done to close, failing as what once testSidecarTimeout passes.
func awaitTestEventsChannel(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testSidecarTimeout):
		t.Fatalf("waited for %s", what)
	}
}

// A core event of kind at generation with every field set to something of its own, its times whole milliseconds; a
// state event names online.
func testFullCoreEvent(t *testing.T, kind sidecar.SidecarEventKind, generation int64) sidecar.SidecarEvent {
	t.Helper()
	event := sidecar.SidecarEvent{}
	value := reflect.ValueOf(&event).Elem()
	at := time.Date(2026, time.November, 14, 15, 16, 17, 0, time.UTC)
	for i := 0; i < value.NumField(); i += 1 {
		field := value.Field(i)
		name := value.Type().Field(i).Name
		switch field.Kind() {
		case reflect.String:
			field.SetString("synthetic-" + strings.ToLower(name))
		case reflect.Int, reflect.Int64:
			field.SetInt(int64(100 + i))
		case reflect.Uint64:
			field.SetUint(uint64(200 + i))
		case reflect.Uint32:
			// a set of bits, such as AuthFeatures, each value one the event names
			field.SetUint(uint64(1 + i%8))
		case reflect.Pointer:
			switch field.Type() {
			case reflect.TypeFor[*sidecar.Id]():
				id := sidecar.NewId()
				field.Set(reflect.ValueOf(&id))
			case reflect.TypeFor[*sidecar.SidecarErasure]():
				field.Set(reflect.ValueOf(&sidecar.SidecarErasure{
					ErasureId:   sidecar.NewId(),
					State:       "waiting",
					ObjectsLeft: 7,
					BytesLeft:   8192,
					NextTry:     at.Add(time.Minute),
					Finish:      at.Add(time.Hour),
				}))
			default:
				t.Fatalf("SidecarEvent.%s's type %s has no synthetic value", name, field.Type())
			}
		case reflect.Struct:
			field.Set(reflect.ValueOf(at.Add(time.Duration(i) * time.Second)))
		case reflect.Array:
			field.Set(reflect.ValueOf([2][]byte{{1, 2, byte(i)}, {4, 5, byte(i)}}))
		default:
			t.Fatalf("SidecarEvent.%s's kind %s has no synthetic value", name, field.Kind())
		}
	}
	event.Kind = kind
	event.Generation = generation
	if kind == sidecar.SidecarEventState {
		event.State, event.Reason = sidecar.SidecarStateOnline, ""
	}
	return event
}

// A ring event with every field set, for the wire's shape.
func testFullControlEvent() *sidecarControlEvent {
	event := &sidecarControlEvent{}
	value := reflect.ValueOf(event).Elem()
	for i := 0; i < value.NumField(); i += 1 {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString("synthetic-" + value.Type().Field(i).Name)
		case reflect.Int, reflect.Int64:
			field.SetInt(int64(100 + i))
		case reflect.Uint64:
			field.SetUint(uint64(200 + i))
		case reflect.Slice:
			field.SetBytes([]byte{byte(i), 1})
		case reflect.Pointer:
			if field.Type() == reflect.TypeFor[*Id]() {
				field.Set(reflect.ValueOf(NewId()))
			} else {
				field.Set(reflect.ValueOf(&sidecarControlEventErasure{
					ErasureId: NewId(), State: "done", ObjectsLeft: 1, BytesLeft: 2, NextTryMs: 3, FinishMs: 4,
				}))
			}
		}
	}
	return event
}

// The JSON of a bound event, for comparing two.
func testBoundEventJson(t *testing.T, event *SidecarEvent) string {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Every event kind the daemon's core raises reaches a remote-mode listener, in the order raised, as the bound event
// the local backend's listener would get for it; and the js leg's conversion of the daemon's own answer is the same.
func TestRemoteEventsReachTheListenerInOrder(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	source := newTestEventsSource()
	startTestEventsDaemon(t, socketPath, source, 1, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	remote, _, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	// subscribed: the status read after the cursor finds the daemon online
	if first := events.next(t); first.Kind != "state" || first.State != "online" || first.Generation != 1 {
		t.Fatalf("the first event: %+v", first)
	}
	// a cursor of the test's own, to read the daemon's answer as the js leg does
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	client, err := sidecar.DialDaemon(ctx, socketPath, &sidecar.DaemonHello{Mode: sidecar.DaemonModeControl, Version: testRemoteDaemonVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{}, false, start); err != nil {
		t.Fatal(err)
	}

	// every kind the core declares, read from its source, so a kind another track adds is raised here the day it
	// lands (a hand list here once lacked mail_receiver_alarm)
	raised := []sidecar.SidecarEvent{}
	for _, kind := range testSidecarEventKinds(t) {
		event := testFullCoreEvent(t, sidecar.SidecarEventKind(kind), 1)
		raised = append(raised, event)
		source.raise(event)
	}
	for i := range raised {
		got := events.next(t)
		if want := newSidecarEventOf(&raised[i]); testBoundEventJson(t, got) != testBoundEventJson(t, want) {
			t.Errorf("event %d (%s):\n%s\nwant\n%s", i, raised[i].Kind, testBoundEventJson(t, got), testBoundEventJson(t, want))
		}
	}
	if state := remote.GetState(); state != "online" {
		t.Errorf("the state after the events: %s", state)
	}

	answered := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{After: start.Cursor}, false, answered); err != nil {
		t.Fatal(err)
	}
	if len(answered.Events) != len(raised) {
		t.Fatalf("the daemon answered %d events of %d", len(answered.Events), len(raised))
	}
	for i, event := range answered.Events {
		if testBoundEventJson(t, event.boundEvent()) != testBoundEventJson(t, newSidecarEventOf(&raised[i])) {
			t.Errorf("the js leg's event %d (%s) differs", i, raised[i].Kind)
		}
		if event.NetworkId == nil || event.NetworkId.String() != source.networkId.String() ||
			event.OriginSidecarId == nil || event.OriginSidecarId.String() != source.sidecarId.String() {
			t.Errorf("event %d's source: %v %v", i, event.NetworkId, event.OriginSidecarId)
		}
	}
}

// A reader the ring passed while it was held gets gap from the daemon, reads status and emits the state it finds,
// none of the events it lost reaching the listener; it goes on from the gap's cursor.
func TestRemoteEventsGapReadsTheState(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	source := newTestEventsSource()
	daemon := startTestEventsDaemon(t, socketPath, source, 1, func(local *sidecar.SidecarLocalSettings) {
		local.EventRingEventCount = 2
	})
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	held, release := dial.holdNext()
	remote, _, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	if first := events.next(t); first.State != "online" || first.Generation != 1 {
		t.Fatalf("the first event: %+v", first)
	}
	awaitTestEventsChannel(t, held, "the long-poll's first call past the cursor")
	readsBefore := daemon.statusReads()
	// three events pass a ring of two while the reader is held, and the state moves with no event the reader keeps
	for range 3 {
		source.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventApprovalPending, Generation: 1, ApprovalId: func() *sidecar.Id {
			id := sidecar.NewId()
			return &id
		}()})
	}
	daemon.set("locked", "", 1)
	close(release)
	if gap := events.next(t); gap.Kind != "state" || gap.State != "locked" || gap.Generation != 1 {
		t.Fatalf("after the gap: %+v", gap)
	}
	if remote.GetState() != "locked" || daemon.statusReads() != readsBefore+1 {
		t.Fatalf("the state after the gap: %s, %d status reads after the hold", remote.GetState(), daemon.statusReads()-readsBefore)
	}
	// the next event is the first past the gap's cursor
	mailSend := sidecar.NewId()
	source.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventMailSend, Generation: 1, SendId: &mailSend})
	if next := events.next(t); next.Kind != "mail_send" || next.SendId == nil || next.SendId.String() != mailSend.String() {
		t.Fatalf("past the gap: %+v", next)
	}
}

// A session that ends reads status at once, offline with no daemon answering, and the long-poll dials again after its
// backoff: a daemon that started since has another ring, so its first answer is a gap and the state it reads follows.
func TestRemoteEventsReconnectAfterTheSessionEnds(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	first := newTestEventsSource()
	ended := startTestEventsDaemon(t, socketPath, first, 1, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	remote, _, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	if online := events.next(t); online.State != "online" {
		t.Fatalf("the first event: %+v", online)
	}
	// the long-poll's first call past a cursor of another ring than this daemon's is held, so the test raises the new
	// daemon's event once the reader follows its ring
	endedRing := testEventsRingOf(t, socketPath)
	held, release := dial.holdNextMatching(func(after string) bool { return !strings.HasPrefix(after, endedRing) })
	ended.stop()
	if offline := events.next(t); offline.State != "offline" {
		t.Fatalf("after the daemon ended: %+v", offline)
	}

	// the same network's sidecar under a daemon that started again
	second := &testEventsSource{callbacks: sidecar.NewCallbackList[sidecar.SidecarEventFunction](), networkId: first.networkId, sidecarId: first.sidecarId}
	startTestEventsDaemon(t, socketPath, second, 2, nil)
	awaitTestEventsChannel(t, held, "the long-poll past the new daemon's cursor")
	if online := events.next(t); online.State != "online" || online.Generation != 2 {
		t.Fatalf("after the daemon started again: %+v", online)
	}
	if remote.GetState() != "online" {
		t.Fatalf("the state: %s", remote.GetState())
	}
	approvalId := sidecar.NewId()
	second.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventApprovalPending, Generation: 2, ApprovalId: &approvalId})
	close(release)
	// the read at the session's end may have found online before the gap's read emitted it again
	pending := events.next(t)
	for pending.Kind == "state" && pending.State == "online" && pending.Generation == 2 {
		pending = events.next(t)
	}
	if pending.Kind != "approval_pending" || pending.ApprovalId == nil || pending.ApprovalId.String() != approvalId.String() {
		t.Fatalf("an event of the new daemon: %+v", pending)
	}
}

// The ring id the daemon at socketPath names in its cursors, with its separator, from a session of the test's own.
func testEventsRingOf(t *testing.T, socketPath string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	client, err := sidecar.DialDaemon(ctx, socketPath, &sidecar.DaemonHello{Mode: sidecar.DaemonModeControl, Version: testRemoteDaemonVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reply := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{}, false, reply); err != nil {
		t.Fatal(err)
	}
	ring, _, ok := strings.Cut(reply.Cursor, ":")
	if !ok {
		t.Fatalf("a cursor %q", reply.Cursor)
	}
	return ring + ":"
}

// While the long-poll answers, its state events move the state and a status read by anything else leaves it, and the
// status poll's turn reads nothing; an older generation's event and another network's go nowhere.
func TestRemoteStateEventsStandForThePoll(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	source := newTestEventsSource()
	daemon := startTestEventsDaemon(t, socketPath, source, 2, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	remote, backend, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	if online := events.next(t); online.State != "online" || online.Generation != 2 {
		t.Fatalf("the first event: %+v", online)
	}
	if !backend.eventsCarry() {
		t.Fatal("the long-poll answered and does not carry the state")
	}
	reads := daemon.statusReads()
	backend.pollStatus(context.Background())
	if daemon.statusReads() != reads {
		t.Fatal("the status poll read while the long-poll carries the state")
	}
	// a read that crossed a state event changes nothing
	daemon.set("locked", "", 2)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.GetState() != "online" {
		t.Fatalf("a status read moved the state past the events: %s", remote.GetState())
	}
	// the state's own event moves it, and a listener sees it
	source.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: 2, State: sidecar.SidecarStateLocked})
	if locked := events.next(t); locked.State != "locked" || remote.GetState() != "locked" {
		t.Fatalf("the locked event: %+v, state %s", locked, remote.GetState())
	}
	// an older generation's event, another network's, and a revocation without its verified reason
	other := newTestEventsSource()
	daemon.daemon.Events().Attach(other)
	other.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventAdmissionAlarm, Generation: 9})
	source.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventApprovalPending, Generation: 1})
	source.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: 3, State: sidecar.SidecarStateRevoked, Reason: sidecar.SidecarReasonRefused})
	if refused := events.next(t); refused.Kind != "state" || refused.State != "unenrolled" || refused.Reason != "refused" || refused.Generation != 3 {
		t.Fatalf("the event after the dropped ones: %+v", refused)
	}
	if state, reason := remote.GetState(), remote.GetStateReason(); state != "unenrolled" || reason != "refused" {
		t.Fatalf("a revocation without its reason: %s %s", state, reason)
	}
	if got := remote.GetSidecarId(); got == nil || got.String() != source.sidecarId.String() {
		t.Fatalf("the sidecar_id: %v", got)
	}
}

// What an events reply carries beyond the fields this sdk names goes nowhere: a broken daemon's extra fields reach no
// listener on either leg's conversion.
func TestRemoteEventsReadOnlyTheirFields(t *testing.T) {
	const leaked = "a-token-no-reply-may-carry"
	networkId := NewId()
	raw := `{"events":[{"kind":"approval_pending","generation":1,"network_id":"` + networkId.String() + `","by_client_jwt":"` +
		leaked + `","pass":"` + leaked + `","erasure":{"erasure_id":"` + NewId().String() + `","state":"done","recovery_code":"` +
		leaked + `"}}],"cursor":"c","auth_key":"` + leaked + `"}`
	reply := &sidecarControlEventsReply{}
	if err := json.Unmarshal([]byte(raw), reply); err != nil || len(reply.Events) != 1 {
		t.Fatalf("%+v %v", reply, err)
	}
	core := sidecarCoreEventOf(reply.Events[0])
	for _, bound := range []*SidecarEvent{reply.Events[0].boundEvent(), newSidecarEventOf(&core)} {
		if encoded := testBoundEventJson(t, bound); strings.Contains(encoded, leaked) || !strings.Contains(encoded, "approval_pending") {
			t.Errorf("a listener's event: %s", encoded)
		}
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(leaked)) {
		t.Errorf("the reply as read keeps %s", encoded)
	}
}

// The long-poll ends with its Sidecar, its session closed, whatever it waited on: a call in flight, or a backoff.
func TestRemoteEventsEndWithTheSidecar(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	source := newTestEventsSource()
	startTestEventsDaemon(t, socketPath, source, 1, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	held, _ := dial.holdNext()
	remote, backend, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	if online := events.next(t); online.State != "online" {
		t.Fatalf("the first event: %+v", online)
	}
	awaitTestEventsChannel(t, held, "the long-poll's call past the cursor")
	remote.Close()
	awaitTestEventsChannel(t, backend.eventsDone, "the long-poll's end")
	if backend.eventsCarry() {
		t.Error("a closed backend's events carry the state")
	}

	// no daemon at all: the long-poll backs off and dials again until its Sidecar closes
	none := newTestEventsDial(sidecarDaemonSocketDial(filepath.Join(filepath.Dir(socketPath), "none.sock"), testRemoteDaemonVersion))
	close(none.ready)
	idle, idleBackend, idleEvents := openTestEventsSidecar(t, none.dial, nil)
	if state := idle.GetState(); state != "offline" {
		t.Fatalf("with no daemon: %s", state)
	}
	idle.Close()
	awaitTestEventsChannel(t, idleBackend.eventsDone, "the long-poll's end with no daemon")
	select {
	case event := <-idleEvents.events:
		if event.State != "offline" {
			t.Errorf("with no daemon a listener heard %+v", event)
		}
	default:
	}
	if err := idleBackend.refresh(context.Background()); !errors.Is(err, errSidecarRemoteClosed) {
		t.Errorf("a read after Close: %v", err)
	}
}

// A sidecar of the network the daemon enrolled anew, after a logout, starts its own generations at its first, below
// the one before it: its events still reach the listener, and the backend follows its sidecar_id from them on.
func TestRemoteEventsFollowANewSidecarOfTheNetwork(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	before := newTestEventsSource()
	daemon := startTestEventsDaemon(t, socketPath, before, 5, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	remote, _, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	if online := events.next(t); online.State != "online" || online.Generation != 5 {
		t.Fatalf("the first event: %+v", online)
	}
	after := &testEventsSource{callbacks: sidecar.NewCallbackList[sidecar.SidecarEventFunction](), networkId: before.networkId, sidecarId: sidecar.NewId()}
	daemon.daemon.Events().Attach(after)
	after.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: 1, State: sidecar.SidecarStateOnline})
	if fresh := events.next(t); fresh.State != "online" || fresh.Generation != 1 {
		t.Fatalf("the new sidecar's first event: %+v", fresh)
	}
	if got := remote.GetSidecarId(); got == nil || got.String() != after.sidecarId.String() {
		t.Fatalf("the sidecar_id followed: %v", got)
	}
	approvalId := sidecar.NewId()
	after.raise(sidecar.SidecarEvent{Kind: sidecar.SidecarEventApprovalPending, Generation: 1, ApprovalId: &approvalId})
	if pending := events.next(t); pending.Kind != "approval_pending" || pending.Generation != 1 {
		t.Fatalf("the new sidecar's next event: %+v", pending)
	}
}

// A pairing that moves the backend to another network's sidecar, whose generations start below the one followed
// before, still delivers that sidecar's state.
func TestRemotePairingToAnotherNetworkPassesTheFloor(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 5)
	daemon.start()
	remote, backend, events := openTestRemoteSidecar(t, socketPath)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if online := events.next(t); online.Generation != 5 {
		t.Fatalf("the first event: %+v", online)
	}
	// the daemon's sidecar of another network, the one the pairing binds, at its first generations
	other := sidecar.NewId()
	func() {
		daemon.stateLock.Lock()
		defer daemon.stateLock.Unlock()
		daemon.networkId, daemon.sidecarId, daemon.generation = sidecar.NewId(), other, 0
	}()
	if reply, err := backend.pair(context.Background(), "a-synthetic-child-token"); err != nil || reply.Generation != 1 {
		t.Fatalf("the pairing: %+v %v", reply, err)
	}
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if paired := events.next(t); paired.State != "online" || paired.Generation != 1 {
		t.Fatalf("the paired network's state: %+v", paired)
	}
	if got := remote.GetSidecarId(); got == nil || got.String() != other.String() {
		t.Fatalf("the sidecar_id followed: %v", got)
	}
}

// Pair on the control socket, then observe logout either in the event stream or by rereading status after a ring gap.
func TestPairedRemoteLogoutClearsTheIdentity(t *testing.T) {
	for _, path := range []string{"live event", "reconnect with gap"} {
		t.Run(path, func(t *testing.T) {
			socketPath := testRemoteSocketPath(t)
			daemon := newTestRemoteDaemon(t, socketPath)
			daemon.set("online", "", 3)
			daemon.start()
			remote, backend, events := openTestRemoteSidecar(t, socketPath)
			if _, err := backend.pair(t.Context(), "synthetic-child-token"); err != nil {
				t.Fatal(err)
			}
			if err := backend.refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			paired := events.next(t)
			if paired.State != "online" || paired.Generation != 4 || remote.GetSidecarId() == nil {
				t.Fatalf("paired: %+v", paired)
			}
			networkId, origin := newId([16]byte(daemon.networkId)), newId([16]byte(daemon.sidecarId))
			backend.applyEvents([]*sidecarControlEvent{{Kind: sidecarControlEventKindState, State: "unenrolled",
				Reason: "refused", Generation: 4, NetworkId: networkId, OriginSidecarId: origin}})
			if refused := events.next(t); refused.Reason != "refused" || !sidecarControlSameId(remote.GetSidecarId(), origin) {
				t.Fatal("a refused generation lost its enrolled identity before logout")
			}
			oldRead := backend.revision()
			if path == "live event" {
				backend.applyEvents([]*sidecarControlEvent{{LoggedOut: true, Kind: sidecarControlEventKindState,
					State: "unenrolled", Generation: 4, NetworkId: networkId, OriginSidecarId: origin}})
			} else {
				daemon.stateLock.Lock()
				daemon.statusExtra = map[string]any{"networks": []any{}}
				daemon.stateLock.Unlock()
				if err := backend.refreshFromEvents(t.Context(), true); err != nil {
					t.Fatal(err)
				}
			}
			if loggedOut := events.next(t); loggedOut.State != "unenrolled" || loggedOut.Generation != 0 {
				t.Fatalf("logout: %+v", loggedOut)
			}
			backend.applyStatusAt(&sidecarControlStatus{Networks: []sidecarControlNetworkStatus{{NetworkId: networkId,
				SidecarId: origin, State: "online", Generation: 4}}}, false, false, oldRead)
			daemon.stateLock.Lock()
			daemon.statusExtra = nil
			daemon.stateLock.Unlock()
			if err := backend.refreshFromEvents(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			// The stock daemon can still list the network after clearing its identity and before detaching it.
			replayAnonymousStatus := func(generation int64, reason string) {
				t.Helper()
				daemon.stateLock.Lock()
				daemon.statusExtra = map[string]any{"networks": []map[string]any{{
					"network_id": daemon.networkId, "state": "unenrolled", "reason": reason, "generation": generation,
				}}}
				daemon.stateLock.Unlock()
				if err := backend.refreshFromEvents(t.Context(), true); err != nil {
					t.Fatal(err)
				}
			}
			replayAnonymousStatus(4, "refused")
			backend.applyEvents([]*sidecarControlEvent{{Kind: sidecarControlEventKindState, State: "online",
				Generation: 4, NetworkId: networkId, OriginSidecarId: origin}})
			state, reason := remote.GetState(), remote.GetStateReason()
			if state != "unenrolled" || reason != "" || remote.GetSidecarId() != nil || backend.generation() != 0 {
				t.Fatalf("state after the removed enrollment's status: %s(%s) at %d", state, reason, backend.generation())
			}
			replayAnonymousStatus(0, "")
			if empty := events.next(t); empty.State != "unenrolled" || empty.Reason != "" || empty.Generation != 0 {
				t.Fatalf("the unenrolled status past the gap: %+v", empty)
			}
			freshId := NewId()
			backend.applyEvents([]*sidecarControlEvent{{Kind: sidecarControlEventKindState, State: "online",
				Generation: 1, NetworkId: networkId, OriginSidecarId: freshId}})
			if next := events.next(t); next.Generation != 1 || next.State != "online" || !sidecarControlSameId(remote.GetSidecarId(), freshId) {
				t.Fatalf("the new enrollment: %+v", next)
			}
		})
	}
}

// A daemon that could not start its sidecars answers status with no_sidecar and omits its network list.
func TestPairedRemoteRecoversTheSameIdentityAfterACoreOutage(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 3)
	daemon.start()
	remote, backend, events := openTestRemoteSidecar(t, socketPath)
	if _, err := backend.pair(t.Context(), "synthetic-child-token"); err != nil {
		t.Fatal(err)
	}
	if err := backend.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	origin := remote.GetSidecarId()
	if paired := events.next(t); paired.State != "online" || paired.Generation != 4 || origin == nil {
		t.Fatalf("the original pairing: %+v, sidecar_id %v", paired, origin)
	}

	daemon.stateLock.Lock()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodStatus:
			return map[string]any{"instance": "default", "key_store": "pass", "no_sidecar": "synthetic proxy configuration unavailable"}, nil, true
		case sidecarControlMethodSearchStatus:
			return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailedPrecondition, Message: "no hosted network"}, true
		}
		return nil, nil, false
	}
	daemon.stateLock.Unlock()
	backend.setEventsCarry(true)
	if err := backend.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if offline := events.next(t); offline.State != "offline" || offline.Reason != "" || offline.Generation != 4 ||
		remote.GetState() != "offline" || !sidecarControlSameId(remote.GetSidecarId(), origin) {
		t.Fatalf("the unavailable core: %+v, sidecar_id %v", offline, remote.GetSidecarId())
	}
	if search := remote.GetSearch(); search == nil || search.GetStatus(nil) != nil {
		t.Fatal("the daemon's refused search status did not reach the paired reader")
	}

	daemon.stop()
	if err := backend.refresh(t.Context()); !errors.Is(err, errSidecarRemoteOffline) {
		t.Fatalf("the ended control session: %v", err)
	}
	restarted := newTestRemoteDaemon(t, socketPath)
	restarted.networkId, restarted.sidecarId = daemon.networkId, daemon.sidecarId
	restarted.set("online", "", 4)
	restarted.start()
	if err := backend.refreshFromEvents(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if online := events.next(t); online.State != "online" || online.Generation != 4 || remote.GetState() != "online" ||
		!sidecarControlSameId(remote.GetSidecarId(), origin) {
		t.Fatalf("the recovered core: %+v, sidecar_id %v", online, remote.GetSidecarId())
	}
}
