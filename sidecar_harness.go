//go:build !ios_extension && !js

// The Harness for the apps (DESIGN-COMPUTE §15.3, §7.6, §7.4; HARN-M11-10): SidecarHarness, the common view controller
// the bot tab binds (DESIGN-UI.md's T1, the run as a terminal with its pills; T5's feed of agents; T8's agent card), over
// the Sidecar's Harness in process, or, on the remote backend, over ur daemon's harness methods (the root's
// harness_daemon.go, the ones `ur harness` calls), so a desktop app in remote mode drives the daemon's Harness and both
// answer the same JSON. gomobile-safe: strings, int64, bool, listener interfaces and JSON.
//
//	NewHarness(settingsJson)  {workspace_directory, mobile, foreground}: the app's cache directory, whether it is a
//	                          phone, and whether it starts in the foreground, set as the Harness's host (HarnessHost)
//	NewAgent(specJson)        {name, model, tools, instructions, budget {day, month, run} in USD}: the agent's id;
//	                          remote: harness.agent
//	Prompt, Steer, FollowUp   the person's text from the app's UI, with the person's origin; remote: harness.prompt,
//	                          confirmed, its mode prompt, steer or follow_up
//	Abort, SetModel, Close    the turn ended; the next call's model, which passes §6.4's checks; the agent's run closed
//	                          for good, its card staying (§20.7); remote: harness.prompt mode abort, harness.agent with
//	                          model, harness.close
//	Agents()                  the bot tab's state: each agent's id, name, model, run, whether it is at work, its parent
//	                          and whether its run read untrusted content; remote: the agents this view made or reached
//	AddListener(listener)     each agent's §7.6 events as JSON strings (OnEvent), text deltas batched to EventRate a
//	                          second per agent; while a listener is added the person is attached to the agents' runs,
//	                          so a run may ask them; remote: a long poll of harness.events per agent
//	RespondUi(id, json)       a select's, confirm's or input's answer, {value, confirmed, cancelled}; a disclosure only
//	                          declined, never acknowledged (§2.8): its acknowledgment is AckDisclosure's, the person's
//	                          own act after the app showed its whole text; remote: harness.respond_ui
//	Compact(id, instructions) the context compacted now, while the agent is idle (T11); local only
//	SetForeground(on)         the app's lifecycle: agents answer only in the foreground, pause at an item boundary
//	                          when the app leaves, and take up from where they left when it comes back (§6.5, §20.7)
//
// A listener is never called with a lock held, each on a goroutine of its own, so a slow or panicking listener holds no
// loop and no other listener: past EventSubscriberBufferCount waiting, its text deltas are dropped, never message_end,
// which carries the whole message. keep: false details are held in memory only: nothing here writes an event to the
// app's storage. Attach, Harnesses, Manage, Launch, Watch, Unmanage, Runs and Continue are HARN-M14's and CTXH's.
//
// Nothing here runs in the VPN extension or on the js leg. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The daemon's harness methods and modes, as harness_daemon.go names them; TestSidecarHarnessControlWireMatchesTheDaemon
// holds them to that source on the legs that build it.
const (
	sidecarControlMethodHarnessAgent     = "harness.agent"
	sidecarControlMethodHarnessPrompt    = "harness.prompt"
	sidecarControlMethodHarnessEvents    = "harness.events"
	sidecarControlMethodHarnessRespondUi = "harness.respond_ui"
	sidecarControlMethodHarnessClose     = "harness.close"

	sidecarHarnessModePrompt   = "prompt"
	sidecarHarnessModeSteer    = "steer"
	sidecarHarnessModeFollowUp = "follow_up"
	sidecarHarnessModeAbort    = "abort"
)

var (
	// A spec or a response the method cannot take; the text names no value it held.
	errSidecarHarnessRequest = errors.New("harness: the request is malformed")
	// A response that acknowledges a disclosure: only AckDisclosure does, the person's own act (§2.8).
	errSidecarHarnessAcknowledge = errors.New("harness: a disclosure is acknowledged only by the person's own act, AckDisclosure")
	// No agent of that id is open in this Harness.
	errSidecarHarnessAgent = errors.New("harness: no such agent is open")
	// What a Harness answers with neither a local core nor a remote backend: a closed Sidecar.
	errSidecarHarnessNoCore = errors.New("harness: this Sidecar has no core, local or remote")
	// What the remote backend answers for a method ur daemon's harness methods do not serve yet.
	errSidecarHarnessRemoteUnbuilt = errors.New("harness: the daemon's harness methods do not take this yet")
)

// One Harness and its agents. Safe for concurrent use.
type SidecarHarness struct {
	// the Sidecar's ctx and backends, which its methods call
	args     *sidecarComputeArgs
	settings *sidecar.HarnessSettings
	clock    sidecar.Clock
	host     *sidecarHarnessHost
	// in process: the Sidecar's Harness, nil on the remote backend
	harness *sidecar.Harness
	// the batcher every agent's events pass through to the listeners
	batcher *sidecar.HarnessEventBatcher
	// the runs' history, continue and storage (sidecar_harness_history.go, sidecar_harness_runs.go): nil takes the
	// Sidecar's in process or the daemon's methods remotely; a test's where it sets one
	history sidecarHarnessHistory

	stateLock sync.Mutex
	listeners map[int]*sidecarHarnessListenerQueue
	nextId    int
	// removes the Harness subscription the listeners take, nil while none is added
	unsubscribe func()
	// each agent's detach from its run's attachment, while a listener is added
	agentIdDetaches map[sidecar.Id]func()
	// the agents this view made or reached on the remote backend, and each one's poll
	remoteAgents      map[string]*sidecarControlHarnessAgentReply
	remoteAgentCancel map[string]context.CancelFunc
	// each pending ui request's agent, for the remote backend's respond_ui
	requestIdAgents map[string]string
}

// A Harness's events, each one of §7.6's typed events as JSON.
type SidecarHarnessListener interface {
	OnEvent(eventJson string)
}

// What NewHarness's settings name.
type sidecarHarnessSettings struct {
	// The app's cache directory, where local agents' workspaces are made; empty for the OS's temp directory.
	WorkspaceDirectory string `json:"workspace_directory,omitempty"`
	// Whether the app is a phone, whose workspace cap is the phone's.
	Mobile bool `json:"mobile,omitempty"`
	// Whether the app starts in the foreground; omitted reads true.
	Foreground *bool `json:"foreground,omitempty"`
}

// The app's host as the root reads it: its directory, whether it is a phone, and its foreground, which SetForeground
// moves.
type sidecarHarnessHost struct {
	workspaceDirectory string
	mobile             bool

	stateLock  sync.Mutex
	foreground bool
}

// The app's cache directory.
func (self *sidecarHarnessHost) WorkspaceDirectory() string {
	return self.workspaceDirectory
}

// Whether the app is a phone.
func (self *sidecarHarnessHost) Mobile() bool {
	return self.mobile
}

// Whether the app is in the foreground now.
func (self *sidecarHarnessHost) Foreground() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.foreground
}

var _ sidecar.HarnessHost = (*sidecarHarnessHost)(nil)

// A Harness on args with settingsJson's settings: the Sidecar's own Harness in process, its host set to the app's, or
// the daemon's on the remote backend.
func newSidecarHarness(args *sidecarComputeArgs, settingsJson string) (*SidecarHarness, error) {
	settings := &sidecarHarnessSettings{}
	if err := sidecarComputeDecode(settingsJson, settings); err != nil {
		return nil, errSidecarHarnessRequest
	}
	host := &sidecarHarnessHost{workspaceDirectory: settings.WorkspaceDirectory, mobile: settings.Mobile, foreground: true}
	if settings.Foreground != nil {
		host.foreground = *settings.Foreground
	}
	switch {
	case args.local != nil:
		harness, err := args.local.Harness()
		if err != nil {
			return nil, err
		}
		return newSidecarHarnessOver(args, harness, host, nil), nil
	case args.remote != nil:
		return newSidecarHarnessOver(args, nil, host, nil), nil
	}
	return nil, errSidecarHarnessNoCore
}

// A view over harness, nil on the remote backend, its events batched on clock (nil for real time).
func newSidecarHarnessOver(args *sidecarComputeArgs, harness *sidecar.Harness, host *sidecarHarnessHost, clock sidecar.Clock) *SidecarHarness {
	settings := sidecar.DefaultHarnessSettings()
	if harness != nil {
		settings = harness.Settings()
		harness.SetHost(host)
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	self := &SidecarHarness{
		args:              args,
		settings:          settings,
		clock:             clock,
		host:              host,
		harness:           harness,
		listeners:         map[int]*sidecarHarnessListenerQueue{},
		agentIdDetaches:   map[sidecar.Id]func(){},
		remoteAgents:      map[string]*sidecarControlHarnessAgentReply{},
		remoteAgentCancel: map[string]context.CancelFunc{},
		requestIdAgents:   map[string]string{},
	}
	self.batcher = sidecar.NewHarnessEventBatcher(settings, clock, self.deliver)
	return self
}

// The Sidecar's context, which its Close ends; a Sidecar made with none, as a test makes one, never ends.
func (self *SidecarHarness) ctx() context.Context {
	if self.args != nil && self.args.ctx != nil {
		return self.args.ctx
	}
	return context.Background()
}

// The local agent of agentId.
func (self *SidecarHarness) agent(agentId string) (sidecar.HarnessAgent, error) {
	id, err := sidecar.ParseId(agentId)
	if err != nil {
		return nil, errSidecarHarnessAgent
	}
	agent, ok := self.harness.Agent(id)
	if !ok {
		return nil, errSidecarHarnessAgent
	}
	return agent, nil
}

// NewAgent's spec.
type sidecarHarnessAgentSpec struct {
	Name         string   `json:"name,omitempty"`
	Model        string   `json:"model"`
	Tools        []string `json:"tools,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Budget       *struct {
		Day   float64 `json:"day,omitempty"`
		Month float64 `json:"month,omitempty"`
		Run   float64 `json:"run,omitempty"`
	} `json:"budget,omitempty"`
}

// USD as nano-USD, rounded down; 0 for none.
func sidecarHarnessNanoUsd(usd float64) uint64 {
	if !(0 < usd) {
		return 0
	}
	return uint64(usd * 1e9)
}

// Starts an agent on specJson's spec and answers its id.
func (self *SidecarHarness) NewAgent(specJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		spec := &sidecarHarnessAgentSpec{}
		if err := sidecarComputeDecode(specJson, spec); err != nil {
			return "", errSidecarHarnessRequest
		}
		if self.harness == nil {
			reply := &sidecarControlHarnessAgentReply{}
			params := &sidecarControlHarnessAgentParams{Network: self.network(), Name: spec.Name, Model: spec.Model}
			if err := self.remote().call(self.ctx(), sidecarControlMethodHarnessAgent, params, true, reply); err != nil {
				return "", err
			}
			self.addRemoteAgent(reply)
			return reply.Agent, nil
		}
		agentSpec := &sidecar.HarnessAgentSpec{Name: spec.Name, Model: spec.Model, Tools: spec.Tools, Instructions: spec.Instructions}
		if spec.Budget != nil {
			agentSpec.Budget = &sidecar.HarnessBudget{Id: sidecar.NewId(), DayNanoUsd: sidecarHarnessNanoUsd(spec.Budget.Day),
				MonthNanoUsd: sidecarHarnessNanoUsd(spec.Budget.Month), RunNanoUsd: sidecarHarnessNanoUsd(spec.Budget.Run)}
		}
		agent, err := self.harness.NewAgent(self.ctx(), agentSpec)
		if err != nil {
			return "", err
		}
		self.attachWhileListened(agent)
		return agent.Id().String(), nil
	})
}

// The person's prompt to agentId, refused while it runs: steer or follow up instead.
func (self *SidecarHarness) Prompt(agentId string, text string) error {
	return traceWithError(func() error {
		return self.send(agentId, text, sidecarHarnessModePrompt)
	})
}

// The person's text for agentId, delivered after its current turn.
func (self *SidecarHarness) Steer(agentId string, text string) error {
	return traceWithError(func() error {
		return self.send(agentId, text, sidecarHarnessModeSteer)
	})
}

// The person's text for agentId, delivered once it has no more work.
func (self *SidecarHarness) FollowUp(agentId string, text string) error {
	return traceWithError(func() error {
		return self.send(agentId, text, sidecarHarnessModeFollowUp)
	})
}

// Prompt, Steer and FollowUp: the person's origin, the app's UI being the person's input (§7.10's origins).
func (self *SidecarHarness) send(agentId string, text string, mode string) error {
	if self.harness == nil {
		params := &sidecarControlHarnessPromptParams{Network: self.network(), Agent: agentId, Text: text, Mode: mode}
		return self.remote().call(self.ctx(), sidecarControlMethodHarnessPrompt, params, true, &json.RawMessage{})
	}
	agent, err := self.agent(agentId)
	if err != nil {
		return err
	}
	origin := sidecar.HarnessPersonOrigin()
	switch mode {
	case sidecarHarnessModeSteer:
		return agent.SteerAs(self.ctx(), origin, text)
	case sidecarHarnessModeFollowUp:
		return agent.FollowUpAs(self.ctx(), origin, text)
	}
	return agent.PromptAs(self.ctx(), origin, text, nil)
}

// Ends agentId's turn: its run settles aborted.
func (self *SidecarHarness) Abort(agentId string) {
	trace(func() {
		if self.harness == nil {
			params := &sidecarControlHarnessPromptParams{Network: self.network(), Agent: agentId, Mode: sidecarHarnessModeAbort}
			self.remote().call(self.ctx(), sidecarControlMethodHarnessPrompt, params, true, &json.RawMessage{})
			return
		}
		if agent, err := self.agent(agentId); err == nil {
			agent.Abort()
		}
	})
}

// Takes model at agentId's next call, which passes §6.4's checks, a none model's disclosure among them.
func (self *SidecarHarness) SetModel(agentId string, model string) error {
	return traceWithError(func() error {
		if self.harness == nil {
			reply := &sidecarControlHarnessAgentReply{}
			params := &sidecarControlHarnessAgentParams{Network: self.network(), Agent: agentId, Model: model}
			if err := self.remote().call(self.ctx(), sidecarControlMethodHarnessAgent, params, true, reply); err != nil {
				return err
			}
			self.addRemoteAgent(reply)
			return nil
		}
		agent, err := self.agent(agentId)
		if err != nil {
			return err
		}
		return agent.SetModel(model)
	})
}

// Closes agentId's run and its leases for good; its card stays (§20.7).
func (self *SidecarHarness) Close(agentId string) error {
	return traceWithError(func() error {
		if self.harness == nil {
			params := &sidecarControlHarnessCloseParams{Network: self.network(), Agent: agentId}
			if err := self.remote().call(self.ctx(), sidecarControlMethodHarnessClose, params, true, &json.RawMessage{}); err != nil {
				return err
			}
			self.removeRemoteAgent(agentId)
			return nil
		}
		agent, err := self.agent(agentId)
		if err != nil {
			return err
		}
		agent.Close()
		self.detach(agent.Id())
		return nil
	})
}

// One agent as Agents names it: what the bot tab's pills, feed and agent card show of it.
type sidecarHarnessAgentState struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	Run     string `json:"run"`
	Running bool   `json:"running"`
	// read untrusted content, so its sends wait for the person (§7.8)
	Tainted bool `json:"tainted"`
	// the context's tokens and the model's window: the session pill's share and the Compact screen's figure
	ContextTokens int64 `json:"context_tokens,omitempty"`
	ContextWindow int64 `json:"context_window,omitempty"`
}

// The agents' state, JSON: each agent's id, name, model, run, whether it is at work and whether its run is tainted.
func (self *SidecarHarness) Agents() (string, error) {
	return traceWithReturnError(func() (string, error) {
		states := []*sidecarHarnessAgentState{}
		if self.harness == nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				for _, reply := range self.remoteAgents {
					states = append(states, &sidecarHarnessAgentState{Agent: reply.Agent, Name: reply.Name, Model: reply.Model,
						Run: reply.Run, Running: reply.Running})
				}
			}()
		} else {
			for _, agent := range self.harness.Agents() {
				tokens, window := sidecar.HarnessAgentContext(self.ctx(), agent)
				states = append(states, &sidecarHarnessAgentState{Agent: agent.Id().String(), Name: agent.Name(),
					Model: sidecar.HarnessAgentModel(agent), Run: agent.Run().Id().String(), Running: agent.Running(),
					Tainted: agent.Run().Tainted(), ContextTokens: tokens, ContextWindow: window})
			}
		}
		encoded, err := json.Marshal(states)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

// Compacts agentId's context now, with instructions for its summary, while it is idle (DESIGN-UI.md's T11; §7.4);
// the remote backend's daemon takes no compaction yet.
func (self *SidecarHarness) Compact(agentId string, instructions string) error {
	return traceWithError(func() error {
		if self.harness == nil {
			return errSidecarHarnessRemoteUnbuilt
		}
		agent, err := self.agent(agentId)
		if err != nil {
			return err
		}
		return agent.Compact(self.ctx(), instructions)
	})
}

// RespondUi's answer.
type sidecarHarnessUiAnswer struct {
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
	// refused: a disclosure is acknowledged only by AckDisclosure
	Acknowledged bool `json:"acknowledged,omitempty"`
}

// The person's answer to the ui request requestId: a select's value, a confirm's, an input's, or a cancel, which for a
// disclosure declines it ("Not now"); never an acknowledgment, which is AckDisclosure's alone (§2.8).
func (self *SidecarHarness) RespondUi(requestId string, responseJson string) error {
	return traceWithError(func() error {
		answer := &sidecarHarnessUiAnswer{}
		if err := sidecarComputeDecode(responseJson, answer); err != nil {
			return errSidecarHarnessRequest
		}
		if answer.Acknowledged {
			return errSidecarHarnessAcknowledge
		}
		response := &sidecar.HarnessUiResponse{Id: requestId, Value: answer.Value, Confirmed: answer.Confirmed, Cancelled: answer.Cancelled}
		if self.harness == nil {
			agentId := func() string {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				return self.requestIdAgents[requestId]
			}()
			params := &sidecarControlHarnessRespondUiParams{Network: self.network(), Agent: agentId, Response: response}
			return self.remote().call(self.ctx(), sidecarControlMethodHarnessRespondUi, params, true, &json.RawMessage{})
		}
		return self.harness.UiBroker().Respond(response)
	})
}

// The app's lifecycle: agents answer only in the foreground, pausing at an item boundary when the app leaves and taking
// up from where they left when it comes back.
func (self *SidecarHarness) SetForeground(foreground bool) {
	trace(func() {
		func() {
			self.host.stateLock.Lock()
			defer self.host.stateLock.Unlock()
			self.host.foreground = foreground
		}()
		if self.harness != nil {
			self.harness.HostForegroundChanged(self.ctx())
		}
	})
}

// One listener's queue: its events in order on a goroutine of its own, text deltas past the bound dropped, never
// message_end or any other event.
type sidecarHarnessListenerQueue struct {
	listener SidecarHarnessListener
	bound    int

	stateLock sync.Mutex
	events    []string
	deltas    int
	wake      chan struct{}
	closed    bool
}

// A queue for listener, its delta bound settings', its goroutine started.
func newSidecarHarnessListenerQueue(listener SidecarHarnessListener, bound int) *sidecarHarnessListenerQueue {
	queue := &sidecarHarnessListenerQueue{listener: listener, bound: max(bound, 1), wake: make(chan struct{}, 1)}
	go connect.HandleError(queue.run)
	return queue
}

// Queues one event's JSON without waiting: a delta past the bound is dropped.
func (self *sidecarHarnessListenerQueue) offer(eventJson string, delta bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || (delta && self.bound <= self.deltas) {
			return
		}
		self.events = append(self.events, eventJson)
		if delta {
			self.deltas += 1
		}
	}()
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Calls the listener with each queued event in order, until closed; a listener's panic is its own.
func (self *sidecarHarnessListenerQueue) run() {
	for {
		events, closed := func() ([]string, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			events := self.events
			self.events = nil
			self.deltas = 0
			return events, self.closed
		}()
		if closed {
			return
		}
		for _, eventJson := range events {
			connect.HandleError(func() { self.listener.OnEvent(eventJson) })
		}
		<-self.wake
	}
}

// Stops the queue's goroutine.
func (self *sidecarHarnessListenerQueue) close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
	}()
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Adds listener for every agent's events; Close removes it. While one is added, the person is attached to the agents'
// runs, so a run may ask them.
func (self *SidecarHarness) AddListener(listener SidecarHarnessListener) Sub {
	sub, _ := traceWithReturn(func() Sub {
		queue := newSidecarHarnessListenerQueue(listener, self.settings.EventSubscriberBufferCount)
		id, first := func() (int, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			id := self.nextId
			self.nextId += 1
			self.listeners[id] = queue
			return id, len(self.listeners) == 1
		}()
		if first {
			self.listen()
		}
		return newSub(func() {
			last := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				delete(self.listeners, id)
				return len(self.listeners) == 0
			}()
			queue.close()
			if last {
				self.unlisten()
			}
		})
	})
	return sub
}

// Starts listening for the listeners: the Harness's events through the batcher, every agent's run attached; or the
// remote backend's polls.
func (self *SidecarHarness) listen() {
	if self.harness == nil {
		self.stateLock.Lock()
		agentIds := []string{}
		for agentId := range self.remoteAgents {
			agentIds = append(agentIds, agentId)
		}
		self.stateLock.Unlock()
		for _, agentId := range agentIds {
			self.pollRemote(agentId)
		}
		return
	}
	unsubscribe := self.harness.Subscribe(func(event *sidecar.HarnessEvent) {
		self.batcher.Add(event)
	})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.unsubscribe = unsubscribe
	}()
	for _, agent := range self.harness.Agents() {
		self.attachWhileListened(agent)
	}
}

// Stops listening once the last listener is gone: the subscription removed, every attachment detached, every poll ended.
func (self *SidecarHarness) unlisten() {
	unsubscribe, detaches, cancels := func() (func(), []func(), []context.CancelFunc) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		unsubscribe := self.unsubscribe
		self.unsubscribe = nil
		detaches := []func(){}
		for id, detach := range self.agentIdDetaches {
			detaches = append(detaches, detach)
			delete(self.agentIdDetaches, id)
		}
		cancels := []context.CancelFunc{}
		for agentId, cancel := range self.remoteAgentCancel {
			cancels = append(cancels, cancel)
			delete(self.remoteAgentCancel, agentId)
		}
		return unsubscribe, detaches, cancels
	}()
	if unsubscribe != nil {
		unsubscribe()
	}
	for _, detach := range detaches {
		detach()
	}
	for _, cancel := range cancels {
		cancel()
	}
	self.batcher.Flush()
}

// Marks the person attached to agent's run while a listener is added.
func (self *SidecarHarness) attachWhileListened(agent sidecar.HarnessAgent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.listeners) == 0 {
		return
	}
	if _, attached := self.agentIdDetaches[agent.Id()]; attached {
		return
	}
	self.agentIdDetaches[agent.Id()] = self.harness.UiBroker().AttachPerson(agent.Run().Id())
}

// Detaches the person from agentId's run.
func (self *SidecarHarness) detach(agentId sidecar.Id) {
	detach := func() func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		detach := self.agentIdDetaches[agentId]
		delete(self.agentIdDetaches, agentId)
		return detach
	}()
	if detach != nil {
		detach()
	}
}

// Hands one batched event to every listener as JSON, never waiting on one; called by the batcher with no lock held.
func (self *SidecarHarness) deliver(event *sidecar.HarnessEvent) {
	encoded, err := event.Json()
	if err != nil {
		return
	}
	if event.Type == sidecar.HarnessEventUiRequest && self.harness == nil {
		if request := event.UiRequest(); request != nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.requestIdAgents[request.Id] = event.Agent
			}()
		}
	}
	if self.harness != nil && event.Type == sidecar.HarnessEventAgentSpawned {
		// a child's run is attached as its parent's is
		childId := ""
		event.Decode("child", &childId)
		if id, err := sidecar.ParseId(childId); err == nil {
			if child, ok := self.harness.Agent(id); ok {
				self.attachWhileListened(child)
			}
		}
	}
	delta := event.Type == sidecar.HarnessEventMessageUpdate
	queues := func() []*sidecarHarnessListenerQueue {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		queues := make([]*sidecarHarnessListenerQueue, 0, len(self.listeners))
		for _, queue := range self.listeners {
			queues = append(queues, queue)
		}
		return queues
	}()
	for _, queue := range queues {
		queue.offer(string(encoded), delta)
	}
}

// The remote backend.
func (self *SidecarHarness) remote() sidecarBackend {
	if self.args == nil || self.args.remote == nil {
		return sidecarHarnessNoBackend{}
	}
	return self.args.remote
}

// The paired network the daemon's methods act on.
func (self *SidecarHarness) network() string {
	if self.args == nil || self.args.remote == nil {
		return ""
	}
	return sidecarSupportNetwork(self.args.remote)
}

// Keeps a remote agent this view made or reached, and polls its events while a listener is added.
func (self *SidecarHarness) addRemoteAgent(reply *sidecarControlHarnessAgentReply) {
	listened := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.remoteAgents[reply.Agent] = reply
		return 0 < len(self.listeners)
	}()
	if listened {
		self.pollRemote(reply.Agent)
	}
}

// Forgets a closed remote agent and ends its poll.
func (self *SidecarHarness) removeRemoteAgent(agentId string) {
	cancel := func() context.CancelFunc {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.remoteAgents, agentId)
		cancel := self.remoteAgentCancel[agentId]
		delete(self.remoteAgentCancel, agentId)
		return cancel
	}()
	if cancel != nil {
		cancel()
	}
}

// Long-polls harness.events for agentId until its poll is cancelled, handing each event to the batcher in order; once
// the agent closed the poll ends, and after a failure it calls again once its wait passes.
func (self *SidecarHarness) pollRemote(agentId string) {
	ctx, cancel := context.WithCancel(self.ctx())
	started := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, polling := self.remoteAgentCancel[agentId]; polling {
			return false
		}
		self.remoteAgentCancel[agentId] = cancel
		return true
	}()
	if !started {
		cancel()
		return
	}
	go connect.HandleError(func() {
		defer cancel()
		after := uint64(0)
		failures := 0
		waitMs := self.settings.DaemonViewWait.Milliseconds()
		for ctx.Err() == nil {
			if 0 < failures {
				wait := self.settings.ManageLinkReconnectBase
				for i := 1; i < failures && wait < self.settings.ManageLinkReconnectMax; i += 1 {
					wait *= 2
				}
				select {
				case <-ctx.Done():
					return
				case <-self.clock.After(min(wait, self.settings.ManageLinkReconnectMax)):
				}
			}
			reply := &sidecarControlHarnessEventsReply{}
			params := &sidecarControlHarnessEventsParams{Network: self.network(), Agent: agentId, After: after, WaitMs: waitMs}
			if err := self.remote().call(ctx, sidecarControlMethodHarnessEvents, params, true, reply); err != nil {
				failures += 1
				continue
			}
			failures = 0
			for _, raw := range reply.Events {
				event := &sidecar.HarnessEvent{}
				if err := json.Unmarshal(raw, event); err == nil {
					self.batcher.Add(event)
				}
			}
			after = reply.Next
			if reply.Closed {
				return
			}
		}
	})
}

// What harness.agent takes, as harness_daemon.go names it.
type sidecarControlHarnessAgentParams struct {
	Network string `json:"network,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Name    string `json:"name,omitempty"`
	Model   string `json:"model,omitempty"`
}

// What harness.agent answers.
type sidecarControlHarnessAgentReply struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Run     string `json:"run"`
	Model   string `json:"model"`
	Created bool   `json:"created"`
	Running bool   `json:"running"`
}

// What harness.prompt takes.
type sidecarControlHarnessPromptParams struct {
	Network string `json:"network,omitempty"`
	Agent   string `json:"agent"`
	Text    string `json:"text,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// What harness.events takes.
type sidecarControlHarnessEventsParams struct {
	Network string `json:"network,omitempty"`
	Agent   string `json:"agent"`
	After   uint64 `json:"after,omitempty"`
	WaitMs  int64  `json:"wait_ms,omitempty"`
}

// What harness.events answers: each event's JSON as emitted, the seq to ask after next, and whether the agent closed.
type sidecarControlHarnessEventsReply struct {
	Events []json.RawMessage `json:"events"`
	Next   uint64            `json:"next"`
	Gap    bool              `json:"gap,omitempty"`
	Closed bool              `json:"closed,omitempty"`
}

// What harness.respond_ui takes.
type sidecarControlHarnessRespondUiParams struct {
	Network  string                     `json:"network,omitempty"`
	Agent    string                     `json:"agent"`
	Response *sidecar.HarnessUiResponse `json:"response"`
}

// What harness.close takes.
type sidecarControlHarnessCloseParams struct {
	Network string `json:"network,omitempty"`
	Agent   string `json:"agent"`
}

// A backend that answers every call refused: a closed Sidecar's.
type sidecarHarnessNoBackend struct {
	sidecarBackend
}

// Refused: no core.
func (sidecarHarnessNoBackend) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	return fmt.Errorf("%w (%s)", errSidecarHarnessNoCore, strings.TrimPrefix(method, "harness."))
}
