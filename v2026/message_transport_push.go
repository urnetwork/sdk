//go:build !sdk_mobile_bind

package sdk

import (
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// §4.3.5's PUSH, AND THE ONE RECONNECT THIS BINDING CAN SEE.
//
// A push is the one message-server code point that answers no request: it carries no request_id
// and correlates with nothing. [messageTransport.receive] reads it and hands it here, and this
// file hands it to every [messageTransport.OnPush] callback. Nothing is queued and nothing is
// started: the callbacks run on the client's receive goroutine and must not block, which is the
// rule the receive callback itself already lives under.
//
// THE RECONNECT. NonceEpoch counts Hellos, and a connection replaced underneath this binding
// without a Hello through it used to be visible only as a refusal (S2-2). A client that knows when
// its session to the server was replaced -- [MessageRouteClient] does, because a new WebSocket
// session IS a new connection at the server -- says so through [messageSessionReplacer], and this
// binding passes that on to whoever asked in [messageTransport.OnSessionReplaced]. It does not say
// Hello itself, because it starts no goroutine (TestTheMessageTransportStartsNoGoroutine): the
// device does, on its own caller's goroutine, at its next operation.

// messageSessionReplacer is the optional half of a client that can see its own reconnects.
type messageSessionReplacer interface {
	OnSessionReplaced(callback func()) func()
}

func (self *messageTransport) deliverPush(frame *protocol.Frame) {
	push := &protocol.MessageServerPush{}
	if proto.Unmarshal(frame.GetMessageBytes(), push) != nil {
		return
	}
	self.mutex.Lock()
	self.counts.PushFrames += 1
	self.mutex.Unlock()
	self.pushMutex.Lock()
	callbacks := make([]func(*protocol.MessageServerPush), 0, len(self.pushCallbacks))
	for _, callback := range self.pushCallbacks {
		callbacks = append(callbacks, callback)
	}
	self.pushMutex.Unlock()
	for _, callback := range callbacks {
		callback(push)
	}
}

// OnPush registers a callback for every §4.3.5 push this binding receives. It runs on the client's
// receive goroutine and must not block.
func (self *messageTransport) OnPush(callback func(*protocol.MessageServerPush)) func() {
	self.pushMutex.Lock()
	defer self.pushMutex.Unlock()
	id := self.nextPushCallback
	self.nextPushCallback += 1
	self.pushCallbacks[id] = callback
	return func() {
		self.pushMutex.Lock()
		defer self.pushMutex.Unlock()
		delete(self.pushCallbacks, id)
	}
}

// OnSessionReplaced registers a callback for every session the client under this binding opens
// after its first, when that client can see them; otherwise it registers nothing. The callback runs
// on the client's own goroutine and must not block. Whoever registers is the one that says Hello
// again: the server holds no connection for the new session until somebody does.
func (self *messageTransport) OnSessionReplaced(callback func()) func() {
	if replacer, ok := self.client.(messageSessionReplacer); ok {
		return replacer.OnSessionReplaced(callback)
	}
	return func() {}
}
