package sdk

// Task 5's four properties, driven without a message server.
//
// ── WHAT THIS FILE CANNOT SEE, stated first because it bounds every number
//    below ────────────────────────────────────────────────────────────────────
//
// `sdk` cannot import `github.com/urnetwork/message-server`: it is a different
// module, `sdk/go.mod` neither requires nor replaces it, and adding a replace is
// out of scope for this task. So nothing here runs against the real server, and
// nothing here runs over a real `connect.Client` either — the client is injected
// through `messageTransportConfig` (S2-7 is open: how a client actually REACHES
// the message server is specified nowhere, and this task does not resolve it),
// and every test below injects [messageTransportFake] in its place.
//
// Concretely, this file establishes NOTHING about:
//
//   - whether the frames this binding emits are the frames the server accepts.
//     `msgrepo/cmd/message-server/twoclient_test.go` is CP3c and is the only
//     place that is asserted; it was read for shape and deliberately not
//     imported.
//   - whether `connect` really delivers responses on the callback this binding
//     registers, or in what order, or on which goroutine.
//   - whether the borrow rule is OBEYED AT RUN TIME under real pool reuse. The
//     `-race` detector cannot run in this sandbox (CGO_ENABLED=0, no C
//     compiler), so Property 1 below is a gate over the CODE and not a
//     behaviour test. That is not a workaround for the missing detector: a
//     borrowed slice handed to a goroutine that copies it promptly is a race a
//     behaviour test WINS most of the time, and a gate that passes there has
//     measured luck. The code gate refuses the construction instead of timing
//     it.
//   - §4.6 fragmentation in either direction. Task 6 owns the cut and the
//     reassembler; this binding sends every request as one frame and reads only
//     the §10.1 response code point.
//   - Hello, `server_nonce`, and `Capabilities`. Task 7.
//
// What it does establish is correlation, copying, refusal typing and
// non-blocking delivery, and every one of those is observable by driving the
// receive callback directly, which is what these tests do.
//
// WHAT WAS IN REACH AND WAS NOT LOOKED AT, added after the Task 5 review,
// because the list above reads as exhaustive and was not. None of these was
// blocked by the missing server; the injected client already received all of
// them. Each now has an assertion, named here so the next reader can tell a
// boundary from a horizon:
//
//   - the frame's ADDRESS. `TestMessageTransportAddressesEveryFrameToTheConfiguredServer`
//     reads every destination the client was handed. Before it, `SendWithTimeout`
//     took `destination` and dropped it, and `connect.DestinationId(self.server)`
//     could be the zero id with the whole suite green.
//   - the SEND-side code point, which was pinned only incidentally by a fake that
//     refused everything else.
//   - the RECEIVE-side code-point filter, which was untested because the fake
//     only ever delivered response frames. The class of "not ours" is now read
//     off protocol's compiled enum.
//   - `config.ProtocolVersion`, which is documented as stamped on every request
//     and was never read back.
//   - `Counts().RequestFrames` and `Counts().ResponseFrames`, two of the TEN
//     counters `messageTransportCounts` declares at this commit — the Task 5
//     review said six, which was true of Task 5 and stopped being true when
//     Task 6 added the four §4.6 counters — which no test read.
//   - the `ctx.Done()` arm of `Call`, which no test reached: no test in the suite
//     constructed a cancellable context.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ─────────────────────────────────────────────────────────────────────────────
// The injected client
// ─────────────────────────────────────────────────────────────────────────────

// A stand-in for the one method pair this binding uses of a `*connect.Client`.
//
// It decodes each frame it is handed and keeps the DECODED request, never the
// frame: a fake that retained the frame would be the one place in this file
// allowed to break the rule the file is about.
type messageTransportFake struct {
	mutex      sync.Mutex
	receive    connect.ReceiveFunction
	subscribed int
	requests   []*protocol.MessageServerRequest
	refuse     bool

	// Every frame's ADDRESS and CODE POINT, recorded rather than taken and
	// dropped. Review finding F4: this fake already RECEIVED `destination` and
	// never looked at it, so `connect.DestinationId(self.server)` could be
	// replaced by the zero id with the whole suite green — a gap that was
	// inside the fake's reach rather than behind the missing server.
	destinations []connect.TransferPath
	sent         []protocol.MessageType

	// §4.6's fragments of a request too large for one frame, decoded in the
	// order they were handed over, and this side's own reassembly of them.
	//
	// The reassembly is deliberately written HERE and not borrowed from the
	// binding: a fake that reassembled with the binding's reassembler would
	// make a fragmented request answerable by the same code that cut it, and a
	// mistake in the cutting would be undone by the same mistake in the joining.
	fragments []*protocol.MessageServerFragment
	joining   []byte

	// Called inline from inside SendWithTimeout, with the transport's own
	// goroutine still inside `send` and not yet in its select. Property 4's
	// whole construction.
	onSend func(request *protocol.MessageServerRequest)
}

func (self *messageTransportFake) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.receive = receiveCallback
	self.subscribed += 1
	return func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.subscribed -= 1
	}
}

func (self *messageTransportFake) SendWithTimeout(
	frame *protocol.Frame,
	destinationId connect.Id,
	ackCallback connect.AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	self.mutex.Lock()
	self.destinations = append(self.destinations, connect.DestinationId(destinationId))
	self.sent = append(self.sent, frame.GetMessageType())
	refuse := self.refuse
	self.mutex.Unlock()

	switch frame.GetMessageType() {
	case protocol.MessageType_MessageMessageServerRequest:
		request := &protocol.MessageServerRequest{}
		if proto.Unmarshal(frame.GetMessageBytes(), request) != nil {
			return false
		}
		self.mutex.Lock()
		self.requests = append(self.requests, request)
		onSend := self.onSend
		self.mutex.Unlock()
		if refuse {
			return false
		}
		if onSend != nil {
			onSend(request)
		}
		return true
	case protocol.MessageType_MessageMessageServerFragment:
		fragment := &protocol.MessageServerFragment{}
		if proto.Unmarshal(frame.GetMessageBytes(), fragment) != nil {
			return false
		}
		self.mutex.Lock()
		self.fragments = append(self.fragments, fragment)
		self.joining = append(self.joining, fragment.GetPart()...)
		complete := fragment.GetIndex()+1 == fragment.GetCount()
		assembled := self.joining
		if complete {
			self.joining = nil
		}
		self.mutex.Unlock()
		if refuse {
			return false
		}
		if !complete {
			return true
		}
		request := &protocol.MessageServerRequest{}
		if proto.Unmarshal(assembled, request) != nil {
			return false
		}
		self.mutex.Lock()
		self.requests = append(self.requests, request)
		onSend := self.onSend
		self.mutex.Unlock()
		if onSend != nil {
			onSend(request)
		}
		return true
	}
	// a code point this binding has no business sending
	return false
}

// Every destination this fake was handed, in order.
func (self *messageTransportFake) addressed() []connect.TransferPath {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]connect.TransferPath(nil), self.destinations...)
}

// Every code point this fake was handed, in order.
func (self *messageTransportFake) codePoints() []protocol.MessageType {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]protocol.MessageType(nil), self.sent...)
}

// The §4.6 fragments this fake was handed, in order.
func (self *messageTransportFake) cutFragments() []*protocol.MessageServerFragment {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]*protocol.MessageServerFragment(nil), self.fragments...)
}

func (self *messageTransportFake) requestCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.requests)
}

func (self *messageTransportFake) requestAt(index int) *protocol.MessageServerRequest {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.requests[index]
}

// Drive the binding's receive callback the way connect does: inline, with
// borrowed frames, whatever the frames are.
func (self *messageTransportFake) deliver(t *testing.T, frames ...*protocol.Frame) {
	t.Helper()
	self.mutex.Lock()
	receive := self.receive
	self.mutex.Unlock()
	if receive == nil {
		t.Fatal("the transport registered no receive callback, so no frame can reach it")
	}
	receive(connect.TransferPath{}, frames, connect.Peer{})
}

// One response, at §10.1's response code point.
func (self *messageTransportFake) answer(t *testing.T, response *protocol.MessageServerResponse) {
	t.Helper()
	self.deliver(t, &protocol.Frame{
		MessageType:  protocol.MessageType_MessageMessageServerResponse,
		MessageBytes: encodeMessageResponse(t, response),
	})
}

func encodeMessageResponse(t *testing.T, response *protocol.MessageServerResponse) []byte {
	t.Helper()
	encoded, err := proto.Marshal(response)
	if err != nil {
		t.Fatalf("could not encode the response: %v", err)
	}
	return encoded
}

func helloResponse(requestId uint64, nonce string) *protocol.MessageServerResponse {
	return &protocol.MessageServerResponse{
		RequestId: requestId,
		Reason:    protocol.Reason_REASON_OK,
		Body: &protocol.MessageServerResponse_Hello{
			Hello: &protocol.HelloResponse{ServerNonce: []byte(nonce)},
		},
	}
}

type messageTransportResult struct {
	response *protocol.MessageServerResponse
	err      error
}

// Start a Call and hand back the channel its answer will arrive on.
func callInBackground(
	transport *messageTransport,
	ctx context.Context,
	body proto.Message,
) chan messageTransportResult {
	results := make(chan messageTransportResult, 1)
	go func() {
		response, err := transport.Call(ctx, body)
		results <- messageTransportResult{response: response, err: err}
	}()
	return results
}

// Wait for the fake to have taken `count` requests, so the test knows the
// binding has registered its waiter and is in its select.
func awaitRequests(t *testing.T, fake *messageTransportFake, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fake.requestCount() < count {
		if deadline.Before(time.Now()) {
			t.Fatalf("only %d of %d requests reached the client", fake.requestCount(), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func newTestMessageTransport(t *testing.T, fake *messageTransportFake, timeout time.Duration) *messageTransport {
	t.Helper()
	transport, err := newMessageTransport(&messageTransportConfig{
		Client:          fake,
		Server:          connect.Id{0xC0, 0xFF, 0xEE},
		ProtocolVersion: 1,
		Timeout:         timeout,
	})
	if err != nil {
		t.Fatalf("the transport would not construct: %v", err)
	}
	t.Cleanup(transport.Close)
	return transport
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 2 — a response is delivered to the waiter that asked for it, or to
// nobody.
// ─────────────────────────────────────────────────────────────────────────────

func TestMessageTransportAnswersEachWaiterWithItsOwnResponse(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)
	ctx := context.Background()

	first := callInBackground(transport, ctx, &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	second := callInBackground(transport, ctx, &protocol.HelloRequest{SupportedVersions: []uint32{2}})
	awaitRequests(t, fake, 2)

	firstId := fake.requestAt(0).GetRequestId()
	secondId := fake.requestAt(1).GetRequestId()
	if firstId == secondId {
		t.Fatalf("two concurrent requests shared request_id %d, so nothing here can correlate", firstId)
	}

	// answered out of order, which is the only interesting order
	fake.answer(t, helloResponse(secondId, "second"))
	fake.answer(t, helloResponse(firstId, "first"))

	for _, each := range []struct {
		name      string
		results   chan messageTransportResult
		requestId uint64
		nonce     string
	}{
		{"first", first, firstId, "first"},
		{"second", second, secondId, "second"},
	} {
		select {
		case result := <-each.results:
			if result.err != nil {
				t.Fatalf("the %s call failed: %v", each.name, result.err)
			}
			if got := result.response.GetRequestId(); got != each.requestId {
				t.Fatalf("the %s call asked under request_id %d and was answered under %d",
					each.name, each.requestId, got)
			}
			if got := string(result.response.GetHello().GetServerNonce()); got != each.nonce {
				t.Fatalf("the %s call was handed the body %q, want %q — the correlator delivered another request's answer",
					each.name, got, each.nonce)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s call was never answered, though a response carrying request_id %d was delivered",
				each.name, each.requestId)
		}
	}

	counts := transport.Counts()
	if counts.Responses != 2 {
		t.Fatalf("Counts().Responses is %d, want 2", counts.Responses)
	}
	if counts.Unmatched != 0 {
		t.Fatalf("Counts().Unmatched is %d, want 0 — both responses had a waiter", counts.Unmatched)
	}
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d, want 0 — both waiters were answered", counts.Waiting)
	}
}

func TestMessageTransportCountsAndDropsAResponseNobodyAskedFor(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()

	fake.answer(t, helloResponse(requestId+9999, "stranger"))

	counts := transport.Counts()
	if counts.Unmatched != 1 {
		t.Fatalf("Counts().Unmatched is %d after a response nobody asked for, want 1 — "+
			"an unmatched response has to be COUNTED, so that \"nothing arrived\" and "+
			"\"something arrived for nobody\" are two readings", counts.Unmatched)
	}
	if counts.Responses != 0 {
		t.Fatalf("Counts().Responses is %d, want 0 — no waiter asked under that request_id", counts.Responses)
	}
	if counts.Waiting != 1 {
		t.Fatalf("Counts().Waiting is %d, want 1 — the outstanding waiter must still be outstanding", counts.Waiting)
	}

	select {
	case result := <-results:
		t.Fatalf("the waiter for request_id %d was handed an answer to request_id %d "+
			"(err %v, body %q): an unmatched response must go to NOBODY, never to the oldest waiter",
			requestId, result.response.GetRequestId(), result.err,
			string(result.response.GetHello().GetServerNonce()))
	case <-time.After(500 * time.Millisecond):
	}

	fake.answer(t, helloResponse(requestId, "mine"))
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("the call failed after its own response arrived: %v", result.err)
		}
		if got := string(result.response.GetHello().GetServerNonce()); got != "mine" {
			t.Fatalf("the call was handed the body %q, want \"mine\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call was never answered, though its own response was delivered")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 3 — a response arriving after its waiter timed out leaks nothing.
// ─────────────────────────────────────────────────────────────────────────────

func TestMessageTransportTimeoutIsTypedAndLeavesNoMapEntry(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 150*time.Millisecond)

	response, err := transport.Call(context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	if err == nil {
		t.Fatal("a Call that was never answered returned a nil error: " +
			"(nil, nil) is the one answer a caller cannot tell from success")
	}
	if response != nil {
		t.Fatalf("a timed-out Call returned a response as well as an error: %v", response)
	}
	if !errors.Is(err, errMessageTransportTimeout) {
		t.Fatalf("a timed-out Call returned %v, which is not errMessageTransportTimeout — "+
			"the refusal Property 3 owes is TYPED", err)
	}

	counts := transport.Counts()
	if counts.Timeouts != 1 {
		t.Fatalf("Counts().Timeouts is %d after one timeout, want 1", counts.Timeouts)
	}
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after the only Call timed out, want 0 — "+
			"the correlation map entry outlived its waiter", counts.Waiting)
	}

	// the late response: it leaks nothing, and it is counted rather than
	// silently discarded
	requestId := fake.requestAt(0).GetRequestId()
	answered := make(chan struct{})
	go func() {
		fake.answer(t, helloResponse(requestId, "late"))
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("delivering a response whose waiter had already timed out blocked the receive path")
	}

	counts = transport.Counts()
	if counts.Unmatched != 1 {
		t.Fatalf("Counts().Unmatched is %d after a late response, want 1", counts.Unmatched)
	}
	if counts.Responses != 0 {
		t.Fatalf("Counts().Responses is %d, want 0 — the waiter that asked was gone", counts.Responses)
	}
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after the late response, want 0", counts.Waiting)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 4 — the callback never blocks on a waiter.
// ─────────────────────────────────────────────────────────────────────────────

// The response is delivered from INSIDE the send, so the calling goroutine is
// still in `send` and has not reached its select. A delivery that needs the
// waiter to be reading — an unbuffered channel with no default — has nobody to
// hand the value to and stops the receive path dead. A delivery that is
// non-blocking by construction hands the value to the buffer and returns.
//
// This is the shape connect can actually produce: `ReceiveFunction` is invoked
// inline by the receive path, on whatever goroutine that path runs on, and
// nothing sequences it after the sender's select.
func TestMessageTransportReceiveCallbackDoesNotWaitForTheWaiter(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)
	fake.onSend = func(request *protocol.MessageServerRequest) {
		fake.answer(t, helloResponse(request.GetRequestId(), "inline"))
	}

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("the call failed: %v", result.err)
		}
		if got := string(result.response.GetHello().GetServerNonce()); got != "inline" {
			t.Fatalf("the call was handed the body %q, want \"inline\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the receive callback blocked on a waiter that was not yet reading: " +
			"delivery has to be non-blocking BY CONSTRUCTION, because connect invokes the " +
			"callback inline and a blocked callback backpressures every other client's frames")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The refusals construction and the send path owe.
// ─────────────────────────────────────────────────────────────────────────────

func TestMessageTransportRefusesWhatItCannotDo(t *testing.T) {
	if _, err := newMessageTransport(&messageTransportConfig{Server: connect.Id{1}}); !errors.Is(err, errMessageTransportNoClient) {
		t.Fatalf("a config with no client was accepted, or refused with %v", err)
	}
	if _, err := newMessageTransport(&messageTransportConfig{Client: &messageTransportFake{}}); !errors.Is(err, errMessageTransportNoServer) {
		t.Fatalf("a config naming no server was accepted, or refused with %v", err)
	}

	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)

	// a body that is not an arm of the request oneof
	if _, err := transport.Call(context.Background(), &protocol.HelloResponse{}); !errors.Is(err, errMessageTransportNoArm) {
		t.Fatalf("a HelloResponse was accepted as a request body, or refused with %v", err)
	}
	if waiting := transport.Counts().Waiting; waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after a refused body, want 0", waiting)
	}

	fake.refuse = true
	if _, err := transport.Call(context.Background(), &protocol.HelloRequest{}); !errors.Is(err, errMessageTransportRefused) {
		t.Fatalf("a refused send was reported as %v, want errMessageTransportRefused", err)
	}
	if waiting := transport.Counts().Waiting; waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after a refused send, want 0 — "+
			"the waiter registered before the send outlived it", waiting)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// What the frame carries and where it goes. Review findings F4 and F6: all of
// this was inside the fake's reach and none of it was asserted, so the boundary
// paragraph's list of what this file cannot see read as exhaustive when three
// locally checkable things were simply not looked at.
// ─────────────────────────────────────────────────────────────────────────────

func TestMessageTransportAddressesEveryFrameToTheConfiguredServer(t *testing.T) {
	fake := &messageTransportFake{}
	server := connect.Id{0x51, 0xE2, 0xA7, 0x03}
	transport, err := newMessageTransport(&messageTransportConfig{
		Client:          fake,
		Server:          server,
		ProtocolVersion: 7,
		Timeout:         150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("the transport would not construct: %v", err)
	}
	t.Cleanup(transport.Close)

	// it will time out, and the timeout is not what this test is about
	transport.Call(context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{7}})

	addressed := fake.addressed()
	if len(addressed) == 0 {
		t.Fatal("no frame reached the client at all, so nothing here is an assertion about its address")
	}
	want := connect.DestinationId(server)
	for index, destination := range addressed {
		if destination != want {
			t.Fatalf("frame %d was addressed to %v, want %v -- errMessageTransportNoServer says "+
				"that every frame is addressed to the server's client_id, and the server is a value "+
				"the caller configured rather than a value the send path invents",
				index, destination, want)
		}
	}

	// the SEND-side code point, asserted rather than inferred from a fake that
	// happens to refuse everything else
	for index, codePoint := range fake.codePoints() {
		if codePoint != protocol.MessageType_MessageMessageServerRequest {
			t.Fatalf("frame %d went out at code point %d (%s), want %d (%s)",
				index, codePoint, codePoint,
				protocol.MessageType_MessageMessageServerRequest,
				protocol.MessageType_MessageMessageServerRequest)
		}
	}

	// the configured version is stamped on the request rather than dropped on
	// the way
	if got := fake.requestAt(0).GetProtocolVersion(); got != 7 {
		t.Fatalf("the request carries protocol_version %d, want the configured 7 -- "+
			"messageTransportConfig.ProtocolVersion says it is stamped on every later request", got)
	}

	// and the frame counter moved, which is a counter Property 2 owes and which
	// no test read before this one
	if frames := transport.Counts().RequestFrames; frames != 1 {
		t.Fatalf("Counts().RequestFrames is %d after one request, want 1", frames)
	}
}

// The receive side reads this binding's own code points and nothing else. The
// class of "everything else" is READ off the compiled enum rather than listed:
// every MessageType protocol declares that is not one this binding reads.
func TestMessageTransportReadsOnlyTheCodePointsThatAreItsOwn(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()
	encoded := encodeMessageResponse(t, helloResponse(requestId, "mine"))

	mine := messageTransportReadCodePoints(t)
	others := []protocol.MessageType{}
	for number := range protocol.MessageType_name {
		codePoint := protocol.MessageType(number)
		if !mine[codePoint] {
			others = append(others, codePoint)
		}
	}
	sort.Slice(others, func(i int, j int) bool { return others[i] < others[j] })
	t.Logf("code points this binding READS: %d %v; complement -- code points protocol declares that it does not: %d %v",
		len(mine), sortedCodePoints(mine), len(others), others)

	// ── the half the class CANNOT supply, because the class is read off the
	//    thing it constrains ───────────────────────────────────────────────────
	//
	// `mine` is derived from the receive path's own switch. That is what makes a
	// `case` added tomorrow join the class — and it is also why the class cannot
	// object to one: a code point added to a case moves OUT of the complement and
	// is then never driven. Found by planting exactly that, this commit's own
	// mutation F6c — `protocol.MessageType_MessageMessageServerPush` added beside
	// the response case — and watching the whole transport set stay green.
	//
	// So the read set is measured against something that is not the switch: every
	// code point this binding reads must name a message that declares
	// `request_id`. Correlation by `request_id` is the ONLY thing this binding
	// does with what it reads — a frame it cannot correlate has no waiter to
	// answer and nothing else to be — and §4.3's push, the one message-server
	// code point that is not an answer, declares a `body` oneof and no
	// `request_id` at all. Read off the compiled descriptor rather than from the
	// name, so a field renamed upstream fails here rather than quietly stopping
	// being the rule.
	parent := (&protocol.MessageServerResponse{}).ProtoReflect().Descriptor().FullName().Parent()
	for _, codePoint := range sortedCodePoints(mine) {
		// §4.3.5's PUSH IS THE ONE EXCEPTION, AND IT IS NAMED RATHER THAN LET THROUGH. It is the one
		// message-server code point that is not an answer: it declares no request_id, and this binding
		// reads it only to hand it to OnPush, never to a waiter. That is asserted below by delivering
		// one, so the exception is a measured route and not a hole in the rule. Ledger 269.
		if codePoint == protocol.MessageType_MessageMessageServerPush {
			t.Logf("  %s is read, declares no request_id, and is routed to OnPush (asserted below)",
				protocol.MessageType_name[int32(codePoint)])
			continue
		}
		spelled, named := protocol.MessageType_name[int32(codePoint)], protoreflect.FullName("")
		if spelled == "" {
			t.Fatalf("the receive path reads code point %d, which protocol's enum does not name", codePoint)
		}
		named = parent.Append(protoreflect.Name(strings.TrimPrefix(spelled, "Message")))
		messageType, err := protoregistry.GlobalTypes.FindMessageByName(named)
		if err != nil {
			t.Fatalf("the receive path reads %s and this build links no message %s for it (%v): a code "+
				"point this binding reads is a code point it DECODES, and one it cannot name is one it "+
				"cannot decode", spelled, named, err)
		}
		if messageType.Descriptor().Fields().ByName("request_id") == nil {
			t.Fatalf("the receive path reads %s, whose message %s declares no request_id. Correlation by "+
				"request_id is the only thing this binding does with what it reads, so a frame it cannot "+
				"correlate has no waiter to answer and nothing else to be. §4.3's push is the one "+
				"message-server code point that is not an answer, and this is the rule that says so "+
				"without naming it", spelled, named)
		}
		t.Logf("  %s names %s, which declares request_id, so this binding can correlate it", spelled, named)
	}
	if len(others) == 0 {
		t.Fatal("the complement is EMPTY: this binding would be reading every code point protocol has, " +
			"which is not a filter at all")
	}
	if len(mine)+len(others) != len(protocol.MessageType_name) {
		t.Fatalf("%d read + %d not read is not the %d code points protocol declares: the partition does not close",
			len(mine), len(others), len(protocol.MessageType_name))
	}

	// the very same well-formed response bytes, at every code point that is not
	// one this binding reads
	for _, codePoint := range others {
		fake.deliver(t, &protocol.Frame{MessageType: codePoint, MessageBytes: encoded})
	}
	counts := transport.Counts()
	if counts.ResponseFrames != 0 {
		t.Fatalf("Counts().ResponseFrames is %d after %d frames at code points this binding does not read, want 0",
			counts.ResponseFrames, len(others))
	}
	if counts.Responses != 0 || counts.Unmatched != 0 {
		t.Fatalf("a frame at another binding's code point was DECODED: Responses %d, Unmatched %d, want 0 and 0 -- "+
			"connect carries every binding's traffic on one callback, so the code point is the only thing "+
			"that says a frame is this binding's", counts.Responses, counts.Unmatched)
	}
	if counts.Waiting != 1 {
		t.Fatalf("Counts().Waiting is %d, want 1 -- the waiter was answered by a frame at another code point", counts.Waiting)
	}

	// the push, at its own code point: handed to OnPush, and nothing else moves
	if !mine[protocol.MessageType_MessageMessageServerPush] {
		t.Fatal("the receive path does not read §4.3.5's push, so nothing ever reaches OnPush")
	}
	pushed := make(chan *protocol.MessageServerPush, 1)
	unsubscribe := transport.OnPush(func(push *protocol.MessageServerPush) { pushed <- push })
	defer unsubscribe()
	pushBytes, err := proto.Marshal(&protocol.MessageServerPush{Body: &protocol.MessageServerPush_Records{
		Records: &protocol.RecordPush{GroupId: []byte("group"), HighWaterRecordId: 7}}})
	if err != nil {
		t.Fatalf("marshalling a push: %v", err)
	}
	fake.deliver(t, &protocol.Frame{MessageType: protocol.MessageType_MessageMessageServerPush, MessageBytes: pushBytes})
	select {
	case push := <-pushed:
		if push.GetRecords().GetHighWaterRecordId() != 7 {
			t.Fatalf("OnPush was handed %v, want the push that was delivered", push)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a push at its own code point did not reach OnPush")
	}
	counts = transport.Counts()
	if counts.PushFrames != 1 || counts.ResponseFrames != 0 || counts.Responses != 0 || counts.Unmatched != 0 || counts.Waiting != 1 {
		t.Fatalf("after one push: PushFrames %d ResponseFrames %d Responses %d Unmatched %d Waiting %d, want 1 0 0 0 1 -- "+
			"a push is never correlated and never answers a waiter",
			counts.PushFrames, counts.ResponseFrames, counts.Responses, counts.Unmatched, counts.Waiting)
	}

	// and the code point that IS this binding's arrives
	fake.deliver(t, &protocol.Frame{
		MessageType:  protocol.MessageType_MessageMessageServerResponse,
		MessageBytes: encoded,
	})
	counts = transport.Counts()
	if counts.ResponseFrames != 1 {
		t.Fatalf("Counts().ResponseFrames is %d after one response frame, want 1", counts.ResponseFrames)
	}
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("the call failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the response at the binding's own code point was not delivered")
	}
}

// The code points this binding READS, derived from the receive path's own
// `switch` rather than written down here.
//
// The derivation is deliberately over the SOURCE and not over a production
// helper the test could be handed: a helper would be a second place to say
// which code points are read, and the two would be free to disagree. A `case`
// added to the switch joins this class on the commit that adds it; the whole
// switch deleted makes this class EMPTY, which the caller fails closed on.
//
// The spelling is turned into a NUMBER through protocol's own compiled enum
// value map, so a constant this gate cannot resolve is a failure rather than a
// silent zero.
func messageTransportReadCodePoints(t *testing.T) map[protocol.MessageType]bool {
	t.Helper()
	gate := newBorrowGate(t)
	decl := gate.decls["messageTransport.receive"]
	if decl == nil {
		t.Fatal("package sdk declares no messageTransport.receive, so there is no receive path to read the class off")
	}
	read := map[protocol.MessageType]bool{}
	ast.Inspect(decl, func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, each := range clause.List {
			selector, ok := each.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "protocol" || !strings.HasPrefix(selector.Sel.Name, "MessageType_") {
				continue
			}
			spelled := strings.TrimPrefix(selector.Sel.Name, "MessageType_")
			number, found := protocol.MessageType_value[spelled]
			if !found {
				t.Fatalf("messageTransport.receive names protocol.%s, which is not a value of protocol's MessageType enum",
					selector.Sel.Name)
			}
			read[protocol.MessageType(number)] = true
		}
		return true
	})
	if len(read) == 0 {
		t.Fatal("messageTransport.receive selects on NO code point: the receive path reads every frame " +
			"connect hands it, including every other binding's")
	}
	return read
}

func sortedCodePoints(set map[protocol.MessageType]bool) []protocol.MessageType {
	points := []protocol.MessageType{}
	for codePoint := range set {
		points = append(points, codePoint)
	}
	sort.Slice(points, func(i int, j int) bool { return points[i] < points[j] })
	return points
}

// A cancelled Call is the OTHER way a waiter goes away, and it is the one the
// caller controls. Review finding F5: no test in the suite constructed a
// cancellable context, and deleting the forget from the ctx arm left a
// permanent correlation-map entry with everything green.
func TestMessageTransportCancelledCallIsTypedAndLeavesNoMapEntry(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	results := callInBackground(transport, ctx, &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	if waiting := transport.Counts().Waiting; waiting != 1 {
		t.Fatalf("Counts().Waiting is %d before the cancel, want 1", waiting)
	}

	cancel()
	select {
	case result := <-results:
		if result.err == nil {
			t.Fatal("a cancelled Call returned a nil error: (nil, nil) is the one answer a caller cannot tell from success")
		}
		if result.response != nil {
			t.Fatalf("a cancelled Call returned a response as well as an error: %v", result.response)
		}
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("a cancelled Call returned %v, which does not wrap context.Canceled -- "+
				"the caller cannot tell its own cancel from a server that never answered", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled Call never returned")
	}

	counts := transport.Counts()
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after the only Call was cancelled, want 0 -- "+
			"the correlation map entry outlived its waiter", counts.Waiting)
	}
	if counts.Timeouts != 0 {
		t.Fatalf("Counts().Timeouts is %d after a CANCEL, want 0 -- a cancel is not a timeout", counts.Timeouts)
	}

	// the answer that arrives afterwards goes to nobody, and is counted
	fake.answer(t, helloResponse(fake.requestAt(0).GetRequestId(), "late"))
	counts = transport.Counts()
	if counts.Unmatched != 1 {
		t.Fatalf("Counts().Unmatched is %d after a response whose waiter was cancelled, want 1", counts.Unmatched)
	}
	if counts.Responses != 0 {
		t.Fatalf("Counts().Responses is %d, want 0 -- the waiter that asked had been cancelled", counts.Responses)
	}
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after the late response, want 0", counts.Waiting)
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// Property 1 — nothing borrowed outlives the receive callback.
// ═════════════════════════════════════════════════════════════════════════════
//
// GATE CLASS, derived and stated separately from the scope (R3):
//
//	every value that reaches this binding through the receive callback's
//	PARAMETERS. The class is read off `connect.ReceiveFunction`'s own
//	declaration at run time — go/parser over connect's source, located through
//	`go list` — and is never listed here. The gate REPORTS the number it read
//	and the source position it read it at, and asserts the binding's callback
//	binds exactly that many parameters. It is three today; the gate does not
//	know that in advance and neither does this comment.
//
//	WHAT THE ARITY CHECK ACTUALLY BUYS, corrected (review finding F7). An
//	earlier draft of this header said a parameter added to `ReceiveFunction`
//	upstream "fails here". It cannot: `messageTransportClient` declares
//	`AddReceiveCallback(connect.ReceiveFunction)` — the alias itself — and
//	`self.receive` is passed to it, so an arity change upstream is a COMPILE
//	ERROR and this test binary never runs. The compiler's answer is the
//	stronger one and it is the one that arrives. What the read buys is the
//	other half: the number and the POSITION are reported, so a reader knows
//	which declaration the class was taken from, and a gate that silently read
//	the wrong file is visible rather than assumed. The taint roots are the
//	sdk callback's OWN parameter names — the connect read is a cross-check and
//	a report, not a driver.
//
// GATE SCOPE, derived separately from the class (R3):
//
//	the callback's whole DYNAMIC EXTENT — the registered callback function plus
//	every function and method in `package sdk` that it transitively calls,
//	computed from the parsed call graph. It is NOT the callback's lexical body
//	and it is NOT one file: a callee declared in another file of the package is
//	in scope. "A gate that checks the body and not what the body calls has read
//	half of it."
//
// WHAT IT REFUSES, which is a construction and not a timing:
//
//	a borrowed value stored anywhere but a local of the extent (a field, a map,
//	an index, a dereference), sent on a channel, referenced inside a `go`
//	statement, or passed to a function outside the package that this gate cannot
//	see inside. A `go` statement is refused whatever it does with the value —
//	promptness is not the rule, and `-race` is unavailable here anyway, so a
//	behaviour test would be measuring a race it happened to win.
//
// THE COMPLEMENTS IT PRINTS. Every narrowing below names, at run time, what it
// removed:
//
//	C1 the package-level declarations in the binding's own production files that
//	   are NOT in the extent. Asserted to partition those files' declarations
//	   with the extent, and asserted NON-EMPTY: an extent that swallowed the
//	   whole file is a call-graph walk that lost its bearings, so the gate fails
//	   closed rather than continuing past it.
//	C2 the receive-callback registrations in `package sdk` that are NOT this
//	   binding's. Printed with its count and members. NOT failed on empty, and
//	   the reason is stated rather than assumed: `sdk` is entitled to contain
//	   exactly one registration, and today it contains two.
//	C3 the borrowed expressions the gate examined and CLEARED, each with the
//	   reason it was cleared. Asserted non-empty — an empty clearance set means
//	   the gate located the callback and then looked at nothing — and asserted
//	   to COVER every source line on which a borrowed identifier appears, which
//	   is the assertion that catches twelve readings where there are thirteen.
func TestNothingBorrowedOutlivesTheReceiveCallback(t *testing.T) {
	gate := newBorrowGate(t)

	// ── the class, read off connect rather than listed here ──────────────────
	classPos, class := gate.receiveFunctionClass()
	t.Logf("Property 1 class: connect.ReceiveFunction at %s declares %d parameters: %s",
		classPos, len(class), strings.Join(class, ", "))

	// ── C2: which registration is this binding's, and which are not ──────────
	mine, others := gate.receiveRegistrations()
	t.Logf("C2 complement — receive-callback registrations in package sdk that are NOT this binding's: %d %v",
		len(others), others)
	if len(mine) != 1 {
		t.Fatalf("package sdk has %d receive-callback registrations on messageTransport %v, want exactly 1",
			len(mine), mine)
	}
	root := mine[0]

	rootDecl := gate.decls[root.callee]
	if rootDecl == nil {
		t.Fatalf("the callback registered at %s resolves to %q, which is not a declaration in package sdk",
			root.pos, root.callee)
	}
	bound := fieldNames(rootDecl.Type.Params)
	t.Logf("Property 1 class as this binding binds it: %s binds %d parameters: %s",
		root.callee, len(bound), strings.Join(bound, ", "))
	if len(bound) != len(class) {
		t.Fatalf("connect.ReceiveFunction declares %d parameters (%s) and %s binds %d (%s): "+
			"the class this gate tracks is read off the signature, so a parameter added upstream "+
			"has to fail here",
			len(class), strings.Join(class, ", "), root.callee, len(bound), strings.Join(bound, ", "))
	}

	// ── the scope: the dynamic extent ────────────────────────────────────────
	gate.walkExtent(root.callee)
	t.Logf("Property 1 scope: the dynamic extent of %s is %d functions: %s",
		root.callee, len(gate.order), strings.Join(gate.order, ", "))
	if len(gate.duplicates) != 0 {
		t.Logf("note: %d declaration names appear more than once across the package's production files "+
			"(build-tagged variants): %v", len(gate.duplicates), gate.duplicates)
	}

	// ── C1: what the scope removed ───────────────────────────────────────────
	bindingFiles := gate.bindingFiles()
	inside, outside := gate.partition(bindingFiles)
	t.Logf("C1 complement — declarations in the binding's production files %v that are NOT in the extent: %d %v",
		bindingFiles, len(outside), outside)
	declared := gate.declarationsIn(bindingFiles)
	if len(inside)+len(outside) != len(declared) {
		t.Fatalf("the extent and its complement are %d + %d over %d declarations in %v: the partition does not close",
			len(inside), len(outside), len(declared), bindingFiles)
	}
	if len(outside) == 0 {
		t.Fatalf("the C1 complement is EMPTY: the extent claims all %d declarations in %v, "+
			"which means the call-graph walk lost its bearings rather than that the binding is all callback",
			len(declared), bindingFiles)
	}

	// ── the taint walk ───────────────────────────────────────────────────────
	gate.analyze(root.callee, setOf(bound...))

	// ── C3: what was examined and cleared ────────────────────────────────────
	t.Logf("C3 complement — borrowed expressions examined and CLEARED: %d", len(gate.cleared))
	for _, site := range gate.cleared {
		t.Logf("    cleared  %s  %s  — %s", site.pos, site.what, site.reason)
	}
	if len(gate.cleared) == 0 {
		t.Fatal("the C3 complement is EMPTY: the gate located the callback, walked its extent, " +
			"and classified not one borrowed expression. A gate that removes nothing has not looked.")
	}

	uncovered := gate.uncoveredBorrowLines()
	t.Logf("borrowed-identifier occurrences in the extent: %d, every one on a line a clearance or a flag covers: %v",
		gate.borrowLineCount(), len(uncovered) == 0)
	if len(uncovered) != 0 {
		t.Fatalf("%d borrowed identifier(s) sit on a source line this gate classified neither way: %v — "+
			"the classifier is silent on a construction it walked past", len(uncovered), uncovered)
	}

	// ── the verdict ──────────────────────────────────────────────────────────
	if len(gate.flagged) != 0 {
		for _, site := range gate.flagged {
			t.Errorf("a borrowed value outlives the receive callback: %s  %s  — %s",
				site.pos, site.what, site.reason)
		}
		t.Fatalf("%d borrowed value(s) escape the callback. connect: \"the frames, frame objects, "+
			"and their message bytes are borrowed and valid only until the callback returns ... never "+
			"hand a borrowed Frame to an asynchronous send, goroutine, or channel.\"", len(gate.flagged))
	}
}

// Property 3's other half, and it is a code check for the same reason Property 1
// is: "no goroutine leaks" is not observable in a suite that cannot run -race
// and whose other tests run concurrently with this one, so `runtime.NumGoroutine`
// would be measuring the package and not this binding.
//
// GATE CLASS, derived: every `go` statement in the files this gate is scoped to.
//
// GATE SCOPE, derived SEPARATELY from the class and from THREE derivations that
// are unioned rather than picked between:
//
//	(a) the production files of package sdk that declare a method on
//	    `messageTransport`,
//	(b) the production files that declare any function in the receive callback's
//	    DYNAMIC EXTENT, and
//	(c) the production files that declare any function in the dynamic extent of
//	    the binding's EXPORTED methods — its entry points, derived as the
//	    exported methods on `messageTransport` rather than listed.
//
// Review finding F3 was the gap between (a) and (b): the borrow gate NAMED a
// free function as inside the receive extent on the line above this gate
// reporting a scope that excluded it. Review finding D is the gap (a) ∪ (b) left
// open on the other side — a production file declaring no `messageTransport`
// method, reached only from the SEND path, was in neither, and a `go` statement
// in a function `Call` invokes on every request was printed by nothing and
// passed filtered AND unfiltered. Property 3's refusal is "No goroutine, no map
// entry, no channel", not "no goroutine in the receive extent", so the scope is
// the union of every way into this binding: what connect calls, and what a
// caller calls.
//
// Complements printed: what each derivation contributed that the others did not,
// and, per file, the statements scanned that are NOT `go` statements. A file in
// scope that contributes zero statements means the gate parsed a file and read
// nothing out of it, and it fails closed on that PER FILE rather than on the
// total -- a total that is merely non-zero is satisfied by one big file while
// every other file in scope is silently empty, which is the shape the house rule
// calls the silent one.
func TestTheMessageTransportStartsNoGoroutine(t *testing.T) {
	gate := newBorrowGate(t)

	binding := gate.bindingFiles()
	if len(binding) == 0 {
		t.Fatal("no production file of package sdk declares a method on messageTransport")
	}
	mine, _ := gate.receiveRegistrations()
	if len(mine) != 1 {
		t.Fatalf("package sdk has %d receive-callback registrations on messageTransport %v, want exactly 1",
			len(mine), mine)
	}
	gate.walkExtent(mine[0].callee)
	extent := gate.extentFiles()
	if len(extent) == 0 {
		t.Fatal("the receive callback's dynamic extent is declared in no file, so the second derivation read nothing")
	}

	// (c) on a gate of its own, so that walking the entry points cannot move
	// what (b) reported. The entry points are DERIVED -- every exported method
	// on messageTransport -- so a method added to the binding joins them on the
	// commit that adds it, and a send path that calls out to a new file is in
	// scope on the same commit.
	callers := newBorrowGate(t)
	entries := callers.exportedBindingMethods()
	if len(entries) == 0 {
		t.Fatal("messageTransport declares no exported method, so the third derivation has no root to " +
			"walk and this binding has no entry point a caller could use")
	}
	for _, entry := range entries {
		callers.walkExtent(entry)
	}
	reached := callers.extentFiles()
	if len(reached) == 0 {
		t.Fatal("the exported methods' dynamic extent is declared in no file, so the third derivation read nothing")
	}

	scope := unionOfFiles(unionOfFiles(binding, extent), reached)
	t.Logf("GATE SCOPE, the union of three derivations: %d files %v", len(scope), scope)
	t.Logf("  (a) files declaring a messageTransport method: %d %v", len(binding), binding)
	t.Logf("  (b) files declaring a function in the receive callback's extent (%d functions): %d %v",
		len(gate.order), len(extent), extent)
	t.Logf("  (c) files declaring a function in the extent of the %d exported method(s) %v (%d functions): %d %v",
		len(entries), entries, len(callers.order), len(reached), reached)
	t.Logf("  complement -- in (b) and NOT in (a): %d %v", len(filesNotIn(extent, binding)), filesNotIn(extent, binding))
	t.Logf("  complement -- in (a) and NOT in (b): %d %v", len(filesNotIn(binding, extent)), filesNotIn(binding, extent))
	t.Logf("  complement -- in (c) and NOT in (a) ∪ (b): %d %v",
		len(filesNotIn(reached, unionOfFiles(binding, extent))), filesNotIn(reached, unionOfFiles(binding, extent)))
	t.Logf("  complement -- production files of package sdk this scope does NOT hold: %d %v",
		len(filesNotIn(gate.fileNames, scope)), filesNotIn(gate.fileNames, scope))
	if len(scope) != len(unionOfFiles(scope, binding)) ||
		len(scope) != len(unionOfFiles(scope, extent)) ||
		len(scope) != len(unionOfFiles(scope, reached)) {
		t.Fatalf("the scope %v does not contain all three derivations %v, %v and %v",
			scope, binding, extent, reached)
	}
	if len(filesNotIn(gate.fileNames, scope)) == 0 {
		t.Fatalf("the scope holds every one of package sdk's %d production files: a scope that narrows "+
			"nothing has not been derived, it has been assumed", len(gate.fileNames))
	}

	statements := 0
	found := []string{}
	for _, name := range scope {
		here, perFile := gate.goStatements(name)
		statements += perFile
		found = append(found, here...)
		t.Logf("  %s: %d statements, %d of them `go`; complement -- statements that are not `go`: %d",
			name, perFile, len(here), perFile-len(here))
		if perFile-len(here) == 0 {
			t.Fatalf("%s contributes an EMPTY complement: %d statements scanned in a file this gate "+
				"holds in scope, so the gate parsed it and read nothing", name, perFile)
		}
	}
	// There is deliberately NO total-statement check here beside the per-file
	// one. `statements == 0` cannot happen once every file in scope has been
	// asserted to contribute a non-empty complement and the scope has been
	// asserted to contain a non-empty (a) — it is the second check in this test
	// that could not fail, and removing the first one (review finding G) while
	// leaving this one in would have been the repair shipping the defect it was
	// repairing. The per-file check is strictly stronger anyway: a total that is
	// merely non-zero is satisfied by one big file while the rest are silent.
	t.Logf("class: `go` statements in %v; %d found; complement -- statements scanned that are not `go`: %d of %d",
		scope, len(found), statements-len(found), statements)

	if len(found) != 0 {
		t.Fatalf("the binding starts %d goroutine(s), at %v: a request that timed out has to leave "+
			"no goroutine behind, and a goroutine here is also the construction Property 1 refuses",
			len(found), found)
	}
}

// REVIEW FINDING G — what the tautology was standing in for.
//
// The assertion this replaces read
//
//	if statements != len(found)+(statements-len(found)) || statements == 0 {
//
// and `x != y + (x - y)` is identically false, so the only live half of it was
// the zero check the per-file complement above already makes. A check that
// cannot fail, in a file whose subject is checks that cannot fail.
//
// The question it was standing in for is the one worth asking: does the class
// derivation FIND a `go` statement when there is one? That is asked here
// directly, over source that is NOT package sdk, through the same
// `borrowGate.goStatements` the gate above runs -- so a derivation that stopped
// seeing `go` statements fails here even if package sdk never grows one. Both
// directions, because a scan that finds a `go` statement everywhere is as broken
// as one that finds it nowhere: the negative file holds the two constructions
// nearest to a `go` -- a `defer` and a function literal called on the spot.
func TestTheGoroutineScanFindsAGoStatementWhenThereIsOne(t *testing.T) {
	gate := newBorrowGateOver(t, map[string]any{
		"probe_with.go": `package probe

type held struct{}

func (self *held) start() {
	go func() { _ = 1 }()
}

func loose(f func()) {
	go f()
}
`,
		"probe_without.go": `package probe

func nothingStarted(f func()) {
	defer f()
	func() { _ = 1 }()
	for index := 0; index < 3; index += 1 {
		_ = index
	}
}
`,
	})

	with, withStatements := gate.goStatements("probe_with.go")
	without, withoutStatements := gate.goStatements("probe_without.go")
	t.Logf("the scan over a file holding two `go` statements in two shapes: %d found in %d statements %v",
		len(with), withStatements, with)
	t.Logf("the scan over a file holding a `defer` and a called function literal: %d found in %d statements",
		len(without), withoutStatements)

	if len(with) != 2 {
		t.Fatalf("the scan found %d `go` statement(s) in a file that holds two -- one whose callee is a "+
			"function literal and one whose callee is a parameter: %v. The gate above reports `0 found` "+
			"by running this same scan, and a scan that finds nothing reports that either way",
			len(with), with)
	}
	if withStatements <= len(with) {
		t.Fatalf("the scan counted %d statement(s) in a file with %d `go` statements and a body around "+
			"them: the complement it prints is the statements that are NOT `go`, and it is empty here",
			withStatements, len(with))
	}
	if len(without) != 0 {
		t.Fatalf("the scan found %d `go` statement(s) in a file that holds none: %v. A `defer` and a "+
			"function literal called on the spot are the two constructions nearest to a `go`, and "+
			"reading either as one would make the gate above fail on a binding that is correct",
			len(without), without)
	}
	if withoutStatements == 0 {
		t.Fatal("the scan counted 0 statements in a file that holds several, so it read nothing and " +
			"`0 go statements` was an answer about nothing")
	}
}

// The `go` statements of one file in scope, and the statements scanned to find
// them.
//
// A method rather than a loop body, so that the control above runs the SAME
// derivation the gate runs rather than a copy of it that can drift from it.
func (self *borrowGate) goStatements(name string) (found []string, statements int) {
	file := self.prodFiles[name]
	if file == nil {
		self.t.Fatalf("this gate holds %s in scope and parsed no such file, so the class would be read "+
			"out of nothing for it", name)
		return nil, 0
	}
	ast.Inspect(file, func(node ast.Node) bool {
		statement, ok := node.(ast.Stmt)
		if !ok {
			return true
		}
		statements += 1
		if _, isGo := statement.(*ast.GoStmt); isGo {
			found = append(found, self.fset.Position(statement.Pos()).String())
		}
		return true
	})
	return found, statements
}

// The binding's entry points: every EXPORTED method on messageTransport.
//
// Derived rather than listed, so that a method added to the binding is a root of
// the third scope derivation on the commit that adds it. Unexported methods are
// deliberately not roots -- they are reachable only from an exported one or from
// the receive callback, and both are already walked.
func (self *borrowGate) exportedBindingMethods() []string {
	keys := []string{}
	for _, key := range self.declOrder {
		decl := self.decls[key]
		if recvTypeName(decl) != "messageTransport" || !decl.Name.IsExported() {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The production files that declare any function of the walked extent.
func (self *borrowGate) extentFiles() []string {
	seen := map[string]bool{}
	names := []string{}
	for _, key := range self.order {
		name := self.declFile[key]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func unionOfFiles(left []string, right []string) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, name := range append(append([]string{}, left...), right...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func filesNotIn(left []string, right []string) []string {
	within := map[string]bool{}
	for _, name := range right {
		within[name] = true
	}
	names := []string{}
	for _, name := range left {
		if !within[name] {
			names = append(names, name)
		}
	}
	return names
}

// ─────────────────────────────────────────────────────────────────────────────
// the gate's machinery
// ─────────────────────────────────────────────────────────────────────────────

type borrowSite struct {
	pos    string
	what   string
	reason string
}

type registration struct {
	pos    string
	callee string
	owner  string
}

type borrowGate struct {
	t          *testing.T
	fset       *token.FileSet
	prodFiles  map[string]*ast.File
	fileNames  []string
	decls      map[string]*ast.FuncDecl
	declFile   map[string]string
	declOrder  []string
	duplicates []string

	// Every package-level value NAME of package sdk, with where it was
	// declared. Review finding F2: the AssignStmt arm cleared any assignment
	// whose left-hand side is a bare identifier, with the reason "bound to the
	// local %s, which dies with the callback" -- and nothing syntactic
	// distinguishes a local from a package-level variable, so a borrowed frame
	// stored into a package-level var was cleared as a local. A name declared
	// here is not a local, whatever it looks like at the assignment.
	packageValues map[string]string

	// What each function literal in the walked declarations IS, so that the
	// classifier can tell a closure that runs inside the callback from one that
	// is kept for later. Review finding F1: isBorrowed had no FuncLit case, the
	// store of a closure over a borrowed frame read as a store of something
	// unborrowed, and the ReturnStmt inside the literal then CLEARED the
	// borrowed expression with a reason about a caller that is inside the
	// callback -- when the caller is whoever invokes the stored closure, after
	// it returned.
	litRole map[*ast.FuncLit]string

	extent   map[string]bool
	order    []string
	analyzed map[string]bool

	cleared     []borrowSite
	flagged     []borrowSite
	borrowLines map[string]bool
	coveredLine map[string]bool
}

// The functions this gate treats as copying their borrowed argument, and the
// only way a borrowed value is cleared out of the class once it is in it. Each
// one either copies the bytes or reads a scalar out of them; connect's own rule
// names `MessagePoolShareReadOnly` as the third way, so it is here too.
var borrowSanitizers = map[string]string{
	"len":                              "len reads a length, not the bytes",
	"cap":                              "cap reads a capacity, not the bytes",
	"copy":                             "copy writes the borrowed bytes into a buffer of our own",
	"string":                           "a string conversion copies",
	"proto.Unmarshal":                  "Unmarshal decodes into a message of our own, which copies every byte it keeps",
	"connect.MessagePoolShareReadOnly": "connect's own rule names this as a way to outlive the callback",
}

func newBorrowGate(t *testing.T) *borrowGate {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("could not list the package's files: %v", err)
	}
	sources := map[string]any{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		// nil means "read it off disk"
		sources[path] = nil
	}
	return newBorrowGateOver(t, sources)
}

// The same gate over source handed to it rather than read off the package.
//
// It exists so that the classifier can be driven against constructions that are
// NOT in this package -- see TestTheBorrowClassifierRefusesWhatItClaimsTo. A
// gate whose verdicts are never themselves tested is a gate whose count can be
// right while every verdict in it is wrong, which is exactly how the two false
// clears of the Task 5 review survived a complement that asserted coverage.
func newBorrowGateOver(t *testing.T, sources map[string]any) *borrowGate {
	t.Helper()
	gate := &borrowGate{
		t:             t,
		fset:          token.NewFileSet(),
		prodFiles:     map[string]*ast.File{},
		decls:         map[string]*ast.FuncDecl{},
		declFile:      map[string]string{},
		packageValues: map[string]string{},
		litRole:       map[*ast.FuncLit]string{},
		extent:        map[string]bool{},
		analyzed:      map[string]bool{},
		borrowLines:   map[string]bool{},
		coveredLine:   map[string]bool{},
	}
	names := []string{}
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, path := range names {
		file, err := parser.ParseFile(gate.fset, path, sources[path], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("could not parse %s: %v", path, err)
		}
		gate.prodFiles[path] = file
		gate.fileNames = append(gate.fileNames, path)
		for _, decl := range file.Decls {
			if genDecl, isGen := decl.(*ast.GenDecl); isGen {
				if genDecl.Tok != token.VAR && genDecl.Tok != token.CONST {
					continue
				}
				for _, spec := range genDecl.Specs {
					valueSpec, isValue := spec.(*ast.ValueSpec)
					if !isValue {
						continue
					}
					for _, name := range valueSpec.Names {
						if name.Name == "_" {
							continue
						}
						gate.packageValues[name.Name] = gate.fset.Position(name.Pos()).String()
					}
				}
				continue
			}
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := declKey(funcDecl)
			if _, already := gate.decls[key]; already {
				gate.duplicates = append(gate.duplicates, key)
				continue
			}
			gate.decls[key] = funcDecl
			gate.declFile[key] = path
			gate.declOrder = append(gate.declOrder, key)
			gate.readLiteralRoles(funcDecl)
		}
	}
	if len(gate.prodFiles) == 0 {
		t.Fatal("no production file of package sdk was parsed, so this gate has read nothing")
	}
	sort.Strings(gate.fileNames)
	return gate
}

// What every function literal in a declaration is FOR, read off the node that
// holds it. A literal that is the callee of a `go`, of a `defer`, or of a call
// made on the spot has a lifetime the callback controls; a literal that is
// stored, returned, or handed anywhere else does not, and there is no third
// thing to read here -- the role is the syntax, not a guess.
func (self *borrowGate) readLiteralRoles(decl *ast.FuncDecl) {
	ast.Inspect(decl, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.GoStmt:
			if lit, ok := statement.Call.Fun.(*ast.FuncLit); ok {
				self.litRole[lit] = "go"
			}
		case *ast.DeferStmt:
			if lit, ok := statement.Call.Fun.(*ast.FuncLit); ok {
				self.litRole[lit] = "defer"
			}
		case *ast.CallExpr:
			if lit, ok := statement.Fun.(*ast.FuncLit); ok {
				if _, already := self.litRole[lit]; !already {
					self.litRole[lit] = "called"
				}
			}
		}
		return true
	})
}

// The class, read off connect's own declaration.
func (self *borrowGate) receiveFunctionClass() (string, []string) {
	self.t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/urnetwork/connect").Output()
	if err != nil {
		self.t.Fatalf("could not locate connect's source, so the class cannot be read off its signature: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		self.t.Fatal("go list named no directory for github.com/urnetwork/connect")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		self.t.Fatalf("could not read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "ReceiveFunction" {
					continue
				}
				funcType, ok := typeSpec.Type.(*ast.FuncType)
				if !ok {
					self.t.Fatalf("connect.ReceiveFunction at %s is not a func type",
						fset.Position(typeSpec.Pos()))
				}
				return fset.Position(typeSpec.Pos()).String(), fieldNames(funcType.Params)
			}
		}
	}
	self.t.Fatalf("connect.ReceiveFunction was not found in %s, so the class has no source to be read from", dir)
	return "", nil
}

// Every receive-callback registration in the package, split into this binding's
// and the complement.
func (self *borrowGate) receiveRegistrations() (mine []registration, others []string) {
	for _, key := range self.declOrder {
		decl := self.decls[key]
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "AddReceiveCallback" || len(call.Args) != 1 {
				return true
			}
			pos := self.fset.Position(call.Pos()).String()
			callee, receiverType := self.resolveCallback(decl, call.Args[0])
			if receiverType == "messageTransport" {
				mine = append(mine, registration{pos: pos, callee: callee, owner: receiverType})
			} else {
				others = append(others, fmt.Sprintf("%s registers %s (on %s) at %s",
					key, types.ExprString(call.Args[0]), orUnknown(receiverType), pos))
			}
			return true
		})
	}
	return mine, others
}

// `x.receive` resolves to the decl key `<the type of x>.receive`.
//
// The type of `x` is derived rather than assumed: it is the enclosing method's
// receiver type when `x` is the receiver, and otherwise the composite-literal
// type `x` was built from inside the enclosing function. That is what makes this
// gate find its own registration in [newMessageTransport], where the transport
// is a LOCAL and not a receiver — the earlier draft keyed on the enclosing
// function's receiver, found none, and would have reported zero registrations
// for a binding that plainly has one.
func (self *borrowGate) resolveCallback(enclosing *ast.FuncDecl, arg ast.Expr) (string, string) {
	selector, ok := arg.(*ast.SelectorExpr)
	if !ok {
		return types.ExprString(arg), ""
	}
	base, ok := selector.X.(*ast.Ident)
	if !ok {
		return types.ExprString(arg), ""
	}
	receiverType := localTypeOf(enclosing, base.Name)
	if receiverType == "" {
		return types.ExprString(arg), ""
	}
	return receiverType + "." + selector.Sel.Name, receiverType
}

// The named type a local or receiver was built from, read syntactically: the
// enclosing method's receiver type, or the composite literal the local was
// assigned. Nothing here type-checks, so a local built any other way reads as
// unknown and its registration lands in the complement, where it is PRINTED
// rather than silently dropped.
func localTypeOf(enclosing *ast.FuncDecl, name string) string {
	if name == recvName(enclosing) {
		return recvTypeName(enclosing)
	}
	found := ""
	ast.Inspect(enclosing, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || ident.Name != name || len(assign.Rhs) <= index {
				continue
			}
			if spelled := compositeTypeName(assign.Rhs[index]); spelled != "" {
				found = spelled
			}
		}
		return true
	})
	return found
}

func compositeTypeName(expr ast.Expr) string {
	if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		expr = unary.X
	}
	composite, ok := expr.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	if ident, ok := composite.Type.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func orUnknown(name string) string {
	if name == "" {
		return "an unresolved type"
	}
	return name
}

// The dynamic extent: the root plus every package function it transitively
// calls.
func (self *borrowGate) walkExtent(root string) {
	queue := []string{root}
	for 0 < len(queue) {
		key := queue[0]
		queue = queue[1:]
		if self.extent[key] {
			continue
		}
		decl := self.decls[key]
		if decl == nil {
			continue
		}
		self.extent[key] = true
		self.order = append(self.order, key)
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if callee := self.resolveCallee(decl, call); callee != "" {
				queue = append(queue, callee)
			}
			return true
		})
	}
}

func (self *borrowGate) resolveCallee(enclosing *ast.FuncDecl, call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if _, found := self.decls[fun.Name]; found {
			return fun.Name
		}
	case *ast.SelectorExpr:
		receiver, ok := fun.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if receiver.Name != recvName(enclosing) {
			return ""
		}
		key := recvTypeName(enclosing) + "." + fun.Sel.Name
		if _, found := self.decls[key]; found {
			return key
		}
	}
	return ""
}

// The production files that declare a method on messageTransport. Derived, so
// that Tasks 6 and 7's files join the scope on the commit that adds them.
func (self *borrowGate) bindingFiles() []string {
	seen := map[string]bool{}
	names := []string{}
	for _, key := range self.declOrder {
		if recvTypeName(self.decls[key]) != "messageTransport" {
			continue
		}
		name := self.declFile[key]
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (self *borrowGate) declarationsIn(files []string) []string {
	within := map[string]bool{}
	for _, name := range files {
		within[name] = true
	}
	keys := []string{}
	for _, key := range self.declOrder {
		if within[self.declFile[key]] {
			keys = append(keys, key)
		}
	}
	return keys
}

func (self *borrowGate) partition(files []string) (inside []string, outside []string) {
	for _, key := range self.declarationsIn(files) {
		if self.extent[key] {
			inside = append(inside, key)
		} else {
			outside = append(outside, key)
		}
	}
	return inside, outside
}

// ── the taint walk ───────────────────────────────────────────────────────────

func (self *borrowGate) analyze(key string, roots map[string]bool) {
	decl := self.decls[key]
	if decl == nil || decl.Body == nil || len(roots) == 0 {
		return
	}
	memo := key + "|" + strings.Join(sortedKeys(roots), ",")
	if self.analyzed[memo] {
		return
	}
	self.analyzed[memo] = true

	tainted := map[string]bool{}
	for name := range roots {
		tainted[name] = true
	}

	// fixpoint: a borrowed value bound to a local makes that local borrowed too
	for round := 0; round < 32; round += 1 {
		grew := false
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.AssignStmt:
				for index, rhs := range statement.Rhs {
					if !isBorrowed(rhs, tainted) {
						continue
					}
					for _, lhs := range lhsFor(statement, index) {
						if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" && !tainted[ident.Name] {
							tainted[ident.Name] = true
							grew = true
						}
					}
				}
			case *ast.RangeStmt:
				if !isBorrowed(statement.X, tainted) {
					return true
				}
				for _, each := range []ast.Expr{statement.Key, statement.Value} {
					if ident, ok := each.(*ast.Ident); ok && ident.Name != "_" && !tainted[ident.Name] {
						tainted[ident.Name] = true
						grew = true
					}
				}
			case *ast.ValueSpec:
				for index, value := range statement.Values {
					if !isBorrowed(value, tainted) || len(statement.Names) <= index {
						continue
					}
					if name := statement.Names[index].Name; name != "_" && !tainted[name] {
						tainted[name] = true
						grew = true
					}
				}
			}
			return true
		})
		if !grew {
			break
		}
	}

	// every occurrence of a borrowed identifier, so that a classifier which
	// walks past a construction is caught by the coverage assertion rather than
	// by nobody
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || !tainted[ident.Name] {
			return true
		}
		self.borrowLines[self.fset.Position(ident.Pos()).String()] = true
		return true
	})

	// classification
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		// A function literal is ruled on AS A LITERAL and is not descended
		// into. Descending is what cleared review finding F1: the only
		// statement inside `func() []byte { return frame.GetMessageBytes() }`
		// is a ReturnStmt, and the ReturnStmt arm below clears a borrowed
		// result because the caller of THIS declaration is inside the callback
		// -- which is true of this declaration and false of a closure, whose
		// caller is whoever invokes it, after the callback returned, over a
		// buffer connect has already reclaimed.
		if lit, isLit := node.(*ast.FuncLit); isLit {
			names := borrowedNames(lit, tainted)
			if len(names) == 0 {
				return false
			}
			switch self.litRole[lit] {
			case "go":
				// the GoStmt arm below flags the whole statement, with the
				// reason that is about the goroutine rather than the closure
			case "defer":
				self.clear(lit, strings.Join(names, ", "),
					"captured by a deferred literal, which runs before the callback returns")
			case "called":
				self.clear(lit, strings.Join(names, ", "),
					"captured by a literal that is invoked on the spot, inside the callback")
			default:
				self.flag(lit, strings.Join(names, ", "),
					"captured by a function literal that is stored, returned or passed rather than "+
						"run here. A closure over a borrowed value is a reference whose lifetime the "+
						"callback does not control: whoever calls it calls it after the callback "+
						"returned, over a buffer connect has reclaimed")
			}
			return false
		}

		switch statement := node.(type) {

		case *ast.AssignStmt:
			for index, rhs := range statement.Rhs {
				if !isBorrowed(rhs, tainted) {
					continue
				}
				for _, lhs := range lhsFor(statement, index) {
					if ident, ok := lhs.(*ast.Ident); ok {
						if where, isPackage := self.packageValues[ident.Name]; isPackage {
							self.flag(statement, types.ExprString(rhs),
								fmt.Sprintf("stored into the PACKAGE-LEVEL %s, declared at %s, which "+
									"outlives this callback and every callback after it", ident.Name, where))
							continue
						}
						self.clear(statement, types.ExprString(rhs),
							fmt.Sprintf("bound to the local %s, which dies with the callback", ident.Name))
						continue
					}
					self.flag(statement, types.ExprString(rhs),
						fmt.Sprintf("stored into %s, which outlives the callback", types.ExprString(lhs)))
				}
			}

		case *ast.SendStmt:
			if isBorrowed(statement.Value, tainted) || isBorrowed(statement.Chan, tainted) {
				self.flag(statement, types.ExprString(statement.Value),
					fmt.Sprintf("sent on the channel %s", types.ExprString(statement.Chan)))
			}

		case *ast.GoStmt:
			if names := borrowedNames(statement, tainted); 0 < len(names) {
				self.flag(statement, strings.Join(names, ", "),
					"referenced inside a `go` statement. Promptness is not the rule: a goroutine "+
						"that copies immediately is still a goroutine the callback does not wait for")
			}

		case *ast.DeferStmt:
			if names := borrowedNames(statement, tainted); 0 < len(names) {
				self.clear(statement, strings.Join(names, ", "),
					"deferred, which runs before the callback returns")
			}

		case *ast.RangeStmt:
			if isBorrowed(statement.X, tainted) {
				self.clear(statement, types.ExprString(statement.X), "ranged over, and read element by element")
			}

		case *ast.IfStmt:
			if statement.Cond != nil && isBorrowed(statement.Cond, tainted) {
				self.clear(statement, types.ExprString(statement.Cond), "read in a condition; a comparison keeps nothing")
			}

		case *ast.SwitchStmt:
			if statement.Tag != nil && isBorrowed(statement.Tag, tainted) {
				self.clear(statement, types.ExprString(statement.Tag), "read as a switch tag; a comparison keeps nothing")
			}

		case *ast.CaseClause:
			for _, each := range statement.List {
				if isBorrowed(each, tainted) {
					self.clear(statement, types.ExprString(each), "compared in a case clause")
				}
			}

		case *ast.ReturnStmt:
			for _, each := range statement.Results {
				if isBorrowed(each, tainted) {
					self.clear(statement, types.ExprString(each),
						"returned to a caller that is itself inside the callback")
				}
			}

		case *ast.ExprStmt:
			if isBorrowed(statement.X, tainted) {
				self.clear(statement, types.ExprString(statement.X), "evaluated and discarded")
			}

		case *ast.CallExpr:
			self.classifyCall(decl, statement, tainted)
		}
		return true
	})
}

func (self *borrowGate) classifyCall(enclosing *ast.FuncDecl, call *ast.CallExpr, tainted map[string]bool) {
	name := calleeName(call)

	borrowedArgs := []ast.Expr{}
	for _, arg := range call.Args {
		if isBorrowed(arg, tainted) {
			borrowedArgs = append(borrowedArgs, arg)
		}
	}

	if len(borrowedArgs) == 0 {
		// a method on a borrowed value: reading it is the whole point of the
		// callback, and the RESULT stays in the class
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && isBorrowed(selector.X, tainted) {
			self.clear(call, types.ExprString(call),
				"a method called on a borrowed value; its result is treated as borrowed too")
		}
		return
	}

	if callee := self.resolveCallee(enclosing, call); callee != "" {
		// in-package: the class travels into the callee and this gate walks it
		roots := map[string]bool{}
		target := self.decls[callee]
		for index, arg := range call.Args {
			if !isBorrowed(arg, tainted) {
				continue
			}
			if param := paramNameAt(target, index); param != "" {
				roots[param] = true
			}
		}
		self.clear(call, types.ExprString(call),
			fmt.Sprintf("passed to %s, which is inside the extent and walked by this gate", callee))
		self.analyze(callee, roots)
		return
	}

	if reason, sanitizes := borrowSanitizers[name]; sanitizes {
		self.clear(call, types.ExprString(call), fmt.Sprintf("passed to %s: %s", name, reason))
		return
	}

	shown := []string{}
	for _, arg := range borrowedArgs {
		shown = append(shown, types.ExprString(arg))
	}
	self.flag(call, strings.Join(shown, ", "),
		fmt.Sprintf("handed to %s, which is outside package sdk and outside this gate's sanitizer set, "+
			"so nothing here establishes that it does not retain the value", name))
}

func (self *borrowGate) clear(node ast.Node, what string, reason string) {
	pos := self.fset.Position(node.Pos()).String()
	self.cleared = append(self.cleared, borrowSite{pos: pos, what: what, reason: reason})
	self.coverLine(node)
}

func (self *borrowGate) flag(node ast.Node, what string, reason string) {
	pos := self.fset.Position(node.Pos()).String()
	self.flagged = append(self.flagged, borrowSite{pos: pos, what: what, reason: reason})
	self.coverLine(node)
}

// A classification covers every line of the construction it classified, because
// a `go` statement or a call can span several.
func (self *borrowGate) coverLine(node ast.Node) {
	start := self.fset.Position(node.Pos())
	end := self.fset.Position(node.End())
	for line := start.Line; line <= end.Line; line += 1 {
		self.coveredLine[fmt.Sprintf("%s:%d:", start.Filename, line)] = true
	}
}

func (self *borrowGate) borrowLineCount() int {
	return len(self.borrowLines)
}

func (self *borrowGate) uncoveredBorrowLines() []string {
	uncovered := []string{}
	for position := range self.borrowLines {
		parts := strings.Split(position, ":")
		if len(parts) < 3 {
			continue
		}
		prefix := strings.Join(parts[:len(parts)-2], ":") + ":" + parts[len(parts)-2] + ":"
		if !self.coveredLine[prefix] {
			uncovered = append(uncovered, position)
		}
	}
	sort.Strings(uncovered)
	return uncovered
}

// ── expression helpers ───────────────────────────────────────────────────────

// Conservative by construction: anything derived from a borrowed value is
// borrowed until a sanitizer copies it.
func isBorrowed(expr ast.Expr, tainted map[string]bool) bool {
	switch each := expr.(type) {
	case nil:
		return false
	case *ast.FuncLit:
		// a closure that captures a borrowed value IS a borrowed value: it is a
		// reference to one, held for as long as the closure is held. Review
		// finding F1 -- borrowedNames, which the GoStmt and DeferStmt arms use,
		// always saw captured identifiers; this function did not, so the
		// AssignStmt arm read the store of a thunk as the store of something
		// unborrowed.
		return 0 < len(borrowedNames(each, tainted))
	case *ast.Ident:
		return tainted[each.Name]
	case *ast.SelectorExpr:
		return isBorrowed(each.X, tainted)
	case *ast.IndexExpr:
		return isBorrowed(each.X, tainted) || isBorrowed(each.Index, tainted)
	case *ast.SliceExpr:
		return isBorrowed(each.X, tainted)
	case *ast.StarExpr:
		return isBorrowed(each.X, tainted)
	case *ast.ParenExpr:
		return isBorrowed(each.X, tainted)
	case *ast.UnaryExpr:
		return isBorrowed(each.X, tainted)
	case *ast.BinaryExpr:
		return isBorrowed(each.X, tainted) || isBorrowed(each.Y, tainted)
	case *ast.TypeAssertExpr:
		return isBorrowed(each.X, tainted)
	case *ast.KeyValueExpr:
		return isBorrowed(each.Value, tainted)
	case *ast.CompositeLit:
		for _, element := range each.Elts {
			if isBorrowed(element, tainted) {
				return true
			}
		}
		return false
	case *ast.CallExpr:
		if _, sanitizes := borrowSanitizers[calleeName(each)]; sanitizes {
			return false
		}
		if isBorrowed(each.Fun, tainted) {
			return true
		}
		for _, arg := range each.Args {
			if isBorrowed(arg, tainted) {
				return true
			}
		}
		return false
	}
	return false
}

func borrowedNames(node ast.Node, tainted map[string]bool) []string {
	seen := map[string]bool{}
	names := []string{}
	ast.Inspect(node, func(each ast.Node) bool {
		ident, ok := each.(*ast.Ident)
		if !ok || !tainted[ident.Name] || seen[ident.Name] {
			return true
		}
		seen[ident.Name] = true
		names = append(names, ident.Name)
		return true
	})
	sort.Strings(names)
	return names
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			return pkg.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	}
	return types.ExprString(call.Fun)
}

func lhsFor(statement *ast.AssignStmt, index int) []ast.Expr {
	if len(statement.Lhs) == len(statement.Rhs) {
		return []ast.Expr{statement.Lhs[index]}
	}
	return statement.Lhs
}

func paramNameAt(decl *ast.FuncDecl, index int) string {
	if decl == nil || decl.Type.Params == nil {
		return ""
	}
	position := 0
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			if position == index {
				return ""
			}
			position += 1
			continue
		}
		for _, name := range field.Names {
			if position == index {
				return name.Name
			}
			position += 1
		}
	}
	return ""
}

func fieldNames(fields *ast.FieldList) []string {
	names := []string{}
	if fields == nil {
		return names
	}
	for _, field := range fields.List {
		spelled := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			names = append(names, "_ "+spelled)
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name+" "+spelled)
		}
	}
	return names
}

func declKey(decl *ast.FuncDecl) string {
	if recv := recvTypeName(decl); recv != "" {
		return recv + "." + decl.Name.Name
	}
	return decl.Name.Name
}

func recvTypeName(decl *ast.FuncDecl) string {
	if decl == nil || decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	expr := decl.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if index, ok := expr.(*ast.IndexExpr); ok {
		expr = index.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func recvName(decl *ast.FuncDecl) string {
	if decl == nil || decl.Recv == nil || len(decl.Recv.List) == 0 || len(decl.Recv.List[0].Names) == 0 {
		return ""
	}
	return decl.Recv.List[0].Names[0].Name
}

func setOf(entries ...string) map[string]bool {
	set := map[string]bool{}
	for _, entry := range entries {
		// fieldNames spells "name type"; the root set is the names
		set[strings.SplitN(entry, " ", 2)[0]] = true
	}
	delete(set, "_")
	return set
}

func sortedKeys(set map[string]bool) []string {
	keys := []string{}
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ═════════════════════════════════════════════════════════════════════════════
// The classifier, driven against constructions that are NOT in this package.
// ═════════════════════════════════════════════════════════════════════════════
//
// WHY THIS EXISTS, stated as the defect it answers. C3 above asserts that every
// line carrying a borrowed identifier received A VERDICT. It does not assert
// that the verdict was RIGHT, and the Task 5 review found two constructions
// where it was not: a closure over the borrowed bytes stored in a struct field,
// and a borrowed frame assigned to a package-level variable, each printing a
// CLEARANCE while the value escaped. Both satisfied the coverage identity. The
// count was right and the verdicts were wrong, and nothing in a gate whose only
// subject is the shipped source could have said so -- the shipped source does
// not contain the constructions the gate is supposed to refuse.
//
// So the classifier is driven HERE over source written to be wrong. Each case
// below is a construction and the verdict the rule owes it; the gate is built
// over that source alone, walked from its own registration, and the verdict
// read back. A rule that stops refusing something turns this red on the case
// that names it, without anybody having to plant a mutation in a production
// file and remember to take it out.
//
// GATE CLASS: the verdict the classifier returns for one construction.
// GATE SCOPE: the synthetic package of each case, which is deliberately NOT
//
//	package sdk -- a self-test over the shipped source could only
//	ever re-derive that the shipped source is clean.
func TestTheBorrowClassifierRefusesWhatItClaimsTo(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		extra   string
		escapes bool
		why     string
	}{
		{
			name:    "the frame stored in a map the transport holds",
			body:    "self.held[1] = frame",
			escapes: true,
			why:     "plan mutation 1",
		},
		{
			name:    "the frame's bytes kept without copying",
			body:    "self.bytes = frame.GetMessageBytes()",
			escapes: true,
			why:     "plan mutation 2",
		},
		{
			name:    "a goroutine that copies promptly",
			body:    "go func(borrowed []byte) { copied := append([]byte(nil), borrowed...); _ = copied }(frame.GetMessageBytes())",
			escapes: true,
			why:     "plan mutation 3 -- promptness is not the rule",
		},
		{
			name:    "a closure over the borrowed bytes, stored in a field",
			body:    "self.later = func() []byte { return frame.GetMessageBytes() }",
			escapes: true,
			why:     "review finding F1: cleared before this repair, with the reason \"returned to a caller that is itself inside the callback\"",
		},
		{
			name:    "the frame assigned to a package-level variable",
			body:    "messageTransportProbeLast = frame",
			escapes: true,
			why:     "review finding F2: cleared before this repair as \"bound to the local\"",
		},
		{
			name:    "the frame sent on a channel",
			body:    "self.answers <- frame",
			escapes: true,
			why:     "connect: never hand a borrowed Frame to a channel",
		},
		{
			name:    "the frame handed to a function this gate cannot see inside",
			body:    "elsewhere.Keep(frame)",
			escapes: true,
			why:     "outside the package and outside the sanitizer set",
		},
		{
			name:    "the frame appended to a slice the transport holds",
			body:    "self.keep = append(self.keep, frame)",
			escapes: true,
			why:     "a store through append is still a store",
		},
		{
			name: "the frame stashed two hops away, renamed at every hop",
			body: "self.stashOuter(frame)",
			extra: "func (self *messageTransport) stashOuter(f *protocol.Frame) { self.stashInner(f) }\n" +
				"func (self *messageTransport) stashInner(g *protocol.Frame) { self.keep = append(self.keep, g) }",
			escapes: true,
			why:     "the scope is the dynamic extent, not the lexical body",
		},
		{
			name:    "the bytes decoded into a message of our own",
			body:    "response := &protocol.MessageServerResponse{}\n\t\t_ = proto.Unmarshal(frame.GetMessageBytes(), response)",
			escapes: false,
			why:     "Unmarshal copies every byte it keeps",
		},
		{
			name:    "the bytes bound to a local and measured",
			body:    "borrowed := frame.GetMessageBytes()\n\t\t_ = len(borrowed)",
			escapes: false,
			why:     "a local dies with the callback and len reads a length",
		},
		{
			name:    "the bytes copied into a buffer of our own",
			body:    "ours := make([]byte, len(frame.GetMessageBytes()))\n\t\tcopy(ours, frame.GetMessageBytes())",
			escapes: false,
			why:     "copy is what the rule tells you to do",
		},
		{
			name:    "the frame read in a condition",
			body:    "if frame.GetMessageType() != 0 {\n\t\t\tcontinue\n\t\t}",
			escapes: false,
			why:     "a comparison keeps nothing",
		},
		{
			name:    "the bytes captured by a deferred literal",
			body:    "defer func() { _ = len(frame.GetMessageBytes()) }()",
			escapes: false,
			why:     "a defer runs before the callback returns",
		},
	}

	refusedEscapes := 0
	clearedSafe := 0
	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			source := fmt.Sprintf(borrowClassifierProbe, each.body, each.extra)
			gate := newBorrowGateOver(t, map[string]any{"borrow_probe.go": source})
			mine, others := gate.receiveRegistrations()
			if len(mine) != 1 {
				t.Fatalf("the probe declares %d registrations on messageTransport %v (others %v), want 1",
					len(mine), mine, others)
			}
			root := mine[0].callee
			decl := gate.decls[root]
			if decl == nil {
				t.Fatalf("the probe registers %s, which it does not declare", root)
			}
			gate.walkExtent(root)
			gate.analyze(root, setOf(fieldNames(decl.Type.Params)...))

			verdicts := []string{}
			for _, site := range gate.cleared {
				verdicts = append(verdicts, fmt.Sprintf("CLEARED %s %s -- %s", site.pos, site.what, site.reason))
			}
			for _, site := range gate.flagged {
				verdicts = append(verdicts, fmt.Sprintf("FLAGGED %s %s -- %s", site.pos, site.what, site.reason))
			}
			t.Logf("extent %d %v; %d verdict(s):", len(gate.order), gate.order, len(verdicts))
			for _, verdict := range verdicts {
				t.Logf("    %s", verdict)
			}
			if uncovered := gate.uncoveredBorrowLines(); len(uncovered) != 0 {
				t.Fatalf("%d borrowed identifier(s) got no verdict at all: %v", len(uncovered), uncovered)
			}
			if len(verdicts) == 0 {
				t.Fatal("the classifier returned NO verdict for this construction, so it did not look at it")
			}

			if each.escapes && len(gate.flagged) == 0 {
				t.Fatalf("this construction ESCAPES the callback and the classifier cleared it (%s). "+
					"Verdicts above. A clearance here is the shape review findings F1 and F2 had: the "+
					"count is right, the coverage identity is satisfied, and the value is gone",
					each.why)
			}
			if !each.escapes && len(gate.flagged) != 0 {
				t.Fatalf("this construction is SAFE (%s) and the classifier flagged it: %v. "+
					"A gate that refuses correct code is a gate that gets deleted", each.why, gate.flagged)
			}
		})
		if each.escapes {
			refusedEscapes += 1
		} else {
			clearedSafe += 1
		}
	}

	// R5's two halves, as numbers rather than as an assurance: the classifier is
	// FALSIFIABLE by the constructions above that escape, and SATISFIABLE by the
	// ones that do not. A suite with only one half is a gate that either refuses
	// everything or refuses nothing, and both pass a coverage check.
	t.Logf("constructions that must be refused: %d; constructions that must be cleared: %d; total %d",
		refusedEscapes, clearedSafe, len(cases))
	if refusedEscapes == 0 || clearedSafe == 0 {
		t.Fatalf("this table has %d refusals and %d clearances: a classifier tested in one direction only "+
			"is a classifier that can be right by refusing everything, or by refusing nothing",
			refusedEscapes, clearedSafe)
	}
}

// The probe package. It is never compiled -- go/parser is the only thing that
// reads it -- so the types are spelled the way the real binding spells them and
// nothing here needs to resolve.
const borrowClassifierProbe = `package sdk

type messageTransport struct {
	held    map[uint64]*protocol.Frame
	bytes   []byte
	later   func() []byte
	answers chan *protocol.Frame
	keep    []*protocol.Frame
}

var messageTransportProbeLast *protocol.Frame

func newMessageTransport(client messageTransportClient) *messageTransport {
	self := &messageTransport{}
	client.AddReceiveCallback(self.receive)
	return self
}

func (self *messageTransport) receive(source connect.TransferPath, frames []*protocol.Frame, from connect.Peer) {
	for _, frame := range frames {
		%s
	}
}

%s
`
