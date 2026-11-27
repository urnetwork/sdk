//go:build !ios_extension && !js

// The recURd skill's agents for the apps (DESIGN-COMPUTE §15.3's Agents(), §20.7, §20.8, §20.10; SKILL-M11-08): the
// view controller the bot tab's agents screens bind, gomobile-safe, its structures JSON text, over the local core's
// agents client (the root's agents_*.go) in process, or, on the remote backend, over ur daemon's agents methods (the
// root's agents_daemon.go, the ones ur agents calls), so a desktop app shows what its daemon holds and both answer the
// same JSON.
//
//	List()                 the member's agents: each one's card, host, harness, model, state and its age, activity,
//	                       task and newest summary, the person's me among them; the person's view, every text, since it
//	                       feeds no model; remote: agents.list with untrusted include
//	Send(requestJson)      {"to", "subject", "body", "thread", "close"}: the person's own message, reply or close
//	                       (§20.7's kind (a), bound to no run), the person's act in the app's UI; a reply with no to
//	                       answers the thread's last other sender; remote: agents.send, sent confirmed
//	Broadcast(requestJson) {"subject", "body", "thread"}: a thread to every agent, or, with thread, the person's answer to
//	                       all in it; remote: agents.send with broadcast, sent confirmed
//	Inbox(requestJson)     {"thread"}: the person's threads (one the person wrote in, one to the person's me, one to
//	                       every agent), every message with its verified sender and trust, newest thread first; or one
//	                       thread; remote: agents.inbox with untrusted include
//	Graph(requestJson)     {"retired", "agent"}: the agent graph (§20.8): nodes, launch and conversation edges, retired
//	                       agents only when asked, and with agent its summaries, each a version, and its threads; built
//	                       on the device, no request beyond EVS's sync; remote: agents.graph with untrusted include
//	AddListener(listener)  each new message of the member's threads as the thread_message JSON (OnThreadMessage), a
//	                       local notification's payload for a message to the person's me or one of the member's agents,
//	                       whose sender and subject the device read itself, never a push (OnNotification), and a change
//	                       of the cards, statuses or summaries (OnAgentsChanged), when List and Graph answer anew;
//	                       local: the person's watch (agents_person.go); remote: agents.watch long-polled on a session
//	                       of its own, until the Sub or the Sidecar closes
//
// The screens it serves (DESIGN-UI.md's Bot tab): T5, the agents' feed (List's agents with their state, host and
// harness; AddListener's messages, status changes and summaries; Broadcast for the chat to all, Send for a mention);
// T6, @mention (List's state, host and task); T8, an agent (Graph with agent: its card, model, summaries, threads and
// the agents it started; Send for Message). T8's IP, email address and public TLS names, and its runs (CTXH's
// SidecarHarness.Runs), are not this surface's yet.
//
// Nothing here runs in the VPN extension. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The recURd skill's agents for the apps: what they are doing, their inboxes and the agent graph. Made once per
// Sidecar, at Agents()'s first call; safe for concurrent use.
type SidecarAgents struct {
	// the Sidecar's ctx and backends, which its methods call
	args     *sidecarComputeArgs
	settings *sidecarAgentsSettings

	stateLock sync.Mutex
	// the local watches the listeners hold, closed with the Sidecar
	watches map[*sidecar.AgentPersonWatch]bool
}

// A listener of the person's view of the member's agents.
type SidecarAgentsListener interface {
	// One new message of the member's threads: the thread_message JSON.
	OnThreadMessage(messageJson string)
	// A local notification's payload, for a message to the person's me or one of the member's agents, its sender and
	// subject as this device read them.
	OnNotification(notificationJson string)
	// The cards, statuses or summaries changed: List and Graph answer anew.
	OnAgentsChanged()
}

// The agents' tunables on the remote backend.
type sidecarAgentsSettings struct {
	// The bound of one agents.watch call: the daemon's wait (SkillSettings.DaemonWatchWait, 25 s) and a margin: 35 s.
	WatchCallTimeout time.Duration
	// The first wait before agents.watch is called again after a failure: 1 s, doubling at each failure in a row up to
	// WatchRetryMaxInterval, and back to this once a call answers.
	WatchRetryMinInterval time.Duration
	// The longest such wait: 30 s.
	WatchRetryMaxInterval time.Duration
}

// The defaults, the only place one is set.
func defaultSidecarAgentsSettings() *sidecarAgentsSettings {
	return &sidecarAgentsSettings{
		WatchCallTimeout:      35 * time.Second,
		WatchRetryMinInterval: 1 * time.Second,
		WatchRetryMaxInterval: 30 * time.Second,
	}
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarAgents(args *sidecarComputeArgs) *SidecarAgents {
	return &SidecarAgents{args: args, settings: defaultSidecarAgentsSettings(), watches: map[*sidecar.AgentPersonWatch]bool{}}
}

// The daemon's agents methods, as agents_daemon.go names them; TestAgentsControlWireMatchesTheDaemon holds them to
// that source on the legs that build it.
const (
	sidecarControlMethodAgentsList  = "agents.list"
	sidecarControlMethodAgentsSend  = "agents.send"
	sidecarControlMethodAgentsInbox = "agents.inbox"
	sidecarControlMethodAgentsGraph = "agents.graph"
	sidecarControlMethodAgentsWatch = "agents.watch"
)

// The untrusted word the person's view asks with: every text, since it feeds no model.
const sidecarAgentsUntrustedInclude = "include"

var (
	// What Agents answers with neither a local core nor a remote backend: a closed Sidecar.
	errSidecarAgentsNoCore = errors.New("agents: this Sidecar has no core, local or remote")
	// A request the method cannot take; the text names no value it held.
	errSidecarAgentsRequest = errors.New("agents: the request is malformed")
)

// The params the daemon's methods take, as agents_daemon.go names them; the daemon reads params strictly.
type sidecarControlAgentsListParams struct {
	Network   string `json:"network,omitempty"`
	Retired   bool   `json:"retired,omitempty"`
	Untrusted string `json:"untrusted,omitempty"`
}

type sidecarControlAgentsSendParams struct {
	Network   string `json:"network,omitempty"`
	To        string `json:"to,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Broadcast bool   `json:"broadcast,omitempty"`
	Close     bool   `json:"close,omitempty"`
}

type sidecarControlAgentsInboxParams struct {
	Network   string `json:"network,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Untrusted string `json:"untrusted,omitempty"`
}

type sidecarControlAgentsGraphParams struct {
	Network   string `json:"network,omitempty"`
	Retired   bool   `json:"retired,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Untrusted string `json:"untrusted,omitempty"`
}

type sidecarControlAgentsWatchParams struct {
	Network   string `json:"network,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Untrusted string `json:"untrusted,omitempty"`
}

// One message of agents.watch's answer.
type sidecarControlAgentsWatchMessage struct {
	Message      json.RawMessage `json:"message"`
	Notification json.RawMessage `json:"notification,omitempty"`
}

// agents.watch's answer.
type sidecarControlAgentsWatchReply struct {
	Messages []*sidecarControlAgentsWatchMessage `json:"messages"`
	Cursor   string                              `json:"cursor"`
	Changed  bool                                `json:"changed,omitempty"`
	Gap      bool                                `json:"gap,omitempty"`
	Withheld int                                 `json:"withheld,omitempty"`
}

// What Send and Broadcast answer, the daemon's DaemonAgentsSendReply: what was written, and whether as the person's.
type sidecarAgentsSendReply struct {
	*sidecar.AgentSendToolResult
	Person bool `json:"person"`
}

// Send's request.
type sidecarAgentsSendRequest struct {
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Close   bool   `json:"close,omitempty"`
}

// Broadcast's request.
type sidecarAgentsBroadcastRequest struct {
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	// A thread to all to answer all in; empty opens one.
	Thread string `json:"thread,omitempty"`
}

// Inbox's request.
type sidecarAgentsInboxRequest struct {
	Thread string `json:"thread,omitempty"`
}

// Graph's request.
type sidecarAgentsGraphRequest struct {
	Retired bool   `json:"retired,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// The local core's agents client, or the remote backend to call; one of them.
func (self *SidecarAgents) backend() (*sidecar.AgentsClient, sidecarBackend, error) {
	switch {
	case self.args.local != nil:
		client, err := self.args.local.AgentsClient()
		return client, nil, err
	case self.args.remote != nil:
		return nil, self.args.remote, nil
	}
	return nil, nil, errSidecarAgentsNoCore
}

// The Sidecar's context, which its Close ends; a Sidecar made with none, as a test makes one, never ends.
func (self *SidecarAgents) ctx() context.Context {
	if self.args.ctx != nil {
		return self.args.ctx
	}
	return context.Background()
}

// requestJson into request, strictly; "" leaves it as it is.
func sidecarAgentsDecode(requestJson string, request any) error {
	if err := sidecarComputeDecode(requestJson, request); err != nil {
		return errSidecarAgentsRequest
	}
	return nil
}

// A local answer as JSON.
func sidecarAgentsJson(value any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// A remote call's answer as JSON, as the daemon wrote it.
func (self *SidecarAgents) remoteJson(remote sidecarBackend, method string, params any, confirmed bool) (string, error) {
	var result json.RawMessage
	if err := remote.call(self.ctx(), method, params, confirmed, &result); err != nil {
		return "", err
	}
	return string(result), nil
}

// The member's agents as the person's view shows them, JSON (sidecar.AgentList).
func (self *SidecarAgents) List() (string, error) {
	return traceWithReturnError(func() (string, error) {
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodAgentsList,
				&sidecarControlAgentsListParams{Network: sidecarSupportNetwork(remote), Untrusted: sidecarAgentsUntrustedInclude}, false)
		}
		return sidecarAgentsJson(client.List(self.ctx(), &sidecar.AgentListArgs{}))
	})
}

// The person's own message, reply or close, JSON in and out (see the file header).
func (self *SidecarAgents) Send(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarAgentsSendRequest{}
		if err := sidecarAgentsDecode(requestJson, request); err != nil {
			return "", err
		}
		if request.Thread == "" && request.To == "" {
			return "", errSidecarAgentsRequest
		}
		return self.send(&sidecar.AgentPersonSendArgs{To: request.To, Subject: request.Subject, Body: request.Body,
			Thread: request.Thread, Close: request.Close})
	})
}

// A thread to every agent, or the person's answer to all in one, JSON in and out (see the file header).
func (self *SidecarAgents) Broadcast(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarAgentsBroadcastRequest{}
		if err := sidecarAgentsDecode(requestJson, request); err != nil {
			return "", err
		}
		args := &sidecar.AgentPersonSendArgs{Subject: request.Subject, Body: request.Body, Broadcast: true}
		if request.Thread != "" {
			args.Thread, args.To, args.Broadcast = request.Thread, "*", false
		}
		return self.send(args)
	})
}

// The person's send on either backend.
func (self *SidecarAgents) send(args *sidecar.AgentPersonSendArgs) (string, error) {
	client, remote, err := self.backend()
	if err != nil {
		return "", err
	}
	if remote != nil {
		// the person confirmed it in the app's UI (D-CORE-13); the daemon writes it as theirs
		return self.remoteJson(remote, sidecarControlMethodAgentsSend, &sidecarControlAgentsSendParams{
			Network: sidecarSupportNetwork(remote), To: args.To, Subject: args.Subject, Body: args.Body, Thread: args.Thread,
			Broadcast: args.Broadcast, Close: args.Close,
		}, true)
	}
	result, err := client.SendAsPerson(self.ctx(), args)
	if err != nil {
		return "", err
	}
	return sidecarAgentsJson(&sidecarAgentsSendReply{AgentSendToolResult: result, Person: true}, nil)
}

// The person's threads, or one, JSON in and out (sidecar.AgentPersonInbox).
func (self *SidecarAgents) Inbox(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarAgentsInboxRequest{}
		if err := sidecarAgentsDecode(requestJson, request); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodAgentsInbox, &sidecarControlAgentsInboxParams{
				Network: sidecarSupportNetwork(remote), Thread: request.Thread, Untrusted: sidecarAgentsUntrustedInclude,
			}, false)
		}
		return sidecarAgentsJson(client.PersonInbox(self.ctx(), &sidecar.AgentPersonInboxArgs{Thread: request.Thread}))
	})
}

// The agent graph, JSON in and out (sidecar.AgentsGraph).
func (self *SidecarAgents) Graph(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarAgentsGraphRequest{}
		if err := sidecarAgentsDecode(requestJson, request); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodAgentsGraph, &sidecarControlAgentsGraphParams{
				Network: sidecarSupportNetwork(remote), Retired: request.Retired, Agent: request.Agent, Untrusted: sidecarAgentsUntrustedInclude,
			}, false)
		}
		return sidecarAgentsJson(client.Graph(self.ctx(), &sidecar.AgentsGraphArgs{Retired: request.Retired, Agent: request.Agent}))
	})
}

// Adds listener for the person's view; the Sub removes it. On the remote backend the daemon is asked first, so a daemon
// with no agents client answers its error here rather than a listener that never hears.
func (self *SidecarAgents) AddListener(listener SidecarAgentsListener) (Sub, error) {
	return traceWithReturnError(func() (Sub, error) {
		if listener == nil {
			return nil, errSidecarAgentsRequest
		}
		client, remote, err := self.backend()
		if err != nil {
			return nil, err
		}
		if remote == nil {
			watch, err := client.WatchPerson(self.ctx(), &sidecar.AgentPersonWatchArgs{
				Message: func(message *sidecar.AgentThreadMessage, notification *sidecar.AgentNotification) {
					sidecarAgentsTell(listener, message, notification)
				},
				Changed: listener.OnAgentsChanged,
			})
			if err != nil {
				return nil, err
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.watches[watch] = true
			}()
			return newSub(func() {
				self.stateLock.Lock()
				delete(self.watches, watch)
				self.stateLock.Unlock()
				watch.Close()
			}), nil
		}
		params := &sidecarControlAgentsWatchParams{Network: sidecarSupportNetwork(remote), Untrusted: sidecarAgentsUntrustedInclude}
		first := &sidecarControlAgentsWatchReply{}
		if err := remote.call(self.ctx(), sidecarControlMethodAgentsWatch, params, false, first); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(self.ctx())
		go connect.HandleError(func() { self.watchRemote(ctx, remote, first.Cursor, listener) })
		return newSub(cancel), nil
	})
}

// Tells listener a message and its notification, as JSON.
func sidecarAgentsTell(listener SidecarAgentsListener, message *sidecar.AgentThreadMessage, notification *sidecar.AgentNotification) {
	if encoded, err := json.Marshal(message); err == nil {
		listener.OnThreadMessage(string(encoded))
	}
	if notification != nil {
		if encoded, err := json.Marshal(notification); err == nil {
			listener.OnNotification(string(encoded))
		}
	}
}

// Long-polls agents.watch from cursor on a session of its own until ctx ends, telling each message in order, and
// calling again after a failure once its wait passes.
func (self *SidecarAgents) watchRemote(ctx context.Context, remote sidecarBackend, cursor string, listener SidecarAgentsListener) {
	var channel sidecarControlChannel
	defer func() {
		if channel != nil {
			channel.close()
		}
	}()
	dialer, ownSession := remote.(interface {
		dialOwn(ctx context.Context) (sidecarControlChannel, error)
	})
	failureCount := 0
	for ctx.Err() == nil {
		if 0 < failureCount {
			wait := self.settings.WatchRetryMinInterval
			for i := 1; i < failureCount && wait < self.settings.WatchRetryMaxInterval; i += 1 {
				wait *= 2
			}
			reconnect := connect.NewPacedReconnect(min(wait, self.settings.WatchRetryMaxInterval))
			select {
			case <-ctx.Done():
				return
			case <-reconnect.After():
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, self.settings.WatchCallTimeout)
		reply := &sidecarControlAgentsWatchReply{}
		params := &sidecarControlAgentsWatchParams{Network: sidecarSupportNetwork(remote), Cursor: cursor, Untrusted: sidecarAgentsUntrustedInclude}
		var err error
		if !ownSession {
			err = remote.call(callCtx, sidecarControlMethodAgentsWatch, params, false, reply)
		} else {
			if channel == nil {
				channel, err = dialer.dialOwn(callCtx)
				if err != nil {
					channel = nil
				}
			}
			if channel != nil {
				err = channel.call(callCtx, sidecarControlMethodAgentsWatch, params, false, reply)
				var refusal *sidecarControlError
				if err != nil && !errors.As(err, &refusal) {
					// the session is gone; the next call dials another
					channel.close()
					channel = nil
				}
			}
		}
		cancel()
		if err != nil {
			failureCount += 1
			continue
		}
		failureCount = 0
		if reply.Cursor != "" {
			cursor = reply.Cursor
		}
		for _, message := range reply.Messages {
			if message == nil || ctx.Err() != nil {
				continue
			}
			listener.OnThreadMessage(string(message.Message))
			if len(message.Notification) != 0 && string(message.Notification) != "null" {
				listener.OnNotification(string(message.Notification))
			}
		}
		if (reply.Changed || reply.Gap) && ctx.Err() == nil {
			listener.OnAgentsChanged()
		}
	}
}

// Closes the local watches the listeners hold, at the Sidecar's Close.
func (self *SidecarAgents) close() {
	watches := func() []*sidecar.AgentPersonWatch {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		watches := []*sidecar.AgentPersonWatch{}
		for watch := range self.watches {
			watches = append(watches, watch)
		}
		clear(self.watches)
		return watches
	}()
	for _, watch := range watches {
		watch.Close()
	}
}
