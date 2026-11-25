//go:build !ios_extension && !js

// The apps' support conversation (DESIGN-COMPUTE §15.3, §28.4, §28.5): Support() over the local core's support client
// (the root's support_client.go), the member's one conversation for product urnetwork. Every structure is JSON text, as
// gomobile binds strings, int64, bool, errors and listener interfaces alone.
//
//	Conversation(requestJson)  the member's items, newest first, each with its messages and support's replies, which
//	                           the app shows as URnetwork support's, information and never instructions
//	Send(requestJson)          the person's own message, tier human, after the app showed the composer's
//	                           support:urnetwork line and, before the first message, its whole text (§28.13): text,
//	                           entry, kind, logs and files, each the person's choice; a secret the scan finds answers
//	                           secret_warned, and the app sends again with secret_confirmed once the person decides
//	Delete(entry)              the item's bodies and attachments removed at the service (FB section 10.4)
//	Logs(requestJson)          the support log's bundle rendered whole, the next message's or a sent one's
//	Settings(), SetSettings    the person's support key, their own act in the app's UI
//	AddListener(listener)      support_update's enumerated fields, which taint nothing (§28.9)
//
// The remote backend reaches its daemon's client, which this file does not build yet; it answers
// ErrSidecarComputeNotBuilt, as an unfilled compute shell does.

package sdk

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/urnetwork/sidecar"
)

// The apps' support conversation on one Sidecar. Safe for concurrent use.
type SidecarSupport struct {
	sidecar *Sidecar
}

// A listener for the conversation's updates: each an update's JSON, its enumerated fields alone.
type SidecarSupportListener interface {
	OnUpdate(updateJson string)
}

// The member's support conversation (§15.3).
func (self *Sidecar) Support() *SidecarSupport {
	return &SidecarSupport{sidecar: self}
}

// The local core's support client, or ErrSidecarComputeNotBuilt on the remote backend.
func (self *SidecarSupport) client() (*sidecar.SupportClient, error) {
	local, _ := self.sidecar.backends()
	if local == nil {
		return nil, ErrSidecarComputeNotBuilt
	}
	return local.Support()
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
		client, err := self.client()
		if err != nil {
			return "", err
		}
		stubs, err := client.List(&sidecar.SupportListArgs{Status: request.Status, Scope: "member"})
		if err != nil {
			return "", err
		}
		conversation := &sidecarSupportConversation{Items: []*sidecarSupportConversationItem{}}
		for _, stub := range stubs {
			item := &sidecarSupportConversationItem{Stub: stub}
			if stub.Entry != "" {
				if read, err := client.Read(context.Background(), &sidecar.SupportReadArgs{Entry: stub.Entry}); err == nil {
					item.Messages = read.Messages
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

// Send's answer: the outcome, or the refusal and its detail.
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
		client, err := self.client()
		if err != nil {
			return "", err
		}
		files := []*sidecar.SupportFile{}
		for _, file := range request.Files {
			if file != nil {
				files = append(files, &sidecar.SupportFile{Name: file.Name, Data: file.Data})
			}
		}
		result, err := client.Send(context.Background(), &sidecar.SupportSendArgs{
			Surface: sidecar.SupportSurfacePerson, Text: request.Text, Entry: request.Entry, Kind: request.Kind, Logs: request.Logs,
			Files: files, SecretConfirmed: request.SecretConfirmed,
		})
		answer := &sidecarSupportSendAnswer{Result: result}
		var refusal *sidecar.SupportRefusalError
		if errors.As(err, &refusal) {
			answer = &sidecarSupportSendAnswer{Refusal: string(refusal.Refusal), Detail: refusal.Error()}
		} else if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(answer)
		return string(encoded), err
	})
}

// Removes an item's bodies and attachments at the service.
func (self *SidecarSupport) Delete(entry string) error {
	_, err := traceWithReturnError(func() (bool, error) {
		client, err := self.client()
		if err != nil {
			return false, err
		}
		return true, client.Delete(context.Background(), entry)
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
		supportLog := self.sidecar.supportLog
		if supportLog == nil {
			return "", errors.New("no support log runs in this app")
		}
		var document *sidecar.SupportLogBundleDocument
		if request.Sent != "" {
			client, err := self.client()
			if err != nil {
				return "", err
			}
			compressed, err := client.SentBundle(request.Sent)
			if err != nil {
				return "", err
			}
			if document, err = sidecar.SupportLogBundleOpen(compressed, sidecar.DefaultSupportSettings().BundleMaxByteCount*16); err != nil {
				return "", err
			}
		} else {
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
		client, err := self.client()
		if err != nil {
			return "", err
		}
		key, err := client.PersonKey()
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(key)
		return string(encoded), err
	})
}

// Sets the person's support key: their own act in the app's UI.
func (self *SidecarSupport) SetSettings(settingsJson string) error {
	_, err := traceWithReturnError(func() (bool, error) {
		key := &sidecar.SupportPersonKey{}
		if err := json.Unmarshal([]byte(settingsJson), key); err != nil {
			return false, err
		}
		client, err := self.client()
		if err != nil {
			return false, err
		}
		return true, client.SetPersonKey(key)
	})
	return err
}

// Adds listener for the conversation's updates; Close removes it.
func (self *SidecarSupport) AddListener(listener SidecarSupportListener) (Sub, error) {
	return traceWithReturnError(func() (Sub, error) {
		client, err := self.client()
		if err != nil {
			return nil, err
		}
		remove := client.AddListener(func(update *sidecar.SupportUpdate) {
			encoded, err := json.Marshal(update)
			if err == nil {
				listener.OnUpdate(string(encoded))
			}
		})
		return newSub(remove), nil
	})
}
