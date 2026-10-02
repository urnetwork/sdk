package sdk

// The message-server binding: §4.2 frames over an existing connect.Client,
// §4.3 request/response correlation by `request_id`, and the borrow rule.
//
// This file is the transport and nothing else. It holds no key, no group and no
// session, and it never looks inside a request body: every body it carries is a
// `proto.Message` the caller built, and every response it returns is the one the
// server answered. Spec A §10.1's four code points are the whole vocabulary.
//
// ── The one rule in `connect` that is normative here ──────────────────────────
//
// Quoted from connect/transfer.go's own declaration of `ReceiveFunction`, rather
// than paraphrased, because paraphrasing it is how it gets broken:
//
//	"The frames, frame objects, and their message bytes are borrowed and valid
//	 only until the callback returns. Decode, copy, or MessagePoolShareReadOnly
//	 any data that must outlive the callback; never hand a borrowed Frame to an
//	 asynchronous send, goroutine, or channel."
//
// and:
//
//	"ReceiveFunction is invoked inline by the receive path. A blocked callback
//	 intentionally backpressures that path."
//
// Both halves bind this file. The first says what may cross the callback
// boundary — [messageTransport.receive] below unmarshals, which copies, and
// keeps a reference to neither a frame nor its bytes. The second says the
// callback may not block on a waiter — [messageTransport.deliver] removes the
// waiter from the correlation map under the same hold of the lock that found it
// and sends on a channel buffered by one, so the send has a free slot by
// construction and there is no second sender that could have taken it.
//
// ── There is no server push, and the receive path is a poll ───────────────────
//
// Verified against msgrepo at 0590aa3 rather than taken from a document:
// `Peer.buildRoutes` serves exactly four arms of §4.3's request oneof — Hello,
// CreateGroup, Submit and Fetch — and `grep -rn MessageMessageServerPush
// --include=*.go` over the whole of msgrepo returns nothing, so the push code
// point at 1002 has no emitter. `Peer.receive` answers only on the connection a
// request arrived over. A binding that registered a subscription and waited to
// be told things would wait forever; fetching is the only receive there is.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The refusals this binding raises itself. None of them is a `protocol.Reason`:
// a Reason is something a server decided, and everything below was decided on
// this side of the wire without one.
var (
	errMessageTransportNoClient = errors.New("message transport: this binding speaks over a connect client and there is none here to speak over")
	errMessageTransportNoServer = errors.New("message transport: every frame is addressed to the server's client_id and this one names no server")
	errMessageTransportNoArm    = errors.New("message transport: this message is not an arm of the request body oneof, so §4.3 gives it nowhere to travel")
	errMessageTransportRefused  = errors.New("message transport: the connect client would not take the frame, so the request is on no wire at all")

	// Property 3's typed timeout. A Call that gave up says so with this and with
	// a nil response; it never answers (nil, nil), which is the one answer a
	// caller cannot tell from success.
	errMessageTransportTimeout = errors.New("message transport: no response carrying this request_id arrived before the deadline")

	// Property 2's impossible case, asserted anyway: the correlator keys on
	// `request_id`, so a waiter cannot be handed an answer to another request
	// unless the correlator itself is wrong, and that is the one failure a
	// caller has no other way to see.
	errMessageTransportMiscorrelated = errors.New("message transport: a request was answered under another request's request_id")
)

// How long a Call waits when the config names no timeout.
//
// Generous, because what it protects against is a dispatcher that lost
// `request_id` — which is a hang rather than a slow answer, and a caller that
// hangs reports nothing at all.
const messageTransportDefaultTimeout = 30 * time.Second

// What this binding needs of a `*connect.Client`, and nothing more.
//
// It is an interface rather than the concrete client for one reason, and the
// reason is a ledger item rather than a test convenience: S2-7 is open — how a
// client actually reaches the message server is specified nowhere — so the
// client is INJECTED here and this file stands nothing up. Nothing in `sdk`
// constructs the connect client this binding runs on, and this task does not
// resolve that.
//
// The methods are spelled to connect's own signatures; `connect.ReceiveFunction`
// is the type alias itself, so a parameter added to it upstream is a compile
// error here and not a silent widening.
type messageTransportClient interface {
	SendWithTimeout(
		frame *protocol.Frame,
		destination connect.TransferPath,
		ackCallback connect.AckFunction,
		timeout time.Duration,
		opts ...any,
	) bool
	AddReceiveCallback(receiveCallback connect.ReceiveFunction) func()
}

// The real one satisfies it. A compile-time assertion rather than a comment,
// because the whole point of the seam is that production runs on the shipped
// client.
var _ messageTransportClient = (*connect.Client)(nil)

// This binding's collaborators. Everything whose zero value would be a silent
// hole is refused by [newMessageTransport].
type messageTransportConfig struct {
	// The connect client this binding speaks over. It stays the caller's:
	// [messageTransport.Close] unsubscribes and does not close it.
	Client messageTransportClient

	// The message server's client_id, which is the destination of every frame
	// this binding sends.
	Server connect.Id

	// The version offered at Hello and stamped on every later request. Zero
	// sends no version at all, which §4.3.1 reads as a client that did not
	// negotiate.
	ProtocolVersion uint32

	// How long a Call waits. Zero takes [messageTransportDefaultTimeout].
	Timeout time.Duration
}

// What crossed the wire and what became of it, counted on this side.
//
// Declared here, with the transport that owns them, because the counters are
// what Properties 2 and 3 are readable through and a type named in a return
// signature and declared nowhere is how a plan ships an unbuildable task.
type messageTransportCounts struct {
	// §4.2 frames handed to connect for requests.
	RequestFrames uint64

	// Frames that arrived at the §10.1 response code point. Frames of any other
	// type are not counted, because connect's own traffic is not this binding's.
	ResponseFrames uint64

	// Responses that decoded and reached the waiter that asked for them.
	Responses uint64

	// Responses that decoded and carried a `request_id` no waiter is waiting on
	// — because it never was one, or because its waiter has already timed out.
	// Counted rather than dropped in silence, so that "nothing arrived" and
	// "something arrived for nobody" are two readings and not one.
	Unmatched uint64

	// Frames that arrived at §10.1's fragment code point, whether or not the
	// reassembly they belong to ever completed.
	FragmentFrames uint64

	// §4.6 reassemblies that completed and produced a response.
	Reassembled uint64

	// §4.6 reassemblies this binding abandoned: an index that was not the one
	// the buffer was waiting for, a count that changed under it, or a part past
	// §4.6's ceiling. Counted so that an ABORT and a TIMEOUT are two readings
	// and not one -- they are the same silence to a caller that only watches
	// the clock, and they have different causes and different fixes.
	Aborted uint64

	// Calls that gave up. Property 3's other half: a timeout is a thing that
	// happened, not an absence.
	Timeouts uint64

	// The size of the correlation map right now, not a cumulative count. It is
	// how "a timed-out request left nothing behind" is read: a leak shows up
	// here as a number that never comes back down.
	Waiting uint64

	// The size of the §4.6 reassembly map right now, for Waiting's reason: a
	// request that timed out with fragments half-arrived must leave no buffer
	// behind either, and a buffer is the other map entry a waiter can strand.
	Reassembling uint64

	// §4.3.5 pushes that arrived and were handed to the OnPush callbacks.
	PushFrames uint64
}

// What a waiter is handed: the response, or the local refusal that ended the
// wait before one arrived.
//
// A struct rather than the response alone, because §4.6's abort is decided
// inside the receive callback and has nowhere else to go. Without it an
// abandoned reassembly is indistinguishable from a server that never answered,
// and the caller waits out the whole timeout to be told the wrong thing.
type messageTransportAnswer struct {
	response *protocol.MessageServerResponse
	err      error
}

// A binding to one message server, over one connect client.
type messageTransport struct {
	client          messageTransportClient
	server          connect.Id
	protocolVersion uint32
	timeout         time.Duration

	unsubscribe func()
	closed      sync.Once

	pushMutex        sync.Mutex
	pushCallbacks    map[uint64]func(*protocol.MessageServerPush)
	nextPushCallback uint64

	nextRequestId atomic.Uint64

	mutex   sync.Mutex
	waiting map[uint64]chan messageTransportAnswer
	partial map[uint64]*messageFragmentPartial
	counts  messageTransportCounts

	// §4.3.1's per-CONNECTION state, replaced unconditionally by every Hello.
	// See message_transport_hello.go, and S2-2 for what the replacement costs a
	// GroupSession that copied the nonce at construction.
	nonce        []byte
	nonceEpoch   uint64
	capabilities *protocol.Capabilities
}

func newMessageTransport(config *messageTransportConfig) (*messageTransport, error) {
	if config == nil || config.Client == nil {
		return nil, errMessageTransportNoClient
	}
	if config.Server == (connect.Id{}) {
		return nil, errMessageTransportNoServer
	}
	self := &messageTransport{
		client:          config.Client,
		server:          config.Server,
		protocolVersion: config.ProtocolVersion,
		timeout:         config.Timeout,
		waiting:         map[uint64]chan messageTransportAnswer{},
		partial:         map[uint64]*messageFragmentPartial{},
		pushCallbacks:   map[uint64]func(*protocol.MessageServerPush){},
	}
	if self.timeout <= 0 {
		self.timeout = messageTransportDefaultTimeout
	}
	self.unsubscribe = config.Client.AddReceiveCallback(self.receive)
	return self, nil
}

// Stop receiving. The connect client is the caller's and is not closed here.
func (self *messageTransport) Close() {
	self.closed.Do(func() {
		self.unsubscribe()
	})
}

func (self *messageTransport) Counts() messageTransportCounts {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	counts := self.counts
	counts.Waiting = uint64(len(self.waiting))
	counts.Reassembling = uint64(len(self.partial))
	return counts
}

// ── the receive path ─────────────────────────────────────────────────────────

// connect's receive callback.
//
// Every value here is borrowed for the duration of this call — the source, the
// frame slice with every frame and byte inside it, and the peer — so nothing
// below keeps a reference to any of them. The response is unmarshaled, which
// copies into a message of our own; the frame and its bytes are read and left
// behind.
//
// Nothing is handed to a goroutine or a channel from here. `deliver` sends on a
// waiter's channel, and the value it sends is the unmarshaled response, which is
// borrowed from nothing.
func (self *messageTransport) receive(source connect.TransferPath, frames []*protocol.Frame, from connect.Peer) {
	for _, frame := range frames {
		switch frame.GetMessageType() {
		case protocol.MessageType_MessageMessageServerResponse:
			self.countResponseFrame()
			response := &protocol.MessageServerResponse{}
			if proto.Unmarshal(frame.GetMessageBytes(), response) != nil {
				// a response that did not decode carries no `request_id` to
				// correlate, so there is no waiter to tell and nothing to answer
				continue
			}
			self.deliver(response)
		case protocol.MessageType_MessageMessageServerPush:
			// §4.3.5's push: the one code point that answers no request. It is not correlated;
			// it is handed to the OnPush callbacks (message_transport_push.go)
			self.deliverPush(frame)
		case protocol.MessageType_MessageMessageServerFragment:
			self.countFragmentFrame()
			fragment := &protocol.MessageServerFragment{}
			if proto.Unmarshal(frame.GetMessageBytes(), fragment) != nil {
				// a fragment that did not decode carries no `request_id`, so
				// there is no reassembly to abandon and no waiter to tell
				continue
			}
			assembled, complete, err := self.acceptFragment(fragment)
			if err != nil {
				self.abort(fragment.GetRequestId(), err)
				continue
			}
			if !complete {
				continue
			}
			response := &protocol.MessageServerResponse{}
			if err := proto.Unmarshal(assembled, response); err != nil {
				// REVIEW FINDING H. Unlike the response-frame arm above, the
				// `request_id` IS in hand here — every fragment carried it, and
				// it is the key the reassembly was filed under — so the waiter
				// that will never be answered is reachable and is TOLD, which is
				// what every other §4.6 abandonment does and for the reason
				// message_transport_fragment.go gives: a caller that waits
				// thirty seconds to learn what the receive path knew immediately
				// has been told the wrong thing about why.
				self.abort(fragment.GetRequestId(), fmt.Errorf(
					"%w: request %d, the %d byte(s) reassembled from %d fragment(s) are not a MessageServerResponse: %v",
					errMessageFragmentAborted, fragment.GetRequestId(), len(assembled), fragment.GetCount(), err))
				continue
			}
			self.deliver(response)
		default:
			// connect carries every binding's traffic over one callback, so the
			// code point is the only thing that says a frame is this binding's.
			// The case list above is the WHOLE read set and is what
			// TestMessageTransportReadsOnlyTheCodePointsThatAreItsOwn derives
			// its class from, so a code point read here without a case is not
			// expressible.
			continue
		}
	}
}

func (self *messageTransport) countResponseFrame() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.counts.ResponseFrames++
}

// Property 2 and Property 4, in eleven lines.
//
// The waiter is found and removed under one hold of the lock, so the channel
// this sends on has no other sender and a free slot in its buffer of one. The
// send is therefore non-blocking by construction rather than by timing, which is
// what "the callback never blocks on a waiter" has to mean on a path connect
// calls inline.
//
// A response nobody is waiting for is counted and dropped. It is never given to
// another waiter: the map is keyed on `request_id` and there is no fallback arm.
func (self *messageTransport) deliver(response *protocol.MessageServerResponse) {
	self.mutex.Lock()
	waiter, found := self.waiting[response.GetRequestId()]
	if found {
		delete(self.waiting, response.GetRequestId())
		self.counts.Responses += 1
	} else {
		self.counts.Unmatched += 1
	}
	self.mutex.Unlock()
	if found {
		waiter <- messageTransportAnswer{response: response}
	}
}

// ── the send path ────────────────────────────────────────────────────────────

// One request, sent and answered.
//
// The waiter is registered BEFORE the send, because a response that arrives
// before its waiter does is a response this binding would file as uncorrelated —
// a correlation failure invented on this side of the wire, and exactly the
// number Property 2 is written to make visible.
func (self *messageTransport) Call(ctx context.Context, body proto.Message) (*protocol.MessageServerResponse, error) {
	request := &protocol.MessageServerRequest{
		RequestId:       self.nextRequestId.Add(1),
		ProtocolVersion: self.protocolVersion,
	}
	if err := setMessageServerRequestBody(request, body); err != nil {
		return nil, err
	}
	// §4.3.1's two local refusals, raised BEFORE a waiter is registered and
	// before anything reaches a wire: a request refused here costs no round
	// trip and leaves no correlation entry. See message_transport_hello.go.
	if err := self.refuseBeforeHello(body); err != nil {
		return nil, err
	}
	if err := self.refuseOverCapability(request); err != nil {
		return nil, err
	}

	// buffered by one: see [messageTransport.deliver] for why one is enough and
	// why it is what keeps connect's receive path unblocked
	waiter := make(chan messageTransportAnswer, 1)
	self.mutex.Lock()
	self.waiting[request.GetRequestId()] = waiter
	self.mutex.Unlock()

	if err := self.send(request); err != nil {
		self.forget(request.GetRequestId())
		return nil, err
	}

	timer := time.NewTimer(self.timeout)
	defer timer.Stop()
	select {
	case answer := <-waiter:
		if answer.err != nil {
			// §4.6 abandoned the reassembly this response was arriving in. The
			// map entry went with it, inside [messageTransport.abort]
			return nil, answer.err
		}
		if answer.response.GetRequestId() != request.GetRequestId() {
			return nil, fmt.Errorf("%w: request %d was answered under request_id %d",
				errMessageTransportMiscorrelated, request.GetRequestId(), answer.response.GetRequestId())
		}
		return answer.response, nil
	case <-ctx.Done():
		self.forget(request.GetRequestId())
		return nil, fmt.Errorf("message transport: request %d abandoned: %w", request.GetRequestId(), ctx.Err())
	case <-timer.C:
		self.forget(request.GetRequestId())
		self.countTimeout()
		return nil, fmt.Errorf("%w: request %d, after %v",
			errMessageTransportTimeout, request.GetRequestId(), self.timeout)
	}
}

// Both map entries, and nothing else, because there is nothing else: no
// goroutine was started for this request, and the waiter channel is
// unreferenced once the entry is gone.
//
// The §4.6 reassembly buffer goes with the waiter. A request that timed out
// with half its response arrived would otherwise leave a buffer keyed on a
// request_id nothing is waiting for, which is the same leak as the correlation
// entry and is not visible in the same counter -- see Counts().Reassembling.
func (self *messageTransport) forget(requestId uint64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	delete(self.waiting, requestId)
	delete(self.partial, requestId)
}

func (self *messageTransport) countTimeout() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.counts.Timeouts += 1
}

// The request on the wire: one §4.2 frame at §10.1's request code point, or
// §4.6's fragments of it when it does not fit in a part.
//
// The cut is [messageTransport.fragments] and the part size is its constant.
// Nothing here chooses a budget, which is the point: a second place that could
// choose one is a second place for the bound to live.
func (self *messageTransport) send(request *protocol.MessageServerRequest) error {
	frames, err := self.fragments(request)
	if err != nil {
		return err
	}
	for index, frame := range frames {
		if !self.client.SendWithTimeout(frame, connect.DestinationId(self.server), nil, -1) {
			// this frame and every frame after it are on no wire, so their
			// buffers are ours to give back. The ones already handed over are
			// connect's now
			messageFragmentReturn(frames[index:])
			return fmt.Errorf("%w: request %d, frame %d of %d",
				errMessageTransportRefused, request.GetRequestId(), index+1, len(frames))
		}
		self.mutex.Lock()
		self.counts.RequestFrames += 1
		self.mutex.Unlock()
	}
	return nil
}

// The arm of the request's `body` oneof that carries this type, read out of the
// compiled descriptor.
//
// A switch listing the typed wrappers by hand is where a copy-paste puts a Fetch
// body in the Submit arm, and §4.3.8's op byte is the arm's field number, so a
// binding that wrote the arms down twice would have two places to disagree.
// There are FIFTEEN of them at connect 71d2482 —
// `grep -oE 'type MessageServerRequest_[A-Za-z]+ struct' protocol/message.pb.go | wc -l`
// — and the number is here as a measurement rather than as a bound: nothing in
// this function knows it, which is the point.
func setMessageServerRequestBody(request *protocol.MessageServerRequest, body proto.Message) error {
	if body == nil {
		return errMessageTransportNoArm
	}
	oneof := request.ProtoReflect().Descriptor().Oneofs().ByName("body")
	if oneof == nil {
		return errMessageTransportNoArm
	}
	want := body.ProtoReflect().Descriptor().FullName()
	for index := 0; index < oneof.Fields().Len(); index += 1 {
		field := oneof.Fields().Get(index)
		if field.Kind() != protoreflect.MessageKind || field.Message().FullName() != want {
			continue
		}
		request.ProtoReflect().Set(field, protoreflect.ValueOfMessage(body.ProtoReflect()))
		return nil
	}
	return fmt.Errorf("%w: %s", errMessageTransportNoArm, want)
}
