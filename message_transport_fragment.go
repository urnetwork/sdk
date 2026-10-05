//go:build !sdk_mobile_bind

package sdk

// §4.6 fragmentation, both directions: the cut this binding applies to a request
// too large for one frame, and the reassembly it applies to a response the
// server cut.
//
// ── WHERE THE NUMBER COMES FROM, AND WHY `sdk` HOLDS ITS OWN COPY ────────────
//
// [messageFragmentPartBytes] below is the SECOND copy of §4.6's bound in this
// workspace and the THIRD fragmenter. That is a real cost and it is filed as
// S2-9 rather than absorbed. The query is published beside the claim, and it was
// re-run at connect 71d2482 rather than taken from the plan's measurement at
// 33932e0:
//
//	grep -rn MessageServerFragment --include=*.go connect/
//
// returns hits in exactly three files, two of which carry `// Code generated`
// (protocol/frame.pb.go, protocol/message.pb.go) — leaving exactly ONE
// hand-written hit, protocol/message_wire_test.go:71, a name-to-number mapping
// entry in a test. `grep -rn 'PartBytes|FragmentPart' --include=*.go connect/`
// returns nothing at all. connect holds no cut, no reassembler and no part-size
// constant, so there is nothing here to import and the justification has not
// expired.
//
// The other copy is `MaxFragmentPartBytes` in msgrepo/peer/frame.go — the
// SERVER module, which `sdk` cannot import: it is a different module, `go.mod`
// neither requires nor replaces it, and it would be an inversion of the
// dependency besides. The two copies are held together by nothing but this
// comment until one of them moves into `connect`.
//
// ── WHY THIS IS BUILT NOW, OFF THE CP3b PREFIX ───────────────────────────────
//
// One durable text message at size bucket 0–2 is far under 2048 octets, so the
// cut is never entered on the CP3b path. It is here because `CreateGroupRequest`
// carries a whole record and an `EpochAttachment` and is the first request that
// plausibly crosses the bound, and because a reassembler written later is a
// reassembler written under deadline.

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// §4.6's `part` size, and the ONE declaration of it in `sdk`.
//
// Spec B §4.6: "The sender chooses `part` size as min(peer_advertised_frame_budget,
// 2048) bytes and MUST NOT exceed the negotiated budget." The 2048 is the ceiling
// of that min. connect advertises no per-peer frame budget to a caller — the
// grep above returns nothing for `PartBytes` anywhere in it — so with nothing to
// be the smaller of, the ceiling is also the whole of the budget, and this
// binding has no configurable part size for a value to disagree with.
//
// That is also why `messageTransportConfig` carries no part size. A knob with no
// source of truth is a second home for a bound, and the bound's home is here.
//
// It is a bound in BOTH directions and that is deliberate: what this binding
// will not exceed when it cuts is what it will not accept when it joins, since
// a part larger than §4.6's ceiling is a sender that broke a MUST NOT. See
// [messageFragmentAborts].
//
// THE OTHER COPY: `MaxFragmentPartBytes` in msgrepo/peer/frame.go. S2-9.
const messageFragmentPartBytes = 2048

// The refusals §4.6 raises on this side of the wire.
var (
	// Property 2's typed abort. §4.6 "aborts the request rather than buffering
	// holes", and an abort is told to the waiter rather than left for its
	// timeout to discover: a caller that waits thirty seconds to learn what the
	// receive path knew immediately has been told the wrong thing about why.
	errMessageFragmentAborted = errors.New("message transport: a §4.6 reassembly was abandoned, so the response it was carrying will never be completed")
)

// One request being reassembled out of §4.6's fragments.
//
// `next` is the index this buffer will accept and there is nothing else here,
// because §4.6 is explicit that "out-of-order `index` aborts the request rather
// than buffering holes" — so there is no place in this struct to put a hole in.
type messageFragmentPartial struct {
	count uint32
	next  uint32
	bytes []byte
}

// What one abort rule decides on: the arriving fragment and whatever is already
// buffered under its `request_id`.
//
// A value rather than the transport itself, so that a rule cannot reach past
// what it is deciding about, and so that a test can build the state a rule fires
// on without building the history that would produce it.
type messageFragmentState struct {
	fragment *protocol.MessageServerFragment

	// nil when nothing is open for this request_id yet: this fragment would
	// open it.
	current *messageFragmentPartial
}

// The index this reassembly will accept. A reassembly that does not exist yet
// accepts index 0, which is what makes "a first fragment must be index 0" the
// same rule as "in order" rather than a second rule beside it.
func (self messageFragmentState) next() uint32 {
	if self.current == nil {
		return 0
	}
	return self.current.next
}

func (self messageFragmentState) opening() bool {
	return self.current == nil
}

// One of §4.6's abort conditions.
//
// They are VALUES because the class of them is what a test has to be right
// about, and a class typed out by hand has understated itself every time it has
// been tried on this project. A gate that iterates this table fails while any
// rule in it has no case of its own, and proves each case belongs to its own
// rule by taking that rule away and watching the refusal disappear — which is
// something a chain of `||` inside one function cannot be asked.
type messageFragmentAbort struct {
	name string

	// True when this rule refuses this fragment. False means "not this rule's
	// business", never "accept".
	aborts func(state messageFragmentState) bool
}

// Every way this binding abandons a §4.6 reassembly, in the order they are
// asked.
//
// Order is not load-bearing — every rule answers the same abort — but it is
// stable so that the reason a test reads back is the reason it planted.
var messageFragmentAborts = []messageFragmentAbort{
	{
		// a `count` of zero is the degenerate case of this rather than a rule
		// beside it: it names no fragments at all, and no index is below zero
		// of them
		name: "an index that is not below the fragment count",
		aborts: func(state messageFragmentState) bool {
			return state.fragment.GetCount() <= state.fragment.GetIndex()
		},
	},
	{
		// §4.6's MUST NOT, applied to what this binding ACCEPTS and not only to
		// what it sends. A part above the ceiling is a sender that exceeded the
		// budget, and buffering it would make this side the place the bound
		// stops being one. It is also the whole of the memory bound on a
		// reassembly: nothing here is capped by `max_response_bytes`, which
		// §4.3.1 advertises and this task does not read — see the boundary note
		// on [messageTransport.acceptFragment].
		name: "a part larger than §4.6's own ceiling",
		aborts: func(state messageFragmentState) bool {
			return messageFragmentPartBytes < len(state.fragment.GetPart())
		},
	},
	{
		// §4.6: "out-of-order `index` aborts the request rather than buffering
		// holes". Asked before the buffer exists, this is also "a first fragment
		// that is not index 0 is a request whose beginning is not coming", and
		// asked after it, it is also "a duplicate index is not the one expected"
		name: "an index that is not the one this reassembly is waiting for",
		aborts: func(state messageFragmentState) bool {
			return state.fragment.GetIndex() != state.next()
		},
	},
	{
		// the count is fixed by the fragment that opened the reassembly. A
		// sender that changes it mid-request is describing two different
		// responses under one request_id, and this buffer completes on the
		// number it was opened with
		name: "a fragment count that changed mid-reassembly",
		aborts: func(state messageFragmentState) bool {
			return !state.opening() && state.fragment.GetCount() != state.current.count
		},
	},
}

// ── the cut ──────────────────────────────────────────────────────────────────

// One request, in the §4.2 frames §4.6 carries it in.
//
// The whole request is ONE frame at the request code point when it fits in a
// part, and `count` fragment frames otherwise. §4.6 numbers them from zero and
// the receiver aborts on any index it was not waiting for, so they go on the
// wire in order and connect keeps them in it — "frames are received in order of
// send" is the guarantee transfer.go opens with.
//
// An exact multiple of the part size produces exactly `len/partBytes` frames and
// no empty final part: `count` is the ceiling of the division, and the ceiling
// of an exact multiple is the multiple. An off-by-one here is invisible to a
// round trip — an empty trailing part reassembles to the same bytes — which is
// why the count is asserted and not only the bytes.
func (self *messageTransport) fragments(request *protocol.MessageServerRequest) ([]*protocol.Frame, error) {
	body, err := connect.ProtoMarshal(request)
	if err != nil {
		return nil, err
	}
	if len(body) <= messageFragmentPartBytes {
		return []*protocol.Frame{{
			MessageType:  protocol.MessageType_MessageMessageServerRequest,
			MessageBytes: body,
		}}, nil
	}

	count := (len(body) + messageFragmentPartBytes - 1) / messageFragmentPartBytes
	frames := make([]*protocol.Frame, 0, count)
	for index := 0; index < count; index += 1 {
		end := min((index+1)*messageFragmentPartBytes, len(body))
		encoded, err := connect.ProtoMarshal(&protocol.MessageServerFragment{
			RequestId: request.GetRequestId(),
			Index:     uint32(index),
			Count:     uint32(count),
			Part:      body[index*messageFragmentPartBytes : end],
		})
		if err != nil {
			// free what was built before giving up, so a marshal failure
			// halfway through a large request does not leak the pool buffers of
			// the fragments before it
			messageFragmentReturn(frames)
			connect.MessagePoolReturn(body)
			return nil, err
		}
		frames = append(frames, &protocol.Frame{
			MessageType:  protocol.MessageType_MessageMessageServerFragment,
			MessageBytes: encoded,
		})
	}
	// the whole request now lives in the fragments, so the buffer it was
	// marshaled into goes back to the pool rather than to the collector
	connect.MessagePoolReturn(body)
	return frames, nil
}

// Give a frame's bytes back to the pool. MessagePoolReturn drops anything that
// did not come from one, so this is safe on a frame built any other way.
func messageFragmentReturn(frames []*protocol.Frame) {
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
}

// ── the join ─────────────────────────────────────────────────────────────────

// One inbound fragment, applied.
//
// Answers the assembled response bytes, whether the response is now complete,
// and the typed abort on any of [messageFragmentAborts]. Every abort frees the
// buffer before returning, which is §4.6's "frees the buffer immediately": a
// buffer freed one stage later is a buffer a sender gets to hold open by never
// sending the last fragment.
//
// Every rule is asked before anything is created, so a refusal on the fragment
// that would have opened a reassembly allocates nothing at all.
//
// A fragment whose `request_id` no waiter is waiting on opens NOTHING. See the
// first branch below for why the waiter set is the only reassembly bound this
// side has.
//
// It is called from inside connect's receive callback and holds the borrow rule:
// `fragment` is a message of ours that `proto.Unmarshal` decoded — which copies
// — and the part is appended into a buffer of our own. No frame and no frame
// byte reaches this function.
//
// BOUNDARY, stated rather than left to be discovered. The bounds on ONE
// reassembly here are §4.6's part ceiling and the `count` the opening fragment
// declared; the bound on how MANY are open is the size of the waiter set, which
// is how many Calls are in flight. §4.3.1's `max_response_bytes` is advertised
// by the server and is NOT read — Task 7 brings Capabilities and applies them to
// REQUESTS, which is what §5.1 check 1 is about — so a server that declared a
// count of four billion is bounded only by the bytes it actually sends, which is
// the bytes this binding is already receiving. Filed rather than absorbed.
func (self *messageTransport) acceptFragment(fragment *protocol.MessageServerFragment) ([]byte, bool, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	requestId := fragment.GetRequestId()
	if _, expected := self.waiting[requestId]; !expected {
		// §4.6's reassembly is per (source client_id, request_id), and this
		// binding opens one only for a request it MADE. A fragment carrying a
		// request_id no waiter is waiting on has nowhere to go and nobody to
		// tell, and BUFFERING it is how an unbounded reassembly map is built
		// out of one number the sender chooses — the memory-exhaustion vector
		// §4.6 is written against, arriving on the side of the wire where §4.6
		// states no cap at all. §4.6 gives the server sixteen per client and
		// thirty seconds; it gives a client nothing, and the waiter set is the
		// bound this side already has.
		//
		// Counted rather than dropped in silence, for [messageTransport.deliver]'s
		// reason: "nothing arrived" and "something arrived for nobody" have to
		// be two readings. Whatever this request_id held goes with it — a
		// waiter that timed out mid-reassembly took its buffer through
		// [messageTransport.forget] already, and this is the arm that catches
		// the buffer no Call ever owned.
		delete(self.partial, requestId)
		self.counts.Unmatched += 1
		return nil, false, nil
	}
	current, open := self.partial[requestId]
	state := messageFragmentState{fragment: fragment}
	if open {
		state.current = current
	}
	for _, rule := range messageFragmentAborts {
		if !rule.aborts(state) {
			continue
		}
		// whatever this request_id holds goes now, and a request_id that holds
		// nothing is a drop of nothing
		delete(self.partial, requestId)
		self.counts.Aborted += 1
		return nil, false, fmt.Errorf("%w: request %d, fragment %d of %d: %s",
			errMessageFragmentAborted, requestId, fragment.GetIndex(), fragment.GetCount(), rule.name)
	}

	if !open {
		current = &messageFragmentPartial{count: fragment.GetCount()}
		self.partial[requestId] = current
	}
	current.bytes = append(current.bytes, fragment.GetPart()...)
	current.next += 1
	if current.next < current.count {
		return nil, false, nil
	}
	delete(self.partial, requestId)
	self.counts.Reassembled += 1
	return current.bytes, true, nil
}

// The waiter for this request, told that its response is not coming.
//
// The waiter is found and removed under one hold of the lock, so the channel
// this sends on has no other sender and a free slot in its buffer of one —
// [messageTransport.deliver]'s argument, and the same reason: this runs inside
// connect's receive callback and a blocked callback backpressures every other
// client's frames.
func (self *messageTransport) abort(requestId uint64, err error) {
	self.mutex.Lock()
	waiter, found := self.waiting[requestId]
	if found {
		delete(self.waiting, requestId)
	}
	self.mutex.Unlock()
	if found {
		waiter <- messageTransportAnswer{err: err}
	}
}

func (self *messageTransport) countFragmentFrame() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.counts.FragmentFrames += 1
}
