package sdk

// Task 7's four properties: Hello, the per-connection nonce, the Capabilities
// cache, and the compiled-in part budget the first request of a connection has
// to use.
//
// ── WHAT THIS FILE CANNOT SEE ────────────────────────────────────────────────
//
// The boundary of message_transport_test.go, and one more: every Hello here is
// answered by the injected client, so nothing here establishes that a real
// server issues 32 octets, that it replaces the nonce on every Hello rather
// than on the first, or that it advertises the fields §4.3.1 says it does.
// msgrepo/peer/hello.go was READ for that — `Connections.Open` replaces
// unconditionally, and `capabilities` is cloned out of one process-lifetime
// value — and deliberately not imported: `sdk` cannot import the server module.
// What is established here is what THIS side does with what it is told.
//
// AND ONE MORE, WHICH IS REVIEW FINDING E AND IS THE GAP BETWEEN PROPERTY 2 AS
// THE PLAN WORDS IT AND PROPERTY 2 AS THIS FILE ESTABLISHES IT.
//
// The plan's Property 2 is "the nonce a caller reads is the CURRENT
// connection's, and a nonce from a superseded connection is never handed out".
// What is established below is the narrower "the nonce a caller reads is the one
// the most recent COMPLETED HELLO issued, and the epoch moves on every Hello".
// The two coincide only if every connection change is followed by a Hello
// through this transport, and NOTHING IN `sdk` ARRANGES THAT:
// `grep -rn 'nonceEpoch' --include=*.go .` in sdk returns three lines — the
// field, the increment in [messageTransport.adopt], and the accessor — and
// `adopt` is called from exactly one place, a Hello that returned REASON_OK on
// the Hello arm. Nothing here observes a `connect` reconnect, and nothing in
// `sdk` constructs the `connect.Client` at all.
//
// So under a transparent reconnect that no Hello follows, [messageTransport.Nonce]
// hands out a superseded nonce at an unchanged epoch, and Task 10's refusal —
// which compares a seal-time epoch against the current one — sees no change and
// submits. That is not fixable in this task: it needs something in `sdk` to be
// told that the connection changed, which is S2-7, and it is the shadow S2-2
// casts over this property. Filed beside S2-2 rather than left for Task 10 to
// discover.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// What the injected client answers a Hello with, and a way for a test to change
// it between connections.
type messageHelloAnswers struct {
	mutex        sync.Mutex
	nonce        string
	capabilities *protocol.Capabilities
	reason       protocol.Reason
	wrongArm     bool
}

func (self *messageHelloAnswers) advertise(nonce string, capabilities *protocol.Capabilities) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.nonce = nonce
	self.capabilities = capabilities
	self.reason = protocol.Reason_REASON_OK
}

func (self *messageHelloAnswers) response(requestId uint64) *protocol.MessageServerResponse {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	answer := &protocol.MessageServerResponse{RequestId: requestId, Reason: self.reason}
	if self.reason != protocol.Reason_REASON_OK {
		return answer
	}
	if self.wrongArm {
		answer.Body = &protocol.MessageServerResponse_Submit{Submit: &protocol.SubmitResponse{}}
		return answer
	}
	answer.Body = &protocol.MessageServerResponse_Hello{Hello: &protocol.HelloResponse{
		ServerNonce:  []byte(self.nonce),
		Capabilities: self.capabilities,
	}}
	return answer
}

// A transport whose client answers every Hello inline, the way connect's
// receive path can: from inside the send, before the caller reaches its select.
func newHelloTransport(t *testing.T, answers *messageHelloAnswers) (*messageTransportFake, *messageTransport) {
	t.Helper()
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)
	fake.onSend = func(request *protocol.MessageServerRequest) {
		if request.GetHello() == nil {
			return
		}
		fake.answer(t, answers.response(request.GetRequestId()))
	}
	return fake, transport
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 1 — no request that requires an authenticator precedes Hello.
// ─────────────────────────────────────────────────────────────────────────────
//
// GATE CLASS, derived and stated separately from the scope (R3):
//
//	every arm of `MessageServerRequest.body` EXCEPT the one that issues this
//	connection's `server_nonce`. The exempt arm is found from the RESPONSE side
//	— the one arm of `MessageServerResponse.body` whose message declares a field
//	named `server_nonce` — and carried back across §4.2's invariant that a
//	response arm and its request arm share a name AND a number. Nothing is
//	listed: an arm added to the oneof upstream joins the class on the build that
//	adds it.
//
// GATE SCOPE, derived separately from the class:
//
//	the `Call` path from the caller's argument to the client's SendWithTimeout,
//	asserted at both ends — the typed refusal comes back AND the injected client
//	was handed nothing. "Refused locally, not on the wire" is two claims and the
//	second one is the one plan mutation 1 is about.
//
// The over-approximation is deliberate and is stated in the production file: two
// arms carry an authenticator no descriptor rule can see, and refusing them
// before Hello is right anyway because §5.1 check 2 resolves the connection the
// server opened at Hello.
func TestNoRequestThatNeedsTheConnectionNonceGoesOutBeforeHello(t *testing.T) {
	issuing, err := messageTransportNonceIssuingArm()
	if err != nil {
		t.Fatalf("the exempt arm could not be derived from the descriptors: %v", err)
	}
	oneof := (&protocol.MessageServerRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	if oneof == nil {
		t.Fatal("protocol.MessageServerRequest declares no `body` oneof")
	}

	needs := []string{}
	exempt := []string{}
	bodies := map[string]proto.Message{}
	for index := 0; index < oneof.Fields().Len(); index += 1 {
		field := oneof.Fields().Get(index)
		if field.Kind() != protoreflect.MessageKind {
			t.Fatalf("the arm %s of the request body oneof is a %s and not a message", field.Name(), field.Kind())
		}
		full := field.Message().FullName()
		messageType, err := protoregistry.GlobalTypes.FindMessageByName(full)
		if err != nil {
			t.Fatalf("this build links no Go type for the request arm %s: %v", full, err)
		}
		bodies[string(full)] = messageType.New().Interface()
		if full == issuing {
			exempt = append(exempt, string(full))
			continue
		}
		needs = append(needs, string(full))
	}

	t.Logf("GATE CLASS: %d arm(s) of MessageServerRequest.body need this connection's server_nonce: %v",
		len(needs), needs)
	t.Logf("COMPLEMENT — the arm that ISSUES it, derived from the response arm declaring server_nonce "+
		"rather than from its name: %d %v", len(exempt), exempt)
	if len(exempt) != 1 {
		t.Fatalf("%d arm(s) are exempt from Hello, want exactly 1: an exemption that is not one arm is "+
			"either a nonce nothing issues or a nonce everything issues", len(exempt))
	}
	if len(needs) == 0 {
		t.Fatal("the class is EMPTY: every arm of the request oneof would be exempt from Hello, which " +
			"is not a refusal, it is an absence of one")
	}
	if len(needs)+len(exempt) != oneof.Fields().Len() {
		t.Fatalf("%d needing + %d exempt is not the %d arms the oneof declares: the partition does not close",
			len(needs), len(exempt), oneof.Fields().Len())
	}

	// ── falsifiable half: before Hello, every member of the class is refused
	//    here and reaches no wire ─────────────────────────────────────────────
	for _, name := range needs {
		t.Run(strings.TrimPrefix(name, "bringyour."), func(t *testing.T) {
			fake := &messageTransportFake{}
			transport := newTestMessageTransport(t, fake, 200*time.Millisecond)
			if epoch := transport.NonceEpoch(); epoch != 0 {
				t.Fatalf("NonceEpoch() is %d on a transport that has said no Hello, want 0", epoch)
			}
			_, err := transport.Call(context.Background(), bodies[name])
			if !errors.Is(err, errMessageTransportNoHello) {
				t.Fatalf("a %s before Hello was refused with %v, want errMessageTransportNoHello — "+
					"every §5.4 write_auth and every §4.3.8 req_auth is a MAC over a nonce this "+
					"connection does not have", name, err)
			}
			if points := fake.codePoints(); len(points) != 0 {
				t.Fatalf("%d frame(s) reached the client before Hello: %v. The refusal Property 1 owes is "+
					"raised LOCALLY, never a request put on the wire to be refused there", len(points), points)
			}
			if waiting := transport.Counts().Waiting; waiting != 0 {
				t.Fatalf("Counts().Waiting is %d after a locally refused request, want 0", waiting)
			}
		})
	}

	// ── satisfiable half: after Hello, the same bodies are not refused for
	//    this reason. Without it the gate is satisfied by a Call that refuses
	//    everything forever ───────────────────────────────────────────────────
	answers := &messageHelloAnswers{}
	answers.advertise("a-connection-nonce", &protocol.Capabilities{})
	fake, transport := newHelloTransport(t, answers)
	reason, hello, err := transport.Hello(context.Background(), 1)
	if err != nil || reason != protocol.Reason_REASON_OK || hello == nil {
		t.Fatalf("the Hello failed: reason %v, hello %v, err %v", reason, hello, err)
	}
	for _, name := range needs {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := transport.Call(ctx, bodies[name])
		cancel()
		if errors.Is(err, errMessageTransportNoHello) {
			t.Fatalf("a %s was refused as preceding Hello AFTER a Hello completed: %v", name, err)
		}
	}
	if len(fake.codePoints()) < len(needs) {
		t.Fatalf("only %d frame(s) reached the client for %d requests after Hello, so the refusal did not "+
			"stop being raised, it stopped being reported", len(fake.codePoints()), len(needs))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 2 — the nonce a caller reads is the CURRENT connection's.
// ─────────────────────────────────────────────────────────────────────────────

func TestTheNonceIsTheCurrentConnectionsAndIsHandedOutAsACopy(t *testing.T) {
	answers := &messageHelloAnswers{}
	_, transport := newHelloTransport(t, answers)

	if nonce := transport.Nonce(); len(nonce) != 0 {
		t.Fatalf("Nonce() is %q before any Hello, want empty", nonce)
	}
	if epoch := transport.NonceEpoch(); epoch != 0 {
		t.Fatalf("NonceEpoch() is %d before any Hello, want 0", epoch)
	}

	answers.advertise("first-connection-nonce", &protocol.Capabilities{MaxRequestBytes: 100000})
	if reason, hello, err := transport.Hello(context.Background(), 1); err != nil || hello == nil {
		t.Fatalf("the first Hello failed: reason %v, err %v", reason, err)
	}
	first := transport.Nonce()
	if string(first) != "first-connection-nonce" {
		t.Fatalf("Nonce() is %q after the first Hello, want the nonce the server issued", first)
	}
	firstEpoch := transport.NonceEpoch()
	if firstEpoch != 1 {
		t.Fatalf("NonceEpoch() is %d after one Hello, want 1", firstEpoch)
	}

	// a caller that mutates what it was handed cannot move the transport
	for index := range first {
		first[index] ^= 0xFF
	}
	if string(transport.Nonce()) != "first-connection-nonce" {
		t.Fatalf("Nonce() is %q after a caller wrote through what it was handed: it returned the "+
			"transport's OWN slice, and that slice is the input to every MAC a caller computes — "+
			"a whole session could seal against something the server never issued with no line of "+
			"that session's code saying so", transport.Nonce())
	}

	// the reconnect. The server replaces its nonce unconditionally at every
	// Hello; a client that kept the one it had is MAC'ing against nothing
	answers.advertise("second-connection-nonce", &protocol.Capabilities{MaxRequestBytes: 100000})
	if _, hello, err := transport.Hello(context.Background(), 1); err != nil || hello == nil {
		t.Fatalf("the second Hello failed: %v", err)
	}
	if got := string(transport.Nonce()); got != "second-connection-nonce" {
		t.Fatalf("Nonce() is %q after a second Hello, want the second connection's — the nonce is a "+
			"property of the CONNECTION and the server replaced it", got)
	}
	secondEpoch := transport.NonceEpoch()
	if secondEpoch != 2 {
		t.Fatalf("NonceEpoch() is %d after two Hellos, want 2 — an epoch that does not move is an epoch "+
			"no caller can use to tell that what it sealed against is gone, which is exactly what makes "+
			"Task 10's refusal unreachable", secondEpoch)
	}
	if secondEpoch <= firstEpoch {
		t.Fatalf("the epoch went %d -> %d across a reconnect: a caller that read a nonce at %d has no "+
			"way to tell it is stale", firstEpoch, secondEpoch, firstEpoch)
	}
}

func TestCapabilitiesAreHandedOutAsACloneAndReplacedByEveryHello(t *testing.T) {
	answers := &messageHelloAnswers{}
	_, transport := newHelloTransport(t, answers)

	if capabilities := transport.Capabilities(); capabilities != nil {
		t.Fatalf("Capabilities() is %v before any Hello, want nil — there is nothing advertised yet, and "+
			"that is Property 4's whole point", capabilities)
	}

	answers.advertise("n1", &protocol.Capabilities{MaxRequestBytes: 100000, MaxRecordsPerSubmit: 256})
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the Hello failed: %v", err)
	}
	advertised := transport.Capabilities()
	if advertised.GetMaxRequestBytes() != 100000 {
		t.Fatalf("Capabilities().max_request_bytes is %d, want 100000", advertised.GetMaxRequestBytes())
	}
	advertised.MaxRequestBytes = 7
	if again := transport.Capabilities(); again.GetMaxRequestBytes() != 100000 {
		t.Fatalf("a caller wrote through Capabilities() and moved the transport's own advertisement to "+
			"%d: it is a CLONE for Nonce()'s reason", again.GetMaxRequestBytes())
	}

	answers.advertise("n2", &protocol.Capabilities{MaxRequestBytes: 64})
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the second Hello failed: %v", err)
	}
	if got := transport.Capabilities().GetMaxRequestBytes(); got != 64 {
		t.Fatalf("Capabilities().max_request_bytes is %d after a re-Hello advertising 64: the "+
			"advertisement is replaced by every Hello, and a re-Hello is the ONLY path on which the "+
			"advertised bounds move — nothing emits CapabilityChange and nothing emits "+
			"MessageMessageServerPush", got)
	}
	if got := transport.Capabilities().GetMaxRecordsPerSubmit(); got != 0 {
		t.Fatalf("Capabilities().max_records_per_submit is %d after a re-Hello that advertised none: the "+
			"advertisement is REPLACED and not merged", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 3 — Capabilities bounds every later request, locally.
// ─────────────────────────────────────────────────────────────────────────────

func TestAnAdvertisedBoundIsEnforcedLocallyAndAReHelloReplacesIt(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("n1", &protocol.Capabilities{MaxRequestBytes: 512})
	fake, transport := newHelloTransport(t, answers)
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the Hello failed: %v", err)
	}

	big := &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 1024)}
	before := len(fake.codePoints())
	_, err := transport.Call(context.Background(), big)
	if !errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a request past the advertised max_request_bytes was refused with %v, want "+
			"errMessageTransportOverCapability — §4.3.1 makes Capabilities the server's whole "+
			"advertised contract, and a client that discovers a bound by being refused has spent a "+
			"round trip to read a field it already had", err)
	}
	for _, named := range []string{"max_request_bytes", "512"} {
		if !strings.Contains(err.Error(), named) {
			t.Fatalf("the refusal %q does not name %q: the refusal Property 3 owes names the capability "+
				"AND both numbers", err, named)
		}
	}
	if !strings.Contains(err.Error(), "1029") && !strings.Contains(err.Error(), "103") {
		t.Logf("the refusal is %q", err)
	}
	if grew := len(fake.codePoints()) - before; grew != 0 {
		t.Fatalf("%d frame(s) reached the client for a request the advertisement already refused", grew)
	}

	// under the bound, it goes out
	small := &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)}
	before = len(fake.codePoints())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = transport.Call(ctx, small)
	cancel()
	if errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a request comfortably under the advertised bound was refused as over it: %v", err)
	}
	if grew := len(fake.codePoints()) - before; grew == 0 {
		t.Fatal("a request under the advertised bound reached no wire at all")
	}

	// plan mutation 6's shape: a re-Hello advertising a SMALLER bound. It is the
	// only path on which the advertised bounds actually move, and it is the path
	// a real connect.Client takes on every reconnect.
	answers.advertise("n2", &protocol.Capabilities{MaxRequestBytes: 8})
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the re-Hello failed: %v", err)
	}
	before = len(fake.codePoints())
	_, err = transport.Call(context.Background(), small)
	if !errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a request that the PREVIOUS connection's advertisement allowed was accepted after a "+
			"re-Hello advertising 8 bytes: %v. The cached advertisement is stale and is being enforced", err)
	}
	if !strings.Contains(err.Error(), " 8 ") {
		t.Fatalf("the refusal %q does not name the CURRENT bound of 8", err)
	}
	if grew := len(fake.codePoints()) - before; grew != 0 {
		t.Fatalf("%d frame(s) reached the client under a stale bound", grew)
	}
}

func TestAnUnadvertisedBoundIsNotABoundOfZero(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("n1", &protocol.Capabilities{})
	fake, transport := newHelloTransport(t, answers)
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the Hello failed: %v", err)
	}

	before := len(fake.codePoints())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := transport.Call(ctx, &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)})
	cancel()
	if errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a server that advertised NOTHING refused every request: %v. §4.3.1's fields are proto3 "+
			"scalars, so an absent Capabilities is indistinguishable from one advertising zero "+
			"everywhere, and a zero that refused everything would make such a server unusable", err)
	}
	if grew := len(fake.codePoints()) - before; grew == 0 {
		t.Fatal("nothing reached the wire under an empty advertisement")
	}
}

// ── the bounds this binding enforces, and the ones it does not ───────────────
//
// GATE CLASS: the fields of §4.3.1's `Capabilities`, read off the compiled
//
//	descriptor rather than listed.
//
// GATE SCOPE: `messageCapabilityBounds`, which is the production table `Call`
//
//	actually enforces — so a bound that is in the table is in this
//	gate, and a bound that is enforced by a line of code somewhere
//	else is not in the table and therefore not claimed here.
//
// The complement is the point. §4.3.1 advertises twenty-five fields and this
// binding enforces ONE; a reader who is told only "Capabilities is enforced"
// would have to infer the other twenty-four from an absence of code. Each one is
// printed by name, and every member of the table is shown to actually refuse
// something — a table entry that refuses nothing is a bound in a comment.
func TestTheAdvertisedBoundsThisBindingEnforcesAndTheOnesItDoesNot(t *testing.T) {
	descriptor := (&protocol.Capabilities{}).ProtoReflect().Descriptor()
	enforced := map[string]bool{}
	for _, bound := range messageCapabilityBounds {
		field := descriptor.Fields().ByName(protoreflect.Name(bound.field))
		if field == nil {
			t.Fatalf("messageCapabilityBounds names Capabilities.%s and protocol.Capabilities declares no "+
				"such field: a bound named by a field that does not exist is a bound that is not enforced",
				bound.field)
		}
		enforced[bound.field] = true
	}
	notEnforced := []string{}
	for index := 0; index < descriptor.Fields().Len(); index += 1 {
		name := string(descriptor.Fields().Get(index).Name())
		if !enforced[name] {
			notEnforced = append(notEnforced, name)
		}
	}
	t.Logf("GATE CLASS: %d field(s) of §4.3.1 Capabilities, read off the compiled descriptor",
		descriptor.Fields().Len())
	t.Logf("  ENFORCED locally by this binding: %d %v", len(enforced), sortedKeys(enforced))
	t.Logf("  COMPLEMENT — advertised and NOT enforced here, by name: %d %v", len(notEnforced), notEnforced)
	if len(enforced) == 0 {
		t.Fatal("this binding enforces NO advertised bound, so Property 3 is a comment")
	}
	if len(notEnforced) == 0 {
		t.Fatal("the complement is EMPTY: this binding would be enforcing every field Capabilities has, " +
			"including the string ones, which is a mistake in this gate")
	}
	if len(enforced)+len(notEnforced) != descriptor.Fields().Len() {
		t.Fatalf("%d enforced + %d not enforced is not the %d fields Capabilities declares: "+
			"the partition does not close", len(enforced), len(notEnforced), descriptor.Fields().Len())
	}

	// every member of the table refuses something, driven through Call
	for _, bound := range messageCapabilityBounds {
		t.Run(bound.field, func(t *testing.T) {
			capabilities := &protocol.Capabilities{}
			field := descriptor.Fields().ByName(protoreflect.Name(bound.field))
			switch field.Kind() {
			case protoreflect.Uint32Kind:
				capabilities.ProtoReflect().Set(field, protoreflect.ValueOfUint32(1))
			case protoreflect.Uint64Kind:
				capabilities.ProtoReflect().Set(field, protoreflect.ValueOfUint64(1))
			default:
				t.Fatalf("Capabilities.%s is a %s, which messageCapabilityValue does not read as a number",
					bound.field, field.Kind())
			}

			answers := &messageHelloAnswers{}
			answers.advertise("n", capabilities)
			fake, transport := newHelloTransport(t, answers)
			if _, _, err := transport.Hello(context.Background(), 1); err != nil {
				t.Fatalf("the Hello failed: %v", err)
			}
			before := len(fake.codePoints())
			_, err := transport.Call(context.Background(),
				&protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 64)})
			if !errors.Is(err, errMessageTransportOverCapability) {
				t.Fatalf("Capabilities.%s advertised as 1 refused nothing: %v. A table entry that cannot "+
					"be made to refuse is a bound in a comment", bound.field, err)
			}
			if !strings.Contains(err.Error(), bound.field) {
				t.Fatalf("the refusal %q does not name %s", err, bound.field)
			}
			if grew := len(fake.codePoints()) - before; grew != 0 {
				t.Fatalf("%d frame(s) reached the client for a request the bound refused", grew)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 4 — the first request of a connection uses a compiled-in budget.
// ─────────────────────────────────────────────────────────────────────────────
//
// §4.3.1's ordering hole, which a plan can either state or discover: Hello must
// be sent before `Capabilities` exists, so the first request of a connection has
// no advertised budget to cut itself to. The compiled-in value is Task 6's
// constant and must be the SAME one — plan mutation 7 is a Hello cut at a
// different budget, and it has to fail here AND at Task 6 Property 3.
func TestTheFirstRequestOfAConnectionUsesTheCompiledInPartBudget(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("n", &protocol.Capabilities{})
	fake, transport := newHelloTransport(t, answers)

	if capabilities := transport.Capabilities(); capabilities != nil {
		t.Fatalf("Capabilities() is %v before the first Hello: this test is about the state in which "+
			"there IS no advertised budget", capabilities)
	}

	// a Hello large enough to need §4.6. The version list is what a Hello has
	// to be large with — it is the only repeated field HelloRequest offers a
	// caller of Hello(ctx, versions...).
	versions := []uint32{}
	for version := uint32(1); version <= 4000; version += 1 {
		versions = append(versions, version)
	}
	if _, _, err := transport.Hello(context.Background(), versions...); err != nil {
		t.Fatalf("the Hello failed: %v", err)
	}

	fragments := fake.cutFragments()
	if len(fragments) < 2 {
		t.Fatalf("this Hello was cut into %d fragment(s), so it never crossed the part size and this "+
			"test is measuring nothing. Raise the version count.", len(fragments))
	}
	for index, fragment := range fragments {
		if index == len(fragments)-1 {
			break
		}
		if length := len(fragment.GetPart()); length != messageFragmentPartBytes {
			t.Fatalf("fragment %d of the FIRST request of this connection carries %d bytes, want the "+
				"compiled-in part size of %d. Hello is sent before Capabilities exists, so the budget it "+
				"cuts to is the compiled-in one — and it must be the same constant Task 6 declares, not "+
				"a second one chosen for Hello",
				index, length, messageFragmentPartBytes)
		}
	}
	if last := len(fragments[len(fragments)-1].GetPart()); last == 0 || messageFragmentPartBytes < last {
		t.Fatalf("the final fragment carries %d bytes, want between 1 and %d",
			last, messageFragmentPartBytes)
	}

	// and it was cut with nothing advertised in hand, which is the property
	if transport.NonceEpoch() != 1 {
		t.Fatalf("NonceEpoch() is %d after the Hello, want 1", transport.NonceEpoch())
	}
	t.Logf("the first request of this connection was cut into %d fragments of %d bytes with "+
		"Capabilities nil, and the transport advertises %v now",
		len(fragments), messageFragmentPartBytes, transport.Capabilities() != nil)
}

// REVIEW FINDING C — the other half of Property 4's ordering hole.
//
// The plan names one half: the first request of a connection has no advertised
// budget to CUT itself to, so it uses the compiled-in part size (the test
// above). The half the plan does not name is the same ordering, one step later:
// that request has no advertised bound to be MEASURED against either, because
// the `Capabilities` cache belongs to the connection that is gone. Enforcing it
// against the Hello that opens the NEXT connection is a refusal read off an
// advertisement that has expired — and with a small enough previous bound it is
// a WEDGE rather than a refusal, because the only thing that replaces the cache
// is a Hello, and the Hello is what is being refused.
//
// Both directions are driven, because an exemption that is too wide is the same
// defect facing the other way: after the re-Hello, a request that is NOT the
// nonce-issuing arm is still measured against the bound that Hello fetched.
func TestAReHelloIsBoundedByNoAdvertisementOfTheConnectionItReplaces(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("n1", &protocol.Capabilities{MaxRequestBytes: 4})
	fake, transport := newHelloTransport(t, answers)
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the first Hello failed: %v", err)
	}

	// the previous connection's bound is real, and it is small enough that
	// everything including a Hello is over it
	_, err := transport.Call(context.Background(), &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)})
	if !errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a request past an advertised max_request_bytes of 4 was refused with %v, want "+
			"errMessageTransportOverCapability: this test is measuring nothing if the bound is not live", err)
	}

	answers.advertise("n2", &protocol.Capabilities{MaxRequestBytes: 100000})
	before := len(fake.codePoints())
	reason, hello, err := transport.Hello(context.Background(), 1)
	if err != nil {
		t.Fatalf("the re-Hello was refused with %v. It was measured against the PREVIOUS connection's "+
			"max_request_bytes of 4, and that advertisement belongs to a connection that is gone — "+
			"§4.3.1 makes every Capabilities value config, so 4 is as legal as 4000000. This is not a "+
			"refused request, it is a WEDGE: self.capabilities is replaced only by a Hello that "+
			"completed, and no Hello can complete", err)
	}
	if reason != protocol.Reason_REASON_OK || hello == nil {
		t.Fatalf("the re-Hello answered reason %v hello %v", reason, hello)
	}
	if grew := len(fake.codePoints()) - before; grew == 0 {
		t.Fatal("the re-Hello reached no wire at all, so it was refused somewhere this test did not look")
	}
	if got := transport.Capabilities().GetMaxRequestBytes(); got != 100000 {
		t.Fatalf("Capabilities().max_request_bytes is %d after the re-Hello, want the advertisement the "+
			"re-Hello fetched", got)
	}
	if epoch := transport.NonceEpoch(); epoch != 2 {
		t.Fatalf("NonceEpoch() is %d after two Hellos, want 2", epoch)
	}

	// ── the other direction: the exemption is ONE arm, not "everything after a
	//    Hello". A re-Hello that advertises a smaller bound still binds
	//    everything that is not the arm which issues the nonce ────────────────
	answers.advertise("n3", &protocol.Capabilities{MaxRequestBytes: 8})
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the third Hello failed: %v", err)
	}
	before = len(fake.codePoints())
	_, err = transport.Call(context.Background(), &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)})
	if !errors.Is(err, errMessageTransportOverCapability) {
		t.Fatalf("a SubmitRequest past the CURRENT advertisement of 8 was answered with %v: the "+
			"exemption is the arm that issues this connection's nonce and nothing else, and an "+
			"exemption that reaches further has turned Property 3 off", err)
	}
	if grew := len(fake.codePoints()) - before; grew != 0 {
		t.Fatalf("%d frame(s) reached the client for a request the current advertisement refused", grew)
	}
}

// REVIEW FINDING F — a Hello that issued no `server_nonce` opens nothing.
//
// Property 1's basis is that `write_auth` and `req_auth` are MACs over this
// connection's `server_nonce`. The gate was keyed on the EPOCH, which
// [messageTransport.adopt] moves after any REASON_OK Hello carrying a Hello arm
// — including one whose `server_nonce` is empty — so a Hello that issued no
// nonce opened the gate and a Submit went on the wire to be MAC'd over nothing.
//
// The fake is what makes this drivable and is also the boundary: nothing here
// says a real server can answer REASON_OK with an empty nonce. What it says is
// that if one did, this side would not treat it as a connection.
func TestAHelloThatIssuedNoNonceIsNotAConnectionEither(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("", &protocol.Capabilities{})
	fake, transport := newHelloTransport(t, answers)

	reason, hello, err := transport.Hello(context.Background(), 1)
	if err != nil || reason != protocol.Reason_REASON_OK || hello == nil {
		t.Fatalf("the Hello failed: reason %v hello %v err %v — this test is about a Hello that "+
			"SUCCEEDS and issues nothing", reason, hello, err)
	}
	if nonce := transport.Nonce(); len(nonce) != 0 {
		t.Fatalf("Nonce() is %q, want empty: this test is measuring nothing otherwise", nonce)
	}

	before := len(fake.codePoints())
	_, err = transport.Call(context.Background(), &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)})
	if !errors.Is(err, errMessageTransportNoHello) {
		t.Fatalf("a SubmitRequest after a Hello that issued NO server_nonce was answered with %v, want "+
			"errMessageTransportNoHello. Task 10 MACs over Nonce(), which is empty here, and a MAC over "+
			"nothing is not an authenticator", err)
	}
	if grew := len(fake.codePoints()) - before; grew != 0 {
		t.Fatalf("%d frame(s) reached the client after a Hello that issued no nonce: the refusal "+
			"Property 1 owes is raised LOCALLY", grew)
	}

	// satisfiable half: the same body, after a Hello that DID issue one
	answers.advertise("a-real-nonce", &protocol.Capabilities{})
	if _, _, err := transport.Hello(context.Background(), 1); err != nil {
		t.Fatalf("the second Hello failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = transport.Call(ctx, &protocol.SubmitRequest{GroupId: bytes.Repeat([]byte{0x11}, 16)})
	cancel()
	if errors.Is(err, errMessageTransportNoHello) {
		t.Fatalf("a SubmitRequest was refused as preceding Hello after a Hello that issued %q: %v",
			transport.Nonce(), err)
	}
}

// A Hello the server answers REASON_OK on another arm changes nothing about
// this connection: no nonce, no advertisement, no epoch.
func TestAHelloAnsweredOnAnotherArmIsNotAConnection(t *testing.T) {
	answers := &messageHelloAnswers{}
	answers.advertise("never-issued", &protocol.Capabilities{MaxRequestBytes: 99})
	answers.wrongArm = true
	_, transport := newHelloTransport(t, answers)

	_, hello, err := transport.Hello(context.Background(), 1)
	if !errors.Is(err, errMessageTransportWrongArm) {
		t.Fatalf("a REASON_OK carried on the submit arm was accepted as a Hello, or refused with %v", err)
	}
	if hello != nil {
		t.Fatalf("a wrong-arm Hello returned a response: %v", hello)
	}
	if epoch := transport.NonceEpoch(); epoch != 0 {
		t.Fatalf("NonceEpoch() is %d after a Hello that issued nothing, want 0", epoch)
	}
	if nonce := transport.Nonce(); len(nonce) != 0 {
		t.Fatalf("Nonce() is %q after a Hello that issued nothing", nonce)
	}
	if capabilities := transport.Capabilities(); capabilities != nil {
		t.Fatalf("Capabilities() is %v after a Hello that advertised nothing this connection can use",
			capabilities)
	}
}
