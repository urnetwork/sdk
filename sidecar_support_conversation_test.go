//go:build !ios_extension && !js

package sdk

// The apps' support conversation (sidecar_support_conversation.go; DESIGN-COMPUTE §15.3, §28.4): on the remote backend
// every call is one of ur daemon's support methods, the person's acts (send, delete, set) its Confirm methods sent
// confirmed and the reads unconfirmed, each naming the paired network; the listener long-polls support.watch on a
// session of its own until its Sub closes; the method names and params are cli's, read from its source; and a Sidecar
// with no core answers an error from every call. Synthetic values only.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// A Sidecar with neither a local core nor a remote backend, a closed one, answers an error from every support call,
// never an empty conversation or a success.
func TestSidecarSupportOnNoCoreAnswersAnError(t *testing.T) {
	support := (&Sidecar{}).Support()
	if _, err := support.Conversation(""); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Conversation: %v", err)
	}
	if _, err := support.Send(`{"text":"hello"}`); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Send: %v", err)
	}
	if err := support.Delete("100171"); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := support.Settings(); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Settings: %v", err)
	}
	if _, err := support.Logs(""); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Logs: %v", err)
	}
}

// Until G-SUP-service passes the release's switch is off, and the person's message answers that support is reached
// through the app's form (§28.21); the app's support log still renders whole.
func TestSidecarSupportSendAnswersUnavailableWhileTheSwitchIsOff(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	support := local.Support()
	if _, err := support.Send(`{"text":"the app's VPN drops"}`); !errors.Is(err, sidecar.ErrSupportUnavailable) {
		t.Fatalf("Send: %v", err)
	}
	logs, err := support.Logs("")
	if err != nil || logs == "" {
		t.Fatalf("Logs: %q, %v", logs, err)
	}
}

// One call the daemon took.
type testSupportDaemonCall struct {
	method    string
	confirmed bool
	params    json.RawMessage
}

// A daemon answering ur's support methods with replies, recording each call; its own sessions answer support.watch
// from updates, one reply a call, until the call's context ends.
type testSupportDaemon struct {
	stateLock sync.Mutex
	calls     []testSupportDaemonCall
	replies   map[string]any
	refusal   error
	sessions  int
	// support.watch's replies, each one call's
	updates chan []*sidecar.SupportUpdate
	// told once each of its own sessions closes
	sessionClosed chan struct{}
}

func newTestSupportDaemon(replies map[string]any) *testSupportDaemon {
	return &testSupportDaemon{replies: replies, updates: make(chan []*sidecar.SupportUpdate), sessionClosed: make(chan struct{}, 8)}
}

func (self *testSupportDaemon) sidecarId() *Id {
	return nil
}

func (self *testSupportDaemon) state() (string, string) {
	return "online", ""
}

func (self *testSupportDaemon) generation() int64 {
	return 1
}

func (self *testSupportDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testSupportDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("this daemon pairs nothing")
}

func (self *testSupportDaemon) network() string {
	return "the paired network"
}

// Records one call, answering the daemon's refusal where one is set.
func (self *testSupportDaemon) record(method string, params any, confirmed bool) error {
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, testSupportDaemonCall{method: method, confirmed: confirmed, params: paramsJson})
	return self.refusal
}

func (self *testSupportDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	if err := self.record(method, params, confirmed); err != nil {
		return err
	}
	self.stateLock.Lock()
	reply, ok := self.replies[method]
	self.stateLock.Unlock()
	if !ok {
		return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
	}
	if result == nil {
		return nil
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

func (self *testSupportDaemon) close() {
}

// A session of the listener's own, answering support.watch alone.
func (self *testSupportDaemon) dialOwn(ctx context.Context) (sidecarControlChannel, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sessions += 1
	return &testSupportDaemonSession{daemon: self}, nil
}

// The calls taken so far.
func (self *testSupportDaemon) taken() []testSupportDaemonCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// One of the daemon's sessions dialed for a long poll.
type testSupportDaemonSession struct {
	daemon *testSupportDaemon
}

func (self *testSupportDaemonSession) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	if err := self.daemon.record(method, params, confirmed); err != nil {
		return err
	}
	if method != sidecarControlMethodSupportWatch {
		return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case updates := <-self.daemon.updates:
		lines := []*sidecarControlSupportWatchLine{}
		for _, update := range updates {
			lines = append(lines, &sidecarControlSupportWatchLine{Update: update})
		}
		replyJson, err := json.Marshal(&sidecarControlSupportWatchReply{Updates: lines})
		if err != nil {
			return err
		}
		return json.Unmarshal(replyJson, result)
	}
}

func (self *testSupportDaemonSession) close() {
	self.daemon.sessionClosed <- struct{}{}
}

// The params a call sent, decoded.
func testSupportParamsOf(t *testing.T, call testSupportDaemonCall) *sidecarControlSupportParams {
	t.Helper()
	params := &sidecarControlSupportParams{}
	if err := json.Unmarshal(call.params, params); err != nil {
		t.Fatal(err)
	}
	return params
}

// The newest call taken.
func (self *testSupportDaemon) last(t *testing.T) testSupportDaemonCall {
	t.Helper()
	calls := self.taken()
	if len(calls) == 0 {
		t.Fatal("no call taken")
	}
	return calls[len(calls)-1]
}

// On the remote backend each call is the daemon's support method: the reads unconfirmed, the person's send, delete and
// settings change its Confirm methods sent confirmed, each naming the paired network; the daemon's answers reach the
// app as the local core's would, its refusal as the app's error.
func TestSidecarSupportRemoteCallsTheDaemonsSupportMethods(t *testing.T) {
	stub := &sidecar.SupportItemStub{Entry: "100171", Status: "in_progress", Senders: []string{"you", "release-bot"}, Messages: 2, Replies: 1}
	read := &sidecar.SupportReadResult{Entry: "100171", Status: "in_progress", Messages: []*sidecar.SupportReadMessage{
		{Seq: 1, From: "you", Tier: sidecar.SupportTierHuman, Text: "the VPN drops after sleep"},
		{Seq: 2, From: "support", Text: "Thanks: we are looking at it.", Reply: true},
	}}
	daemon := newTestSupportDaemon(map[string]any{
		sidecarControlMethodSupportList:     &sidecarControlSupportListReply{Items: []*sidecar.SupportItemStub{stub, {Status: "sending", Waiting: []string{"0123"}}}},
		sidecarControlMethodSupportShow:     read,
		sidecarControlMethodSupportSend:     &sidecarSupportSendAnswer{Result: &sidecar.SupportSendResult{Outcome: sidecar.SupportOutcomeQueued, ClientRef: "abcd"}},
		sidecarControlMethodSupportDelete:   nil,
		sidecarControlMethodSupportSettings: &sidecar.SupportPersonKey{Agents: true, AgentLogs: false},
		sidecarControlMethodSupportSet:      &sidecar.SupportPersonKey{Agents: false, AgentLogs: true},
		sidecarControlMethodSupportLogs: &sidecarControlSupportLogsReply{Document: &sidecar.SupportLogBundleDocument{
			Format: "urnetwork-support-log-bundle-v1", Header: &sidecar.SupportLogBundleHeader{},
		}},
	})
	support := (&Sidecar{remote: daemon}).Support()

	conversationJson, err := support.Conversation(`{"status":"in_progress"}`)
	if err != nil {
		t.Fatal(err)
	}
	conversation := &sidecarSupportConversation{}
	if err := json.Unmarshal([]byte(conversationJson), conversation); err != nil {
		t.Fatal(err)
	}
	if len(conversation.Items) != 2 || len(conversation.Items[0].Messages) != 2 || !conversation.Items[0].Messages[1].Reply ||
		len(conversation.Items[1].Messages) != 0 {
		t.Fatalf("the conversation: %s", conversationJson)
	}
	calls := daemon.taken()
	if len(calls) != 2 || calls[0].method != sidecarControlMethodSupportList || calls[0].confirmed ||
		calls[1].method != sidecarControlMethodSupportShow || calls[1].confirmed {
		t.Fatalf("it called %+v", calls)
	}
	if listed := testSupportParamsOf(t, calls[0]); listed.Network != "the paired network" || listed.Status != "in_progress" {
		t.Fatalf("support.ls took %+v", listed)
	}
	if shown := testSupportParamsOf(t, calls[1]); shown.Entry != "100171" || shown.Network != "the paired network" {
		t.Fatalf("support.show took %+v", shown)
	}

	answerJson, err := support.Send(`{"text":"it drops again","entry":"100171","kind":"bug","logs":true,"files":[{"name":"vpn.log","data":"c3RlcCAz"}],"secret_confirmed":true}`)
	if err != nil {
		t.Fatal(err)
	}
	answer := &sidecarSupportSendAnswer{}
	if err := json.Unmarshal([]byte(answerJson), answer); err != nil || answer.Result == nil || answer.Result.ClientRef != "abcd" {
		t.Fatalf("Send answered %s: %v", answerJson, err)
	}
	sent := daemon.last(t)
	sentParams := testSupportParamsOf(t, sent)
	if sent.method != sidecarControlMethodSupportSend || !sent.confirmed || sentParams.Text != "it drops again" || sentParams.Entry != "100171" ||
		sentParams.Kind != "bug" || !sentParams.Logs || !sentParams.SecretConfirmed || len(sentParams.Files) != 1 ||
		sentParams.Files[0].Name != "vpn.log" || string(sentParams.Files[0].Data) != "step 3" || sentParams.Network != "the paired network" {
		t.Fatalf("support.send took %+v confirmed %v", sentParams, sent.confirmed)
	}

	if err := support.Delete("100171"); err != nil {
		t.Fatal(err)
	}
	if deleted := daemon.last(t); deleted.method != sidecarControlMethodSupportDelete || !deleted.confirmed || testSupportParamsOf(t, deleted).Entry != "100171" {
		t.Fatalf("support.delete took %+v", deleted)
	}

	settingsJson, err := support.Settings()
	if err != nil || settingsJson != `{"agents":true,"agent_logs":false}` {
		t.Fatalf("Settings: %s, %v", settingsJson, err)
	}
	if read := daemon.last(t); read.method != sidecarControlMethodSupportSettings || read.confirmed {
		t.Fatalf("support.settings took %+v", read)
	}
	if err := support.SetSettings(`{"agents":false,"agent_logs":true}`); err != nil {
		t.Fatal(err)
	}
	set := daemon.last(t)
	if setParams := testSupportParamsOf(t, set); set.method != sidecarControlMethodSupportSet || !set.confirmed || setParams.Agents != "off" || setParams.AgentLogs != "on" {
		t.Fatalf("support.set took %+v confirmed %v", setParams, set.confirmed)
	}

	logsJson, err := support.Logs(`{"sent":"abcd"}`)
	if err != nil || !strings.Contains(logsJson, "urnetwork-support-log-bundle-v1") {
		t.Fatalf("Logs: %s, %v", logsJson, err)
	}
	logs := daemon.last(t)
	logsParams := &sidecarControlSupportLogsParams{}
	if err := json.Unmarshal(logs.params, logsParams); err != nil || logs.method != sidecarControlMethodSupportLogs || logs.confirmed ||
		*logsParams != (sidecarControlSupportLogsParams{Network: "the paired network", Sent: "abcd"}) {
		t.Fatalf("support.logs took %+v", logsParams)
	}

	// the daemon's refusal is the app's error, and a listener is refused where the daemon refuses support
	daemon.stateLock.Lock()
	daemon.refusal = &sidecarControlError{Code: -32000, Message: "support is not available in this release: reach support through the app's form"}
	daemon.stateLock.Unlock()
	if _, err := support.Send(`{"text":"hello"}`); err == nil || err.Error() != daemon.refusal.Error() {
		t.Fatalf("the daemon's refusal: %v", err)
	}
	if _, err := support.AddListener(&testSupportListener{}); err == nil {
		t.Fatal("a listener added where the daemon refuses support")
	}
}

// A listener's updates, each its JSON, and a signal for each.
type testSupportListener struct {
	stateLock sync.Mutex
	updates   []string
	heard     chan struct{}
}

func (self *testSupportListener) OnUpdate(updateJson string) {
	self.stateLock.Lock()
	self.updates = append(self.updates, updateJson)
	self.stateLock.Unlock()
	if self.heard != nil {
		self.heard <- struct{}{}
	}
}

// The remote listener long-polls support.watch on a session of its own, never the backend's call session, telling
// each update's enumerated fields in order; closing its Sub ends the poll and closes the session.
func TestSidecarSupportRemoteListenerWatchesOnItsOwnSession(t *testing.T) {
	daemon := newTestSupportDaemon(map[string]any{sidecarControlMethodSupportSettings: &sidecar.SupportPersonKey{Agents: true, AgentLogs: true}})
	support := (&Sidecar{remote: daemon}).Support()
	listener := &testSupportListener{heard: make(chan struct{}, 8)}
	sub, err := support.AddListener(listener)
	if err != nil {
		t.Fatal(err)
	}
	daemon.updates <- []*sidecar.SupportUpdate{
		{Product: "urnetwork", Entry: "100171", Seq: 2, Type: "note", Status: "in_progress", Agents: []string{"release-bot"}},
		{Product: "urnetwork", Entry: "100171", Seq: 3, Type: "status", Status: "fixed", FixedIn: "2026.11.24-1234"},
	}
	for range 2 {
		select {
		case <-listener.heard:
		case <-time.After(30 * time.Second):
			t.Fatal("the listener heard nothing")
		}
	}
	listener.stateLock.Lock()
	heard := slices.Clone(listener.updates)
	listener.stateLock.Unlock()
	if len(heard) != 2 || !strings.Contains(heard[0], `"seq":2`) || !strings.Contains(heard[1], `"fixed_in":"2026.11.24-1234"`) {
		t.Fatalf("the listener heard %v", heard)
	}
	sub.Close()
	select {
	case <-daemon.sessionClosed:
	case <-time.After(30 * time.Second):
		t.Fatal("the watch's session stayed open after its Sub closed")
	}
	calls := daemon.taken()
	watches := 0
	for _, call := range calls {
		if call.method == sidecarControlMethodSupportWatch {
			watches += 1
			if call.confirmed || testSupportParamsOf(t, call).Network != "the paired network" {
				t.Fatalf("support.watch took %+v", call)
			}
		}
	}
	daemon.stateLock.Lock()
	sessions := daemon.sessions
	daemon.stateLock.Unlock()
	if watches < 1 || sessions != 1 || calls[0].method != sidecarControlMethodSupportSettings {
		t.Fatalf("%d watches on %d sessions after %+v", watches, sessions, calls)
	}
}

// The control methods and params the remote backend sends and reads are cli's ur support's, read from its source since
// cli is package main, so a renamed method or field fails here rather than at the daemon, which reads params strictly.
func TestSupportControlWireMatchesUr(t *testing.T) {
	cliDirectory := filepath.Join("..", "sidecar", "cli")
	cli := testSidecarDeclarations(t, filepath.Join(cliDirectory, "cmd_support_conversation.go"), filepath.Join(cliDirectory, "cmd_support.go"))
	for own, name := range map[string]string{
		sidecarControlMethodSupportList:     "urMethodSupportList",
		sidecarControlMethodSupportShow:     "urMethodSupportShow",
		sidecarControlMethodSupportSend:     "urMethodSupportSend",
		sidecarControlMethodSupportDelete:   "urMethodSupportDelete",
		sidecarControlMethodSupportSettings: "urMethodSupportSettings",
		sidecarControlMethodSupportSet:      "urMethodSupportSet",
		sidecarControlMethodSupportWatch:    "urMethodSupportWatch",
		sidecarControlMethodSupportLogs:     "urMethodSupportLogs",
	} {
		if !strings.Contains(cli[name], strconv.Quote(own)) {
			t.Errorf("%s is not %q: %s", name, own, cli[name])
		}
	}
	for declaration, fields := range map[string][]string{
		"urSupportParams": {`json:"network,omitempty"`, `json:"entry,omitempty"`, `json:"status,omitempty"`, `json:"text,omitempty"`,
			`json:"kind,omitempty"`, `json:"logs,omitempty"`, `json:"files,omitempty"`, `json:"secret_confirmed,omitempty"`,
			`json:"agents,omitempty"`, `json:"agent_logs,omitempty"`},
		"urSupportFile":       {`json:"name"`, `json:"data"`},
		"urSupportLogsParams": {`json:"network,omitempty"`, `json:"sent,omitempty"`},
		"urSupportLogsReply":  {`json:"manifest"`, `json:"document"`},
		"urSupportListReply":  {`json:"items"`},
		"urSupportWatchReply": {`json:"updates"`},
		"urSupportWatchLine":  {`json:"update"`},
		"urSupportSendReply":  {`json:"result,omitempty"`, `json:"refusal,omitempty"`, `json:"detail,omitempty"`},
	} {
		for _, field := range fields {
			if !strings.Contains(cli[declaration], field) {
				t.Errorf("%s lacks %s", declaration, field)
			}
		}
	}
}

// Every exported signature the file adds is one gomobile binds (sidecar_accessors_compute_test.go's check).
func TestSupportConversationSignaturesAreGomobileSafe(t *testing.T) {
	findings := testComputeSignatureFindings(t, []string{"sidecar_support_conversation.go"}, map[string]string{"Sidecar": "struct", "Sub": "interface"})
	for _, finding := range findings {
		t.Error(finding)
	}
}
