//go:build !ios_extension && !js

// The support push topic's side of the apps' support conversation (DESIGN-COMPUTE §28.9, FB section 7; owner decision
// 783): what an app binds so support's reply reaches its person, on the Support tab or, with the app closed, as a
// notification the app composes itself.
//
//	Wake(callback)                     the app's vendor wake of topic class support, §18.7's {"t":"support","v":...}:
//	                                   the local core reads the conversation now and answers once that read is done,
//	                                   Ok with the notifications it raised in Value, so the app ends its OS background
//	                                   window then; the remote backend's daemon holds its own push and reads on it, so
//	                                   Wake answers Ok at once
//	AddNotificationListener(listener)  a notification per reply or move of support's: its enumerated fields and the
//	                                   person's unread replies across the conversation, from which the app composes its
//	                                   own words, "URnetwork support replied on #100171", with no reply's text, so
//	                                   Apple and Google, who carry the app's notification, see none; the person's own
//	                                   messages, their agents' and the acknowledgments raise none
//	Unread()                           the person's unread replies, the tab's badge before any notification comes
//
// The Support tab's screens (DESIGN-UI.md S1 to S3) bind these as a view controller beside
// sidecar_support_conversation.go: S1 lists Conversation, whose stubs carry each item's number and status chip and
// whose messages each name their sender and tier, reloading it on AddListener's update, and the notification hook
// speaks for it while it is off screen; S2's previews are Logs; S1's composer is Send and Delete. A listener runs on a
// goroutine of its subscription's own behind a bounded queue, so an app slow to take one stalls neither the support
// client's loop nor the daemon's watch; past the bound a notification is dropped, its event still in the conversation.

package sdk

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The vendor wake's topic class for the support conversation, the t of §18.7's payload: an app's push handler calls
// Support().Wake for a payload of this class.
const SidecarWakeClassSupport = sidecar.PushTopicSupport

// A listener for the notifications an app composes its own words from: each a notification's JSON.
type SidecarSupportNotificationListener interface {
	OnNotification(notificationJson string)
}

// One notification as the app reads it: support's reply or move, its enumerated fields, and the person's unread
// replies, absent when they could not be read.
type sidecarSupportNotification struct {
	sidecar.SupportNotification
	Unread *int `json:"unread,omitempty"`
}

// The app's vendor wake of class support: reads the conversation now, and answers once, with the notifications the read
// raised in Value, or the read's refusal; within WakeTimeout.
func (self *SidecarSupport) Wake(callback SidecarResultCallback) {
	trace(func() {
		go func() {
			result, err := traceWithReturnError(func() (*SidecarResult, error) {
				client, remote, err := self.backend()
				switch {
				case err != nil:
					return sidecarResultOf("", err), nil
				case remote != nil:
					// the daemon's own push wakes its client, which reads on it
					return sidecarResultOf("0", nil), nil
				}
				raised := atomic.Int64{}
				remove := client.AddListener(func(update *sidecar.SupportUpdate) {
					if _, ok := sidecar.SupportNotificationOf(update); ok {
						raised.Add(1)
					}
				})
				defer remove()
				ctx, cancel := context.WithTimeout(self.ctx(), self.settings.WakeTimeout)
				defer cancel()
				if err := client.Wake(ctx); err != nil {
					return sidecarResultOf("", err), nil
				}
				return sidecarResultOf(strconv.FormatInt(raised.Load(), 10), nil), nil
			})
			if callback != nil {
				callback.Result(result, err)
			}
		}()
	})
}

// Adds listener for support's replies and moves; Close removes it. On the remote backend the daemon is asked first, as
// AddListener asks it, so a daemon with no support client answers its error here rather than a listener that never
// hears.
func (self *SidecarSupport) AddNotificationListener(listener SidecarSupportNotificationListener) (Sub, error) {
	return traceWithReturnError(func() (Sub, error) {
		client, remote, err := self.backend()
		if err != nil {
			return nil, err
		}
		if remote != nil {
			params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote)}
			key := &sidecar.SupportPersonKey{}
			if err := remote.call(self.ctx(), sidecarControlMethodSupportSettings, params, false, key); err != nil {
				return nil, err
			}
		}
		ctx, cancel := context.WithCancel(self.ctx())
		notifications := make(chan *sidecar.SupportNotification, max(self.settings.NotificationQueueCount, 1))
		tell := func(update *sidecar.SupportUpdate) {
			notification, raised := sidecar.SupportNotificationOf(update)
			if !raised {
				return
			}
			select {
			case notifications <- notification:
			default:
				// past the bound: the conversation holds the event, and the next notification's unread covers it
			}
		}
		go connect.HandleError(func() { self.tellNotifications(ctx, client, remote, notifications, listener) })
		if remote != nil {
			go connect.HandleError(func() { self.watchRemote(ctx, remote, tell) })
			return newSub(cancel), nil
		}
		removeListener := client.AddListener(tell)
		return newSub(func() {
			removeListener()
			cancel()
		}), nil
	})
}

// Tells listener each notification in order, with the person's unread replies read as it goes, until ctx ends.
func (self *SidecarSupport) tellNotifications(
	ctx context.Context,
	client *sidecar.SupportClient,
	remote sidecarBackend,
	notifications <-chan *sidecar.SupportNotification,
	listener SidecarSupportNotificationListener,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case notification := <-notifications:
			told := &sidecarSupportNotification{SupportNotification: *notification}
			if unread, err := self.unread(ctx, client, remote); err == nil {
				told.Unread = &unread
			}
			encoded, err := json.Marshal(told)
			if err != nil || ctx.Err() != nil {
				continue
			}
			listener.OnNotification(string(encoded))
		}
	}
}

// The person's unread replies across the conversation.
func (self *SidecarSupport) Unread() (int, error) {
	return traceWithReturnError(func() (int, error) {
		client, remote, err := self.backend()
		if err != nil {
			return 0, err
		}
		return self.unread(self.ctx(), client, remote)
	})
}

// The person's unread replies: the local client's count, or the sum of the daemon's stubs for the person.
func (self *SidecarSupport) unread(
	ctx context.Context,
	client *sidecar.SupportClient,
	remote sidecarBackend,
) (int, error) {
	if remote == nil {
		return client.UnreadReplies()
	}
	reply := &sidecarControlSupportListReply{}
	params := &sidecarControlSupportParams{Network: sidecarSupportNetwork(remote)}
	if err := remote.call(ctx, sidecarControlMethodSupportList, params, false, reply); err != nil {
		return 0, err
	}
	unread := 0
	for _, stub := range reply.Items {
		if stub != nil {
			unread += stub.UnreadReplies
		}
	}
	return unread, nil
}
