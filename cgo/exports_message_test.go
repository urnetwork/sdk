package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unsafe"

	"github.com/urnetwork/sdk/v2026/urmessage"
)

// THE GO-LEVEL HALF, AND IT IS THE SMALLER HALF ON PURPOSE.
//
// ctest/message_abi_test.c is the real test of this binding: it drives the abi from C, through a
// real message server, with real handles, real malloc'd strings and a real second OS thread, and
// it is where "a C program can send a message and read it back" is answered. What is HERE is the
// part that C test cannot reach on this host:
//
//   - THE RACE DETECTOR. `go build -race -buildmode=c-shared` produces a dll, and on Windows
//     ThreadSanitizer cannot map its shadow memory when that dll is loaded into an already
//     running process: the C consumer dies before main with "ThreadSanitizer failed to allocate
//     0x6920000 bytes ... (error code: 87)". So `go test -race` over the same registry and the
//     same context handles is what holds the concurrency property on this host, and
//     ctest/run.sh prints that where it tries.
//   - THE COMPLEMENT OF THE BODY DECISION. The C test shows octets surviving. This shows what
//     they would NOT have survived, which is the half that says the decision is load bearing
//     rather than decorative.
//
// NOTE FOR ANYONE EXTENDING THIS FILE: it must not `import "C"`. Go does not support cgo in a
// _test.go file ("use of cgo in test ... not supported"), so nothing here may NAME a C type. The
// exported functions are still callable -- their results carry the C types without being spelled.

// urnet_message_context_new and urnet_release are the two halves of a handle's life, and a ui
// that polls a conversation on one thread while another closes the app runs them at once.
//
// THE ASSERTION IS THE COUNT, NOT THE ABSENCE OF A CRASH. A registry that lost an entry under a
// race leaves the count low and one that double-inserted leaves it high; both are silent without
// this, and both would show up in a real app as handles that outlive their objects.
func TestTheHandleCountComesBackUnderConcurrentCreateCancelAndRelease(t *testing.T) {
	before := handleCount()
	const writers = 16
	const each = 250
	var wait sync.WaitGroup
	wait.Add(writers)
	for at := 0; at < writers; at += 1 {
		go func() {
			defer wait.Done()
			for n := 0; n < each; n += 1 {
				handle := urnet_message_context_new()
				if handle == 0 {
					t.Error("context_new answered 0")
					return
				}
				urnet_message_context_cancel(handle)
				// cancel is idempotent: a ui that closes twice must not be a panic
				urnet_message_context_cancel(handle)
				if !urnet_release(handle) {
					t.Error("releasing a live handle answered false")
					return
				}
				if urnet_release(handle) {
					t.Errorf("releasing handle %d twice answered true both times", uint64(handle))
					return
				}
			}
		}()
	}
	wait.Wait()
	if after := handleCount(); after != before {
		t.Errorf("%d handles leaked across %d create/cancel/release cycles (%d -> %d)",
			after-before, writers*each, before, after)
	}
}

// A handle id is never reused, so a stale handle a C caller kept can never resolve to a NEW
// object. It is the property that makes a bare uint64_t safe to hold: without it, releasing a
// group and then calling _group_send on the old value could reach some other group entirely.
func TestAReleasedHandleIsNeverHandedOutAgain(t *testing.T) {
	seen := map[uint64]bool{}
	var last uint64
	for n := 0; n < 2000; n += 1 {
		handle := urnet_message_context_new()
		if seen[uint64(handle)] {
			t.Fatalf("handle %d was issued twice", uint64(handle))
		}
		seen[uint64(handle)] = true
		last = uint64(handle)
		urnet_message_context_cancel(handle)
		urnet_release(handle)
	}
	if _, ok := handleValue(last); ok {
		t.Fatalf("released handle %d still resolves to an object", last)
	}
}

// messageCtx is what every blocking export calls, and the two answers it must keep apart are
// "the caller passed nothing, so this call is uncancellable" and "the caller passed a handle
// that is not one of ours". The second must be REFUSED: a call that silently ran on
// context.Background() while its caller believed it held a cancel handle is a call that cannot
// be stopped, which is the whole failure the handle exists to prevent.
func TestAnUnknownContextHandleIsRefusedRatherThanDowngradedToBackground(t *testing.T) {
	ctx, ok := messageCtx(0, "test")
	if !ok || ctx == nil {
		t.Error("a zero context handle was refused; it is the documented uncancellable call")
	} else if ctx != context.Background() {
		t.Error("a zero context handle did not answer context.Background()")
	}
	// an id the registry has never issued. 1<<63 is far above any id this process allocates.
	if _, ok := messageCtx(1<<63, "test"); ok {
		t.Error("an unknown context handle was accepted, and the call it came from would have run " +
			"uncancellable while its caller believed it could be stopped")
	}
	// an id that WAS ours and has been released
	handle := urnet_message_context_new()
	urnet_release(handle)
	if _, ok := messageCtx(handle, "test"); ok {
		t.Error("a released context handle was accepted")
	}
}

// Cancelling from one goroutine while others hold the context is the shape of "the app is
// closing while a poll is in flight", which is what the C test does across an OS thread.
func TestCancelIsSeenByEveryHolderOfTheContext(t *testing.T) {
	handle := urnet_message_context_new()
	defer urnet_release(handle)
	ctx, ok := messageCtx(handle, "test")
	if !ok {
		t.Fatal("a context this test just created did not resolve")
	}
	const holders = 32
	var wait sync.WaitGroup
	wait.Add(holders)
	for at := 0; at < holders; at += 1 {
		go func() {
			defer wait.Done()
			<-ctx.Done()
		}()
	}
	go urnet_message_context_cancel(handle)
	// a holder that is never woken hangs here rather than failing, and the panic the test
	// timeout prints names every stuck goroutine, which is the more useful report
	wait.Wait()
	if ctx.Err() == nil {
		t.Error("the context reports no error after being cancelled")
	}
}

// ── the complement of the body decision ─────────────────────────────────────────────────────

// THE BODY THAT WOULD NOT HAVE SURVIVED. exports_message.go decides that a body crosses as
// counted octets and never as a char* and never inside json. This measures what each REJECTED
// alternative would have done to these octets, so that the decision is defended by a number rather
// than by the paragraph above it.
//
// THESE 21 OCTETS USED TO BE ctest/message_abi_test.c's kBody, CHARACTER FOR CHARACTER, AND THEY
// ARE NO LONGER. Since the content envelope a TEXT tail is checked for valid UTF-8 before it is
// sealed, so the C test cannot SEND 0xFF 0xFE or the ill-formed 0xC3 0x28 -- Group.Send refuses
// them, and that is what had made ctest red at sdk bd4672d, at its first send. kBody there is now
// valid UTF-8 with the two NULs kept, because the NUL is the half a real seal can still carry.
//
// SO THE JSON HALF LIVES HERE, AND ONLY HERE. This case seals nothing and can therefore still hold
// the octets that would be corrupted, which is the whole reason it is not deleted along with them:
// a body from a kind this build does not know is not checked for UTF-8 by anything (it is not
// text, and there is no tail to check), so json remains the wrong carrier for a body even though
// no send path can now produce an ill-formed one.
func TestTheRejectedBodyEncodingsWouldHaveChangedTheseOctets(t *testing.T) {
	body := []byte("hello from C\x00\xff\xfex\xc3\x28\n\x00z")
	if len(body) != 21 {
		t.Fatalf("this case's body is %d octets and both halves of it are written for 21", len(body))
	}

	// (1) as a char*, which is NUL terminated: a C caller reading it stops at the first 0x00.
	truncated := body
	for at, octet := range body {
		if octet == 0 {
			truncated = body[:at]
			break
		}
	}
	if len(truncated) == len(body) {
		t.Fatal("this body has no NUL in it, so the char* case proves nothing")
	}
	t.Logf("as a char*: %d octets of %d, losing %d", len(truncated), len(body), len(body)-len(truncated))

	// (2) inside json, which is how every other data type in this abi crosses.
	encoded, err := json.Marshal(map[string]string{"text": string(body)})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if back["text"] == string(body) {
		t.Error("json round-tripped these octets unchanged, so the no-json rule defends nothing " +
			"and this body needs ill-formed utf-8 in it")
	}
	t.Logf("through json: %d octets of %d, carrying %d replacement characters",
		len(back["text"]), len(body), strings.Count(back["text"], "�"))

	// The abi's own answer -- copyOut over these same octets -- is measured in C, where the
	// pointer and the length are real: ctest/message_abi_test.c compares all 21 one at a time.
}

// The metadata projection carries no body at all, which is what keeps (2) above from happening
// by accident the day somebody adds a field to it.
func TestTheMessageMetadataProjectionCarriesNoBody(t *testing.T) {
	message := &urmessage.Message{
		RecordId:     7,
		SenderHandle: []byte{0x00, 0x11, 0xAB, 0xFF, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		Mine:         true,
		Text:         "a body that must not appear anywhere in this json \x00\xff",
		SentAtMs:     1234,
	}
	encoded, err := json.Marshal(messageInfoOf(messageEntryOf(message)))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(encoded), "a body that must not appear") {
		t.Errorf("the metadata json carries the body: %s", encoded)
	}
	for _, want := range []string{
		`"record_id":7`,
		`"sender_handle":"0011abff0102030405060708090a0b0c"`,
		`"mine":true`,
		`"sent_at_ms":1234`,
		`"body_len":52`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the metadata json is missing %s: %s", want, encoded)
		}
	}
	if messageInfoOf(messageEntryOf(nil)) != nil {
		t.Error("a nil message projected to something")
	}
}

// ── the projection that lost five fields, and the gate that makes the next one loud ─────────

// messageInfoExempt is every field of urmessage.Message that the metadata json deliberately does
// NOT carry, with the reason and with where it crosses instead. It is the gate's exemption list and
// it is printed on every run: an exemption nobody reads is an exemption that outlives its reason.
var messageInfoExempt = map[string]string{
	"Text": "the BODY. it is arbitrary octets from another device and json would replace every " +
		"ill-formed one with U+FFFD, so it crosses through urnet_message_list_body and never " +
		"here. body_len is what the json says about it.",
	"Reactions": "a per-message COLLECTION with no cap on it, so it crosses through " +
		"urnet_message_list_reaction_count and _reaction_info rather than as an array that " +
		"would make one row's metadata unbounded. reaction_count is what the json says about it.",
}

// EVERY FIELD urmessage.Message CARRIES REACHES A C CALLER, OR IS EXEMPT BY NAME WITH A REASON.
//
// THIS IS THE GATE FOR THE DEFECT ITSELF AND NOT FOR ONE INSTANCE OF IT. messageInfo is a HAND
// projection -- exports_message.go says why -- so a field added to urmessage.Message arrives at a C
// caller as NOTHING AT ALL unless somebody remembers to add it here. Kind, ReplyToId, Deleted,
// Reactions and then Gap all landed that way and all reached C as nothing, with every test in this
// package green (msgrepo ledger item 236). This is what makes the next one red.
//
// IT IS TWO CHECKS AND THE SECOND IS THE ONE THAT MATTERS. A field that exists on messageInfo and
// is never ASSIGNED is exactly as invisible as one that does not exist, so the fixture below sets
// every field of urmessage.Message to a non-zero value and every field of the projection must come
// out non-zero. Declaring the field is not enough; filling it is the property.
//
// WHAT WOULD GO RED: add a field to urmessage.Message and not to messageInfo; declare one in
// messageInfo and forget to assign it in messageInfoOf; exempt a field that no longer exists.
func TestTheMessageInfoCarriesEveryFieldUrmessageKeeps(t *testing.T) {
	// the fixture: every field of urmessage.Message non-zero, so that "carried" can mean
	// "arrived with a value" rather than "was declared"
	message := &urmessage.Message{
		RecordId:     7,
		SenderHandle: []byte{0x00, 0x11, 0xAB, 0xFF, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		// AND AN IDENTITY THAT IS NOT THE HANDLE, deliberately: the two are the field pair
		// msgrepo ledger item 245 exists to keep apart, and a fixture that gave them the same
		// octets would pass a projection that carried the handle twice.
		SenderIdentity: bytes.Repeat([]byte{0x9E}, 32),
		Mine:           true,
		// "observer" and not "member", because it is the one value of this field a caller must
		// ACT on: a projection that carried the field and answered "" for it would pass the
		// non-zero check with any other role in the fixture and would hide every observer.
		SenderRoleAtSend: "observer",
		Text:             "a body",
		SentAtMs:         1234,
		MessageId:        bytes.Repeat([]byte{0x5A}, 32),
		Kind:             urmessage.KindReply,
		Gap:              urmessage.GapUnsupported,
		ReplyToId:        bytes.Repeat([]byte{0xC3}, 32),
		Deleted:          true,
		Reactions:        []urmessage.Reaction{{SenderHandle: []byte{0x01}, Emoji: "x", Mine: true}},
	}

	kept := reflect.TypeOf(urmessage.Message{})
	fixture := reflect.ValueOf(*message)
	for at := 0; at < kept.NumField(); at += 1 {
		if fixture.Field(at).IsZero() {
			t.Fatalf("this case's fixture leaves urmessage.Message.%s at its zero value, so "+
				"nothing below can tell a field that is carried from one that is dropped",
				kept.Field(at).Name)
		}
	}

	// every field of the projection arrived with a value
	info := messageInfoOf(messageEntryOf(message))
	if info == nil {
		t.Fatal("the projection answered nil for a message")
	}
	carried := reflect.TypeOf(messageInfo{})
	projected := reflect.ValueOf(*info)
	for at := 0; at < carried.NumField(); at += 1 {
		if projected.Field(at).IsZero() {
			t.Errorf("messageInfo.%s (json %q) is declared and never assigned, which reaches a C "+
				"caller as exactly the same nothing as not existing",
				carried.Field(at).Name, carried.Field(at).Tag.Get("json"))
		}
	}

	// and every field urmessage keeps is either one of those or exempt by name
	names := map[string]bool{}
	for at := 0; at < carried.NumField(); at += 1 {
		names[carried.Field(at).Name] = true
	}
	exempted := 0
	for at := 0; at < kept.NumField(); at += 1 {
		name := kept.Field(at).Name
		if names[name] {
			continue
		}
		why, allowed := messageInfoExempt[name]
		if !allowed {
			t.Errorf("urmessage.Message keeps %s and urnet_message_list_info does not carry it; "+
				"a C caller cannot see it at all", name)
			continue
		}
		exempted += 1
		t.Logf("EXEMPT %s: %s", name, why)
	}
	// AND AN EXEMPTION THAT OUTLIVED ITS USE IS A FAILURE. An exemption for a field urmessage no
	// longer has, or for one the projection has since started carrying, is a hole standing open
	// over nothing -- and it is silent, because an exemption reports nothing by construction.
	for name := range messageInfoExempt {
		if _, kept := kept.FieldByName(name); !kept {
			t.Errorf("%s is exempt from this gate and urmessage.Message no longer has a field by "+
				"that name; the exemption has outlived what it was for", name)
		}
		if names[name] {
			t.Errorf("%s is exempt from this gate and messageInfo carries it anyway; the "+
				"exemption is stale and hides the next field that goes missing", name)
		}
	}
	if exempted != len(messageInfoExempt) {
		t.Errorf("the gate used %d exemptions of the %d declared", exempted, len(messageInfoExempt))
	}
	t.Logf("%d of urmessage.Message's %d fields cross in the info json, %d by other exports",
		kept.NumField()-exempted, kept.NumField(), exempted)
}

// A GAP IS NOT A MESSAGE WITH NO TEXT, AND THAT IS THE WHOLE OF LEDGER ITEM 236.
//
// Both are body_len 0. Before the gap field a C caller had no other difference to read: an
// unsupported record and a line somebody sent nothing on projected to the same json, and a UI that
// drew a blank line for the first was telling a user that a member had said nothing when in fact
// something was there that this build could not show.
//
// WHAT WOULD GO RED: drop Gap from messageInfo, or project it as "" -- a gap reported as a normal
// message, which is the mutation this case exists for.
func TestAGapIsDistinguishableFromAMessageWithNoText(t *testing.T) {
	blank := &urmessage.Message{RecordId: 4, Kind: urmessage.KindText}
	// A MALFORMED REPLY IS THE HARD CASE AND THAT IS WHY IT IS THE ONE HERE: Kind is the code the
	// record ARRIVED under and not what the record is, so this gap carries KindReply. A caller
	// that branched on kind would render it as a reply with nothing in it.
	gap := &urmessage.Message{RecordId: 5, Kind: urmessage.KindReply, Gap: urmessage.GapMalformed}

	blankInfo := messageInfoOf(messageEntryOf(blank))
	gapInfo := messageInfoOf(messageEntryOf(gap))
	if blankInfo.BodyLen != gapInfo.BodyLen {
		t.Fatalf("the two differ by body_len alone (%d and %d), so this case is not measuring what "+
			"it says it is", blankInfo.BodyLen, gapInfo.BodyLen)
	}
	if blankInfo.Gap != "" {
		t.Errorf("a message that is a message reports gap %q", blankInfo.Gap)
	}
	if gapInfo.Gap != string(urmessage.GapMalformed) {
		t.Errorf("a malformed record reports gap %q, want %q", gapInfo.Gap, urmessage.GapMalformed)
	}
	// and the two vocabularies do not collide: "malformed" and "unsupported" are different
	// sentences to a user, and one of them offers an upgrade that the other must not
	unsupported := messageInfoOf(messageEntryOf(&urmessage.Message{Gap: urmessage.GapUnsupported}))
	if unsupported.Gap == gapInfo.Gap {
		t.Errorf("both gap reasons project to %q", unsupported.Gap)
	}
	encoded, err := json.Marshal(gapInfo)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if !strings.Contains(string(encoded), `"gap":"malformed"`) {
		t.Errorf("the gap json is %s", encoded)
	}
	if !strings.Contains(string(encoded), `"kind":2`) {
		t.Errorf("the gap does not carry the code it arrived under: %s", encoded)
	}
}

// ── a list handle is ONE INSTANT of the conversation ────────────────────────────────────────

// callExport drives an exported function with values this package minted.
//
// IT IS reflect RATHER THAN A CALL because this file must not `import "C"` -- go does not support
// cgo in a _test.go file -- so C.uint64_t and C.int32_t cannot be NAMED here and a plain uint64
// handle cannot be passed to an export that declares one. Converting through the parameter types
// the function itself declares needs no name at all, and it is what lets the cases below drive the
// REAL exports rather than the unexported machinery under them.
func callExport(t *testing.T, fn any, args ...any) []reflect.Value {
	t.Helper()
	value := reflect.ValueOf(fn)
	shape := value.Type()
	if shape.NumIn() != len(args) {
		t.Fatalf("the export takes %d arguments and %d were passed", shape.NumIn(), len(args))
	}
	in := make([]reflect.Value, 0, len(args))
	for at, arg := range args {
		in = append(in, reflect.ValueOf(arg).Convert(shape.In(at)))
	}
	return value.Call(in)
}

func listReactionCount(t *testing.T, list uint64, index int) int {
	t.Helper()
	return int(callExport(t, urnet_message_list_reaction_count, list, index)[0].Int())
}

// A LIST HANDLE ANSWERS FROM ONE INSTANT, AND urmessage REWRITES A MESSAGE AFTER IT IS IN THE LOG.
//
// Group.Messages says "the messages themselves are shared and are not written after they are
// appended" and that sentence is FALSE for two fields: reapplyLocked sets Deleted and rebuilds
// Reactions on a message that is already in the log, every time a reaction or a tombstone for it
// arrives. This abi is polled from one thread and rendered from another -- the threading decision
// at the top of exports_message.go is exactly that -- so a list handle that read those two live
// would answer _reaction_count and _reaction_info from two different instants, and a C caller
// looping `for k in 0..count` would read past the end of a list that had just shrunk.
//
// The mutation below is a later Receive, written as the in-place rewrite it is.
//
// WHAT WOULD GO RED: have newMessageList keep the *Message and have the accessors read
// entry.message.Reactions and entry.message.Deleted.
func TestAListHandleAnswersFromOneInstantOfTheConversation(t *testing.T) {
	message := &urmessage.Message{
		RecordId:     9,
		SenderHandle: bytes.Repeat([]byte{0x11}, 16),
		Text:         "a line two people reacted to",
		MessageId:    bytes.Repeat([]byte{0x22}, 32),
		Kind:         urmessage.KindText,
		Reactions: []urmessage.Reaction{
			{SenderHandle: bytes.Repeat([]byte{0xAA}, 16), Emoji: "👍"},
		},
	}
	list := newMessageList([]*urmessage.Message{message})
	if list == 0 {
		t.Fatal("a one message list answered handle 0")
	}
	defer handleRelease(list)

	if got := listReactionCount(t, list, 0); got != 1 {
		t.Fatalf("the list answered %d reactions, want 1", got)
	}
	before := messageInfoOf(messageEntryOf(message))
	// the array the message held at that instant, kept for the copy check at the end: the append
	// below reallocates, so after it the message no longer points at this one
	original := message.Reactions

	// a later Receive: one more reaction, and the sender's own tombstone, both written IN PLACE
	// onto a message that is already in the log
	message.Reactions = append(message.Reactions, urmessage.Reaction{
		SenderHandle: bytes.Repeat([]byte{0xBB}, 16), Emoji: "🎯", Mine: true,
	})
	message.Deleted = true

	if got := listReactionCount(t, list, 0); got != 1 {
		t.Errorf("a reaction that landed after the list was built changed it to %d; the count and "+
			"the info a caller has already read would disagree with each other", got)
	}
	held, ok := handleValue(list)
	if !ok {
		t.Fatal("the list handle stopped resolving")
	}
	after := messageInfoOf(held.(*messageList).entries[0])
	if after.ReactionCount != before.ReactionCount || after.Deleted != before.Deleted {
		t.Errorf("the list's own info moved under it: reaction_count %d -> %d, deleted %v -> %v",
			before.ReactionCount, after.ReactionCount, before.Deleted, after.Deleted)
	}
	// AND THE SNAPSHOT IS A COPY OF THE ARRAY AND NOT A RESLICE OF IT, which is a guard rather
	// than a repair and is labelled as one. urmessage REBUILDS the whole slice today --
	// reapplyLocked clears Reactions and re-appends, and the REMOVE arm makes a new slice -- so
	// nothing it does now can be seen through a shared array, and the append above cannot see it
	// either, because appending past cap reallocates and leaves the old array alone. The only
	// thing that can observe the difference is a write INTO an element, which is why the line
	// below writes into the array the message held when the snapshot was taken. Without it,
	// dropping the copy has no killer at all.
	standing := held.(*messageList).entries[0].reactions
	original[0].Emoji = "a rebuild that wrote in place"
	if standing[0].Emoji != "👍" {
		t.Errorf("the snapshot shares its array with the message: reaction 0 is now %q", standing[0].Emoji)
	}

	// the bounds, which C cannot tell apart from a refusal because cgoGuard recovers a panic
	for _, one := range []struct{ index, reaction int }{{-1, 0}, {1, 0}, {0, -1}, {0, 5}} {
		if got := callExport(t, urnet_message_list_reaction_info, list, one.index, one.reaction)[0]; !got.IsNil() {
			t.Errorf("message %d reaction %d answered something", one.index, one.reaction)
		}
	}
	if got := listReactionCount(t, list, 3); got != 0 {
		t.Errorf("reaction_count at index 3 of a 1 message list answered %d", got)
	}
	if got := listReactionCount(t, 0, 0); got != 0 {
		t.Errorf("reaction_count on handle 0 answered %d", got)
	}
}

// THE EMOJI MAY CROSS INSIDE JSON AND A BODY MAY NOT, AND THE DIFFERENCE IS ONE VALIDATION.
//
// The body decision at the top of exports_message.go bans json for octets that arrive from another
// device, because encoding/json replaces every ill-formed byte with U+FFFD in silence. A reaction's
// emoji arrives from another device too, and it rides inside json anyway -- the premise being that
// urmessage's checkEmoji requires valid UTF-8 of 1..MaxEmojiOctets octets on BOTH paths into a
// Reaction. This measures both halves of that premise so it is a number rather than a sentence: what
// a valid emoji does through the projection, and what one checkEmoji would have refused would have
// done. The second half is the one that says the validation is load-bearing.
//
// WHAT WOULD GO RED: nothing here, if checkEmoji stops requiring valid UTF-8 -- that is urmessage's
// gate to keep. What goes red here is the emoji being carried as anything other than its own octets.
func TestTheEmojiSurvivesJsonAndAnIllFormedOneWouldNot(t *testing.T) {
	// the last of these is the one a char* would have lost: U+0000 IS valid utf-8, so checkEmoji
	// accepts it, and json carries it as an escape while a NUL-terminated string would have handed
	// back one octet of a three octet emoji with no error raised anywhere
	for _, emoji := range []string{"👍", "👨‍👩‍👧", "❤️", "a\x00b"} {
		encoded, err := json.Marshal(reactionInfoOf(urmessage.Reaction{
			SenderHandle: bytes.Repeat([]byte{0x7F}, 16), Emoji: emoji, Mine: true,
		}))
		if err != nil {
			t.Fatalf("marshalling %q: %v", emoji, err)
		}
		var back messageReactionInfo
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("unmarshalling %q: %v", emoji, err)
		}
		if back.Emoji != emoji {
			t.Errorf("the emoji %q came back as %q", emoji, back.Emoji)
		}
		if !back.Mine || back.SenderHandle != strings.Repeat("7f", 16) {
			t.Errorf("the rest of the reaction came back as %+v", back)
		}
		if len(emoji) > urmessage.MaxEmojiOctets {
			t.Errorf("this case's emoji %q is %d octets and checkEmoji caps one at %d",
				emoji, len(emoji), urmessage.MaxEmojiOctets)
		}
	}

	// THE COMPLEMENT. An emoji checkEmoji would have refused does NOT survive, which is what makes
	// the validation the reason this field may be json at all rather than a coincidence.
	illFormed := "\xff\xfe\xc3\x28"
	encoded, err := json.Marshal(reactionInfoOf(urmessage.Reaction{Emoji: illFormed}))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var back messageReactionInfo
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if back.Emoji == illFormed {
		t.Error("json round-tripped ill-formed utf-8 unchanged, so the checkEmoji premise defends " +
			"nothing and this case measures nothing")
	}
	t.Logf("ill-formed: %d octets in, %d out, %d replacement characters -- which is what the emoji "+
		"would do here if checkEmoji ever stopped requiring valid utf-8",
		len(illFormed), len(back.Emoji), strings.Count(back.Emoji, "�"))
}

// PROTOCOL_VERSION: 0 IS THIS BUILD'S VERSION, THE BUILD'S VERSION IS ITSELF, AND NOTHING ELSE IS
// ACCEPTED.
//
// The review that found this passed 0 by analogy with every other 0 in this abi and lost a whole
// conversation to a Hello the server refused two calls later. The C consumer holds the export end to
// end (2 and 3 refused at transport_new, 0 connecting); this holds the whole range of the rule at
// its edges, which C would need a server per value to reach.
//
// WHAT WOULD GO RED: pass protocol_version straight through again, or map 0 to itself.
func TestProtocolVersionZeroIsThisBuildsAndEveryOtherValueIsRefused(t *testing.T) {
	for _, one := range []struct {
		in   uint32
		want uint32
		ok   bool
	}{
		{0, messageProtocolVersion, true},
		{messageProtocolVersion, messageProtocolVersion, true},
		{messageProtocolVersion + 1, 0, false},
		{messageProtocolVersion + 2, 0, false},
		{^uint32(0), 0, false},
	} {
		got, err := messageProtocolVersionOf(one.in)
		if (err == nil) != one.ok || got != one.want {
			t.Errorf("protocol_version %d answered %d, %v; want %d and ok=%v", one.in, got, err, one.want, one.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "URNET_MESSAGE_PROTOCOL_VERSION") {
			t.Errorf("the refusal of %d does not name the constant that works: %v", one.in, err)
		}
	}
}

// THE STATS JSON CARRIES EVERY COUNTER urmessage KEEPS, BY NAME.
//
// urnet_message_group_stats's own comment says "every counter urmessage keeps is carried", and until
// this case that sentence was held by nobody: a counter added to urmessage.Stats and not to the json
// projection would leave a C caller unable to see it with every test green. OwnWithoutCopy is the
// counter that made it matter -- the number of this device's own lines it cannot show.
//
// WHAT WOULD GO RED: add a field to urmessage.Stats and not to messageGroupStats.
func TestTheStatsJsonCarriesEveryCounterUrmessageKeeps(t *testing.T) {
	kept := reflect.TypeOf(urmessage.Stats{})
	carried := reflect.TypeOf(messageGroupStats{})
	names := map[string]bool{}
	for at := 0; at < carried.NumField(); at += 1 {
		names[carried.Field(at).Name] = true
	}
	for at := 0; at < kept.NumField(); at += 1 {
		if name := kept.Field(at).Name; !names[name] {
			t.Errorf("urmessage.Stats keeps %s and urnet_message_group_stats does not carry it", name)
		}
	}
	if kept.NumField() != carried.NumField() {
		t.Errorf("urmessage.Stats has %d counters and the json carries %d", kept.NumField(), carried.NumField())
	}
}

// THE HEADER'S DOCUMENTED STATS KEY LIST IS THE JSON'S OWN, IN ITS ORDER.
//
// include/urnetwork_message.h documents urnet_message_group_stats's keys in the comment above its
// declaration, and that list is what a C caller reads before it reads anything else; the case
// above holds the json against urmessage.Stats and nothing held the header against the json. It
// drifted: four counters (gap_out_of_window, opened_past_epoch, ingested, commit_refused) reached
// the json with every test green and the header never named them. So the list is READ OFF THE
// HEADER FILE -- the text after "as json:" up to its full stop, split on commas -- and held equal,
// entry for entry and in order, to the keys the json actually carries: the marshalled
// messageGroupStats, walked with a decoder so the keys are the output's and not a tag's.
//
// THE POSITIVE CONTROL is that the header's list is found and non-trivial: a header that lost
// the "as json:" phrase, or whose list came back empty, fails here rather than passing on two
// empty lists.
//
// WHAT WOULD GO RED: add a counter to messageGroupStats and not to the header's list, drop one
// from the header, misspell one, or reorder the header against the struct.
func TestTheHeaderDocumentsExactlyTheStatsJsonKeys(t *testing.T) {
	header, err := os.ReadFile("include/urnetwork_message.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	text := strings.ReplaceAll(string(header), "\r\n", "\n")
	declaration := "char* urnet_message_group_stats("
	at := strings.Index(text, declaration)
	if at < 0 {
		t.Fatalf("the header declares no %s", declaration)
	}
	comment := strings.LastIndex(text[:at], "/*")
	if comment < 0 {
		t.Fatal("the stats declaration has no comment block above it")
	}
	block := text[comment:at]
	phrase := "as json:"
	start := strings.Index(block, phrase)
	if start < 0 {
		t.Fatalf("the stats comment does not say %q; the documented key list is found by that phrase", phrase)
	}
	rest := block[start+len(phrase):]
	end := strings.Index(rest, ".")
	if end < 0 {
		t.Fatal("the documented key list has no full stop ending it")
	}
	documented := []string{}
	for _, entry := range strings.Split(rest[:end], ",") {
		entry = strings.TrimSpace(entry)
		entry = strings.TrimSpace(strings.TrimPrefix(entry, "*"))
		if entry != "" {
			documented = append(documented, entry)
		}
	}
	if len(documented) < 2 {
		t.Fatalf("the documented key list is %v; the positive control that the header carries a list is not met", documented)
	}

	encoded, err := json.Marshal(&messageGroupStats{})
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	carried := []string{}
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		t.Fatalf("the stats json does not open an object: %v %v", open, err)
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name, isString := key.(string)
		if !isString {
			t.Fatalf("a key of the stats json is %T, not a string", key)
		}
		carried = append(carried, name)
		if _, err := decoder.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(documented, carried) {
		t.Fatalf("the header documents the stats keys as\n  %v\nand the json carries\n  %v", documented, carried)
	}
}

// ── the role model's surface: the kinds, the roster json, and the header that documents both ─

// THE COMMIT-KIND PROJECTION TELLS THE THREE ANSWERS APART, and it does so through errors.Is over
// the chain urmessage answers: a REFUSED verb is ErrCommitUnauthorized wrapping the rule, a LOST
// one is ErrCommitLost wrapping ErrSubmitRefused, and the projection must not read the second as
// FAILED because of what it wraps, nor a bare ErrSubmitRefused as LOST because of what wraps it
// elsewhere. INVALID is the two by-name refusals and this file's own hex refusal; everything else
// -- the transport, a group not yet open, a context cancelled -- is FAILED.
//
// WHAT WOULD GO RED: swap the order of the REFUSED and LOST arms (neither wraps the other, so
// nothing); drop the errors.Is for a string compare; map ErrSubmitRefused to LOST.
func TestTheCommitKindProjectionTellsTheThreeAnswersApart(t *testing.T) {
	rule := fmt.Errorf("%w: %w", urmessage.ErrCommitUnauthorized, urmessage.ErrCommitAddByNonAdmin)
	lost := fmt.Errorf("%w: %w: the server said COMMIT_LOST", urmessage.ErrCommitLost, urmessage.ErrSubmitRefused)
	if !errors.Is(lost, urmessage.ErrSubmitRefused) {
		t.Fatal("this case's LOST fixture does not wrap ErrSubmitRefused, so the ordering it measures is not exercised")
	}
	for _, one := range []struct {
		name string
		err  error
		want int32
	}{
		{"nil", nil, messageCommitOk},
		{"a rule under ErrCommitUnauthorized", rule, messageCommitRefused},
		{"ErrCommitUnauthorized alone", urmessage.ErrCommitUnauthorized, messageCommitRefused},
		{"a configured authorizer's cause under ErrCommitUnauthorized", fmt.Errorf("%w: %w", urmessage.ErrCommitUnauthorized, errors.New("the product says no")), messageCommitRefused},
		{"ErrCommitLost wrapping ErrSubmitRefused", lost, messageCommitLost},
		{"a wrapped ErrCommitLost", fmt.Errorf("SetRole: %w", lost), messageCommitLost},
		{"ErrRoleNotSettable", fmt.Errorf("%w: %q", urmessage.ErrRoleNotSettable, "king"), messageCommitInvalid},
		{"ErrAlreadyOwner", urmessage.ErrAlreadyOwner, messageCommitInvalid},
		// the three RemoveMember refuses by name before any rule is reached (ledger item 258).
		// Each is INVALID and not REFUSED: nothing was built and nothing was counted, so a caller
		// that showed "your role does not permit this" would be inventing a role answer.
		{"ErrRemoveSelf", fmt.Errorf("%w: leaves [3] include this device's own", urmessage.ErrRemoveSelf), messageCommitInvalid},
		{"ErrRemoveOwner", fmt.Errorf("%w: ab holds 1 leaf/leaves", urmessage.ErrRemoveOwner), messageCommitInvalid},
		{"ErrNoSuchMember", fmt.Errorf("%w: ab", urmessage.ErrNoSuchMember), messageCommitInvalid},
		{"an identity that is not hex", fmt.Errorf("%w: odd length", errIdentityNotHex), messageCommitInvalid},
		{"ErrSubmitRefused alone", urmessage.ErrSubmitRefused, messageCommitFailed},
		{"ErrNotConnected", urmessage.ErrNotConnected, messageCommitFailed},
		{"ErrGroupNotOpen", urmessage.ErrGroupNotOpen, messageCommitFailed},
		{"ErrNotReconciled", urmessage.ErrNotReconciled, messageCommitFailed},
		{"a cancelled context", context.Canceled, messageCommitFailed},
		{"an unknown error", errors.New("something else"), messageCommitFailed},
	} {
		if got := messageCommitKindOf(one.err); got != one.want {
			t.Errorf("%s projects to kind %d, want %d", one.name, got, one.want)
		}
	}
	// and the four non-OK kinds are four different numbers, none of them OK
	kinds := map[int32]bool{}
	for _, kind := range []int32{messageCommitOk, messageCommitRefused, messageCommitLost, messageCommitInvalid, messageCommitFailed} {
		kinds[kind] = true
	}
	if len(kinds) != 5 {
		t.Errorf("the five kinds are %d distinct values", len(kinds))
	}
}

// THE IDENTITY A VERB TAKES IS THE HEX A ROSTER ROW CARRIES, AND NOTHING ELSE. An empty string is
// refused too rather than passed on as an empty identity: an empty identity holds no leaf and the
// rules would answer a phantom, which is a sentence about roles for a missing argument.
func TestAVerbsIdentityIsTheRosterRowsHexAndAnythingElseIsInvalid(t *testing.T) {
	identity := bytes.Repeat([]byte{0xA5}, 32)
	row := memberInfoOf(urmessage.Member{IdentityPub: identity})
	decoded, err := messageIdentityOf(cString(row.IdentityPub))
	if err != nil || !bytes.Equal(decoded, identity) {
		t.Errorf("the roster row's identity_pub %q did not round-trip: %x, %v", row.IdentityPub, decoded, err)
	}
	for _, bad := range []string{"", "not hex", "abc", "zz"} {
		decoded, err := messageIdentityOf(cString(bad))
		if !errors.Is(err, errIdentityNotHex) || decoded != nil {
			t.Errorf("identity_pub_hex %q answered %x, %v; want errIdentityNotHex", bad, decoded, err)
		}
		if messageCommitKindOf(err) != messageCommitInvalid {
			t.Errorf("identity_pub_hex %q projects to kind %d, want INVALID", bad, messageCommitKindOf(err))
		}
	}
	// NULL is the empty string, which is refused the same way
	if _, err := messageIdentityOf(nil); !errors.Is(err, errIdentityNotHex) {
		t.Errorf("a NULL identity_pub_hex answered %v, want errIdentityNotHex", err)
	}
}

// EVERY FIELD urmessage.Member CARRIES REACHES A C CALLER, WITH A VALUE. The same gate
// TestTheMessageInfoCarriesEveryFieldUrmessageKeeps is, over the roster row: memberInfo is a hand
// projection and a field added to urmessage.Member arrives at a C caller as nothing at all unless
// it is added here too. The fixture sets every field non-zero, and every projected field must come
// out non-zero -- declaring one is not carrying it.
//
// WHAT WOULD GO RED: add a field to urmessage.Member and not to memberInfo; declare one in
// memberInfo and forget to assign it in memberInfoOf.
func TestTheMemberInfoCarriesEveryFieldUrmessageKeeps(t *testing.T) {
	member := urmessage.Member{
		LeafIndex:    3,
		SenderHandle: bytes.Repeat([]byte{0x1F}, 16),
		IdentityPub:  bytes.Repeat([]byte{0xE7}, 32),
		Role:         "admin",
		Mine:         true,
	}
	kept := reflect.TypeOf(urmessage.Member{})
	fixture := reflect.ValueOf(member)
	for at := 0; at < kept.NumField(); at += 1 {
		if fixture.Field(at).IsZero() {
			t.Fatalf("this case's fixture leaves urmessage.Member.%s at its zero value", kept.Field(at).Name)
		}
	}
	info := memberInfoOf(member)
	carried := reflect.TypeOf(memberInfo{})
	projected := reflect.ValueOf(*info)
	names := map[string]bool{}
	for at := 0; at < carried.NumField(); at += 1 {
		names[carried.Field(at).Name] = true
		if projected.Field(at).IsZero() {
			t.Errorf("memberInfo.%s (json %q) is declared and never assigned", carried.Field(at).Name, carried.Field(at).Tag.Get("json"))
		}
	}
	for at := 0; at < kept.NumField(); at += 1 {
		if name := kept.Field(at).Name; !names[name] {
			t.Errorf("urmessage.Member keeps %s and urnet_message_member_list_info does not carry it", name)
		}
	}
	if kept.NumField() != carried.NumField() {
		t.Errorf("urmessage.Member has %d fields and the json carries %d", kept.NumField(), carried.NumField())
	}
	// and the two octet fields cross as lower case hex of the right width, which is what the
	// header promises and what a verb decodes
	if info.SenderHandle != strings.Repeat("1f", 16) || info.IdentityPub != strings.Repeat("e7", 32) {
		t.Errorf("the octet fields crossed as %q and %q", info.SenderHandle, info.IdentityPub)
	}
}

// headerJsonKeys reads the documented json shape that follows `phrase` in the comment block above
// `declaration` in include/urnetwork_message.h: the `{"key":value,...}` object, its keys in order.
// It fails the test rather than answering an empty list, which is the positive control that the
// header still carries a shape at all.
func headerJsonKeys(t *testing.T, declaration string, phrase string) []string {
	t.Helper()
	header, err := os.ReadFile("include/urnetwork_message.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	text := strings.ReplaceAll(string(header), "\r\n", "\n")
	at := strings.Index(text, declaration)
	if at < 0 {
		t.Fatalf("the header declares no %s", declaration)
	}
	comment := strings.LastIndex(text[:at], "/*")
	if comment < 0 {
		t.Fatalf("%s has no comment block above it", declaration)
	}
	block := text[comment:at]
	start := strings.Index(block, phrase)
	if start < 0 {
		t.Fatalf("the comment above %s does not say %q", declaration, phrase)
	}
	rest := block[start+len(phrase):]
	open := strings.Index(rest, "{")
	end := strings.Index(rest, "}")
	if open < 0 || end < open {
		t.Fatalf("the comment above %s carries no {...} shape after %q", declaration, phrase)
	}
	keys := []string{}
	for _, match := range regexp.MustCompile(`"([a-z_]+)":`).FindAllStringSubmatch(rest[open:end], -1) {
		keys = append(keys, match[1])
	}
	if len(keys) < 2 {
		t.Fatalf("the documented shape above %s has %d keys; the positive control is not met", declaration, len(keys))
	}
	return keys
}

// jsonKeysOf is the keys the marshalled value carries, in its order, walked with a decoder so
// they are the output's and not a tag's.
func jsonKeysOf(t *testing.T, value any) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		t.Fatalf("the json does not open an object: %v %v", open, err)
	}
	keys := []string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name, isString := key.(string)
		if !isString {
			t.Fatalf("a key is %T, not a string", key)
		}
		keys = append(keys, name)
		if _, err := decoder.Token(); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// THE HEADER'S DOCUMENTED MESSAGE JSON IS THE JSON'S OWN, KEY FOR KEY AND IN ORDER -- the stats
// header gate, over the row a conversation is actually made of.
//
// IT IS THE GATE THAT WAS MISSING WHEN R4 ADDED A FIELD. The projection gate above catches a field
// of urmessage.Message that does not reach C at all; nothing caught a field that reaches C and
// that the HEADER does not name, which is the same drift the stats list suffered (four counters,
// every test green). A C caller reads this header before it reads anything else.
//
// WHAT WOULD GO RED: add a field to messageInfo and not to the header's shape, or the reverse, or
// reorder either.
func TestTheHeaderDocumentsExactlyTheMessageJsonKeys(t *testing.T) {
	documented := headerJsonKeys(t, "char* urnet_message_list_info(", "one message's metadata as json")
	carried := jsonKeysOf(t, &messageInfo{})
	if !reflect.DeepEqual(documented, carried) {
		t.Fatalf("the header documents the message keys as\n  %v\nand the json carries\n  %v", documented, carried)
	}
	t.Logf("%d keys, in order: %v", len(carried), carried)
}

// THE HEADER'S DOCUMENTED MEMBER JSON IS THE JSON'S OWN, KEY FOR KEY AND IN ORDER -- the stats
// header gate, over the roster row. The stats list drifted by four counters before its gate
// existed; this one is born with its shape.
//
// WHAT WOULD GO RED: add a field to memberInfo and not to the header's shape, or the reverse, or
// reorder either.
func TestTheHeaderDocumentsExactlyTheMemberJsonKeys(t *testing.T) {
	documented := headerJsonKeys(t, "int32_t urnet_message_member_list_count(", "one member as json:")
	carried := jsonKeysOf(t, &memberInfo{})
	if !reflect.DeepEqual(documented, carried) {
		t.Fatalf("the header documents the member keys as\n  %v\nand the json carries\n  %v", documented, carried)
	}
}

// THE HEADER'S DOCUMENTED REMOVAL JSON IS THE JSON'S OWN, KEY FOR KEY AND IN ORDER -- ledger ruling
// 52's projection, held by the gate the two above it are held by. It is born with its shape rather
// than acquiring one after drifting, which is what the stats list did for four counters.
//
// AND THE PROJECTION'S OWN VALUES ARE HELD HERE TOO, because a key list says nothing about what goes
// in it: a group nobody removed must answer removed false at epoch zero, and a group whose state is
// set must carry the epoch it was removed at rather than the epoch it stands at plus one. The
// urmessage side of that -- which epoch is filed, and that it survives a restart -- is
// urmessage/removalstate_test.go's and cp3b's; what this holds is that the boundary carries what it
// was handed.
//
// WHAT WOULD GO RED: add a field to messageGroupRemoval and not to the header's shape, or the
// reverse; reorder either; have the export report the flag off something other than the error.
func TestTheHeaderDocumentsExactlyTheRemovalJsonKeys(t *testing.T) {
	documented := headerJsonKeys(t, "char* urnet_message_group_removal(", "as json:")
	carried := jsonKeysOf(t, &messageGroupRemoval{})
	if !reflect.DeepEqual(documented, carried) {
		t.Fatalf("the header documents the removal keys as\n  %v\nand the json carries\n  %v", documented, carried)
	}
	for _, one := range []struct {
		name  string
		value messageGroupRemoval
		want  string
	}{
		{"a member", messageGroupRemoval{}, `{"removed":false,"removed_epoch":0}`},
		{"a removed device", messageGroupRemoval{Removed: true, RemovedEpoch: 7}, `{"removed":true,"removed_epoch":7}`},
	} {
		encoded, err := json.Marshal(&one.value)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != one.want {
			t.Errorf("%s crosses as %s, want %s", one.name, encoded, one.want)
		}
	}
}

// THE HEADER'S URNET_MESSAGE_GAP_* DEFINES ARE THE GapReason VALUES THIS BUILD PRODUCES, AND THE
// PRODUCED SET IS DERIVED FROM urmessage's OWN SOURCE.
//
// IT WENT STALE THE WAY THE STATS LIST WENT STALE. The header declared two and said "THIS BUILD
// PRODUCES TWO" while urmessage had been producing three since ledger item 241: out_of_window is
// the commonest gap in the product -- a later joiner produces one per pre-admission record -- and
// a C caller branching on the defines read it as an unrecognised string. R4 made it visible
// because out_of_window is the one reason for which sender_role_at_send is "".
//
// AND THE FIRST REPAIR DID NOT TRACK ITS OWN PROPERTY, WHICH IS WHY THIS IS THE SECOND. It held the
// header against a HAND-WRITTEN three-entry map and said so in its own comment: "it does NOT catch
// a brand new GapReason that nobody adds to either". A fourth constant declared in urmessage with
// no define here left this gate green, which is the shape the stats list had before it was read off
// the file -- a gate that measures the two things somebody remembered to write down. So the
// produced set is now READ OFF ../urmessage's source: every constant of type GapReason in the
// package, by name and by value, wherever it is declared, with its define name derived from its go
// name (GapOutOfWindow -> OUT_OF_WINDOW). A constant with no define now goes red on its own.
//
// AN UNTYPED CONSTANT IN A GapReason BLOCK IS REFUSED RATHER THAN SKIPPED. `GapExpired = "expired"`
// beside the typed ones is NOT of type GapReason in go -- only a spec with no value at all repeats
// the one above it -- so a type filter alone would read it as somebody else's constant and narrow
// it away silently. That is the same invisibility the hand-written map had, one level down, so the
// walk names it and fails.
//
// THE COMPLEMENT IS PRINTED ON EVERY RUN: the constants the type filter REMOVED. An empty
// complement means "of type GapReason" is narrowing nothing today, and a run that read no constants
// at all, or a header that defines none, is a FATAL rather than an agreement between two empty sets.
//
// WHAT WOULD GO RED: drop a define, misspell one, change a string on either side, add a define with
// no constant behind it, ADD A CONSTANT WITH NO DEFINE, or declare one in the block without its
// type.
func TestTheHeaderDefinesExactlyTheGapReasonsThisBuildProduces(t *testing.T) {
	// ---- step 1: the produced set, off the package's own source ----
	produced, sites, complement, files, specs := gapReasonsInSource(t)
	t.Logf("step 1 -- %d go file(s) of ../urmessage walked, %d constant spec(s) read", files, specs)
	if specs == 0 {
		t.Fatal("no constant declaration was read from ../urmessage at all, so the produced set is derived from nothing and every comparison below would be an agreement between two empty maps")
	}
	if len(produced) == 0 {
		t.Fatal("no constant of type GapReason was found in ../urmessage; the positive control that the derivation can SEE the set it narrows is not met")
	}
	for _, at := range sites {
		t.Logf("step 1 -- %s", at)
	}
	// THE COMPLEMENT: what "of type GapReason" removed.
	t.Logf("complement -- the %d constant(s) of ../urmessage this filter removed: %v", len(complement), complement)
	if len(complement) == 0 {
		t.Error("the complement is EMPTY: every constant in ../urmessage is a GapReason, so the type filter narrows nothing today and a constant of another type would be indistinguishable from one this walk read and dismissed")
	}

	// ---- step 2: the header's defines ----
	header, err := os.ReadFile("include/urnetwork_message.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	defined := map[string]string{}
	for _, match := range regexp.MustCompile(`(?m)^#define URNET_MESSAGE_GAP_([A-Z_]+)\s+"([a-z_]+)"`).
		FindAllStringSubmatch(string(header), -1) {
		defined[match[1]] = match[2]
	}
	t.Logf("step 2 -- the header defines %d URNET_MESSAGE_GAP_* reason(s): %v", len(defined), defined)
	if len(defined) < 2 {
		t.Fatalf("the header defines %d URNET_MESSAGE_GAP_* reasons; the positive control is not met", len(defined))
	}
	// AND THE TWO MUST MEET SOMEWHERE. Two non-empty sets that share no key would fail the
	// comparison below on every entry, which reads as drift; it is more likely that one side was
	// read wrongly, and this says which.
	shared := 0
	for name := range produced {
		if _, both := defined[name]; both {
			shared += 1
		}
	}
	if shared == 0 {
		t.Fatalf("the derivation found %v and the header defines %v, and they share NO name: one of the two was read wrongly rather than having drifted", produced, defined)
	}

	if !reflect.DeepEqual(defined, produced) {
		t.Fatalf("the header defines the gap reasons as %v and this build produces %v", defined, produced)
	}
	// AND A MESSAGE THAT IS A MESSAGE IS NONE OF THEM, which is what "" means on the wire and is
	// the value a caller branches on first.
	for name, value := range produced {
		if value == "" {
			t.Errorf("%s is the empty string, which is the answer for a message that is NOT a gap", name)
		}
	}
}

// gapReasonsInSource reads every GapReason constant urmessage declares, by define name and wire
// value, out of the package's source. It answers the produced set, one line per site for the log,
// the constants the type filter removed, and how much it read.
//
// IT PARSES RATHER THAN GREPS because the property is "a constant of this TYPE", which a regexp
// over lines cannot state: the const block carries paragraphs of prose holding the same words, and
// a future declaration may sit in another file of the package entirely. The walk is the whole
// package's production source for that reason -- a gate that read group.go alone would be a gate
// with a directory-shaped blind spot.
func gapReasonsInSource(t *testing.T) (map[string]string, []string, []string, int, int) {
	t.Helper()
	const pkg = "../urmessage"
	entries, err := os.ReadDir(pkg)
	if err != nil {
		t.Fatalf("reading %s: %v; the produced set is derived from that package's source", pkg, err)
	}
	produced := map[string]string{}
	sites := []string{}
	complement := []string{}
	files, specs := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files += 1
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, filepath.Join(pkg, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			general, isGeneral := decl.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.CONST {
				continue
			}
			// IS THIS A BLOCK THAT DECLARES GapReasons AT ALL? An untyped spec is refused only
			// inside one, because every other const block in the package is somebody else's.
			gapBlock := false
			for _, spec := range general.Specs {
				if value, isValue := spec.(*ast.ValueSpec); isValue && isGapReason(value.Type) {
					gapBlock = true
				}
			}
			for _, spec := range general.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				specs += 1
				at := fileSet.Position(value.Pos())
				if !isGapReason(value.Type) {
					for _, declared := range value.Names {
						complement = append(complement, fmt.Sprintf("%s (%s:%d)", declared.Name, name, at.Line))
						if gapBlock {
							t.Errorf("%s:%d: %s is declared in the GapReason const block WITHOUT the type, so it is an untyped constant in go and this walk would narrow it away in silence. A spec in that block is REFUSED rather than skipped: write `%s GapReason = \"...\"`",
								name, at.Line, declared.Name, declared.Name)
						}
					}
					continue
				}
				for index, declared := range value.Names {
					if index >= len(value.Values) {
						t.Errorf("%s:%d: %s is typed GapReason and has no value of its own", name, at.Line, declared.Name)
						continue
					}
					literal, isLiteral := value.Values[index].(*ast.BasicLit)
					if !isLiteral || literal.Kind != token.STRING {
						t.Errorf("%s:%d: %s is typed GapReason and its value is not a string literal, which is the one form this walk can read",
							name, at.Line, declared.Name)
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Errorf("%s:%d: %s's value %s does not unquote: %v", name, at.Line, declared.Name, literal.Value, err)
						continue
					}
					if !strings.HasPrefix(declared.Name, "Gap") {
						t.Errorf("%s:%d: %s is typed GapReason and is not named Gap<Something>, so no URNET_MESSAGE_GAP_* name can be derived for it",
							name, at.Line, declared.Name)
						continue
					}
					suffix := defineSuffixOf(declared.Name)
					if held, twice := produced[suffix]; twice {
						t.Errorf("two GapReason constants derive the define name %s (%q and %q)", suffix, held, text)
					}
					produced[suffix] = text
					sites = append(sites, fmt.Sprintf("%s:%d %s = %q -> URNET_MESSAGE_GAP_%s", name, at.Line, declared.Name, text, suffix))
				}
			}
		}
	}
	slices.Sort(sites)
	slices.Sort(complement)
	return produced, sites, complement, files, specs
}

// isGapReason reports whether a spec's declared type is urmessage's GapReason, written from inside
// that package as the bare identifier it is there.
func isGapReason(of ast.Expr) bool {
	name, isName := of.(*ast.Ident)
	return isName && name.Name == "GapReason"
}

// defineSuffixOf is the header name a GapReason constant's go name derives: GapOutOfWindow becomes
// OUT_OF_WINDOW. It is a derivation and not a table for the same reason the set is.
func defineSuffixOf(name string) string {
	out := []rune{}
	for at, letter := range strings.TrimPrefix(name, "Gap") {
		if unicode.IsUpper(letter) && at > 0 {
			out = append(out, '_')
		}
		out = append(out, unicode.ToUpper(letter))
	}
	return string(out)
}

// THE HEADER'S URNET_MESSAGE_COMMIT_* DEFINES ARE THE LIBRARY'S KINDS, BY NAME AND BY VALUE. A C
// caller branches on the defines and the library answers the constants; a define renumbered on
// one side is a UI that tells a user "the network failed" for a role refusal. The defines are
// READ OFF THE HEADER FILE and held equal to the go constants, both ways: every define has a
// constant and every constant has a define.
//
// WHAT WOULD GO RED: renumber either side, add a kind to one side, misspell a define.
func TestTheHeaderDefinesExactlyTheCommitKinds(t *testing.T) {
	header, err := os.ReadFile("include/urnetwork_message.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	defined := map[string]int32{}
	for _, match := range regexp.MustCompile(`(?m)^#define URNET_MESSAGE_COMMIT_([A-Z]+)\s+(\d+)`).FindAllStringSubmatch(string(header), -1) {
		value, err := strconv.ParseInt(match[2], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		defined[match[1]] = int32(value)
	}
	constants := map[string]int32{
		"OK":      messageCommitOk,
		"REFUSED": messageCommitRefused,
		"LOST":    messageCommitLost,
		"INVALID": messageCommitInvalid,
		"FAILED":  messageCommitFailed,
	}
	if len(defined) < 2 {
		t.Fatalf("the header defines %d URNET_MESSAGE_COMMIT_* kinds; the positive control is not met", len(defined))
	}
	if !reflect.DeepEqual(defined, constants) {
		t.Fatalf("the header defines the commit kinds as %v and the library's constants are %v", defined, constants)
	}
}

// goStringAt reads a NUL-terminated C string an export handed back, without naming a C type.
func goStringAt(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	out := []byte{}
	for at := uintptr(0); ; at += 1 {
		c := *(*byte)(unsafe.Pointer(uintptr(p) + at))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return string(out)
}

// callExportNil is callExport with two more spellings: a nil argument is the zero value of the
// parameter's own type, which is how a NULL out_error or a NULL char* is passed from here, and an
// unsafe.Pointer argument becomes a typed pointer at that address through reflect.NewAt, which
// is how a char* an export handed back is passed to another export without naming C.char.
func callExportNil(t *testing.T, fn any, args ...any) []reflect.Value {
	t.Helper()
	value := reflect.ValueOf(fn)
	shape := value.Type()
	if shape.NumIn() != len(args) {
		t.Fatalf("the export takes %d arguments and %d were passed", shape.NumIn(), len(args))
	}
	in := make([]reflect.Value, 0, len(args))
	for at, arg := range args {
		if arg == nil {
			in = append(in, reflect.Zero(shape.In(at)))
			continue
		}
		if p, isPointer := arg.(unsafe.Pointer); isPointer {
			in = append(in, reflect.NewAt(shape.In(at).Elem(), p))
			continue
		}
		in = append(in, reflect.ValueOf(arg).Convert(shape.In(at)))
	}
	return value.Call(in)
}

// LEAVING OVER A DEVICE HANDLE THAT DOES NOT RESOLVE answers FAILED with out_error left NULL, which
// is the same shape every device verb has: what a real leave erases, and the owner it refuses, is
// cp3b's leave_test.go over a real server.
func TestForgetGroupRefusesADeviceHandleThatDoesNotResolve(t *testing.T) {
	const unknown = uint64(1) << 62
	for _, handle := range []uint64{0, unknown} {
		if got := callExportNil(t, urnet_message_device_forget_group, handle, nil, int32(0), nil)[0].Int(); int32(got) != messageForgetFailed {
			t.Errorf("forget_group on device handle %d answered kind %d, want FAILED", handle, got)
		}
	}
}

// THE FORGET KINDS TELL THE FOUR ANSWERS APART, and the one a bool folded together is the point:
// UNFINISHED is a device that has LEFT, and must not read as a refusal (review H1).
func TestTheForgetKindProjectionTellsALeaveThatHappenedFromOneThatDidNot(t *testing.T) {
	rows := []struct {
		err  error
		want int32
	}{
		{nil, messageForgetOk},
		{fmt.Errorf("%w: a file stayed open", urmessage.ErrForgetUnfinished), messageForgetUnfinished},
		{fmt.Errorf("%w: ab", urmessage.ErrGroupNotHeld), messageForgetFailed},
		{fmt.Errorf("%w: 1 other leaf/leaves", urmessage.ErrOwnerMustTransfer), messageForgetRefused},
		{errors.New("urmessage: who owns this group could not be read"), messageForgetRefused},
	}
	for _, row := range rows {
		if got := messageForgetKindOf(row.err); got != row.want {
			t.Errorf("%v projected to kind %d, want %d", row.err, got, row.want)
		}
	}
}

func TestTheHeaderDefinesExactlyTheForgetKinds(t *testing.T) {
	header, err := os.ReadFile("include/urnetwork_message.h")
	if err != nil {
		t.Fatalf("reading the header: %v", err)
	}
	defined := map[string]int32{}
	for _, match := range regexp.MustCompile(`(?m)^#define URNET_MESSAGE_FORGET_([A-Z]+)\s+(\d+)`).FindAllStringSubmatch(string(header), -1) {
		value, err := strconv.ParseInt(match[2], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		defined[match[1]] = int32(value)
	}
	constants := map[string]int32{
		"OK":         messageForgetOk,
		"REFUSED":    messageForgetRefused,
		"UNFINISHED": messageForgetUnfinished,
		"FAILED":     messageForgetFailed,
	}
	if len(defined) < 2 {
		t.Fatalf("the header defines %d URNET_MESSAGE_FORGET_* kinds; the positive control is not met", len(defined))
	}
	if !reflect.DeepEqual(defined, constants) {
		t.Fatalf("the header defines the forget kinds as %v and the library's constants are %v", defined, constants)
	}
}

// THE ROSTER AND THE VERBS OVER A HANDLE THAT DOES NOT RESOLVE, WHICH C CANNOT TELL FROM A
// REFUSAL: the zero handle and an unknown one answer 0 / NULL / FAILED with out_error left NULL,
// the verbs answer FAILED for an unknown ctx too, and the member list accessors bound their index.
// The conversation itself -- a real owner promoting a real member, a real member refused -- is
// ctest/message_abi_test.c's, over a real server; what is here is the half that test cannot see,
// because cgoGuard recovers a panic into the same false.
func TestTheRosterAndTheVerbsRefuseAHandleThatDoesNotResolve(t *testing.T) {
	const unknown = uint64(1) << 62
	for _, handle := range []uint64{0, unknown} {
		if got := callExportNil(t, urnet_message_group_members, handle, nil)[0].Uint(); got != 0 {
			t.Errorf("members on handle %d answered %d", handle, got)
		}
		if got := callExportNil(t, urnet_message_group_my_role, handle, nil)[0]; !got.IsNil() {
			t.Errorf("my_role on handle %d answered something", handle)
		}
		if got := callExportNil(t, urnet_message_group_set_role, handle, uint64(0), cString("ab"), cString("admin"), nil)[0].Int(); int32(got) != messageCommitFailed {
			t.Errorf("set_role on handle %d answered kind %d, want FAILED", handle, got)
		}
		if got := callExportNil(t, urnet_message_group_transfer_ownership, handle, uint64(0), cString("ab"), nil)[0].Int(); int32(got) != messageCommitFailed {
			t.Errorf("transfer_ownership on handle %d answered kind %d, want FAILED", handle, got)
		}
		if got := callExportNil(t, urnet_message_group_remove_member, handle, uint64(0), cString("ab"), nil)[0].Int(); int32(got) != messageCommitFailed {
			t.Errorf("remove_member on handle %d answered kind %d, want FAILED", handle, got)
		}
		// AND RULING 52's PROJECTION, WHICH MUST ANSWER NULL AND NOT `{"removed":false,…}`. A json
		// body here would tell a caller "this device is still a member of that group", which is a
		// claim about a group this library cannot see -- and it is the unsafe direction of that
		// claim, because a screen acting on it leaves its composer live.
		if got := callExport(t, urnet_message_group_removal, handle)[0]; !got.IsNil() {
			t.Errorf("removal on handle %d answered %q, want NULL: a json body here says this "+
				"device is still a member of a group this handle does not name",
				handle, goStringAt(got.UnsafePointer()))
		}
		if got := callExport(t, urnet_message_member_list_count, handle)[0].Int(); got != 0 {
			t.Errorf("member_list_count on handle %d answered %d", handle, got)
		}
		if got := callExport(t, urnet_message_member_list_info, handle, 0)[0]; !got.IsNil() {
			t.Errorf("member_list_info on handle %d answered something", handle)
		}
	}
	// an unknown ctx over a handle that IS a group: still FAILED, still no out_error, because a
	// call that cannot be cancelled while its caller believes it can is the failure the ctx
	// handle exists to prevent -- and the group is never asked
	group := newHandle(&memberList{})
	defer handleRelease(group)
	if got := callExportNil(t, urnet_message_group_set_role, group, unknown, cString("ab"), cString("admin"), nil)[0].Int(); int32(got) != messageCommitFailed {
		t.Errorf("set_role with an unknown ctx answered kind %d, want FAILED", got)
	}
	if got := callExportNil(t, urnet_message_group_remove_member, group, unknown, cString("ab"), nil)[0].Int(); int32(got) != messageCommitFailed {
		t.Errorf("remove_member with an unknown ctx answered kind %d, want FAILED", got)
	}

	// the member list handle bounds its index and answers the row's json in between
	list := newHandle(&memberList{members: []urmessage.Member{
		{LeafIndex: 0, SenderHandle: bytes.Repeat([]byte{1}, 16), IdentityPub: bytes.Repeat([]byte{2}, 32), Role: "owner", Mine: true},
		{LeafIndex: 1, SenderHandle: bytes.Repeat([]byte{3}, 16), IdentityPub: bytes.Repeat([]byte{4}, 32), Role: "member"},
	}})
	defer handleRelease(list)
	if got := callExport(t, urnet_message_member_list_count, list)[0].Int(); got != 2 {
		t.Errorf("a two member list counts %d", got)
	}
	for _, index := range []int{-1, 2, 7} {
		if got := callExport(t, urnet_message_member_list_info, list, index)[0]; !got.IsNil() {
			t.Errorf("index %d of a two member list answered json", index)
		}
	}
	row := callExport(t, urnet_message_member_list_info, list, 1)[0]
	if row.IsNil() {
		t.Fatal("index 1 of a two member list answered NULL")
	}
	text := goStringAt(row.UnsafePointer())
	callExportNil(t, urnet_free_string, row.UnsafePointer())
	var decoded memberInfo
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("row 1 is not json: %q: %v", text, err)
	}
	if decoded.LeafIndex != 1 || decoded.Role != "member" || decoded.Mine || decoded.IdentityPub != strings.Repeat("04", 32) {
		t.Errorf("row 1 crossed as %+v", decoded)
	}
}
