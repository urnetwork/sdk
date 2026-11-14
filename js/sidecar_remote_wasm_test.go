//go:build js

// sdk.Sidecar on js (sidecar §18.5's js row; SDK-M8-02): NewSidecarRemote over a fake native-host port, which answers
// as `ur native-host` does: status relayed, every other method refused, a long reply in chunks. Each test waits on an
// event or a callback, never a clock.
package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"syscall/js"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

const (
	testSidecarPortNetworkId = "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e5f"
	testSidecarPortSidecarId = "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e60"
)

// A fake port to ur native-host, shaped as chrome.runtime.Port.
type testSidecarPort struct {
	port            js.Value
	messageListener js.Value
	goneListener    js.Value
	funcs           []js.Func
	// what the host posted, each as JSON
	posted []string
	// the daemon's sidecar, as status answers it
	state      string
	reason     string
	generation int64
	// a reply longer than this goes as chunks; 0 sends every reply whole
	chunkByteCount int
	disconnected   int
}

func newTestSidecarPort(t *testing.T) *testSidecarPort {
	t.Helper()
	self := &testSidecarPort{port: js.Global().Get("Object").New(), state: "online", generation: 3}
	listenerEvent := func(set func(listener js.Value)) js.Value {
		event := js.Global().Get("Object").New()
		addListener := js.FuncOf(func(this js.Value, args []js.Value) any {
			set(args[0])
			return nil
		})
		self.funcs = append(self.funcs, addListener)
		event.Set("addListener", addListener)
		return event
	}
	self.port.Set("onMessage", listenerEvent(func(listener js.Value) { self.messageListener = listener }))
	self.port.Set("onDisconnect", listenerEvent(func(listener js.Value) { self.goneListener = listener }))
	postMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		posted := js.Global().Get("JSON").Call("stringify", args[0]).String()
		self.posted = append(self.posted, posted)
		// the native host answers later, never inside postMessage
		go self.reply(posted)
		return nil
	})
	disconnect := js.FuncOf(func(this js.Value, args []js.Value) any {
		self.disconnected += 1
		return nil
	})
	self.funcs = append(self.funcs, postMessage, disconnect)
	self.port.Set("postMessage", postMessage)
	self.port.Set("disconnect", disconnect)
	t.Cleanup(func() {
		for _, fn := range self.funcs {
			fn.Release()
		}
	})
	return self
}

func (self *testSidecarPort) deliver(message []byte) {
	self.messageListener.Invoke(js.Global().Get("JSON").Call("parse", string(message)))
}

// Answers one request as ur native-host does.
func (self *testSidecarPort) reply(posted string) {
	request := struct {
		Id     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}{}
	if err := json.Unmarshal([]byte(posted), &request); err != nil {
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": request.Id}
	if request.Method == "status" {
		response["result"] = map[string]any{
			"instance":   "default",
			"state":      self.state,
			"reason":     self.reason,
			"generation": self.generation,
			"key_store":  "keychain",
			"networks": []map[string]any{{
				"network_id": testSidecarPortNetworkId,
				"sidecar_id": testSidecarPortSidecarId,
				"state":      self.state,
				"reason":     self.reason,
				"generation": self.generation,
			}},
		}
	} else {
		response["error"] = map[string]any{"code": -32601, "message": "the native host does not relay this method"}
	}
	encoded, _ := json.Marshal(response)
	if self.chunkByteCount == 0 || len(encoded) <= self.chunkByteCount {
		self.deliver(encoded)
		return
	}
	count := (len(encoded) + self.chunkByteCount - 1) / self.chunkByteCount
	for index := 0; index < count; index += 1 {
		part := encoded[index*self.chunkByteCount : min(len(encoded), (index+1)*self.chunkByteCount)]
		chunk, _ := json.Marshal(map[string]any{
			"ur_chunk":    map[string]any{"id": request.Id, "index": index, "count": count},
			"data_base64": base64.StdEncoding.EncodeToString(part),
		})
		self.deliver(chunk)
	}
}

// Events in order, off a listener.
type testSidecarPortEvents struct {
	events chan *sdk.SidecarEvent
}

func (self *testSidecarPortEvents) SidecarEventReceived(event *sdk.SidecarEvent) {
	self.events <- event
}

// A Sidecar over port and its events, the listener in place before the first status is read.
func openTestSidecarOverPort(t *testing.T, port *testSidecarPort) (*sdk.Sidecar, *testSidecarPortEvents) {
	t.Helper()
	remote, err := sdk.NewSidecarRemote(port.port, nil, sdk.DefaultSidecarSettings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	events := &testSidecarPortEvents{events: make(chan *sdk.SidecarEvent, 16)}
	remote.AddEventListener(events)
	return remote, events
}

func testSidecarPortResult() (sdk.SidecarResultCallback, chan *sdk.SidecarResult) {
	results := make(chan *sdk.SidecarResult, 1)
	return connect.NewApiCallback[*sdk.SidecarResult](func(result *sdk.SidecarResult, err error) {
		results <- result
	}), results
}

func TestSidecarRemoteWasmReadsStatusOverThePort(t *testing.T) {
	port := newTestSidecarPort(t)
	remote, events := openTestSidecarOverPort(t, port)
	if state := remote.GetState(); state != "offline" {
		t.Fatalf("before the first status: %s", state)
	}
	online := <-events.events
	if online.Kind != "state" || online.State != "online" || online.Generation != 3 {
		t.Fatalf("the first event: %+v", online)
	}
	if state, reason := remote.GetState(), remote.GetStateReason(); state != "online" || reason != "" {
		t.Fatalf("the daemon's state: %s %s", state, reason)
	}
	if sidecarId := remote.GetSidecarId(); sidecarId == nil || sidecarId.String() != testSidecarPortSidecarId {
		t.Fatalf("the daemon's sidecar_id: %v", sidecarId)
	}
	// what crossed is JSON-RPC 2.0 and nothing more, which the native host holds a request to
	if len(port.posted) != 1 {
		t.Fatalf("%d requests posted", len(port.posted))
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(port.posted[0]), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || string(fields["jsonrpc"]) != `"2.0"` || string(fields["method"]) != `"status"` || len(fields["id"]) == 0 {
		t.Fatalf("the request: %s", port.posted[0])
	}
}

func TestSidecarRemoteWasmJoinsAChunkedReply(t *testing.T) {
	port := newTestSidecarPort(t)
	port.chunkByteCount = 40
	port.state, port.reason, port.generation = "unenrolled", "parent_lost", 5
	remote, events := openTestSidecarOverPort(t, port)
	lost := <-events.events
	if lost.State != "unenrolled" || lost.Reason != "parent_lost" || lost.Generation != 5 {
		t.Fatalf("the chunked status: %+v", lost)
	}
	if sidecarId := remote.GetSidecarId(); sidecarId == nil || sidecarId.String() != testSidecarPortSidecarId {
		t.Fatalf("the daemon's sidecar_id: %v", sidecarId)
	}
}

// Only the reason a verified SidecarRevoke carries reads revoked.
func TestSidecarRemoteWasmRevokedNeedsItsReason(t *testing.T) {
	forged := newTestSidecarPort(t)
	forged.state, forged.reason = "revoked", ""
	remote, events := openTestSidecarOverPort(t, forged)
	if event := <-events.events; event.State != "unenrolled" || event.Reason != "refused" || remote.GetState() != "unenrolled" {
		t.Fatalf("revoked with no reason: %+v", event)
	}
	signed := newTestSidecarPort(t)
	signed.state, signed.reason = "revoked", "signed"
	remote, events = openTestSidecarOverPort(t, signed)
	if event := <-events.events; event.State != "revoked" || event.Reason != "signed" || remote.GetStateReason() != "signed" {
		t.Fatalf("a signed revocation: %+v", event)
	}
}

func TestSidecarRemoteWasmRefusesWhatTheNativeHostDoesNotRelay(t *testing.T) {
	if local, err := sdk.NewSidecarLocal(nil, nil, nil, sdk.DefaultSidecarSettings()); err == nil || local != nil {
		t.Fatal("a local sidecar opened on js")
	}
	if _, err := sdk.NewSidecarRemote(js.Global().Get("Object").New(), nil, sdk.DefaultSidecarSettings()); err == nil {
		t.Fatal("a remote sidecar opened on something that is no port")
	}
	port := newTestSidecarPort(t)
	named := sdk.DefaultSidecarSettings()
	named.Components.Add(sdk.SidecarComponentEvs)
	if _, err := sdk.NewSidecarRemote(port.port, nil, named); err == nil {
		t.Fatal("a remote sidecar opened with a component")
	}

	remote, events := openTestSidecarOverPort(t, port)
	<-events.events
	const pass = "a pass no port may carry"
	callback, results := testSidecarPortResult()
	remote.Unlock(pass, callback)
	if result := <-results; result.Ok || result.Message == "" {
		t.Fatalf("an unlock over the port: %+v", result)
	}
	callback, results = testSidecarPortResult()
	remote.EnrollWithDevice(nil, callback)
	if result := <-results; result.Ok {
		t.Fatalf("an enrollment over the port: %+v", result)
	}
	callback, results = testSidecarPortResult()
	remote.SetPushToken("apns", "0123456789abcdef", callback)
	if result := <-results; result.Ok {
		t.Fatalf("a push token over the port: %+v", result)
	}
	// each refusal is this sdk's own: nothing but the status read crossed, the pass least of all
	if len(port.posted) != 1 || strings.Contains(strings.Join(port.posted, ""), pass) {
		t.Fatalf("the port carried %v", port.posted)
	}
}

func TestSidecarRemoteWasmDisconnectIsOffline(t *testing.T) {
	port := newTestSidecarPort(t)
	remote, events := openTestSidecarOverPort(t, port)
	<-events.events
	port.goneListener.Invoke()
	offline := <-events.events
	if offline.State != "offline" || offline.Generation != 3 || remote.GetState() != "offline" {
		t.Fatalf("after the port went: %+v", offline)
	}
	// the identity it read stands, and Close disconnects nothing twice
	if sidecarId := remote.GetSidecarId(); sidecarId == nil || sidecarId.String() != testSidecarPortSidecarId {
		t.Fatalf("the sidecar_id after the port went: %v", sidecarId)
	}
	remote.Close()
	remote.Close()
	if port.disconnected != 0 {
		t.Fatalf("a port that had gone was disconnected %d times", port.disconnected)
	}

	open := newTestSidecarPort(t)
	closing, events := openTestSidecarOverPort(t, open)
	<-events.events
	closing.Close()
	if open.disconnected != 1 || closing.GetState() != "unenrolled" {
		t.Fatalf("Close disconnected the port %d times, state %s", open.disconnected, closing.GetState())
	}
}
