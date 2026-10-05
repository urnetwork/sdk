package urmessage

import (
	"fmt"
	"unicode/utf8"

	"github.com/urnetwork/connect/message"
)

// ── the content envelope ─────────────────────────────────────────────────────────────────────
//
// WHAT AN APPLICATION PLAINTEXT IS, and it is the whole of the 2026-09-17 ruling's section 1:
//
//	plaintext := u8 kind ‖ body(kind)
//
// These are exactly the octets handed to (*mls.Group).ProtectBound as bodyPlain
// (connect/messagegroup/mlsframe.go:253, :265) and exactly the octets OpenRecord returns as its
// second value (connect/messagegroup/seal.go:905). CONNECT NEVER PARSES THEM AND CHANGES BY ZERO
// LINES: both plaintexts are opaque to it at all four of its own call sites (seal.go:133, :160;
// mlsframe.go:252-253; keyschedule.go:350-352 passes head_plain through raw), so this file is the
// only reader of a body in the system.
//
// WHY THE CODEC IS HERE AND NOT IN connect/messagegroup, measured rather than preferred. The
// unknown-kind rule is an ACCOUNTING rule about the walk -- "keeps its position, is NOT a fail(),
// does not count toward ErrRecordAbandoned" -- and fail(), resolve() and [ErrRecordAbandoned] exist
// only in [Group.openPageLocked]. OpenRecord has ONE error channel and every error in that package
// is fatal by construction (connect/messagegroup/seal.go:618-624, open item M1-15), so a codec
// there could answer a parsed body and could NOT answer "this record keeps its position and is not
// a failure". Spec A section 2.2's tombstone.go and reaction.go rows are struck by the same ruling,
// narrowly: their own stated rationale is emoji VALIDATION, not dispatch, and that validation
// cannot be performed in connect without linking a UAX-29 implementation into the module that also
// carries the VPN dataplane.
//
// THE HEAD'S VERSION BYTE IS WHAT KEEPS A PRE-KINDS RECORD OUT OF THIS FILE. An sdk at eebd50c did
// `Text: string(bodyPlain)` unconditionally, so a kinded record handed to it renders
// `0x05 ‖ <32 octets> ‖ 👍` as a text line attributed to a real sender. headVersion 0x02 refuses
// every record written before this build, by strict equality, in decodeHead -- see record.go.

// ContentKind is the u8 at octet 0 of an application plaintext: the code that decides which grammar
// the rest of the plaintext is read under.
//
// THE CODE IS THE VERSION OF ITS OWN GRAMMAR (rule R-e). There is no body version octet, now or
// ever, and an incompatible change to a kind takes a NEW CODE rather than a flag inside an old one.
// That is what makes every later kind additive: adding one is a new value here, no head change, no
// ct_head width change, no record refused, and older builds keep the record's position and render a
// placeholder under the unknown-kind rule.
type ContentKind uint8

// The registry. The code VALUES are the design's recommendation; the ranges, the rules R-a..R-e and
// the layouts are the ruling.
//
// FOUR OF THESE ARE CODES WITHOUT BODIES IN THIS BUILD, and they are here rather than left out
// because a code that is absent from the registry is indistinguishable from a code nobody has
// assigned -- and the ranges, which are the half of the rule that decides a refusal, cannot be
// exercised by codes that do not exist. Each is blocked on something outside this package:
//
//   - ATTACHMENT (0x03) and ATTACHMENT_BODY (0x09): the blob plane, which is not built.
//   - EDIT (0x08): reserved by the design and unspecified in v1.
//   - DELIVERED (0x40), READ_THROUGH (0x41), TYPING_START (0x42), TYPING_STOP (0x43): the whole
//     TRANSIENT range is legal on EPH(0) ONLY, and THERE IS NO EPH(0) FAN-OUT CHANNEL: msgrepo's
//     api/submit.go:607 refuses an EPH(0) record with REASON_INTERNAL and InstallEphRoot has zero
//     non-test call sites. A receipt or a typing indicator cannot be SENT by anything in this tree,
//     so building a body for one would be building against nothing.
//
// A plaintext carrying one of these four codes is therefore answered [ContentUnsupported] on a
// stored class and [ContentDropped] on EPH(0), which is exactly what a v1 receiver owes a code a
// later build assigns.
const (
	// RESERVED, refused on every class, always. It mirrors ContentTypeApplication's refusal of
	// MLS's zero (connect/mls/framing.go:55-63), and it is what makes "the plaintext is empty"
	// and "the plaintext says nothing" the same refusal rather than two.
	KindReserved ContentKind = 0x00

	// ── 0x01..0x3F: the STORED range ─────────────────────────────────────────────────────
	//
	// Legal on PERMANENT, DURABLE, MEDIA and EPH(1..5). On EPH(0) a stored code is refused as
	// MALFORMED, because the sender broke a rule the code alone decides.
	KindText           ContentKind = 0x01
	KindReply          ContentKind = 0x02
	KindAttachment     ContentKind = 0x03
	KindTombstone      ContentKind = 0x04
	KindReactionAdd    ContentKind = 0x05
	KindReactionRemove ContentKind = 0x06
	KindCover          ContentKind = 0x07
	KindEdit           ContentKind = 0x08
	KindAttachmentBody ContentKind = 0x09

	// ── 0x40..0x7F: the TRANSIENT range ──────────────────────────────────────────────────
	//
	// Legal on EPH(0) and nowhere else. On any stored class a transient code is MALFORMED.
	KindDelivered   ContentKind = 0x40
	KindReadThrough ContentKind = 0x41
	KindTypingStart ContentKind = 0x42
	KindTypingStop  ContentKind = 0x43

	// ── 0xFF: the ESCAPE ─────────────────────────────────────────────────────────────────
	//
	// No escaped code is assigned in v1 and the SKELETON IS FIXED ANYWAY:
	//
	//	0xFF ‖ u8 kind2 ‖ body(0xFF, kind2)	kind2 != 0x00, kind2 != 0xFF
	//
	// A v1 receiver treats a 0xFF plaintext as an unknown kind, MUST know that the body begins
	// at OFFSET 2, and MUST NOT refuse it for shape. Fixing the skeleton now costs zero octets
	// and is what makes the reservation a reservation rather than one more unknown code.
	KindEscape ContentKind = 0xFF
)

// MessageIdBytes is the width of MASTER section 8.4.5's message_id, which is the ONE referent every
// kind that names another message uses. It is carried RAW and never LP'd (rule R-c): the id is
// fixed width, so rule R-a covers it, and the 4 octets an LP would spend are 4 octets of the 256 B
// rung, which is the rung the whole encoding is fighting for.
const MessageIdBytes = 32

// MaxEmojiOctets is spec A section 7.4a's cap on a reaction's emoji_utf8. The longest
// fully-qualified RGI sequence in Emoji 17.0 is 35 octets, so the cap rejects no emoji and the RUNG
// is what decides what a reaction costs.
const MaxEmojiOctets = 64

// ── the range rule ───────────────────────────────────────────────────────────────────────────

// kindSpan is which band of the u8 a code falls in, which is what fixes the retention classes the
// code is legal on. It is evaluated FIRST among the post-open checks, for known and unknown codes
// alike.
//
// NOTHING HERE IS READ BEFORE THE RECORD OPENS, and under this ruling that is free rather than a
// concession: the kind does not exist before self.handle.Unprotect returns
// (connect/messagegroup/mlsframe.go:397). Both operands of the rule arrive at one place covered by
// ONE Ed25519 signature -- the kind from the Protect plaintext, the retention class from AAD_body
// through aad_mls through FramedContentTBS -- so there is nothing to check across a seam.
type kindSpan int

const (
	spanReserved kindSpan = iota
	spanStored
	spanTransient
	spanUnassigned
	spanEscape
)

// spanOf is the range rule's table, written as comparisons on the code alone.
//
// IT IS COMPARISONS AND NOT ARITHMETIC ON PURPOSE. connect/message's
// TestClassBucketJoinIsConfinedToRecordGo scans this repo and bans the SHAPES that pack or unpack a
// retention wire byte -- any shift by four, any mask of the low nibble, and the ordinary operators
// beside an operand whose identifiers name the class or the bucket. A band of a u8 is not that
// byte and must not be written as though it were.
func spanOf(kind ContentKind) kindSpan {
	switch {
	case kind == KindReserved:
		return spanReserved
	case kind <= 0x3F:
		return spanStored
	case kind <= 0x7F:
		return spanTransient
	case kind == KindEscape:
		return spanEscape
	default:
		// 0x80..0xFE: UNASSIGNED. The range rule does not constrain them and the retention
		// class alone decides.
		return spanUnassigned
	}
}

// transientOnly is the one class the TRANSIENT range is legal on and the one class every stored
// code is refused on: EPH(0), the never-persisted bucket.
//
// IT IS UNREACHABLE FROM THIS BUILD IN BOTH DIRECTIONS TODAY, which is stated here rather than
// discovered: [Group.Send] seals DURABLE (group.go's send path), the walk opens DURABLE and skips
// every other class by name, and the server refuses an EPH(0) submit outright
// (msgrepo/api/submit.go:607). The rule is implemented anyway because it is the half of the range
// rule that decides a REFUSAL, and a rule with one arm built is a rule whose other arm is written
// the day somebody needs it, by somebody who was not here.
func transientOnly(retentionClass message.RetentionClass, ephBucket uint8) bool {
	return retentionClass == message.RetentionEph && ephBucket == 0
}

// ── what a parse answers ─────────────────────────────────────────────────────────────────────

// ContentVerdict is what [ParseContent] decided. It is a value and not an error because three of
// the four outcomes are not failures of this device and the walk owes each of them a different
// accounting.
type ContentVerdict int

const (
	// The plaintext is one this build knows and the entry is filled in.
	ContentParsed ContentVerdict = iota

	// The SENDER broke a rule the code alone decides: a body too short for its layout, trailing
	// octets after a layout with no tail, an empty required tail, a code of 0x00, or a code
	// outside its range's classes (rule R-d). It becomes a [GapMalformed] gap in the receive
	// walk: RESOLVED ONCE and never retried, because every refusal raised after OpenRecord
	// returns is a disagreement about GRAMMAR and the record already opened. See
	// [Group.openPageLocked].
	ContentMalformed

	// A CODE THIS BUILD DOES NOT KNOW, on a class its range allows. The record keeps its
	// position and its message_id, it is NOT a failure, it does not count toward
	// [ErrRecordAbandoned], it renders as one closed placeholder, and IT IS NEVER PARSED AS ANY
	// KNOWN KIND. It becomes a [GapUnsupported] gap in the receive walk, which is spec A section
	// 7.4's closed-set value for it; [Message.Kind] carries the code that arrived beside it.
	ContentUnsupported

	// An unknown code on EPH(0): dropped silently. Nothing was persisted, so there is no history
	// for the gap to be a hole in.
	ContentDropped
)

func (self ContentVerdict) String() string {
	switch self {
	case ContentParsed:
		return "parsed"
	case ContentMalformed:
		return "malformed"
	case ContentUnsupported:
		return "unsupported"
	case ContentDropped:
		return "dropped"
	}
	return fmt.Sprintf("content verdict %d", int(self))
}

// Content is one application plaintext, parsed.
//
// THE FIELDS ARE PER KIND AND THE KIND SAYS WHICH ONES MEAN ANYTHING. A reader that branched on a
// non-empty Text rather than on the Kind would read a REPLY's text off a TOMBSTONE the day a layout
// grew one.
type Content struct {
	// The code at octet 0.
	Kind ContentKind

	// The octets after the code: from offset 1, and from OFFSET 2 for the 0xFF escape. It is
	// here so that a build which cannot parse a body can still say how long it was and where it
	// began, which is the whole content of the escape's reservation.
	Body []byte

	// TEXT and REPLY: the utf8 tail.
	Text string

	// REPLY's reply_to, and TOMBSTONE's and both REACTIONs' target: a raw 32-octet message_id.
	Target []byte

	// REACTION_ADD and REACTION_REMOVE: the emoji as the reactor's device sent it, raw.
	Emoji string

	// The 0xFF escape's kind2, when the plaintext carried one. It is NOT a [ContentKind] of the
	// registry above: the escape opens a SECOND code space and a kind2 of 0x01 is not TEXT.
	EscapedKind uint8
}

// ── the parse ────────────────────────────────────────────────────────────────────────────────

// ParseContent reads one application plaintext under the class it arrived on.
//
// IT IS THE ONE ENTRY POINT, and it takes the retention class and the eph bucket because the range
// rule's two operands are the code and the class -- neither decides alone. It sits directly above
// the OpenRecord call in [Group.openPageLocked] and directly below the [Message] construction.
//
// WHAT COMES BACK. On [ContentParsed] the entry is the body, filled in per kind. On
// [ContentUnsupported] the entry carries the CODE and the raw body and nothing else, because a
// build that does not know a code must not guess at its layout -- the placeholder the walk renders
// is built from the Kind. On [ContentMalformed] and [ContentDropped] there is no entry: one is a
// refusal and the other is a record that leaves no trace. The error is never nil except on
// [ContentParsed], and it is a sentence rather than a code so that a refusal in a log says which
// clause refused.
func ParseContent(plaintext []byte, retentionClass message.RetentionClass, ephBucket uint8) (*Content, ContentVerdict, error) {
	transient := transientOnly(retentionClass, ephBucket)

	// A ZERO-LENGTH APPLICATION PLAINTEXT HAS NO KIND. It is refused before the table is
	// consulted, because every branch below reads octet 0.
	if len(plaintext) == 0 {
		return nil, ContentMalformed, fmt.Errorf("%w: the application plaintext is empty, so it carries no kind",
			ErrContentMalformed)
	}
	kind := contentKindOf(plaintext)

	// THE RANGE RULE, FIRST AMONG THE POST-OPEN CHECKS, for known and unknown codes alike.
	switch spanOf(kind) {
	case spanReserved:
		return nil, ContentMalformed, fmt.Errorf("%w: kind 0x00 is reserved and is refused on every retention class",
			ErrContentMalformed)
	case spanStored:
		if transient {
			return nil, ContentMalformed, fmt.Errorf("%w: kind %s is a stored kind and this record is EPH(0), which is never persisted",
				ErrContentMalformed, kind)
		}
	case spanTransient:
		if !transient {
			return nil, ContentMalformed, fmt.Errorf("%w: kind %s is a transient kind and is legal on EPH(0) only",
				ErrContentMalformed, kind)
		}
	}

	// THE ESCAPE, WHOSE SHAPE IS FIXED AND WHOSE CODES ARE NOT. A v1 receiver knows where the
	// body begins and refuses NOTHING about it: a truncated escape, an escaped 0x00 and an
	// escaped 0xFF are all shapes of a code this build does not have, and refusing them here
	// would be this build having an opinion about a layout a later ruling owns. What it MUST NOT
	// do is read kind2 as a code of the registry above.
	if kind == KindEscape {
		if transient {
			return nil, ContentDropped, fmt.Errorf("%w: an escaped kind on EPH(0)", ErrContentDropped)
		}
		entry := &Content{Kind: kind, Body: []byte{}}
		if 2 <= len(plaintext) {
			entry.EscapedKind = plaintext[1]
			entry.Body = plaintext[2:]
		}
		return entry, ContentUnsupported, fmt.Errorf("%w: the 0xFF escape, kind2 0x%02x, %d octets of body",
			ErrContentUnsupported, entry.EscapedKind, len(entry.Body))
	}

	body := plaintext[1:]
	parse, known := contentParsers[kind]
	if !known {
		if transient {
			return nil, ContentDropped, fmt.Errorf("%w: kind %s on EPH(0)", ErrContentDropped, kind)
		}
		return &Content{Kind: kind, Body: body}, ContentUnsupported,
			fmt.Errorf("%w: kind %s, %d octets of body", ErrContentUnsupported, kind, len(body))
	}
	entry, err := parse(body)
	if err != nil {
		return nil, ContentMalformed, fmt.Errorf("%w: kind %s: %w", ErrContentMalformed, kind, err)
	}
	entry.Kind = kind
	entry.Body = body
	return entry, ContentParsed, nil
}

// contentKindOf is the code at octet 0, and [KindReserved] for a plaintext that has no octet 0.
//
// IT IS ONE READER OF OCTET 0 AND NOT TWO. [ParseContent] reads it to dispatch; the receive walk
// reads it again to say what code a MALFORMED record arrived under, because the parse answers no
// entry for one and a gap still owes a reader the code it was refused on.
//
// AN EMPTY PLAINTEXT ANSWERS [KindReserved] AND THAT IS NOT A SUBSTITUTION. 0x00 is the code that is
// refused on every retention class, always, and the registry above already rules that "the plaintext
// is empty" and "the plaintext says nothing" are ONE refusal rather than two -- so the code that
// means "no grammar this build will read" is exactly what was concluded about an empty one.
func contentKindOf(plaintext []byte) ContentKind {
	if len(plaintext) == 0 {
		return KindReserved
	}
	return ContentKind(plaintext[0])
}

// contentParsers is the layouts this build reads. A code in the registry and NOT in this table is a
// code this build knows the name of and not the grammar of, which is the unsupported answer and not
// a malformed one.
var contentParsers = map[ContentKind]func(body []byte) (*Content, error){
	KindText:           parseTextBody,
	KindReply:          parseReplyBody,
	KindTombstone:      parseTombstoneBody,
	KindReactionAdd:    parseReactionBody,
	KindReactionRemove: parseReactionBody,
	KindCover:          parseCoverBody,
}

// TEXT: utf8 text (tail, >= 1).
func parseTextBody(body []byte) (*Content, error) {
	text, err := tailText(body)
	if err != nil {
		return nil, err
	}
	return &Content{Text: text}, nil
}

// REPLY: reply_to[32] ‖ utf8 text (tail, >= 1).
func parseReplyBody(body []byte) (*Content, error) {
	if len(body) < MessageIdBytes {
		return nil, fmt.Errorf("%d octets, and a reply_to is %d before the text begins", len(body), MessageIdBytes)
	}
	text, err := tailText(body[MessageIdBytes:])
	if err != nil {
		return nil, err
	}
	return &Content{Target: append([]byte(nil), body[:MessageIdBytes]...), Text: text}, nil
}

// TOMBSTONE: target[32], and nothing after it.
func parseTombstoneBody(body []byte) (*Content, error) {
	if len(body) != MessageIdBytes {
		return nil, fmt.Errorf("%d octets, want exactly %d: a tombstone is one message_id and has no tail",
			len(body), MessageIdBytes)
	}
	return &Content{Target: append([]byte(nil), body...)}, nil
}

// REACTION_ADD and REACTION_REMOVE: target[32] ‖ emoji (tail, 1..64). The op folds into the kind,
// which is what buys the layout its 26 octets of emoji on the 256 B rung.
func parseReactionBody(body []byte) (*Content, error) {
	if len(body) < MessageIdBytes {
		return nil, fmt.Errorf("%d octets, and a target is %d before the emoji begins", len(body), MessageIdBytes)
	}
	emoji := body[MessageIdBytes:]
	if err := checkEmoji(emoji); err != nil {
		return nil, err
	}
	return &Content{Target: append([]byte(nil), body[:MessageIdBytes]...), Emoji: string(emoji)}, nil
}

// COVER: empty. The rung hides the length, so a one-octet COVER is indistinguishable by size from a
// 58-octet text -- which is the whole of what it is for, and is why an octet after it is a refusal
// rather than something to ignore.
func parseCoverBody(body []byte) (*Content, error) {
	if len(body) != 0 {
		return nil, fmt.Errorf("%d octets, want 0: a cover record's body is empty", len(body))
	}
	return &Content{}, nil
}

// tailText is rule R-b's tail, for the two kinds whose last field is utf8 text: it runs to the end
// of the plaintext, it is REQUIRED to be non-empty, and it must be valid UTF-8.
//
// THE TAIL IS SAFE BECAUSE THE OPENER RETURNS EXACTLY THE OCTETS SEALED. padBody writes
// LP(bodyPlain) into a rung-sized buffer (connect/messagegroup/seal.go:1045-1062) and unpadBody
// reads back LP(plaintext) and nothing else (:1076-1091), so "to the end" is a length the sender
// chose and not a length the padding left behind.
func tailText(tail []byte) (string, error) {
	if len(tail) == 0 {
		return "", fmt.Errorf("the text tail is empty and at least one octet is required")
	}
	if !utf8.Valid(tail) {
		return "", fmt.Errorf("the %d octet text tail is not valid utf8", len(tail))
	}
	return string(tail), nil
}

// ── the emoji, and the half of its validation that is honest with the stdlib ─────────────────

// checkEmoji is spec A section 7.4a's validation AS FAR AS THE STANDARD LIBRARY CAN TAKE IT, and
// the gap is named rather than papered over.
//
// WHAT IS CHECKED: valid UTF-8, at least one octet, at most [MaxEmojiOctets].
//
// WHAT IS NOT CHECKED, AND IT IS THE LARGER HALF. Section 5.1 requires "exactly one extended
// grapheme cluster, and every codepoint drawn from the emoji set of the pinned Unicode version",
// on send AND on receipt. Go's standard library provides NO UAX #29 segmentation, so neither half
// is reachable from here: this function will accept "ab", a lone combining mark, a ZWJ sequence
// that is not RGI, and any 64 octets of ordinary text.
//
// WHY IT IS NOT BUILT HERE. That is open item M1-41, verbatim: "REACTION validation needs
// segmentation Go does not have ... so this is a dependency decision -- a new module, or a
// hand-rolled subset plus the pinned tables -- and the Unicode version is not pinned anywhere this
// plan could find." The ruling MOVES that decision from connect to sdk, where a Unicode table is an
// ordinary cost; it does not take it. What it would cost, so the next person prices it rather than
// rediscovers it: a UAX-29 grapheme segmenter plus a vendored emoji-data table for the pinned
// version (owner choice 4 names the version; this pass measured against Emoji 17.0), in a module
// that ships inside the mobile SDK.
//
// AND THE GROUPING KEY IS NOT COMPUTED EITHER, for the same reason. Section 5.3 folds a reaction to
// (NFC, skin-tone modifiers and variation selectors removed) before it is grouped, and that needs
// normalisation tables this module does not carry. [Group.React] and [Group.Unreact] therefore
// group on the RAW octets, so two spellings of one emoji are two reactions rather than one.
//
// AN HONEST PARTIAL CHECK WITH A NAMED GAP IS WHAT THIS IS. The alternative -- a hand-rolled
// "looks like an emoji" test -- would be a check that passes a review and refuses real emoji, and
// it would be the thing a later reader trusts instead of reading this comment.
func checkEmoji(emoji []byte) error {
	if len(emoji) == 0 {
		return fmt.Errorf("the emoji tail is empty and a reaction carries one")
	}
	if MaxEmojiOctets < len(emoji) {
		return fmt.Errorf("the emoji is %d octets and section 7.4a caps it at %d", len(emoji), MaxEmojiOctets)
	}
	if !utf8.Valid(emoji) {
		return fmt.Errorf("the %d octet emoji is not valid utf8", len(emoji))
	}
	return nil
}

// ── the encoders ─────────────────────────────────────────────────────────────────────────────
//
// ONE ENCODING PER BODY (rule R-d), which is why these are the only writers and why each one
// refuses rather than truncates: a body this package would not READ back is a body it must not
// seal, and the refusal has to happen before [Group.Send]'s seal, which is the irreversible half.

// encodeText is TEXT's plaintext.
func encodeText(text string) ([]byte, error) {
	if len(text) == 0 {
		return nil, fmt.Errorf("%w: a text record carries at least one octet", ErrContentMalformed)
	}
	if MaxTextOctets < len(text) {
		return nil, fmt.Errorf("%w: %d octets, and the largest inline rung carries %d",
			ErrTextTooLong, len(text), MaxTextOctets)
	}
	plaintext := make([]byte, 0, 1+len(text))
	plaintext = append(plaintext, byte(KindText))
	return append(plaintext, text...), nil
}

// encodeReply is REPLY's plaintext: the parent's raw message_id, then the text as the tail.
func encodeReply(replyTo []byte, text string) ([]byte, error) {
	if err := checkTarget(replyTo); err != nil {
		return nil, err
	}
	if len(text) == 0 {
		return nil, fmt.Errorf("%w: a reply carries at least one octet of text", ErrContentMalformed)
	}
	if MaxReplyTextOctets < len(text) {
		return nil, fmt.Errorf("%w: %d octets of reply text, and the largest inline rung carries %d beside a 32 octet reply_to",
			ErrTextTooLong, len(text), MaxReplyTextOctets)
	}
	plaintext := make([]byte, 0, 1+MessageIdBytes+len(text))
	plaintext = append(plaintext, byte(KindReply))
	plaintext = append(plaintext, replyTo...)
	return append(plaintext, text...), nil
}

// encodeReaction is REACTION_ADD's and REACTION_REMOVE's plaintext. The op is the code.
func encodeReaction(kind ContentKind, target []byte, emoji string) ([]byte, error) {
	if kind != KindReactionAdd && kind != KindReactionRemove {
		return nil, fmt.Errorf("%w: %s is not a reaction", ErrContentMalformed, kind)
	}
	if err := checkTarget(target); err != nil {
		return nil, err
	}
	if err := checkEmoji([]byte(emoji)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmojiRefused, err)
	}
	plaintext := make([]byte, 0, 1+MessageIdBytes+len(emoji))
	plaintext = append(plaintext, byte(kind))
	plaintext = append(plaintext, target...)
	return append(plaintext, emoji...), nil
}

// encodeTombstone is TOMBSTONE's plaintext: one raw message_id and nothing else.
func encodeTombstone(target []byte) ([]byte, error) {
	if err := checkTarget(target); err != nil {
		return nil, err
	}
	plaintext := make([]byte, 0, 1+MessageIdBytes)
	plaintext = append(plaintext, byte(KindTombstone))
	return append(plaintext, target...), nil
}

// encodeCover is COVER's plaintext: the code and nothing else.
//
// IT IS UNUSED BY ANY SEND PATH IN THIS BUILD AND THAT IS THE RULING'S SPLIT, not an omission.
// COVER-as-a-code is sdk's; COVER-as-traffic-policy -- when one is emitted, at which rung, against
// which real message it is hiding -- is connect/messagegroup's, because it needs the rung and the
// record key and this package has neither. sdk decides what a cover record SAYS; messagegroup
// decides when one is SENT and how big it is.
func encodeCover() []byte {
	return []byte{byte(KindCover)}
}

// checkTarget is rule R-c's referent, checked on the way IN: a reference to another message is its
// raw 32-octet message_id, so a caller that passed a record id, a hex string or a truncated id is
// refused here rather than sealing a body nothing can resolve.
func checkTarget(target []byte) error {
	if len(target) != MessageIdBytes {
		return fmt.Errorf("%w: a message_id is %d octets and this one is %d",
			ErrContentMalformed, MessageIdBytes, len(target))
	}
	return nil
}

// String names a code rather than printing a number, so a refusal reads as a sentence. An
// unassigned code prints as its value, which is the only true thing to say about it.
func (self ContentKind) String() string {
	if name, named := kindNames[self]; named {
		return name
	}
	return fmt.Sprintf("0x%02x", uint8(self))
}

var kindNames = map[ContentKind]string{
	KindReserved:       "RESERVED(0x00)",
	KindText:           "TEXT(0x01)",
	KindReply:          "REPLY(0x02)",
	KindAttachment:     "ATTACHMENT(0x03)",
	KindTombstone:      "TOMBSTONE(0x04)",
	KindReactionAdd:    "REACTION_ADD(0x05)",
	KindReactionRemove: "REACTION_REMOVE(0x06)",
	KindCover:          "COVER(0x07)",
	KindEdit:           "EDIT(0x08)",
	KindAttachmentBody: "ATTACHMENT_BODY(0x09)",
	KindDelivered:      "DELIVERED(0x40)",
	KindReadThrough:    "READ_THROUGH(0x41)",
	KindTypingStart:    "TYPING_START(0x42)",
	KindTypingStop:     "TYPING_STOP(0x43)",
	KindEscape:         "ESCAPE(0xFF)",
}
