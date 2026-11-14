//go:build js && !ios_extension

// sdk.Sidecar on js (§18.5's js row, §18.8's connectNative path; plan-SDK-small-readings #7): the remote backend over
// a host-supplied port to `ur native-host`, which relays to the default instance's `ur daemon` in control mode (§5.8).
// sdk.wasm links no sidecar package, so this leg has no local backend: NewSidecarLocal answers unsupported until the
// extension's own Worker (Later).
//
//	port     a chrome.runtime.Port from connectNative, or anything shaped like one: postMessage(message),
//	         onMessage.addListener(listener), onDisconnect.addListener(listener) and disconnect(). The host opens it;
//	         this file never names a native host or a manifest
//	frames   one JSON-RPC 2.0 object a message, either way. A reply past the native host's chunk size arrives as
//	         {"ur_chunk":{"id","index","count"},"data_base64"} messages in index order, joined here within
//	         AnswerMaxByteCount (cli/native_host_frame.go)
//	relayed  what the native host's allow-list names, status today: the state, its reason, the generation and the
//	         sidecar_id. It never relays a confirmed call, so one is refused here before it is sent, and it pairs
//	         nothing, so EnrollWithDevice refuses: the extension is alarms-only (§18.4)
//	state    as on a desktop (sidecar_backend.go): offline while the port or the daemon is gone, an older
//	         generation's status dropped, revoked only with its verified reason. A port that disconnected stays
//	         offline: the host opens another and a new Sidecar
//
// Only state events reach a listener (question sdk-m8-02-remote-events).

package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"syscall/js"
)

var (
	// The js leg links no core.
	errSidecarLocalUnsupported = errors.New("a local sidecar is not supported on js until the extension's own Worker")
	// The native host pairs nothing and unlocks nothing.
	errSidecarJsEnroll    = errors.New("the native host pairs no sidecar: enroll with ur login or a desktop app")
	errSidecarJsUnlock    = errors.New("a remote-mode sidecar's daemon unlocks its own keychain")
	errSidecarJsPushToken = errors.New("a remote-mode sidecar's daemon takes no app wake token")
	// The port is gone: the host opens another.
	errSidecarPortDisconnected = errors.New("the native host's port disconnected")
	// A reply the port carried that is no answer to the call in flight.
	errSidecarPortReply = errors.New("the native host's reply does not read")
)

// What the native host answers a call it does not relay confirmed, as the daemon names it (§5.8, D-CORE-13).
const sidecarPortConfirmationRefused = "the native host relays no call a person must confirm"

// NewSidecarLocal on js: unsupported, since sdk.wasm links no sidecar core.
func NewSidecarLocal(networkSpace *NetworkSpace, keyStore SidecarKeyStore, presenceSigner SidecarPresenceSigner, settings *SidecarSettings) (*Sidecar, error) {
	return nil, errSidecarLocalUnsupported
}

// One network's sidecar, on the remote backend. Safe for concurrent use.
type Sidecar struct {
	ctx            context.Context
	cancel         context.CancelFunc
	settings       *SidecarSettings
	presenceSigner SidecarPresenceSigner
	remote         *sidecarRemoteBackend
	channel        *sidecarPortChannel
	events         chan *SidecarEvent

	stateLock sync.Mutex
	listeners map[int]SidecarEventListener
	nextId    int
	closed    bool
}

// What a test adjusts in a js Sidecar. Production passes the zero value.
type sidecarJsOptions struct {
	// Nil: defaultSidecarRemoteSettings.
	remoteSettings *sidecarRemoteSettings
	// The test reads the daemon's status itself: no read runs on its own.
	noStatusPoll bool
}

// A Sidecar on the remote backend over port, a host-supplied port to `ur native-host`. presenceSigner may be nil. A
// Sidecar named with a component is refused: none is bound over the native host.
func NewSidecarRemote(port js.Value, presenceSigner SidecarPresenceSigner, settings *SidecarSettings) (*Sidecar, error) {
	return traceWithReturnError(func() (*Sidecar, error) {
		return newSidecarRemoteJs(context.Background(), port, presenceSigner, settings, &sidecarJsOptions{})
	})
}

// NewSidecarRemote with a test's options.
func newSidecarRemoteJs(ctx context.Context, port js.Value, presenceSigner SidecarPresenceSigner, settings *SidecarSettings, options *sidecarJsOptions) (*Sidecar, error) {
	if settings == nil {
		return nil, errors.New("a remote sidecar needs settings")
	}
	if settings.Components != nil && 0 < settings.Components.Len() {
		return nil, errors.New("no sidecar component is bound over the native host")
	}
	if settings.MemoryBudgetByteCount < 0 {
		return nil, errors.New("a sidecar's memory budget must not be negative")
	}
	for _, name := range []string{"postMessage", "disconnect"} {
		if port.Type() != js.TypeObject || port.Get(name).Type() != js.TypeFunction {
			return nil, errors.New("a remote sidecar on js needs a port to ur native-host")
		}
	}
	for _, name := range []string{"onMessage", "onDisconnect"} {
		if event := port.Get(name); event.Type() != js.TypeObject || event.Get("addListener").Type() != js.TypeFunction {
			return nil, errors.New("a remote sidecar on js needs a port to ur native-host")
		}
	}
	remoteSettings := options.remoteSettings
	if remoteSettings == nil {
		remoteSettings = defaultSidecarRemoteSettings()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &Sidecar{
		ctx:            cancelCtx,
		cancel:         cancel,
		settings:       settings,
		presenceSigner: presenceSigner,
		channel:        newSidecarPortChannel(port, remoteSettings),
		events:         make(chan *SidecarEvent, remoteSettings.EventQueueSize),
		listeners:      map[int]SidecarEventListener{},
	}
	self.remote = newSidecarRemoteBackend(cancelCtx, self.channel.dial, remoteSettings, func(generation int64, state string, reason string) {
		// never blocks: a full queue drops the event, and an app reads GetState for where things stand
		select {
		case self.events <- &SidecarEvent{Kind: "state", Generation: generation, State: state, Reason: reason}:
		default:
		}
	})
	self.channel.onGone = self.remote.applyOffline
	go self.deliver()
	if !options.noStatusPoll {
		go self.remote.run()
	}
	return self, nil
}

// Delivers each event to every listener, in order, until the Sidecar closes.
func (self *Sidecar) deliver() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case event := <-self.events:
			listeners := func() []SidecarEventListener {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				listeners := make([]SidecarEventListener, 0, len(self.listeners))
				for id := 0; id < self.nextId; id += 1 {
					if listener, ok := self.listeners[id]; ok {
						listeners = append(listeners, listener)
					}
				}
				return listeners
			}()
			for _, listener := range listeners {
				if self.ctx.Err() != nil {
					return
				}
				bound := *event
				trace(func() {
					listener.SidecarEventReceived(&bound)
				})
			}
		}
	}
}

// The daemon's state (§18.5's State): offline while the port or the daemon is gone, and unenrolled once closed.
func (self *Sidecar) GetState() string {
	if self.ctx.Err() != nil {
		return sidecarRemoteStateUnenrolled
	}
	state, _ := self.remote.state()
	return state
}

// The state's reason, empty where the state has none.
func (self *Sidecar) GetStateReason() string {
	if self.ctx.Err() != nil {
		return ""
	}
	_, reason := self.remote.state()
	return reason
}

// The daemon's sidecar_id, nil before it has one.
func (self *Sidecar) GetSidecarId() *Id {
	return self.remote.sidecarId()
}

// Calls listener with each event from here on, in order, until the answer closes. Only state events arrive.
func (self *Sidecar) AddEventListener(listener SidecarEventListener) Sub {
	if listener == nil {
		return newSub(func() {})
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	id := self.nextId
	self.nextId += 1
	self.listeners[id] = listener
	return newSub(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.listeners, id)
	})
}

// Answers callback once with a refusal.
func sidecarJsRefuse(callback SidecarResultCallback, err error) {
	go func() {
		if callback != nil {
			callback.Result(&SidecarResult{Message: err.Error()}, nil)
		}
	}()
}

// Refused: the native host pairs nothing (§18.4).
func (self *Sidecar) EnrollWithDevice(device Device, callback SidecarResultCallback) {
	sidecarJsRefuse(callback, errSidecarJsEnroll)
}

// Refused: the daemon unlocks (§5.9). The passphrase goes nowhere.
func (self *Sidecar) Unlock(passphrase string, callback SidecarResultCallback) {
	sidecarJsRefuse(callback, errSidecarJsUnlock)
}

// Refused: the daemon registers no app wake token.
func (self *Sidecar) SetPushToken(platform string, token string, callback SidecarResultCallback) {
	sidecarJsRefuse(callback, errSidecarJsPushToken)
}

// Ends the Sidecar: no event reaches a listener after it returns from outside one, and the port disconnects. Safe to
// call again.
func (self *Sidecar) Close() {
	alreadyClosed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.closed = true
		return false
	}()
	if alreadyClosed {
		return
	}
	self.remote.close()
	self.channel.close()
	self.cancel()
}

// A request as the native host takes it: JSON-RPC 2.0 and nothing more (cli/cmd_native_host.go).
type sidecarPortRequest struct {
	JsonRpc string `json:"jsonrpc"`
	Id      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// A message the native host sends: a reply, or one chunk of one.
type sidecarPortMessage struct {
	Id     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Chunk *struct {
		Id    json.RawMessage `json:"id"`
		Index int             `json:"index"`
		Count int             `json:"count"`
	} `json:"ur_chunk"`
	DataBase64 string `json:"data_base64"`
}

// The control channel over a native-host port: one call at a time, as the backend makes them.
type sidecarPortChannel struct {
	port     js.Value
	settings *sidecarRemoteSettings
	// closed once the port disconnected or the channel closed
	gone chan struct{}
	// called once when the port goes, so the state reads offline without waiting for a call to fail; set before the
	// first call
	onGone func()
	funcs  []js.Func

	stateLock sync.Mutex
	nextId    int64
	// the call in flight: its id as the reply spells it, and where its reply goes
	pendingId string
	pending   chan *sidecarPortMessage
	// a chunked reply so far
	chunks         [][]byte
	chunkByteCount int64
	isGone         bool
}

// A channel on port, listening for its messages and its disconnect.
func newSidecarPortChannel(port js.Value, settings *sidecarRemoteSettings) *sidecarPortChannel {
	self := &sidecarPortChannel{port: port, settings: settings, gone: make(chan struct{})}
	onMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			self.receive(args[0])
		}
		return nil
	})
	onDisconnect := js.FuncOf(func(this js.Value, args []js.Value) any {
		self.markGone()
		return nil
	})
	self.funcs = []js.Func{onMessage, onDisconnect}
	port.Get("onMessage").Call("addListener", onMessage)
	port.Get("onDisconnect").Call("addListener", onDisconnect)
	return self
}

// The backend's dial: the one session this port is, until it disconnects.
func (self *sidecarPortChannel) dial(ctx context.Context) (sidecarControlChannel, error) {
	select {
	case <-self.gone:
		return nil, errSidecarPortDisconnected
	default:
		return self, nil
	}
}

// The port is gone: a call in flight ends, and the state reads offline.
func (self *sidecarPortChannel) markGone() {
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.isGone {
			return false
		}
		self.isGone = true
		close(self.gone)
		return true
	}()
	if first && self.onGone != nil {
		self.onGone()
	}
}

// One message from the port: the pending call's reply, a chunk of it, or something to drop.
func (self *sidecarPortChannel) receive(value js.Value) {
	text := js.Global().Get("JSON").Call("stringify", value)
	if text.Type() != js.TypeString {
		return
	}
	message := &sidecarPortMessage{}
	if err := json.Unmarshal([]byte(text.String()), message); err != nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		return
	}
	if chunk := message.Chunk; chunk != nil {
		// chunks of the call in flight, in order, within the bound; anything else ends the reply as unreadable
		data, err := base64.StdEncoding.DecodeString(message.DataBase64)
		self.chunkByteCount += int64(len(data))
		if err != nil || string(chunk.Id) != self.pendingId || chunk.Index != len(self.chunks) || chunk.Count <= chunk.Index ||
			self.settings.AnswerMaxByteCount < self.chunkByteCount {
			self.answer(nil)
			return
		}
		self.chunks = append(self.chunks, data)
		if len(self.chunks) < chunk.Count {
			return
		}
		joined := []byte{}
		for _, part := range self.chunks {
			joined = append(joined, part...)
		}
		whole := &sidecarPortMessage{}
		if err := json.Unmarshal(joined, whole); err != nil || whole.Chunk != nil {
			self.answer(nil)
			return
		}
		message = whole
	}
	if string(message.Id) != self.pendingId {
		return
	}
	self.answer(message)
}

// Hands the call in flight its reply, nil for one that does not read. Called under the lock.
func (self *sidecarPortChannel) answer(message *sidecarPortMessage) {
	self.pending <- message
	self.pending = nil
	self.chunks = nil
	self.chunkByteCount = 0
}

// See sidecarControlChannel. A confirmed call is refused unsent: the native host relays none.
func (self *sidecarPortChannel) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	if confirmed {
		return &sidecarControlError{Code: sidecarControlCodeConfirmationRequired, Message: sidecarPortConfirmationRefused}
	}
	replies := make(chan *sidecarPortMessage, 1)
	id, err := func() (int64, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.isGone {
			return 0, errSidecarPortDisconnected
		}
		self.nextId += 1
		self.pendingId = strconv.FormatInt(self.nextId, 10)
		self.pending = replies
		self.chunks = nil
		self.chunkByteCount = 0
		return self.nextId, nil
	}()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(&sidecarPortRequest{JsonRpc: "2.0", Id: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := traceWithReturn(func() js.Value {
		return self.port.Call("postMessage", js.Global().Get("JSON").Call("parse", string(encoded)))
	}); err != nil {
		return errors.Join(errSidecarPortDisconnected, err)
	}
	select {
	case message := <-replies:
		switch {
		case message == nil:
			return errSidecarPortReply
		case message.Error != nil:
			return &sidecarControlError{Code: message.Error.Code, Message: message.Error.Message}
		case result == nil:
			return nil
		}
		if err := json.Unmarshal(message.Result, result); err != nil {
			return errSidecarPortReply
		}
		return nil
	case <-self.gone:
		return errSidecarPortDisconnected
	case <-ctx.Done():
		return ctx.Err()
	}
}

// See sidecarControlChannel. Disconnects the port: a session that failed cannot be told from its next answer.
func (self *sidecarPortChannel) close() {
	alreadyGone := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.isGone
	}()
	self.markGone()
	if !alreadyGone {
		trace(func() {
			self.port.Call("disconnect")
		})
	}
}
