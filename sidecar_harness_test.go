//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// The model the sdk Harness tests talk to.
const sidecarHarnessTestModel = "engy-glm-5.2-high"

// The longest a test waits on a listener or a run: a bound on a broken test, which proves nothing.
const sidecarHarnessTestTimeout = 60 * time.Second

// A router a test scripts: each call answers the next script's events, after its gate where one is given.
type sidecarHarnessTestRouter struct {
	catalog *sidecar.InferCatalog
	// a refusal Check and Infer answer before any script, nil for none
	refuse func(request *sidecar.InferRequest) error

	stateLock sync.Mutex
	scripts   [][]*sidecar.InferEvent
	calls     int
	called    chan struct{}
}

func newSidecarHarnessTestRouter(t *testing.T) *sidecarHarnessTestRouter {
	t.Helper()
	entry := &sidecar.InferCatalogEntry{
		Provider: "engy", Model: "glm-5.2", Kind: sidecar.InferCatalogKindTee, Label: sidecar.ComputeLabelA1,
		Efforts: []sidecar.InferEffort{sidecar.InferEffortHigh}, DefaultEffort: sidecar.InferEffortHigh,
		ContextWindow: 128 * 1024, MaxOutput: 32 * 1024, Tools: true, StructuredOutput: true,
		Modalities: []sidecar.InferModality{sidecar.InferModalityText}, License: "Apache-2.0",
		Remote: &sidecar.InferRemoteModel{ProviderModelId: "glm-5.2"},
	}
	catalog, err := sidecar.NewInferCatalog([]*sidecar.InferCatalogEntry{entry}, sidecar.DefaultInferSettings())
	if err != nil {
		t.Fatal(err)
	}
	return &sidecarHarnessTestRouter{catalog: catalog, called: make(chan struct{}, 64)}
}

// Scripts the next calls.
func (self *sidecarHarnessTestRouter) script(scripts ...[]*sidecar.InferEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.scripts = append(self.scripts, scripts...)
}

// The calls made so far.
func (self *sidecarHarnessTestRouter) callCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls
}

func (self *sidecarHarnessTestRouter) Catalog(ctx context.Context) (*sidecar.InferCatalog, error) {
	return self.catalog, nil
}

func (self *sidecarHarnessTestRouter) Infer(ctx context.Context, request *sidecar.InferRequest) (<-chan *sidecar.InferEvent, error) {
	if self.refuse != nil {
		if err := self.refuse(request); err != nil {
			return nil, err
		}
	}
	events := func() []*sidecar.InferEvent {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.calls += 1
		if len(self.scripts) == 0 {
			return nil
		}
		events := self.scripts[0]
		self.scripts = self.scripts[1:]
		return events
	}()
	self.called <- struct{}{}
	if events == nil {
		return nil, &sidecar.InferError{Code: sidecar.InferErrorEngineFailed, Message: "no call is scripted"}
	}
	out := make(chan *sidecar.InferEvent, len(events)+1)
	for _, event := range events {
		out <- event
	}
	close(out)
	return out, nil
}

func (self *sidecarHarnessTestRouter) Check(ctx context.Context, request *sidecar.InferRequest) error {
	if self.refuse != nil {
		return self.refuse(request)
	}
	return nil
}

// A text answer, its text in deltas.
func sidecarHarnessTestText(deltas ...string) []*sidecar.InferEvent {
	events := []*sidecar.InferEvent{{Kind: sidecar.InferEventStart}}
	for _, delta := range deltas {
		events = append(events, &sidecar.InferEvent{Kind: sidecar.InferEventText, Text: delta})
	}
	return append(events, &sidecar.InferEvent{Kind: sidecar.InferEventUsage, Usage: &sidecar.InferUsage{InputTokens: 10, OutputTokens: 2}},
		&sidecar.InferEvent{Kind: sidecar.InferEventEnd})
}

// One tool call.
func sidecarHarnessTestToolCall(id string, name string, arguments string) []*sidecar.InferEvent {
	return []*sidecar.InferEvent{
		{Kind: sidecar.InferEventStart},
		{Kind: sidecar.InferEventToolCallStart, ToolCall: &sidecar.InferToolCall{Id: id, Name: name}},
		{Kind: sidecar.InferEventToolCallEnd, ToolCall: &sidecar.InferToolCall{Id: id, Name: name, Arguments: arguments}},
		{Kind: sidecar.InferEventUsage, Usage: &sidecar.InferUsage{InputTokens: 10, OutputTokens: 2}},
		{Kind: sidecar.InferEventEnd},
	}
}

// A journal a test reads back: each node kept in memory, each request the run's last user and tool texts.
type sidecarHarnessTestJournal struct {
	stateLock sync.Mutex
	nodes     []*sidecar.HarnessJournalNode
}

func (self *sidecarHarnessTestJournal) Journal(ctx context.Context, node *sidecar.HarnessJournalNode) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nodes = append(self.nodes, node)
	return nil
}

func (self *sidecarHarnessTestJournal) Render(ctx context.Context, args *sidecar.HarnessRenderArgs) (*sidecar.InferRequest, error) {
	return &sidecar.InferRequest{Model: args.Model, Messages: []*sidecar.InferMessage{{Role: sidecar.InferRoleUser,
		Parts: []*sidecar.InferPart{{Kind: sidecar.InferPartText, Text: "go"}}}}}, nil
}

func (self *sidecarHarnessTestJournal) TurnEnd(ctx context.Context, runId sidecar.Id) error {
	return nil
}

// What the journal holds, as JSON lines.
func (self *sidecarHarnessTestJournal) text(t *testing.T) string {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{}
	for _, node := range self.nodes {
		line, err := node.Entry.Json()
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	return strings.Join(lines, "\n")
}

// The recURd skill as these tests fix it.
type sidecarHarnessTestSkill struct{}

func (sidecarHarnessTestSkill) Text(profile sidecar.HarnessProfile) (string, error) {
	return "You are an agent of the person's network.", nil
}

func (sidecarHarnessTestSkill) Register(ctx context.Context, registration *sidecar.HarnessAgentRegistration) (string, error) {
	if registration.Name == "" {
		return "agent", nil
	}
	return registration.Name, nil
}

// A tool whose details are a built-in's keep: false (§24.9): a search result the record keeps only as a stub.
type sidecarHarnessTestKeepFalseTool struct{}

func (sidecarHarnessTestKeepFalseTool) Name() string        { return "web_search" }
func (sidecarHarnessTestKeepFalseTool) Description() string { return "Searches." }
func (sidecarHarnessTestKeepFalseTool) Schema() []byte      { return []byte(`{"type":"object"}`) }
func (sidecarHarnessTestKeepFalseTool) Mode() string        { return sidecar.HarnessToolParallel }
func (sidecarHarnessTestKeepFalseTool) Tier() string        { return sidecar.HarnessToolTierRead }

func (sidecarHarnessTestKeepFalseTool) Execute(ctx context.Context, callId string, args []byte, update func(*sidecar.HarnessToolUpdate)) (*sidecar.HarnessToolResult, error) {
	return &sidecar.HarnessToolResult{Text: "3 results", Details: []byte(`{"kind":"web_search","keep":false,"results":["provider-only-text-7f3a"]}`),
		Trust: sidecar.HarnessToolUntrusted}, nil
}

// A clock a test moves: its timers fire only when the test advances past them, on the test's goroutine.
type sidecarHarnessTestClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*sidecarHarnessTestTimer
}

type sidecarHarnessTestTimer struct {
	at       time.Time
	function func()
	stopped  bool
}

func (self *sidecarHarnessTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *sidecarHarnessTestClock) After(timeout time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	self.AfterFunc(timeout, func() { fired <- self.Now() })
	return fired
}

func (self *sidecarHarnessTestClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	timer := &sidecarHarnessTestTimer{at: self.now.Add(timeout), function: function}
	self.timers = append(self.timers, timer)
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		stopped := !timer.stopped
		timer.stopped = true
		return stopped
	}
}

// Moves the clock by, firing each timer it passes.
func (self *sidecarHarnessTestClock) advance(by time.Duration) {
	due := func() []*sidecarHarnessTestTimer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(by)
		due := []*sidecarHarnessTestTimer{}
		kept := []*sidecarHarnessTestTimer{}
		for _, timer := range self.timers {
			switch {
			case timer.stopped:
			case !timer.at.After(self.now):
				timer.stopped = true
				due = append(due, timer)
			default:
				kept = append(kept, timer)
			}
		}
		self.timers = kept
		return due
	}()
	for _, timer := range due {
		timer.function()
	}
}

// A listener collecting each event's JSON.
type sidecarHarnessTestListener struct {
	events chan string
}

func newSidecarHarnessTestListener() *sidecarHarnessTestListener {
	return &sidecarHarnessTestListener{events: make(chan string, 4096)}
}

func (self *sidecarHarnessTestListener) OnEvent(eventJson string) {
	self.events <- eventJson
}

// One event as a test reads it.
type sidecarHarnessTestEvent struct {
	Type                  string          `json:"type"`
	Agent                 string          `json:"agent"`
	Seq                   uint64          `json:"seq"`
	StopReason            string          `json:"stopReason"`
	Method                string          `json:"method"`
	Id                    string          `json:"id"`
	DisclosureId          string          `json:"disclosureId"`
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
	Message               json.RawMessage `json:"message"`
	Result                json.RawMessage `json:"result"`
}

// The events up to and with the first of eventType, failing past the bound.
func (self *sidecarHarnessTestListener) until(t *testing.T, eventType sidecar.HarnessEventType) ([]*sidecarHarnessTestEvent, []string) {
	t.Helper()
	events := []*sidecarHarnessTestEvent{}
	raws := []string{}
	guard := time.After(sidecarHarnessTestTimeout)
	for {
		select {
		case raw := <-self.events:
			event := &sidecarHarnessTestEvent{}
			if err := json.Unmarshal([]byte(raw), event); err != nil {
				t.Fatalf("an event is no JSON: %v: %s", err, raw)
			}
			events = append(events, event)
			raws = append(raws, raw)
			if sidecar.HarnessEventType(event.Type) == eventType {
				return events, raws
			}
		case <-guard:
			t.Fatalf("no %s after %d events", eventType, len(events))
			return nil, nil
		}
	}
}

// The run files as these tests keep them, in memory: what the Harness's runs taint and mark.
type sidecarHarnessTestRuns struct {
	stateLock sync.Mutex
	runs      map[sidecar.Id]*sidecar.RunFile
	reasons   map[sidecar.Id][]sidecar.RunTaintReason
	marks     map[sidecar.Id][]string
}

func newSidecarHarnessTestRuns() *sidecarHarnessTestRuns {
	return &sidecarHarnessTestRuns{runs: map[sidecar.Id]*sidecar.RunFile{}, reasons: map[sidecar.Id][]sidecar.RunTaintReason{},
		marks: map[sidecar.Id][]string{}}
}

func (self *sidecarHarnessTestRuns) Create(ctx context.Context, run *sidecar.RunFile) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.runs[run.RunId]; ok {
		return sidecar.ErrRunExists
	}
	copied := *run
	self.runs[run.RunId] = &copied
	return nil
}

func (self *sidecarHarnessTestRuns) Get(runId sidecar.Id) (*sidecar.RunFile, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	run, ok := self.runs[runId]
	if !ok {
		return nil, sidecar.ErrRunUnknown
	}
	copied := *run
	return &copied, nil
}

func (self *sidecarHarnessTestRuns) Taint(ctx context.Context, runId sidecar.Id, reason sidecar.RunTaintReason) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !slices.Contains(self.reasons[runId], reason) {
		self.reasons[runId] = append(self.reasons[runId], reason)
	}
	return nil
}

func (self *sidecarHarnessTestRuns) Tainted(runId sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.reasons[runId])
}

func (self *sidecarHarnessTestRuns) TaintReasons(runId sidecar.Id) []sidecar.RunTaintReason {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reasons[runId])
}

func (self *sidecarHarnessTestRuns) Mark(ctx context.Context, runId sidecar.Id, mark string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !slices.Contains(self.marks[runId], mark) {
		self.marks[runId] = append(self.marks[runId], mark)
	}
	return nil
}

func (self *sidecarHarnessTestRuns) Marks(runId sidecar.Id) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.marks[runId]), nil
}

// A world for the sdk Harness: a root Harness over the scripted router, the test journal, the skill and a run store,
// and the view over it with its host and clock.
type sidecarHarnessTestWorld struct {
	router    *sidecarHarnessTestRouter
	journal   *sidecarHarnessTestJournal
	harness   *sidecar.Harness
	runs      *sidecarHarnessTestRuns
	clock     *sidecarHarnessTestClock
	directory string
	view      *SidecarHarness
}

func newSidecarHarnessTestWorld(t *testing.T, args *sidecar.HarnessArgs, foreground bool) *sidecarHarnessTestWorld {
	t.Helper()
	runs := newSidecarHarnessTestRuns()
	world := &sidecarHarnessTestWorld{
		router:    newSidecarHarnessTestRouter(t),
		journal:   &sidecarHarnessTestJournal{},
		runs:      runs,
		clock:     &sidecarHarnessTestClock{now: time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)},
		directory: t.TempDir(),
	}
	if args == nil {
		args = &sidecar.HarnessArgs{}
	}
	args.Router = world.router
	args.Journal = world.journal
	args.Skill = sidecarHarnessTestSkill{}
	args.Runs = runs
	if args.Tools == nil {
		args.Tools = map[string]*sidecar.HarnessToolSpec{}
	}
	if args.TurnHooks == nil {
		args.TurnHooks = map[string]sidecar.HarnessTurnHook{}
	}
	harness, err := sidecar.NewHarness(args)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(harness.Close)
	world.harness = harness
	host := &sidecarHarnessHost{workspaceDirectory: world.directory, mobile: true, foreground: foreground}
	world.view = newSidecarHarnessOver(&sidecarComputeArgs{ctx: context.Background()}, harness, host, world.clock)
	return world
}

// A prompt from the app's UI: its events reach the listener as JSON strings, each with its type, agent and seq in
// order, Pi's from agent_start to agent_settled; the run was the person's, so nothing tainted it.
func TestSidecarHarnessPromptEmitsJsonEvents(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, true)
	listener := newSidecarHarnessTestListener()
	sub := world.view.AddListener(listener)
	defer sub.Close()
	agentId, err := world.view.NewAgent(`{"name":"helper","model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	world.router.script(sidecarHarnessTestText("Hello."))
	if err := world.view.Prompt(agentId, "Say hello."); err != nil {
		t.Fatal(err)
	}
	events, _ := listener.until(t, sidecar.HarnessEventAgentSettled)
	types := []string{}
	for i, event := range events {
		types = append(types, event.Type)
		if event.Agent != agentId || event.Seq == 0 || (0 < i && event.Seq <= events[i-1].Seq) {
			t.Fatalf("event %d: agent %q seq %d", i, event.Agent, event.Seq)
		}
	}
	if types[0] != "agent_start" || !slices.Contains(types, "message_end") || events[len(events)-1].StopReason != sidecar.HarnessStopEnded {
		t.Fatalf("the events: %v", types)
	}
	state := []*sidecarHarnessAgentState{}
	answer, err := world.view.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(answer), &state); err != nil || len(state) != 1 || state[0].Agent != agentId || state[0].Name != "helper" ||
		state[0].Model != sidecarHarnessTestModel || state[0].Tainted {
		t.Fatalf("the agents: %s", answer)
	}
}

// RespondUi answers a disclosure request only by declining it: an acknowledgment is refused, the request still waiting,
// and a cancel declines it, failing the call that waits on it with disclosure_required; a confirm is answered whole.
func TestSidecarHarnessRespondUiDeclinesDisclosureOnly(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, &sidecar.HarnessArgs{Acks: &sidecarHarnessTestAcks{}}, true)
	world.router.refuse = func(request *sidecar.InferRequest) error {
		return &sidecar.InferError{Code: sidecar.InferErrorDisclosureRequired, Reason: sidecar.InferDisclosureReasonUnacknowledged,
			Model: request.Model, DisclosureId: "provider-engy", DisclosureVersion: 2}
	}
	listener := newSidecarHarnessTestListener()
	sub := world.view.AddListener(listener)
	defer sub.Close()
	agentId, err := world.view.NewAgent(`{"model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.view.Prompt(agentId, "go"); err != nil {
		t.Fatal(err)
	}
	events, _ := listener.until(t, sidecar.HarnessEventUiRequest)
	request := events[len(events)-1]
	if request.Method != string(sidecar.HarnessUiDisclosure) || request.DisclosureId != "provider-engy" {
		t.Fatalf("the request: %+v", request)
	}
	if err := world.view.RespondUi(request.Id, `{"acknowledged":true}`); !errors.Is(err, errSidecarHarnessAcknowledge) {
		t.Fatalf("an acknowledgment: %v", err)
	}
	if len(world.harness.UiBroker().Pending(mustSidecarHarnessRun(t, world, agentId))) != 1 {
		t.Fatalf("a refused acknowledgment answered the request")
	}
	if err := world.view.RespondUi(request.Id, `{"cancelled":true}`); err != nil {
		t.Fatal(err)
	}
	// the declined call's failure offers its choice, which the person cancels too
	events, _ = listener.until(t, sidecar.HarnessEventUiRequest)
	choice := events[len(events)-1]
	if choice.Method != string(sidecar.HarnessUiSelect) {
		t.Fatalf("after the decline: %+v", choice)
	}
	if err := world.view.RespondUi(choice.Id, `{"cancelled":true}`); err != nil {
		t.Fatal(err)
	}
	events, _ = listener.until(t, sidecar.HarnessEventAgentSettled)
	if events[len(events)-1].StopReason == sidecar.HarnessStopEnded {
		t.Fatalf("a declined disclosure's run ended as if called")
	}
}

// The run an agent of the view runs.
func mustSidecarHarnessRun(t *testing.T, world *sidecarHarnessTestWorld, agentId string) sidecar.Id {
	t.Helper()
	id, err := sidecar.ParseId(agentId)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := world.harness.Agent(id)
	if !ok {
		t.Fatalf("no agent %s", agentId)
	}
	return agent.Run().Id()
}

// An acknowledgment store that never acknowledges: the request waits for the person's own act.
type sidecarHarnessTestAcks struct{}

func (*sidecarHarnessTestAcks) Await(ctx context.Context, id string, version uint32) error {
	<-ctx.Done()
	return ctx.Err()
}

// A built-in tool's keep: false details reach the listener, in memory, and nowhere else: the journal holds none of
// them, and nothing under the app's directory holds them.
func TestSidecarHarnessKeepFalseNotPersisted(t *testing.T) {
	tool := sidecarHarnessTestKeepFalseTool{}
	world := newSidecarHarnessTestWorld(t, &sidecar.HarnessArgs{Tools: map[string]*sidecar.HarnessToolSpec{
		tool.Name(): {Name: tool.Name(), Profiles: []sidecar.HarnessProfile{sidecar.HarnessProfileLocal}, BuiltIn: true,
			New: func(harness *sidecar.Harness) (sidecar.HarnessTool, error) { return tool, nil }},
	}}, true)
	listener := newSidecarHarnessTestListener()
	sub := world.view.AddListener(listener)
	defer sub.Close()
	agentId, err := world.view.NewAgent(`{"model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	world.router.script(sidecarHarnessTestToolCall("call_1", tool.Name(), `{}`), sidecarHarnessTestText("Found three."))
	if err := world.view.Prompt(agentId, "search"); err != nil {
		t.Fatal(err)
	}
	_, raws := listener.until(t, sidecar.HarnessEventAgentSettled)
	if !strings.Contains(strings.Join(raws, "\n"), "provider-only-text-7f3a") {
		t.Fatalf("the listener never got the details")
	}
	if strings.Contains(world.journal.text(t), "provider-only-text-7f3a") {
		t.Fatalf("the journal kept keep: false details")
	}
	err = filepath.Walk(world.directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(content), "provider-only-text-7f3a") {
			t.Errorf("%s holds keep: false details", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// In the background an agent starts no model call: its prompt settles backgrounded at the item boundary, and once the
// app is back in the foreground it takes up from where it left, calling the model once.
func TestSidecarHarnessBackgroundPausesAtItemBoundary(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, false)
	listener := newSidecarHarnessTestListener()
	sub := world.view.AddListener(listener)
	defer sub.Close()
	agentId, err := world.view.NewAgent(`{"model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	world.router.script(sidecarHarnessTestText("Done."))
	if err := world.view.Prompt(agentId, "work"); err != nil {
		t.Fatal(err)
	}
	events, _ := listener.until(t, sidecar.HarnessEventAgentSettled)
	if events[len(events)-1].StopReason != sidecar.HarnessStopBackgrounded || world.router.callCount() != 0 {
		t.Fatalf("in the background: %s, %d calls", events[len(events)-1].StopReason, world.router.callCount())
	}
	world.view.SetForeground(true)
	events, _ = listener.until(t, sidecar.HarnessEventAgentSettled)
	if events[len(events)-1].StopReason != sidecar.HarnessStopEnded || world.router.callCount() != 1 {
		t.Fatalf("back in the foreground: %s, %d calls", events[len(events)-1].StopReason, world.router.callCount())
	}
	// the workspace is the app's, under its cache directory
	entries, err := os.ReadDir(world.directory)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no workspace under the app's directory: %v", err)
	}
}

// Close ends the agent's run for good: the agent is gone from the view's state and refuses a prompt, and its run's file
// stays, as its card does (§20.7).
func TestSidecarHarnessCloseKeepsCard(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, true)
	agentId, err := world.view.NewAgent(`{"name":"closing","model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	runId := mustSidecarHarnessRun(t, world, agentId)
	if err := world.view.Close(agentId); err != nil {
		t.Fatal(err)
	}
	if err := world.view.Prompt(agentId, "again"); !errors.Is(err, errSidecarHarnessAgent) {
		t.Fatalf("a closed agent's prompt: %v", err)
	}
	if answer, _ := world.view.Agents(); answer != "[]" {
		t.Fatalf("the agents after the close: %s", answer)
	}
	if run, err := world.runs.Get(runId); err != nil || run == nil {
		t.Fatalf("the closed run's file: %v", err)
	}
}

// Text deltas reach the listener batched: on a clock that does not move, a reply's many deltas fold into at most two
// message_update deltas, and message_end carries the whole text.
func TestSidecarHarnessEventsBatched(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, true)
	listener := newSidecarHarnessTestListener()
	sub := world.view.AddListener(listener)
	defer sub.Close()
	agentId, err := world.view.NewAgent(`{"model":"` + sidecarHarnessTestModel + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	deltas := []string{}
	for i := 0; i < 100; i++ {
		deltas = append(deltas, "word ")
	}
	world.router.script(sidecarHarnessTestText(deltas...))
	if err := world.view.Prompt(agentId, "talk"); err != nil {
		t.Fatal(err)
	}
	events, _ := listener.until(t, sidecar.HarnessEventAgentSettled)
	updates := 0
	whole := ""
	for _, event := range events {
		nested := struct {
			Type string `json:"type"`
		}{}
		if sidecar.HarnessEventType(event.Type) == sidecar.HarnessEventMessageUpdate && json.Unmarshal(event.AssistantMessageEvent, &nested) == nil &&
			nested.Type == sidecar.HarnessAssistantEventTextDelta {
			updates += 1
		}
		if sidecar.HarnessEventType(event.Type) == sidecar.HarnessEventMessageEnd && strings.Contains(string(event.Message), strings.Repeat("word ", 100)) {
			whole = string(event.Message)
		}
	}
	if 2 < updates || whole == "" {
		t.Fatalf("%d text deltas reached the listener, whole message %t", updates, whole != "")
	}
}
