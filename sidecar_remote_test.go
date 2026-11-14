//go:build !ios_extension && !js

package sdk

// The remote backend against a fake daemon over a real Unix socket (SDK-M8-02): the fake speaks §5.8's hello and
// control mode's JSON-RPC lines, answers status, pair and the forwarding methods as ur daemon does, and records every
// request line it took, so a test can say what crossed and what did not. Status reads are the test's own (no poll),
// and events are read in order off a listener's channel, so nothing waits on a clock.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

const testRemoteDaemonVersion = "synthetic-version"

// One request line the fake daemon took.
type testRemoteDaemonRequest struct {
	Method      string          `json:"method"`
	Id          json.RawMessage `json:"id"`
	Params      json.RawMessage `json:"params"`
	UrConfirmed bool            `json:"ur_confirmed"`
	// the line as it arrived
	line []byte
}

// A fake ur daemon: one network's sidecar, whose state a test sets.
type testRemoteDaemon struct {
	t          *testing.T
	socketPath string
	listener   net.Listener
	serving    sync.WaitGroup

	stateLock  sync.Mutex
	networkId  sidecar.Id
	sidecarId  sidecar.Id
	state      string
	reason     string
	generation int64
	requests   []testRemoteDaemonRequest
	conns      []net.Conn
	pairedJwts []string
	// answers a request in place of the daemon's own, when it returns true
	answer func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool)
	// extra fields every status reply carries
	statusExtra map[string]any
}

// A short directory of its own, since a test's own directory can pass sun_path's 104 bytes on macOS.
func testRemoteSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "ur")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return filepath.Join(directory, "default.sock")
}

// A fake daemon, not yet listening: unenrolled at generation 0 with a sidecar_id.
func newTestRemoteDaemon(t *testing.T, socketPath string) *testRemoteDaemon {
	t.Helper()
	return &testRemoteDaemon{
		t:          t,
		socketPath: socketPath,
		networkId:  sidecar.NewId(),
		sidecarId:  sidecar.NewId(),
		state:      "unenrolled",
	}
}

// Listens and serves until stop or the test's end.
func (self *testRemoteDaemon) start() {
	self.t.Helper()
	listener, err := net.Listen("unix", self.socketPath)
	if err != nil {
		self.t.Fatal(err)
	}
	self.listener = listener
	self.serving.Add(1)
	go func() {
		defer self.serving.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.conns = append(self.conns, conn)
			}()
			self.serving.Add(1)
			go func() {
				defer self.serving.Done()
				defer conn.Close()
				self.serve(conn)
			}()
		}
	}()
	self.t.Cleanup(self.stop)
}

// Ends every session and the listener, and removes the socket.
func (self *testRemoteDaemon) stop() {
	if self.listener == nil {
		return
	}
	self.listener.Close()
	conns := func() []net.Conn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		conns := self.conns
		self.conns = nil
		return conns
	}()
	for _, conn := range conns {
		conn.Close()
	}
	self.serving.Wait()
	self.listener = nil
	os.Remove(self.socketPath)
}

func (self *testRemoteDaemon) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	hello := &sidecar.DaemonHello{}
	if err := json.Unmarshal(line, hello); err != nil {
		return
	}
	answer := map[string]any{"ur_local": 1, "version": testRemoteDaemonVersion}
	if hello.Version != testRemoteDaemonVersion {
		answer["error"] = sidecar.DaemonHelloErrorVersionSkew
	} else if hello.Mode != sidecar.DaemonModeControl {
		answer["error"] = sidecar.DaemonHelloErrorMode
	}
	encoded, _ := json.Marshal(answer)
	if _, err := conn.Write(append(encoded, '\n')); err != nil || answer["error"] != nil {
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		request := testRemoteDaemonRequest{line: bytes.Clone(line)}
		if err := json.Unmarshal(line, &request); err != nil {
			return
		}
		result, refusal := self.handle(request)
		response := map[string]any{"jsonrpc": "2.0", "id": request.Id}
		if refusal != nil {
			response["error"] = refusal
		} else {
			response["result"] = result
		}
		encoded, _ := json.Marshal(response)
		if _, err := conn.Write(append(encoded, '\n')); err != nil {
			return
		}
	}
}

func (self *testRemoteDaemon) handle(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, request)
	if self.answer != nil {
		if result, refusal, ok := self.answer(request); ok {
			return result, refusal
		}
	}
	switch request.Method {
	case "status":
		reply := map[string]any{
			"instance":   "default",
			"state":      self.state,
			"reason":     self.reason,
			"generation": self.generation,
			"key_store":  "keychain",
			"networks": []map[string]any{{
				"network_id": self.networkId,
				"sidecar_id": self.sidecarId,
				"state":      self.state,
				"reason":     self.reason,
				"generation": self.generation,
			}},
		}
		for name, value := range self.statusExtra {
			reply[name] = value
		}
		return reply, nil
	case sidecar.DaemonMethodPair:
		params := &sidecar.DaemonPairParams{}
		if json.Unmarshal(request.Params, params) != nil || params.ChildJwt == "" {
			return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "pair takes the child's token"}
		}
		self.pairedJwts = append(self.pairedJwts, params.ChildJwt)
		self.generation += 1
		self.state, self.reason = "online", ""
		return &sidecar.DaemonPairReply{NetworkId: self.networkId, Generation: self.generation}, nil
	}
	return nil, &sidecar.DaemonRpcError{Code: -32601, Message: "method not found"}
}

// Sets the sidecar's state as the daemon's core would.
func (self *testRemoteDaemon) set(state string, reason string, generation int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.state, self.reason, self.generation = state, reason, generation
}

func (self *testRemoteDaemon) seen() []testRemoteDaemonRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testRemoteDaemonRequest{}, self.requests...)
}

func (self *testRemoteDaemon) seenMethods() []string {
	methods := []string{}
	for _, request := range self.seen() {
		methods = append(methods, request.Method)
	}
	return methods
}

func (self *testRemoteDaemon) paired() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.pairedJwts...)
}

// Events in order, off a listener.
type testRemoteEvents struct {
	events chan *SidecarEvent
}

func (self *testRemoteEvents) SidecarEventReceived(event *SidecarEvent) {
	self.events <- event
}

func (self *testRemoteEvents) next(t *testing.T) *SidecarEvent {
	t.Helper()
	select {
	case event := <-self.events:
		return event
	case <-time.After(testSidecarTimeout):
		t.Fatal("no event")
		return nil
	}
}

// A remote Sidecar on the daemon at socketPath, its status reads the test's own, and its events.
func openTestRemoteSidecar(t *testing.T, socketPath string) (*Sidecar, *sidecarRemoteBackend, *testRemoteEvents) {
	t.Helper()
	remoteSettings := defaultSidecarRemoteSettings()
	remoteSettings.CallTimeout = testSidecarTimeout
	remoteSettings.PairTimeout = testSidecarTimeout
	remote, err := newSidecarRemote(context.Background(), nil, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:           sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion),
		remoteSettings: remoteSettings,
		noStatusPoll:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	events := &testRemoteEvents{events: make(chan *SidecarEvent, 64)}
	remote.AddEventListener(events)
	return remote, remote.remote.(*sidecarRemoteBackend), events
}

// The device's api: each mint answers a new child with both marks unless marks says otherwise, and a removal is
// answered. The tokens it minted, in order.
type testRemoteDeviceApi struct {
	server      *testSessionServer
	ctx         context.Context
	api         *Api
	deviceToken string

	stateLock sync.Mutex
	children  []string
}

func newTestRemoteDeviceApi(t *testing.T, marks map[string]bool) *testRemoteDeviceApi {
	t.Helper()
	self := &testRemoteDeviceApi{}
	self.server = newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			childClientId := sidecar.NewId()
			child := testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 0)
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.children = append(self.children, child)
			reply := map[string]any{"by_client_jwt": child, "client_id": childClientId.String()}
			for name, value := range marks {
				reply[name] = value
			}
			return http.StatusOK, reply
		case "/network/remove-client":
			return http.StatusOK, map[string]any{}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	self.ctx, self.api = newTestApi(t, self.server)
	self.deviceToken = dpopTestClientToken(t)
	self.api.SetByJwt(self.deviceToken)
	return self
}

func (self *testRemoteDeviceApi) minted() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.children...)
}

func (self *testRemoteDeviceApi) seenPaths() []string {
	paths := []string{}
	for _, request := range self.server.seen() {
		paths = append(paths, request.path)
	}
	return paths
}

var testRemoteBothMarks = map[string]bool{"admin_refused": true, "strict_exp": true}

func TestRemoteUnlockRefused(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("locked", "", 0)
	daemon.start()
	remote, backend, _ := openTestRemoteSidecar(t, socketPath)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := remote.GetState(); state != "locked" {
		t.Fatalf("the daemon's state: %s", state)
	}

	const pass = "a pass no daemon may see"
	for _, passphrase := range []string{pass, ""} {
		callback, results := testSidecarResultCallback()
		remote.Unlock(passphrase, callback)
		if result := <-results; result.Ok || result.Message != errSidecarRemoteUnlock.Error() {
			t.Fatalf("a remote unlock of %q: %+v", passphrase, result)
		}
	}
	// the refusal is the sdk's own: nothing crossed, the pass least of all
	for _, request := range daemon.seen() {
		if request.Method != "status" || bytes.Contains(request.line, []byte(pass)) {
			t.Fatalf("an unlock reached the daemon: %s", request.Method)
		}
	}
	if state := remote.GetState(); state != "locked" {
		t.Fatalf("the state after a refused unlock: %s", state)
	}

	// and the daemon takes no app wake token
	callback, results := testSidecarResultCallback()
	remote.SetPushToken(SidecarPushPlatformApns, "0123456789abcdef", callback)
	if result := <-results; result.Ok {
		t.Fatalf("a remote push token: %+v", result)
	}
	if methods := daemon.seenMethods(); len(methods) != 1 {
		t.Fatalf("the daemon saw %v", methods)
	}
}

func TestRemoteRepliesHoldNoSecret(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, _, events := openTestRemoteSidecar(t, socketPath)

	// a broken daemon that repeats the token in its refusal: the app sees a fixed message
	daemon.stateLock.Lock()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecar.DaemonMethodPair {
			return nil, nil, false
		}
		params := &sidecar.DaemonPairParams{}
		json.Unmarshal(request.Params, params)
		return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "cannot bind " + params.ChildJwt}, true
	}
	daemon.stateLock.Unlock()
	_, err := remote.enrollWithDeviceApi(device.ctx, device.api)
	refused := sidecarResultOf("", err)
	if err == nil || !errors.Is(err, errSidecarRemotePairRefused) {
		t.Fatalf("a refusal repeating the token: %v", err)
	}

	// a daemon whose replies carry fields no reply may: none is read
	const leaked = "a-token-no-reply-may-carry"
	daemon.stateLock.Lock()
	daemon.statusExtra = map[string]any{"by_client_jwt": leaked, "pass": leaked}
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecar.DaemonMethodPair {
			return nil, nil, false
		}
		daemon.generation += 1
		daemon.state = "online"
		return map[string]any{"network_id": daemon.networkId, "generation": daemon.generation, "child_jwt": leaked, "recovery_code": leaked}, nil, true
	}
	daemon.stateLock.Unlock()
	value, err := remote.enrollWithDeviceApi(device.ctx, device.api)
	if err != nil {
		t.Fatal(err)
	}
	paired := sidecarResultOf(value, err)
	heard := []*SidecarEvent{}
	for len(heard) == 0 || heard[len(heard)-1].State != "online" {
		heard = append(heard, events.next(t))
	}

	secrets := append(device.minted(), device.deviceToken, leaked)
	if len(secrets) != 4 {
		t.Fatalf("minted %d children", len(device.minted()))
	}
	surfaced := []string{}
	for _, result := range []*SidecarResult{refused, paired} {
		surfaced = append(surfaced, result.Value, result.Code, result.Message)
	}
	surfaced = append(surfaced, remote.GetState(), remote.GetStateReason(), remote.GetSidecarId().String())
	eventJson, err := json.Marshal(heard)
	if err != nil {
		t.Fatal(err)
	}
	surfaced = append(surfaced, string(eventJson))
	for _, text := range surfaced {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("a secret reached the app in %q", text)
			}
		}
	}

	// the device's token never reaches the daemon, and a child's token only in its pair
	for _, request := range daemon.seen() {
		if bytes.Contains(request.line, []byte(device.deviceToken)) {
			t.Fatalf("the device's token reached the daemon in %s", request.Method)
		}
		for _, child := range device.minted() {
			if request.Method != sidecar.DaemonMethodPair && bytes.Contains(request.line, []byte(child)) {
				t.Fatalf("a child's token reached the daemon in %s", request.Method)
			}
		}
	}
	// the refused child was removed with the device's token
	removed := 0
	for _, request := range device.server.seen() {
		if request.path == "/network/remove-client" {
			removed += 1
			if request.authorization != "Bearer "+device.deviceToken {
				t.Fatalf("the removal on %q", request.authorization)
			}
		}
	}
	if removed != 1 {
		t.Fatalf("%d removals for one refused pairing", removed)
	}
}

func TestPairKeepsSidecarId(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	// an enrolled daemon whose parent was lost: its identity stands
	daemon.set("unenrolled", "parent_lost", 1)
	daemon.start()
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, backend, events := openTestRemoteSidecar(t, socketPath)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lost := events.next(t); lost.State != "unenrolled" || lost.Reason != "parent_lost" || lost.Generation != 1 {
		t.Fatalf("the first event: %+v", lost)
	}
	sidecarId := remote.GetSidecarId()
	if sidecarId == nil || sidecarId.String() != daemon.sidecarId.String() {
		t.Fatalf("the daemon's sidecar_id: %v", sidecarId)
	}

	for generation := int64(2); generation <= 3; generation += 1 {
		callback, results := testSidecarResultCallback()
		go func() {
			value, err := remote.enrollWithDeviceApi(device.ctx, device.api)
			callback.Result(sidecarResultOf(value, err), nil)
		}()
		result := <-results
		if !result.Ok || result.Value != sidecarId.String() {
			t.Fatalf("the pairing at generation %d: %+v", generation, result)
		}
		if got := remote.GetSidecarId(); got == nil || got.Cmp(sidecarId) != 0 {
			t.Fatalf("the sidecar_id after generation %d: %v", generation, got)
		}
		if state, bound := remote.GetState(), backend.generation(); state != "online" || bound != generation {
			t.Fatalf("after the pairing: %s at generation %d", state, bound)
		}
		if generation == 2 {
			if online := events.next(t); online.State != "online" || online.Generation != 2 {
				t.Fatalf("the pairing's event: %+v", online)
			}
		}
	}

	// each mint on the device's token with both marks, each child given to the daemon and to nothing else
	minted := device.minted()
	if paired := daemon.paired(); len(minted) != 2 || len(paired) != 2 || paired[0] != minted[0] || paired[1] != minted[1] {
		t.Fatalf("minted %d, paired %d", len(minted), len(daemon.paired()))
	}
	for _, request := range device.server.seen() {
		if request.path != "/network/auth-client" || request.authorization != "Bearer "+device.deviceToken {
			t.Fatalf("the device's api saw %s on %q", request.path, request.authorization)
		}
		args := &sidecarChildMintArgs{}
		if err := json.Unmarshal(request.body, args); err != nil || !args.AdminRefused || !args.StrictExp {
			t.Fatalf("the mint's marks: %s", request.body)
		}
	}
	for _, request := range daemon.seen() {
		if request.UrConfirmed {
			t.Fatalf("%s went confirmed: a pairing is no person's act", request.Method)
		}
	}
}

func TestPairRefusesAChildWithoutMarks(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	device := newTestRemoteDeviceApi(t, map[string]bool{"strict_exp": true})
	remote, _, _ := openTestRemoteSidecar(t, socketPath)

	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); !errors.Is(err, errSidecarRemoteChildMarks) {
		t.Fatalf("a child without admin_refused: %v", err)
	}
	if paired := daemon.paired(); len(paired) != 0 {
		t.Fatal("a child without admin_refused reached the daemon")
	}
	if paths := device.seenPaths(); len(paths) != 2 || paths[1] != "/network/remove-client" {
		t.Fatalf("the unkept child was not removed: %v", paths)
	}

	// a pairing whose session dies before its answer may have bound the child, so the child is left
	lost := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	daemon.stateLock.Lock()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecar.DaemonMethodPair {
			return nil, nil, false
		}
		for _, conn := range daemon.conns {
			conn.Close()
		}
		return nil, nil, true
	}
	daemon.stateLock.Unlock()
	if _, err := remote.enrollWithDeviceApi(lost.ctx, lost.api); !errors.Is(err, errSidecarRemoteOffline) {
		t.Fatalf("a pairing whose session died: %v", err)
	}
	if paths := lost.seenPaths(); len(paths) != 1 || paths[0] != "/network/auth-client" {
		t.Fatalf("a child of unknown outcome was removed: %v", paths)
	}
	if state := remote.GetState(); state != "offline" {
		t.Fatalf("the state after the session died: %s", state)
	}

	// no device token: nothing is minted
	device.api.SetByJwt("")
	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); err == nil || len(device.seenPaths()) != 2 {
		t.Fatalf("a pairing without a device token: %v", err)
	}
}

func TestNoDaemonIsOffline(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, backend, events := openTestRemoteSidecar(t, socketPath)

	if state, reason := remote.GetState(), remote.GetStateReason(); state != "offline" || reason != "" {
		t.Fatalf("with no daemon: %s %s", state, reason)
	}
	if err := backend.refresh(context.Background()); !errors.Is(err, errSidecarRemoteOffline) {
		t.Fatalf("a status read with no daemon: %v", err)
	}
	if remote.GetSidecarId() != nil || remote.Core() != nil {
		t.Fatal("a remote sidecar with no daemon holds an identity or a core")
	}
	// a pairing mints nothing while no daemon answers, so no client is left behind
	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); !errors.Is(err, errSidecarRemoteOffline) {
		t.Fatalf("a pairing with no daemon: %v", err)
	}
	if paths := device.seenPaths(); len(paths) != 0 {
		t.Fatalf("the device's api saw %v", paths)
	}
	// nothing was started: no socket, and nothing else in its directory
	entries, err := os.ReadDir(filepath.Dir(socketPath))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the socket's directory holds %d entries (%v)", len(entries), err)
	}

	// a daemon that starts is found at the next read, and one that goes reads offline again
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 4)
	daemon.start()
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if online := events.next(t); online.Kind != "state" || online.State != "online" || online.Generation != 4 {
		t.Fatalf("the daemon's first event: %+v", online)
	}
	daemon.stop()
	if err := backend.refresh(context.Background()); !errors.Is(err, errSidecarRemoteOffline) {
		t.Fatalf("a status read after the daemon went: %v", err)
	}
	if offline := events.next(t); offline.State != "offline" || offline.Generation != 4 {
		t.Fatalf("the event after the daemon went: %+v", offline)
	}
	if state, sidecarId := remote.GetState(), remote.GetSidecarId(); state != "offline" || sidecarId == nil {
		t.Fatalf("after the daemon went: %s, sidecar_id %v", state, sidecarId)
	}
}

// A daemon of another version refuses the hello, and the Sidecar reads offline.
func TestRemoteVersionSkewIsOffline(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, err := newSidecarRemote(context.Background(), nil, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:         sidecarDaemonSocketDial(socketPath, "another-version"),
		noStatusPoll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	err = remote.remote.refresh(context.Background())
	var refusal *sidecar.DaemonHelloRefusal
	if !errors.Is(err, errSidecarRemoteOffline) || !errors.As(err, &refusal) || refusal.Code != sidecar.DaemonHelloErrorVersionSkew {
		t.Fatalf("another version's hello: %v", err)
	}
	if state := remote.GetState(); state != "offline" || len(daemon.seen()) != 0 {
		t.Fatalf("after a refused hello: %s, %d requests", state, len(daemon.seen()))
	}
}

func TestStaleEventDroppedAcrossRepair(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 1)
	daemon.start()
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, backend, events := openTestRemoteSidecar(t, socketPath)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first := events.next(t); first.State != "online" || first.Generation != 1 {
		t.Fatalf("the first event: %+v", first)
	}

	// the status read is in flight when the re-pair binds generation 2: the daemon answers it late, with the old
	// generation's end
	stale := &sidecarControlStatus{Networks: []sidecarControlNetworkStatus{{
		NetworkId:  newId([16]byte(daemon.networkId)),
		SidecarId:  newId([16]byte(daemon.sidecarId)),
		State:      "unenrolled",
		Reason:     "parent_lost",
		Generation: 1,
	}}}
	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); err != nil {
		t.Fatal(err)
	}
	if paired := events.next(t); paired.State != "online" || paired.Generation != 2 {
		t.Fatalf("the re-pair's event: %+v", paired)
	}
	backend.applyStatus(stale)
	if state, reason := remote.GetState(), remote.GetStateReason(); state != "online" || reason != "" || backend.generation() != 2 {
		t.Fatalf("a stale status ended the new pairing: %s %s at %d", state, reason, backend.generation())
	}

	// the same end at the new generation is taken, and it is the next event: the stale one raised none
	daemon.set("unenrolled", "parent_lost", 2)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lost := events.next(t); lost.State != "unenrolled" || lost.Reason != "parent_lost" || lost.Generation != 2 {
		t.Fatalf("the event after the stale status: %+v", lost)
	}
	// a logout event of the old generation released late is dropped by the dispatch too
	remote.events.offer(sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: 1, State: sidecar.SidecarStateRevoked})
	daemon.set("locked", "", 2)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if locked := events.next(t); locked.State != "locked" || locked.Generation != 2 {
		t.Fatalf("an old generation's event was delivered: %+v", locked)
	}
	if got := remote.GetSidecarId(); got == nil || got.String() != daemon.sidecarId.String() {
		t.Fatalf("the sidecar_id across the re-pair: %v", got)
	}
}

// §22.5's §18 row on the remote backend: the daemon's core decides a revocation, and the app reads revoked only with
// the reason a verified SidecarRevoke carries. A forged core revocation, which the core answers unenrolled(refused),
// reads so here, and revoked under any other reason is not taken as revoked.
func TestRemoteForgedRevocationReadsUnenrolled(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 3)
	daemon.start()
	remote, backend, events := openTestRemoteSidecar(t, socketPath)
	read := func() *SidecarEvent {
		t.Helper()
		if err := backend.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		return events.next(t)
	}
	if online := read(); online.State != "online" {
		t.Fatalf("the first event: %+v", online)
	}

	// the core found no SidecarRevoke behind the hint
	daemon.set("unenrolled", "refused", 3)
	if forged := read(); forged.State != "unenrolled" || forged.Reason != "refused" || remote.GetState() != "unenrolled" || remote.GetStateReason() != "refused" {
		t.Fatalf("a forged revocation: %+v", forged)
	}
	daemon.set("online", "", 3)
	read()
	// revoked with no verified reason is never revoked here
	for _, reason := range []string{"", "refused", "expired"} {
		daemon.set("revoked", reason, 3)
		if err := backend.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if state, stateReason := remote.GetState(), remote.GetStateReason(); state != "unenrolled" || stateReason != "refused" {
			t.Fatalf("revoked(%s) read %s %s", reason, state, stateReason)
		}
	}
	if unsigned := events.next(t); unsigned.State != "unenrolled" || unsigned.Reason != "refused" {
		t.Fatalf("the unsigned revocation's event: %+v", unsigned)
	}
	// a SidecarRevoke the core verified
	daemon.set("revoked", "signed", 3)
	if signed := read(); signed.State != "revoked" || signed.Reason != "signed" || remote.GetState() != "revoked" || remote.GetStateReason() != "signed" {
		t.Fatalf("a signed revocation: %+v", signed)
	}
	// the identity stands either way
	if got := remote.GetSidecarId(); got == nil || got.String() != daemon.sidecarId.String() {
		t.Fatalf("the sidecar_id after a revocation: %v", got)
	}
	// a state this sdk does not know reads offline
	daemon.set("a-later-state", "", 3)
	if unknown := read(); unknown.State != "offline" {
		t.Fatalf("an unknown state: %+v", unknown)
	}
}

func TestRemoteForwardingRelay(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); err != nil {
		t.Fatal(err)
	}

	keys := &bridge.ForwardingKeys{
		KeySetId:             bytes.Repeat([]byte{1}, 16),
		ForwardPublicKey:     bytes.Repeat([]byte{2}, 32),
		ReaderXwingPublicKey: bytes.Repeat([]byte{3}, 1216),
		ClientPublicKey:      bytes.Repeat([]byte{4}, 32),
		HostClientId:         bytes.Repeat([]byte{5}, 16),
		HostClientPublicKey:  bytes.Repeat([]byte{6}, 32),
	}
	handOff := &bridge.ForwardingHandOff{KeySetId: keys.KeySetId, ForwardClientId: bytes.Repeat([]byte{7}, 16), ForwardClientJwt: "a-forwarding-token", StrictExpMs: 42}
	pin := &bridge.AuthPin{}
	daemon.stateLock.Lock()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecar.DaemonMethodForwardingSetup:
			params := &sidecar.DaemonForwardingSetupParams{}
			if err := json.Unmarshal(request.Params, params); err != nil {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "params"}, true
			}
			if params.ConfirmAfterRevocation && !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "needs the person's confirmation"}, true
			}
			encoded, _ := proto.Marshal(handOff)
			return &sidecar.DaemonForwardingSetupReply{HandOff: encoded}, nil, true
		case sidecar.DaemonMethodForwardingWitness:
			encoded, _ := proto.Marshal(pin)
			return &sidecar.DaemonForwardingWitnessReply{Pin: encoded, Witnessed: true, SetupDue: "foreign_revocation"}, nil, true
		}
		return nil, nil, false
	}
	daemon.stateLock.Unlock()

	got, err := remote.relayForwardingSetup(context.Background(), keys, false)
	if err != nil || !proto.Equal(got, handOff) {
		t.Fatalf("the hand-off: %v (%v)", got, err)
	}
	witness, err := remote.relayForwardingWitness(context.Background())
	if err != nil || !witness.Witnessed || witness.Fork || witness.SetupDue != "foreign_revocation" || !proto.Equal(witness.Pin, pin) {
		t.Fatalf("the witness: %+v (%v)", witness, err)
	}
	if _, err := remote.relayForwardingSetup(context.Background(), keys, true); err != nil {
		t.Fatalf("a confirmed setup after a revocation: %v", err)
	}

	setups := []testRemoteDaemonRequest{}
	for _, request := range daemon.seen() {
		switch request.Method {
		case sidecar.DaemonMethodForwardingSetup:
			setups = append(setups, request)
		case sidecar.DaemonMethodForwardingWitness:
			params := &sidecar.DaemonForwardingWitnessParams{}
			if err := json.Unmarshal(request.Params, params); err != nil || params.Network != daemon.networkId.String() || request.UrConfirmed {
				t.Fatalf("the witness call: %s", request.line)
			}
		}
	}
	if len(setups) != 2 || setups[0].UrConfirmed || !setups[1].UrConfirmed {
		t.Fatalf("%d setup calls, confirmed out of place", len(setups))
	}
	for i, request := range setups {
		params := &sidecar.DaemonForwardingSetupParams{}
		if err := json.Unmarshal(request.Params, params); err != nil {
			t.Fatal(err)
		}
		// what crossed is the component's ForwardingKeys and nothing more: public halves, for the paired network
		sent := &bridge.ForwardingKeys{}
		if err := proto.Unmarshal(params.Keys, sent); err != nil || !proto.Equal(sent, keys) {
			t.Fatalf("setup %d carried other keys (%v)", i, err)
		}
		if params.Network != daemon.networkId.String() || params.ConfirmAfterRevocation != (i == 1) {
			t.Fatalf("setup %d: network %s, confirm %t", i, params.Network, params.ConfirmAfterRevocation)
		}
		extra := map[string]json.RawMessage{}
		if err := json.Unmarshal(request.Params, &extra); err != nil {
			t.Fatal(err)
		}
		delete(extra, "network")
		delete(extra, "keys")
		delete(extra, "confirm_after_revocation")
		if len(extra) != 0 {
			t.Fatalf("setup %d carried more than the keys: %v", i, extra)
		}
	}

	// a hand-off that does not read names nothing of its bytes
	daemon.stateLock.Lock()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecar.DaemonMethodForwardingSetup {
			return nil, nil, false
		}
		return &sidecar.DaemonForwardingSetupReply{HandOff: []byte("a-forwarding-token\xff")}, nil, true
	}
	daemon.stateLock.Unlock()
	if _, err := remote.relayForwardingSetup(context.Background(), keys, false); err == nil || strings.Contains(err.Error(), "a-forwarding-token") {
		t.Fatalf("a hand-off that does not read: %v", err)
	}

	// the local backend sets forwarding up itself
	local := &Sidecar{}
	if _, err := local.relayForwardingSetup(context.Background(), keys, false); !errors.Is(err, errSidecarNotRemote) {
		t.Fatalf("a relay on no remote backend: %v", err)
	}
}

// The names and shapes this sdk spells for the js leg are the daemon's own.
func TestRemoteWireMatchesTheDaemon(t *testing.T) {
	names := map[string]string{
		sidecarControlMethodPair:              sidecar.DaemonMethodPair,
		sidecarControlMethodForwardingSetup:   sidecar.DaemonMethodForwardingSetup,
		sidecarControlMethodForwardingWitness: sidecar.DaemonMethodForwardingWitness,
		sidecarRemoteStateLocked:              string(sidecar.SidecarStateLocked),
		sidecarRemoteStateUnenrolled:          string(sidecar.SidecarStateUnenrolled),
		sidecarRemoteStateOffline:             string(sidecar.SidecarStateOffline),
		sidecarRemoteStateOnline:              string(sidecar.SidecarStateOnline),
		sidecarRemoteStateRevoked:             string(sidecar.SidecarStateRevoked),
		sidecarRemoteReasonSigned:             string(sidecar.SidecarReasonSigned),
		sidecarRemoteReasonRefused:            string(sidecar.SidecarReasonRefused),
	}
	for own, daemons := range names {
		if own != daemons {
			t.Errorf("%q is the daemon's %q", own, daemons)
		}
	}
	if sidecarControlCodeConfirmationRequired != sidecar.DaemonRpcConfirmationRequired {
		t.Errorf("the confirmation code %d is the daemon's %d", sidecarControlCodeConfirmationRequired, sidecar.DaemonRpcConfirmationRequired)
	}
	// each message through the other side's type and back, losing nothing
	crossings := []struct {
		name    string
		own     any
		daemons any
		back    any
	}{
		{"pair params", &sidecarControlPairParams{ChildJwt: "a-child"}, &sidecar.DaemonPairParams{}, &sidecarControlPairParams{}},
		{"pair reply", &sidecarControlPairReply{NetworkId: NewId(), Generation: 7}, &sidecar.DaemonPairReply{}, &sidecarControlPairReply{}},
		{"setup params", &sidecarControlForwardingSetupParams{Network: "a-network", Keys: []byte{1, 2}, ConfirmAfterRevocation: true}, &sidecar.DaemonForwardingSetupParams{}, &sidecarControlForwardingSetupParams{}},
		{"setup reply", &sidecarControlForwardingSetupReply{HandOff: []byte{3}}, &sidecar.DaemonForwardingSetupReply{}, &sidecarControlForwardingSetupReply{}},
		{"witness params", &sidecarControlNetworkParams{Network: "a-network"}, &sidecar.DaemonForwardingWitnessParams{}, &sidecarControlNetworkParams{}},
		{"witness reply", &sidecarControlForwardingWitnessReply{Pin: []byte{4}, Witnessed: true, Fork: true, SetupDue: "due"}, &sidecar.DaemonForwardingWitnessReply{}, &sidecarControlForwardingWitnessReply{}},
	}
	for _, crossing := range crossings {
		first, err := json.Marshal(crossing.own)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(first))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(crossing.daemons); err != nil {
			t.Errorf("%s: the daemon's type does not take it: %v", crossing.name, err)
			continue
		}
		second, err := json.Marshal(crossing.daemons)
		if err != nil {
			t.Fatal(err)
		}
		decoder = json.NewDecoder(bytes.NewReader(second))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(crossing.back); err != nil {
			t.Errorf("%s: this sdk's type does not take the daemon's: %v", crossing.name, err)
			continue
		}
		third, err := json.Marshal(crossing.back)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, third) {
			t.Errorf("%s changed across the daemon's type: %s to %s", crossing.name, first, third)
		}
	}
}

func TestRemoteSettingsAndClose(t *testing.T) {
	// no wrapper is bound over the daemon yet: a named component refuses rather than open without it
	for _, name := range sidecarComponentNames {
		settings := DefaultSidecarSettings()
		settings.Components.Add(name)
		if _, err := newSidecarRemote(context.Background(), nil, settings, &sidecarRemoteOptions{noStatusPoll: true}); err == nil {
			t.Fatalf("a remote sidecar opened with %s", name)
		}
	}
	relative := DefaultSidecarSettings()
	relative.DaemonSocketDir = "a/relative/dir"
	if _, err := newSidecarRemote(context.Background(), nil, relative, &sidecarRemoteOptions{noStatusPoll: true}); err == nil {
		t.Fatal("a relative DaemonSocketDir was taken")
	}
	if _, err := newSidecarRemote(context.Background(), nil, nil, &sidecarRemoteOptions{}); err == nil {
		t.Fatal("a remote sidecar opened with no settings")
	}
	// a sandboxed app's container holds the instance's short-named socket
	grouped := DefaultSidecarSettings()
	grouped.DaemonSocketDir = "/synthetic/Group Containers/group"
	if path, err := sidecarDaemonSocketPath(grouped); err != nil || path != "/synthetic/Group Containers/group/ur" {
		t.Fatalf("the default instance's app-group socket: %s (%v)", path, err)
	}
	grouped.Instance = "work"
	if path, err := sidecarDaemonSocketPath(grouped); err != nil || path != filepath.Join(grouped.DaemonSocketDir, sidecar.DaemonAppGroupSocketName("work")) {
		t.Fatalf("another instance's app-group socket: %s (%v)", path, err)
	}
	own, err := sidecar.DaemonSocketPath("default")
	if err != nil {
		t.Fatal(err)
	}
	if path, err := sidecarDaemonSocketPath(DefaultSidecarSettings()); err != nil || path != own {
		t.Fatalf("the instance's own socket: %s (%v)", path, err)
	}

	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 1)
	daemon.start()
	remote, backend, _ := openTestRemoteSidecar(t, socketPath)
	if err := backend.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// no wrapper reaches the daemon's engines yet, and no core runs here
	if remote.GetEvs() != nil || remote.GetSockets() != nil || remote.Core() != nil {
		t.Fatal("a remote sidecar answered a wrapper or a core")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	if err := remote.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	remote.Close()
	if err := backend.refresh(context.Background()); !errors.Is(err, errSidecarRemoteClosed) {
		t.Fatalf("a status read after Close: %v", err)
	}
	if err := backend.call(context.Background(), "status", nil, false, nil); !errors.Is(err, errSidecarRemoteClosed) {
		t.Fatalf("a call after Close: %v", err)
	}
	// the session ended at the daemon's end too
	daemon.stop()
	if methods := daemon.seenMethods(); len(methods) != 1 {
		t.Fatalf("the daemon saw %v", methods)
	}
}

// The poll reads the daemon's status on its own, once started.
func TestRemoteStatusPoll(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.set("online", "", 1)
	daemon.start()
	remoteSettings := defaultSidecarRemoteSettings()
	remoteSettings.StatusPollInterval = time.Millisecond
	remote, err := newSidecarRemote(context.Background(), nil, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:           sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion),
		remoteSettings: remoteSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	events := &testRemoteEvents{events: make(chan *SidecarEvent, 64)}
	remote.AddEventListener(events)
	// a listener added after the first read hears the next change instead
	daemon.set("locked", "", 1)
	for {
		if event := events.next(t); event.State == "locked" {
			break
		}
	}
	if state := remote.GetState(); state != "locked" {
		t.Fatalf("the polled state: %s", state)
	}
}

// The top-level declarations of files, by name, each printed without its comments: a type, a constant or variable
// with its value, a function with its body.
func testSidecarDeclarations(t *testing.T, files ...string) map[string]string {
	t.Helper()
	declarations := map[string]string{}
	fileSet := token.NewFileSet()
	printed := func(node any) string {
		out := &bytes.Buffer{}
		if err := printer.Fprint(out, fileSet, node); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, file := range files {
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil {
					declaration.Doc = nil
					declarations[declaration.Name.Name] = printed(declaration)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						spec.Doc, spec.Comment = nil, nil
						declarations[spec.Name.Name] = printed(spec)
					case *ast.ValueSpec:
						spec.Doc, spec.Comment = nil, nil
						for _, name := range spec.Names {
							declarations[name.Name] = printed(spec)
						}
					}
				}
			}
		}
	}
	return declarations
}

// The js leg repeats the bound types the other legs declare beside code that links the sidecar root
// (sidecar_types_js.go): each must be the other legs' own, so an app sees one Sidecar surface on every leg.
func TestJsBoundTypesMatch(t *testing.T) {
	js := testSidecarDeclarations(t, "sidecar_types_js.go")
	native := testSidecarDeclarations(t, "sidecar_types.go", "sidecar_keystore.go", "sidecar_events.go")
	for _, name := range []string{
		"SidecarSettings", "DefaultSidecarSettings", "SidecarResult", "SidecarResultCallback", "SidecarKeyStore", "SidecarSecret",
		"SidecarPresenceSigner", "SidecarEvent", "SidecarErasure", "SidecarEventListener", "sidecarComponentNames",
		"SidecarComponentEvs", "SidecarKeyStoreOk", "SidecarSecretPresent",
	} {
		if _, ok := js[name]; !ok {
			t.Errorf("the js leg declares no %s", name)
		}
	}
	for name, declaration := range js {
		if other, ok := native[name]; !ok || other != declaration {
			t.Errorf("the js leg's %s is not the other legs':\n%s\n%s", name, declaration, other)
		}
	}
}
