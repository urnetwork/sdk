//go:build !ios_extension && !js

package sdk

// Agents() for the apps (sidecar_agents.go; SKILL-M11-08; DESIGN-COMPUTE §15.3, §20.8): on the remote backend each
// method calls ur daemon's agents method with the request's JSON mapped to its params, the person's sends confirmed
// and the reads asking for every text, the daemon's answer returned as it wrote it; a listener hears each message, its
// notification and a change from agents.watch's long poll, in order, from the cursor the first call gave; a malformed
// request and a Sidecar with no core answer errors; and every exported signature is one gomobile binds.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// A daemon answering agents.watch from a queue of replies, after its first, recording each call.
type testAgentsWatchDaemon struct {
	*testComputeDaemon
	stateLock sync.Mutex
	watches   []json.RawMessage
	replies   chan *sidecarControlAgentsWatchReply
}

func (self *testAgentsWatchDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	if method != sidecarControlMethodAgentsWatch {
		return self.testComputeDaemon.call(ctx, method, params, confirmed, result)
	}
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	first := len(self.watches) == 0
	self.watches = append(self.watches, paramsJson)
	self.stateLock.Unlock()
	reply := &sidecarControlAgentsWatchReply{Cursor: "7"}
	if !first {
		select {
		case reply = <-self.replies:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

// What a listener heard, in order.
type testAgentsListener struct {
	heard chan string
}

func (self *testAgentsListener) OnThreadMessage(messageJson string) {
	self.heard <- "message " + messageJson
}

func (self *testAgentsListener) OnNotification(notificationJson string) {
	self.heard <- "notification " + notificationJson
}

func (self *testAgentsListener) OnAgentsChanged() {
	self.heard <- "changed"
}

// The next thing the listener heard, failing past a guard.
func (self *testAgentsListener) next(t *testing.T) string {
	t.Helper()
	select {
	case heard := <-self.heard:
		return heard
	case <-time.After(30 * time.Second):
		t.Fatal("the listener heard nothing")
		return ""
	}
}

// The params of the calls of method, decoded into fresh values of T.
func testAgentsParams[T any](t *testing.T, daemon *testComputeDaemon, method string) []*T {
	t.Helper()
	params := []*T{}
	for _, call := range daemon.taken() {
		if call.method != method {
			continue
		}
		decoded := new(T)
		decoder := json.NewDecoder(strings.NewReader(string(call.params)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(decoded); err != nil {
			t.Fatal(err)
		}
		params = append(params, decoded)
	}
	return params
}

func TestSidecarAgentsRemoteJsonRoundTrips(t *testing.T) {
	list := &sidecar.AgentList{Agents: []*sidecar.AgentListEntry{{Name: "fix", Host: "dana-laptop", State: "working", Trusted: true}}}
	sendReply := &sidecarAgentsSendReply{AgentSendToolResult: &sidecar.AgentSendToolResult{Id: "aa", Thread: "bb", Seq: 1, Kind: "message", To: "fix"}, Person: true}
	inbox := &sidecar.AgentPersonInbox{Threads: []*sidecar.AgentPersonThread{{Thread: "bb", Repo: "cc", Messages: []*sidecar.AgentInboxMessage{{Thread: "bb", Seq: 1, From: "person"}}}}}
	graph := &sidecar.AgentsGraph{Nodes: []*sidecar.AgentsGraphNode{{AgentListEntry: &sidecar.AgentListEntry{Name: "fix"}}},
		Edges: []*sidecar.AgentsGraphEdge{{Kind: sidecar.AgentsGraphEdgeLaunch, From: "fix", To: "child"}}}
	daemon := newTestComputeDaemon(map[string]any{
		sidecarControlMethodAgentsList: list, sidecarControlMethodAgentsSend: sendReply,
		sidecarControlMethodAgentsInbox: inbox, sidecarControlMethodAgentsGraph: graph,
	})
	agents := newSidecarAgents(&sidecarComputeArgs{remote: daemon})

	listJson, err := agents.List()
	if err != nil {
		t.Fatal(err)
	}
	readList := &sidecar.AgentList{}
	if err := json.Unmarshal([]byte(listJson), readList); err != nil || len(readList.Agents) != 1 || readList.Agents[0].Host != "dana-laptop" {
		t.Fatalf("List: %s, %v", listJson, err)
	}
	listed := testAgentsParams[sidecarControlAgentsListParams](t, daemon, sidecarControlMethodAgentsList)
	if len(listed) != 1 || listed[0].Untrusted != sidecarAgentsUntrustedInclude || listed[0].Network != "the paired network" {
		t.Fatalf("List's params: %+v", listed)
	}

	sentJson, err := agents.Send(`{"to":"fix","subject":"status?","body":"How close?"}`)
	if err != nil {
		t.Fatal(err)
	}
	readSent := &sidecarAgentsSendReply{}
	if err := json.Unmarshal([]byte(sentJson), readSent); err != nil || !readSent.Person || readSent.To != "fix" {
		t.Fatalf("Send: %s, %v", sentJson, err)
	}
	if _, err := agents.Broadcast(`{"body":"Wrap up."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.Broadcast(`{"body":"Thanks, all.","thread":"bb"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.Send(`{"thread":"bb","close":true}`); err != nil {
		t.Fatal(err)
	}
	sends := testAgentsParams[sidecarControlAgentsSendParams](t, daemon, sidecarControlMethodAgentsSend)
	if len(sends) != 4 || sends[0].To != "fix" || sends[0].Body != "How close?" || !sends[1].Broadcast || sends[2].Broadcast ||
		sends[2].To != "*" || sends[2].Thread != "bb" || !sends[3].Close {
		t.Fatalf("the sends' params: %+v", sends)
	}
	for _, call := range daemon.taken() {
		if call.method == sidecarControlMethodAgentsSend && !call.confirmed {
			t.Fatal("the person's send went unconfirmed")
		}
		if call.method != sidecarControlMethodAgentsSend && call.confirmed {
			t.Fatalf("%s went confirmed", call.method)
		}
	}

	inboxJson, err := agents.Inbox(`{"thread":"bb"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inboxJson, `"thread":"bb"`) {
		t.Fatalf("Inbox: %s", inboxJson)
	}
	graphJson, err := agents.Graph(`{"retired":true,"agent":"fix"}`)
	if err != nil {
		t.Fatal(err)
	}
	readGraph := &sidecar.AgentsGraph{}
	if err := json.Unmarshal([]byte(graphJson), readGraph); err != nil || len(readGraph.Edges) != 1 || readGraph.Nodes[0].Name != "fix" {
		t.Fatalf("Graph: %s, %v", graphJson, err)
	}
	graphed := testAgentsParams[sidecarControlAgentsGraphParams](t, daemon, sidecarControlMethodAgentsGraph)
	if len(graphed) != 1 || !graphed[0].Retired || graphed[0].Agent != "fix" || graphed[0].Untrusted != sidecarAgentsUntrustedInclude {
		t.Fatalf("Graph's params: %+v", graphed)
	}

	// requests the methods cannot take, and a Sidecar with no core
	for _, request := range []func() (string, error){
		func() (string, error) { return agents.Send(`{"to":"fix","body":"x","extra":1}`) },
		func() (string, error) { return agents.Send(`{"body":"no recipient"}`) },
		func() (string, error) { return agents.Graph(`not json`) },
	} {
		if _, err := request(); !errors.Is(err, errSidecarAgentsRequest) {
			t.Errorf("a malformed request: %v", err)
		}
	}
	bare := newSidecarAgents(&sidecarComputeArgs{})
	if _, err := bare.List(); !errors.Is(err, errSidecarAgentsNoCore) {
		t.Errorf("no core: %v", err)
	}
	if _, err := bare.AddListener(&testAgentsListener{heard: make(chan string, 1)}); !errors.Is(err, errSidecarAgentsNoCore) {
		t.Errorf("no core's listener: %v", err)
	}
}

func TestSidecarAgentsListenerDelivery(t *testing.T) {
	daemon := &testAgentsWatchDaemon{testComputeDaemon: newTestComputeDaemon(map[string]any{}), replies: make(chan *sidecarControlAgentsWatchReply, 4)}
	agents := newSidecarAgents(&sidecarComputeArgs{remote: daemon})
	listener := &testAgentsListener{heard: make(chan string, 16)}
	sub, err := agents.AddListener(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	message := &sidecar.AgentThreadMessage{ThreadHex: "bb", IdHex: "aa", Seq: 2, Kind: sidecar.AgentMessageKindReply, SenderName: "fix",
		Trust: sidecar.AgentMessageTrustMemberRun, Subject: "done"}
	messageJson, _ := json.Marshal(message)
	notification := &sidecar.AgentNotification{Thread: "bb", Id: "aa", Sender: "fix", To: "me", Subject: "done", ToPerson: true}
	notificationJson, _ := json.Marshal(notification)
	daemon.replies <- &sidecarControlAgentsWatchReply{Cursor: "8", Messages: []*sidecarControlAgentsWatchMessage{
		{Message: messageJson, Notification: notificationJson},
	}}
	daemon.replies <- &sidecarControlAgentsWatchReply{Cursor: "9", Changed: true}
	if heard := listener.next(t); heard != "message "+string(messageJson) {
		t.Fatalf("first: %s", heard)
	}
	if heard := listener.next(t); heard != "notification "+string(notificationJson) {
		t.Fatalf("second: %s", heard)
	}
	if heard := listener.next(t); heard != "changed" {
		t.Fatalf("third: %s", heard)
	}
	// each poll carried the cursor the last answer gave, from the first call's
	daemon.stateLock.Lock()
	watches := append([]json.RawMessage{}, daemon.watches...)
	daemon.stateLock.Unlock()
	cursors := []string{}
	for _, watch := range watches {
		params := &sidecarControlAgentsWatchParams{}
		if err := json.Unmarshal(watch, params); err != nil {
			t.Fatal(err)
		}
		cursors = append(cursors, params.Cursor)
	}
	if len(cursors) < 3 || cursors[0] != "" || cursors[1] != "7" || cursors[2] != "8" {
		t.Fatalf("the cursors: %v", cursors)
	}
}

// Every exported method of SidecarAgents takes and answers only what gomobile binds: strings, errors, the listener and
// Sub; and the listener's methods take strings alone.
func TestSidecarAgentsGomobileSignatures(t *testing.T) {
	allowed := map[reflect.Type]bool{
		reflect.TypeOf(""): true, reflect.TypeOf((*error)(nil)).Elem(): true, reflect.TypeOf((*Sub)(nil)).Elem(): true,
		reflect.TypeOf((*SidecarAgentsListener)(nil)).Elem(): true,
	}
	agentsType := reflect.TypeOf(&SidecarAgents{})
	for i := 0; i < agentsType.NumMethod(); i += 1 {
		method := agentsType.Method(i)
		for j := 1; j < method.Type.NumIn(); j += 1 {
			if !allowed[method.Type.In(j)] {
				t.Errorf("%s takes %s", method.Name, method.Type.In(j))
			}
		}
		for j := 0; j < method.Type.NumOut(); j += 1 {
			if !allowed[method.Type.Out(j)] {
				t.Errorf("%s answers %s", method.Name, method.Type.Out(j))
			}
		}
	}
	listenerType := reflect.TypeOf((*SidecarAgentsListener)(nil)).Elem()
	for i := 0; i < listenerType.NumMethod(); i += 1 {
		method := listenerType.Method(i)
		for j := 0; j < method.Type.NumIn(); j += 1 {
			if method.Type.In(j) != reflect.TypeOf("") {
				t.Errorf("the listener's %s takes %s", method.Name, method.Type.In(j))
			}
		}
	}
}
