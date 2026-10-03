package urmessage

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

// ── the content envelope, and every refusal it owes ──────────────────────────────────────────
//
// WHAT THESE CASES ARE FOR. The body is OPAQUE to connect at all four of its call sites, so
// connect's suite cannot gate one thing about kinds -- not the range rule, not a malformed refusal,
// not the unknown-kind rule, not the escape's shape. Every one of those properties is gated here or
// it is gated nowhere. That is the one honest cost of the 2026-09-17 ruling's ownership decision
// and it is written into the plan rather than discovered.

// aTarget is a message_id shaped value. It is not derived from a record because nothing in these
// cases opens one: what the codec owes is that a 32 octet referent goes in and the same 32 octets
// come out.
func aTarget(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, MessageIdBytes)
}

// stored is any class the STORED range is legal on. DURABLE is the one this build seals at.
const storedClass = message.RetentionDurable

// parseStored reads one plaintext on the class this build actually uses.
func parseStored(plaintext []byte) (*Content, ContentVerdict, error) {
	return ParseContent(plaintext, storedClass, 0)
}

// parseEph0 reads one plaintext on EPH(0), the never-persisted bucket.
func parseEph0(plaintext []byte) (*Content, ContentVerdict, error) {
	return ParseContent(plaintext, message.RetentionEph, 0)
}

// EVERY KIND THIS BUILD WRITES IS READ BACK AS WHAT IT WROTE.
//
// It is the round trip and not the parse alone, because an encoder and a parser that disagree
// produce a sender whose own screen is right and whose records every receiver refuses.
func TestEveryKindThisBuildSealsReadsBackAsWhatItSealed(t *testing.T) {
	target := aTarget(0x7A)

	text, err := encodeText("a line")
	if err != nil {
		t.Fatalf("encodeText: %v", err)
	}
	reply, err := encodeReply(target, "an answer")
	if err != nil {
		t.Fatalf("encodeReply: %v", err)
	}
	add, err := encodeReaction(KindReactionAdd, target, "👍")
	if err != nil {
		t.Fatalf("encodeReaction(add): %v", err)
	}
	remove, err := encodeReaction(KindReactionRemove, target, "👍")
	if err != nil {
		t.Fatalf("encodeReaction(remove): %v", err)
	}
	tombstone, err := encodeTombstone(target)
	if err != nil {
		t.Fatalf("encodeTombstone: %v", err)
	}

	for _, one := range []struct {
		name      string
		plaintext []byte
		octets    int
		want      Content
	}{
		{"TEXT", text, 1 + 6, Content{Kind: KindText, Text: "a line"}},
		{"REPLY", reply, 1 + 32 + 9, Content{Kind: KindReply, Target: target, Text: "an answer"}},
		{"REACTION_ADD", add, 1 + 32 + 4, Content{Kind: KindReactionAdd, Target: target, Emoji: "👍"}},
		{"REACTION_REMOVE", remove, 1 + 32 + 4, Content{Kind: KindReactionRemove, Target: target, Emoji: "👍"}},
		{"TOMBSTONE", tombstone, 1 + 32, Content{Kind: KindTombstone, Target: target}},
		{"COVER", encodeCover(), 1, Content{Kind: KindCover}},
	} {
		if len(one.plaintext) != one.octets {
			t.Errorf("%s: the plaintext is %d octets, and the ruling's registry says %d",
				one.name, len(one.plaintext), one.octets)
		}
		if ContentKind(one.plaintext[0]) != one.want.Kind {
			t.Errorf("%s: octet 0 is 0x%02x, want %s", one.name, one.plaintext[0], one.want.Kind)
		}
		entry, verdict, err := parseStored(one.plaintext)
		if verdict != ContentParsed {
			t.Fatalf("%s: verdict %s: %v", one.name, verdict, err)
		}
		if entry.Kind != one.want.Kind {
			t.Errorf("%s: came back as %s", one.name, entry.Kind)
		}
		if entry.Text != one.want.Text {
			t.Errorf("%s: text came back as %q, want %q", one.name, entry.Text, one.want.Text)
		}
		if entry.Emoji != one.want.Emoji {
			t.Errorf("%s: emoji came back as %q, want %q", one.name, entry.Emoji, one.want.Emoji)
		}
		if !bytes.Equal(entry.Target, one.want.Target) {
			t.Errorf("%s: target came back as %x, want %x", one.name, entry.Target, one.want.Target)
		}
	}
}

// RULE R-d: EXACTLY ONE ENCODING PER BODY, AND EVERY OTHER SHAPE IS REFUSED BY NAME.
//
// The five clauses of the rule are the five groups below: too short for its layout, trailing octets
// after a layout with no tail, an empty required tail, a kind of 0x00 -- and the one the rule does
// not enumerate because it precedes every layout, a plaintext with no octets at all.
//
// WHAT WOULD GO RED: relax any length comparison in kind.go from `!=` to `<`, or drop the
// non-empty check on a tail, and the matching row here stops being refused.
func TestAContentBodyWithAnySecondEncodingIsRefused(t *testing.T) {
	target := aTarget(0x11)
	for _, one := range []struct {
		name      string
		plaintext []byte
	}{
		{"an empty plaintext, which carries no kind", []byte{}},
		{"kind 0x00, reserved on every class", []byte{0x00}},
		{"kind 0x00 with a body", append([]byte{0x00}, target...)},

		{"TEXT with an empty tail", []byte{byte(KindText)}},
		{"TEXT whose tail is not utf8", []byte{byte(KindText), 0xC3, 0x28}},

		{"REPLY with no reply_to at all", []byte{byte(KindReply)}},
		{"REPLY with a short reply_to", append([]byte{byte(KindReply)}, target[:MessageIdBytes-1]...)},
		{"REPLY with a whole reply_to and an empty text", append([]byte{byte(KindReply)}, target...)},

		{"TOMBSTONE with no target", []byte{byte(KindTombstone)}},
		{"TOMBSTONE with a short target", append([]byte{byte(KindTombstone)}, target[:MessageIdBytes-1]...)},
		{"TOMBSTONE with an octet after its target", append(append([]byte{byte(KindTombstone)}, target...), 0x00)},

		{"REACTION_ADD with a short target", append([]byte{byte(KindReactionAdd)}, target[:MessageIdBytes-1]...)},
		{"REACTION_ADD with an empty emoji", append([]byte{byte(KindReactionAdd)}, target...)},
		{"REACTION_ADD whose emoji is not utf8", append(append([]byte{byte(KindReactionAdd)}, target...), 0xFF, 0xFE)},
		{"REACTION_ADD whose emoji is over the cap", append(append([]byte{byte(KindReactionAdd)}, target...),
			bytes.Repeat([]byte{'u'}, MaxEmojiOctets+1)...)},
		{"REACTION_REMOVE with an empty emoji", append([]byte{byte(KindReactionRemove)}, target...)},

		{"COVER with an octet after it", []byte{byte(KindCover), 0x00}},
	} {
		entry, verdict, err := parseStored(one.plaintext)
		if verdict != ContentMalformed {
			t.Errorf("%s: verdict %s, want malformed", one.name, verdict)
			continue
		}
		if !errors.Is(err, ErrContentMalformed) {
			t.Errorf("%s: refused with %v, want ErrContentMalformed", one.name, err)
		}
		if entry != nil {
			t.Errorf("%s: was refused AND answered an entry", one.name)
		}
	}

	// THE CONTROL, WHICH IS WHAT KEEPS THE TABLE ABOVE FROM PASSING FOR A CODEC THAT REFUSES
	// EVERYTHING: the shortest legal form of each of those layouts parses.
	for _, one := range []struct {
		name      string
		plaintext []byte
	}{
		{"TEXT with one octet of tail", []byte{byte(KindText), 'u'}},
		{"REPLY with one octet of tail", append(append([]byte{byte(KindReply)}, target...), 'u')},
		{"TOMBSTONE", append([]byte{byte(KindTombstone)}, target...)},
		{"REACTION_ADD with one octet of emoji", append(append([]byte{byte(KindReactionAdd)}, target...), 'u')},
		{"REACTION_ADD with an emoji at the cap", append(append([]byte{byte(KindReactionAdd)}, target...),
			bytes.Repeat([]byte{'u'}, MaxEmojiOctets)...)},
		{"COVER", []byte{byte(KindCover)}},
	} {
		if _, verdict, err := parseStored(one.plaintext); verdict != ContentParsed {
			t.Errorf("the control %s: verdict %s: %v", one.name, verdict, err)
		}
	}
}

// THE RANGE RULE, BOTH DIRECTIONS, and it is evaluated on the CODE and the CLASS together because
// neither decides alone.
//
// It runs FIRST among the post-open checks and for known and unknown codes alike, so a transient
// code on a stored class is refused without its body ever being looked at -- which is what makes
// "the sender broke a rule the code alone decides" a true sentence about it.
func TestTheRangeRuleRefusesACodeOnAClassItsRangeDoesNotAllow(t *testing.T) {
	target := aTarget(0x22)

	// A TRANSIENT CODE ON A STORED CLASS IS MALFORMED. Every one of the four, including the two
	// whose bodies are empty, because the refusal is about the code and not about the body.
	for _, kind := range []ContentKind{KindDelivered, KindReadThrough, KindTypingStart, KindTypingStop, 0x5A, 0x7F} {
		plaintext := append([]byte{byte(kind)}, target...)
		_, verdict, err := parseStored(plaintext)
		if verdict != ContentMalformed || !errors.Is(err, ErrContentMalformed) {
			t.Errorf("%s on a stored class: verdict %s, %v; want malformed", kind, verdict, err)
		}
	}

	// AND A STORED CODE ON EPH(0) IS MALFORMED, including the ones this build knows how to read:
	// the range rule comes first, so a well formed TEXT on the never-persisted bucket is still a
	// rule the sender broke.
	for _, kind := range []ContentKind{KindText, KindReply, KindTombstone, KindReactionAdd, KindCover, 0x3F} {
		plaintext := append([]byte{byte(kind)}, target...)
		_, verdict, err := parseEph0(plaintext)
		if verdict != ContentMalformed || !errors.Is(err, ErrContentMalformed) {
			t.Errorf("%s on EPH(0): verdict %s, %v; want malformed", kind, verdict, err)
		}
	}

	// A TRANSIENT CODE ON EPH(0) IS LEGAL BY RANGE AND UNBUILT BY THIS BUILD, so it is DROPPED
	// rather than refused: nothing was persisted, so there is no history for a gap to be a hole
	// in. This is the arm that says the two refusals above are about the RANGE and not about the
	// codes being unknown.
	for _, kind := range []ContentKind{KindDelivered, KindReadThrough, KindTypingStart, KindTypingStop} {
		plaintext := append([]byte{byte(kind)}, target...)
		entry, verdict, err := parseEph0(plaintext)
		if verdict != ContentDropped || !errors.Is(err, ErrContentDropped) {
			t.Errorf("%s on EPH(0): verdict %s, %v; want dropped", kind, verdict, err)
		}
		if entry != nil {
			t.Errorf("%s on EPH(0) was dropped AND answered an entry", kind)
		}
	}

	// AND EPH(1..5) IS NOT EPH(0). The stored range is legal on every eph bucket but the zero
	// one, and a rule written as "is it ephemeral" rather than "is it bucket zero" would refuse
	// these.
	for bucket := uint8(1); bucket <= 5; bucket += 1 {
		if _, verdict, err := ParseContent([]byte{byte(KindText), 'u'}, message.RetentionEph, bucket); verdict != ContentParsed {
			t.Errorf("TEXT on EPH(%d): verdict %s, %v; want parsed", bucket, verdict, err)
		}
	}

	// AND THE 0x00 REFUSAL IS ON EVERY CLASS, ALWAYS: the reserved code has no range to be legal
	// in.
	for _, one := range []struct {
		name  string
		parse func([]byte) (*Content, ContentVerdict, error)
	}{{"a stored class", parseStored}, {"EPH(0)", parseEph0}} {
		if _, verdict, _ := one.parse([]byte{0x00, 'u'}); verdict != ContentMalformed {
			t.Errorf("kind 0x00 on %s: verdict %s, want malformed", one.name, verdict)
		}
	}
}

// A KIND THIS BUILD DOES NOT KNOW: THE RULE THAT MAKES EVERY LATER KIND ADDITIVE.
//
// The record keeps its position and its message_id, it is NOT a failure, it renders as one closed
// placeholder, AND IT IS NEVER PARSED AS ANY KNOWN KIND. The last clause is the one a reader is
// most likely to break: a codec that fell back to "read the tail as text" would turn a future
// EDIT's body into a line of the conversation attributed to a real sender, which is exactly the
// failure headVersion 0x02 exists to prevent one build earlier.
//
// The walk half of the rule -- not a fail(), does not count toward ErrRecordAbandoned -- is
// TestAnUnknownKindKeepsItsPositionAndIsNotAFailure, which drives the real walk.
func TestAnUnknownKindIsUnsupportedAndIsNeverParsedAsAKnownOne(t *testing.T) {
	target := aTarget(0x33)
	for _, kind := range []ContentKind{
		KindAttachment,     // 0x03: defined, and the blob plane is not built
		KindEdit,           // 0x08: reserved by the design, unspecified in v1
		KindAttachmentBody, // 0x09: defined, and the blob plane is not built
		0x3F,               // the top of the stored range
		0x80,               // the bottom of the unassigned range
		0xFE,               // the top of it
	} {
		// a body that WOULD parse as a TEXT, a REPLY and a REACTION if anything guessed
		body := append(append([]byte(nil), target...), "not a text"...)
		entry, verdict, err := parseStored(append([]byte{byte(kind)}, body...))
		if verdict != ContentUnsupported || !errors.Is(err, ErrContentUnsupported) {
			t.Errorf("%s: verdict %s, %v; want unsupported", kind, verdict, err)
			continue
		}
		if entry == nil {
			t.Errorf("%s: unsupported and no entry, so a placeholder has no kind to render", kind)
			continue
		}
		if entry.Kind != kind {
			t.Errorf("%s: the entry carries kind %s", kind, entry.Kind)
		}
		if entry.Text != "" || entry.Emoji != "" || entry.Target != nil {
			t.Errorf("%s: an unsupported entry was filled in as text %q, emoji %q, target %x",
				kind, entry.Text, entry.Emoji, entry.Target)
		}
		if !bytes.Equal(entry.Body, body) {
			t.Errorf("%s: the body came back as %d octets, want the %d that followed the code",
				kind, len(entry.Body), len(body))
		}
	}

	// AND ON EPH(0) THE SAME CODES ARE DROPPED, because nothing was persisted. (A stored-range
	// code is refused by the range rule before it reaches here; these are the unassigned ones,
	// which the range rule does not constrain and the class alone decides.)
	for _, kind := range []ContentKind{0x80, 0xFE} {
		if _, verdict, err := parseEph0([]byte{byte(kind), 'u'}); verdict != ContentDropped {
			t.Errorf("%s on EPH(0): verdict %s, %v; want dropped", kind, verdict, err)
		}
	}
}

// THE 0xFF ESCAPE: NO CODE IS ASSIGNED IN V1 AND THE SHAPE IS FIXED ANYWAY.
//
// A v1 receiver treats it as an unknown kind, MUST know the body begins at OFFSET 2, and MUST NOT
// refuse it for shape. Fixing the skeleton costs zero octets and is what makes the reservation a
// specification rather than one more unknown code -- and the case for it is that a build written
// today can hand a later build's escaped record to a placeholder with its body intact.
//
// WHAT WOULD GO RED: parse the escape's body from offset 1, or refuse a truncated one as malformed,
// and the two halves below stop agreeing.
func TestTheEscapesBodyBeginsAtOffsetTwoAndIsNotRefusedForShape(t *testing.T) {
	body := []byte("a body only a later build can read")
	plaintext := append([]byte{byte(KindEscape), 0x01}, body...)
	entry, verdict, err := parseStored(plaintext)
	if verdict != ContentUnsupported || !errors.Is(err, ErrContentUnsupported) {
		t.Fatalf("the escape: verdict %s, %v; want unsupported", verdict, err)
	}
	if entry.Kind != KindEscape {
		t.Errorf("the escape came back as kind %s", entry.Kind)
	}
	// THE ESCAPED CODE IS NOT A CODE OF THE REGISTRY. kind2 0x01 is not TEXT: the escape opens a
	// SECOND code space, and a v1 receiver that read it as this one's would render a later
	// build's record as a line of text.
	if entry.EscapedKind != 0x01 {
		t.Errorf("kind2 came back as 0x%02x, want 0x01", entry.EscapedKind)
	}
	if entry.Text != "" {
		t.Errorf("an escaped 0x01 was parsed as a TEXT: %q", entry.Text)
	}
	if !bytes.Equal(entry.Body, body) {
		t.Errorf("the escape's body came back as %q, want the %d octets from offset 2", entry.Body, len(body))
	}

	// AND NOTHING ABOUT ITS SHAPE IS REFUSED. A truncated escape, an escaped 0x00 and an escaped
	// 0xFF are all shapes of a code this build does not have; a v1 receiver having an opinion
	// about them would be this build deciding a layout a later ruling owns.
	for _, one := range []struct {
		name      string
		plaintext []byte
	}{
		{"a bare 0xFF with no kind2", []byte{byte(KindEscape)}},
		{"an escaped 0x00", []byte{byte(KindEscape), 0x00}},
		{"an escaped 0xFF", []byte{byte(KindEscape), 0xFF}},
		{"an escaped code with an empty body", []byte{byte(KindEscape), 0x42}},
	} {
		entry, verdict, err := parseStored(one.plaintext)
		if verdict != ContentUnsupported {
			t.Errorf("%s: verdict %s, %v; want unsupported and never malformed", one.name, verdict, err)
			continue
		}
		if entry.Kind != KindEscape {
			t.Errorf("%s: came back as kind %s", one.name, entry.Kind)
		}
	}

	// AND ON EPH(0) IT IS DROPPED like any other code the class alone decides.
	if _, verdict, _ := parseEph0([]byte{byte(KindEscape), 0x01, 'u'}); verdict != ContentDropped {
		t.Errorf("an escape on EPH(0): verdict %s, want dropped", verdict)
	}
}

// THE REGISTRY'S OWN RANGES, held against the ruling's table rather than against spanOf's source.
//
// A code that moved band -- because a boundary was written as < where it should be <=, or because a
// code was given a value in the wrong range -- changes which retention classes it is legal on, and
// every other case in this file would go on passing with the new answer.
func TestEveryCodeInTheRegistryIsInTheBandTheRulingPutsItIn(t *testing.T) {
	for _, one := range []struct {
		kind ContentKind
		want kindSpan
	}{
		{KindReserved, spanReserved},
		{KindText, spanStored},
		{KindReply, spanStored},
		{KindAttachment, spanStored},
		{KindTombstone, spanStored},
		{KindReactionAdd, spanStored},
		{KindReactionRemove, spanStored},
		{KindCover, spanStored},
		{KindEdit, spanStored},
		{KindAttachmentBody, spanStored},
		{0x3F, spanStored},
		{KindDelivered, spanTransient},
		{KindReadThrough, spanTransient},
		{KindTypingStart, spanTransient},
		{KindTypingStop, spanTransient},
		{0x7F, spanTransient},
		{0x80, spanUnassigned},
		{0xFE, spanUnassigned},
		{KindEscape, spanEscape},
	} {
		if got := spanOf(one.kind); got != one.want {
			t.Errorf("%s is in band %d, want %d", one.kind, got, one.want)
		}
	}
	// and the two boundaries, walked rather than named, so that an off-by-one in either
	// comparison is red here and not in a build six months from now
	for kind := 0x01; kind <= 0xFF; kind += 1 {
		want := spanUnassigned
		switch {
		case kind <= 0x3F:
			want = spanStored
		case kind <= 0x7F:
			want = spanTransient
		case kind == 0xFF:
			want = spanEscape
		}
		if got := spanOf(ContentKind(kind)); got != want {
			t.Fatalf("code 0x%02x is in band %d, want %d", kind, got, want)
		}
	}
}

// THE EMOJI CHECK IS THE HALF THE STANDARD LIBRARY CAN HONESTLY MAKE, AND THE GAP IS NAMED.
//
// This case asserts BOTH halves: what is refused, and -- deliberately -- what is NOT. The second
// list is not a wish list. It is what a reader of checkEmoji must know is getting through, so that
// "reactions are validated" is never read as more than it is. Closing it is open item M1-41: a
// UAX-29 segmenter plus the pinned Unicode version's emoji table, which is a dependency decision
// and not a `go get`.
func TestTheEmojiCheckRefusesWhatItCanAndSaysWhatItCannot(t *testing.T) {
	target := aTarget(0x44)
	for _, one := range []struct {
		name  string
		emoji string
	}{
		{"an empty emoji", ""},
		{"an emoji over the 64 octet cap", strings.Repeat("u", MaxEmojiOctets+1)},
		{"a 4 octet emoji repeated past the cap", strings.Repeat("👍", 17)},
	} {
		if _, err := encodeReaction(KindReactionAdd, target, one.emoji); !errors.Is(err, ErrEmojiRefused) {
			t.Errorf("%s: %v, want ErrEmojiRefused", one.name, err)
		}
	}
	// invalid UTF-8 cannot be built through a Go string literal the compiler would reject, so it
	// is the raw octets, through the parser, which is the side a hostile sender reaches
	if _, verdict, _ := parseStored(append(append([]byte{byte(KindReactionAdd)}, target...), 0x80)); verdict != ContentMalformed {
		t.Errorf("an emoji that is not utf8: verdict %s, want malformed", verdict)
	}

	// WHAT IS NOT CHECKED, ASSERTED SO THAT THE DAY M1-41 IS CLOSED THIS CASE GOES RED AND IS
	// REWRITTEN, rather than a comment quietly becoming false.
	for _, notAnEmoji := range []string{
		"ab",                                // two grapheme clusters, and neither is an emoji
		"u",                                 // one ordinary letter
		"́",                                 // a lone combining acute, which is not a cluster of its own
		"👍👍",                                // two clusters
		strings.Repeat("u", MaxEmojiOctets), // 64 octets of ordinary text, exactly at the cap
	} {
		if _, err := encodeReaction(KindReactionAdd, target, notAnEmoji); err != nil {
			t.Errorf("checkEmoji has started refusing %q (%v); M1-41 may be closed, and if it is, "+
				"this case and checkEmoji's comment are both now wrong", notAnEmoji, err)
		}
	}
}

// AN ENCODER REFUSES WHAT THIS PACKAGE WOULD NOT READ BACK, AND IT REFUSES IT BEFORE THE SEAL.
//
// Rule R-c is the one these are about: a reference to another message is its RAW 32 octet
// message_id, so a caller that passed a record id, a hex string or a truncated id is refused at the
// encode rather than sealing a body nothing in the group can resolve.
func TestAnEncoderRefusesAReferentThatIsNotAMessageId(t *testing.T) {
	for _, one := range []struct {
		name   string
		target []byte
	}{
		{"no target at all", nil},
		{"a short target", aTarget(0x55)[:MessageIdBytes-1]},
		{"a long target", append(aTarget(0x55), 0x00)},
		{"a hex spelling of one", []byte(strings.Repeat("55", MessageIdBytes))},
	} {
		if _, err := encodeTombstone(one.target); !errors.Is(err, ErrContentMalformed) {
			t.Errorf("encodeTombstone over %s: %v", one.name, err)
		}
		if _, err := encodeReaction(KindReactionAdd, one.target, "👍"); !errors.Is(err, ErrContentMalformed) {
			t.Errorf("encodeReaction over %s: %v", one.name, err)
		}
		if _, err := encodeReply(one.target, "an answer"); !errors.Is(err, ErrContentMalformed) {
			t.Errorf("encodeReply over %s: %v", one.name, err)
		}
	}
	// and an empty text, which is a tail R-d requires to be non-empty and which the encoder
	// therefore must not seal
	if _, err := encodeText(""); !errors.Is(err, ErrContentMalformed) {
		t.Errorf("encodeText over an empty string: %v", err)
	}
	if _, err := encodeReply(aTarget(0x55), ""); !errors.Is(err, ErrContentMalformed) {
		t.Errorf("encodeReply over an empty text: %v", err)
	}
	// and a reaction under a kind that is not one, which is the guard that keeps the op folded
	// into the code rather than passed beside it
	if _, err := encodeReaction(KindText, aTarget(0x55), "👍"); !errors.Is(err, ErrContentMalformed) {
		t.Errorf("encodeReaction under TEXT: %v", err)
	}
}

// THE TEXT CEILING IS THE MEASURED COLUMN MINUS THE KIND OCTET, AND THE PLAINTEXT IS WHAT THE RUNG
// HOLDS.
//
// connect measures the APPLICATION PLAINTEXT's capacity at 65,334 octets
// (messagegroup.TestTheSizeLadderCostOfTheInnerFrameIsMeasuredHere). Under the ruling the plaintext
// is `kind ‖ body`, so the longest TEXT is one octet less and the longest REPLY text is 32 less
// again. This holds the two constants against that arithmetic from BOTH sides -- the ceiling
// encodes and one octet past it is refused -- because every clause of a "too long" case passes for
// a ceiling of zero.
func TestTheTextCeilingIsTheMeasuredPlaintextLessTheKindOctet(t *testing.T) {
	const measuredPlaintextCapacity = 65334
	if MaxTextOctets != measuredPlaintextCapacity-1 {
		t.Errorf("MaxTextOctets is %d and connect measures %d octets of application plaintext, one of which is the kind",
			MaxTextOctets, measuredPlaintextCapacity)
	}
	if MaxReplyTextOctets != MaxTextOctets-MessageIdBytes {
		t.Errorf("MaxReplyTextOctets is %d, want %d", MaxReplyTextOctets, MaxTextOctets-MessageIdBytes)
	}

	ceiling, err := encodeText(strings.Repeat("u", MaxTextOctets))
	if err != nil {
		t.Fatalf("a text of exactly MaxTextOctets was refused: %v", err)
	}
	if len(ceiling) != measuredPlaintextCapacity {
		t.Errorf("the longest text seals %d octets of plaintext, and the rung holds %d",
			len(ceiling), measuredPlaintextCapacity)
	}
	if _, err := encodeText(strings.Repeat("u", MaxTextOctets+1)); !errors.Is(err, ErrTextTooLong) {
		t.Errorf("one octet past the ceiling answered %v, want ErrTextTooLong", err)
	}

	reply, err := encodeReply(aTarget(0x66), strings.Repeat("u", MaxReplyTextOctets))
	if err != nil {
		t.Fatalf("a reply of exactly MaxReplyTextOctets was refused: %v", err)
	}
	if len(reply) != measuredPlaintextCapacity {
		t.Errorf("the longest reply seals %d octets of plaintext, and the rung holds %d",
			len(reply), measuredPlaintextCapacity)
	}
	if _, err := encodeReply(aTarget(0x66), strings.Repeat("u", MaxReplyTextOctets+1)); !errors.Is(err, ErrTextTooLong) {
		t.Errorf("one octet past the reply ceiling answered %v, want ErrTextTooLong", err)
	}
}
