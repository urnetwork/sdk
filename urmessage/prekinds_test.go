package urmessage

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
)

// ── the pre-kinds local copy ─────────────────────────────────────────────────────────────────
//
// WHAT THIS FILE IS ABOUT. A state directory written by a PRE-KINDS build holds copies whose bodies
// are RAW TEXT with no kind octet, because [SentRecord.Body] is "what was sealed, octets, never
// interpreted" and what that build sealed was the text. [Group.openOwnFromCopyLocked] reads every
// copy through [ParseContent], so the first octet of somebody's English sentence is read as a
// ContentKind. NOTHING IN EITHER SUITE DROVE THAT PATH before this file: grep for a copy whose body
// is not an envelope and the answer was nothing.

// A PRE-KINDS COPY IS NOT SHOWN, AND WHAT DECIDES THAT IS THE FIRST OCTET OF THE TEXT.
//
// The two outcomes are the two halves of the range rule and they are NOT the same outcome:
//
//   - a first octet in 0x40..0x7F is a TRANSIENT code, which is legal on EPH(0) and on nothing else.
//     A local copy is DURABLE, so it is MALFORMED -- the sender broke a rule the code alone decides
//     -- and openOwnFromCopyLocked answers false. The record falls through to the ordinary path,
//     where it is authenticated as this device's own and counted in [Stats.OwnWithoutCopy]: a hole
//     this device NAMES.
//   - a first octet in 0x01..0x3F that this build has no parser for is an UNASSIGNED stored code. It
//     is shown, as one closed placeholder under the unknown-kind rule.
//
// This case drives both through the real path, with a real record sealed by a real session, because
// the difference between them is a difference in what the USER sees -- a named hole against a
// placeholder -- and neither answer is reachable from [ParseContent] alone.
func TestAPreKindsCopyIsReadAsAKindAndIsNotShownAsText(t *testing.T) {
	for _, one := range []struct {
		name  string
		text  string
		shown bool
		why   string
	}{
		{
			name:  "an ordinary English sentence",
			text:  "hello, is this working",
			shown: false,
			why:   "'h' is 0x68, which is in the TRANSIENT range and is MALFORMED on DURABLE",
		},
		{
			name:  "a sentence beginning with a capital",
			text:  "Yes it is",
			shown: false,
			why:   "'Y' is 0x59, TRANSIENT, malformed on DURABLE",
		},
		{
			name:  "a line beginning with a digit",
			text:  "3 of them arrived",
			shown: true,
			why:   "'3' is 0x33, an unassigned STORED code, so it is one closed placeholder",
		},
		{
			name:  "a line beginning with a space",
			text:  " indented",
			shown: true,
			why:   "' ' is 0x20, an unassigned STORED code",
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			shown, received := showOnePreKindsCopy(t, one.text)
			if shown != one.shown {
				t.Fatalf("%q was %s from its copy, and %s", one.text,
					map[bool]string{true: "SHOWN", false: "not shown"}[shown], one.why)
			}
			if !shown {
				return
			}
			// AND WHAT IS SHOWN IS A PLACEHOLDER AND NOT THE LINE. The whole point of
			// headVersion 0x02 was that a body this build cannot parse must never be
			// rendered as text attributed to a real sender.
			//
			// IT CARRIES [GapUnsupported] AND THIS IS THE ONLY CASE THAT SAYS SO. The
			// walk's unsupported records are gaps because [Group.openPageLocked] marks
			// them; these reach a caller from [Group.openOwnFromCopyLocked] instead,
			// which is a SECOND site, and a build that marked only the first would leave
			// this device's own pre-kinds line as the one blank entry in a build that has
			// no others -- exactly the "a UI cannot tell a newer feature from a blank
			// message" failure, surviving at the call site nobody was looking at.
			if received.Gap != GapUnsupported {
				t.Errorf("%q came back as a %q entry, want the %q gap every other unknown code becomes",
					one.text, received.Gap, GapUnsupported)
			}
			if received.Text != "" {
				t.Errorf("%q came back carrying Text %q; a code this build does not know has no text",
					one.text, received.Text)
			}
			if received.Kind != ContentKind(one.text[0]) {
				t.Errorf("%q came back as kind %s and its first octet is 0x%02x",
					one.text, received.Kind, one.text[0])
			}
		})
	}
}

// showOnePreKindsCopy seals one record whose body is RAW TEXT -- which is what a pre-kinds build
// sealed -- registers it as this device's own copy, and drives [Group.openOwnFromCopyLocked] over
// it. It answers whether the copy was shown and, if it was, the [Message] it became.
//
// IT GOES THROUGH THE REAL FUNCTION AND NOT THROUGH [ParseContent], because what is under test is
// what the USER gets: a copy that parses is delivered and a copy that does not falls through to the
// ordinary path, and only openOwnFromCopyLocked knows the difference.
func showOnePreKindsCopy(t *testing.T, text string) (bool, *Message) {
	t.Helper()
	world := newKindWalk(t)

	// THE BODY IS THE TEXT AND NOTHING ELSE -- no kind octet. That is the pre-kinds encoding.
	body := []byte(text)
	record, err := world.bob.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), body, 0, nil)
	if err != nil {
		t.Fatalf("sealing a pre-kinds body: %v", err)
	}
	if sha256.Sum256(record.CtBody) != record.Header.BodyHash {
		t.Fatal("the sealed record's body_hash is not the hash of its ct_body")
	}
	world.bob.ownIndices[record.Header.StreamIndex] = &ownSealed{
		bodyHash: record.Header.BodyHash,
		hasCopy:  true,
		body:     body,
		sentAtMs: time.Now().UnixMilli(),
	}

	walk := &pageWalk{opened: []*Message{}, reconciled: true}
	shown, err := world.bob.openOwnFromCopyLocked(walk, 7, record)
	if err != nil {
		t.Fatalf("openOwnFromCopyLocked over a pre-kinds copy: %v", err)
	}
	if !shown {
		return false, nil
	}
	if len(walk.opened) != 1 {
		t.Fatalf("the copy was shown and the walk carries %d message(s)", len(walk.opened))
	}
	return true, walk.opened[0]
}

// THE SPLIT OVER THE WHOLE PRINTABLE-ASCII FIRST OCTET, MEASURED RATHER THAN ASSERTED.
//
// THIS CASE EXISTS BECAUSE THE COMMENT IT REPLACES WAS FALSE. group.go said a pre-kinds copy
// beginning "h" was "kind 0x68, which is unassigned, so it renders as one closed placeholder". 0x68
// is 104, which is inside 0x40..0x7F -- the TRANSIENT range -- and a transient code on a stored
// class is MALFORMED and is not rendered at all. The sentence was the wrong way round about the
// population it was written for, and four reviewers read past it, so the split is a MEASUREMENT
// here and a number in the comment rather than a claim in either place.
//
// THE QUERY, BESIDE THE NUMBER: every printable ASCII octet 0x20..0x7E as the first octet of a
// DURABLE body, counted by verdict.
func TestTheMeasuredSplitOfAPrintableFirstOctetOnADurableCopy(t *testing.T) {
	const firstPrintable, lastPrintable = 0x20, 0x7E

	malformed, unsupported, parsed := []byte{}, []byte{}, []byte{}
	for code := firstPrintable; code <= lastPrintable; code += 1 {
		// A TAIL LONG ENOUGH TO SATISFY EVERY LAYOUT THIS BUILD HAS, so that a verdict of
		// MALFORMED is the RANGE RULE's answer and not "the body was too short".
		body := append([]byte{byte(code)}, []byte(strings.Repeat("x", MessageIdBytes+8))...)
		_, verdict, _ := ParseContent(body, message.RetentionDurable, 0)
		switch verdict {
		case ContentMalformed:
			malformed = append(malformed, byte(code))
		case ContentUnsupported:
			unsupported = append(unsupported, byte(code))
		case ContentParsed:
			parsed = append(parsed, byte(code))
		}
	}
	t.Logf("printable ASCII first octets on DURABLE: %d malformed, %d unsupported, %d parsed (of %d)",
		len(malformed), len(unsupported), len(parsed), lastPrintable-firstPrintable+1)
	t.Logf("malformed:   %q", string(malformed))
	t.Logf("unsupported: %q", string(unsupported))

	if len(malformed) != 63 || len(unsupported) != 32 || len(parsed) != 0 {
		t.Errorf("the split is %d malformed / %d unsupported / %d parsed and the comment in group.go says 63 / 32 / 0",
			len(malformed), len(unsupported), len(parsed))
	}
	// AND WHICH 63, because a count alone would survive the range rule moving by one band.
	if string(malformed) != "@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~" {
		t.Errorf("the malformed half is %q; it is 0x40..0x7E, which is '@', every letter, and [ \\ ] ^ _ ` { | } ~",
			string(malformed))
	}

	// NO PRINTABLE ASCII FIRST OCTET IS A KIND THIS BUILD PARSES, AT ANY LENGTH, and this is the
	// reassuring half: no old line is ever silently REINTERPRETED as a reply, a tombstone or a
	// reaction, which would be a body rendered wrong rather than a hole named.
	//
	// IT IS SWEPT OVER LENGTH AND NOT ONLY OVER THE CODE, because two of the layouts this build
	// knows refuse on length -- a tombstone is exactly 32 octets and a cover is exactly 0 -- so a
	// case at one length could have measured the length rather than the code.
	for code := firstPrintable; code <= lastPrintable; code += 1 {
		for length := 0; length <= MessageIdBytes+4; length += 1 {
			body := append([]byte{byte(code)}, []byte(strings.Repeat("x", length))...)
			if _, verdict, _ := ParseContent(body, message.RetentionDurable, 0); verdict == ContentParsed {
				t.Fatalf("a body of first octet 0x%02x (%q) and %d octets of tail PARSED as kind %s; no printable character is a code this build has a grammar for",
					code, rune(code), length, ContentKind(code))
			}
		}
	}
	// THE CONTROL, which is what says the sweep above is not vacuous: the codes that DO parse are
	// exactly the six this build has parsers for, and every one of them is non-printable.
	for kind := range contentParsers {
		if firstPrintable <= int(kind) && int(kind) <= lastPrintable {
			t.Errorf("kind %s is a code this build parses AND is printable ASCII, so an old line beginning with it would be reinterpreted", kind)
		}
	}
	if len(contentParsers) != 6 {
		t.Errorf("this build has %d parsers and the sweep above is written against six", len(contentParsers))
	}
}
