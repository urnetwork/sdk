//go:build !sdk_mobile_bind

package sdk

// Task 6's three properties: the cut, the join, and the one home for the part
// size.
//
// ── WHAT THIS FILE CANNOT SEE ────────────────────────────────────────────────
//
// The same boundary message_transport_test.go states, and one more that is this
// task's own: nothing here establishes that the SERVER cuts at the size this
// binding expects, or that it numbers from zero, or that connect delivers
// fragments in the order they were sent. §4.6 says "Fragments MUST be delivered
// in order by the underlying sequence" and transfer.go opens with "frames are
// received in order of send"; this file takes both as premises and drives the
// receive callback itself. msgrepo/cmd/message-server/twoclient_test.go (CP3c)
// is the only place the end-to-end cut is asserted, and it was read for shape
// and deliberately not imported — `sdk` cannot import the server module.
//
// What IS established here: the cut's arithmetic, including the exact multiple
// where an off-by-one produces an empty final part; that every §4.6 abort
// condition has a case of its own and is the rule that decides it; that an
// abandoned reassembly frees its buffer and tells its waiter with a TYPED error
// rather than leaving it to the timeout; and that the part size has exactly one
// declaration in package sdk, measured over the type-checked tree of the whole
// package rather than over its text.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// ─────────────────────────────────────────────────────────────────────────────
// Property 1 — a request cut into parts reassembles to the same bytes.
// ─────────────────────────────────────────────────────────────────────────────

// The sizes, derived from the part size rather than typed out: below one part,
// at the boundary in both directions, and at the exact multiples where an
// off-by-one produces an empty final part.
func messageFragmentSizes() []int {
	part := messageFragmentPartBytes
	return []int{
		0, 1, part - 1, part, part + 1,
		2*part - 1, 2 * part, 2*part + 1,
		3 * part, 5*part + 7,
	}
}

// A request whose MARSHALED length is exactly `size`, or the smallest reachable
// length above it.
//
// "Reachable" matters: the length of `client_epoch_hint` is itself a varint, so
// the marshaled size jumps by two at 127 and at 16383 and one target in each
// neighbourhood has no payload that produces it. The test asserts against the
// length it actually got rather than the one it asked for, which is why this
// returns the request and the caller measures it.
func messageFragmentRequestOfSize(t *testing.T, requestId uint64, size int) *protocol.MessageServerRequest {
	t.Helper()
	build := func(hint int) *protocol.MessageServerRequest {
		request := &protocol.MessageServerRequest{RequestId: requestId, ProtocolVersion: 1}
		if err := setMessageServerRequestBody(request, &protocol.HelloRequest{
			SupportedVersions: []uint32{1},
			ClientEpochHint:   bytes.Repeat([]byte{0xA5}, hint),
		}); err != nil {
			t.Fatalf("could not build a request of %d bytes: %v", size, err)
		}
		return request
	}
	for hint := 0; hint <= size+32; hint += 1 {
		request := build(hint)
		if size <= proto.Size(request) {
			return request
		}
	}
	t.Fatalf("no client_epoch_hint length reaches a marshaled request of %d bytes", size)
	return nil
}

func TestAFragmentedRequestReassemblesToTheSameBytes(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 5*time.Second)

	for _, size := range messageFragmentSizes() {
		t.Run(fmt.Sprintf("%d-bytes", size), func(t *testing.T) {
			request := messageFragmentRequestOfSize(t, 1, size)
			want, err := proto.Marshal(request)
			if err != nil {
				t.Fatalf("could not marshal the request: %v", err)
			}
			actual := len(want)

			frames, err := transport.fragments(request)
			if err != nil {
				t.Fatalf("the cut failed on a %d byte request: %v", actual, err)
			}

			// the whole request in one frame when it fits in a part, and
			// exactly ceil(len/part) fragments when it does not. Both numbers
			// are derived from the length and the constant, never typed.
			if actual <= messageFragmentPartBytes {
				if len(frames) != 1 {
					t.Fatalf("a %d byte request (part size %d) was cut into %d frames, want 1 — "+
						"a request that fits in a part is not fragmented at all",
						actual, messageFragmentPartBytes, len(frames))
				}
				if frames[0].GetMessageType() != protocol.MessageType_MessageMessageServerRequest {
					t.Fatalf("an unfragmented request went out at code point %s, want %s",
						frames[0].GetMessageType(), protocol.MessageType_MessageMessageServerRequest)
				}
				if !bytes.Equal(frames[0].GetMessageBytes(), want) {
					t.Fatalf("the unfragmented frame carries %d bytes, want the request's %d",
						len(frames[0].GetMessageBytes()), actual)
				}
				return
			}

			wantCount := (actual + messageFragmentPartBytes - 1) / messageFragmentPartBytes
			if len(frames) != wantCount {
				t.Fatalf("a %d byte request at part size %d was cut into %d frames, want %d — "+
					"an extra frame here is the empty final part an exact multiple produces when the "+
					"count is computed as len/part + 1",
					actual, messageFragmentPartBytes, len(frames), wantCount)
			}

			assembled := []byte{}
			for index, frame := range frames {
				if frame.GetMessageType() != protocol.MessageType_MessageMessageServerFragment {
					t.Fatalf("frame %d went out at code point %s, want %s",
						index, frame.GetMessageType(), protocol.MessageType_MessageMessageServerFragment)
				}
				fragment := &protocol.MessageServerFragment{}
				if err := proto.Unmarshal(frame.GetMessageBytes(), fragment); err != nil {
					t.Fatalf("frame %d does not decode as a MessageServerFragment: %v", index, err)
				}
				if fragment.GetRequestId() != request.GetRequestId() {
					t.Fatalf("fragment %d carries request_id %d, want %d — reassembly is per (source, request_id)",
						index, fragment.GetRequestId(), request.GetRequestId())
				}
				if fragment.GetIndex() != uint32(index) {
					t.Fatalf("the %dth fragment on the wire carries index %d: §4.6 numbers from zero and "+
						"the receiver aborts on any index it was not waiting for", index, fragment.GetIndex())
				}
				if fragment.GetCount() != uint32(wantCount) {
					t.Fatalf("fragment %d carries count %d, want %d", index, fragment.GetCount(), wantCount)
				}
				if len(fragment.GetPart()) == 0 {
					t.Fatalf("fragment %d of %d carries an EMPTY part: an empty part reassembles to the "+
						"same bytes and is invisible to a round trip, which is why the count and the part "+
						"lengths are asserted and not only the bytes", index, wantCount)
				}
				if messageFragmentPartBytes < len(fragment.GetPart()) {
					t.Fatalf("fragment %d carries %d bytes, past §4.6's ceiling of %d, which is a MUST NOT",
						index, len(fragment.GetPart()), messageFragmentPartBytes)
				}
				if index < wantCount-1 && len(fragment.GetPart()) != messageFragmentPartBytes {
					t.Fatalf("fragment %d of %d carries %d bytes rather than a full part of %d: every part "+
						"but the last is full, or the cut is not at the part size",
						index, wantCount, len(fragment.GetPart()), messageFragmentPartBytes)
				}
				assembled = append(assembled, fragment.GetPart()...)
			}

			if !bytes.Equal(assembled, want) {
				t.Fatalf("the parts reassemble to %d bytes, want the request's %d", len(assembled), actual)
			}

			// and the same bytes through the JOIN this binding ships, not only
			// through the concatenation this test just did
			messageFragmentExpect(transport, request.GetRequestId())
			joined := messageFragmentJoin(t, transport, frames)
			if !bytes.Equal(joined, want) {
				t.Fatalf("the binding's own reassembler produced %d bytes, want %d", len(joined), actual)
			}
		})
	}
}

// A waiter for this request_id, registered the way Call registers one.
//
// It is here because the reassembler opens a buffer only for a request this
// binding MADE -- see TestFragmentsForARequestThisBindingNeverMadeOpenNoBuffer
// -- so a test that drives acceptFragment directly has to be a request that was
// made. Reaching into the map is what makes that explicit instead of hiding it
// behind a Call whose timeout the test would then be living inside.
func messageFragmentExpect(transport *messageTransport, requestId uint64) {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	transport.waiting[requestId] = make(chan messageTransportAnswer, 1)
}

// Drive the shipped reassembler over frames the shipped cut produced.
func messageFragmentJoin(t *testing.T, transport *messageTransport, frames []*protocol.Frame) []byte {
	t.Helper()
	for index, frame := range frames {
		fragment := &protocol.MessageServerFragment{}
		if err := proto.Unmarshal(frame.GetMessageBytes(), fragment); err != nil {
			t.Fatalf("frame %d does not decode: %v", index, err)
		}
		assembled, complete, err := transport.acceptFragment(fragment)
		if err != nil {
			t.Fatalf("the reassembler abandoned a reassembly of frames IT CUT, at fragment %d of %d: %v",
				index, len(frames), err)
		}
		if complete != (index == len(frames)-1) {
			t.Fatalf("the reassembler reported complete=%v at fragment %d of %d",
				complete, index, len(frames))
		}
		if complete {
			return assembled
		}
	}
	t.Fatal("the reassembler never completed")
	return nil
}

// The receive direction, end to end: a response too large for one frame,
// delivered as fragments, reaches the Call that is waiting for it.
//
// The fragments are cut HERE rather than by the shipped cut. The shipped cut is
// for requests and this is a response, and more to the point a join checked
// against its own cut is one implementation checking itself: a mistake in the
// cutting would be undone by the same mistake in the joining.
func TestAFragmentedResponseReachesTheWaiterThatAskedForIt(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 10*time.Second)

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()

	nonce := strings.Repeat("n", 5*messageFragmentPartBytes)
	response := helloResponse(requestId, nonce)
	encoded := encodeMessageResponse(t, response)
	frames := messageFragmentCut(t, requestId, encoded, messageFragmentPartBytes)
	if len(frames) < 2 {
		t.Fatalf("this test cut a %d byte response into %d frames, so it is not testing reassembly",
			len(encoded), len(frames))
	}

	fake.deliver(t, frames...)

	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("the call failed on a fragmented response: %v", result.err)
		}
		if got := string(result.response.GetHello().GetServerNonce()); got != nonce {
			t.Fatalf("the reassembled response carries %d nonce bytes, want %d", len(got), len(nonce))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a response delivered in §4.6 fragments never reached the waiter that asked for it")
	}

	counts := transport.Counts()
	if counts.FragmentFrames != uint64(len(frames)) {
		t.Fatalf("Counts().FragmentFrames is %d after %d fragment frames, want %d",
			counts.FragmentFrames, len(frames), len(frames))
	}
	if counts.Reassembled != 1 {
		t.Fatalf("Counts().Reassembled is %d after one completed reassembly, want 1", counts.Reassembled)
	}
	if counts.Aborted != 0 {
		t.Fatalf("Counts().Aborted is %d after a clean reassembly, want 0", counts.Aborted)
	}
	if counts.Responses != 1 {
		t.Fatalf("Counts().Responses is %d, want 1", counts.Responses)
	}
	if counts.Reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after the reassembly completed, want 0 — the buffer outlived it",
			counts.Reassembling)
	}
	if counts.ResponseFrames != 0 {
		t.Fatalf("Counts().ResponseFrames is %d: a fragment frame is not a response frame and the two "+
			"counters must not be one", counts.ResponseFrames)
	}
}

// This file's own cut, deliberately independent of the shipped one.
func messageFragmentCut(t *testing.T, requestId uint64, body []byte, part int) []*protocol.Frame {
	t.Helper()
	frames := []*protocol.Frame{}
	count := (len(body) + part - 1) / part
	for index := 0; index < count; index += 1 {
		end := (index + 1) * part
		if len(body) < end {
			end = len(body)
		}
		frames = append(frames, messageFragmentFrame(t, &protocol.MessageServerFragment{
			RequestId: requestId,
			Index:     uint32(index),
			Count:     uint32(count),
			Part:      body[index*part : end],
		}))
	}
	return frames
}

func messageFragmentFrame(t *testing.T, fragment *protocol.MessageServerFragment) *protocol.Frame {
	t.Helper()
	encoded, err := proto.Marshal(fragment)
	if err != nil {
		t.Fatalf("could not encode a fragment: %v", err)
	}
	return &protocol.Frame{
		MessageType:  protocol.MessageType_MessageMessageServerFragment,
		MessageBytes: encoded,
	}
}

// §4.6: "Reassembly state is per (source client_id, request_id)". This binding
// talks to ONE server, so the source is fixed and the key is the request_id
// alone — and that is exactly the claim worth driving, because a reassembler
// that held one buffer instead of a map answers every test above identically
// and corrupts the moment two requests are outstanding.
//
// The two responses are delivered INTERLEAVED, which is the arrangement a
// single buffer cannot survive: A's first part, B's first part, A's second, B's
// second. §4.6's in-order rule is per reassembly and says nothing about the
// order two different reassemblies arrive in.
func TestTwoReassembliesInFlightDoNotReachIntoEachOther(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 10*time.Second)

	first := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	second := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{2}})
	awaitRequests(t, fake, 2)

	firstId := fake.requestAt(0).GetRequestId()
	secondId := fake.requestAt(1).GetRequestId()
	if firstId == secondId {
		t.Fatalf("both requests share request_id %d, so nothing here is keyed apart", firstId)
	}

	firstNonce := strings.Repeat("A", 3*messageFragmentPartBytes)
	secondNonce := strings.Repeat("B", 3*messageFragmentPartBytes)
	firstFrames := messageFragmentCut(t, firstId,
		encodeMessageResponse(t, helloResponse(firstId, firstNonce)), messageFragmentPartBytes)
	secondFrames := messageFragmentCut(t, secondId,
		encodeMessageResponse(t, helloResponse(secondId, secondNonce)), messageFragmentPartBytes)
	if len(firstFrames) != len(secondFrames) || len(firstFrames) < 3 {
		t.Fatalf("this test needs two equal cuts of at least three frames and got %d and %d",
			len(firstFrames), len(secondFrames))
	}

	for index := range firstFrames {
		fake.deliver(t, firstFrames[index])
		fake.deliver(t, secondFrames[index])
		if index+1 < len(firstFrames) {
			if open := transport.Counts().Reassembling; open != 2 {
				t.Fatalf("Counts().Reassembling is %d after %d part(s) of each of two responses, want 2 — "+
					"§4.6's reassembly state is per request_id and two of them are open", open, index+1)
			}
		}
	}

	for _, each := range []struct {
		name    string
		results chan messageTransportResult
		id      uint64
		nonce   string
	}{
		{"first", first, firstId, firstNonce},
		{"second", second, secondId, secondNonce},
	} {
		select {
		case result := <-each.results:
			if result.err != nil {
				t.Fatalf("the %s call failed: %v", each.name, result.err)
			}
			got := string(result.response.GetHello().GetServerNonce())
			if got != each.nonce {
				t.Fatalf("the %s call was reassembled into %d bytes starting %q, want %d bytes of %q — "+
					"the two reassemblies were sharing a buffer",
					each.name, len(got), firstFewOf(got), len(each.nonce), each.nonce[:1])
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s call was never answered, though every fragment of its response was delivered",
				each.name)
		}
	}

	counts := transport.Counts()
	if counts.Reassembled != 2 {
		t.Fatalf("Counts().Reassembled is %d, want 2", counts.Reassembled)
	}
	if counts.Aborted != 0 {
		t.Fatalf("Counts().Aborted is %d: interleaving two reassemblies is not a §4.6 abort, because "+
			"the in-order rule is per reassembly", counts.Aborted)
	}
	if counts.Reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after both completed, want 0", counts.Reassembling)
	}
}

func firstFewOf(value string) string {
	if len(value) <= 24 {
		return value
	}
	return value[:24] + "..."
}

// §4.6 caps the SERVER's reassembly state at sixteen per client and thirty
// seconds. It caps a client's at nothing, and the client is the side that
// cannot choose who addresses frames to it.
//
// So the bound here is derived rather than declared: this binding opens a
// reassembly only for a request it MADE, and the waiter set is already exactly
// that. A fragment carrying a request_id no waiter is waiting on is the fragment
// analogue of the response nobody asked for -- counted, and dropped, and it
// costs a buffer of nothing.
//
// Without it, one number the sender chooses opens one buffer, and as many
// numbers as it cares to name open as many buffers: the memory-exhaustion vector
// §4.6 exists to close, arriving on the side of the wire where §4.6 closed
// nothing.
func TestFragmentsForARequestThisBindingNeverMadeOpenNoBuffer(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 10*time.Second)

	mine := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()

	// a hundred first-fragments under a hundred request_ids nobody asked for
	strangers := 100
	for stranger := 0; stranger < strangers; stranger += 1 {
		fake.deliver(t, messageFragmentFrame(t, &protocol.MessageServerFragment{
			RequestId: requestId + uint64(1000+stranger),
			Index:     0,
			Count:     4,
			Part:      bytes.Repeat([]byte{0x5A}, messageFragmentPartBytes),
		}))
	}

	counts := transport.Counts()
	if counts.Reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after %d opening fragments under request_ids this binding "+
			"never asked under, want 0 — one number the sender chooses would otherwise open one buffer, "+
			"and §4.6 bounds a client's reassembly state at nothing at all",
			counts.Reassembling, strangers)
	}
	if counts.Unmatched != uint64(strangers) {
		t.Fatalf("Counts().Unmatched is %d after %d such fragments, want %d — dropped is right and "+
			"dropped in SILENCE is not: \"nothing arrived\" and \"something arrived for nobody\" have to "+
			"be two readings", counts.Unmatched, strangers, strangers)
	}
	if counts.Aborted != 0 {
		t.Fatalf("Counts().Aborted is %d: a fragment for a request that was never made is not an abort — "+
			"there is no reassembly to abandon and no waiter to tell", counts.Aborted)
	}
	if counts.FragmentFrames != uint64(strangers) {
		t.Fatalf("Counts().FragmentFrames is %d, want %d — the frames arrived whatever became of them",
			counts.FragmentFrames, strangers)
	}
	if counts.Waiting != 1 {
		t.Fatalf("Counts().Waiting is %d, want 1 — the outstanding Call must still be outstanding", counts.Waiting)
	}

	// and the request this binding DID make still reassembles, so the refusal is
	// a narrowing and not a wall
	nonce := strings.Repeat("C", 3*messageFragmentPartBytes)
	frames := messageFragmentCut(t, requestId,
		encodeMessageResponse(t, helloResponse(requestId, nonce)), messageFragmentPartBytes)
	fake.deliver(t, frames...)
	select {
	case result := <-mine:
		if result.err != nil {
			t.Fatalf("the call this binding actually made failed: %v", result.err)
		}
		if got := string(result.response.GetHello().GetServerNonce()); got != nonce {
			t.Fatalf("the reassembled response carries %d nonce bytes, want %d", len(got), len(nonce))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the response to a request this binding DID make was refused along with the strangers")
	}
	if reassembling := transport.Counts().Reassembling; reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after the real reassembly completed, want 0", reassembling)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Property 2 — an out-of-order, duplicated or short-counted fragment ABORTS.
// ─────────────────────────────────────────────────────────────────────────────

// One arriving sequence and what it must do to the reassembly.
type messageFragmentCase struct {
	name string

	// The fragments as they arrive, built by hand so that the malformation is
	// this test's and not a bug in a cutter.
	arriving []*protocol.MessageServerFragment

	// The rule of [messageFragmentAborts] that must be the one to decide, or ""
	// when this sequence is not an abort at all.
	rule string
}

func messageFragmentCases() []messageFragmentCase {
	part := func(n int) []byte { return bytes.Repeat([]byte{0x7E}, n) }
	return []messageFragmentCase{
		{
			name: "in order and complete",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 2, Part: part(8)},
				{RequestId: 1, Index: 1, Count: 2, Part: part(8)},
			},
			rule: "",
		},
		{
			name: "a first fragment that is not index 0",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 1, Count: 3, Part: part(8)},
			},
			rule: "an index that is not the one this reassembly is waiting for",
		},
		{
			name: "an index skipped mid-reassembly",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 3, Part: part(8)},
				{RequestId: 1, Index: 2, Count: 3, Part: part(8)},
			},
			rule: "an index that is not the one this reassembly is waiting for",
		},
		{
			name: "an index delivered twice",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 3, Part: part(8)},
				{RequestId: 1, Index: 0, Count: 3, Part: part(8)},
			},
			rule: "an index that is not the one this reassembly is waiting for",
		},
		{
			name: "a count that changed mid-reassembly",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 3, Part: part(8)},
				{RequestId: 1, Index: 1, Count: 2, Part: part(8)},
			},
			rule: "a fragment count that changed mid-reassembly",
		},
		{
			name: "an index that is not below the count",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 3, Count: 3, Part: part(8)},
			},
			rule: "an index that is not below the fragment count",
		},
		{
			name: "a count of zero, which names no fragments at all",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 0, Part: part(8)},
			},
			rule: "an index that is not below the fragment count",
		},
		{
			name: "a part one byte past §4.6's ceiling",
			arriving: []*protocol.MessageServerFragment{
				{RequestId: 1, Index: 0, Count: 2, Part: part(messageFragmentPartBytes + 1)},
			},
			rule: "a part larger than §4.6's own ceiling",
		},
	}
}

func TestAMalformedFragmentAbortsTheReassemblyAndTellsTheWaiter(t *testing.T) {
	for _, each := range messageFragmentCases() {
		if each.rule == "" {
			continue
		}
		t.Run(each.name, func(t *testing.T) {
			fake := &messageTransportFake{}
			transport := newTestMessageTransport(t, fake, 30*time.Second)

			results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
			awaitRequests(t, fake, 1)
			requestId := fake.requestAt(0).GetRequestId()

			for _, fragment := range each.arriving {
				arriving := proto.Clone(fragment).(*protocol.MessageServerFragment)
				arriving.RequestId = requestId
				fake.deliver(t, messageFragmentFrame(t, arriving))
			}

			select {
			case result := <-results:
				if result.err == nil {
					t.Fatalf("an aborted reassembly answered the waiter with a response: %v — "+
						"§4.6 aborts rather than buffering holes, and a partial message is exactly "+
						"what it must not produce", result.response)
				}
				if !errors.Is(result.err, errMessageFragmentAborted) {
					t.Fatalf("the abort was reported as %v, which is not errMessageFragmentAborted — "+
						"the refusal Property 2 owes is TYPED, so that a caller can tell an abort "+
						"from a timeout without watching the clock", result.err)
				}
				if !strings.Contains(result.err.Error(), each.rule) {
					t.Fatalf("the abort names %q; this sequence is refused by %q", result.err, each.rule)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("an abandoned reassembly left its waiter waiting: the abort is decided inside " +
					"the receive callback and has nowhere else to go, so a waiter that is not told " +
					"waits out the whole timeout to be told the wrong thing")
			}

			counts := transport.Counts()
			if counts.Aborted != 1 {
				t.Fatalf("Counts().Aborted is %d after one abandoned reassembly, want 1 — an abort and a "+
					"timeout are the same silence to a caller that only watches the clock", counts.Aborted)
			}
			if counts.Reassembling != 0 {
				t.Fatalf("Counts().Reassembling is %d after the abort, want 0 — §4.6 frees the buffer "+
					"immediately, and a buffer freed one stage later is a buffer a sender holds open "+
					"by never sending the last fragment", counts.Reassembling)
			}
			if counts.Responses != 0 {
				t.Fatalf("Counts().Responses is %d after an aborted reassembly, want 0", counts.Responses)
			}
			if counts.Waiting != 0 {
				t.Fatalf("Counts().Waiting is %d after the abort took the waiter, want 0", counts.Waiting)
			}
			if counts.Reassembled != 0 {
				t.Fatalf("Counts().Reassembled is %d, want 0", counts.Reassembled)
			}
		})
	}
}

// A reassembly that is missing a fragment produces NOTHING: no partial message,
// no abort, and no forgetting that it is open.
func TestAReassemblyMissingAFragmentProducesNoPartialMessage(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 400*time.Millisecond)

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()

	nonce := strings.Repeat("m", 3*messageFragmentPartBytes)
	frames := messageFragmentCut(t, requestId, encodeMessageResponse(t, helloResponse(requestId, nonce)), messageFragmentPartBytes)
	if len(frames) < 3 {
		t.Fatalf("this test needs at least three fragments and cut %d", len(frames))
	}
	// everything but the last
	fake.deliver(t, frames[:len(frames)-1]...)

	counts := transport.Counts()
	if counts.Responses != 0 || counts.Reassembled != 0 {
		t.Fatalf("an incomplete reassembly produced a response: Responses %d, Reassembled %d — "+
			"§4.6 does not deliver what it has when a fragment never arrives", counts.Responses, counts.Reassembled)
	}
	if counts.Aborted != 0 {
		t.Fatalf("Counts().Aborted is %d: a fragment that has not arrived yet is not an abort", counts.Aborted)
	}
	if counts.Reassembling != 1 {
		t.Fatalf("Counts().Reassembling is %d while one reassembly is open and incomplete, want 1",
			counts.Reassembling)
	}

	// the Call gives up on the clock, and the buffer goes with the waiter
	select {
	case result := <-results:
		if !errors.Is(result.err, errMessageTransportTimeout) {
			t.Fatalf("the call returned %v, want the typed timeout — a reassembly that never completes "+
				"is a response that never arrived", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never gave up")
	}
	if reassembling := transport.Counts().Reassembling; reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after the waiter timed out, want 0 — a half-arrived "+
			"response leaves a buffer keyed on a request_id nothing is waiting for, which is the same "+
			"leak as the correlation entry and is not visible in the same counter", reassembling)
	}
}

// REVIEW FINDING H — a reassembly that completes into bytes that are not a
// response is a response that will never arrive, and its waiter is TOLD.
//
// The fragment arm of [messageTransport.receive] had a `continue` here, and the
// waiter was left for its own timeout to discover — the exact thing
// message_transport_fragment.go's header refuses for every other §4.6
// abandonment: "a caller that waits thirty seconds to learn what the receive
// path knew immediately has been told the wrong thing about why". The
// response-frame arm's justification for `continue` — a response that did not
// decode carries no request_id — does not apply here: every fragment carried
// one, and it is the key the buffer was filed under.
//
// The transport timeout here is deliberately LONGER than the wait below, so
// that a pass cannot be a timeout arriving early.
func TestAReassemblyThatCompletesIntoUndecodableBytesTellsItsWaiter(t *testing.T) {
	fake := &messageTransportFake{}
	transport := newTestMessageTransport(t, fake, 30*time.Second)

	results := callInBackground(transport, context.Background(), &protocol.HelloRequest{SupportedVersions: []uint32{1}})
	awaitRequests(t, fake, 1)
	requestId := fake.requestAt(0).GetRequestId()

	// 0xFF is wire type 7, which no protobuf field can have, so these bytes
	// reassemble perfectly and decode as nothing
	undecodable := bytes.Repeat([]byte{0xFF}, 3*messageFragmentPartBytes)
	if proto.Unmarshal(undecodable, &protocol.MessageServerResponse{}) == nil {
		t.Fatal("the bytes this test calls undecodable decode as a MessageServerResponse, so it is " +
			"measuring nothing")
	}
	frames := messageFragmentCut(t, requestId, undecodable, messageFragmentPartBytes)
	if len(frames) < 2 {
		t.Fatalf("this test needs a FRAGMENTED response and cut %d frame(s)", len(frames))
	}
	fake.deliver(t, frames...)

	select {
	case result := <-results:
		if result.response != nil {
			t.Fatalf("a reassembly of undecodable bytes answered the waiter with a response: %v", result.response)
		}
		if !errors.Is(result.err, errMessageFragmentAborted) {
			t.Fatalf("the waiter was told %v, want errMessageFragmentAborted", result.err)
		}
		for _, named := range []string{"MessageServerResponse", "6144"} {
			if !strings.Contains(result.err.Error(), named) {
				t.Fatalf("the refusal %q does not name %q: it is the one thing the receive path knew "+
					"and the caller does not", result.err, named)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a reassembly that COMPLETED into bytes that are not a MessageServerResponse left its " +
			"waiter waiting: the request_id is in hand on that path, so the waiter is reachable, and " +
			"every other §4.6 abandonment tells it immediately")
	}

	counts := transport.Counts()
	if counts.Reassembled != 1 {
		t.Fatalf("Counts().Reassembled is %d: the reassembly COMPLETED, and that it decoded to nothing "+
			"is a different reading", counts.Reassembled)
	}
	if counts.Responses != 0 {
		t.Fatalf("Counts().Responses is %d after a reassembly that decoded to nothing, want 0", counts.Responses)
	}
	if counts.Waiting != 0 {
		t.Fatalf("Counts().Waiting is %d after the waiter was told, want 0", counts.Waiting)
	}
	if counts.Reassembling != 0 {
		t.Fatalf("Counts().Reassembling is %d after the reassembly completed, want 0", counts.Reassembling)
	}
}

// ── the abort table, and the gate that keeps it from understating itself ─────
//
// GATE CLASS: the rules of [messageFragmentAborts] — read off the table at run
//
//	time, never listed here.
//
// GATE SCOPE: the arriving sequences of [messageFragmentCases], which is this
//
//	file's whole set of §4.6 malformations.
//
// A class typed out by hand has understated itself every time it has been tried
// on this project, so the assertion is a BIJECTION rather than a count: every
// rule is the deciding rule for at least one case, and every case that claims a
// rule is decided by that rule and not by an earlier one that happens to fire
// first. A rule with no case of its own can be deleted with this file green,
// and a case whose refusal actually comes from a different rule is a case that
// is testing something other than what it says.
func TestEveryWayThisBindingAbortsAReassemblyHasACaseOfItsOwn(t *testing.T) {
	rules := messageFragmentAborts
	if len(rules) == 0 {
		t.Fatal("messageFragmentAborts is empty, so this binding abandons nothing and §4.6's abort is not implemented")
	}
	names := []string{}
	for _, rule := range rules {
		names = append(names, rule.name)
	}
	t.Logf("GATE CLASS: %d abort rule(s) read off messageFragmentAborts: %v", len(rules), names)

	decidedBy := map[string][]string{}
	accepted := []string{}
	for _, each := range messageFragmentCases() {
		// replay the sequence against the table alone, with no transport, so
		// that the rule that fires is read directly
		var current *messageFragmentPartial
		deciding := ""
		for _, fragment := range each.arriving {
			state := messageFragmentState{fragment: fragment, current: current}
			fired := ""
			for _, rule := range rules {
				if rule.aborts(state) {
					fired = rule.name
					break
				}
			}
			if fired != "" {
				deciding = fired
				break
			}
			if current == nil {
				current = &messageFragmentPartial{count: fragment.GetCount()}
			}
			current.bytes = append(current.bytes, fragment.GetPart()...)
			current.next += 1
		}
		if deciding == "" {
			accepted = append(accepted, each.name)
		} else {
			decidedBy[deciding] = append(decidedBy[deciding], each.name)
		}
		if deciding != each.rule {
			t.Errorf("the case %q says it is refused by %q and is actually refused by %q",
				each.name, orNothing(each.rule), orNothing(deciding))
		}
	}

	t.Logf("COMPLEMENT — sequences this table does NOT abort: %d %v", len(accepted), accepted)
	if len(accepted) == 0 {
		t.Fatal("the complement is EMPTY: every sequence in this file is refused, so the table would " +
			"pass a test that refused everything and this file could not tell the difference")
	}

	without := []string{}
	for _, rule := range rules {
		t.Logf("  %-52s decides %d case(s): %v", rule.name, len(decidedBy[rule.name]), decidedBy[rule.name])
		if len(decidedBy[rule.name]) == 0 {
			without = append(without, rule.name)
		}
	}
	if len(without) != 0 {
		t.Fatalf("%d abort rule(s) have no case of their own and could be deleted with this file green: %v",
			len(without), without)
	}
}

func orNothing(name string) string {
	if name == "" {
		return "nothing (accepted)"
	}
	return name
}

// ═════════════════════════════════════════════════════════════════════════════
// Property 3 — the part size has exactly one declaration in `sdk`.
// ═════════════════════════════════════════════════════════════════════════════
//
// GATE CLASS, derived and stated separately from the scope (R3):
//
//	every constant EXPRESSION in package sdk's production files whose EVALUATED
//	VALUE equals the part size. It is read off the TYPE-CHECKED syntax tree —
//	go/types' Info.Types[expr].Value — and NEVER off the literal text. Two
//	things turn on the word EXPRESSION. A gate that scans literals cannot see
//	`1 << 11`, which contains no literal equal to 2048 and evaluates to it; and
//	a gate that scans one file passes on the day somebody writes the number into
//	the send path, which is the divergence this property exists to prevent.
//
// GATE SCOPE, derived separately from the class (R3):
//
//	the WHOLE package — every production file go/build says THIS build compiles
//	— and not this file and not the transport's files. The scope is asked of
//	go/build rather than assumed, and the files it excludes are printed.
//
// WHAT IT REPORTS RATHER THAN ASSUMES. The plan predicted this class has ONE
// member at this task. It does not, and the gate reports the number it READ:
// package sdk already contained a constant expression equal to 2048 before this
// task existed — `MessagePoolGet(2048)` in device_local_ioloop.go, a packet
// buffer size that has nothing to do with §4.6. So the class is partitioned and
// every part is printed:
//
//	DECLARATIONS   named constants of package sdk whose value equals the part
//	               size. Exactly one, and it is the part size's own.
//	REFERENCES     uses of that constant. They are the opposite of a second
//	               copy and they are counted, not flagged.
//	COPIES         everything else — a value equal to the part size written
//	               somewhere that is not the declaration and does not name it.
//	               None of them may be in the binding's own files, which is
//	               where a second copy would diverge; the ones outside are
//	               printed with their positions so that being wrong about one
//	               is visible rather than silent.
//
// The partition is asserted to CLOSE, which is the assertion that catches a
// member the split dropped on the floor.
func TestThePartSizeHasExactlyOneDeclarationInPackageSdk(t *testing.T) {
	checked := messageFragmentTypeCheck(t)

	// ── the part size, read off the type checker rather than typed here ─────
	object := checked.pkg.Scope().Lookup("messageFragmentPartBytes")
	declared, isConst := object.(*types.Const)
	if !isConst {
		t.Fatalf("package sdk declares messageFragmentPartBytes as %T, not a constant, so the part size "+
			"has no value for this gate to read", object)
	}
	partSize, exact := constant.Int64Val(constant.ToInt(declared.Val()))
	if !exact {
		t.Fatalf("the part size %s is not an exact integer, so no expression can be compared with it",
			declared.Val())
	}
	t.Logf("the part size this gate measures against: %s = %d, declared at %s",
		declared.Name(), partSize, checked.fset.Position(declared.Pos()))

	// ── the universe the narrowing is taken out of ──────────────────────────
	//
	// Every expression the checker recorded a CONSTANT VALUE for. The
	// comparison converts to an integer first and refuses a value that is not
	// one: constant.Compare on two values of different kinds does not answer
	// "not equal", and an earlier draft of this gate reported 2371 of 3801
	// constant expressions as equal to 2048 because strings and booleans were
	// being compared against an integer.
	universe := 0
	notInteger := 0
	class := []messageFragmentConstSite{}
	for expr, tv := range checked.info.Types {
		if tv.Value == nil {
			continue
		}
		universe += 1
		number, ok := messageFragmentIntValue(tv.Value)
		if !ok {
			notInteger += 1
			continue
		}
		if number == partSize {
			class = append(class, messageFragmentConstSite{
				expr: expr,
				pos:  checked.fset.Position(expr.Pos()).String(),
				text: types.ExprString(expr),
			})
		}
	}
	t.Logf("SCOPE: %d production file(s) of package sdk that go/build says this %s/%s build compiles; "+
		"%d it does NOT: %v",
		len(checked.files), build.Default.GOOS, build.Default.GOARCH,
		len(checked.excluded), checked.excluded)
	t.Logf("UNIVERSE: %d constant expression(s) in the type-checked tree, %d of them not integers; "+
		"complement — constant expressions whose value is NOT the part size: %d",
		universe, notInteger, universe-len(class))
	if universe == 0 {
		t.Fatal("the type-checked tree holds NO constant expression at all: the gate read nothing, " +
			"and a gate that reads nothing passes whatever the package says")
	}
	if universe-len(class) == 0 {
		t.Fatalf("the complement is EMPTY: all %d constant expressions in package sdk evaluate to the "+
			"part size, which is not a package, it is a mistake in this gate", universe)
	}
	if len(checked.excluded) == 0 {
		t.Fatal("go/build excludes no production file of package sdk, so the scope derivation narrows " +
			"nothing and has not been asked")
	}

	// ── the partition ───────────────────────────────────────────────────────
	declarationExprs := map[ast.Expr]bool{}
	declarations := []string{}
	for ident, defined := range checked.info.Defs {
		named, ok := defined.(*types.Const)
		if !ok {
			continue
		}
		if number, isInteger := messageFragmentIntValue(named.Val()); !isInteger || number != partSize {
			continue
		}
		declarations = append(declarations,
			fmt.Sprintf("%s at %s", named.Name(), checked.fset.Position(ident.Pos())))
	}
	for _, file := range checked.files {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for index, name := range spec.Names {
				named, isConst := checked.info.Defs[name].(*types.Const)
				if !isConst || len(spec.Values) <= index {
					continue
				}
				if number, isInteger := messageFragmentIntValue(named.Val()); !isInteger || number != partSize {
					continue
				}
				declarationExprs[spec.Values[index]] = true
			}
			return true
		})
	}

	references := []messageFragmentConstSite{}
	copies := []messageFragmentConstSite{}
	declaring := []messageFragmentConstSite{}
	for _, site := range class {
		if declarationExprs[site.expr] {
			declaring = append(declaring, site)
			continue
		}
		if ident, ok := site.expr.(*ast.Ident); ok && checked.info.Uses[ident] == object {
			references = append(references, site)
			continue
		}
		if selector, ok := site.expr.(*ast.SelectorExpr); ok && checked.info.Uses[selector.Sel] == object {
			references = append(references, site)
			continue
		}
		copies = append(copies, site)
	}

	sort.Strings(declarations)
	t.Logf("CLASS: %d constant expression(s) in package sdk evaluate to %d", len(class), partSize)
	t.Logf("  DECLARATIONS — named constants of package sdk whose value is the part size: %d %v",
		len(declarations), declarations)
	t.Logf("  their value expressions: %d %v", len(declaring), sitesOf(declaring))
	t.Logf("  REFERENCES — uses of %s: %d %v", declared.Name(), len(references), sitesOf(references))
	t.Logf("  COPIES — the value written without naming the constant: %d %v", len(copies), sitesOf(copies))

	if len(declaring)+len(references)+len(copies) != len(class) {
		t.Fatalf("the partition does not close: %d declaring + %d references + %d copies over a class of %d",
			len(declaring), len(references), len(copies), len(class))
	}
	if len(references) == 0 {
		t.Fatalf("nothing in package sdk USES %s: the part size is declared and read by nobody, so the "+
			"cut is not applying it", declared.Name())
	}
	if len(declarations) != 1 {
		t.Fatalf("package sdk declares %d constant(s) whose value is the part size %d: %v. "+
			"Property 3 is that it has exactly ONE declaration, and two declarations of one wire bound "+
			"are two things that can be edited apart",
			len(declarations), partSize, declarations)
	}
	if !strings.HasPrefix(declarations[0], "messageFragmentPartBytes ") {
		t.Fatalf("the one declaration of the part size is %q, not messageFragmentPartBytes", declarations[0])
	}

	// ── the copies, and the two narrowings that say what happens to them ────
	//
	// REVIEW FINDING A. `binding` is a NARROWING, and a narrowing that comes
	// back empty makes the assertion below a claim about no files at all: the
	// final check was `len(inBinding) != 0`, so with an empty narrowing plan
	// mutation 8 passed green with the copy printed one line above the PASS.
	// Forced empty, that is exactly what happened. It fails closed now, the way
	// the goroutine gate over the same derivation already did.
	gate := newBorrowGate(t)
	binding := gate.bindingFiles()
	t.Logf("  the binding's own files, derived as the production files declaring a method on "+
		"messageTransport: %d %v", len(binding), binding)
	if len(binding) == 0 {
		t.Fatal("the enforcement narrowing is EMPTY: no production file of package sdk declares a " +
			"method on messageTransport, so `no copy in the binding's own files` is a claim about no " +
			"files, and every copy in the package satisfies it")
	}
	within := map[string]bool{}
	for _, name := range binding {
		within[name] = true
	}
	sources := map[string]*ast.File{}
	for index, name := range checked.names {
		sources[name] = checked.files[index]
	}
	inBinding := []messageFragmentConstSite{}
	outside := []messageFragmentCopy{}
	for _, site := range copies {
		position := checked.fset.Position(site.expr.Pos())
		name := filepath.Base(position.Filename)
		if within[name] {
			inBinding = append(inBinding, site)
			continue
		}
		outside = append(outside, messageFragmentCopy{
			file:  name,
			where: messageFragmentEnclosing(sources[name], site.expr.Pos()),
			pos:   position.String(),
			text:  site.text,
		})
	}

	// ── and the files this build does not compile, which the type checker
	//    cannot reach at all ──────────────────────────────────────────────────
	//
	// THIS IS WHERE THE PLAN'S PREDICTION IS STALE, and the gate reports what
	// it read rather than what was predicted. The plan says this class has ONE
	// member at this task. On windows/amd64 it does. On any other build it has
	// TWO: device_local_ioloop.go is `//go:build !windows` and holds
	// `MessagePoolGet(2048)`, a packet buffer size with nothing to do with
	// §4.6, which this build's type checker never sees. A gate that only ever
	// ran here would report "one member" as a property of the package when it
	// is a property of the platform.
	//
	// So the excluded files get the weaker derivation and are told apart from
	// the stronger one: their integer constant EXPRESSIONS are evaluated
	// directly off the syntax tree — which still sees `1 << 11` and still does
	// not see a named constant, since nothing resolves names here. The
	// weakness is printed rather than hidden, and the assertions are the same
	// two the compiled files get: none of them may be in the binding's own
	// files, and none of them may be a DECLARATION. The declaration half is
	// review finding B's sharper half — before it, `const x = 2048` inside a
	// `//go:build !windows` file was printed and passed here while failing on a
	// linux build, which is a property of the platform wearing a gate's coat.
	unmeasured := []string{}
	excludedDeclarations := []string{}
	for _, name := range checked.excluded {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("could not parse the out-of-build file %s: %v", name, err)
		}
		declared := map[ast.Expr]string{}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for index, named := range spec.Names {
				if len(spec.Values) <= index {
					continue
				}
				if value, ok := messageFragmentUntypedInt(spec.Values[index]); ok && value == partSize {
					declared[spec.Values[index]] = named.Name
				}
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			value, ok := messageFragmentUntypedInt(expr)
			if !ok || value != partSize {
				return true
			}
			position := fset.Position(expr.Pos())
			unmeasured = append(unmeasured, fmt.Sprintf("%s %s", position, types.ExprString(expr)))
			if declaredName, isDeclaration := declared[expr]; isDeclaration {
				excludedDeclarations = append(excludedDeclarations,
					fmt.Sprintf("%s at %s", declaredName, position))
				return true
			}
			if within[name] {
				inBinding = append(inBinding, messageFragmentConstSite{
					expr: expr,
					pos:  position.String(),
					text: types.ExprString(expr),
				})
				return true
			}
			outside = append(outside, messageFragmentCopy{
				file:  name,
				where: messageFragmentEnclosing(file, expr.Pos()),
				pos:   position.String(),
				text:  types.ExprString(expr),
			})
			return true
		})
	}
	sort.Strings(unmeasured)
	sort.Strings(excludedDeclarations)

	// ── REVIEW FINDING B: the copies outside the binding are ASSERTED, not
	//    only printed ─────────────────────────────────────────────────────────
	//
	// The class is read package-wide, which is what makes `1 << 11` in a file
	// far from here visible at all — but it used to be asserted over the
	// binding's three files, so a copy anywhere else in package sdk was printed
	// and passed by the whole suite. That is the plan's own named divergence —
	// "a gate that scans one file passes on the day somebody writes the number
	// into the send path" — deferred rather than prevented: Task 10's send path
	// is a different type in a different file, and would declare no
	// messageTransport method.
	//
	// Package-wide now, with one escape: a ruling, by name, with a sentence.
	excused := []string{}
	unexcused := []messageFragmentCopy{}
	matched := map[string]bool{}
	rulings := messageFragmentPartSizeRulings(os.O_EXCL)
	for _, copied := range outside {
		key, ruled := messageFragmentPartSizeRulingKey(copied, rulings)
		if !ruled {
			unexcused = append(unexcused, copied)
			continue
		}
		matched[key] = true
		excused = append(excused, fmt.Sprintf("%s %s (in %s)", copied.pos, copied.text, key))
	}
	stale := []string{}
	for key := range rulings {
		if !matched[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(excused)
	sort.Strings(stale)

	t.Logf("  COPIES INSIDE the binding's files: %d %v", len(inBinding), sitesOf(inBinding))
	t.Logf("  COPIES OUTSIDE them, anywhere in package sdk, which is where the scope is: %d %v",
		len(outside), copiesOf(outside))
	t.Logf("  of those, EXCUSED by a ruling of their own: %d %v", len(excused), excused)
	t.Logf("  IN THE %d FILE(S) THIS BUILD DOES NOT COMPILE, evaluated off the syntax tree rather than "+
		"off the type checker: %d %v", len(checked.excluded), len(unmeasured), unmeasured)
	if len(inBinding) != 0 {
		t.Fatalf("%d constant expression(s) inside the binding's own files %v evaluate to the part size "+
			"without naming it: %v. The class is the VALUE and not the spelling, so `1 << 11` is here "+
			"for the same reason `2048` is",
			len(inBinding), binding, sitesOf(inBinding))
	}
	if len(unexcused) != 0 {
		t.Fatalf("%d constant expression(s) elsewhere in package sdk evaluate to the part size %d "+
			"without naming it and without a ruling: %v. Property 3's scope is the PACKAGE, not this "+
			"binding's files. If one of these is not §4.6's part size, say so in "+
			"messageFragmentPartSizeCopyRulings under the key printed beside it; if it is, name the "+
			"constant", len(unexcused), partSize, copiesOf(unexcused))
	}
	if len(stale) != 0 {
		t.Fatalf("%d ruling(s) in messageFragmentPartSizeCopyRulings excuse a copy that is not there: "+
			"%v. An excuse for something that is gone is an excuse nothing checks, and it is how a "+
			"name-matched class quietly stops being one", len(stale), stale)
	}
	if len(excludedDeclarations) != 0 {
		t.Fatalf("%d named declaration(s) of the part size %d live in a file this build does not "+
			"compile: %v. The type checker above counted the declarations it could reach and found "+
			"one; this is the other half, and a second declaration is two things that can be edited "+
			"apart whatever the build tag on the file says",
			len(excludedDeclarations), partSize, excludedDeclarations)
	}
}

// One copy of the part size, with the declaration it sits inside, which is what
// a ruling names.
type messageFragmentCopy struct {
	file  string
	where string
	pos   string
	text  string
}

func copiesOf(copies []messageFragmentCopy) []string {
	shown := []string{}
	for _, copied := range copies {
		shown = append(shown, fmt.Sprintf("%s %s (in %s %s)", copied.pos, copied.text, copied.file, copied.where))
	}
	sort.Strings(shown)
	return shown
}

// Copies of the part size OUTSIDE the binding's own files, excused one at a
// time and by name.
//
// Every entry is a sentence saying why that value is not §4.6's part size. The
// key is the FILE and the DECLARATION the copy sits inside, optionally followed
// by the exact EXPRESSION when only that expression has been audited. An excuse
// survives the line moving without covering a whole file or, for an expression
// ruling, every other value in the same declaration.
// Every entry is also asserted to MATCH something — an excuse for a copy that is
// gone is an excuse nothing checks any more.
//
// It holds no entry in the binding's own files and cannot: a copy there is
// refused before this table is consulted. This is a table for values that are
// not this bound, never a second home for this bound.
// messageFragmentPartSizeRulings merges the table below with the existing linux
// half and the imported os.O_EXCL value on the current target. Taking that value
// as an argument lets the regression exercise both platform outcomes on one host.
func messageFragmentPartSizeRulings(exclusiveFlag int) map[string]string {
	merged := map[string]string{}
	for key, ruling := range messageFragmentPartSizeCopyRulings {
		merged[key] = ruling
	}
	for key, ruling := range messageFragmentPartSizePlatformCopyRulings {
		merged[key] = ruling
	}
	if exclusiveFlag == messageFragmentPartBytes {
		merged["memory_owner_census.go DeviceLocal.WriteMemoryOwnerCensus os.O_EXCL"] =
			"os.O_EXCL -- the exclusive-create flag in os.OpenFile's flag argument. It is 0x800 " +
				"on Darwin, so its value equals the part size there; on Linux and Windows it is " +
				"0x80 and is outside this gate's class. The census writer requires a fresh diagnostic " +
				"file, not a fragment byte budget. Only this expression is ruled: a literal or " +
				"arithmetic copy in the same method still requires its own ruling"
	}
	return merged
}

func messageFragmentPartSizeRulingKey(copied messageFragmentCopy, rulings map[string]string) (string, bool) {
	declaration := copied.file + " " + copied.where
	expression := declaration + " " + copied.text
	if _, ruled := rulings[expression]; ruled {
		return expression, true
	}
	_, ruled := rulings[declaration]
	return declaration, ruled
}

func TestPartSizeExclusiveFlagRulingTracksPlatformValue(t *testing.T) {
	copy := messageFragmentCopy{file: "memory_owner_census.go", where: "DeviceLocal.WriteMemoryOwnerCensus", text: "os.O_EXCL"}
	for _, test := range []struct {
		name string
		flag int
		want bool
	}{
		{"darwin-collision", 0x800, true},
		{"linux-windows-no-collision", 0x80, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rulings := messageFragmentPartSizeRulings(test.flag)
			key, ruled := messageFragmentPartSizeRulingKey(copy, rulings)
			if ruled != test.want {
				t.Fatalf("flag %#x: ruled=%t, want %t", test.flag, ruled, test.want)
			}
			if ruled && key != "memory_owner_census.go DeviceLocal.WriteMemoryOwnerCensus os.O_EXCL" {
				t.Fatalf("exclusive-create ruling covers more than its audited expression: %q", key)
			}
		})
	}
}

func TestPartSizeExclusiveFlagRulingDoesNotExcuseFragmentCopies(t *testing.T) {
	// Plant value-equivalent copies in the very method whose imported flag has
	// a ruling. Type-check the fixture so arithmetic mutations enter the same
	// evaluated-value class as the package-wide gate, without editing production.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "memory_owner_census.go", `package sdk
type DeviceLocal struct{}
func (*DeviceLocal) WriteMemoryOwnerCensus() {
	_ = 2048
	_ = 1 << 11
	_ = 2 * 1024
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, err := (&types.Config{}).Check("fragment-ruling-fixture", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	rulings := messageFragmentPartSizeRulings(messageFragmentPartBytes)
	copies := 0
	for expr, value := range info.Types {
		number, exact := messageFragmentIntValue(value.Value)
		if !exact || number != messageFragmentPartBytes {
			continue
		}
		copies++
		copied := messageFragmentCopy{file: "memory_owner_census.go",
			where: messageFragmentEnclosing(file, expr.Pos()), text: types.ExprString(expr)}
		if key, ruled := messageFragmentPartSizeRulingKey(copied, rulings); ruled {
			t.Errorf("fragment-size mutation %s was excused by %q", copied.text, key)
		}
	}
	if copies != 3 {
		t.Fatalf("measured %d planted fragment-size copies, want 3", copies)
	}
	for _, copied := range []messageFragmentCopy{
		{file: "memory_owner_census.go", where: "DeviceLocal.otherMethod", text: "os.O_EXCL"},
		{file: "other.go", where: "DeviceLocal.WriteMemoryOwnerCensus", text: "os.O_EXCL"},
	} {
		if key, ruled := messageFragmentPartSizeRulingKey(copied, rulings); ruled {
			t.Errorf("exclusive-create ruling escaped its audited declaration: %+v via %q", copied, key)
		}
	}
}

var messageFragmentPartSizeCopyRulings = map[string]string{
	"device_local_ioloop.go IoLoop.run": "MessagePoolGet(2048) — the buffer the !windows fd read loop " +
		"reads one packet into. It is a packet buffer and not a frame budget: it bounds a read from a " +
		"tun fd, nothing carries it to a MessageServerFragment, and it predates this binding. It is in " +
		"the class because the class is the VALUE, which is the property that makes the class worth " +
		"having, and it is excused by name here rather than by narrowing the scope back to three files.",

	// UPSTREAM sdk's, with the merge of urnetwork/sdk main (msgrepo ledger 277): five more values that
	// happen to equal the part size, ruled the same way rather than by narrowing the scope.
	"device_local.go providerLocalUserNatSettings": "connect.MemoryScaledCount(2048, 256) -- the provider's " +
		"local user-NAT UDP buffer global limit: a COUNT of buffers, scaled by device memory. Not a byte budget of any " +
		"kind, and nothing carries it to a MessageServerFragment",
	"mobile_memory_policy.go mobilePackQueueBudgetMaxByteCount": "2 * 1024 * 1024 -- 2 MiB, the mobile Pack " +
		"queue's byte budget. Its 2048 is the subexpression 2 * 1024 of a MiB product, not a part size",
	"mobile_memory_policy.go mobileReceiveQueueBudgetMaxByteCount": "2 * 1024 * 1024 -- 2 MiB, the mobile " +
		"receive queue's byte budget. The same MiB product for the same reason",
	"mobile_packet_pressure.go mobilePacketPressureMaxOutstandingByteCount": "512 * 2048 -- 1 MiB of outstanding " +
		"packet bytes, written as 512 packet slots of 2048. A packet-pressure gate, not a frame budget",
	"mobile_packet_pressure.go mobilePacketPressureH1AckMaxOutstandingByteCount": "2 * 1024 * 1024 -- 2 MiB, the " +
		"H1 acknowledgement allowance. The same MiB product",
}

// The declaration an expression sits inside, named the way a ruling names it:
// `Type.Method` for a method, the function's name for a function, and the first
// name of the value or type spec that holds it otherwise.
//
// Read off the file's own declaration list rather than tracked during a walk, so
// that the type-checked tree and the files this build does not compile get the
// same answer out of the same code.
func messageFragmentEnclosing(file *ast.File, pos token.Pos) string {
	if file == nil {
		return "(no parsed file)"
	}
	for _, decl := range file.Decls {
		if pos < decl.Pos() || decl.End() < pos {
			continue
		}
		switch each := decl.(type) {
		case *ast.FuncDecl:
			if each.Recv != nil && len(each.Recv.List) == 1 {
				return messageFragmentTypeName(each.Recv.List[0].Type) + "." + each.Name.Name
			}
			return each.Name.Name
		case *ast.GenDecl:
			for _, spec := range each.Specs {
				if pos < spec.Pos() || spec.End() < pos {
					continue
				}
				switch held := spec.(type) {
				case *ast.ValueSpec:
					if len(held.Names) != 0 {
						return held.Names[0].Name
					}
				case *ast.TypeSpec:
					return held.Name.Name
				}
			}
			return each.Tok.String()
		}
	}
	return "(file scope)"
}

// The name of a receiver's type, through the pointer and through the type
// parameters a generic receiver carries.
func messageFragmentTypeName(expr ast.Expr) string {
	for {
		switch each := expr.(type) {
		case *ast.StarExpr:
			expr = each.X
		case *ast.IndexExpr:
			expr = each.X
		case *ast.IndexListExpr:
			expr = each.X
		case *ast.Ident:
			return each.Name
		default:
			return types.ExprString(expr)
		}
	}
}

// The integer a constant value holds, or false when it does not hold one.
//
// constant.Compare requires both values to be of the same kind and does not
// answer "not equal" for a string against an integer, so the kind test is here
// and not at the call site.
func messageFragmentIntValue(value constant.Value) (int64, bool) {
	if value == nil || value.Kind() == constant.Unknown {
		return 0, false
	}
	asInt := constant.ToInt(value)
	if asInt.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(asInt)
}

// The integer an expression evaluates to WITHOUT a type checker: literals and
// the arithmetic over them, and nothing else.
//
// It exists only for the production files this build does not compile, where
// there is no Info.Types to read. It sees `1 << 11` and `2 * 1024`, which is
// the half of the class that a literal scan misses; it does not see a named
// constant, because nothing here resolves a name. Stated rather than implied:
// this is the weaker derivation, and the gate says which files got it.
func messageFragmentUntypedInt(expr ast.Expr) (int64, bool) {
	switch each := expr.(type) {
	case *ast.BasicLit:
		if each.Kind != token.INT {
			return 0, false
		}
		value := constant.MakeFromLiteral(each.Value, token.INT, 0)
		return messageFragmentIntValue(value)
	case *ast.ParenExpr:
		return messageFragmentUntypedInt(each.X)
	case *ast.UnaryExpr:
		inner, ok := messageFragmentUntypedInt(each.X)
		if !ok {
			return 0, false
		}
		switch each.Op {
		case token.ADD:
			return inner, true
		case token.SUB:
			return -inner, true
		case token.XOR:
			return ^inner, true
		}
		return 0, false
	case *ast.BinaryExpr:
		left, okLeft := messageFragmentUntypedInt(each.X)
		right, okRight := messageFragmentUntypedInt(each.Y)
		if !okLeft || !okRight {
			return 0, false
		}
		switch each.Op {
		case token.ADD:
			return left + right, true
		case token.SUB:
			return left - right, true
		case token.MUL:
			return left * right, true
		case token.QUO:
			if right == 0 {
				return 0, false
			}
			return left / right, true
		case token.REM:
			if right == 0 {
				return 0, false
			}
			return left % right, true
		case token.SHL:
			if right < 0 || 62 < right {
				return 0, false
			}
			return left << uint(right), true
		case token.SHR:
			if right < 0 || 62 < right {
				return 0, false
			}
			return left >> uint(right), true
		case token.AND:
			return left & right, true
		case token.OR:
			return left | right, true
		case token.XOR:
			return left ^ right, true
		}
		return 0, false
	}
	return 0, false
}

type messageFragmentConstSite struct {
	expr ast.Expr
	pos  string
	text string
}

func sitesOf(sites []messageFragmentConstSite) []string {
	shown := []string{}
	for _, site := range sites {
		shown = append(shown, fmt.Sprintf("%s %s", site.pos, site.text))
	}
	sort.Strings(shown)
	return shown
}

// ── the cut, and the budget that must reach it from one place ────────────────
//
// GATE CLASS, derived: every function in package sdk's production files that
//
//	CONSTRUCTS a protocol.MessageServerFragment, read off the
//	type-checked composite literal's own type rather than off a name.
//
// GATE SCOPE, derived separately: the whole package, for Property 3's reason.
//
// This is the half of Property 3 that Task 7's Property 4 is coupled to. The
// value gate above sees a second spelling of 2048; it CANNOT see a second
// budget that is a different number — a Hello cut at 1024 contains no constant
// equal to the part size and would sail past it. So the budget is narrowed from
// the other end: there is exactly one place that cuts, and inside it and at
// every call of it the only constant that could be a budget is the part size
// itself.
//
// Zero and one are the residue of index arithmetic — a loop that starts at
// zero, a ceiling that adds one less than the divisor, a part that ends at
// (index+1)*part — and neither is a byte budget. Every constant in the cut is
// printed with its value, so a third value arriving is visible in the log
// before it is visible in the failure.
func TestThereIsOnePlaceThatCutsAndOneBudgetThatReachesIt(t *testing.T) {
	checked := messageFragmentTypeCheck(t)
	object := checked.pkg.Scope().Lookup("messageFragmentPartBytes")
	if object == nil {
		t.Fatal("package sdk declares no messageFragmentPartBytes")
	}

	fragmentType := "github.com/urnetwork/connect/protocol.MessageServerFragment"
	protocolPrefix := "github.com/urnetwork/connect/protocol."
	cuts := []string{}
	decodeTargets := []string{}
	otherBuilders := []string{}
	byName := map[string]*ast.FuncDecl{}
	for _, file := range checked.files {
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Body == nil {
				continue
			}
			name := declKey(funcDecl)
			byName[name] = funcDecl
			fills := false
			allocates := false
			builds := false
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				composite, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				tv, found := checked.info.Types[composite]
				if !found || tv.Type == nil {
					return true
				}
				spelled := strings.TrimPrefix(tv.Type.String(), "*")
				if !strings.HasPrefix(spelled, protocolPrefix) {
					return true
				}
				builds = true
				if spelled != fragmentType {
					return true
				}
				// an EMPTY literal of the fragment type is a decode target --
				// somewhere for proto.Unmarshal to write -- and not a cut. A
				// literal that fills fields in is the cut, and `part` is one of
				// the fields it fills.
				if len(composite.Elts) == 0 {
					allocates = true
					return true
				}
				fills = true
				return true
			})
			switch {
			case fills:
				cuts = append(cuts, fmt.Sprintf("%s (%s)", name, checked.fset.Position(funcDecl.Pos())))
			case allocates:
				decodeTargets = append(decodeTargets, name)
			case builds:
				otherBuilders = append(otherBuilders, name)
			}
		}
	}
	sort.Strings(cuts)
	sort.Strings(decodeTargets)
	sort.Strings(otherBuilders)
	t.Logf("GATE CLASS: %d function(s) of package sdk construct a %s with its fields filled in: %v",
		len(cuts), fragmentType, cuts)
	t.Logf("COMPLEMENT — functions that allocate an EMPTY fragment as a decode target: %d %v",
		len(decodeTargets), decodeTargets)
	t.Logf("COMPLEMENT — functions that build some other connect/protocol message and no fragment: %d %v",
		len(otherBuilders), otherBuilders)
	if len(otherBuilders)+len(decodeTargets) == 0 {
		t.Fatalf("both complements are EMPTY: no function of package sdk builds a connect/protocol "+
			"message without cutting a fragment, so this gate is not distinguishing anything. "+
			"%d function(s) were walked", len(byName))
	}
	if len(cuts) != 1 {
		t.Fatalf("%d function(s) of package sdk cut §4.6 fragments: %v. Two cuts are two budgets, and "+
			"Task 7's Hello must use the same one this file declares", len(cuts), cuts)
	}
	cut := strings.SplitN(cuts[0], " ", 2)[0]

	// ── every constant inside the cut ───────────────────────────────────────
	//
	// Partitioned four ways, all four printed. A byte budget is a plain integer
	// count; §10.1's code points are constants too and are of a DEFINED type
	// (protocol.MessageType), which is what tells them apart from a number of
	// bytes without anybody writing 1000 and 1003 down here.
	inside := []string{}
	partSizeUses := []string{}
	defined := []string{}
	arithmetic := []string{}
	budgets := []string{}
	ast.Inspect(byName[cut].Body, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		tv, found := checked.info.Types[expr]
		if !found || tv.Value == nil {
			return true
		}
		shown := fmt.Sprintf("%s %s = %s (%s)",
			checked.fset.Position(expr.Pos()), types.ExprString(expr), tv.Value, tv.Type)
		inside = append(inside, shown)
		if ident, isIdent := expr.(*ast.Ident); isIdent && checked.info.Uses[ident] == object {
			partSizeUses = append(partSizeUses, shown)
			return true
		}
		if _, isNamed := tv.Type.(*types.Named); isNamed {
			// a constant of a defined type is not a byte count in this package:
			// the code points are protocol.MessageType and the timeouts are
			// time.Duration
			defined = append(defined, shown)
			return true
		}
		number, isInteger := messageFragmentIntValue(tv.Value)
		if isInteger && number <= 1 {
			arithmetic = append(arithmetic, shown)
			return true
		}
		budgets = append(budgets, shown)
		return true
	})
	sort.Strings(inside)
	t.Logf("every constant expression inside %s: %d", cut, len(inside))
	for _, each := range inside {
		t.Logf("    %s", each)
	}
	t.Logf("  the part size, by name: %d", len(partSizeUses))
	t.Logf("  COMPLEMENT — constants of a DEFINED type, which are not byte counts: %d %v", len(defined), defined)
	t.Logf("  COMPLEMENT — plain integers of 0 or 1, which are index arithmetic and not a budget: %d %v",
		len(arithmetic), arithmetic)
	t.Logf("  what is left, which could only be a second budget: %d %v", len(budgets), budgets)
	if len(inside) == 0 {
		t.Fatalf("%s holds no constant expression at all, not even a loop bound: this gate read nothing", cut)
	}
	if len(partSizeUses) == 0 {
		t.Fatalf("%s never names the part size, so whatever it cuts at, it is not the declared bound", cut)
	}
	if len(arithmetic) == 0 {
		t.Fatalf("the index-arithmetic complement is EMPTY: a cut with no 0 and no 1 in it is not "+
			"walking an index, so this partition has not looked at %s", cut)
	}
	if len(budgets) != 0 {
		t.Fatalf("%d plain integer constant(s) in %s are neither the part size nor index arithmetic: %v. "+
			"A byte budget is a plain integer count, so anything here that is one and is not the "+
			"declared part size is a second budget",
			len(budgets), cut, budgets)
	}

	// ── every constant handed to the cut at a call site ─────────────────────
	callSites := []string{}
	passed := []string{}
	for name, decl := range byName {
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			plain, isPlain := call.Fun.(*ast.Ident)
			calls := (isSelector && strings.HasSuffix(cut, "."+selector.Sel.Name)) ||
				(isPlain && plain.Name == cut)
			if !calls {
				return true
			}
			callSites = append(callSites, fmt.Sprintf("%s at %s", name, checked.fset.Position(call.Pos())))
			for _, arg := range call.Args {
				tv, found := checked.info.Types[arg]
				if !found || tv.Value == nil {
					continue
				}
				if ident, isIdent := arg.(*ast.Ident); isIdent && checked.info.Uses[ident] == object {
					continue
				}
				passed = append(passed, fmt.Sprintf("%s %s = %s",
					checked.fset.Position(arg.Pos()), types.ExprString(arg), tv.Value))
			}
			return true
		})
	}
	sort.Strings(callSites)
	t.Logf("call sites of %s: %d %v; constant arguments at them that are not the part size: %d %v",
		cut, len(callSites), callSites, len(passed), passed)
	if len(callSites) == 0 {
		t.Fatalf("nothing in package sdk calls %s, so the cut is dead code and this gate measures nothing", cut)
	}
	if len(passed) != 0 {
		t.Fatalf("%d constant(s) are handed to %s that are not the part size: %v. A budget passed in is a "+
			"budget that can differ from the declared one, which is exactly what Task 7's Hello must not do",
			len(passed), cut, passed)
	}
}

// ── the type checker ─────────────────────────────────────────────────────────

type messageFragmentChecked struct {
	fset     *token.FileSet
	files    []*ast.File
	names    []string
	excluded []string
	info     *types.Info
	pkg      *types.Package
}

// Package sdk, type-checked from source against the compiler's own export data
// for its dependencies.
//
// The dependency export files come from `go list -export -deps`, which is the
// build cache the compiler just wrote; go/importer reads them directly. The
// alternative, type-checking every dependency from source, would walk gvisor,
// quic-go and pion on every run.
//
// The file set is go/build's answer to each file's build constraints, not a
// glob: package sdk has js, ios, android and windows variants that redeclare
// each other, and type-checking all of them at once produces a tree full of
// "redeclared in this block" where a constant's value is anybody's guess. It
// FAILS CLOSED on a type-check error, because the gates above read
// Info.Types[expr].Value and an expression the checker gave up on is an
// expression with no value — which is silence that looks exactly like absence.
func messageFragmentTypeCheck(t *testing.T) messageFragmentChecked {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("could not read the build cache's export data, so package sdk cannot be type-checked: %v", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		exports[parts[0]] = parts[1]
	}
	if len(exports) == 0 {
		t.Fatal("go list -export named no export file for any dependency of package sdk")
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("could not list the package's files: %v", err)
	}
	checked := messageFragmentChecked{fset: token.NewFileSet()}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		compiled, err := build.Default.MatchFile(".", path)
		if err != nil {
			t.Fatalf("go/build could not answer whether this build compiles %s: %v", path, err)
		}
		if !compiled {
			checked.excluded = append(checked.excluded, path)
			continue
		}
		file, err := parser.ParseFile(checked.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("could not parse %s: %v", path, err)
		}
		checked.files = append(checked.files, file)
		checked.names = append(checked.names, path)
	}
	if len(checked.files) == 0 {
		t.Fatal("go/build says this build compiles no production file of package sdk")
	}

	problems := []string{}
	checked.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	config := &types.Config{
		Importer: importer.ForCompiler(checked.fset, "gc", func(path string) (io.ReadCloser, error) {
			file, found := exports[path]
			if !found {
				return nil, os.ErrNotExist
			}
			return os.Open(file)
		}),
		Error: func(err error) { problems = append(problems, err.Error()) },
	}
	pkg, err := config.Check("github.com/urnetwork/sdk", checked.fset, checked.files, checked.info)
	if len(problems) != 0 {
		t.Fatalf("package sdk did not type-check cleanly (%d problem(s)); every gate that reads a "+
			"constant VALUE out of this tree would be reading silence where the checker gave up: %v",
			len(problems), problems)
	}
	if err != nil {
		t.Fatalf("package sdk did not type-check: %v", err)
	}
	checked.pkg = pkg
	return checked
}
