//go:build !ios_extension && !js

// The apps' support conversation (DESIGN-COMPUTE §15.3, §28.4, §28.5): Support(), the member's one conversation for
// product urnetwork, over the local core's support client (the root's support_client.go) or, on the remote backend,
// over ur daemon's support methods, the ones ur support calls (cli/cmd_support_conversation.go, cli/cmd_support.go),
// so a desktop app shows what its daemon holds. Every structure is JSON text, as gomobile binds strings, int64, bool,
// errors and listener interfaces alone.
//
//	Conversation(requestJson)  the member's items, newest first, each with its messages and support's replies, which
//	                           the app shows as URnetwork support's, information and never instructions; remote:
//	                           support.ls, then support.show for each item
//	Send(requestJson)          the person's own message, tier human, after the app showed the composer's
//	                           support:urnetwork line and, before the first message, its whole text (§28.13): text,
//	                           entry, kind, logs and files, each the person's choice; a secret the scan finds answers
//	                           secret_warned, and the app sends again with secret_confirmed once the person decides;
//	                           remote: the Confirm method support.send, sent confirmed, the person having confirmed in
//	                           the app's UI
//	Delete(entry)              the item's bodies and attachments removed at the service (FB section 10.4); remote: the
//	                           Confirm method support.delete
//	Logs(requestJson)          the support log's bundle rendered whole, the next message's or a sent one's: the app
//	                           process's ring locally, the daemon's (support.logs) remotely, as a remote Sidecar writes
//	                           through its daemon's
//	Settings(), SetSettings    the person's support key, their own act in the app's UI; remote: support.settings, and
//	                           the Confirm method support.set
//	AddListener(listener)      support_update's enumerated fields, which taint nothing (§28.9); remote: support.watch
//	                           long-polled on a session of its own, so the backend's call session stays free, until the
//	                           Sub or the Sidecar closes
//
// A refusal of the daemon's reaches the app as its error, which names a refusal's code and never a message's text.
// Files go to the daemon in the call itself, so a remote message's files together stay within the daemon's request
// bound (SidecarLocalSettings.RequestByteCount, 4 MiB with their JSON escaping), which the daemon refuses past.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The apps' support conversation on one Sidecar. Safe for concurrent use.
type SidecarSupport struct {
	sidecar  *Sidecar
	settings *sidecarSupportSettings
}

// A listener for the conversation's updates: each an update's JSON, its enumerated fields alone.
type SidecarSupportListener interface {
	OnUpdate(updateJson string)
}

// The support conversation's tunables on the remote backend.
type sidecarSupportSettings struct {
	// The bound of one support.watch call: the daemon's wait for an update (SupportSettings.WatchWait, 25 s) and a
	// call's margin: 35 s.
	WatchCallTimeout time.Duration
	// The first wait before support.watch is called again after a failure or a refusal: 1 s, doubling at each failure
	// in a row up to WatchRetryMaxInterval, and back to this once a call answers.
	WatchRetryMinInterval time.Duration
	// The longest such wait: 30 s.
	WatchRetryMaxInterval time.Duration
	// The longest a support wake reads before it answers (sidecar_support_notify.go): 25 s, inside the about 30 s an
	// app's OS gives a background push to finish in, so the app ends its window on Wake's answer.
	WakeTimeout time.Duration
	// The notifications one subscription holds for its listener before it drops the next: 64. A dropped notification's
	// event stays in the conversation, and the next notification's unread count covers it.
	NotificationQueueCount int
}

// The defaults, the only place one is set.
func defaultSidecarSupportSettings() *sidecarSupportSettings {
	return &sidecarSupportSettings{
		WatchCallTimeout:       35 * time.Second,
		WatchRetryMinInterval:  1 * time.Second,
		WatchRetryMaxInterval:  30 * time.Second,
		WakeTimeout:            25 * time.Second,
		NotificationQueueCount: 64,
	}
}

// The member's support conversation (§15.3).
func (self *Sidecar) Support() *SidecarSupport {
	return &SidecarSupport{sidecar: self, settings: defaultSidecarSupportSettings()}
}

// What Support answers with neither a local core nor a remote backend: a closed Sidecar.
var errSidecarSupportNoCore = errors.New("support: this Sidecar has no core, local or remote")

// The control methods the remote backend calls, as ur's cmd_support_conversation.go and cmd_support.go name them;
// TestSupportControlWireMatchesUr holds them to that source.
const (
	sidecarControlMethodSupportList     = "support.ls"
	sidecarControlMethodSupportShow     = "support.show"
	sidecarControlMethodSupportSend     = "support.send"
	sidecarControlMethodSupportDelete   = "support.delete"
	sidecarControlMethodSupportSettings = "support.settings"
	sidecarControlMethodSupportSet      = "support.set"
	sidecarControlMethodSupportWatch    = "support.watch"
	sidecarControlMethodSupportLogs     = "support.logs"
)

// The conversation methods' params, as ur's urSupportParams names them; the daemon reads params strictly.
type sidecarControlSupportParams struct {
	Network         string                       `json:"network,omitempty"`
	Entry           string                       `json:"entry,omitempty"`
	Status          string                       `json:"status,omitempty"`
	Text            string                       `json:"text,omitempty"`
	Kind            string                       `json:"kind,omitempty"`
	Logs            bool                         `json:"logs,omitempty"`
	Files           []*sidecarControlSupportFile `json:"files,omitempty"`
	SecretConfirmed bool                         `json:"secret_confirmed,omitempty"`
	Agents          string                       `json:"agents,omitempty"`
	AgentLogs       string                       `json:"agent_logs,omitempty"`
}

// One file of a message, as urSupportFile names it.
type sidecarControlSupportFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// support.logs' params, as urSupportLogsParams names them.
type sidecarControlSupportLogsParams struct {
	Network string `json:"network,omitempty"`
	Sent    string `json:"sent,omitempty"`
}

// support.logs' reply, as urSupportLogsReply names it.
type sidecarControlSupportLogsReply struct {
	Manifest *sidecar.SupportLogManifest       `json:"manifest"`
	Document *sidecar.SupportLogBundleDocument `json:"document"`
}

// support.ls' reply, as urSupportListReply names it.
type sidecarControlSupportListReply struct {
	Items []*sidecar.SupportItemStub `json:"items"`
}

// support.watch's reply, as urSupportWatchReply names it.
type sidecarControlSupportWatchReply struct {
	Updates []*sidecarControlSupportWatchLine `json:"updates"`
}

// One update of support.watch's, as urSupportWatchLine names it.
type sidecarControlSupportWatchLine struct {
	Update *sidecar.SupportUpdate `json:"update"`
}

// The local core's support client, or the remote backend to call; one of them.
func (self *SidecarSupport) backend() (*sidecar.SupportClient, sidecarBackend, error) {
	args := self.sidecar.computeArgs()
	switch {
	case args.local != nil:
		client, err := args.local.Support()
		return client, nil, err
	case args.remote != nil:
		return nil, args.remote, nil
	}
	return nil, nil, errSidecarSupportNoCore
}

// The network the remote backend's pairing bound, as a method's network argument.
func sidecarSupportNetwork(remote sidecarBackend) string {
	if named, ok := remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// The Sidecar's context, which its Close ends; a Sidecar made with none, as a test makes one, never ends.
func (self *SidecarSupport) ctx() context.Context {
	if ctx := self.sidecar.computeArgs().ctx; ctx != nil {
		return ctx
	}
	return context.Background()
}

// Conversation's request.
type sidecarSupportConversationRequest struct {
	Status string `json:"status,omitempty"`
}

// Conversation's answer: each item with its messages and replies.
type sidecarSupportConversation struct {
	Items []*sidecarSupportConversationItem `json:"items"`
}

// One item: its stub and, read whole, its messages and replies.
type sidecarSupportConversationItem struct {
	Stub     *sidecar.SupportItemStub      `json:"stub"`
	Messages []*sidecar.SupportReadMessage `json:"messages,omitempty"`
}

// The member's whole conversation, newest first: the person reads every item, each agent's messages under its name.
func (self *SidecarSupport) Conversation(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarSupportConversationRequest{}
		if requestJson != "" {
			if err := json.Unmarshal([]byte(requestJson), request); err != nil {
				return "", err
			}
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		ctx := self.ctx()
		var stubs []*sidecar.SupportItemStub
		read := func(entry string) (*sidecar.SupportReadResult, error) {
			return client.Read(ctx, &sidecar.SupportReadArgs{Entry: entry})
		}
		if remote != nil {
			reply := &sidecarControlSupportListReply{}
			params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote), Status: request.Status}
			if err := remote.call(ctx, sidecarControlMethodSupportList, params, false, reply); err != nil {
				return "", err
			}
			stubs = reply.Items
			read = func(entry string) (*sidecar.SupportReadResult, error) {
				result := &sidecar.SupportReadResult{}
				params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote), Entry: entry}
				return result, remote.call(ctx, sidecarControlMethodSupportShow, params, false, result)
			}
		} else if stubs, err = client.List(&sidecar.SupportListArgs{Status: request.Status, Scope: "member"}); err != nil {
			return "", err
		}
		conversation := &sidecarSupportConversation{Items: []*sidecarSupportConversationItem{}}
		for _, stub := range stubs {
			if stub == nil {
				continue
			}
			item := &sidecarSupportConversationItem{Stub: stub}
			if stub.Entry != "" {
				if result, err := read(stub.Entry); err == nil {
					item.Messages = result.Messages
				}
			}
			conversation.Items = append(conversation.Items, item)
		}
		encoded, err := json.Marshal(conversation)
		return string(encoded), err
	})
}

// Send's request: the person's message and what the person chose to attach.
type sidecarSupportSendRequest struct {
	Text  string `json:"text"`
	Entry string `json:"entry,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Logs  bool   `json:"logs,omitempty"`
	Files []*struct {
		Name string `json:"name"`
		Data []byte `json:"data"`
	} `json:"files,omitempty"`
	SecretConfirmed bool `json:"secret_confirmed,omitempty"`
}

// Send's answer: the outcome, or the refusal and its detail. The daemon's support.send answers the same shape
// (urSupportSendReply).
type sidecarSupportSendAnswer struct {
	Result  *sidecar.SupportSendResult `json:"result,omitempty"`
	Refusal string                     `json:"refusal,omitempty"`
	Detail  string                     `json:"detail,omitempty"`
}

// The person's own message, tier human.
func (self *SidecarSupport) Send(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarSupportSendRequest{}
		if err := json.Unmarshal([]byte(requestJson), request); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		answer := &sidecarSupportSendAnswer{}
		if remote != nil {
			params := &sidecarControlSupportParams{
				Network: sidecarSupportNetwork(remote), Text: request.Text, Entry: request.Entry, Kind: request.Kind, Logs: request.Logs,
				SecretConfirmed: request.SecretConfirmed,
			}
			for _, file := range request.Files {
				if file != nil {
					params.Files = append(params.Files, &sidecarControlSupportFile{Name: file.Name, Data: file.Data})
				}
			}
			// the person confirmed it in the app's UI, as support.send asks (D-CORE-13)
			if err := remote.call(self.ctx(), sidecarControlMethodSupportSend, params, true, answer); err != nil {
				return "", err
			}
		} else {
			files := []*sidecar.SupportFile{}
			for _, file := range request.Files {
				if file != nil {
					files = append(files, &sidecar.SupportFile{Name: file.Name, Data: file.Data})
				}
			}
			result, err := client.Send(self.ctx(), &sidecar.SupportSendArgs{
				Surface: sidecar.SupportSurfacePerson, Text: request.Text, Entry: request.Entry, Kind: request.Kind, Logs: request.Logs,
				Files: files, SecretConfirmed: request.SecretConfirmed,
			})
			answer.Result = result
			var refusal *sidecar.SupportRefusalError
			if errors.As(err, &refusal) {
				answer = &sidecarSupportSendAnswer{Refusal: string(refusal.Refusal), Detail: refusal.Error()}
			} else if err != nil {
				return "", err
			}
		}
		encoded, err := json.Marshal(answer)
		return string(encoded), err
	})
}

// Removes an item's bodies and attachments at the service.
func (self *SidecarSupport) Delete(entry string) error {
	_, err := traceWithReturnError(func() (bool, error) {
		client, remote, err := self.backend()
		if err != nil {
			return false, err
		}
		if remote != nil {
			params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote), Entry: entry}
			return true, remote.call(self.ctx(), sidecarControlMethodSupportDelete, params, true, nil)
		}
		return true, client.Delete(self.ctx(), entry)
	})
	return err
}

// Logs' request: the client_ref of a sent message whose bundle to show, empty for the next message's.
type sidecarSupportLogsRequest struct {
	Sent string `json:"sent,omitempty"`
}

// The support log's bundle rendered whole: the next message's, or a sent one's.
func (self *SidecarSupport) Logs(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarSupportLogsRequest{}
		if requestJson != "" {
			if err := json.Unmarshal([]byte(requestJson), request); err != nil {
				return "", err
			}
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		var document *sidecar.SupportLogBundleDocument
		switch {
		case remote != nil:
			reply := &sidecarControlSupportLogsReply{}
			params := &sidecarControlSupportLogsParams{Network: sidecarSupportNetwork(remote), Sent: request.Sent}
			if err := remote.call(self.ctx(), sidecarControlMethodSupportLogs, params, false, reply); err != nil {
				return "", err
			}
			if reply.Document == nil {
				return "", errors.New("the daemon answered no bundle")
			}
			document = reply.Document
		case request.Sent != "":
			compressed, err := client.SentBundle(request.Sent)
			if err != nil {
				return "", err
			}
			if document, err = sidecar.SupportLogBundleOpen(compressed, sidecar.DefaultSupportSettings().BundleMaxByteCount*16); err != nil {
				return "", err
			}
		default:
			supportLog := self.sidecar.supportLog
			if supportLog == nil {
				return "", errors.New("no support log runs in this app")
			}
			bundle, err := supportLog.log.Bundle(&sidecar.SupportLogBundleArgs{Host: &sidecar.SupportLogBundleHost{Version: Version}})
			if err != nil {
				return "", err
			}
			document = bundle.Document
		}
		encoded, err := json.Marshal(document)
		return string(encoded), err
	})
}

// The person's support key as JSON.
func (self *SidecarSupport) Settings() (string, error) {
	return traceWithReturnError(func() (string, error) {
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		key := &sidecar.SupportPersonKey{}
		if remote != nil {
			params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote)}
			if err := remote.call(self.ctx(), sidecarControlMethodSupportSettings, params, false, key); err != nil {
				return "", err
			}
		} else if key, err = client.PersonKey(); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(key)
		return string(encoded), err
	})
}

// on or off, as support.set takes a switch.
func sidecarSupportSwitch(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// Sets the person's support key: their own act in the app's UI.
func (self *SidecarSupport) SetSettings(settingsJson string) error {
	_, err := traceWithReturnError(func() (bool, error) {
		key := &sidecar.SupportPersonKey{}
		if err := json.Unmarshal([]byte(settingsJson), key); err != nil {
			return false, err
		}
		client, remote, err := self.backend()
		if err != nil {
			return false, err
		}
		if remote != nil {
			params := &sidecarControlSupportParams{
				Network: sidecarSupportNetwork(remote), Agents: sidecarSupportSwitch(key.Agents), AgentLogs: sidecarSupportSwitch(key.AgentLogs),
			}
			// the person's own act in the app's UI, as support.set asks
			return true, remote.call(self.ctx(), sidecarControlMethodSupportSet, params, true, &sidecar.SupportPersonKey{})
		}
		return true, client.SetPersonKey(key)
	})
	return err
}

// Adds listener for the conversation's updates; Close removes it. On the remote backend the daemon is asked first, so a
// daemon with no support client answers its error here rather than a listener that never hears.
func (self *SidecarSupport) AddListener(listener SidecarSupportListener) (Sub, error) {
	return traceWithReturnError(func() (Sub, error) {
		client, remote, err := self.backend()
		if err != nil {
			return nil, err
		}
		tell := func(update *sidecar.SupportUpdate) {
			encoded, err := json.Marshal(update)
			if err == nil {
				listener.OnUpdate(string(encoded))
			}
		}
		if remote == nil {
			return newSub(client.AddListener(tell)), nil
		}
		params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote)}
		if err := remote.call(self.ctx(), sidecarControlMethodSupportSettings, params, false, &sidecar.SupportPersonKey{}); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(self.ctx())
		go connect.HandleError(func() { self.watchRemote(ctx, remote, tell) })
		return newSub(cancel), nil
	})
}

// Long-polls support.watch on a session of its own until ctx ends, telling each update in order, and calling again
// after a failure or a refusal once its wait passes.
func (self *SidecarSupport) watchRemote(ctx context.Context, remote sidecarBackend, tell func(update *sidecar.SupportUpdate)) {
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
		reply := &sidecarControlSupportWatchReply{}
		params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote)}
		var err error
		if !ownSession {
			err = remote.call(callCtx, sidecarControlMethodSupportWatch, params, false, reply)
		} else {
			if channel == nil {
				channel, err = dialer.dialOwn(callCtx)
				if err != nil {
					channel = nil
				}
			}
			if channel != nil {
				err = channel.call(callCtx, sidecarControlMethodSupportWatch, params, false, reply)
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
		for _, line := range reply.Updates {
			if line != nil && line.Update != nil && ctx.Err() == nil {
				tell(line.Update)
			}
		}
	}
}
