//go:build !sdk_mobile_bind

package sdk

// §4.3.1: Hello, the per-connection `server_nonce`, and the `Capabilities`
// cache.
//
// ── S2-2 BECOMES CONCRETE HERE, AND THIS FILE TAKES A POSITION RATHER THAN
//    FIXING IT ───────────────────────────────────────────────────────────────
//
// The server draws a fresh 32-octet nonce per connection and replaces it
// UNCONDITIONALLY at every Hello — msgrepo/peer/hello.go calls
// `Connections.Open`, which replaces, and its own comment is emphatic that this
// ends the previous connection for that client_id. Every §5.4 `write_auth` and
// every §4.3.8 `req_auth` is a MAC over that nonce.
//
// `NewGroupSession` copies the nonce ONCE, at construction, and `GroupSession`
// has no setter. So after any reconnect, every record a session seals carries a
// `write_auth` over a nonce the server no longer holds, and every submit of it
// is refused. CP3c already proves the server does exactly that.
//
// THE POSITION: this transport exposes [messageTransport.NonceEpoch], and the
// send path — Task 10, not this task — refuses to submit a record sealed under
// a superseded nonce rather than putting it on the wire to be refused there.
// REJECTED: tearing down and rebuilding the `GroupSession` inside s2 on every
// reconnect. Its cost has never been priced — a rebuild drops every sender and
// receiver ratchet, re-walks each ladder from the store's high water, and is
// refused outright above `maxLadderWalk`, and `NewSenderRatchet` refuses that
// resume for EVERY class of that sender because the counter they share is what
// crossed the bound — and it needs `pq_secret` and `groupHandleKeyEpoch0` back
// in hand. S2-2 stays open and needs a `connect` change nobody owns.
//
// ── THE ONE HAZARD THIS FILE SHIPS, NAMED ────────────────────────────────────
//
// [messageTransport.Nonce] and [messageTransport.NonceEpoch] are two reads under
// two acquisitions of one lock, so a Hello landing between them gives a caller a
// nonce from one epoch and a number from another. Task 10's refusal is not
// exposed to it — it records the epoch AT SEAL TIME, which is one read, and
// compares that recorded number at submit — but a caller that reads the pair
// expecting them to agree is reading two things. Stated rather than papered
// over; the fix, if one is wanted, is a single accessor answering both.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	// Property 1's refusal, raised LOCALLY. A request whose authenticator is a
	// MAC over this connection's `server_nonce` cannot be built before the
	// connection has one, and putting it on the wire to be refused there spends
	// a round trip to be told something this side already knew.
	errMessageTransportNoHello = errors.New("message transport: every §5.4 write_auth and every §4.3.8 req_auth is a MAC over this connection's server_nonce, and no Hello has issued one")

	// Property 3's refusal. §4.3.1 makes Capabilities the server's whole
	// advertised contract, and a client that discovers a bound by being refused
	// has spent a round trip to read a field it already had.
	errMessageTransportOverCapability = errors.New("message transport: this request exceeds a bound the server advertised in Capabilities")

	// The server answered REASON_OK on an arm that is not the one the request
	// travelled in. Not a `protocol.Reason`: the server said OK, and this is
	// this side's refusal to read an answer to a question it did not ask.
	errMessageTransportWrongArm = errors.New("message transport: the server answered REASON_OK carrying a body arm that is not the one this request travelled in")

	// The advertisement named a field this build cannot find on
	// `protocol.Capabilities`. It is a build error wearing a run-time coat: the
	// bound is named by its DESCRIPTOR name so a field renamed upstream fails
	// here rather than quietly stopping being enforced.
	errMessageTransportNoCapabilityField = errors.New("message transport: protocol.Capabilities declares no such field, so a bound this binding claims to enforce cannot be read")
)

// One bound this binding enforces locally, out of everything §4.3.1 advertises.
//
// A TABLE rather than a chain of ifs, and named by the field's DESCRIPTOR name
// rather than by its Go accessor, for two reasons. A Capabilities field renamed
// upstream becomes a run-time refusal here instead of a bound that silently
// stops being enforced. And the set of bounds this binding enforces becomes a
// value a gate can read, so the COMPLEMENT — everything §4.3.1 advertises and
// this binding does not enforce — is printable rather than a thing a reader has
// to infer from the absence of code.
type messageCapabilityBound struct {
	// the field of `protocol.Capabilities`, by its descriptor name
	field string

	// what this field bounds, in words, for the refusal
	what string

	// the measurement of the request that this field bounds
	measure func(request *protocol.MessageServerRequest) uint64
}

// The bounds this binding enforces. ONE, and the complement is 24 fields wide —
// see TestTheAdvertisedBoundsThisBindingEnforcesAndTheOnesItDoesNot, which
// prints every one of them by name.
//
// §5.1 check 1 is `max_request_bytes` over the reassembled request, which is
// the one bound that applies to EVERY request rather than to one operation's
// shape. `max_records_per_submit` and `max_submit_bytes` bound a Submit, and
// Submit is Task 10's; enforcing them here would be this file deciding the send
// path's refusals for it.
var messageCapabilityBounds = []messageCapabilityBound{
	{
		field:   "max_request_bytes",
		what:    "the marshaled request",
		measure: func(request *protocol.MessageServerRequest) uint64 { return uint64(proto.Size(request)) },
	},
}

// ── §4.3.1, as a client performs it ──────────────────────────────────────────

// Negotiate a version, take this connection's nonce, and cache what the server
// advertised.
//
// Property 4's ordering hole, which §4.3.1 has and a plan can either state or
// discover: Hello must be sent BEFORE `Capabilities` exists, so the first
// request of a connection has no advertised budget to cut itself to and uses
// the compiled-in one. That is Task 6's [messageFragmentPartBytes] and it is the
// same constant — [messageTransport.fragments] is the only cut in the package
// and it takes no budget, so there is nothing here that could choose a different
// one.
func (self *messageTransport) Hello(ctx context.Context, versions ...uint32) (protocol.Reason, *protocol.HelloResponse, error) {
	if len(versions) == 0 {
		versions = []uint32{self.protocolVersion}
	}
	response, err := self.Call(ctx, &protocol.HelloRequest{SupportedVersions: versions})
	if err != nil {
		return protocol.Reason_REASON_INTERNAL, nil, err
	}
	if response.GetReason() != protocol.Reason_REASON_OK {
		// §4.3.1 negotiates before it issues: a refused Hello leaves this
		// connection with no nonce, which is what it had before
		return response.GetReason(), nil, nil
	}
	hello := response.GetHello()
	if hello == nil {
		return response.GetReason(), nil, errMessageTransportWrongArm
	}
	self.adopt(hello)
	return response.GetReason(), hello, nil
}

// This connection's state, replaced UNCONDITIONALLY.
//
// Unconditional is the whole of it. The server replaces its own nonce at every
// Hello without asking whether it had one, so a client that kept the one it had
// would be MAC'ing against a nonce nothing holds; and a client that kept the
// Capabilities it had would be enforcing bounds the server has stopped
// advertising, which is the only path on which the advertised bounds actually
// move (`CapabilityChange` is emitted by nothing — measured 2026-09-12,
// `grep -rn 'CapabilityChange' --include=*.go` over msgrepo returns nothing and
// so does `grep -rn 'MessageMessageServerPush'`, so §4.3's push code point has
// no emitter and a re-Hello is the only path there is).
func (self *messageTransport) adopt(hello *protocol.HelloResponse) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.nonce = append([]byte(nil), hello.GetServerNonce()...)
	if advertised := hello.GetCapabilities(); advertised != nil {
		self.capabilities, _ = proto.Clone(advertised).(*protocol.Capabilities)
	} else {
		self.capabilities = nil
	}
	self.nonceEpoch += 1
}

// The `server_nonce` this connection was issued, or nil.
//
// A COPY. It is the input to every MAC a caller computes, and a caller that
// could write through it could make a whole session seal against something the
// server never issued without any line of that caller's code saying so.
func (self *messageTransport) Nonce() []byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]byte(nil), self.nonce...)
}

// How many Hellos this transport has completed. Zero before the first.
//
// This is S2-2's whole handle. A caller that sealed a record while this said n
// can tell, at n+1, that the nonce it sealed against is one the server no longer
// holds — and Task 10 refuses such a record rather than submitting it to be
// refused on the wire. See this file's header for why the session rebuild that
// would actually recover is not in this plan.
//
// IT COUNTS HELLOS, NOT CONNECTIONS, and the difference is load-bearing for the
// caller that reads it. The counter moves in [messageTransport.adopt] and
// `adopt` is reached from one place, a Hello that answered REASON_OK on the
// Hello arm. Nothing in `sdk` observes a `connect` reconnect — nothing in `sdk`
// constructs the `connect.Client` at all, which is S2-7 — so a connection that
// is replaced underneath this binding WITHOUT a Hello through it leaves a
// superseded nonce readable here at an unchanged number, and a refusal built on
// comparing this number sees nothing to refuse. The two coincide only while
// every connection change is followed by a Hello through this transport, and
// this binding cannot make that true by itself. Stated here because it is the
// caller of this accessor who would otherwise assume otherwise.
func (self *messageTransport) NonceEpoch() uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.nonceEpoch
}

// §4.3.1's advertisement from the last Hello, or nil. A CLONE, for
// [messageTransport.Nonce]'s reason.
func (self *messageTransport) Capabilities() *protocol.Capabilities {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.capabilities == nil {
		return nil
	}
	clone, _ := proto.Clone(self.capabilities).(*protocol.Capabilities)
	return clone
}

// ── the two local refusals Call owes ─────────────────────────────────────────

// Does this connection HAVE a nonce?
//
// REVIEW FINDING F, and it is the difference between the epoch MOVING and the
// connection having a nonce. [messageTransport.refuseBeforeHello] opened on
// `NonceEpoch() != 0`, and [messageTransport.adopt] moves the epoch after any
// REASON_OK Hello carrying a Hello arm — including one whose `server_nonce` is
// empty. An empty nonce is not a nonce: a `write_auth` over it is a MAC over
// nothing, and the gate that is supposed to stop that request opened for it.
//
// WHY THE EPOCH IS NOT ALSO CHECKED, stated because the obvious spelling is
// `0 < epoch && 0 < len(nonce)`: the nonce is written in exactly one place and
// that place moves the epoch in the same critical section, so a non-empty nonce
// already implies an epoch that moved. The extra clause defends nothing, and a
// clause that defends nothing was DELETED and the suite run to find that out.
//
// It is also one read under ONE acquisition of the lock rather than two under
// two — the hazard this file's header names — which is why it is a method here
// rather than two accessor calls at the call site.
func (self *messageTransport) hasConnectionNonce() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return 0 < len(self.nonce)
}

// Property 1: no request that requires an authenticator precedes Hello.
//
// THE CLASS IS DERIVED FROM THE DESCRIPTOR, not listed. Hello is the one
// operation that carries no authenticator, and it is identified by what its
// RESPONSE carries rather than by its name: the arm of `MessageServerResponse.body`
// whose message declares `server_nonce` is the arm that ISSUES the nonce, and
// §4.2's invariant that a response arm and its request arm share a number and a
// name is what carries that back to the request side. Everything else in the
// request oneof needs the nonce.
//
// The over-approximation is deliberate and is the safe direction. Two request
// arms carry no authenticator this binding could point at — `UnsubscribeRequest`
// has no field 15 at all, and `RecoveryFetchRequest`'s `proof` is an Ed25519
// signature over LP(server_nonce) at field 2 that no descriptor-level rule can
// see — and refusing both before Hello is correct anyway: §5.1 check 2 resolves
// the connection the server opened at Hello, so a request that precedes Hello
// has no connection to be resolved against.
//
// The gate is [messageTransport.hasConnectionNonce] and not the epoch: review
// finding F is that a Hello which issued an EMPTY server_nonce moved the epoch
// and opened this.
func (self *messageTransport) refuseBeforeHello(body proto.Message) error {
	if self.hasConnectionNonce() {
		return nil
	}
	exempt, err := messageTransportNonceIssuingArm()
	if err != nil {
		return err
	}
	if body != nil && body.ProtoReflect().Descriptor().FullName() == exempt {
		return nil
	}
	name := protoreflect.FullName("")
	if body != nil {
		name = body.ProtoReflect().Descriptor().FullName()
	}
	return fmt.Errorf("%w: %s would have to be authenticated and %s is the only request that issues one",
		errMessageTransportNoHello, name, exempt)
}

// The request-body arm that issues `server_nonce`, read off the compiled
// descriptors.
//
// Found from the RESPONSE side: exactly one arm of `MessageServerResponse.body`
// has a message that declares a field named `server_nonce`, and §4.2's
// invariant — a response arm and its request arm share a number and a name —
// names the request arm. Both halves are checked, so a build where the numbers
// and the names disagree is a refusal here rather than a wrong exemption.
func messageTransportNonceIssuingArm() (protoreflect.FullName, error) {
	response := (&protocol.MessageServerResponse{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	request := (&protocol.MessageServerRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	if response == nil || request == nil {
		return "", errMessageTransportNoArm
	}
	found := []protoreflect.FieldDescriptor{}
	for index := 0; index < response.Fields().Len(); index += 1 {
		field := response.Fields().Get(index)
		if field.Kind() != protoreflect.MessageKind {
			continue
		}
		if field.Message().Fields().ByName("server_nonce") != nil {
			found = append(found, field)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: %d response arm(s) declare server_nonce, so which request issues the "+
			"connection's nonce is not decidable from the descriptor", errMessageTransportNoArm, len(found))
	}
	issuing := found[0]
	byName := request.Fields().ByName(issuing.Name())
	byNumber := request.Fields().ByNumber(issuing.Number())
	if byName == nil || byNumber == nil || byName.Number() != byNumber.Number() {
		return "", fmt.Errorf("%w: the response arm %s (%d) has no request arm sharing both its name and "+
			"its number, so §4.2's pairing does not carry the exemption across",
			errMessageTransportNoArm, issuing.Name(), issuing.Number())
	}
	if byName.Kind() != protoreflect.MessageKind {
		return "", errMessageTransportNoArm
	}
	return byName.Message().FullName(), nil
}

// Property 3: a request exceeding an advertised bound is refused LOCALLY, with
// the capability named and both numbers in the refusal.
//
// A bound of zero is "not advertised" and not "nothing is allowed": §4.3.1's
// fields are proto3 scalars and an absent Capabilities is indistinguishable
// from one advertising zero everywhere, so a zero that refused every request
// would make a server that sent no advertisement unusable.
func (self *messageTransport) refuseOverCapability(request *protocol.MessageServerRequest) error {
	capabilities := self.Capabilities()
	if capabilities == nil {
		// no Hello has completed, or the server advertised nothing. Property 4:
		// this is the state the FIRST request of a connection is in, by
		// construction, and it is why the part budget is compiled in
		return nil
	}

	// REVIEW FINDING C — the OTHER half of Property 4's ordering hole, which the
	// part budget closed and this did not.
	//
	// `Capabilities` is per CONNECTION and is replaced only by a Hello that
	// completed. So a cached advertisement outlives the connection that made it,
	// and the first request of the NEXT connection — which is the Hello that
	// will fetch the next advertisement — was being measured against an
	// advertisement that is gone. With a small enough previous bound that is not
	// a refused request, it is a WEDGE: the cache is replaced only by a Hello,
	// and no Hello can get out to replace it.
	//
	// The exemption is not "Hello" by name. It is the arm that ISSUES this
	// connection's nonce, derived from the descriptors by the same function
	// Property 1's refusal uses — the one arm that by construction needs no
	// prior connection state, because it is what creates it.
	exempt, err := messageTransportNonceIssuingArm()
	if err != nil {
		return err
	}
	if oneof := request.ProtoReflect().Descriptor().Oneofs().ByName("body"); oneof != nil {
		field := request.ProtoReflect().WhichOneof(oneof)
		if field != nil && field.Kind() == protoreflect.MessageKind && field.Message().FullName() == exempt {
			return nil
		}
	}

	for _, bound := range messageCapabilityBounds {
		advertised, err := messageCapabilityValue(capabilities, bound.field)
		if err != nil {
			return err
		}
		if advertised == 0 {
			continue
		}
		measured := bound.measure(request)
		if measured <= advertised {
			continue
		}
		return fmt.Errorf("%w: Capabilities.%s is %d and %s is %d bytes",
			errMessageTransportOverCapability, bound.field, advertised, bound.what, measured)
	}
	return nil
}

// One advertised bound, read off the message by its descriptor name.
func messageCapabilityValue(capabilities *protocol.Capabilities, name string) (uint64, error) {
	descriptor := capabilities.ProtoReflect().Descriptor()
	field := descriptor.Fields().ByName(protoreflect.Name(name))
	if field == nil {
		return 0, fmt.Errorf("%w: %s", errMessageTransportNoCapabilityField, name)
	}
	value := capabilities.ProtoReflect().Get(field)
	switch field.Kind() {
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return uint64(value.Uint()), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return value.Uint(), nil
	case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		if value.Int() < 0 {
			return 0, nil
		}
		return uint64(value.Int()), nil
	}
	return 0, fmt.Errorf("%w: %s is a %s and not a number of anything",
		errMessageTransportNoCapabilityField, name, field.Kind())
}
