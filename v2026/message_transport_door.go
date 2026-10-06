//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// THE DOOR ONTO [messageTransport], AND IT EXISTS BECAUSE THE SEAM IS NOT IN THIS PACKAGE.
//
// The §10.1 binding is unexported and stays unexported: every property message_transport_test.go
// holds is held over the unexported type, and an exported copy of the binding would be a second
// thing to keep true. What is exported here is a WRAPPER with no behaviour of its own -- every
// method below forwards, and none of them decides anything.
//
// WHY A WRAPPER AT ALL. The send/receive seam of the alpha lives in package
// github.com/urnetwork/sdk/urmessage rather than here, for a reason that is a measurement rather
// than a taste: TestNoProductionSourceOfPackageSdkSpellsAStreamKeyFieldName refuses the
// identifiers `GroupId` and `SenderHandle` in EVERY production file of package sdk's own
// directory, and the seam spells both on every second line -- they are field names of
// message.RecordHeader, of protocol.Record, of protocol.FetchRequest and of protocol.SubmitRequest,
// which have nothing to do with messagegroup.StreamKey. Widening that gate to let the seam past
// would be a gate narrowed to fit the code it was written to catch. Moving the seam one directory
// down leaves the gate exactly as strict as it was and costs this file.
//
// THE CONNECT CLIENT IS STILL THE CALLER'S AND THIS PACKAGE STILL STANDS NOTHING UP. S2-7 is open
// and this does not resolve it: [MessageTransportConfig.Client] is injected, [MessageTransport.Close]
// unsubscribes and does not close the client, and nothing here dials, authenticates or routes.

// MessageTransportClient is what a transport needs of a connect client. It is
// [messageTransportClient] under another name, so the one declaration of the method set stays the
// unexported one and an exported second copy cannot drift from it.
type MessageTransportClient = messageTransportClient

// MessageTransportConfig is [messageTransportConfig]'s exported shape. Each field has the meaning
// its unexported twin documents.
type MessageTransportConfig struct {
	// The connect client this transport speaks over. It stays the caller's.
	Client MessageTransportClient

	// The message server's client_id: the destination of every frame.
	Server connect.Id

	// The version offered at Hello and stamped on every later request.
	ProtocolVersion uint32

	// How long a Call waits. Zero takes the binding's own default.
	Timeout time.Duration
}

// MessageTransport is one §10.1 binding to one message server.
type MessageTransport struct {
	inner *messageTransport
}

// NewMessageTransport opens a binding. The client is not dialed, not authenticated and not closed
// by this package.
func NewMessageTransport(config *MessageTransportConfig) (*MessageTransport, error) {
	if config == nil {
		return nil, errMessageTransportNoClient
	}
	inner, err := newMessageTransport(&messageTransportConfig{
		Client:          config.Client,
		Server:          config.Server,
		ProtocolVersion: config.ProtocolVersion,
		Timeout:         config.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return &MessageTransport{inner: inner}, nil
}

// Hello performs §4.3.1: negotiate a version, take this connection's nonce, cache what the server
// advertised.
func (self *MessageTransport) Hello(ctx context.Context, versions ...uint32) (protocol.Reason, *protocol.HelloResponse, error) {
	return self.inner.Hello(ctx, versions...)
}

// Call sends one request and waits for the response correlated to it.
func (self *MessageTransport) Call(ctx context.Context, body proto.Message) (*protocol.MessageServerResponse, error) {
	return self.inner.Call(ctx, body)
}

// Nonce is the `server_nonce` this connection was issued, or nil. A copy.
func (self *MessageTransport) Nonce() []byte {
	return self.inner.Nonce()
}

// NonceEpoch is how many Hellos this transport has completed.
//
// IT COUNTS HELLOS AND NOT CONNECTIONS, which is Wave 2's filed Finding E and is carried through
// this door verbatim rather than softened: a connection replaced underneath the binding without a
// Hello through it leaves a superseded nonce readable at an unchanged number. What the seam does
// about the half this number cannot see is in urmessage's own document.
func (self *MessageTransport) NonceEpoch() uint64 {
	return self.inner.NonceEpoch()
}

// Capabilities is §4.3.1's advertisement from the last Hello, or nil. A clone.
func (self *MessageTransport) Capabilities() *protocol.Capabilities {
	return self.inner.Capabilities()
}

// Close stops receiving. The connect client is the caller's and is not closed.
// OnPush registers a callback for every §4.3.5 push. It runs on the receive goroutine and must not
// block. The answer unregisters it.
func (self *MessageTransport) OnPush(callback func(*protocol.MessageServerPush)) func() {
	return self.inner.OnPush(callback)
}

// OnSessionReplaced registers a callback for every session the client opens after its first,
// when the client can see them ([MessageRouteClient] can). The registrant says Hello again.
func (self *MessageTransport) OnSessionReplaced(callback func()) func() {
	return self.inner.OnSessionReplaced(callback)
}

func (self *MessageTransport) Close() {
	self.inner.Close()
}
