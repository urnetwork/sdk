package urmessage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
	"github.com/urnetwork/connect/protocol"
)

// GroupIdBytes is the width of the identifier the server keys its rows by, and the width a record
// header carries. It is the same 32 octets on both sides and a group id of any other width names
// nothing.
const GroupIdBytes = 32

// The alg id an EpochAttachment announces: 0x0031, HKDF-SHA-256, which is what derived write_key
// and read_key out of storage_root.
//
// connect/message holds the same number in an UNEXPORTED table (attachmentAlgIds), and refuses an
// attachment that announces any other, so this is a second site by construction of the visibility
// rules exactly as storageExporterLabel is. The change connect owes is to export it.
const epochAttachmentAlgId uint16 = 0x0031

// What an alpha wrap record carries, in the clear inside its own AEAD, so that a reader who finds
// one knows what it is and what it is not.
//
// 6.1 publishes an epoch by fanning a wrap out to every member and then closing the fan-out with a
// marker, and the server will not accept an ordinary record until the marker lands. In the full
// design a wrap is how a member is handed the epoch's secret. IN THE ALPHA IT IS NOT: the epoch's
// key schedule is derived from the MLS exporter on both sides, and the material a joiner needs
// travels in the MLS Welcome. So these records carry NO KEY MATERIAL, they are the ceremony the
// server's step (2) requires, and a device that cannot process a Welcome cannot join this build
// however many wraps it reads.
const alphaWrapBody = "urmessage/v1 alpha epoch wrap: no key material, the epoch secret travels in the mls welcome"

// The body of the marker that closes 6.1's fan-out.
const alphaEpochCompleteBody = "urmessage/v1 alpha epoch complete"

// How many 4.3.4 pages one [Group.Receive] will walk before it stops and SAYS it stopped.
//
// It is a bound and not a limit on history: at the advertised default of 512 records per fetch
// this is more records than the alpha can produce, and reaching it means either a group with an
// enormous backlog or a server that is paging a client in circles. Either way the answer is the
// same and it is the whole reason the constant exists: [Group.Receive] returns what it read AND
// [ErrFetchIncomplete], so "that is all there is" and "I stopped early" are two readings.
//
// A LOOP WITH NO BOUND WOULD BE THE WORSE FAILURE. A server that answered complete=false forever
// would hang the call rather than answer it, and a UI holding that call would look like a network
// problem instead of like a server problem.
const maxFetchPages = 1024

// The page size a walk ASKS FOR on its first fetch, which is spec B §4.3.1's own advertised
// `max_records_per_fetch` and is therefore what the server would have used for a request that
// named none.
//
// IT IS A STARTING POINT AND NOT A BOUND: see [Group.Receive]'s REASON_OVERSIZE arm, which halves
// it when a page of this many records is more than `max_response_bytes` will carry. The two
// server bounds are a COUNT and a SIZE and nothing in the protocol relates them.
const defaultFetchLimit = 512

// Message is one entry of the conversation: what one record SAYS, after the content envelope has
// been read.
//
// IT IS NOT ONE RECORD. Three kinds of record produce no Message at all -- a REACTION, a TOMBSTONE
// and a COVER -- because a reaction is a change to another message and a cover is a change to
// nothing. Their effects arrive here as [Message.Reactions] and [Message.Deleted], on the message
// they name, whenever that message is present. See [Group.noteEffectLocked] for the ordering
// problem that makes "whenever" the right word.
type Message struct {
	// The server's own id for the record: per group, gapless, and the cursor a later fetch
	// resumes from. Zero on a message this device has sent and the server has not yet numbered.
	RecordId uint64

	// 3.1's sender_handle, 16 octets. It is the routing identity of the member that sealed the
	// record and is not a name: the alpha has no identity system.
	//
	// IT IS NOT AN ATTRIBUTION AND SINCE LEDGER ITEM 245 THIS SAYS SO. SenderHandle(group_handle_key,
	// leaf) takes NO epoch and no identity and group_handle_key never rotates, so a newcomer that
	// lands on a leaf a removed member stood at carries the removed member's sixteen octets, byte
	// for byte. Two occupants of one leaf are one value here, forever. [Message.SenderIdentity] is
	// what tells them apart, and item 245's ruling accepts the LINKABILITY that leaves -- the
	// server and any archive holder see one label spanning two members -- because keys and nonces
	// are per epoch and the handle authenticates nothing.
	SenderHandle []byte

	// THE CREDENTIAL IDENTITY OF THE MEMBER THAT SIGNED THIS RECORD, at the epoch it was SEALED
	// at, and this is the field a caller attributes a line by. A copy.
	//
	// IT COMES OFF THE MLS-AUTHENTICATED SIGNING LEAF AND NEVER OFF THE SIXTEEN PLAINTEXT OCTETS
	// BESIDE IT -- ledger item 242's ruling 24, and item 245's first repair. MASTER section 8.4.3's
	// R1 refuses any frame whose signing leaf's SenderHandle is not the one the record carries, so
	// a record that OPENED has named its leaf; this is that leaf's credential identity, read at the
	// record's own epoch through the same door [Message.SenderRoleAtSend] is, so a removed member's
	// line stays the removed member's after a newcomer has taken its leaf and its handle.
	//
	// IT IS EMPTY WHEN THE OPEN DID NOT HAPPEN OR THE EPOCH CANNOT BE ASKED: a [GapOutOfWindow]
	// gap carries none, for the reason [Message.SenderRoleAtSend] carries none, and the residual
	// where the role is undeterminable ([Stats.RoleUndeterminable]) carries none either -- the two
	// are one ask and cannot disagree.
	//
	// THE ONE EXCEPTION IS THIS DEVICE'S OWN, AND IT IS NOT AN OPEN THAT FILLS IT. Two roads set
	// this field to this device's own identity with nothing signed: [Group.openOwnFromCopyLocked],
	// which shows a line from the copy [Group.Send] kept, and the [GapOutOfWindow] road for a
	// record whose stream index and body_hash are ones THIS DEVICE sealed
	// ([Group.noteEpochGapLocked]). Both rest on the same statement -- "nobody can produce a
	// second ct_body under one SHA-256" -- which is about this device and not about a leaf, and
	// both are why [Message.Mine] can be true on a record that never opened without ever being
	// read off the handle.
	SenderIdentity []byte

	// True when this device sealed it.
	//
	// IT IS DECIDED ON [Message.SenderIdentity] AND NOT ON THE HANDLE, which is the same repair
	// one field up. A device that reads its own handle off a record and concludes "mine" is a
	// device that shows a removed member's history as its own the day it lands on that member's
	// leaf. The handle is still what the pre-open roads pre-filter on -- a copy can only be shown
	// for a record whose body_hash this device sealed -- and the answer delivered here is the one
	// the open authenticated. The residual is named where it is taken
	// ([Group.recordIsOwnLocked]): a record that opened at an epoch whose membership this device
	// can no longer ask falls back to the handle, which is what every build before this one did.
	//
	// AND ON THE ROAD WHERE NOTHING OPENS AT ALL IT IS DECIDED ON OCTETS THIS DEVICE PRODUCED.
	// A [GapOutOfWindow] record is one no key on this device reaches, so there is no open to read
	// an identity off -- and this field used to be the sixteen-octet handle there, which is the
	// whole of the harm above arriving through the one road that cannot open anything. It is now
	// the copy road's own test, one clause of it: this device sealed at that stream index and the
	// record carries the body_hash it sealed there. Its residual, stated: a record of this
	// device's own whose index it no longer holds -- one sealed by a build that kept no copies,
	// or by another copy of this folder -- is `false` here, and carries no identity either.
	Mine bool

	// THE ROLE THE SENDER HELD AT THE EPOCH THIS RECORD WAS SEALED AT, as the wire stable name:
	// "owner", "admin", "member" or "observer". Spec C section 5.6's
	// `MessageEntry.SenderRoleAtSend`, and ledger item 242's R4.
	//
	// IT IS A FACT ABOUT AN EPOCH AND NOT ABOUT NOW, which is the whole reason it is a field on the
	// message rather than a lookup at render time. Spec A rules where the answer comes from --
	// "READ FROM THE TRANSCRIPT-COVERED GROUP-CONTEXT EXTENSION OF THE SENDING EPOCH, never from
	// current membership" -- so a member demoted at a later epoch keeps the role it held on every
	// line it had already written, and one promoted later does not acquire the new role
	// retroactively. [Group.Members] answers the roles NOW, which is the wrong answer for a
	// historical message.
	//
	// IT IS CAPTURED AT THE OPEN (item 242's ruling 21) and never derived afterwards. Two reasons,
	// and the first decides it: the role must survive a restart that can no longer re-derive it --
	// an epoch aged past [messagegroup.PastEpochWindow] is one whose state mls has deleted, and the
	// record is still in this group's log. The second is cost: one field against two (the epoch and
	// the leaf) plus a seam call per render.
	//
	// IT IS EMPTY EXACTLY WHEN THE RECORD DID NOT OPEN. A [GapOutOfWindow] gap carries none,
	// because nothing on this device can say what a sender's role was at an epoch it holds no state
	// for -- and reading it off the CURRENT policy would be answering a question about a different
	// epoch. A malformed or an unsupported gap DID open and carries one like any other record. See
	// [Stats.RoleUndeterminable] for the residual case that is neither.
	//
	// "observer" IS THE ONE VALUE THAT ASKS A UI FOR ANYTHING. Spec C section 5.6: a receiving
	// client HIDES an observer's message -- collapsed to section 5.1's system row, "A message from
	// an observer was hidden.", expanding to the content with a warning. THE RECORD IS KEPT AND ITS
	// CONTENT IS INTACT (item 242's ruling 16): dropping it would be indistinguishable from a
	// record that never arrived, and it is not an eighth [GapReason] -- that set is closed at
	// seven. [Stats.HiddenObserver] is how many.
	SenderRoleAtSend string

	// The text.
	Text string

	// The sender's own clock reading, unix milliseconds, out of the head the AEAD authenticated.
	SentAtMs int64

	// MASTER section 8.4.5's message_id for this record: 32 octets, derived by
	// [messagegroup.GroupSession.MessageIdOf] from this group's epoch-zero group_handle_key and
	// three fields of the record's own plaintext header.
	//
	// IT IS THE NAME EVERY LATER KIND QUOTES. A reply, a reaction, a tombstone and a read cursor
	// all have to say WHICH message they are about, and [Message.RecordId] cannot be that name:
	// it is the SERVER's per-group counter, so a sender does not know it until the submit is
	// answered, and a device whose submit response was lost holds a message whose RecordId is
	// zero. message_id is a function of the record alone and both sides compute it from the
	// header they already hold.
	//
	// IT IS A NAME AND NOT AN AUTHENTICATION, carried through verbatim from the derivation's own
	// document rather than softened here: group_handle_key is group-shared, so any member can
	// compute any member's id at any index, including indices nobody has written yet. What makes
	// a message's id trustworthy is that the record it names OPENED, and opening is what MASTER
	// section 8.4.3's R1 and R2 decide.
	//
	// Every [Message] this package produces carries one, because every one of them is built
	// beside the header it is derived from.
	MessageId []byte

	// ── what the content envelope said ───────────────────────────────────────────────────

	// The code at octet 0 of the application plaintext: what grammar [Message.Text] and the
	// fields below were read under. See kind.go.
	//
	// A KIND THIS BUILD DOES NOT KNOW IS CARRIED HERE AS ITSELF, and that is the whole of the
	// unknown-kind rule's rendering obligation: the record kept its position and its
	// [Message.MessageId], [Message.Text] is empty, and a UI shows one closed placeholder
	// rather than a line of garbage attributed to a real sender. [ContentKind.String] names the
	// codes this registry has and prints the value of one it does not.
	//
	// ON A GAP IT IS THE CODE THE RECORD ARRIVED UNDER AND NOT WHAT THE RECORD IS. A malformed
	// REPLY carries [KindReply] here and is still a gap; what a reader branches on is
	// [Message.Gap], which is set on every gap and on nothing else. See [GapReason].
	Kind ContentKind

	// SET IFF THIS ENTRY IS A GAP: something is at this position in the conversation and this
	// build cannot show it. Empty on every message that is a message.
	//
	// IT IS THE FIELD A UI BRANCHES ON, and the reason it exists is that a gap and a blank line
	// were the same value before it. An unsupported record used to reach a caller as a [Message]
	// with an unknown [Message.Kind] and an EMPTY [Message.Text] -- indistinguishable, without
	// the registry in hand, from somebody sending nothing -- and a malformed one did not reach a
	// caller at all. Spec A section 7.4's `MessageEntry.GapReason` is this value; spec C section
	// 5.1's render table is the copy each one owes.
	Gap GapReason

	// REPLY only: the raw 32-octet message_id of the message being replied to. The quoted text
	// never travels -- the reply renders by looking its parent up -- and the parent may be
	// unavailable, deleted or not yet fetched.
	ReplyToId []byte

	// A TOMBSTONE from this message's OWN sender has been applied to it. The body is still in
	// [Message.Text]: this package refuses to decide what a UI does with a deleted line, and
	// the record itself is on the server either way.
	Deleted bool

	// The reactions standing on this message, rebuilt from every reaction record this group
	// holds for it whenever one arrives. Empty on a message nobody has reacted to.
	Reactions []Reaction
}

// GapReason is WHY a position in the conversation holds a gap rather than a message: spec A
// section 7.4's closed set, which this package renders into [Message.Gap].
//
// A GAP IS A FIRST-CLASS ENTRY AND NOT AN ERROR, which is the whole reason it is a field and not a
// refusal: something IS at this record id, this build cannot show it, and "a messenger that silently
// drops what it cannot read is a messenger that cannot be trusted to have shown you everything"
// (spec A section 7.4, verbatim). The position is kept, the message_id is kept, and the record after
// it arrives.
//
// THE SET IS CLOSED AT SEVEN AND THIS BUILD PRODUCES THREE. The three below are the three the
// receive walk can reach. THE OTHER FOUR ARE DELIBERATELY NOT DECLARED HERE -- a constant with no
// producer is a constant the next reader assumes is reachable, and each of these is waiting on
// something this package does not have:
//
//   - "expired"          a DESTROYED disappearing key. Needs the disappearing-message timer and
//     `DeleteGroupStateBefore`, and EPH(1..5) is not a class this walk opens.
//   - "not_a_member_yet" a record from before this device joined. A member added at a later epoch
//     holds no earlier epoch's schedule, so the store answers not-found for it and it reaches
//     [GapOutOfWindow], not a value of its own. Since ledger item 241 the walk CAN tell the two
//     apart -- an epoch below the window refuses by arithmetic and an epoch this device never
//     stood in refuses through the store -- and still renders one reason, because spec C's copy
//     for this value is the history grant's banner and the grant is not built; a reason with no
//     grant beside it would promise a UI something nothing can deliver yet.
//   - "withheld"         section 9.6's attestation refusal. The deployed server signs nothing;
//     see [Group.Receive] and S2-27.
//   - "no_wrap"          this device has no key wrap for the record's class yet. The wrap ceremony
//     is section 6.1's and no walk here reaches its absence as a gap.
//
// A further value for "the server erased the body" is OWED and does not exist in the closed set yet:
// that is ledger item 220, and a pruned DURABLE body refuses at the body-hash compare BEFORE either
// AEAD, so it is a pre-open refusal and is a fail() here today. It is deliberately not invented.
type GapReason string

const (
	// THE SENDER BROKE A RULE THAT IS ALREADY WRITTEN: [ContentMalformed]. It is NOT
	// [GapUnsupported], and the distinction is load-bearing in both directions -- a build that
	// called a future kind "malformed" would ACCUSE CORRECT SENDERS, and one that called a
	// genuinely malformed body "unsupported" would tell a user to upgrade out of a bug that no
	// upgrade fixes. Spec C section 5.1's copy for it carries NO upgrade affordance for exactly
	// that reason.
	GapMalformed GapReason = "malformed"

	// A CODE THIS BUILD DOES NOT KNOW, on a class its range allows: [ContentUnsupported]. The
	// record opened and its signature verified, so the sender did nothing wrong -- it is a newer
	// feature, and spec C section 5.1's copy for it is the one that offers the upgrade.
	GapUnsupported GapReason = "unsupported"

	// A RECORD SEALED AT AN EPOCH THIS DEVICE CANNOT OPEN ANY MORE -- OR NEVER COULD. Since ledger
	// item 241 a record from a PRIOR epoch is opened under that epoch's own schedule, rebuilt out
	// of the MLS state this device persisted when it stood there ([Group.pastHeadLocked] and
	// connect's [messagegroup.GroupSession.TrackSenderAt] are the two halves), so a member that
	// WAS in the group keeps its history across a membership change. What is left as a gap is
	// exactly what no schedule reaches: an epoch more than [messagegroup.PastEpochWindow] behind
	// this device's -- the line MLS itself deletes state below -- and an epoch this device holds
	// no state for, which is every epoch before it was admitted. The second is MLS's own answer
	// for a later joiner and item 241 rules it stays that way unless the group GRANTS history,
	// which is Spec A section 7's MessageHistoryGrant and is not built.
	//
	// IT IS A GAP AND NOT A fail(), and that is the decision A5 takes deliberately. Retrying it
	// three times spends three fetches on a disagreement no re-fetch repairs -- the epoch will not
	// come back -- and abandoning it names it [ErrRecordAbandoned], loudly, for a record that is a
	// known and expected consequence of a membership change rather than a fault. It sets no
	// `firstFailure`, so it does not hold the cursor and does not stop a restored group
	// reconciling. The two causes still share ONE reason: telling "before you were admitted"
	// from "too long ago" needs `not_a_member_yet`, which the grant's arrival will give a
	// producer; see [GapReason].
	GapOutOfWindow GapReason = "out_of_window"
)

// Reaction is one emoji standing on one message, from one member.
//
// THE REACTOR IS A sender_handle AND NOT A PERSON. The alpha has no identity system, so "the same
// reactor across one person's devices" is open item D7; until it is ruled a reactor is the leaf.
//
// THE EMOJI IS RAW AND IS NOT FOLDED TO A GROUPING KEY. Section 5.3 groups on (NFC, skin-tone
// modifiers and variation selectors removed), which needs normalisation tables this module does not
// carry -- see checkEmoji and open item M1-41 -- so two spellings of one emoji are two reactions
// here.
type Reaction struct {
	// The 16-octet sender_handle of the member who reacted.
	SenderHandle []byte

	// The emoji as that member's device sent it.
	Emoji string

	// True when this device sealed the reaction.
	Mine bool
}

// MaxTextOctets is the longest text [Group.Send] will seal, and it is a MEASURED number rather
// than a rung of the size ladder.
//
// WHERE IT COMES FROM. Since connect 4c030dc an application record's ct_body plaintext is an MLS
// PrivateMessage and the frame sits INSIDE the size rung, so the usable text per rung is the
// rung's own capacity less the frame's overhead. connect measures the whole column in
// messagegroup.TestTheSizeLadderCostOfTheInnerFrameIsMeasuredHere -- run on connect d368fea, it
// logs 59 / 826 / 3,898 / 16,186 / 65,334 usable, at 193 / 194 / 194 / 194 / 198 octets lost --
// and cp3b.TestEveryRecordTypeUrmessageSealsLandsOnTheRungItsBodyNeeds re-measures the same column
// through THIS package and a real server's own rows. This constant is the top of that column, and
// cp3b.TestTheTextCeilingRefusesBeforeItSpendsAnythingIrreversible is what holds it against a
// measurement rather than against this comment.
//
// THE OVERHEAD AS A FUNCTION OF LENGTH IS NOT RESTATED HERE, deliberately: connect's own
// mlsframe.go prose gives it as three steps -- 193 below 64, 194 below 16,384, 198 at or above --
// and that sentence is FALSE at P = 16,383, which connect's own applicationFrameOverhead table
// pins at 196 in the same file, in a case that passes. msgrepo ledger item 218 and MASTER §8.4.4's
// 2026-09-17 correction carry the four-band form. What this constant needs is the TOP of the
// capacity column and nothing else, so it takes the measured column and leaves the step function
// where it is measured.
//
// WHY THE REFUSAL IS HERE AND NOT LEFT TO THE SEALER, which is the whole reason the constant
// exists. messagegroup takes a CHEAP half of the ladder refusal before it reserves anything --
// bucketForBody over the CALLER's own length -- and that half passes for every body up to 65,532,
// because 65,532 is what the 64 KiB rung holds. The real bucket is chosen AFTER the frame exists,
// which is after the stream index has been reserved and after Protect has consumed an MLS
// generation. So a text of 65,335..65,532 octets -- connect's ledger open item 203, whose own
// TestABodyNoRungCouldHoldCostsNeitherAnIndexNorAGeneration measures the band by name -- used to
// pass through here, spend one DURABLE stream index and one MLS generation, and only then be
// answered [ErrTextTooLong]. Both are legal gaps and neither is recoverable, and a caller that
// retried a failed send spent another of each on every attempt.
//
// THE BAND IS 198 OCTETS WIDE AND IT IS NOT THE ONLY THING THIS REFUSES. Above 65,532 the sealer
// already refused for free. This makes the two answers one answer, taken in one place, before
// anything irreversible has happened -- so a send refused for length costs nothing however many
// times it is retried.
//
// AND IT IS ONE OCTET SHORT OF THE MEASURED COLUMN SINCE THE CONTENT ENVELOPE LANDED. What connect
// measures is the APPLICATION PLAINTEXT's capacity, 65,334; under the 2026-09-17 ruling the
// plaintext is `kind ‖ body`, so the kind octet comes out of the same budget and the longest TEXT
// this package will seal is 65,333. That is the whole of what the envelope costs a stored record,
// and it costs nothing at all except on a body whose plaintext sat EXACTLY on a rung boundary.
const MaxTextOctets = 65333

// MaxReplyTextOctets is the same ceiling for a REPLY, which spends 32 more octets of the plaintext
// on the raw message_id its text is an answer to (rule R-c).
//
// IT IS DERIVED AND NOT MEASURED, deliberately: a second literal here would be a second column to
// keep level with connect's, and the subtraction is the layout itself.
const MaxReplyTextOctets = MaxTextOctets - MessageIdBytes

// Stats is what a group has seen, so that "nothing arrived" and "something arrived and this build
// would not open it" are two readings rather than one silence.
type Stats struct {
	// Records the server answered a fetch with.
	Fetched uint64

	// Records OPENED into a [Message]: decrypted, and their inner MLS frame authenticated to the
	// member whose sender_handle they carry. This device's own records are never among them; see
	// [Stats.OpenedOwn].
	Opened uint64

	// Records skipped because they are 6.1's ceremony rather than a message: the founding commit,
	// the epoch's wraps, the marker that closes them.
	SkippedCeremony uint64

	// Records skipped because this device sealed them AND ALREADY HOLDS THEM: their record id
	// is in this group's log, so they are the ordinary echo of a send this process made.
	//
	// IT USED TO COUNT EVERY OWN RECORD AND THAT IS THE DEFECT IT WAS PART OF. A restored
	// group's log starts empty, so "this device sealed it" and "this device still has it" came
	// apart at exactly the moment a user reopened the app -- and a counter that moves on both
	// readings cannot tell a UI which one happened. The two are now two numbers.
	SkippedOwn uint64

	// Records this device sealed that became a [Message] because this group does NOT already hold
	// them: the whole of a restarted device's own half of the conversation, and a record whose
	// submit response was lost after the server had stored it.
	//
	// THEY ARE RENDERED FROM THE COPY THIS DEVICE KEPT AND ARE NEVER DECRYPTED, and the name is
	// older than that. Since connect 4c030dc an application record's body is an MLS
	// PrivateMessage, and a member cannot open its own: Protect spends a generation of the leaf's
	// own ratchet and MLS keeps no receiving ratchet for it (connect messagegroup OPENITEMS MG-4).
	// So what moves this is a record whose stream index and body_hash are the ones [Group.Send]
	// sealed and kept -- see [Group.openOwnFromCopyLocked] -- and [Stats.Opened] does NOT move
	// with it, because nothing was opened.
	OpenedOwn uint64

	// Records under this device's own sender_handle that this group's keys AUTHENTICATED and that
	// it CANNOT SHOW, because it keeps no copy of what it sealed at that index.
	//
	// A NUMBER HERE IS A HOLE IN THIS DEVICE'S OWN HALF OF THE CONVERSATION. The ordinary ways
	// to reach it: a state directory written before this build kept copies, and a copy of the
	// app-data folder meeting a record the original sealed after the copy was taken (which
	// [Group.Receive] also refuses as [ErrIdentityInUse]). The record is not a failure -- it is
	// this device's own, and it moves [Stats.FailedOpen] never -- and it is resolved past, so it
	// costs one MLS peek per Receive that re-reads it and nothing else.
	OwnWithoutCopy uint64

	// Records skipped because this group's log already holds them under that record id. It
	// moves when a fetch is REWOUND -- which is what a record that failed to open now causes,
	// see [Group.Receive] -- and it is what keeps that rewind from delivering a message twice.
	SkippedSeen uint64

	// Records that did not open after [maxRecordAttempts] fetches and are no longer asked for.
	// [Group.UnopenedRecords] is which ones. A number here is a hole in the conversation that
	// this build has stopped trying to fill, and it is the number that must stay zero.
	Unopened uint64

	// Fetch pages the server called COMPLETE while naming a `high_water_record_id` above every
	// record it handed over -- §4.3.4's own statement that it is holding records back. See
	// [Group.Receive] for the one honest server that also moves this.
	Omitted uint64

	// Records skipped because they are not a class this build opens.
	SkippedClass uint64

	// ── the device wrap that carries pq_secret[n+1] (ledger item 251, rulings 37 and 38) ──
	//
	// FOUR NUMBERS FOR FOUR STATES, and three of them are failures with a sentinel each. A
	// wrap that carries key material is the thing a member goes permanently dark without, in
	// BOTH directions, and a caller that can only learn about it by holding an error cannot
	// answer "is this happening to my users". These are what it reads instead.
	//
	// THEY ARE THIS PROCESS'S AND THEY RESET AT A RESTART, which is ordinary for a counter and is
	// said because the STATE they count does not reset: a group that went dark comes back dark,
	// off [GroupRecord.WrapDarkKind], with these four at zero. A caller looking for "is this
	// happening" reads the counters; a caller asking "is this group dark" reads the error
	// [Group.Send] and [Group.Receive] refuse with.

	// Device wraps addressed to THIS device that opened and were staged for the epoch they
	// deliver. In an unremarkable group this rises by exactly one per epoch change this device
	// did not commit itself, and a zero here across a commit is the first thing to look at.
	WrapOpened uint64

	// Epochs this device could not take a pq_secret for because NO wrap addressed to it
	// arrived and the secret it already held is not the one the epoch was opened with.
	// [ErrNoWrapForEpoch]. It is item 132's omission measured at the victim.
	WrapMissing uint64

	// Device wraps at this device's own wrap_target_handle that did NOT open.
	// [ErrWrapUnreadable].
	WrapUnreadable uint64

	// Device wraps that opened and were not the epoch's own secret: the fan-out of a commit
	// that lost its CAS race. [ErrOrphanWrap]. A number here with no [Stats.WrapMissing] beside
	// it is the healthy reading -- two committers raced, this device opened both wraps and used
	// the winner's. A number here WITH one is not a lesser failure than the others: see
	// [ErrOrphanWrap] for why the loser-only case is as permanent as a wrap that never came.
	//
	// AND THE HEALTHY READING IS ORDER-INDEPENDENT SINCE 2026-09-24, which it was not before and
	// which is what made it the reading this counter could not show. The resolution used to
	// RETURN at the winning candidate, so an orphan numbered after it was never reached and read
	// as zero -- and which of two racing committers wrote its fan-out first is arbitrary, so half
	// the race orderings reported nothing at all. Every candidate is judged before any is
	// answered now, and both orderings are driven by
	// TestAnOrphanIsCountedOnEveryArmOfTheResolutionAndInBothRaceOrderings.
	WrapOrphaned uint64

	// Records that OPENED and became a GAP rather than a message: the two values of [GapReason]
	// this build produces, counted apart because they are two different sentences about the
	// group and only one of them is anybody's fault.
	//
	// GapMalformed IS THE LOUD HALF OF LEDGER ITEM 224 AND IT IS WHY IT IS A COUNTER AT ALL.
	// Before it, a malformed body was a fail(): the cursor was held, the record was re-fetched
	// [maxRecordAttempts] times and then named by [ErrRecordAbandoned] -- three retries spent on
	// a disagreement about GRAMMAR, which no re-fetch can repair, and a loud error at the end of
	// them. The retries are gone and the record now resolves once, so this counter and
	// [Message.Gap] are the whole of what is left to be loud WITH: [Stats.FailedOpen] does not
	// move, [Stats.Unopened] does not move, and [Group.Receive] answers a nil error. A caller
	// that watches only the error no longer learns that a record could not be read, and that is
	// the cost of the repair, paid deliberately and written down here rather than discovered.
	//
	// GapUnsupported is not a fault in either direction: it is a member running a newer build.
	// A number here that keeps growing is this build getting old.
	GapMalformed   uint64
	GapUnsupported uint64

	// Records that became a [GapOutOfWindow] gap: sealed at an epoch no schedule on this device
	// reaches -- below the past epoch window, or before this device was admitted. See [GapReason]
	// and [Group.noteEpochGapLocked]. Since ledger item 241 a member that WAS there produces none
	// of these across a membership change; a later joiner produces one per pre-admission record,
	// which is the count the milestone measured as 602 and which stays 602 for the joiner.
	GapOutOfWindow uint64

	// Records that OPENED under a PRIOR epoch's schedule: sealed at an epoch this device has left,
	// and opened anyway because this device was a member then. Ledger item 241. It is a subset of
	// [Stats.Opened] counted separately so that "history survived the change" is a number and
	// not an absence of gaps.
	OpenedPastEpoch uint64

	// Records that became a LINE of the conversation whose sender was an OBSERVER at the epoch it
	// sealed them: [Message.SenderRoleAtSend] == "observer", ledger item 242's R4, spec C §5.6.
	//
	// A NUMBER HERE IS NOT AN ERROR AND IT IS NOT A DROP. The record opened, it is in this group's
	// log, its text is intact, and a UI collapses it to §5.1's system row with the content one
	// expansion away (ruling 16). What it IS is a member of this group running a build that does
	// not take R4's send refusal, because OBSERVER is enforced in the client and by the MLS
	// proposal rules and NOT by the server: an observer holds the group keys and can encrypt a
	// valid application message. Spec C's own settings copy says exactly that -- "this version of
	// URmessage cannot stop it at the server -- it can only hide the result" -- and this counter is
	// how often it had to.
	//
	// AN OBSERVER'S REACTION IS NOT COUNTED HERE, because it never becomes a line: it is counted
	// by [Stats.ObserverReactionRefused] and it is not applied at all (ruling 25). An observer's
	// TOMBSTONE is not counted here either and IS applied (ruling 26). The two counters are apart
	// because they answer two different questions and neither can answer the other's: this one is
	// how many ROWS a UI is being asked to collapse, and every one of them is in the log with a
	// position and a body to expand; that one is how many records produced NO row and NO change
	// anywhere, and there is nothing for a UI to draw. One number over both would be a number no
	// caller could use.
	HiddenObserver uint64

	// Reaction records REFUSED because their sender was an OBSERVER at the epoch it sealed them:
	// item 242's ruling 25, the receiving half of "read only".
	//
	// THE ASYMMETRY WITH [Stats.HiddenObserver] IS THE RULING AND IT IS ONE SENTENCE. A message is
	// KEPT because dropping it would hide that something was said, and this package's whole gap
	// design exists because silent omission is the one thing a messenger may not do -- but a
	// reaction that is not applied hides nothing, because the message it names is right there,
	// whole. No position in the conversation goes blank. So the reaction never reaches
	// [Message.Reactions] at any honest receiver, which is "read only" failing in the most visible
	// way this product has: on another member's line, where a chip a UI has no way to attribute to
	// an observer would otherwise stand.
	//
	// IT COUNTS RECORDS AND NOT APPLICATIONS, which is why it moves in [Group.noteEffectLocked]
	// and the refusal itself is in [Group.reapplyLocked]. An effect is applied once per REBUILD of
	// its target and a target is rebuilt every time another effect lands on it, so a counter at
	// the refusal would count a number about this device's walk order rather than about the group.
	// One record is one effect ([Group.noteEffectLocked]'s own rule, which a re-delivery after a
	// failed open goes through), so this is one per record, however many rebuilds refuse it and
	// whether or not its target ever arrives.
	//
	// A TOMBSTONE IS NOT COUNTED HERE BECAUSE IT IS NOT REFUSED (ruling 26): see
	// [contentEffect.isObserverReaction] for why an observer may still retract its own words.
	ObserverReactionRefused uint64

	// Records that OPENED and whose sender's role at the sending epoch could NOT be read, so
	// [Message.SenderRoleAtSend] is empty on a record that is otherwise whole.
	//
	// IT MUST STAY ZERO AND IT IS HERE BECAUSE "must" is not "does". The ask goes through
	// [messagegroup.GroupSession.RoleAt], which reads the SAME handle the open read (item 242's
	// ruling 17), so a door that answered the record cannot refuse to say who sent it -- with one
	// measured condition, written on that method: the sentence holds for an ask with NO EPOCH
	// INSTALL between it and the open. This package keeps the ask inside that window by taking it
	// in the same loop iteration as [messagegroup.GroupSession.OpenRecord], which is what ruling
	// 21's capture-at-open buys. A number here is that window having been left, or a store that
	// would not read a past epoch it had just served.
	//
	// IT IS NOT [Stats.GapOutOfWindow]'s counterpart and never moves with it. A record whose epoch
	// no schedule reaches never opens and is never asked about, so it contributes a gap and not a
	// number here; R4 introduces no new disappearance, and the records whose role is underivable
	// are a subset of the records that do not open.
	RoleUndeterminable uint64

	// Commits INGESTED into this group: §6.1 membership-change records this device processed,
	// authorized, applied and followed into the next epoch. One per epoch this device did NOT
	// author but was carried into. See [Group.ingestCommitLocked].
	Ingested uint64

	// Commits REFUSED by the receiving-client authorization check (MASTER §11, ledger item 242):
	// processed, judged against the role model's rules and this device's [CommitAuthorizer], and
	// not applied. The staged epoch is erased and this group stays at the epoch it was at. A
	// number here is a member of this group that committed something its role does not permit --
	// and, because the server has already advanced past it, a group this device can no longer
	// write to: a hostile committer can halt a group and cannot take it. It is a security event
	// and it is counted rather than logged, because this package logs nothing; [Group.Receive]
	// answers the refusal, wrapping [ErrCommitUnauthorized] and the rule, and this is the number
	// that persists past the call. A refused record is retried like any other ingest failure,
	// and the retry never reaches the rules: the first Process opened the commit's MLS frame and
	// spent that generation of the committer's ratchet, so every later walk is refused by mls at
	// Process as an ingest failure. One refusal moves this once.
	CommitRefused uint64

	// Commits THIS DEVICE was asked to make and REFUSED before building them: the committing
	// arm of MASTER §11, ledger item 242's R2. [Group.AddMemberAndPublish], [Group.SetRole] and
	// [Group.TransferOwnership] each judge the commit they are about to build against the same
	// rules every receiver judges an ingested one by ([authorizeCommit]), over the value the
	// commit WOULD produce, and a refusal here is a commit that was never built, never merged
	// and never published -- so nothing moved: not this device's epoch, not the server's, not
	// anybody else's. The counterpart of [Stats.CommitRefused] on the other arm; where that
	// number is a halted group, this one is a request this device's role did not permit, answered
	// at once and at no cost to the group. The call answers [ErrCommitUnauthorized] wrapping the
	// rule, exactly as a receiver would.
	CommitRefusedOwn uint64

	// ATTEMPTS to open a record from a member of this group that did not open -- one per
	// fetch, so a record retried [maxRecordAttempts] times moves this three times. It counts
	// attempts and not records because that is what it can honestly count: the retry is what
	// repairs a transient, and a counter that deduplicated would hide how hard this group is
	// working. [Stats.Unopened] is the one that counts RECORDS, and it counts the ones given
	// up on.
	//
	// This is the number that must stay zero, and [Group.Receive] returns an error naming the
	// first one whenever it does not.
	FailedOpen uint64

	// Records submitted, and records the server refused on the first attempt and accepted after
	// S2-2's single re-Hello and re-MAC. The second is the cost of Finding E and is readable
	// rather than invisible.
	Submitted uint64
	Rebound   uint64

	// Fetch PAGES the server answered, across every [Group.Receive]. It is here because one
	// Receive is not one page: 4.3.4 truncates a page by `limit` or by `max_response_bytes`
	// and calls both NORMAL, so a conversation longer than the server's page is several
	// requests. A number bigger than the Receive count is the ordinary reading of a backlog.
	Pages uint64

	// Fetch pages this build could NOT verify the 4.3.4 attestation of, which today is every
	// page the deployed server answers. It is a counter rather than a silence because the
	// thing it measures -- a server that OMITS records -- is the one thing the AEAD does not
	// catch. See [Group.Receive] for what is and is not checked, and what closing it needs.
	Unattested uint64

	// Times this group RAISED the floor of its own durable stream past indices the server already
	// holds claims at under this device's own sender_handle. Ledger item 245's first piece; see
	// [Group.seedOwnStreamLocked].
	//
	// A NUMBER HERE IS A LEAF THAT CHANGED HANDS, and on a healthy device it is exactly zero for
	// the life of a group: the highest index claimed under a handle is one this device's own
	// reserver allocated, so there is nothing to move. It goes to one on the first walk of a
	// device that was added onto a removed member's leaf -- RFC 9420 §7.7 refills the leftmost
	// blank and the handle is a function of the LEAF -- which is the walk that stops that device
	// being refused REASON_STREAM_INDEX_REUSED on its first send and bricked for the life of the
	// process.
	//
	// IT IS A COUNTER AND NOT A SILENCE BECAUSE IT IS ALSO THE ONE NUMBER A HOSTILE SERVER CAN
	// MOVE. The index it acts on is read off a PLAINTEXT header, deliberately and for reasons
	// argued where it is taken; what a forged header costs is stream indices this device did not
	// need to spend, and this is where that shows.
	StreamFloorSeeded uint64

	// Records given up on ([Stats.Unopened]) that the server WOULD NOT ATTRIBUTE TO A STREAM: no
	// §4.3.3 `sender_handle` projection of sixteen octets, or one with no `stream_index` beside
	// it. A strict subset of [Stats.Unopened], and the only rows about which this device cannot
	// say whether they spent an index under its own sender_handle.
	//
	// IT IS A COUNTER BECAUSE THE ALTERNATIVE WAS A REFUSAL AND THE REFUSAL WAS WORSE. A row this
	// build cannot parse used to take [Group.Send] away for the life of every later process -- see
	// [Group.ownFloorHeldByLocked] -- on the strength of a question the row's own projection
	// answers. Every deployed server fills that projection out of the same `projectionOf` the
	// submit path verifies a client's against (message-server `api/fetch.go`), so a number here is
	// a server that is not serving §4.3.3 rows; what this device then cannot rule out is a claim
	// under its own handle, and what that costs is the [ErrIdentityInUse] the same server can
	// answer any submit with directly ([Group.cloneRefusalLocked]).
	UnopenedUnattributed uint64
}

// ladderKey names one receiver ladder INDEPENDENT of the epoch its key schedule is derived at.
//
// IT IS THE EPOCH-INDEPENDENT HALF ON PURPOSE, and it is what [Group.peerHeads] is keyed by.
// §5.6's stream index is continuous across epochs -- a sender's counter does not rewind at a
// commit, because [messagegroup.SenderHandle] is derived from the epoch-zero group_handle_key and
// never moves -- so "the head this device has authenticated for this sender" is a fact about the
// whole stream and not about one epoch's schedule. A record layer key that carried the epoch would
// forget that head at every commit, which is the D3 starvation [Group.crossEpochLadderLocked]
// exists to prevent.
type ladderKey struct {
	leaf          uint32
	retentionWire byte
	ephWindow     uint64
}

// epochLadderKey is one receiver ladder AT one epoch: what [Group.peerHeadsAt] and
// [Group.persistedHeads] are keyed by, and what one [PeerHead] row names.
//
// It is a distinct type from [trackedKey] although the two carry the same pair, because they
// answer different questions: a trackedKey says "this ladder is installed at this epoch", which is
// cleared at every epoch change, and this says "this is the head authenticated at this epoch",
// which is exactly what must NOT be cleared.
type epochLadderKey struct {
	epoch uint64
	ladderKey
}

// trackedKey is one receiver ladder this group has installed AT one epoch. A second TrackSender
// over a live ladder would reset it to its head index and re-derive rungs it has already
// committed, so each one is installed exactly once -- and [Group.tracked] is the memo that keeps
// it to once.
//
// THE EPOCH IS IN THE KEY AND IT IS LOAD-BEARING. [messagegroup.GroupSession.AdvanceEpoch] (and
// the constructor it shares an install with) ZEROIZES every receiver ratchet on an epoch change
// and nothing rebuilds them, so a memo that carried no epoch would still say "this ladder is
// tracked" after the change and the first open at the new epoch would fail with "no receiver
// ratchet is tracked for this sender and retention class." The epoch is what makes a new-epoch key
// miss the memo and re-track; [Group.crossEpochLadderLocked] clears the whole map in the same block
// as the install anyway, so the two are the one rule stated twice, and
// `TestAfterAnEpochChangeEveryTrackedKeyNamesTheNewEpochAndTheOldOnesArePrinted` holds it -- as a
// behaviour over a seeded old-epoch key and its printed complement, not as a reading of this type.
type trackedKey struct {
	epoch uint64
	ladderKey
}

// ownSealed is what this group knows about one stream index of its own.
type ownSealed struct {
	// The body_hash of the record sealed at this index.
	bodyHash [32]byte

	// hasCopy is whether this device SEALED it and kept what it sealed. False for an index a
	// reconciling walk learned off a record the group's keys authenticated and this device holds
	// no copy of -- which is this lineage's own history, sealed before this build kept copies or
	// by a copy of the folder.
	hasCopy  bool
	body     []byte
	sentAtMs int64

	// recordId is the record id this copy has been shown under, zero until it has been. A second
	// record id carrying the same index and the same body_hash is one record shown twice, and a
	// server is the only party that numbers records.
	recordId uint64
}

// Group is one group on one device.
type Group struct {
	device         *Device
	id             []byte
	handle         messagegroup.GroupHandle
	groupHandleKey []byte

	// pq_secret PER EPOCH, ledger item 251's ruling 40 read from this side. It used to be ONE
	// []byte and the field it replaces was the group-lifetime scalar item 243 ruled in 2026-09-18
	// "on the explicit condition that rotating it is a prerequisite of shipping REMOVAL", because
	// a lifetime value leaves a removed member a permanent contribution to every future epoch's
	// storage_root. pqepoch.go's header carries the whole account; the short form is that this is
	// the only post-quantum material in the system, the MLS exporter contributes none, and one
	// value forever is a removal that removes nothing from a quantum adversary.
	//
	// BOUNDED BY [messagegroup.PastEpochWindow], the same bound connect's own table and
	// connect/mls's state deletion use, and every evicted entry is ERASED rather than dropped.
	// It is persisted: see [GroupRecord.PqSecrets] and what an old store does.
	pqSecrets map[uint64][]byte

	// pqSecretWitness is SHA-256 of every pq_secret THIS DEVICE has EVER filed, keyed by the
	// lowest epoch that value was filed at, AND IT IS NOT PRUNED BY THE WINDOW.
	//
	// THIS DEVICE'S HISTORY AND NOT THE GROUP'S, WHICH IS WHAT THE RULE ABOVE IT CAN PROMISE
	// (ledger rulings 42-45). [Device.Join] files exactly one row, so this map is STRICTLY SMALLER
	// on a member admitted later and the rule reading it refuses strictly less. The full residual
	// -- including the case where nobody refuses at all -- is written out at
	// [refuseRemovalOnHeldSecret]; what must not be written anywhere is a claim about what the
	// GROUP holds, because no receiver can check one.
	//
	// WHY IT EXISTS, AND IT IS THE 2026-09-24 REPAIR. The removal rule's subject used to be
	// `pqSecrets` alone -- the table above -- and that table is pruned at
	// [messagegroup.PastEpochWindow] by [Group.dropPqSecretsBelowWindowLocked]. So the set the rule
	// USED TO BE spelled against, "a value this group already holds", SHRANK while the removed
	// member's did not, and a removal fanned out on a pq_secret this device had EVICTED was
	// followed with a nil error. REPRODUCED
	// against the production receive path by
	// TestARemovalFannedOutOnAnEvictedEpochsSecretIsRefusedToo: 33 honest rotations, then a
	// removal opening on pq_secret[1], and the removed member's retained value was octet for
	// octet the post-quantum half of the survivors' storage_root at the epoch it was removed at.
	// The eviction is LOCAL HYGIENE and an adversary does not run it.
	//
	// IT IS HASHES AND NEVER SECRETS, which is what makes keeping them for ever acceptable: a
	// digest of a 32-octet uniformly drawn value answers "is this the same value" and nothing
	// else, and the disk already carries the secrets themselves for the window. The cost is 40
	// octets per epoch the group has ever stood at, and it is bounded only by the group's
	// lifetime; that is stated rather than hidden, and it is the price of a rule whose subject
	// does not shrink.
	//
	// WHAT IT DOES NOT REACH, NAMED AND NOT CLAIMED CLOSED: a device that never held the replayed
	// epoch's row at all -- a member ADDED after it, or one restored from a record written before
	// this field was persisted -- has no witness for it and follows. That is not a defect in this
	// map and no widening of it can reach the case: a receiver cannot answer a question about a
	// history it does not have, so the false negative is a THEOREM. The only repair is on the
	// wire: the wrap payload and the digest preimage authenticated as drawn FOR `opensEpoch`, so a
	// replay of any earlier epoch's value is refused by construction whatever the receiver still
	// holds. That is a `connect` change and is filed rather than taken here. Ruling 45 prices the
	// cheaper half -- shipping this witness in the Welcome -- and records that it NARROWS rather
	// than closes, because a late-joining inviter's own witness is already truncated.
	// See [GroupRecord.PqSecretWitness].
	pqSecretWitness map[uint64][sha256.Size]byte

	// The device wraps this group has OPENED and not yet judged, under the epoch each one
	// delivers, and how many arrived at this device's own handle and did not open.
	//
	// THEY ARE STAGED AND NOT INSTALLED because the only thing that can judge them arrives after
	// them: ruling 37 puts the fan-out on the wire ahead of the commit, and the commit's
	// H(epoch_keys) is the authenticator that says which candidate is the epoch's own secret. See
	// [Group.resolvePqSecretLocked].
	wrapsFor        map[uint64][]wrapCandidate
	wrapsUnreadable map[uint64]int

	mutex sync.Mutex

	// The session at epoch zero, which exists on the FOUNDER only and only until the group is
	// open. It is what seals the founding commit: 4.3.2 self-certifies that record under
	// bootstrap_write_key, which is epoch zero's write key, and the session that holds epoch
	// zero's key schedule is the one constructed before the commit moved the handle.
	founding      *messagegroup.GroupSession
	foundingBound uint64

	// The session at the group's current epoch, and the one every message goes through.
	session      *messagegroup.GroupSession
	sessionBound uint64
	epoch        uint64

	// The MLS commit that opened the current epoch. It is the founding commit record's body, so
	// the record the server stores as is_commit actually carries a commit.
	commit []byte

	opened bool
	closed bool

	// cursor is the RESOLVED position: the highest record id below which every record has been
	// opened, skipped for a reason this build names, or given up on. It is deliberately NOT the
	// highest record id the server has handed over -- see [Group.Receive] and openPageLocked,
	// where a record that did not open holds this back so that the next fetch asks for it again.
	cursor uint64

	// log is every message this group holds, in the order it learned them, AND IT IS THE ONLY
	// PLACE A *Message IS HELD. Nothing else in this struct carries one -- [Group.logIndex] holds
	// a POSITION in this slice and not a pointer into it -- because the repair for msgrepo ledger
	// item 227 is that a [Message] is REPLACED rather than written through, and a second table
	// holding pointers would be a second table to leave stale. See [Group.reapplyLocked].
	log     []*Message
	tracked map[trackedKey]bool
	stats   Stats

	// delivered is the record ids that are in log. A fetch that is rewound over a record that
	// did not open re-reads everything after it, and this is what makes that free of duplicates.
	delivered map[uint64]bool

	// attempts is how many times one record id has been fetched and failed to open.
	attempts map[uint64]int

	// unopened is the record ids this group has given up on, ascending.
	unopened []uint64

	// ── the kinds that change another message ────────────────────────────────────────────
	//
	// logIndex is where in [Group.log] every message this group holds sits, under its own
	// message_id. It is what a reaction, a tombstone or a reply resolves its target through, and
	// it is a map because the alternative is a scan of the log per effect record.
	//
	// IT HOLDS A POSITION AND NOT A POINTER, WHICH IS LEDGER ITEM 227's REPAIR IN ONE FIELD.
	// It used to be `map[...]*Message`, so a [Message] lived in two places at once and
	// [Group.reapplyLocked] kept them agreeing by writing THROUGH the pointer both of them held --
	// which is the write that reached callers holding a [Group.Messages] copy. Now the rebuild
	// REPLACES the message at its position, and a table of positions cannot go stale when it does:
	// there is exactly one holder of every *Message and it is [Group.log].
	//
	// ONE MESSAGE_ID IS ONE POSITION, which is what makes the position safe to hold. Both this map
	// and the log are written in [Group.deliverLocked] and nowhere else, together, and a record is
	// delivered at most once ([Group.delivered], keyed on the server's record id, and
	// [Group.openOwnFromCopyLocked]'s refusal of a second record id for one copy).
	logIndex map[[MessageIdBytes]byte]int

	// effects is every reaction and tombstone this group has read, under the EFFECT RECORD's
	// OWN message_id. The key is what makes a re-delivery idempotent: one record is one effect
	// however many times a rewind walks back over it, and a record's id is a function of the
	// record alone (MASTER §8.4.5).
	effects map[[MessageIdBytes]byte]*contentEffect

	// effectsOn is the same effects indexed by the message they NAME, which is the order they
	// have to be replayed in. See [Group.reapplyLocked] for why a replay rather than an
	// application.
	effectsOn map[[MessageIdBytes]byte][]*contentEffect

	// dirtyTargets is the messages whose effect set changed during the walk in progress and whose
	// rebuild has NOT happened yet. It is drained by [Group.rebuildDirtyLocked], once, where the
	// walk commits.
	//
	// IT EXISTS BECAUSE A REBUILD PER EFFECT IS A CUBE. Every arriving effect used to call
	// [Group.reapplyLocked] on its target, so n effects on ONE message cost n rebuilds of n
	// effects, and the ADD arm's dedupe scanned the reactions it had already appended -- n from the
	// walk, n from the rebuild, n from the scan. Measured on the real codec before this set
	// existed: 4,000 REACTION_ADD records on one message cost 8,087,950 mallocs and 41 seconds on
	// every OTHER member's client, growing as n^3 in time and n^2 in allocations (4x per doubling,
	// exactly), and the victim pays it ON EVERY LAUNCH because the cursor is not persisted. The
	// CONTROL that localises it: n effects over n DIFFERENT targets was already LINEAR, so the cost
	// was never the walk.
	//
	// WHAT IT DOES NOT CHANGE, AND THIS IS THE WHOLE OF WHY IT IS SAFE. The rebuild still happens,
	// still from the full sorted effect set, so [Group.reapplyLocked]'s rebuild-not-accumulate
	// property is untouched. The only new state is a target that is stale PART-WAY THROUGH A WALK,
	// and no caller outside this file can observe that: [Group.Receive] holds [Group.mutex] across
	// every page and across the commit, and [Group.Messages] takes the same mutex.
	//
	// THE TWO PLACES THAT NEED AN EFFECT'S ANSWER IMMEDIATELY DO NOT GO THROUGH THIS SET, and both
	// are outside a walk or are the walk's own repair: [Group.sendContentLocked] drains it before
	// it returns the message it just sealed, and [Group.deliverLocked] rebuilds a target directly
	// at the moment the target itself arrives.
	dirtyTargets map[[MessageIdBytes]byte]struct{}

	// ── one identity, two devices ────────────────────────────────────────────────────────
	//
	// ownIndices is every §5.6 stream index this group has accounted for as its own, AND THE
	// body_hash OF WHAT WAS SEALED AT IT -- and, for an index THIS DEVICE sealed, the copy of what
	// it sealed there.
	//
	// THE HASH IS WHY THIS IS NOT A SET, and it is what catches the hardest case. An index is
	// recorded at the SEAL and not at the submit, because a submit whose response was lost is
	// still a record this device sealed. So when two copies of one folder are EXACTLY level,
	// both seal at the same index, one submission wins and one is refused -- and the loser then
	// meets, on the server, a record under its own sender_handle at an index it DID seal, whose
	// body is not the body it sealed. An index alone cannot tell those apart. The hash can, and
	// 3.1's body_hash is authenticated by both AEADs, so a server cannot forge one that opens.
	//
	// THE COPY IS WHY IT CARRIES MORE THAN A HASH, and it is connect MG-4 read from this side. A
	// member cannot open its own application record any more, so this device's own half of a
	// conversation exists in exactly one place it can read: here, and in the durable store's copy
	// of it ([DeviceStore.PutSentRecord]), which [Device.Restore] reads back into this map.
	ownIndices map[uint64]*ownSealed

	// withoutCopy is every record id this group has authenticated as its own and cannot show. See
	// [Stats.OwnWithoutCopy].
	//
	// IT EXISTS BECAUSE A REWIND RE-READS THESE: without it a record re-fetched behind an earlier
	// failure is authenticated and counted again
	// (cp3b.TestAnOwnRecordThisDeviceKeptNoCopyOfIsCountedAndIsNotAFailure).
	// IT KEEPS NO INDEX, and it used to: the skip re-noted the index it was authenticated at, and
	// deleting that re-note turned nothing red in urmessage or cp3b, because the index is already in
	// [Group.ownIndexSeen], which is the group's and not the walk's.
	withoutCopy map[uint64]bool

	// ownIndexSeen is the highest §5.6 stream index on a record of this device's own that this
	// group's keys AUTHENTICATED, across every walk since the group came up.
	//
	// IT IS THE GROUP'S AND NOT ONE WALK'S, and it used to be one walk's. The reconciliation holds
	// it against the reserver's high water on the first clean walk -- and a clean walk that comes
	// after a dirty one does not re-open the records the dirty one already resolved: a delivered
	// record is skipped by record id and contributes nothing. So a copy whose evidence arrived in
	// a walk that ALSO lost some other record reconciled on the next, clean walk with the evidence
	// forgotten, and sealed at an index the original had already used.
	// cp3b.TestACopyWhoseEvidenceArrivedInADirtyWalkIsStillCaught drives that.
	ownIndexSeen uint64

	// ownHandles is EVERY §3.1 sender_handle this device has held in this group, and it is the set
	// the pre-open roads decide `mine` against. Ledger item 245's fourth piece.
	//
	// IT IS A SET AND NOT ONE VALUE BECAUSE A DEVICE'S HANDLE CAN MOVE AND ITS HISTORY CANNOT.
	// SenderHandle(group_handle_key, leaf) is a function of the LEAF, so a device removed from a
	// group and re-added lands at whatever leaf the tree gives it and seals under different
	// octets from then on -- while the records it wrote before that are still its own, are still
	// on the server, and are still the only place its half of that conversation can be read from.
	// A group that held one handle showed those lines as a stranger's; both graceful own-record
	// roads ([Group.openOwnFromCopyLocked] and the MG-4 spent-generation arm) are gated on `mine`,
	// so a handle that moved took this device's own history away from it.
	//
	// IT IS DURABLE, AND WHAT AN OLD STORE DOES IS NAMED RATHER THAN DISCOVERED. The set is part
	// nine of [GroupRecord]; a record written before part nine carries NO set, and
	// [Device.restoreOne] then seeds it with the ONE handle this device's leaf derives today --
	// which is exactly what every build before this one held, so such a device comes back working
	// and loses only the handles it held at an EARLIER leaf. A restore that refused a record
	// written by an older build would be a device that can never start again.
	//
	// IT IS A PRE-FILTER AND NEVER AN ATTRIBUTION. What a record IS, is decided after the open by
	// [Group.recordIsOwnLocked] off the signing leaf's identity; this set only decides which cheap
	// roads are tried first, and each of those has its own proof beneath it -- the copy road
	// compares a body_hash this device sealed, and the MG-4 arm needs MLS's own spent-generation
	// refusal.
	//
	// IT STILL HAS EXACTLY ONE ENTRY FOR EVERY DEVICE THIS BUILD CAN PRODUCE, AND THE REASON IS NOT
	// THE ONE THAT STOOD HERE. It used to be "the sdk exposes no product method over the seam's
	// CommitRemove", which ledger item 242's R1 was true about and [Group.RemoveMember] is not: a
	// device can be removed now, and being added back is driven by
	// TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory,
	// which moves the handle in one of its two rows. What keeps the set at one entry is
	// [Device.Join]: a re-add builds a FRESH [Group], so [Group.initTables] seeds this from the new
	// leaf alone, and the persist that follows rewrites part nine with it. The earlier handle is
	// dropped.
	//
	// AND THAT LOSS COSTS NOTHING TODAY, WHICH IS MEASURED AND IS NOT THE SAME AS BEING HARMLESS. A
	// re-added device holds state from its admission on, so every record it wrote before the removal
	// answers [ErrRecordOpen] on each of [maxRecordAttempts] walks and is then abandoned -- the same
	// case measures it -- and what this set would have decided about those records is never asked.
	// The day a re-added device can reach an epoch below its admission, [Device.Join] owes carrying
	// part nine forward, and this paragraph is where that debt is written down.
	ownHandles map[[16]byte]bool

	// departedAt is, for every leaf this group has watched a commit REMOVE, the epoch the LAST such
	// commit OPENED. No occupant of that leaf stood at any epoch at or above that number; below it
	// the table over-claims, because it says nothing about the gaps between two occupancies when
	// the leaf stood blank. It is a PRE-FILTER and is deliberately not exact; see
	// [Group.leavesAtLocked], and [Group.noteDepartedLeavesLocked] for why the LAST departure is
	// the one that is kept.
	//
	// WHY THE TABLE EXISTS, and it is ledger item 245's third piece. [Group.leavesLocked] built
	// the handle table from the membership at the CURRENT epoch only, so a record sealed at epoch
	// n by a leaf removed at n+1 -- the ordinary first day of Remove, and every restart afterwards
	// -- resolved to no leaf at all, took the fail() road, and was abandoned after
	// [maxRecordAttempts]. This is what [Group.leavesAtLocked] adds back for the epochs the leaf
	// really stood at.
	//
	// IT IS DURABLE FOR THE SAME REASON THE HANDLE SET IS: the cursor is not persisted, so a
	// restarted device re-walks its whole history and meets those records again. It rides in part
	// nine beside the set, and a record written before part nine carries no table -- such a device
	// resolves a departed leaf's records only while the leaf has been REFILLED (the refilling
	// member stands at the same leaf and derives the same handle), which is the state every build
	// before this one was in.
	departedAt map[uint32]uint64

	// ownHeads is the head the receiver ladder over this device's OWN leaf was last tracked at, per
	// ladder. See [Group.advanceOwnLadderLocked]. It is CLEARED at every epoch install, in the
	// same block, because the ratchet it describes is zeroized there; [Group.ownIndexSeen] is the
	// authenticated own head that survives the clear and re-seeds it.
	ownHeads map[trackedKey]uint64

	// peerHeads is the highest §5.6 stream index of a record from each PEER ladder that this group's
	// keys have AUTHENTICATED, keyed epoch-independent by [ladderKey].
	//
	// IT IS THE HEAD AN EPOCH CHANGE RE-TRACKS THAT PEER AT, AND NEVER 0. A ratchet re-tracked at 0
	// answers [messagegroup.DefaultRecordWindowSize] rungs and then ErrOutOfWindow, so a peer that
	// had sent more than 1024 records before a membership change would go silent on its very next
	// message -- the D3 starvation from the group-chat survey. Because the stream index is
	// continuous across epochs (see [ladderKey]), the head the previous epoch left off at is the
	// head the next epoch resumes from, so this is the ONE piece of ladder bookkeeping
	// [Group.crossEpochLadderLocked] does not clear. It is the peer analogue of
	// [Group.ownIndexSeen]. 0 for a ladder never seen -- a member just added, or a fresh group at
	// epoch one -- which is exactly the head [Group.trackLocked] used to pass unconditionally.
	//
	// IT IS THE CURRENT EPOCH'S HEAD AND IT MUST NOT POSITION A PRIOR EPOCH'S LADDER. A record
	// opened at epoch n+1 raises it above every record of epoch n, and a ladder for epoch n tracked
	// there refuses all of them; [Group.peerHeadsAt] below is what a prior epoch reads instead.
	peerHeads map[ladderKey]uint64

	// peerHeadsAt is [Group.peerHeads] PER EPOCH: the highest stream index this group's keys have
	// authenticated on each peer ladder AT each epoch, in THIS process. It is what positions a
	// PRIOR epoch's ladder ([Group.pastHeadLocked]) and it is what [Group.commitWalkLocked]
	// persists as [PeerHead]s when it has changed. Ledger item 241.
	peerHeadsAt map[epochLadderKey]uint64

	// persistedHeads is the [PeerHead] table as it stood ON THE DISK when this group was restored,
	// and it is READ ONLY afterwards. It is kept apart from peerHeadsAt on purpose: a head written
	// before the restart at epoch n is the head of records the re-walk is about to open AGAIN, so it
	// positions the ladder for epochs ABOVE n and never for n itself. [PeerHead] has the argument.
	// Empty for a group founded or joined in this process, and for one restored from a directory a
	// build before item 241 wrote.
	persistedHeads map[epochLadderKey]uint64

	// headsDirty is whether peerHeadsAt has risen since the table was last persisted.
	headsDirty bool

	// reconciled is whether this group has compared its own stream position against the
	// server's rows since it came back. A group created or joined in THIS process is
	// reconciled by construction -- its identity was drawn here and nothing else holds it. A
	// RESTORED group is not, and [Group.Send] refuses until [Group.Receive] has run once.
	reconciled bool

	// ownFloorHeld is whether this device's own durable stream floor HAS BEEN ESTABLISHED against
	// every claim the server holds under this device's sender_handle THAT THIS GROUP HAS SEEN, over
	// a walk that saw the whole history and left nothing it could not read. Ledger item 245's first
	// piece. [Group.ownFloorHeldByLocked] is the predicate and the only writer of the true value.
	//
	// THE FIRST SHAPE OF THIS FIELD SAID NOTHING ABOUT THE FLOOR, WHICH IS WHY THE PREDICATE HAS A
	// NAME NOW. It was raised on [Group.walkSawTheWholeHistoryLocked] alone -- "the server finished
	// handing over a page and nothing in it failed" -- while [Group.seedOwnStreamLocked], the only
	// thing that MOVES the floor, returned early on three conditions the gate could not see. Two
	// reproductions, both over one reuse cohort (bob spends stream indices 1..3 on leaf 1, eve is
	// added onto the same leaf and derives the same sender_handle):
	//
	//   - ONE CLEAN WALK OVER AN EMPTY PAGE raised it: ownFloorHeld true, the reserver's high water
	//     0 -> 0, while the server held claims at 1, 2 and 3. Nothing about the floor was checked,
	//     so nothing about the floor was true.
	//   - THE REACHABLE TRIGGER, with no adversary in it: bob's TOP row served as octets this build
	//     cannot parse. Walks 1 and 2 answered `record 3 does not parse`; walk 3 abandoned it and
	//     resolved the cursor PAST it; walk 4 met an empty page, was clean by every clause, and
	//     raised the gate with the reserver at 2. Eve's next seal was at stream index 3 carrying a
	//     message_id byte-identical to bob's line 3 -- the sticky [ErrIdentityInUse] this gate
	//     exists to prevent, produced by the gate certifying a floor nobody had held.
	//
	// SO THE GATE CARRIES THE SEED'S OWN VERDICT NOW, plus two clauses about what the walk could not
	// see. [Group.ownFloorHeldByLocked] lists all three and argues each one.
	//
	// AND THE SECOND REPRODUCTION IS CLOSED BY READING THE ROW RATHER THAN BY REFUSING THE GROUP,
	// which is the repair of a fix that was worse than the defect. The first shape of that repair
	// was a FOURTH clause -- one record this build gave up on unparsed, anywhere in a group's
	// history, took this flag away permanently -- and because the cursor is not persisted, a
	// restarted device re-walks that row, re-abandons it and re-derives the veto every time the app
	// opens. Measured: a group with one unparseable row and NO reused leaf at all -- three founding
	// members, three distinct sender_handles, nothing ever removed -- sent before the restart and
	// was refused [ErrStreamFloorUnheld] after it, for ever. A record that does not parse is not
	// evidence about a previous occupant of THIS device's leaf unless it is a record under THIS
	// device's own sender_handle, and §4.3.3's projection is what says which:
	// [Group.noteUnparsedClaimLocked] takes the server's own `sender_handle` and `stream_index`
	// beside the octets, so the honest case -- a previous occupant that wrote in a record format
	// this build cannot read -- RAISES THE FLOOR instead of bricking the group. What is left is
	// [Stats.UnopenedUnattributed].
	//
	// IT IS A DIFFERENT QUESTION FROM [Group.reconciled] AND THE TWO MUST NOT BE ONE FIELD.
	// `reconciled` asks "is another copy of this folder writing my stream"; this asks "did
	// somebody ELSE stand at my leaf before me and spend indices under the sixteen octets I now
	// derive". A group JOINED in this process answers the first by construction -- its identity
	// was drawn here -- and CANNOT answer the second without looking at the server, because
	// SenderHandle(group_handle_key, leaf) takes no epoch and no identity and RFC 9420 section
	// 7.7 hands a joiner the leftmost BLANK leaf.
	//
	// TRUE AT A FOUNDING, FALSE AT A JOIN, FALSE AT A RESTORE. A founder drew the group id here,
	// so the server holds no claim under any handle of that group. A joiner and a restored group
	// both wait for one complete, clean walk -- the same walk [Group.seedOwnStreamLocked] moves
	// the floor on -- and [Group.Send] answers [ErrStreamFloorUnheld] until then.
	ownFloorHeld bool

	// ownClaimSeen is the highest §5.6 stream index this GROUP has ever seen CLAIMED under
	// ownClaimHandle, and ownClaimHandle is the sender_handle it was seen under. It is the number
	// [Group.seedOwnStreamLocked] raises the floor to, and it is read off a PLAINTEXT HEADER --
	// which is argued and bounded there.
	//
	// IT IS ON THE GROUP AND NOT ON THE WALK FOR THE REASON [Group.ownIndexSeen] IS, and it is the
	// same defect one field along: one walk's number forgets what an earlier walk already read. The
	// seed is gated on [Group.reconciled] and the walk that ABANDONS a record is a walk that failed,
	// so a restored group could note a claim on walk 1 (unreconciled, no seed), give the record up
	// on walk 3, and meet a clean walk 4 that skips the row as a passenger and sees the claim
	// NOWHERE. Cumulative, the number survives every one of those walks and the floor is raised off
	// it the first time the seed is allowed to run.
	//
	// KEYED BY HANDLE BECAUSE A FLOOR IS A FACT ABOUT ONE STREAM. A device whose leaf moved -- a
	// restore onto a re-Add -- seals under different octets, and a claim seen under the old ones is
	// not evidence about the new stream. [Group.ownClaimedLocked] answers 0 for a handle that is not
	// the one the number was seen under rather than carrying it across.
	ownClaimSeen   uint64
	ownClaimHandle [16]byte

	// identityInUse is sticky and is the whole of the clone refusal. Once set, every Send is
	// refused with it. See [Group.Receive].
	identityInUse error

	// wrapDark is STICKY and is ruling 38's diagnosis kept where a caller can still find it.
	//
	// A member that followed a commit into an epoch it holds no pq_secret for is dark in BOTH
	// directions at that epoch and permanently: read_key and write_key both descend from
	// storage_root, and the server verifies req_auth before any AEAD, so what the field sees is
	// REASON_REJECTED with nothing readable behind it. The ingest says so ONCE, through its own
	// error, and this is the copy every later refusal is made with -- because without it the
	// second walk reports an AEAD failure on some record and the sentence about the wrap is gone.
	// It is one of [ErrNoWrapForEpoch], [ErrWrapUnreadable] and [ErrOrphanWrap], with the epoch
	// named inside it.
	//
	// IT IS NOT REPAIRABLE AT ALL -- not in this process and not in any later one -- AND THAT IS
	// WHY IT IS STICKY AND WHY IT IS PERSISTED. This used to say "not repairable in this
	// process", which reads as though a restart might fix it. A restart does not. Two independent
	// reasons, either alone sufficient, both measured rather than argued:
	//
	//  1. THE WRAP CANNOT BE SERVED AGAIN. A fetch carries req_auth MAC'd under
	//     read_key[read_epoch] and msgrepo's api/fetch.go check 7 verifies it against the key the
	//     committer published, before a row is read. This device holds the wrong pq_secret for
	//     the epoch it stands at, so it derives the wrong read_key and every fetch it makes is
	//     REASON_REJECTED. It cannot ask at the epoch BELOW either: connect 74abe029 answers
	//     read_key and write_key through one door, EpochKeys, which answers the session's OWN
	//     epoch (session.go:446), and a `pastEpoch` carries classKeys and no read or write key.
	//     The control for that query is inside it -- RoleAt and TrackSenderAt are past-epoch
	//     doors that DO exist -- so what is missing is this key pair and not past-epoch access.
	//  2. EVEN HANDED THE WRAP, THERE IS NOWHERE TO PUT IT. The session has already advanced into
	//     the epoch on the fallback secret, and connect refuses both doors onto that entry:
	//     InstallPqSecret refuses `epoch == self.epoch` by name (ErrPqSecretEpochIsCurrent) and
	//     AdvanceEpoch refuses a differing value at an epoch already filed
	//     (ErrPqSecretEpochConflict). So [Group.ingestWrapLocked]'s `tag.Epoch <= self.epoch`
	//     guard is not what forecloses recovery; these two are.
	//
	// The repair is out of band and it is a re-Add. cp3b's darkgroup_test.go drives (1) against a
	// real server.
	wrapDark error

	// wrapDarkEpoch is the epoch [Group.wrapDark] was taken at, kept beside it because the error
	// string is not a field anything can read a number out of and [GroupRecord] has to carry one.
	// Meaningless while wrapDark is nil, and the two are written in one place.
	wrapDarkEpoch uint64

	// halted is RULING 41's OTHER OUTCOME, and it is a DIFFERENT FIELD from [Group.wrapDark]
	// because the two are different states and the whole of ruling 41 is that they are told
	// apart. It is [ErrRemovalWithoutRotation] and nothing else.
	//
	//   - VALID COMMIT whose wrap did not arrive or did not open -> [Group.wrapDark] at n+1. The
	//     group FOLLOWED the commit and cannot derive that epoch's keys.
	//   - INVALID COMMIT -- a removal this device could only follow on a pq_secret it already
	//     holds -> THIS, at n. The group did NOT follow the commit. Item 242's semantics: a
	//     hostile committer can HALT a group; it cannot TAKE it.
	//
	// IT IS STICKY AND IT IS PERSISTED, AND BOTH ARE 2026-09-24 REPAIRS OF A MEASURED DEFECT.
	// Before them the refusal was returned once and nothing was kept: the second walk over the
	// same record answered `mls: ratchet generation already consumed` (step (0) of
	// [Group.ingestCommitLocked] tracks the committer's ladder BEFORE the refusal, so the
	// sentinel cannot be re-derived), the third answered [ErrRecordAbandoned] and resolved the
	// cursor PAST the refused commit, and the fourth and fifth answered nil over a group standing
	// an epoch behind its own log with a record on the disk reading HEALTHY. A refusal that halts
	// a group has to be at least as durable as the dark state it is contrasted with.
	//
	// AND THE HALT IS PERMANENT, WHICH IS MEASURED AND WAS ONCE DENIED IN PROSE. The comment that
	// stood here said "a committer that re-commits properly is followed normally". That is FALSE:
	// the refused commit stays in the log AHEAD of this receiver for ever, every record after it
	// is sealed at an epoch this device is not in, and a proper re-commit lands above it. Driven
	// by TestARefusedRemovalHaltsTheGroupForEveryLaterWalkAndAcrossARestart, which walks the same
	// record five times and then re-commits properly. The repair is out of band and it is the
	// same one a dark group needs: this device is re-Added.
	halted error

	// haltedEpoch is the epoch [Group.halted] left this group standing at -- n, the epoch it did
	// NOT move off. Meaningless while halted is nil, and the two are written in one place.
	haltedEpoch uint64

	// removed is RULING 52's THIRD STATE: a VALID commit this group received took this device out
	// of the group. It is [ErrRemovedFromGroup] wrapping mls's own answer, and nothing else.
	//
	// IT IS A THIRD FIELD AND NOT A THIRD KIND OF ONE OF THE TWO ABOVE, which is the ruling. Three
	// states, three subjects, and the group stands at a different place in each:
	//
	//   - VALID commit, wrap missing        -> [Group.wrapDark] at n+1. Followed; holds no keys.
	//   - INVALID commit (unrotated removal) -> [Group.halted] at n.   Refused; still a member.
	//   - VALID commit that REMOVED THIS DEVICE -> THIS, at n. Not refused, not followed, and not a
	//     member: there is no n+1 for this device to be in or to be dark at, and the fan-out its own
	//     removal opened never addressed it (ledger item 258's derivation).
	//   - A COMMIT THE ROLE MODEL REFUSED -> NEITHER FIELD, at n. Still a member, and that is the
	//     point: a commit this device refused removed it from nothing, so it is not THIS; and
	//     [Group.halted] is [ErrRemovalWithoutRotation] and nothing else, so it is not that either.
	//     The row is here because the fourth outcome is the one a reader of a three-row table goes
	//     looking for; [ErrCommitUnauthorized] carries what it costs, and
	//     TestARoleModelRefusalIsNeitherTheRemovalNorTheHaltAndTheWalksSayWhich drives it.
	//
	// AND THE SAME READING ANSWERS THE VICTIM OF A DIGEST-LESS REMOVAL, which is the one case where
	// the subject of a removal ends up in the SECOND row rather than the third: step (3a) refuses
	// that commit before ApplyCommit for the victim exactly as for every survivor, so the victim is
	// HALTED and reads (0, nil) from [Group.Removal] -- `removed: false` at cgo's
	// urnet_message_group_removal, deliberately, because it was not removed, it refused. Driven by
	// TestTheVictimOfADigestLessRemovalIsHaltedAndReadsNoRemoval.
	//
	// IT IS STICKY AND PERSISTED FOR THE REASON THE HALT IS, and the measurement is the same shape
	// one ruling along: the sentinel is available for exactly ONE walk in the life of the handle,
	// because mls closes the group and zeroizes its epoch secrets when it answers, so the second
	// walk's Process answers `the group is closed` and the third spends [maxRecordAttempts] and
	// resolves the cursor PAST the record. See [ErrRemovedFromGroup] for the four walks as measured.
	// Part TEN of [GroupRecord] carries it, so the state does not have to be re-derived from a
	// record mls can no longer be asked about.
	//
	// AND IT IS NOT A REFUSAL OF THE GROUP'S HISTORY. The rows at and below removedEpoch still
	// fetch under this device's own read_key and still open, so the walk runs, delivers what it
	// opens, and answers this at the end -- which is what lets a client render the transcript it
	// is entitled to beside the sentence that says it is over. Driven end to end, across a real
	// restart, by cp3b's
	// TestARemovedDeviceIsToldSoByNameOnEveryWalkAndStillIsAfterARestart.
	removed error

	// removedEpoch is the epoch [Group.removed] left this group standing at -- n, the last epoch
	// this device was a member of, and the highest one item 246's ceiling will serve it. It is
	// [Group.haltedEpoch]'s reading and not [LeafOccupancy.DepartedEpoch]'s: the epoch the commit
	// OPENED is n+1 and is an epoch this device has no state for, so it is the wrong number to
	// render and the wrong number to ask the server for. Meaningless while removed is nil, and the
	// two are written in one place.
	removedEpoch uint64
}

// ── founding and joining ─────────────────────────────────────────────────────────────────────

// CreateGroup founds a group on this device. NOTHING REACHES THE SERVER HERE.
//
// The group is local and at MLS epoch zero, which is not an epoch any record can be written in:
// 6.1 opens a group at the epoch its first commit creates. [Group.AddMember] makes that commit and
// [Group.Open] publishes it. A caller that skips either is refused by name rather than by a
// REASON_REJECTED it has to decode.
//
// groupId is 32 octets and is the caller's to choose. It must be unpredictable -- the server keys
// its rows by it and anyone who can guess one can ask whether it exists -- so draw it from a
// CSPRNG.
func (self *Device) CreateGroup(ctx context.Context, groupId []byte) (*Group, error) {
	if len(groupId) != GroupIdBytes {
		return nil, fmt.Errorf("urmessage: a group id is %d octets and this one is %d", GroupIdBytes, len(groupId))
	}
	nonce, nonceEpoch, err := self.nonce()
	if err != nil {
		return nil, err
	}

	handle, err := self.createMlsGroup(groupId)
	if err != nil {
		return nil, err
	}
	if epoch := handle.Epoch(); epoch != 0 {
		handle.Close()
		return nil, fmt.Errorf("urmessage: a freshly created mls group is at epoch %d, want 0", epoch)
	}

	pqSecret, err := messagegroup.NewPqSecret(self.random)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("urmessage: pq_secret: %w", err)
	}
	// AT EPOCH ZERO AND NOWHERE ELSE. group_handle_key is the epoch zero storage root's expansion
	// and it never moves; a value recomputed from a later root gives every epoch a different
	// sender_handle and ends every member's stream at every commit.
	mlsSecret, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("urmessage: the epoch zero exporter: %w", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

	founding, err := messagegroup.NewGroupSession(handle, pqSecret, nil, self.reserver, self.nowMs, nonce)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("urmessage: the session at epoch 0: %w", err)
	}
	group := &Group{
		device:         self,
		id:             append([]byte(nil), groupId...),
		handle:         handle,
		groupHandleKey: groupHandleKey,
		// AT EPOCH ZERO, WHICH IS THE ONE EPOCH A FOUNDING DRAW CAN FILE. The commit
		// [Group.AddMember] makes opens epoch one and files that epoch's own entry beside it;
		// from the first commit to an OPEN group on, every epoch's value arrives through the
		// device wrap. See pqepoch.go.
		pqSecrets:     map[uint64][]byte{0: pqSecret},
		founding:      founding,
		foundingBound: nonceEpoch,
		// a group founded in THIS process holds an identity drawn in this process. There is
		// no earlier writer of its stream to reconcile against.
		reconciled: true,
		// AND NO EARLIER OCCUPANT OF ITS LEAF EITHER, which is the other question and is
		// answered here for a different reason: the group id was drawn in this process, so
		// the server holds no stream claim under ANY handle of this group. A JOINED group
		// cannot say that about the leaf it lands on. See [Group.ownFloorHeld].
		ownFloorHeld: true,
	}
	group.initTables()
	self.hold(group)
	// NOTHING IS PERSISTED HERE AND THAT IS DELIBERATE. A group at epoch zero with no second
	// member cannot be restored into anything a caller can use -- [Group.AddMember] needs the
	// founding session, which is not persisted, and [Group.Open] needs the commit AddMember
	// makes -- so a record written here would describe a group that comes back dead. mls has
	// already written its own epoch-zero state by now; [DurableStateStore.GroupRecords] skips a
	// group directory with no record in it for exactly this state, and says so.
	return group, nil
}

// Join takes an [Invite] a founder handed over out of band and becomes a member.
//
// WHAT THIS DOES NOT CHECK, and it is MG-1 rather than an omission of this file: nothing here
// decides whether the device that built this Welcome is the device the user meant to talk to. The
// Welcome names a group and a membership, JoinFromWelcome joins it, and the identity a caller would
// read off the membership afterwards is whatever the Welcome's author put there. Deciding that is
// the contact card's job and contact cards are out of scope.
func (self *Device) Join(ctx context.Context, invite *Invite) (*Group, error) {
	if invite == nil {
		return nil, fmt.Errorf("urmessage: no invite")
	}
	if err := invite.check(); err != nil {
		return nil, err
	}
	nonce, nonceEpoch, err := self.nonce()
	if err != nil {
		return nil, err
	}
	handle, err := self.engine.JoinFromWelcome(invite.Welcome, invite.RatchetTree)
	if err != nil {
		return nil, fmt.Errorf("urmessage: JoinFromWelcome: %w", err)
	}
	if !bytes.Equal(handle.GroupId(), invite.GroupId) {
		handle.Close()
		return nil, fmt.Errorf("urmessage: the welcome joined group %x and the invite names %x",
			handle.GroupId(), invite.GroupId)
	}
	session, err := messagegroup.NewGroupSession(handle, invite.PqSecret, invite.GroupHandleKey,
		self.reserver, self.nowMs, nonce)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("urmessage: the session at epoch %d: %w", handle.Epoch(), err)
	}
	// the door to prior epochs, ledger item 241. A member admitted here holds state from its
	// admission on, so every epoch before it answers not-found through this door and renders as
	// a gap; that is MLS's own answer for a later member and item 241 rules it stays that way.
	if err := session.InstallPastEpochLoader(self.pastEpochLoader(invite.GroupId)); err != nil {
		session.Close()
		return nil, fmt.Errorf("urmessage: the past epoch loader: %w", err)
	}
	group := &Group{
		device:         self,
		id:             append([]byte(nil), invite.GroupId...),
		handle:         handle,
		groupHandleKey: append([]byte(nil), invite.GroupHandleKey...),
		// AT THE EPOCH THE WELCOME ADMITTED THIS DEVICE AT, and at no other. MASTER section 7's
		// out-of-band delivery is what an [Invite] is: the joiner is handed the secret of the
		// epoch it is joining, it was not present for any epoch below and can vouch for none of
		// them, and every epoch ABOVE arrives through that epoch's own device wrap.
		pqSecrets:    map[uint64][]byte{handle.Epoch(): append([]byte(nil), invite.PqSecret...)},
		session:      session,
		sessionBound: nonceEpoch,
		epoch:        handle.Epoch(),
		// The founder opened it. A joiner cannot observe that and does not pretend to: if it
		// has not, every send below is refused by the server and the refusal is returned.
		opened: true,
		// as for a founded group: no OTHER COPY OF THIS FOLDER holds this device's identity in
		// this group, because the identity was drawn here. That is the clone question and it is
		// the only one this flag answers.
		reconciled: true,
		// AND THE OTHER QUESTION IS ANSWERED THE OTHER WAY, WHICH IS LEDGER ITEM 245's FIRST
		// PIECE MADE INTO A GATE. This device's stream in this group does NOT start here: it
		// lands on the leftmost BLANK leaf (RFC 9420 section 7.7), a leaf a removed member may
		// have stood at, and it derives that member's sender_handle byte for byte -- so the
		// server may already hold claims at the indices this device is about to seal at. It
		// cannot know without looking. [Group.seedOwnStreamLocked] looks, on the first walk;
		// until that walk has completed cleanly [Group.Send] answers [ErrStreamFloorUnheld],
		// because a seal is the irreversible half and the refusal that follows one
		// ([ErrIdentityInUse], through REASON_STREAM_INDEX_REUSED) is STICKY for the life of the
		// process.
		//
		// EXCEPT AT EPOCH ONE, WHERE THE LEAF IS FRESH BY CONSTRUCTION AND THE GATE WOULD BE A
		// FALSE POSITIVE ON EVERY GROUP THIS BUILD MAKES. A commit that OPENS epoch one is
		// committed against epoch zero, and [Device.CreateGroup] makes epoch zero with exactly
		// ONE member -- so the only leaf that commit could have removed is the committer's own,
		// which RFC 9420 section 12.4 forbids (mls.ErrRemoveCommitter). There is no blank leaf
		// for section 7.7 to refill, so a device admitted at epoch one is on a leaf nobody has
		// ever stood at and no claim can exist under its handle. Above epoch one that argument
		// is gone: any earlier commit may have removed somebody, and this device cannot see
		// which from a tree snapshot. The narrowing is DRIVEN in both directions rather than
		// asserted -- see the removal suite's gate case -- so a build whose epoch zero ever
		// holds two members turns it red rather than shipping a silent false negative.
		ownFloorHeld: handle.Epoch() <= 1,
	}
	group.initTables()
	if err := self.persistGroup(group.groupRecordLocked(true)); err != nil {
		// the session first and the handle after it, which is [Group.Close]'s own order: the
		// session owns the loop that the handle is reached through.
		session.Close()
		handle.Close()
		return nil, fmt.Errorf(
			"urmessage: this group joined and its record could not be persisted, so a restart would not come back into it: %w", err)
	}
	self.hold(group)
	return group, nil
}

// AddMember adds one device to this group and answers the [Invite] it joins with.
//
// IT IS THE COMMIT THAT OPENS EPOCH ONE, which is why the alpha takes exactly one of them and
// takes it before [Group.Open]. A second add is a second epoch: every member's session has to
// advance, the server has to be handed the new epoch's keys in a new commit, and the wrap fan-out
// has to run again. None of that is built, so a second call is refused by name rather than half
// performed.
func (self *Group) AddMember(keyPackage []byte) (*Invite, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return nil, fmt.Errorf("urmessage: this group is closed")
	}
	if self.opened {
		return nil, ErrGroupOpen
	}
	if self.founding == nil {
		return nil, ErrAlphaOneAdd
	}
	if self.session != nil {
		return nil, ErrAlphaOneAdd
	}
	nonce, nonceEpoch, err := self.device.nonce()
	if err != nil {
		return nil, err
	}

	// BY VALUE AND NOT BY REFERENCE, since 2026-09-18's CommitAdd (ledger item 239, step A2).
	// ProposeAdd-then-Commit names the add by a reference every receiver resolves against its
	// own proposal cache, so it only works while every member is handed the proposal before the
	// commit; the founding add has no other member to hand it to, and the second add -- the one
	// track A builds towards -- would have to fan a proposal record to every member first.
	// CommitAdd carries the Add inside the commit, attributed to the committer, and the SAME
	// call takes N key packages: one commit admits N members. Nothing is staged on its refusal,
	// and mls refuses the same things it refused through ProposeAdd, at the same call.
	commit, welcome, ratchetTree, err := self.handle.CommitAdd([][]byte{keyPackage})
	if err != nil {
		return nil, fmt.Errorf("urmessage: CommitAdd: %w", err)
	}
	if err := self.handle.MergePendingCommit(); err != nil {
		return nil, fmt.Errorf("urmessage: MergePendingCommit: %w", err)
	}
	// THE HANDLE IS AT EPOCH ONE FROM HERE AND self.founding IS STILL AT EPOCH ZERO, which is the
	// one subtlety in this file and is load-bearing rather than incidental. A GroupSession
	// installs its epoch's whole key schedule at construction and reads NOTHING off the handle on
	// the seal path afterwards -- newRecordBuilderOnLoop takes the group id, the sender handle,
	// the epoch and the class keys out of its own fields -- so the founding session goes on
	// sealing epoch zero records after the handle has moved, which is exactly what 4.3.2's
	// self-certified founding commit needs. Delete that property in connect and this file starts
	// sealing the founding commit under epoch one's key, which the server refuses because it
	// verifies it under the bootstrap key the same request carries.
	//
	// AND THE FOUNDING COMMIT'S EPOCH TAKES THE SECRET EPOCH ZERO WAS DRAWN WITH, which is the
	// ONE epoch change in this package that does not rotate, and the reason is the carrier rather
	// than an exception. The device wrap is what delivers a rotated secret, a wrap is addressed to
	// a leaf's published X-Wing key, and at this moment the only OTHER member of this group has
	// not joined yet -- it is holding the Welcome this call is about to answer. Its copy of epoch
	// one's secret is [Invite.PqSecret], out of band, MASTER section 7's founding delivery. So
	// epoch one's entry is filed here as a COPY of epoch zero's, deliberately and once, and every
	// commit to an OPEN group from then on draws a fresh one ([Group.publishCommitLocked]).
	foundingSecret := self.pqSecretLocked()
	self.filePqSecretLocked(self.handle.Epoch(), foundingSecret)
	session, err := messagegroup.NewGroupSession(self.handle, foundingSecret, self.groupHandleKey,
		self.device.reserver, self.device.nowMs, nonce)
	if err != nil {
		return nil, fmt.Errorf("urmessage: the session at epoch %d: %w", self.handle.Epoch(), err)
	}
	// the door to prior epochs, ledger item 241: the founder's session is the one that will
	// commit later adds and then meet, on its next walk, the records the others sealed before
	// the commit it did not fetch first.
	if err := session.InstallPastEpochLoader(self.device.pastEpochLoader(self.id)); err != nil {
		session.Close()
		return nil, fmt.Errorf("urmessage: the past epoch loader: %w", err)
	}
	self.session = session
	self.sessionBound = nonceEpoch
	self.commit = append([]byte(nil), commit...)
	// NOTHING IS PERSISTED HERE EITHER, for CreateGroup's reason carried one step further, and
	// it is written down because a record here LOOKS obviously right and is not.
	//
	// The handle is now at the epoch the commit opened and mls has persisted that epoch's state
	// inside MergePendingCommit above, so a restore could rebuild an MLS member. What it could
	// not rebuild is a group anybody can USE: [Group.Open] needs the epoch-zero founding session
	// to self-certify the founding commit, that session is not persisted, and a restored group
	// therefore answers ErrNoMemberAdded to Open and ErrGroupNotOpen to Send, for ever. A record
	// written here would make "a founder that died before Open" come back as a conversation the
	// user can see and cannot ever send in.
	//
	// MEASURED rather than reasoned: a record written here was deleted and the whole suite
	// stayed green, because [Group.Open] writes the founder's record and [Device.Join] writes
	// the joiner's, and those are the two moments a group becomes usable.
	//
	// AND THE EPOCH STILL MOVES THROUGH THE ONE DOOR. enterEpochLocked is what writes the record
	// on every later epoch change; here it finds the group unopened and writes nothing, which is
	// the paragraph above stated as a rule rather than as a site that remembered.
	if err := self.enterEpochLocked(); err != nil {
		return nil, err
	}
	// THE SECRET OF THE EPOCH THIS INVITE ADMITS INTO, read out of the table after the entry above
	// rather than off a field that used to mean one thing forever. [Device.Join] files it at
	// `handle.Epoch()`, which is the same epoch, and the two sides are two reads of one value.
	return &Invite{
		GroupId:        append([]byte(nil), self.id...),
		Welcome:        append([]byte(nil), welcome...),
		RatchetTree:    append([]byte(nil), ratchetTree...),
		PqSecret:       append([]byte(nil), self.pqSecretLocked()...),
		GroupHandleKey: append([]byte(nil), self.groupHandleKey...),
	}, nil
}

// Open publishes this group on the message server: 6.1's founding commit, the epoch's wrap set,
// and the marker that closes the fan-out.
//
// ALL THREE, BECAUSE THE SERVER WILL NOT TAKE A MESSAGE UNTIL ALL THREE HAVE LANDED. CreateGroup
// leaves the group at epoch one with epoch_complete false, which step (2) makes
// readable-but-not-writable for everything except a wrap, a snapshot or the marker; an ordinary
// record before the marker is answered REASON_EPOCH_INCOMPLETE.
func (self *Group) Open(ctx context.Context) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return fmt.Errorf("urmessage: this group is closed")
	}
	if self.opened {
		return ErrGroupOpen
	}
	if self.session == nil || self.founding == nil {
		return ErrNoMemberAdded
	}
	if err := self.rebindLocked(); err != nil {
		return err
	}

	keys, err := self.session.EpochKeys()
	if err != nil {
		return fmt.Errorf("urmessage: this epoch's keys: %w", err)
	}
	defer keys.Destroy()
	writeKey, err := keys.WriteKey()
	if err != nil {
		return err
	}
	readKey, err := keys.ReadKey()
	if err != nil {
		return err
	}
	bootstrap, err := self.founding.EpochKeys()
	if err != nil {
		return fmt.Errorf("urmessage: epoch zero's keys: %w", err)
	}
	defer bootstrap.Destroy()
	bootstrapWriteKey, err := bootstrap.WriteKey()
	if err != nil {
		return err
	}

	groupContext, err := self.handle.GroupContextBytes()
	if err != nil {
		return fmt.Errorf("urmessage: the group context: %w", err)
	}
	contextHash := sha256.Sum256(groupContext)

	// THE FOUNDING FAN-OUT IS THE ONE THAT CARRIES NO KEY MATERIAL, and it stays that way. Every
	// other epoch's fan-out delivers pq_secret[n+1] under X-Wing to each leaf that is already a
	// member; at epoch one there is no such leaf -- the only other member is holding the Welcome
	// and takes its copy out of band in [Invite.PqSecret], MASTER section 7's founding delivery.
	// A wrap addressed to a leaf for a secret the device at that leaf already has would be a second
	// copy of the same value on the wire for nothing. See [alphaWrapBody], and pqepoch.go's header.
	//
	// AND IT IS THE ONE FAN-OUT WITH NO STAGED COMMIT BEHIND IT, which is why it goes through its
	// own door: [Group.AddMember] merged the founding commit before this group was publishable, so
	// there is nothing staged to read an exclusion off, and a group standing at its founding epoch
	// has carried no removal for one to be about. Every other fan-out in this package is
	// [Group.wrapTargetsAtLocked]'s and derives its exclusion off the staged commit.
	wrapTargets, err := self.foundingWrapTargetsLocked(self.epoch)
	if err != nil {
		return err
	}
	if len(wrapTargets) == 0 {
		return ErrNoMemberAdded
	}

	// (1) the founding commit, sealed at epoch zero and carrying the epoch it opens.
	//
	// ITS ATTACHMENT IS KIND 0x0005 AND CARRIES NO KEY, which is item 244 CLOSED at this site.
	// [Group.publishCommitLocked] carries the whole argument; the short form is that what the
	// server serves back to every reader, for as long as the group exists, is
	// `LP(H(epoch_keys))` -- a digest the server RECOMPUTES from the pair handed to it beside the
	// record, on the request, under ruling 33. The pair reaches the server once, on a message no
	// server→client type can carry, and is never in anything served.
	group, err := epochDigestGroupId(self.id)
	if err != nil {
		return err
	}
	foundingDigest, err := message.NewEpochDigestAttachment(group, message.EpochDigestAttachment{
		Epoch:             self.epoch,
		AlgId:             epochAttachmentAlgId,
		GroupContextHash:  contextHash[:],
		ExpectedWrapCount: uint32(len(wrapTargets)),
	}, writeKey, readKey)
	if err != nil {
		return fmt.Errorf("urmessage: the digest of the keys epoch %d opens with: %w", self.epoch, err)
	}
	founding, err := self.founding.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(self.device.nowMs()), self.commit, 0, &message.ServerAttachment{
			Kind:        message.AttachmentEpochDigest,
			EpochDigest: foundingDigest,
		})
	if err != nil {
		return fmt.Errorf("urmessage: sealing the founding commit: %w", err)
	}
	// RULING 33's ROAD FOR EPOCH 1'S TWO KEYS, DECIDED OFF THE RECORD THAT WAS JUST SEALED.
	// [epochKeysFor] answers a delivery for a kind 0x0005 commit and nil for a kind 0x0001 one,
	// because under 0x0001 the keys are already inside the attachment and a delivery beside one is
	// refused. The record above is 0x0005, so THIS ROAD IS THE ONE THE PAIR NOW TRAVELS, and the
	// decision is still read off the sealed octets rather than off the literal a dozen lines up:
	// the two sides of that question must not come from one expression, or the check cannot fail.
	// §4.3.2's `epoch_keys` is SINGULAR -- this request carries exactly one record and it
	// is always a commit -- so there is no alignment to compute here and no list to keep in step.
	//
	// AND THESE ARE EPOCH 1'S KEYS, NOT EPOCH 0'S. `bootstrap_write_key` below is write_key[0] and
	// is a different field for a different job: it is what the server verifies the founding
	// commit's own `write_auth` under, and §4.3.2 calls it self-certification protected by nothing
	// but a rate limit. The pair here is what the commit OPENS -- the epoch the attachment names --
	// and the two must never be confused, which is the reason they are derived from two different
	// sessions a dozen lines apart and copied at two different sites.
	delivery, err := epochKeysFor(founding, writeKey, readKey)
	if err != nil {
		return fmt.Errorf("urmessage: the epoch keys this group opens epoch %d with: %w", self.epoch, err)
	}
	created := append([]byte(nil), bootstrapWriteKey...)
	if _, err := self.sendSealedLocked(ctx, self.founding, founding, "the founding commit",
		func(record *protocol.Record) (protocol.Reason, uint64, error) {
			response, err := self.device.transport.Call(ctx, &protocol.CreateGroupRequest{
				GroupId:           self.id,
				InitialCommit:     record,
				BootstrapWriteKey: created,
				EpochKeys:         delivery,
			})
			if err != nil {
				return protocol.Reason_REASON_INTERNAL, 0, err
			}
			if response.GetReason() != protocol.Reason_REASON_OK {
				return response.GetReason(), 0, nil
			}
			body := response.GetCreateGroup()
			if body == nil {
				return protocol.Reason_REASON_INTERNAL, 0, fmt.Errorf("%w: the response carried no create_group arm", ErrCreateRefused)
			}
			if body.GetCurrentEpoch() != self.epoch {
				return protocol.Reason_REASON_INTERNAL, 0, fmt.Errorf(
					"%w: the group opened at epoch %d and this device is at %d",
					ErrCreateRefused, body.GetCurrentEpoch(), self.epoch)
			}
			return protocol.Reason_REASON_OK, body.GetRecordId(), nil
		}); err != nil {
		return err
	}

	// (2) the wrap set: one per member, carrying no key material. See [alphaWrapBody].
	for _, target := range wrapTargets {
		wrap, err := self.session.SealRecord(message.RetentionPermanent, 0, false,
			encodeHead(self.device.nowMs()), []byte(alphaWrapBody), 0, &message.ServerAttachment{
				Kind: message.AttachmentWrap,
				Wrap: &message.WrapTag{WrapTargetHandle: append([]byte(nil), target.handle[:]...), Epoch: self.epoch},
			})
		if err != nil {
			return fmt.Errorf("urmessage: sealing an epoch wrap: %w", err)
		}
		if _, err := self.submitLocked(ctx, self.session, wrap, "an epoch wrap", nil); err != nil {
			return err
		}
	}

	// (3) the marker that closes the fan-out and makes the group writable.
	marker, err := self.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(self.device.nowMs()), []byte(alphaEpochCompleteBody), 0, &message.ServerAttachment{
			Kind:     message.AttachmentComplete,
			Complete: &message.EpochComplete{Epoch: self.epoch, WrapCount: uint32(len(wrapTargets))},
		})
	if err != nil {
		return fmt.Errorf("urmessage: sealing the epoch complete marker: %w", err)
	}
	if _, err := self.submitLocked(ctx, self.session, marker, "the epoch complete marker", nil); err != nil {
		return err
	}

	self.opened = true
	// The opened bit, so that a restarted device knows the group is publishable rather than
	// finding out at its first send. The error says what actually happened: the group IS open
	// on the server, and it is the RECORD that did not land.
	if err := self.device.persistGroup(self.groupRecordLocked(true)); err != nil {
		return fmt.Errorf("urmessage: this group is open on the server and its record could not be persisted, so a restart would refuse to send in it: %w", err)
	}
	return nil
}

// AddMemberAndPublish adds one device to an ALREADY-OPEN group and publishes the epoch it opens:
// the commit that admits the member, the wrap fan-out for the new epoch, and the marker that closes
// it -- and it answers the [Invite] the new member joins with.
//
// IT IS THE SECOND-EPOCH SIBLING OF [Group.AddMember] + [Group.Open], and the split is the alpha's
// two shapes of an add. AddMember/Open is the FOUNDING add: it runs before the group is open, needs
// the epoch-zero founding session to self-certify the founding commit, and reaches the server
// through CreateGroup. This one is every add AFTER: the group is open, there is no founding session,
// and the commit is an ordinary submit that opens the next epoch. Item 239 is what lifted the
// one-add limit that used to make this method [ErrAlphaOneAdd].
//
// THE COMMIT RECORD IS SEALED AT THE OLD EPOCH AND ANNOUNCES THE NEW ONE, which is the one subtlety.
// The server takes a commit iff its header names the current epoch and its attachment opens the
// next, so [Group.session] -- at the old epoch, which is where the handle still stands too, because
// the commit is STAGED and not merged until the server has taken it -- seals the record, while the
// write and read keys the attachment carries are derived off the staged epoch's exporter through
// the seam's PendingExport. The merge, the session's advance, A4's re-track and A3's persist all
// follow the server's REASON_OK, in [Group.publishCommitLocked]; a refusal erases the staged epoch
// and leaves this group exactly where it was, and the race's two reasons come back as
// [ErrCommitLost]. Until 2026-09-22 the merge came FIRST and a lost race forked this device.
//
// TWO WRITERS OF EPOCH STATE, IN ORDER. mls persists the new epoch's MLS state inside
// MergePendingCommit, after the server's answer; [Group.enterEpochLocked] persists this package's
// record after the whole ceremony -- and nothing is a third writer. A restored group is refused
// ([ErrNotReconciled]) until it has received once, because a committer that has not checked its
// own stream against the server must not seal.
//
// AND IT IS REFUSED BEFORE IT IS BUILT WHEN THIS DEVICE'S ROLE DOES NOT PERMIT IT, which is the
// committing arm of MASTER §11 (ledger item 242's R2, ruling 1): an Add of a new identity is an
// ADMIN's or the OWNER's, and an identity's own second device is its own to add at any role. The
// decision is [authorizeCommit] -- the one predicate every receiver judges the commit by -- over the
// value the commit WOULD produce ([Group.authorizeOutgoingLocked]), so a MEMBER's add is answered
// here with the receivers' own sentence, [ErrCommitUnauthorized] wrapping [ErrCommitAddByNonAdmin],
// and nothing is built, merged or published. Before R2 the non-founder alone moved to n+1 and the
// group halted for everyone else.
func (self *Group) AddMemberAndPublish(ctx context.Context, keyPackage []byte) (*Invite, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if err := self.committableLocked(); err != nil {
		return nil, err
	}
	// (0) THE SEND-SIDE AUTHORIZATION, before the connection is consulted and before anything is
	// built: a refusal is a fact about this group and this device's role, and it costs no round
	// trip and touches no session.
	if _, err := self.authorizeOutgoingLocked(&outgoingCommit{addKeyPackages: [][]byte{keyPackage}}); err != nil {
		return nil, err
	}
	if err := self.rebindLocked(); err != nil {
		return nil, err
	}

	// (1) build the commit that admits the new member, BY VALUE. It is STAGED behind the seam and
	// not merged: the handle and self.session both stay at the epoch that is closing, and the
	// Welcome and the ratchet tree it answers are the joiner's whether or not the commit lands.
	commit, welcome, ratchetTree, err := self.handle.CommitAdd([][]byte{keyPackage})
	if err != nil {
		return nil, fmt.Errorf("urmessage: CommitAdd: %w", err)
	}

	// (2)-(5) announce, submit, and -- once the server has taken it -- merge, enter and fan out
	// the epoch the commit opens. A refusal erases the staged epoch and answers here with nothing
	// moved; the key package is the joiner's and a retry after Receive may offer it again.
	if err := self.publishCommitLocked(ctx, commit); err != nil {
		return nil, err
	}

	// THE ROTATED SECRET AND NOT THE ONE THE GROUP HAD A MOMENT AGO. publishCommitLocked drew
	// pq_secret[n+1], fanned it out to the members that were already here and entered the epoch,
	// so the table's current entry IS the new epoch's -- which is what this joiner needs and what
	// no wrap could have carried to it, because the leaf it will occupy did not exist when the
	// fan-out was sealed. See [Group.wrapTargetsAtLocked]'s Add arm.
	return &Invite{
		GroupId:        append([]byte(nil), self.id...),
		Welcome:        append([]byte(nil), welcome...),
		RatchetTree:    append([]byte(nil), ratchetTree...),
		PqSecret:       append([]byte(nil), self.pqSecretLocked()...),
		GroupHandleKey: append([]byte(nil), self.groupHandleKey...),
	}, nil
}

// committableLocked is every refusal a commit to an ALREADY-OPEN group owes before it looks at
// what is being committed: the guard block [Group.AddMemberAndPublish] carried alone until
// [Group.SetRole] and [Group.TransferOwnership] came to owe the same five, in the same order.
// It is [Group.sendableLocked] with the open check ahead of the session check, because a group
// that is not open answers ErrGroupNotOpen to a commit whatever else it lacks.
func (self *Group) committableLocked() error {
	if self.closed {
		return fmt.Errorf("urmessage: this group is closed")
	}
	if !self.opened {
		return ErrGroupNotOpen
	}
	if self.session == nil {
		return ErrNoMemberAdded
	}
	// A DEVICE THIS GROUP REMOVED COMMITS NOTHING -- RULING 52, and it is first for
	// [Group.commitWalkLocked]'s reason: it is the only clause here that is not about how this
	// device is doing in a group it is in. It is also the only one mls would refuse anyway, which is
	// exactly why it belongs by name: a closed group answers `the group is closed and its epoch
	// secrets have been zeroized` to every commit verb, which is a sentence about a data structure
	// where what the caller needs is a sentence about a membership.
	if self.removed != nil {
		return self.removed
	}
	if self.identityInUse != nil {
		return self.identityInUse
	}
	// A GROUP THAT WENT DARK MUST NOT COMMIT EITHER, and the reason is worse than for a send: a
	// commit built on a storage_root no peer reproduces opens an epoch whose write and read keys
	// nobody else can derive, and every member that follows it is dark behind this one. Ruling
	// 38's sentinel is returned rather than the REASON_REJECTED the server would answer with.
	if self.wrapDark != nil {
		return self.wrapDark
	}
	// AND A GROUP THAT HALTED MUST NOT COMMIT, for a different reason that comes to the same
	// refusal: it stands at an epoch the server has already left. A commit sealed at n is a write
	// at a stale epoch and the server refuses it, and building one on top of a commit this device
	// judged INVALID would be this member re-opening the epoch the removal was supposed to close.
	// Ruling 41's sentinel by name, rather than the server's reason code.
	if self.halted != nil {
		return self.halted
	}
	if !self.reconciled {
		return fmt.Errorf("%w: group %x", ErrNotReconciled, self.id)
	}
	// AND A COMMIT IS A RECORD LIKE ANY OTHER: it is sealed through
	// [messagegroup.GroupSession.SealRecord] and spends a stream index under this device's own
	// sender_handle, so a committer whose floor has not been held collides exactly as a sender
	// does -- and the group it was trying to change is the one it can then never write to. See
	// [Group.ownFloorHeld].
	if !self.ownFloorHeld {
		return self.streamFloorRefusalLocked()
	}
	return nil
}

// streamFloorRefusalLocked is the one place [ErrStreamFloorUnheld] is built, because the two doors
// that answer it -- the send door and every commit door -- must not drift apart on WHY.
//
// IT IS ONE SENTENCE AND IT IS CLEARED BY THE NEXT CLEAN [Group.Receive], WHICH IS THE WHOLE OF WHAT
// IT PROMISES. It used to have a second arm for a record given up on unparsed, whose text said "AND
// NOT BY THE Receive ABOVE" -- a refusal that no Receive cleared and that a restart re-derived,
// because the cursor is not persisted. There is no such state any more: a row this build cannot
// parse contributes the server's own §4.3.3 projection of its stream
// ([Group.noteUnparsedClaimLocked]) and the floor is raised off it, so the only thing that keeps
// this refusal standing is a walk that has not finished yet. See [Group.ownFloorHeldByLocked].
func (self *Group) streamFloorRefusalLocked() error {
	return fmt.Errorf("%w: group %x", ErrStreamFloorUnheld, self.id)
}

// publishCommitLocked publishes the epoch a commit this device has BUILT AND STAGED opens: steps
// (2) to (5) of [Group.AddMemberAndPublish], which is where they stood until the role model's
// committing arm (ledger item 242's R2) gave a policy commit the same road. THE HANDLE HAS NOT
// MOVED when this is entered -- the commit is staged behind the seam and nothing has been merged --
// and it is this method that decides whether it ever does: the server's answer to the commit
// record is what merges it or erases it.
//
// SUBMIT FIRST, MERGE SECOND, which is MASTER §9.3's order and the seam's own: the delivery service
// accepts at most one commit per (group, epoch), a loser "re-derives against the winner and
// retries", and [messagegroup.GroupHandle.Commit]'s contract stages rather than merges because "a
// committer that merged optimistically would fork itself off the group". Until 2026-09-22 this
// method was entered AFTER the merge all the same, because the record that announces an epoch
// carries facts of that epoch and the live handle after the merge was the only door onto them.
// Measured: an owner whose transfer lost the race to an admin's role change, within one fetch
// interval, was left with a handle at a private n+1 while the group stood at n -- Receive refused
// the winner's commit ("message does not decrypt"), Send was answered EPOCH_STALE, and Members()
// showed the policy that never landed -- dead until the app restarted. The seam's
// [messagegroup.GroupHandle.PendingEpoch] and [messagegroup.GroupHandle.PendingExport] are the
// facts read off the STAGED value instead, so the announcement is built, sealed and submitted
// with the group still at n, and the merge waits for REASON_OK.
//
// ON ANY OTHER ANSWER THE STAGED EPOCH IS ERASED AND THE GROUP STAYS WHERE IT WAS. A refusal is
// spec B §6.2's "any rejection of a commit submission", whose first step is to discard the
// provisional epoch, and the two reasons that name the race -- COMMIT_LOST and EPOCH_STALE, read by
// [epochRaceRefusal] before S2-2's recovery is spent on them -- come back as [ErrCommitLost], which
// tells the caller what it owes: Receive, then the verb again. A transport error is treated the
// same way, and that is a choice with a cost, stated: the answer was lost, so this device cannot
// know whether the server stored the record, and if it did, this device cannot follow its own
// commit through Receive (a committer cannot open its own update path) -- the same dead end a
// restart met under the old order. The other reading, merge on a guess, is the fork the old order
// produced on every refusal; between a rare dead end and a routine fork, the routine one goes.
// What closes the rare one is §6.3's idempotent resubmission of the SAME record under §6.2 step
// 7's backoff, and neither is built.
//
// THERE IS NO WELCOME HERE AND NOTHING HERE WANTS ONE: a Welcome is the joiner's, handed over out of
// band in an [Invite], and the server never sees it. A policy commit produces none and an Add
// produces one, and the ceremony record -- the commit body under an epoch attachment, the wrap set,
// the marker -- is the same three records either way. expected_wrap_count is the STAGED tree's
// member count, and the fan-out after the merge wraps to the live tree's members: they are one
// tree, and the seam's own test holds the two readings equal across a merge.
//
// THE LEAVES THIS COMMIT TAKES OUT OF THE GROUP ARE NOT AN ARGUMENT AND NO ARM PASSES THEM. The
// fan-out is built pre-merge, from the LIVE tree (the seam publishes no staged leaf's X-Wing key),
// and a removed member is still standing in that tree -- so a fan-out that did not exclude it
// would hand the member this commit removes the next epoch's post-quantum secret, which is item
// 243's whole subject arriving inverted. Until 2026-09-25 that exclusion was a `removing []uint32`
// parameter on this method, passed down by whichever arm had built the commit, on the recorded
// reasoning that there was nothing to read it off. Ledger item 257's ruling 51 refuted the
// reasoning and `connect 98b72dfa` built the read: the staged commit's own removed leaves are a
// field on the [messagegroup.PendingEpoch] value step (2) below ALREADY reads, one line from here,
// post-CreateCommit and pre-merge. So the derivation and the epoch come off one answer, and the
// omission class an arm can cause by forgetting an argument is gone by construction rather than by
// a sentence asking arms to remember. What remains is a bug in the fan-out, which is what ruling
// 54 leaves "re-found the group" as the recovery for.
//
// THE PROPERTY THAT HOLDS IT is `TestThreeMembersRotateAcrossTwoEpochsAndAMemberRemovedByThatCommitCannotFollow`:
// a three-member group where the second rotation's commit removes one member, holding that the
// epoch's fan-out addresses every survivor and not the removed leaf, that the removed member's
// RETAINED pq_secret does not reproduce the survivors' storage_root even when the epoch's exporter
// is granted to it, and that its own table never gains the removed-from epoch's row. The exclusion
// it drives is this method's derivation and no argument of the case's own, which is what makes it
// a property of the code rather than of the fixture.
func (self *Group) publishCommitLocked(ctx context.Context, commit []byte) error {
	// (2) the facts of the epoch the staged commit opens, off the staged value: the epoch, the
	// member count the fan-out will wrap to, the group context the server keys the epoch under,
	// and the NEW epoch's write and read keys, derived STRAIGHT OFF the staged exporter -- the
	// same three steps installEpochOnLoop takes inside a session -- rather than off a second
	// GroupSession, because a GroupSession's Close closes the handle it shares with this group.
	//
	// WHERE write_key AND read_key TRAVEL, AS OF RULING 33 AND AS THE TREE STANDS TODAY. The road
	// ruling 33 built is the REQUEST, in `SubmitRequest.epoch_keys` aligned with the record, and
	// IT IS THE ROAD THEY TRAVEL. The record carries a kind 0x0005 `EpochDigestAttachment` --
	// ruling 27's substitution -- which holds the six PUBLIC fields of the old `EpochAttachment`
	// and `LP(H(epoch_keys))` where the pair used to be. So the two keys cross the wire exactly
	// once, on a request message, and `protocol.Record` -- the server→client type in six places --
	// is structurally unable to carry them. THAT IS ITEM 244 CLOSED AT THIS SITE.
	//
	// WHAT THE SERVER DOES WITH THE DIGEST, because a digest that nothing checked would be
	// decoration. §5.1 check 3 recomputes `H(epoch_keys)` over the keys the REQUEST carried and
	// compares it against the attachment's field, constant time, in
	// `message.CheckEpochKeysDigest`. The binding is free and needs no new authenticator:
	// `LP(H(server_attachment))` is already inside the `write_auth` preimage, so the MAC covers
	// the attachment, the attachment covers the digest, and the digest covers the keys. Bend
	// either key and the recomputation fails; bend the digest and `write_auth` fails.
	//
	// AND THE SERVER MAKES THE TWO A PACKAGE, WHICH IS WHY EXACTLY ONE ROAD IS USED AND NOT BOTH.
	// §5.4's acceptance window is keyed on the attachment kind: a kind 0x0001 commit with a
	// delivery beside it is REFUSED -- "a kind 0x0001 commit arrived with an epoch key delivery
	// beside it" -- because under 0x0001 the server reads the keys out of the attachment and a
	// delivery is a second copy it would not read; and a kind 0x0005 commit WITHOUT one is refused
	// the other way, as an epoch the server was never handed what opens. Both directions are
	// measured through the real server in cp3b's TestItem244TheServedCommitHandsOutNoEpochKey. So
	// [epochKeysFor] asks the sealed
	// record which kind it is and answers accordingly, which is the server's own rule read off the
	// same octets, and not a feature flag.
	//
	// THE EPOCH IS READ ONCE, by `message.NewEpochDigestAttachment`, out of the body it is
	// building. The digest's preimage carries `opens_epoch`, and there are THREE epochs live at
	// this call site -- the record header's (the epoch the commit is sealed AT), the attachment's
	// (the epoch it OPENS, one higher), and the server's own current_epoch + 1. A wrong choice
	// among them type checks, so the constructor takes no epoch parameter and there is no second
	// one here to disagree with the body.
	pending, err := self.handle.PendingEpoch()
	if err != nil {
		// THE FIRST EXIT ERASES LIKE EVERY LATER ONE. A staged commit whose facts cannot be read
		// is a staged commit all the same, and one left behind here would ride under the next
		// verb's build: the seam's by-value arms refuse to stage over a pending value, so the
		// group would answer every later commit with a sentence about this one.
		self.handle.ClearPendingCommit()
		return fmt.Errorf("urmessage: the epoch the staged commit opens: %w", err)
	}
	newEpoch := pending.Epoch

	// (2a) and (2b) THE ROTATION AND THE FAN-OUT IT IS CARRIED BY, both decided by ONE call:
	// [Group.stageEpochRotationLocked]. It is a function rather than thirty lines here because
	// everything it decides has to be decided together -- the secret, who gets it, and the rows
	// that carry it -- and because a test that re-spelled those three steps would measure its own
	// arithmetic. MEASURED: it did. The first draft of this file left the draw inline and the
	// property suite built its own fan-out beside it, so a mutant that reused one secret across
	// every epoch -- item 243's whole subject, inverted -- passed the entire suite.
	//
	// AND THE WHOLE STAGED VALUE GOES DOWN, not the epoch off it: `pending.RemovedLeaves` is where
	// the fan-out's exclusion now comes from, so the epoch the rotation is FOR and the leaves it
	// must leave OUT are two fields of one read taken at one instant. See the header above.
	staged, err := self.stageEpochRotationLocked(pending)
	if err != nil {
		self.handle.ClearPendingCommit()
		return err
	}
	pqNext, targets := staged.pqSecret, staged.targets
	defer zeroizeState(pqNext)

	newMlsSecret, err := self.handle.PendingExport(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.handle.ClearPendingCommit()
		return fmt.Errorf("urmessage: the new epoch's exporter: %w", err)
	}
	newRoot := messagegroup.StorageRoot(newMlsSecret, pqNext)
	writeKey := message.WriteKey(newRoot)
	readKey := message.ReadKey(newRoot)
	zeroizeState(newMlsSecret)
	zeroizeState(newRoot)
	contextHash := sha256.Sum256(pending.GroupContext)

	// (2c) RULING 37's WRAPS: SEALED AND SUBMITTED HERE, PRE-MERGE, AT EPOCH n -- AND AHEAD OF THE
	// COMMIT RECORD.
	//
	// AHEAD OF IT, which is the half the ruling states as a consequence rather than as an order,
	// and it is forced by the server rather than chosen here. A write is accepted only at the
	// group's CURRENT epoch; the moment the commit is taken, current_epoch is n+1; so a wrap
	// sealed at n and submitted after the commit is REASON_EPOCH_STALE. Before it, the group is
	// still at n, the fan-out is complete at n, and item 246's F0 ceiling serves a reader standing
	// at n both these rows and the commit -- "the commit and its wrap in one page", one round
	// trip, no server change. After the merge would also be one epoch too late for the OTHER
	// reason the ruling gives: at n+1 a reader at n is served none of it, and read_key[n+1] needs
	// pq_secret[n+1] needs the wrap. Circular.
	//
	// AND THE COST IS THE ORPHAN. A fan-out written before the race is a fan-out that outlives a
	// LOST race, addressed to an epoch that never opened under this secret. That is item 132's
	// orphan case, it is produced here by design, and it is why the detector ships in the same
	// commit (ruling 38): see [ErrOrphanWrap] and [Group.resolvePqSecretLocked].
	for _, wrap := range staged.wraps {
		if _, err := self.submitLocked(ctx, self.session, wrap, "an epoch wrap", nil); err != nil {
			self.handle.ClearPendingCommit()
			return err
		}
	}

	// (3) the commit record, sealed at the OLD epoch by self.session -- which is the epoch the
	// handle is still at -- announcing the new epoch. The server takes it iff its header names
	// the current epoch and its attachment opens the next.
	group, err := epochDigestGroupId(self.id)
	if err != nil {
		self.handle.ClearPendingCommit()
		return err
	}
	commitDigest, err := message.NewEpochDigestAttachment(group, message.EpochDigestAttachment{
		Epoch:             newEpoch,
		AlgId:             epochAttachmentAlgId,
		GroupContextHash:  contextHash[:],
		ExpectedWrapCount: uint32(len(targets)),
	}, writeKey, readKey)
	if err != nil {
		self.handle.ClearPendingCommit()
		return fmt.Errorf("urmessage: the digest of the keys epoch %d opens with: %w", newEpoch, err)
	}
	commitRecord, err := self.session.SealRecord(message.RetentionPermanent, 0, true,
		encodeHead(self.device.nowMs()), commit, 0, &message.ServerAttachment{
			Kind:        message.AttachmentEpochDigest,
			EpochDigest: commitDigest,
		})
	if err != nil {
		self.handle.ClearPendingCommit()
		return fmt.Errorf("urmessage: sealing the epoch commit: %w", err)
	}
	// (3a) ruling 33's road for those two keys: BESIDE the record, on the request that carries it,
	// and never on `protocol.Record`. The decision is [epochKeysFor]'s and it is read off the
	// attachment kind in the octets just sealed, which is how the server reads it -- so the kind
	// 0x0005 commit above answers the pair, and a kind 0x0001 commit would answer nil and ride
	// alone. The 0x0001 arm is not dead code: §5.4's acceptance window is dated and both kinds are
	// accepted until it closes, so a client built from this source and pointed at a server -- or a
	// record replayed out of a store -- can still meet one. The delivery COPIES, because the pair
	// is also inside a sealer's hands here and `EpochKeys.Destroy` zeroizes the backing array.
	delivery, err := epochKeysFor(commitRecord, writeKey, readKey)
	if err != nil {
		self.handle.ClearPendingCommit()
		return fmt.Errorf("urmessage: the epoch keys this commit opens epoch %d with: %w", newEpoch, err)
	}
	if _, err := self.submitLocked(ctx, self.session, commitRecord, "an epoch commit", delivery); err != nil {
		// THE STAGED EPOCH IS ERASED ON EVERY ANSWER BUT REASON_OK, through the seam's own door,
		// and the group is exactly where it was: handle, session and epoch all at the epoch the
		// commit was built against, nothing persisted, nothing announced. The error says which
		// kind of answer it was -- ErrCommitLost for the race, the submit's own otherwise.
		self.handle.ClearPendingCommit()
		return err
	}

	// (4) the epoch is open on the server, and ONLY NOW does this device enter it: the merge is
	// where mls persists the new epoch's state, the first of the two writers of epoch state.
	// Then advance this group's OWN session onto it -- reusing the lifetime pq_secret (item 243),
	// the same value AdvanceEpoch re-extracts the new root from -- carry the ladder bookkeeping
	// across (A4) and persist through the one door (A3), the second writer. Session and epoch
	// move together, so a persist failure leaves the disk behind and never leaves the two
	// disagreeing. AdvanceEpoch is the committer's install, as ApplyCommit's AdvanceEpoch is the
	// receiver's; either way self.session is the one session, never a second one.
	if err := self.handle.MergePendingCommit(); err != nil {
		return fmt.Errorf("urmessage: the server accepted the commit that opens epoch %d and this device could not merge it: %w", newEpoch, err)
	}
	// THE ROTATED SECRET IS FILED AND ADVANCED WITH, AND IT IS ONE VALUE READ TWICE RATHER THAN
	// TWO. connect's AdvanceEpoch now REFUSES a value that differs from an entry already standing
	// at the epoch it is entering -- messagegroup.ErrPqSecretEpochConflict, added in the same
	// track's step 3 -- so a caller that filed pq_secret[n+1] and then advanced with the group
	// scalar meets a typed refusal at this line instead of the silent blackout it used to get.
	// Both halves take pqNext.
	self.filePqSecretLocked(newEpoch, pqNext)
	if err := self.session.AdvanceEpoch(pqNext); err != nil {
		return fmt.Errorf("urmessage: advancing the session to epoch %d: %w", newEpoch, err)
	}
	self.commit = append([]byte(nil), commit...)
	// (4a) WHAT THIS DEVICE'S OWN COMMIT TOOK OUT OF THE GROUP, FILED AND PRUNED -- THE TWO LINES
	// [Group.ingestCommitLocked]'s step (5a) has run since ledger item 245, ON THE ARM THAT INGESTS
	// SOMEBODY ELSE'S COMMIT. Until [Group.RemoveMember] there was no verb that could put a leaf in
	// `pending.RemovedLeaves` here, so the committer's own arm owed nothing; the day the removal verb
	// ships, the ADMIN that removes somebody is the one device in the group that does not learn it
	// from an ingest, and the two costs land on it exactly as they would on a receiver:
	//
	//   - THE FILING. The removed member's records sit BELOW this commit in record order and are the
	//     whole of its half of the conversation. The cursor is not persisted, so this device re-walks
	//     them at its next restart -- and without [Group.departedAt], [Group.leavesAtLocked] cannot
	//     resolve the sender_handle they carry once the leaf is out of the membership: each takes the
	//     fail() road and is abandoned after [maxRecordAttempts]. The admin who removed somebody
	//     would be the ONE member of the group that loses their history.
	//   - THE PRUNE, and it must precede [Group.crossEpochLadderLocked] for that function's own
	//     reason: it re-tracks a ladder for every entry of [Group.peerHeads] at the new epoch, so a
	//     head left behind here installs a receiver ratchet for a leaf that no longer stands in this
	//     group, positioned at the removed member's last index -- which §7.7's next Add then refills.
	//
	// The order and the argument are step (5a)'s, and the vector is the STAGED commit's own
	// `pending.RemovedLeaves`: the same field the fan-out above took its exclusion from, so the
	// leaves this device stops wrapping to and the leaves it files as departed are one read.
	self.noteDepartedLeavesLocked(pending.RemovedLeaves, newEpoch)
	self.pruneRemovedLaddersLocked(pending.RemovedLeaves)
	if err := self.crossEpochLadderLocked(newEpoch); err != nil {
		return err
	}
	if err := self.enterEpochLocked(); err != nil {
		return err
	}

	// (5) the marker that makes the new epoch writable. The wraps it closes went out at (2c),
	// before the commit, and its wrap_count is the length of the SAME target list
	// expected_wrap_count was taken from -- one expression, so the server's only fan-out check
	// cannot be defeated by this client disagreeing with itself. That is the half of item 132 a
	// client can close; the half it cannot is that neither store counts a wrap row, which is why
	// the receive side binds the rows to the epoch through H(epoch_keys) instead.
	marker, err := self.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(self.device.nowMs()), []byte(alphaEpochCompleteBody), 0, &message.ServerAttachment{
			Kind:     message.AttachmentComplete,
			Complete: &message.EpochComplete{Epoch: self.epoch, WrapCount: uint32(len(targets))},
		})
	if err != nil {
		return fmt.Errorf("urmessage: sealing the epoch complete marker: %w", err)
	}
	if _, err := self.submitLocked(ctx, self.session, marker, "the epoch complete marker", nil); err != nil {
		return err
	}
	return nil
}

// enterEpochLocked moves this group to the epoch its handle now stands at, AND WRITES THE RECORD IN
// THE SAME BLOCK. It is the one place [Group.epoch] is assigned, and epochpersist_test.go holds
// the package to that by reading the syntax tree rather than by trusting this sentence.
//
// WHY ONE DOOR. [GroupRecord.Epoch] is the epoch `messagegroup.GroupEngine.LoadGroup` is asked
// for at the next restart, and nothing below this package can answer "the latest" -- the mls
// store holds one blob per epoch and enumerates none of them. Until this door the record was
// written at exactly two moments, [Device.Join] and [Group.Open], both of which are epoch one.
// Every epoch change after them -- a commit this member ingests, an add it makes to a live group
// -- moved the handle and left the record naming the epoch before, and mls keeps 32 past epochs'
// state, so the next restart did not refuse: LoadGroup answered the OLD epoch, internally
// consistent in every way, and the restored device sealed under a schedule every peer had left.
// Nothing on that path says so. A record that moves with the epoch is what makes the restart
// come back at the epoch the group is at, and the only way a site can forget to write it is to
// not go through here, which the gate refuses.
//
// A GROUP WITH NO RECORD YET WRITES NONE. [Group.AddMember]'s paragraph is the rule: the
// founder's record is written by Open, because a founder that died before Open must NOT come
// back as a conversation it can see and never send in. `opened` is the field both record
// writers set, so it is the fact "a record exists" read off the group rather than a second flag
// to keep agreeing with the first.
//
// THE CALLER HAS ALREADY REBUILT THE SESSION, or is about to and holds no record of the epoch
// either way. This method does not touch [Group.session]: a session is an epoch's whole key
// schedule installed at construction, and which nonce, reserver and clock it is built over is
// the caller's business. What this method owes is that the number the next restart is handed is
// the number the handle answers now, and that the two are written in one place.
//
// ON A REFUSAL THE IN-MEMORY GROUP HAS MOVED AND THE DISK HAS NOT, and the error says so. The
// alternative -- move the field only after the write -- leaves a group whose session is at one
// epoch and whose epoch field names another, which every header this group seals would carry.
func (self *Group) enterEpochLocked() error {
	self.epoch = self.handle.Epoch()
	// THE WINDOW IS A FUNCTION OF THIS FIELD, SO IT IS BOUNDED WHERE THIS FIELD MOVES. The
	// pq_secret table's bound is `self.epoch - epoch > PastEpochWindow`, which cannot be evaluated
	// correctly anywhere the epoch has not yet moved: [Group.filePqSecretLocked] runs BEFORE the
	// advance on both the commit path and the ingest path, so its own drop is always one epoch
	// behind. MEASURED as exactly that -- a member walked to epoch 33 kept its epoch-zero entry,
	// persisted it, and the next restore refused the whole group because
	// messagegroup.InstallPqSecret will not file a row 33 epochs behind. Here it is one line at
	// the one door, ahead of the persist, so what reaches the disk is already inside the bound.
	self.dropPqSecretsBelowWindowLocked()
	if !self.opened {
		return nil
	}
	// AND THE pq_secret TABLE GOES WITH IT, THROUGH THIS SAME DOOR. The epoch number and the
	// secret that epoch runs on are one fact: a record naming epoch n+1 beside a table whose
	// highest entry is n is a restart that comes back holding the wrong post-quantum half and
	// opens nothing, with the AEAD tag as its only diagnosis -- which is ruling 40's own defect
	// with a restart in front of it. [Group.groupRecordLocked] builds both from one read.
	if err := self.device.persistGroup(self.groupRecordLocked(true)); err != nil {
		return fmt.Errorf(
			"urmessage: this group entered epoch %d and its record could not be persisted, so a restart would come back at the epoch before: %w",
			self.epoch, err)
	}
	return nil
}

// haltLocked records RULING 41's refusal, PERSISTS IT, and answers the sentence the caller returns.
//
// IT IS ONE FUNCTION BECAUSE THERE ARE TWO REFUSAL SITES AND THEY MUST PRODUCE ONE STATE.
// [Group.ingestCommitLocked] refuses at (3a), before ApplyCommit, and at (4b), after it; two
// spellings of "set the field and write the record" is one of them being the site that forgot,
// which is [Group.groupRecordLocked]'s own argument about the four persist sites one paragraph
// over.
//
// THE FIRST REFUSAL WINS, for [Group.wrapDark]'s reason: the state is permanent, the first sentence
// is the one that names what actually happened, and a later walk that re-derived some other
// refusal over the same halted group would overwrite the diagnosis with a symptom.
//
// AND IT IS WRITTEN TO THE DISK HERE RATHER THAN AT [Group.enterEpochLocked]'s step (7), because a
// refusal RETURNS before step (7) is reached -- that is the whole of what a refusal is. Without
// this line the halt died with the process, and the next one came back to a record reading HEALTHY
// over a group standing an epoch behind its own log. The epoch does not move, so this is not an
// epoch change and does not go through that door; what it writes is the diagnosis column beside an
// epoch that is already there.
//
// A STORE THAT WILL NOT TAKE THE RECORD DOES NOT SWALLOW THE REFUSAL. The halt is the sentence the
// caller needs, so it stays the wrapped sentinel -- errors.Is still answers
// [ErrRemovalWithoutRotation] -- and the persist failure is carried inside it, because a halt that
// did not reach the disk is a halt this device forgets at its next start.
func (self *Group) haltLocked(refusal error) error {
	if self.halted == nil {
		self.halted = refusal
		self.haltedEpoch = self.epoch
	}
	if !self.opened {
		return self.halted
	}
	if err := self.device.persistGroup(self.groupRecordLocked(true)); err != nil {
		return fmt.Errorf("%w -- and this refusal could not be persisted, so a restart would come back to a group that reads healthy: %v",
			self.halted, err)
	}
	return self.halted
}

// removedLocked records RULING 52's state, PERSISTS IT, and answers the sentence the caller returns.
//
// IT IS [Group.haltLocked]'s SHAPE AND NOT ITS FIELD, deliberately. The two functions are the same
// three statements -- set the field once, write the record, answer the sentinel -- because a state
// that survives a restart is exactly those three statements and a second spelling of them is the
// site that forgets one. What they must NOT share is the field: ruling 52's whole content is that a
// valid commit that removed this device is a different state from a commit this device refused, and
// one field with two meanings is how the two become one sentence again.
//
// THE CAUSE IS CARRIED AND IT IS mls's OWN. `cause` is the error [mls.Group.ApplyCommit] answered,
// which carries mls.ErrRemovedFromGroup, and it is wrapped rather than replaced so a caller that
// branches on the MLS-level fact keeps its answer. That matters across the restart too, which is why
// [removedErrorOf] re-wraps the same sentinel from the persisted octets: a caller must not get one
// answer before a restart and another after it for a state that did not change.
//
// AND IT IS WRITTEN TO THE DISK HERE, at the same point and for the same reason the halt is: this
// path RETURNS before [Group.enterEpochLocked]'s step (7) is reached, because not entering the epoch
// is the whole of what happened. The epoch does not move, so this is not an epoch change and does
// not go through that door.
//
// A STORE THAT WILL NOT TAKE THE RECORD DOES NOT SWALLOW THE STATE, for the halt's reason: the
// sentinel stays intact so errors.Is answers [ErrRemovedFromGroup], and the persist failure is
// carried inside it, because a removal that did not reach the disk is a removal this device forgets
// at its next start -- and what it forgets it into is a group that looks caught up and silent.
func (self *Group) removedLocked(cause error) error {
	if self.removed == nil {
		self.removed = fmt.Errorf("%w: group %x, at epoch %d: %w",
			ErrRemovedFromGroup, self.id, self.epoch, cause)
		self.removedEpoch = self.epoch
	}
	if !self.opened {
		return self.removed
	}
	if err := self.device.persistGroup(self.groupRecordLocked(true)); err != nil {
		return fmt.Errorf("%w -- and this could not be persisted, so a restart would come back to a group that reads caught up and silent: %v",
			self.removed, err)
	}
	return self.removed
}

// ── sending ──────────────────────────────────────────────────────────────────────────────────

// Send seals one line of text as a DURABLE record and submits it.
//
// WHAT IS SEALED IS `kind(TEXT) ‖ text` AND NOT THE TEXT, which is the 2026-09-17 ruling and is why
// [MaxTextOctets] is one octet short of connect's measured column. See kind.go.
//
// The size bucket is whatever the text needs: [messagegroup.GroupSession.SealRecord] walks the
// ladder and takes the smallest rung the padded body fits, so a message leaks its rung rather than
// its length. THE RUNGS ARE NOT WHAT THEY WERE. Since connect 4c030dc the text is carried inside an
// MLS PrivateMessage that itself sits inside the rung, and the usable PLAINTEXT per rung, measured
// through this method and a real server's rows by
// cp3b.TestEveryRecordTypeUrmessageSealsLandsOnTheRungItsBodyNeeds, is 59 / 826 / 3,898 / 16,186 /
// 65,334 octets where it was 252 / 1,020 / 4,092 / 16,380 / 65,532 -- one of which the kind now
// spends, so the text column is 58 / 825 / 3,897 / 16,185 / 65,333. A text over [MaxTextOctets] --
// including the 198 octets up to the old ceiling -- is refused with [ErrTextTooLong]; blob-backed
// bodies are out of scope.
//
// THE LENGTH REFUSAL IS TAKEN HERE AND COSTS NOTHING, which is a change from every build before
// this one: see [MaxTextOctets] for the 198 octet band that used to spend a durable stream index
// and an MLS generation on its way to the same error, and for why the sealer's own early refusal
// cannot cover it.
//
// AND AN EMPTY TEXT IS REFUSED, which is a PRODUCT change and not a size one: TEXT's body is a tail
// of at least one octet (rule R-d), so an empty line is a message the format has no encoding for
// and [ErrContentMalformed] is the answer. Before the content envelope it sealed a record with an
// empty body, which every receiver rendered as a blank line.
//
// IT NEVER RETURNS NIL ON A MESSAGE THAT DID NOT LAND. The record is accepted by the server, or
// this returns an error naming the refusal -- including after S2-2's single re-Hello and re-MAC.
func (self *Group) Send(ctx context.Context, text string) (*Message, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	role, err := self.sendableLocked(KindText)
	if err != nil {
		return nil, err
	}
	// THE ENCODE IS THE LENGTH REFUSAL AND IT IS BEFORE THE SEAL, one clause further out than the
	// sealer can take it. See [MaxTextOctets]: the sealer's own early refusal is over the CALLER's
	// length against the rung, and the frame that decides the real rung does not exist until an
	// index has been reserved and a generation spent.
	//
	// IT IS AFTER EVERY REFUSAL IN sendableLocked AND THAT ORDER IS DELIBERATE. A group that has
	// seen a second writer, one that has not reconciled, and a caller whose role may not send must
	// each say THAT rather than report a fact about the length of this particular line.
	plaintext, err := encodeText(text)
	if err != nil {
		return nil, err
	}
	return self.sendContentLocked(ctx, plaintext, "a message", role)
}

// SendReply seals one line of text that names the message it answers, and submits it.
//
// replyTo is the parent's [Message.MessageId], raw, 32 octets. THE QUOTED TEXT NEVER TRAVELS: a
// reply carries its parent's NAME and renders by looking the parent up, which is what spec A §7.4's
// ephemeral-containment rule requires of a reply to an ephemeral message and what keeps a reply
// from being a second copy of a line the group already paid for.
//
// A reply is a new message and takes the conversation's own class, so its ceiling is
// [MaxReplyTextOctets] rather than [MaxTextOctets]: the 32 octets of the name come out of the same
// plaintext budget as the text.
func (self *Group) SendReply(ctx context.Context, replyTo []byte, text string) (*Message, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	role, err := self.sendableLocked(KindReply)
	if err != nil {
		return nil, err
	}
	// THE PARENT IS NOT REQUIRED TO BE PRESENT, and that is the one place this differs from
	// [Group.React] and [Group.Delete]. A reply is a message in its own right: it renders whether
	// or not its parent is holdable, and the parent may legitimately be gone -- pruned, expired,
	// or not yet fetched by THIS device while the sender holds it. A reaction and a tombstone
	// have nothing to be but a change to something else, so those two refuse.
	plaintext, err := encodeReply(replyTo, text)
	if err != nil {
		return nil, err
	}
	return self.sendContentLocked(ctx, plaintext, "a reply", role)
}

// React seals one REACTION_ADD naming a message this group holds, and submits it.
//
// The emoji is sealed as the octets the caller passed. WHAT IS AND IS NOT VALIDATED is checkEmoji's
// comment and it is the honest half: valid UTF-8 of 1..[MaxEmojiOctets] octets, and NOT "exactly one
// extended grapheme cluster from the pinned Unicode version", which needs a UAX-29 dependency
// nobody has decided to take (open item M1-41).
//
// IT ANSWERS THE REACTION RECORD'S OWN [Message], WHICH IS NOT A LINE OF THE CONVERSATION. A
// reaction creates no entry: it changes the message it names, which this group applies locally at
// the same moment. The value is returned so that a caller has the record id and the message_id of
// what it just sent -- the two things a later Unreact and any log would need.
func (self *Group) React(ctx context.Context, target []byte, emoji string) (*Message, error) {
	return self.react(ctx, KindReactionAdd, target, emoji)
}

// Unreact seals one REACTION_REMOVE. It cancels an ADD with the same (reactor, target, emoji) --
// see [Reaction] for what "the same" means in a build with no identity system and no grouping key.
func (self *Group) Unreact(ctx context.Context, target []byte, emoji string) (*Message, error) {
	return self.react(ctx, KindReactionRemove, target, emoji)
}

func (self *Group) react(ctx context.Context, kind ContentKind, target []byte, emoji string) (*Message, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	role, err := self.sendableLocked(kind)
	if err != nil {
		return nil, err
	}
	// K9/K5: a reaction may name a stored content message and nothing else, and a call naming an
	// id this device does not hold is a CALL ERROR that emits no record. It is not a courtesy
	// check: a reaction on an id nothing carries is a record every receiver holds for ever
	// waiting for a target that does not exist.
	if _, err := self.reactableLocked(target); err != nil {
		return nil, err
	}
	plaintext, err := encodeReaction(kind, target, emoji)
	if err != nil {
		return nil, err
	}
	return self.sendContentLocked(ctx, plaintext, "a reaction", role)
}

// Delete seals one TOMBSTONE naming a message of THIS DEVICE'S OWN, and submits it.
//
// THE SAME-SENDER RULE IS ENFORCED ON BOTH SIDES AND THIS IS THE SEND SIDE (T-b). A tombstone
// applies only if its sender_handle equals the target's, which is what MASTER §12.1's "a deletion
// cannot be forged" needs beyond R1: R1 proves who wrote the TOMBSTONE and nothing proves they
// wrote the target. So a tombstone naming somebody else's message is a record every honest receiver
// would ignore, and the honest thing is not to seal one.
//
// WHAT IT DOES NOT DO. It does not erase the record on the server -- spec B's B6 is "no
// client-initiated server-side erase in v1" -- and it does not decide what a UI shows: the target
// is marked [Message.Deleted] and keeps its text, because this package refuses to be the layer that
// throws away a user's data on a peer's say-so.
//
// THE 24-HOUR WINDOW IS NOT IMPLEMENTED AND IS NOT FORGOTTEN. MASTER §12.1:2564 bounds a tombstone
// to 24 hours and no document says WHICH CLOCK measures it; every clock reading in a record is its
// sender's claim, and the three candidate clocks are enumerated as owner choice 6 (msgrepo
// docs/reports/2026-09-16-content-kinds.md §5.4 T-d). Building one of them here would be this
// package taking an owner's decision.
func (self *Group) Delete(ctx context.Context, target []byte) (*Message, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	role, err := self.sendableLocked(KindTombstone)
	if err != nil {
		return nil, err
	}
	held, err := self.reactableLocked(target)
	if err != nil {
		return nil, err
	}
	if !held.Mine {
		return nil, fmt.Errorf("%w: message %x was sealed by %x and this device may only delete its own",
			ErrContentMalformed, target, held.SenderHandle)
	}
	plaintext, err := encodeTombstone(target)
	if err != nil {
		return nil, err
	}
	return self.sendContentLocked(ctx, plaintext, "a tombstone", role)
}

// kindsAnObserverMaySend is the exception list ruling 19 leaves behind, AND IT IS EMPTY.
//
// THE EMPTINESS IS THE RULING AND THE LIST IS THE SHAPE. Item 242's ruling 19 refuses an OBSERVER
// all four sendable kinds -- TEXT, REPLY, REACTION_ADD/REMOVE and TOMBSTONE -- because that is the
// entire askable set (COVER has zero call sites, the whole TRANSIENT range including READ_THROUGH
// needs an EPH(0) channel that does not exist, and ATTACHMENT and EDIT have no bodies), and because
// the refusal has to be sayable in one sentence: "You can read this group but not send to it." A
// reaction exception fails that test. So the rule is one clause and one sentence -- and the day a kind IS
// excepted, this map is where it goes and nothing else moves.
//
// WHAT THE RECEIVING SIDE DOES WITH THE ONE THAT GETS THROUGH ANYWAY IS RULING 25's AND IT IS NO
// LONGER "no design". "Hiding a reaction has no design" is true of a ROW -- there is none to
// collapse -- and NOT APPLYING one needs no design at all: see [contentEffect.isObserverReaction].
var kindsAnObserverMaySend = map[ContentKind]bool{}

// sendableLocked is every refusal a send owes BEFORE it looks at what is being sent, and it answers
// THE ROLE THIS DEVICE IS SENDING UNDER. It is one function because four entry points owe the same
// six, in the same order, and a fifth entry point that forgot one would seal under a reused
// identity.
//
// THE ROLE IT ANSWERS IS THE ROLE THE RECORD WILL CARRY, which is why it is a return value and not
// a second read at the seal. The clause below and [Message.SenderRoleAtSend] are then the same
// value from the same call: a build in which the role a send is JUDGED by and the role its message
// ADVERTISES could differ would be two answers to one question, which is the defect ruling 20
// forbids across the two arms of the predicate and there is no reason to allow inside one arm. The
// group's mutex is held from here through the seal, so no commit moves the epoch in between.
//
// THE SIXTH CLAUSE IS R4 (item 242) AND ITS PLACE IN THE ORDER IS LOAD-BEARING. It is AFTER
// identityInUse and AFTER the reconciled check, because a group that has seen a second writer, or a
// restored group that has not yet compared its stream position, must say THAT rather than report a
// fact about the caller's role: the first two are about a key this device is about to reuse and
// this one is about a permission, and a permission refusal on a group that is already unsafe to
// seal in would send a user looking for an admin instead of for their other device.
//
// THE ROLE IS READ THROUGH THE SAME DOOR THE RECEIVING SIDE READS IT THROUGH. Not
// [Group.MyRole]'s road through the live policy, but [messagegroup.GroupSession.RoleAt] at this
// group's own epoch and this device's own leaf -- the one door R4 added -- so that the refusal a
// sender takes and the role every receiver captures are the same function of the same tree. Both
// arms of one predicate must not disagree about one group at one epoch (ruling 20).
//
// AND THE ROLE IS NOT CACHED HERE, deliberately. A send is a network round trip and the ask is a
// map lookup behind the seam; connect already holds the epoch's leaf -> role table as a field of
// the SCHEDULE it belongs to, so it dies at the one moment it must (ruling 18's install), whereas a
// copy kept here would need [Group.enterEpochLocked] to be remembered as its single invalidation
// site for ever. One cache with the right owner beats two caches with one reminder.
func (self *Group) sendableLocked(kind ContentKind) (string, error) {
	if self.closed {
		return "", fmt.Errorf("urmessage: this group is closed")
	}
	if self.session == nil {
		return "", ErrNoMemberAdded
	}
	if !self.opened {
		return "", ErrGroupNotOpen
	}
	// A DEVICE THIS GROUP REMOVED SENDS NOTHING -- RULING 52, and it is the first of the sticky
	// clauses for [Group.commitWalkLocked]'s reason. What it replaces was measured: a removed
	// device's Send answered `urmessage: sealing a message: messagegroup: an application record's
	// inner MLS frame did not open: mls: the group is closed and its epoch secrets have been
	// zeroized` -- a sentence about a data structure, arriving after the seal was attempted, that
	// carries neither mls.ErrRemovedFromGroup nor any sentinel a caller could branch on. The
	// removal is the one refusal a composer has to be able to read by name, because it is the one
	// that is never going to clear.
	if self.removed != nil {
		return "", self.removed
	}
	// BEFORE THE REBIND AND BEFORE THE SEAL, because the seal is the irreversible half: a
	// record sealed under a reused (key, nonce) exists whatever this method then returns.
	if self.identityInUse != nil {
		return "", self.identityInUse
	}
	// AND BEFORE THE SEAL FOR THE SAME REASON, one epoch further on: a group with no pq_secret
	// for the epoch it stands at seals under a storage_root no other member derives, so the
	// server refuses the write_auth and no peer could have read the record anyway. Refused by
	// the name of what actually happened -- [ErrNoWrapForEpoch], [ErrWrapUnreadable] or
	// [ErrOrphanWrap] -- rather than by a reason code the caller would have to decode.
	//
	// AND THE HALT BESIDE IT, WHICH IS RULING 41's OTHER OUTCOME AND IS A DIFFERENT SENTENCE. A
	// halted group did not follow the commit, so it stands at an epoch the server has already
	// left: its write_auth is MAC'd under write_key[n] while the server's current_epoch is n+1,
	// and every send it makes is REASON_REJECTED with nothing to read behind it. Leaving Send
	// open over a halt was leaving the user with exactly the undiagnosable refusal ruling 38
	// exists to prevent, one ruling further along. What it is NOT is a claim that the group is
	// dark: [Group.halted] and [Group.wrapDark] are two fields and errors.Is tells them apart.
	if self.halted != nil {
		return "", self.halted
	}
	if self.wrapDark != nil {
		return "", self.wrapDark
	}
	if !self.reconciled {
		return "", fmt.Errorf("%w: group %x", ErrNotReconciled, self.id)
	}
	// AND THE SEVENTH CLAUSE, BESIDE THE SIXTH AND FOR THE SAME KIND OF REASON. A group JOINED in
	// this process is reconciled by construction and still cannot say that the leaf it landed on
	// is one nobody stood at before: that is [Group.ownFloorHeld], and until one clean walk has
	// held this device's floor against the server's claims a seal here is the collision item
	// 245's first piece exists to prevent -- sticky, for the life of the process. It is ABOVE the
	// role ask for the sixth clause's reason: a group that is not yet safe to seal in must say
	// THAT rather than report a fact about the caller's permissions.
	if !self.ownFloorHeld {
		return "", self.streamFloorRefusalLocked()
	}
	role, err := self.myRoleAtSendLocked()
	if err != nil {
		return "", err
	}
	if role == mls.RoleObserver.String() && !kindsAnObserverMaySend[kind] {
		return "", fmt.Errorf("%w: group %x, %s", ErrObserverMayNotSend, self.id, kind)
	}
	return role, nil
}

// myRoleAtSendLocked is the role this device holds AT THIS GROUP'S CURRENT EPOCH: what
// [Message.SenderRoleAtSend] will say about anything sealed now, read at this device's OWN leaf.
//
// IT CANNOT FAIL BECAUSE THE EPOCH IS STALE, and that is why the send arm does not have the
// receiving arm's [Stats.RoleUndeterminable]: the ask is at self.epoch, where the seam answers off
// the session's own live handle with no load and no window arithmetic. What is left to fail is a
// closed session and a leaf this group's tree does not carry -- a device that is no longer in the
// group it thinks it is in -- and both are refusals a send owes rather than facts to swallow.
func (self *Group) myRoleAtSendLocked() (string, error) {
	_, role, err := self.session.RoleAt(self.epoch, self.handle.OwnLeafIndex())
	if err != nil {
		return "", fmt.Errorf("urmessage: this device could not read its own role in group %x at epoch %d: %w",
			self.id, self.epoch, err)
	}
	return role, nil
}

// reactableLocked answers the message a reaction or a tombstone may name, and refuses one it may
// not. T-a and K9 are the same rule read from two sides: the target must be a STORED CONTENT
// message -- TEXT or REPLY here, ATTACHMENT when the blob plane exists -- and a reaction, a
// tombstone and a COVER are not entries and cannot be named.
//
// THEY CANNOT BE NAMED BY CONSTRUCTION RATHER THAN BY A CLAUSE, which is worth knowing before
// somebody deletes the kind check below: this group's [Group.logIndex] holds only records that
// BECAME a [Message], and a reaction, a tombstone and a COVER become none. The clause is what makes
// the rule survive a later kind that does become a message and still may not be reacted to.
//
// AND A GAP IS REFUSED BEFORE THE KIND IS READ AT ALL, for the same reason [Group.deliverLocked]
// checks it first: a gap's [Message.Kind] is the code the record ARRIVED under, so a malformed REPLY
// body carries [KindReply] and would fall straight through the switch below into "yes, react to
// this" -- a reaction sealed against a record this device could not read, quoting an id whose
// content nobody in the group can agree on. T-a's own words are that a target must be a stored
// CONTENT message, and a gap is by definition the absence of one.
func (self *Group) reactableLocked(target []byte) (*Message, error) {
	if len(target) != MessageIdBytes {
		return nil, fmt.Errorf("%w: a message_id is %d octets and this one is %d",
			ErrContentMalformed, MessageIdBytes, len(target))
	}
	held, found := self.heldLocked(target)
	if !found {
		return nil, fmt.Errorf("%w: %x", ErrNoSuchMessage, target)
	}
	if held.Gap != "" {
		return nil, fmt.Errorf("%w: message %x is a %s gap, which is a record this build could not show",
			ErrNoSuchMessage, target, held.Gap)
	}
	switch held.Kind {
	case KindText, KindReply:
		return held, nil
	}
	return nil, fmt.Errorf("%w: message %x is a %s, which carries no reactions and no tombstone",
		ErrNoSuchMessage, target, held.Kind)
}

// sendContentLocked seals one already-encoded application plaintext as a DURABLE record, submits
// it, and folds what it says back into this group.
//
// IT PARSES WHAT IT IS ABOUT TO SEAL, BEFORE THE SEAL, and that is a gate rather than a
// belt-and-braces: the encoder and the parser are two sides of one grammar, and the day they
// disagree the sender ships a record every receiver refuses as malformed while its own screen shows
// it correctly. Taking the refusal here costs nothing -- no stream index, no MLS generation -- and
// what a sender then displays is built by the SAME code path the receiver's display is.
//
// senderRoleAtSend IS THE ROLE [Group.sendableLocked] JUDGED THIS SEND BY, carried in rather than
// read again. The group's mutex has been held since that clause, so it is the role at the epoch
// this record is about to be sealed at -- which is exactly what [Message.SenderRoleAtSend] means --
// and it is the same value every receiver will capture off this record's own epoch.
func (self *Group) sendContentLocked(ctx context.Context, plaintext []byte, what string,
	senderRoleAtSend string) (*Message, error) {
	entry, verdict, why := ParseContent(plaintext, message.RetentionDurable, 0)
	if verdict != ContentParsed {
		return nil, fmt.Errorf("urmessage: this build would not read back the %s it was about to seal (%s): %w",
			what, verdict, why)
	}
	if err := self.rebindLocked(); err != nil {
		return nil, err
	}
	sentAtMs := self.device.nowMs()
	record, err := self.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(sentAtMs), plaintext, 0, nil)
	if err != nil {
		if errors.Is(err, messagegroup.ErrBodyTooLong) {
			return nil, fmt.Errorf("%w: %d octets: %w", ErrTextTooLong, len(plaintext), err)
		}
		return nil, fmt.Errorf("urmessage: sealing %s: %w", what, err)
	}
	// THE ID IS TAKEN OFF THE RECORD AND BEFORE THE SUBMIT, which is what makes it a name the
	// sender can quote OPTIMISTICALLY: the three inputs are group_handle_key and three fields of
	// the header SealRecord just answered, so nothing about it waits on the server. A reply typed
	// before the submit is acknowledged can already name its parent.
	//
	// IT IS RAISED RATHER THAN LEFT NIL, and it is raised HERE rather than after the submit,
	// because the alternative to both is worse. A Message whose MessageId is nil is a message no
	// later kind can reference and nothing downstream would say so; and an error returned after
	// the submit succeeded would be this method reporting a failure for a record that LANDED,
	// which is the one thing its last paragraph promises it never does. At this point the seal
	// has happened and the submit has not, so this is the same class as the persistSent refusal
	// below: one stream index and one MLS generation spent, both legal gaps, and the send fails.
	messageId, err := self.session.MessageIdOf(&record.Header)
	if err != nil {
		return nil, fmt.Errorf("urmessage: %s was sealed and NOT sent, because its message_id could not be derived: %w", what, err)
	}
	// THE INDEX IS NOTED AT THE SEAL AND NOT AT THE SUBMIT, and the ordering is the whole of
	// why this is here rather than three lines down. A submit whose response never arrived is
	// still a record this device SEALED under this index -- the ciphertext exists and the
	// server may well hold it -- and a device that only recorded acknowledged indices would
	// meet its own lost record on a later fetch and read it as a second writer.
	//
	// AND THE COPY IS KEPT AT THE SAME MOMENT AND FOR THE SAME REASON, and since connect 4c030dc it
	// is the ONLY copy. The body is an MLS PrivateMessage and a member cannot open its own, so a
	// record whose answer was lost comes back on the next fetch as ciphertext this device will
	// never read again -- unless it kept what it sealed.
	//
	// WHAT IS KEPT IS THE PLAINTEXT AND NOT THE TEXT, which is [SentRecord.Body]'s own definition
	// -- "what was sealed, octets, never interpreted" -- and is what lets the own-copy path read a
	// restored device's own reply, reaction and tombstone back through the same codec every other
	// member reads them through.
	//
	// IT IS DURABLE BEFORE THE SUBMIT, OR THE SUBMIT DOES NOT HAPPEN. A record that reached the
	// server with no copy on the disk is a line the user typed that a restart of this device can
	// never show them again, and nothing afterwards could repair it. Refusing here costs one
	// stream index and one MLS generation, both legal gaps, and the user sees the send fail and
	// types it again.
	sealed := &ownSealed{
		bodyHash: record.Header.BodyHash,
		hasCopy:  true,
		body:     append([]byte(nil), plaintext...),
		sentAtMs: sentAtMs,
	}
	self.ownIndices[record.Header.StreamIndex] = sealed
	if err := self.device.persistSent(self.id, record.Header.StreamIndex, sealed); err != nil {
		return nil, fmt.Errorf("urmessage: %s was sealed and NOT sent, because the copy a restart would show it from could not be persisted: %w", what, err)
	}
	recordId, err := self.submitLocked(ctx, self.session, record, what, nil)
	if err != nil {
		return nil, err
	}
	sealed.recordId = recordId
	// NO GAP IS REACHABLE HERE AND IT IS A PRECONDITION RATHER THAN A CHOICE: this method refuses
	// every verdict but [ContentParsed] at its first line, BEFORE the seal, so a record this device
	// sends is by construction one it can read back. A send path that could produce a gap would be a
	// device showing itself a placeholder for a message it had just written.
	// AND THE IDENTITY IS THIS DEVICE'S OWN, TAKEN FROM THE DEVICE AND NOT ASKED OF THE TREE. This
	// record was sealed here, one statement ago, by this device's own leaf key; asking the seam who
	// stands at that leaf would be asking a question whose answer this function already IS, and it
	// would make a line this device just wrote undeterminable at the window edge where the ask can
	// come back short.
	sent := newMessage(entry, recordId, record.Header.SenderHandle[:], self.device.identityPub,
		true, sentAtMs, messageId[:], senderRoleAtSend)
	line := self.deliverLocked(sent, entry)
	// THIS IS A SEND AND NOT A WALK, SO THE REBUILD CANNOT WAIT FOR ONE. A reaction or a tombstone
	// this device has just sealed is one the caller is about to read back off [Group.Messages], and
	// there is no [Group.commitWalkLocked] between here and that read.
	self.rebuildDirtyLocked()
	// AND THE ANSWER IS RE-READ FOR THE SAME REASON [Group.commitWalkLocked] re-reads walk.opened:
	// since ledger item 227 a rebuild REPLACES the message rather than writing through it, so the
	// value built above would be frozen the moment anything standing on it were applied. A message
	// this send added no line for -- a reaction, a tombstone, a COVER -- is not in the log at all
	// and is returned as itself, which is what it has always been: the record's own [Message], not
	// an entry in the conversation.
	//
	// THIS RE-READ DEFENDS NOTHING A TEST CAN SEE TODAY, MEASURED AND NOT ASSUMED: deleting it
	// leaves ./urmessage and ./cp3b green, because NOTHING CAN BE HELD FOR A MESSAGE THIS DEVICE
	// HAS ONLY JUST SEALED. An effect names its target by message_id, message_id is a function of
	// a header this call produced seconds ago, and no member can have named an id that did not
	// exist -- so [Group.effectsOn] for it is empty and [Group.reapplyLocked] returns without
	// replacing anything.
	//
	// IT IS KEPT BECAUSE IT IS THE OTHER END OF THAT ARGUMENT AND NOT BECAUSE IT IS FREE. What
	// makes the branch unreachable is reapplyLocked's empty-effect-set early return, which is a
	// COST decision -- it is what keeps a delivery of a message nobody has reacted to from
	// allocating a copy of it. Delete that early return, for a reason that will look entirely
	// local, and every send starts returning a [Message] this same call replaced in the log: the
	// caller's own line, correct in every field, and a different object from the one the
	// conversation holds. This clause is what keeps that from being a silent change.
	if line {
		if held, found := self.heldLocked(messageId[:]); found {
			sent = held
		}
	}
	self.delivered[recordId] = true
	return sent, nil
}

// submitLocked is 4.3.5's submit of one record, with S2-2's recovery around it.
//
// `delivery` IS RULING 33'S EPOCH KEY PAIR AND IT IS NON NIL FOR EXACTLY THE COMMITS. It is a
// parameter rather than something derived here because the keys are the CALLER's -- they come off
// the epoch the caller staged -- and [alignedEpochKeys] is what holds the parameter against the
// record's own `is_commit`, in both directions, before anything reaches the wire.
//
// IT IS COMPUTED ONCE AND BOTH ATTEMPTS CARRY IT. S2-2's recovery re-MACs the record and submits
// the SAME record a second time; the keys the commit opens its epoch with are a fact of that
// record and not of the connection, so the second request carries the same delivery as the first.
func (self *Group) submitLocked(ctx context.Context, session *messagegroup.GroupSession,
	record *message.Record, what string, delivery *protocol.EpochKeyDelivery) (uint64, error) {

	aligned, err := alignedEpochKeys(record, delivery)
	if err != nil {
		return 0, fmt.Errorf("urmessage: submitting %s: %w", what, err)
	}
	return self.sendSealedLocked(ctx, session, record, what,
		func(projection *protocol.Record) (protocol.Reason, uint64, error) {
			response, err := self.device.transport.Call(ctx, &protocol.SubmitRequest{
				GroupId:   self.id,
				Records:   []*protocol.Record{projection},
				EpochKeys: aligned,
			})
			if err != nil {
				return protocol.Reason_REASON_INTERNAL, 0, err
			}
			if response.GetReason() != protocol.Reason_REASON_OK {
				return response.GetReason(), 0, nil
			}
			body := response.GetSubmit()
			if body == nil || len(body.GetResults()) != 1 {
				return protocol.Reason_REASON_INTERNAL, 0, fmt.Errorf(
					"%w: one record was submitted and %d results came back", ErrSubmitRefused, len(body.GetResults()))
			}
			return body.GetResults()[0].GetReason(), body.GetResults()[0].GetRecordId(), nil
		})
}

// sendSealedLocked submits an already sealed record, and performs S2-2's ONE recovery when the
// server refuses it.
//
// THE RECOVERY IS FOR THE HALF OF A RECONNECT NOTHING IN sdk CAN SEE. NonceEpoch counts Hellos, so
// a connection replaced underneath this binding without a Hello through it leaves a superseded
// nonce readable at an unchanged number and rebindLocked finds nothing to repair. A refusal is
// then the only evidence there is, so one refusal buys one Hello, one rebind, one ReauthRecord --
// which re-MACs the record that is already sealed, consumes no stream index and re-encrypts
// nothing -- and one resubmission.
//
// IT IS ONE AND IT IS NOT A LOOP. A second refusal is a fact about the group, the epoch or the
// record rather than about the nonce, and a client that kept trying would turn a visible failure
// into a busy one. Both reasons are carried in the error.
//
// AND ONE REASON IS NOT A NONCE FACT AND IS NOT TREATED AS ONE: see [Group.cloneRefusalLocked].
func (self *Group) sendSealedLocked(ctx context.Context, session *messagegroup.GroupSession,
	record *message.Record, what string,
	send func(*protocol.Record) (protocol.Reason, uint64, error)) (uint64, error) {

	projection, err := projectionOf(record)
	if err != nil {
		return 0, fmt.Errorf("urmessage: the projection of %s: %w", what, err)
	}
	reason, recordId, err := send(projection)
	if err != nil {
		return 0, fmt.Errorf("urmessage: submitting %s: %w", what, err)
	}
	if reason == protocol.Reason_REASON_OK {
		self.stats.Submitted += 1
		return recordId, nil
	}
	if refusal := self.cloneRefusalLocked(reason, record, what); refusal != nil {
		return 0, refusal
	}
	if lost := epochRaceRefusal(reason, record, what); lost != nil {
		return 0, lost
	}

	// S2-2: one Hello, one rebind, one re-MAC, one resubmission.
	helloReason, hello, err := self.device.transport.Hello(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %s was answered %v, and the Hello that would have repaired the nonce failed: %w",
			ErrSubmitRefused, what, reason, err)
	}
	if helloReason != protocol.Reason_REASON_OK || len(hello.GetServerNonce()) == 0 {
		return 0, fmt.Errorf("%w: %s was answered %v, and the Hello that would have repaired the nonce was answered %v",
			ErrSubmitRefused, what, reason, helloReason)
	}
	if err := self.rebindLocked(); err != nil {
		return 0, err
	}
	if err := session.ReauthRecord(record); err != nil {
		return 0, fmt.Errorf("%w: %s was answered %v and could not be re-MAC'd against the new connection's nonce: %w",
			ErrSubmitRefused, what, reason, err)
	}
	retryProjection, err := projectionOf(record)
	if err != nil {
		return 0, fmt.Errorf("urmessage: the re-MAC'd projection of %s: %w", what, err)
	}
	retryReason, retryRecordId, err := send(retryProjection)
	if err != nil {
		return 0, fmt.Errorf("urmessage: resubmitting %s: %w", what, err)
	}
	if refusal := self.cloneRefusalLocked(retryReason, record, what); refusal != nil {
		return 0, refusal
	}
	if lost := epochRaceRefusal(retryReason, record, what); lost != nil {
		return 0, lost
	}
	if retryReason != protocol.Reason_REASON_OK {
		return 0, fmt.Errorf("%w: %s was answered %v, and %v again after a fresh Hello and a re-MAC",
			ErrSubmitRefused, what, reason, retryReason)
	}
	self.stats.Submitted += 1
	self.stats.Rebound += 1
	return retryRecordId, nil
}

// epochRaceRefusal is the epoch race ON THE SEAL PATH, for a COMMIT record: REASON_COMMIT_LOST and
// REASON_EPOCH_STALE, read as the finding they are rather than pasted into an error string.
//
// WHAT THE TWO REASONS MEAN, READ OUT OF THE SERVER'S SOURCE. The commit-aware epoch gate runs
// under the group's row lock AFTER write_auth verified (spec B §4.5: both "are only ever returned
// after a write_auth verified"): a commit whose epoch already has an accepted commit is answered
// COMMIT_LOST, and a record whose epoch is not the current one is answered EPOCH_STALE (msgrepo
// `store/memory.go` gate, `store/pgx.go` likewise). For a commit either one is the same sentence --
// THE EPOCH THIS COMMIT CLOSES HAS ALREADY BEEN CLOSED BY SOMEBODY ELSE -- which is MASTER §9.3's
// delivery service doing its one job, and the re-derivation §9.3 asks of the loser is
// [Group.Receive] and the verb again.
//
// IT IS TAKEN BEFORE S2-2'S RECOVERY AND NOT AFTER, for [Group.cloneRefusalLocked]'s reason: the
// recovery repairs a NONCE, and a reason answered only after write_auth verified is not a nonce
// fact, so a Hello, a rebind and a re-MAC of the same record cannot change the answer -- measured,
// before this arm, as "REASON_COMMIT_LOST, and REASON_COMMIT_LOST again after a fresh Hello and a
// re-MAC" on every lost race. It is taken at both sites the clone check is, because a first
// refusal that IS a nonce fact can be followed by a retry that meets the race.
//
// FOR A COMMIT, AND FOR THE WRAPS THAT TRAVEL WITH ONE. An application record answered EPOCH_STALE
// is a device that has not fetched since the group moved, and what it owes is the same Receive --
// but that record is a legal gap and nothing of it is staged, so the plain [ErrSubmitRefused] it
// has always been answered stands. [Group.publishCommitLocked] is the one caller that acts on
// this: it erases the staged epoch and answers [ErrCommitLost], which wraps [ErrSubmitRefused] so
// the old reading still holds.
//
// THE WRAP ARM IS LEDGER ITEM 251's RULING 37 ARRIVING HERE, and it is a correction rather than a
// widening: under that ruling the fan-out is submitted BEFORE the commit, at epoch n, so a
// committer that has fallen behind now meets the race at its FIRST WRAP and never reaches the
// commit record at all. The sentence is identical -- the epoch this device is building against has
// already been closed by somebody else -- and answering it as a plain refusal would make a lost
// race report itself differently depending on which record of the same publication happened to be
// first on the wire. MEASURED as exactly that: cp3b's lost-race case, whose whole subject is that
// the honest committer is left where it was and retries, went from ErrCommitLost to "an epoch wrap
// was answered REASON_EPOCH_STALE" on the commit that moved the fan-out.
//
// A WRAP IS ONLY EVER SUBMITTED AS PART OF OPENING AN EPOCH, which is what makes the arm exact
// rather than a guess: the two sites are [Group.Open]'s founding fan-out and
// [Group.publishCommitLocked]'s, and in both a stale epoch means the group moved under this device
// while it was publishing one.
func epochRaceRefusal(reason protocol.Reason, record *message.Record, what string) error {
	if !record.Header.IsCommit && !isEpochWrapRecord(record) {
		return nil
	}
	if reason != protocol.Reason_REASON_COMMIT_LOST && reason != protocol.Reason_REASON_EPOCH_STALE {
		return nil
	}
	return fmt.Errorf("%w: %w: %s at epoch %d was answered %v",
		ErrCommitLost, ErrSubmitRefused, what, record.Header.Epoch, reason)
}

// isEpochWrapRecord is whether a record is one of an epoch fan-out's device wraps.
//
// IT READS THE OCTETS THAT WERE SEALED and not a flag beside them: the attachment is inside
// AAD_head and inside the write_auth preimage, so this is the same answer the server computed when
// it refused the record. A parse failure answers false, which is the safe direction -- a record
// whose attachment this build cannot read is not one this build may reclassify as a lost race.
func isEpochWrapRecord(record *message.Record) bool {
	if len(record.Header.ServerAttachment) == 0 {
		return false
	}
	attachment, err := message.ParseServerAttachment(record.Header.ServerAttachment)
	if err != nil {
		return false
	}
	return attachment.Kind == message.AttachmentWrap
}

// cloneRefusalLocked is the clone check ON THE SEAL PATH: §4.5's REASON_STREAM_INDEX_REUSED, read
// as the finding it is rather than pasted into an error string.
//
// WHY IT HAD TO EXIST. Clause 2 of the clone check (see [Device.Restore]) lives only in
// [Group.Receive], and [Group.Send] consulted nothing but `identityInUse` and `reconciled`. So two
// level copies that kept SENDING collided on every index, not once: the refusal was returned
// non-sticky, and the next Send sealed at the next index and collided there too. The published
// bound -- "one record, not a stream of them" -- was FALSE by measurement, at four collisions from
// four typed messages, every one of them a two-time pad the ct_body XOR shows octet for octet.
//
// WHAT THE REASON MEANS, READ OUT OF THE SERVER'S SOURCE RATHER THAN ASSUMED. Both stores answer it
// from the same place: step (0)'s idempotency probe, BEFORE any gate, any allocation and the row
// lock, compares the submitted record's body_hash AND the hash of its ct_head against the
// `message_stream_claim` already standing at this (group_id, sender_handle, stream_index).
// Equal on both is `probeIdentical` and REASON_OK; different is `probeDiffers` and
// REASON_STREAM_INDEX_REUSED (msgrepo `store/memory.go:452`, `store/pgx.go:994`). So the reason is
// exactly one sentence: SOMETHING ELSE HAS ALREADY WRITTEN DIFFERENT CONTENT AT AN INDEX THIS
// DEVICE'S RESERVER HANDED OUT. A reserver never rewinds and this device seals once per index, so
// there is no second reading of that, and it is the same finding [ErrIdentityInUse] names.
//
// AND THE HONEST RETRY IS PRICED, WHICH IS THE THING THAT HAD TO BE CHECKED FIRST. The one way a
// healthy device resubmits at a consumed index is S2-2's recovery and a lost answer, and
// [messagegroup.GroupSession.ReauthRecord] writes EXACTLY ONE field -- `record.WriteAuth` -- which
// the probe does not read. So an honest resubmission is byte-identical where the probe looks and is
// answered REASON_OK, never REUSED. That is measured rather than reasoned:
// `cp3b.TestAnHonestResubmissionOfTheSameRecordIsAnsweredOkAndNotReadAsAClone`.
//
// IT IS TAKEN BEFORE S2-2'S RECOVERY AND NOT AFTER, and that ordering is the point. The recovery
// repairs a NONCE, and a reused index is not a nonce fact -- so running it here would buy nothing
// and would put the colliding ciphertext on the wire a SECOND time, which is exactly what was
// measured: "3 submissions, 2 distinct ciphertexts" at every collided index.
//
// THE COST, SAID PLAINLY. The reason is PLAINTEXT and unauthenticated, so a hostile or broken
// server can answer REUSED to a device that has no copy and stop that group sealing for the life of
// the process. That is accepted, for two reasons that are worth more than the risk: a server can
// already deny every submit outright, so this buys it only stickiness; and the failure direction is
// "this device will not send", never "this device sends under a reused key and nonce". A restart
// clears it and the next [Group.Receive] decides again on records that OPENED, which is evidence a
// server cannot forge.
func (self *Group) cloneRefusalLocked(reason protocol.Reason, record *message.Record, what string) error {
	if reason != protocol.Reason_REASON_STREAM_INDEX_REUSED {
		return nil
	}
	if self.identityInUse == nil {
		self.identityInUse = fmt.Errorf(
			"%w: group %x epoch %d: the server answered %v to %s at stream index %d, which is its statement that a record it already holds at that index under this device's own sender_handle carries different content -- two records under one (epoch, sender_handle, stream_index) are one record_key and one nonce",
			ErrIdentityInUse, self.id, self.epoch, reason, what, record.Header.StreamIndex)
	}
	return self.identityInUse
}

// ── receiving ────────────────────────────────────────────────────────────────────────────────

// Receive fetches everything the server has for this group since the last call, opens what is a
// message, and answers the messages in record order.
//
// IT PAGES UNTIL THE SERVER SAYS complete, AND WHEN IT CANNOT IT SAYS SO. 4.3.4's FetchResponse
// carries `complete` -- "false when truncated by limit OR by max_response_bytes; both are NORMAL"
// -- `next_record_id` and `high_water_record_id`, and an earlier build of this method read none of
// the three. One page then read as the whole history with a nil error, which is the worst failure
// an alpha can have, because a user cannot tell half a conversation from a quiet one. So:
//
//   - a truncated page is followed by the next one, resuming from `next_record_id`, until the
//     server answers complete;
//   - a server that answers incomplete and advances NO cursor is refused with
//     [ErrFetchNoProgress] rather than looped on forever;
//   - and reaching [maxFetchPages] returns the messages read so far TOGETHER WITH
//     [ErrFetchIncomplete], so a caller that ignores the error still sees messages and a caller
//     that reads it knows there are more.
//
// WHAT IT SKIPS IS COUNTED RATHER THAN DROPPED. 6.1's ceremony, records this group's log already
// holds, and classes this build does not open are each their own counter on [Group.Stats]; a
// record from a member that DID NOT OPEN is counted too AND returns an error, because that is the
// one case where a message was sent and this device cannot show it.
//
// THIS DEVICE'S OWN RECORDS ARE SHOWN AND NOT SKIPPED, and the sentence that stood here before
// that said the opposite. Every own record was skipped on the ground that this device "holds its
// own plaintext already" -- which is true of a device that has been running since it sent them, and
// FALSE of a restored one, whose log starts empty and whose cursor is not persisted. A user closed
// the app, reopened it, and got the other side's half of the conversation and none of their own,
// with a nil error and one counter that moves on the ordinary echo case too. Now an own record is
// shown unless its record id is already in this group's log, and the readings are counters:
// [Stats.SkippedOwn], [Stats.OpenedOwn] and [Stats.OwnWithoutCopy].
//
// SHOWN, AND SINCE connect 4c030dc NOT OPENED: a member cannot open its own application record
// (MG-4), so this device's own lines come from the copy [Group.Send] kept and the durable store
// holds. [Group.openPageLocked] carries the three roads an own record takes.
//
// A RECORD THAT DID NOT OPEN IS ASKED FOR AGAIN, up to [maxRecordAttempts] times, and then GIVEN
// UP ON BY NAME. The cursor this method resumes from is the RESOLVED position and not the paging
// one -- see [pageWalk] -- because an earlier build advanced one number over every row before the
// fail paths, so one transient cost the conversation that message for ever and the retry answered
// nothing with a nil error. At the bound the record is [ErrRecordAbandoned], [Stats.Unopened] and
// [Group.UnopenedRecords], which is a hole a caller can show.
//
// A SERVER HOLDING RECORDS BACK IS CAUGHT BY ITS OWN `high_water_record_id`, WITH NO KEY. See the
// complete-page branch in the body for the check, and for the one honest server that also trips
// it. This is the half of S2-27 below that is not blocked on key custody.
//
// AND A SECOND DEVICE SEALING UNDER THIS DEVICE'S IDENTITY -- a copied app-data folder -- IS
// REFUSED HERE, before this group seals again. [Device.Restore] carries the whole of that
// decision: what it covers, what it does not, and why the server's refusal of the duplicate is
// not a defence.
//
// ---------------------------------------------------------------------------------------------
// 4.3.4'S FETCH ATTESTATION: WHAT IS CHECKED, WHAT IS NOT, AND WHY NOT.
// ---------------------------------------------------------------------------------------------
//
// The attestation is the server's Ed25519 signature over what it returned -- since, until, the
// record ids, the high water, its own time and its own id. The AEAD catches a server that TAMPERS;
// nothing but this catches a server that OMITS, so a page with records missing from it is
// invisible to a client that ignores it.
//
// TWO OF THE THREE CHECKS ARE MADE HERE AND THEY NEED NO KEY.
//
//  1. THE DOWNGRADE. If the server ADVERTISED `capabilities.attestation_supported` and then
//     answered a page with no attestation, that is refused with [ErrFetchAttestation]. A server
//     that can sign and did not is not the same server as one that never could.
//  2. THE DESCRIPTION. If an attestation IS present, its group_id, its since, its record id
//     vector and its high water are compared against the page it arrived with. An attestation
//     that describes a DIFFERENT page -- one replayed from another fetch, or one that lists
//     records this page does not carry -- is refused. This is what turns the value from
//     decoration into a statement about these records.
//
// THE THIRD -- THE SIGNATURE ITSELF -- IS NOT VERIFIED, AND THAT IS STATED RATHER THAN PAPERED
// OVER. Verifying it needs the fleet's public key, and 4.3.1 says where that comes from:
// `HelloResponse.server_keys`, each certified by a FLEET ROOT key the client holds compiled in
// (Spec A section 7.6). MEASURED against the server this alpha is deployed from, at msgrepo's
// committed HEAD:
//
//	msgrepo/peer/peer.go:392  -- "HelloResponse.server_keys and HelloResponse.kt_gossip ... This
//	                             process holds no fleet key and observes no log" (declared NotBuilt)
//	msgrepo/api/api.go:497    -- "FetchAttestation: an Ed25519 signature by the fleet key over
//	                             nine response fields, and this process holds no fleet key"
//	msgrepo/api/fetch.go:112  -- "4.3.4's FetchAttestation is absent, not empty"
//
// So the deployed server signs nothing, advertises `attestation_supported` false, and publishes no
// key chain; and there is no compiled-in fleet root anywhere in this workspace to chain one to.
// VERIFYING AGAINST A KEY THE SERVER ITSELF HANDED OVER WOULD BE WORSE THAN NOT VERIFYING: it
// would read as verified and would authenticate the server to itself. So it is not done, it is
// COUNTED -- [Stats.Unattested] moves once per page whose signature this build could not verify,
// which today is every page -- and the gap is filed. **S2-27: a client cannot verify a fetch
// attestation until the fleet ships a key chain and this build ships a root to verify it against.
// Until it does, a message server that omits records from the MIDDLE of a page, or that lies about
// its own high water, is undetectable by this client.** (It used to say "omits records from a
// page", flat, and that was too strong; the paragraph below is the correction and the measurement.) It is not this package's to close: the key custody is Spec B section 9.1's, through
// `kt`, which is the owner msgrepo's own NotBuilt entry names.
//
// S2-27 IS NARROWED AND NOT CLOSED, AND THE NARROWING IS THE HALF THAT NEEDED NO KEY. The sentence
// above -- "a message server that silently omits records from a page is undetectable by this
// client" -- was too strong. `high_water_record_id` is the server's own statement of the highest
// record it holds for this group, it arrives on every page, and it needs no signature to read. A
// complete page that names a high water above everything it handed over is now counted in
// [Stats.Omitted] and returned as [ErrFetchOmitted]. What remains S2-27's, and genuinely does need
// the key: a server omitting records from the MIDDLE of a page, or one that lies about its own
// high water. Both are caught by a signature over the record id vector and by nothing else.
func (self *Group) Receive(ctx context.Context) ([]*Message, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return nil, fmt.Errorf("urmessage: this group is closed")
	}
	if self.session == nil {
		return nil, ErrNoMemberAdded
	}
	if err := self.rebindLocked(); err != nil {
		return nil, err
	}
	nonce, _, err := self.device.nonce()
	if err != nil {
		return nil, err
	}
	// THE READ KEY IS RE-DERIVED WHEN THE EPOCH MOVES UNDER THIS WALK, and it used to be derived
	// once. A5's ingest can advance [Group.epoch] in the MIDDLE of a walk -- an is_commit record on
	// one page opens an epoch whose messages arrive on the next -- and §4.3.4's read authenticator
	// is a mac under the read key of the epoch the fetch NAMES. A page requested with ReadEpoch set
	// to the new epoch but MAC'd under the old epoch's read key is refused by the server, so the
	// read key follows the epoch. It is a closure over a captured epoch rather than a re-derivation
	// per page, so a walk that does not cross an epoch pays exactly the one derivation it did before.
	var epochKeys *messagegroup.EpochKeys
	var readKey []byte
	readKeyEpoch := ^uint64(0)
	defer func() {
		if epochKeys != nil {
			epochKeys.Destroy()
		}
	}()
	refreshReadKey := func() error {
		if epochKeys != nil && readKeyEpoch == self.epoch {
			return nil
		}
		next, err := self.session.EpochKeys()
		if err != nil {
			return fmt.Errorf("urmessage: this epoch's keys: %w", err)
		}
		key, err := next.ReadKey()
		if err != nil {
			next.Destroy()
			return err
		}
		if epochKeys != nil {
			epochKeys.Destroy()
		}
		epochKeys = next
		readKey = key
		readKeyEpoch = self.epoch
		return nil
	}
	own, err := self.session.SenderHandle()
	if err != nil {
		return nil, fmt.Errorf("urmessage: this device's sender handle: %w", err)
	}
	// AND THE SESSION'S ANSWER IS FILED IN THE SET BEFORE THE WALK TAKES IT. The session derives
	// the handle from the leaf this device stands at right now, so this is the one line that keeps
	// [Group.ownHandles] level with a leaf that moved without an epoch install this process saw --
	// a restore onto a re-Add, for instance. It only ever adds.
	self.ownHandles[own] = true

	walk := &pageWalk{
		// THE GROUP'S OWN MAP AND NOT A SNAPSHOT OF IT, which is the one field of this struct
		// that is deliberately shared. Every other field here is "as it stood when this walk
		// STARTED" because a walk must not be re-decided under itself; this set only ever GROWS,
		// and every entry added to it is a handle this device really holds, so sharing it can
		// only ever make a record this device wrote resolve as its own sooner.
		own:          self.ownHandles,
		ownNow:       own,
		leaves:       map[uint64]map[[16]byte]uint32{},
		opened:       []*Message{},
		from:         self.cursor,
		reached:      self.cursor,
		resolvedTo:   self.cursor,
		reconciled:   self.reconciled,
		unobtainable: map[uint64]bool{},
	}
	// THE PAGE THIS WALK ASKS FOR, WHICH IS NOT A CONSTANT. It starts at the server's own
	// advertised bound -- §4.3.1 says a request for more, or for nothing in particular, gets that
	// bound anyway -- and comes down only when a page is refused for its SIZE. See the
	// REASON_OVERSIZE arm below for why a fixed number is the wrong answer in both directions.
	pageLimit := uint32(defaultFetchLimit)
	for page := 0; ; page += 1 {
		if maxFetchPages <= page {
			// walk.resolvedTo AND NOT self.cursor, because this sentence is now built BEFORE
			// the commit rather than after it. They are ONE number -- commitWalkLocked's first
			// statement is `self.cursor = walk.resolvedTo` -- and the walk's copy is the one
			// that reads the position the next [Group.Receive] will resume from.
			return walk.opened, self.commitWalkLocked(walk, fmt.Errorf(
				"%w: %d pages, cursor at record %d", ErrFetchIncomplete, page, walk.resolvedTo))
		}
		if err := refreshReadKey(); err != nil {
			return walk.opened, self.commitWalkLocked(walk, err)
		}
		since := walk.from
		request := &protocol.FetchRequest{
			GroupId:       self.id,
			SinceRecordId: since,
			ReadEpoch:     self.epoch,
			Limit:         pageLimit,
		}
		if err := authorizeFetch(request, readKey, nonce); err != nil {
			return walk.opened, self.commitWalkLocked(walk, err)
		}
		response, err := self.device.transport.Call(ctx, request)
		if err != nil {
			return walk.opened, self.commitWalkLocked(walk, fmt.Errorf("urmessage: Fetch: %w", err))
		}
		if response.GetReason() == protocol.Reason_REASON_OVERSIZE && 1 < pageLimit {
			// §4.3.1's TWO BOUNDS DO NOT AGREE, AND THE CLIENT IS THE PARTY THAT CAN SAY SO.
			// `max_records_per_fetch` is a COUNT (512 by default) and `max_response_bytes` is a
			// SIZE (1 MiB), and nothing relates them: a page of 512 records from the 4 KiB rung
			// is 2.2 MiB, which the transport replaces wholesale with this refusal. It was
			// unreachable in practice while the only large records were a user's own long
			// messages; ledger item 251's device wrap makes it ORDINARY, because a rotating
			// fan-out writes one 4 KiB PERMANENT record per member per epoch and a catch-up walk
			// meets them in runs.
			//
			// HALVING AND RETRYING THE SAME `since` IS THE WHOLE REPAIR, and it is a fact about
			// the transport rather than about the group: nothing was served, nothing was opened,
			// the cursor has not moved, and §4.3.1 lets a client ask for fewer records than the
			// advertised bound. A CONSTANT would have been the wrong shape -- the safe constant
			// for the 64 KiB rung is sixteen records, which is a catch-up thirty times slower
			// than it needs to be for every group that never writes one.
			//
			// IT DOES NOT COUNT AS A PAGE and it cannot loop: the limit strictly decreases and
			// stops at one, and a single record the transport will not carry is a refusal this
			// client cannot repair and answers by name.
			pageLimit = pageLimit / 2
			page -= 1
			continue
		}
		if response.GetReason() != protocol.Reason_REASON_OK {
			// THE ARM A DARK GROUP TAKES ON EVERY FETCH AFTER THE FIRST, which is why the
			// answer has to come out of commitWalkLocked and not out of this line. A group with
			// the wrong pq_secret has the wrong read_key, and the server verifies req_auth
			// before it reaches any AEAD -- so REASON_REJECTED here is the SYMPTOM of
			// [Group.wrapDark] and this used to be the sentence that replaced it.
			return walk.opened, self.commitWalkLocked(walk,
				fmt.Errorf("%w: %v", ErrFetchRefused, response.GetReason()))
		}
		fetched := response.GetFetch()
		if fetched == nil {
			return walk.opened, self.commitWalkLocked(walk,
				fmt.Errorf("%w: the response carried no fetch arm", ErrFetchRefused))
		}
		self.stats.Pages += 1
		if err := self.checkAttestationLocked(since, request.GetReadEpoch(), fetched); err != nil {
			return walk.opened, self.commitWalkLocked(walk, err)
		}
		self.openPageLocked(fetched, walk)
		if fetched.GetComplete() {
			// 4.3.4'S HIGH WATER, AND IT COSTS NOTHING. `high_water_record_id` is the
			// server's own statement of the highest record it holds for this group. On a
			// page it calls COMPLETE, a high water above everything it handed over is the
			// server saying it kept records back -- which is the one failure the AEAD
			// cannot see, and the half of it that needs no key, no fleet root and no
			// attestation. Before this clause the field was read in exactly one place,
			// inside checkAttestationLocked, which returns at its first branch when there
			// is no attestation -- which on the deployed server is every page.
			//
			// IT IS HELD AGAINST `reached` AND NOT AGAINST THE CURSOR. A record this device
			// could not open is still a record the server DID hand over, and naming the
			// server for this device's failure would be a true-sounding sentence about the
			// wrong party.
			//
			// THE ONE HONEST SERVER THAT WOULD ALSO MOVE IT, AND IT IS NOT BUILT YET.
			// high_water_record_id is `next_record_id - 1` off a monotone allocator on the
			// group row (msgrepo/store/pgx.go:501, store/memory.go:236), so it does NOT
			// come down when rows go. 7.2's retention sweep is what would take rows out
			// from under it, and a group whose oldest records had been pruned would answer
			// a complete page that stops short of its own high water with no dishonesty
			// anywhere.
			//
			// MEASURED rather than assumed, over msgrepo at ca8662d, because "there is a
			// legitimate cause" is the sentence that would quietly excuse every future
			// failure of this check:
			//
			//	grep -rn "DELETE FROM" --include=*.go --include=*.sql .
			//
			// answers TWO lines, both `DELETE FROM migration_audit` in a startup test.
			// NOTHING DELETES A message_record ROW. The `prune_after` column is written
			// and the sweep worklist index exists (store/migrations.go:155, :233) and the
			// sweep itself is NOT BUILT. So today this check has no known false positive
			// on the deployed server, and it acquires one the day 7.2 lands.
			//
			// It is a COUNTER and a returned error rather than a refusal anyway, and that
			// is the right way round for the same reason: the day the sweep lands, a
			// client that REFUSED the page would refuse an honest server doing its own
			// retention, and it would do it in a release nobody connected to this line.
			if walk.reached < fetched.GetHighWaterRecordId() {
				self.stats.Omitted += 1
				if walk.omitted == nil {
					walk.omitted = fmt.Errorf(
						"%w: group %x: it names high_water %d and handed over nothing above record %d",
						ErrFetchOmitted, self.id, fetched.GetHighWaterRecordId(), walk.reached)
				}
			}
			walk.complete = true
			break
		}
		// 4.3.4's resume cursor. It is taken as a MAXIMUM against what the rows moved the
		// paging position to rather than as an assignment: a server that answered a
		// next_record_id BEHIND the records it just sent would otherwise walk this client
		// backwards over records it has already opened, forever.
		if walk.from < fetched.GetNextRecordId() {
			walk.from = fetched.GetNextRecordId()
		}
		if walk.from <= since {
			return walk.opened, self.commitWalkLocked(walk,
				fmt.Errorf("%w: %d records, next_record_id %d, position still %d",
					ErrFetchNoProgress, len(fetched.GetRecords()), fetched.GetNextRecordId(), walk.from))
		}
	}
	return walk.opened, self.commitWalkLocked(walk, nil)
}

// How many times one record is fetched and allowed to fail to open before this group gives up on
// it and says so.
//
// IT IS A BOUND ON A RETRY THAT DID NOT EXIST AT ALL. The cursor used to move past a record on the
// first sight of it, BEFORE the fail paths, so a transient -- a ciphertext bent in flight, a
// truncated body, a page a middlebox chewed -- cost the conversation that message for ever, and
// the next call answered no messages and a nil error while the record sat on the server.
//
// IT IS SMALL BECAUSE THE FAILURES A RETRY CAN REPAIR ARE TRANSIENT BY DEFINITION: a record that
// will not open three times will not open on the thousandth, and an unbounded retry is a group
// that re-reads its whole tail on every fetch for ever -- one bent record turned into a permanent
// cost, which is a shape an unfriendly peer would reach for.
//
// WHAT HAPPENS AT THE BOUND IS THE POINT: the record is named with [ErrRecordAbandoned], counted
// in [Stats.Unopened] and listed by [Group.UnopenedRecords]. A hole in a conversation this build
// has stopped trying to fill is a thing a user can be told about.
const maxRecordAttempts = 3

// pageWalk is one [Group.Receive]'s state across the pages it reads.
//
// IT IS A TYPE BECAUSE THE PAGING POSITION AND THE RESOLVED POSITION USED TO BE ONE NUMBER, and
// that is the whole of how a record that did not open was dropped for ever: `self.cursor` advanced
// over every row before the fail paths, so the next fetch asked from ABOVE the record that failed
// and no later call ever asked for it again. Two numbers cannot be confused for one another by an
// edit; one number could only be right for one of the two jobs.
type pageWalk struct {
	// own is EVERY sender_handle this device has held in this group -- [Group.ownHandles], shared
	// rather than copied. It decides which records the two graceful own-record roads are TRIED
	// for, and it decides nothing else: what a record IS, is [Group.recordIsOwnLocked]'s answer
	// after the open.
	own map[[16]byte]bool

	// ownNow is the handle this device SEALS under right now, which is the one row of the durable
	// reserver this walk can seed. It is a single value and not the set above because a floor is
	// a fact about the stream the NEXT record goes into, and there is exactly one of those.
	ownNow [16]byte

	// THE HIGHEST CLAIMED INDEX IS NOT HERE ANY MORE, and it left for the reason the highest OWN
	// index left the line below: one walk's number forgets what an earlier walk read off a record
	// it later gave up on. It is [Group.ownClaimSeen], keyed by the handle it was seen under, and
	// [Group.seedOwnStreamLocked] is where reading it off a PLAINTEXT HEADER is argued and bounded.

	// leaves is the handle table PER RECORD EPOCH, built on demand by [Group.walkLeavesLocked] and
	// held for the rest of this walk. It used to be ONE table at the current epoch, which is
	// ledger item 245's third defect; see [Group.leavesAtLocked].
	leaves map[uint64]map[[16]byte]uint32

	opened       []*Message
	firstFailure error
	omitted      error

	// from is where the NEXT page is asked from. It moves over every row, always, so that one
	// record that will not open cannot loop this call.
	from uint64

	// reached is the highest record id the server has handed over in this call, and it is what
	// 4.3.4's high_water_record_id is held against.
	reached uint64

	// resolvedTo is the highest record id below which every row has been opened, skipped by a
	// name this build prints, or given up on. It becomes this group's cursor, so a row that did
	// not open is asked for again by the NEXT Receive.
	resolvedTo uint64
	blocked    bool

	complete bool

	// reconciled is [Group.reconciled] as it stood when this walk STARTED. A walk that is doing
	// the reconciling absorbs the own records it finds; one that is not treats an own record
	// this device never sealed as what it is.
	reconciled bool

	// THE HIGHEST OWN INDEX IS NOT HERE ANY MORE: it is [Group.ownIndexSeen], because one walk's
	// number forgot what an earlier, dirty walk had already authenticated. It is still read OFF
	// RECORDS THE GROUP'S KEYS AUTHENTICATED AND NEVER OFF A HEADER -- a record header is plaintext,
	// the server writes any sender_handle and stream_index it likes into one, and a number taken
	// off a header would let it wedge any client with one forged row. The three sources it IS
	// taken from: an own record that OPENED, which since MG-4 only a copy of this folder ahead of
	// this one can have sealed; an own record whose inner frame reached MLS's spent-generation
	// refusal (see ownFrameAlreadySpent); and an own record shown from this device's copy, whose
	// index is by construction one this device sealed at (see [Group.openOwnFromCopyLocked]).

	// the first own record that opened at a stream index this device did not seal THIS record
	// at. foreignBody distinguishes the two ways that happens, because they are two different
	// sentences to show a user.
	foreignIndex  uint64
	foreignRecord uint64
	foreignBody   bool

	// unobtainable is every prior epoch this walk asked the session for and was told this device
	// holds no state at: a member admitted later, draining the records from before its admission.
	// The first record of such an epoch costs one store read and the rest cost nothing, and the
	// memo is the WALK's rather than the group's because the store's answer is a fact about the
	// disk right now and not about the group.
	unobtainable map[uint64]bool
}

// commitWalkLocked folds one walk back into the group and answers what its caller must be told.
//
// THE CURSOR BECOMES THE RESOLVED POSITION AND NOT THE PAGING ONE. That is the repair: a record
// that did not open holds this back, so the next [Group.Receive] asks the server for it again.
//
// `fetchErr` IS THE TRANSPORT'S OWN REFUSAL AND IT IS A PARAMETER BECAUSE A RETURN VALUE WAS
// DISCARDABLE. Five arms of [Group.Receive] used to call this function as a statement and return
// their own transport error, so everything decided below was thrown away on every one of them.
// MEASURED against a real server: a group that went dark at epoch 2 answered the sentinel on its
// FIRST Receive -- the one that ingests the commit -- and `REASON_REJECTED` on the second and
// every later one, because a dark group's read_key is wrong, the server verifies req_auth before
// it reaches any AEAD, and that refusal arm is therefore the GUARANTEED arm for a dark group
// rather than an incidental one. The diagnosis ruling 38 exists to keep could never reach a
// caller again. Passing the error IN makes the decision one place the compiler will not let an
// arm skip; a sixth arm added tomorrow has to say what its error is, and
// `TestEveryErrorReceiveAnswersComesOutOfTheWalksOwnCommit` refuses one that answers around it.
//
// THE ORDER THE SIX ERRORS ARE RETURNED IN IS A DECISION, and the three ahead of `fetchErr` are
// ahead of it for the same reason they are ahead of each other: they are STICKY refusals about
// THIS DEVICE'S OWN PERMANENT STATE, and the transport refusal is a symptom of them.
//
//  0. THE REMOVAL, ruling 52, which is FIRST and is the only one of the six that is not about how
//     this device is doing in a group it is in: it says it is not in the group. Every other answer
//     below is downstream of that -- an identity refusal is about a stream this device will never
//     seal in again, a dark wrap is about an epoch it is not in, and a transport refusal is what
//     the server says to a member it no longer has. And nothing is lost by saying it first: the
//     MLS group is closed, so a removed device cannot seal at all and cannot produce the collision
//     the identity refusal exists to prevent.
//  1. the identity refusal, because it is the only one that stops this device sealing and because
//     carrying on would carry on producing the collision;
//  2. the wrap that never arrived, ruling 38's diagnosis, which is the CAUSE of the fetch refusal
//     below rather than a competitor with it, with ruling 41's halt ahead of it;
//  3. the transport's own refusal, which is what stopped THIS walk and is the more proximate
//     answer for every group that is not in one of the states above;
//  4. the record that did not open, which is the existing contract and names a specific record;
//  5. the server that held records back, which moves [Stats.Omitted] whether or not it is
//     returned.
//
// WHAT IT COSTS, NAMED RATHER THAN LEFT TO BE FOUND: a caller of a group in one of the two sticky
// states can no longer read a cancelled context or a transport outage off the error [Group.Receive]
// answers -- errors.Is(err, context.Canceled) is false there. That is the intended direction. A
// group whose identity is in use or whose epoch has no secret does not become well by being
// retried, and a caller that reads the refusal as transport is a caller that retries for ever.
func (self *Group) commitWalkLocked(walk *pageWalk, fetchErr error) error {
	self.cursor = walk.resolvedTo
	// THE HEADS THIS WALK AUTHENTICATED GO TO THE DISK HERE, once per walk and only when one rose.
	// The write's failure is held and answered LAST, below the walk's own three, because it is the
	// weakest of the four: it costs a future restart the window, and nothing in this process.
	headsErr := self.persistPeerHeadsLocked()
	// AND THE EFFECTS THIS WALK NOTED ARE REBUILT HERE, ONCE PER TARGET. Every [Group.Receive] exit
	// runs through this function, including the ones that return an error, so a walk that ended
	// badly still leaves the targets it touched consistent with the effects it recorded.
	self.rebuildDirtyLocked()
	// AND THEN THE WALK'S OWN ANSWER IS RE-READ, BECAUSE THE REBUILD REPLACES RATHER THAN WRITES.
	// walk.opened carries the [Message] values [Group.Receive] is about to hand its caller, taken
	// as each record was delivered and therefore BEFORE the line above. Since ledger item 227 a
	// rebuild leaves the message it rebuilt frozen and puts a new one in the log, so without this
	// a reaction that arrived in the SAME page as its target would be applied in the group and
	// missing from the slice Receive returns -- a caller that renders what Receive hands it,
	// rather than re-reading [Group.Messages], would never see it. Every entry is re-read from
	// the log, so what comes back is what this group holds at the moment the walk committed.
	for index, one := range walk.opened {
		if held, found := self.heldLocked(one.MessageId); found {
			walk.opened[index] = held
		}
	}
	// ── 0. THE REMOVAL, RULING 52, AHEAD OF EVERYTHING INCLUDING THE RECONCILIATION ─────────────
	//
	// IT IS ABOVE THE RECONCILE BLOCK AND NOT MERELY ABOVE THE RETURNS, and that is the one thing
	// this position decides. The reconciliation's conclusion is "every index on the server under
	// this device's handle is one my reserver allocated", which exists to license [Group.Send]; a
	// device this group removed will never send again whatever it concludes, so running the block
	// would be spending a store read and a permanent flag on a question that has no consumer. The
	// floor seed below it is the same read for the same consumer.
	//
	// WHAT IS ABOVE THIS LINE IS EVERYTHING THE WALK ACTUALLY DID: the cursor, the peer heads, the
	// effect rebuild and the re-read of the messages this walk opened. A removed device still gets
	// its page -- the rows at and below the epoch it was removed at are served under its own
	// read_key and open under its own schedule -- so the sentence below arrives BESIDE the history
	// it is entitled to rather than instead of it.
	if self.removed != nil {
		return self.removed
	}
	if self.walkReconcilesLocked(walk) {
		// THE RECONCILIATION. It runs once per restored group, on the first walk of this
		// group's history that was COMPLETE AND CLEAN, and it is the half of the clone check
		// that happens BEFORE this device has sealed anything.
		//
		// THE INVARIANT IS ONE SENTENCE: every stream index on the server under this
		// device's sender_handle was allocated by this device's durable reserver, and a
		// reserver never rewinds. So a record of this device's own that the group's keys
		// AUTHENTICATE at an index the reserver has never handed out was sealed by something else
		// holding these keys, and there is no other reading of it. ("Authenticate" and not
		// "open" since MG-4: see ownFrameAlreadySpent, and [Group.ownIndexSeen] for why the
		// number compared is every walk's since the group came up and not this walk's.)
		//
		// WHAT THIS WALK CANNOT SEE: an own record that did NOT authenticate contributes no index,
		// because an index is only read off a record the aead authenticated -- so a server
		// bending one of this device's own records suppresses the evidence for that record.
		// THAT IS WHY THE GATE IS [Group.walkReconcilesLocked] AND NOT `walk.complete` ALONE.
		// The evidence this walk is missing is named by the walk itself, and a sentence as
		// strong as "this device is alone with its identity" is not written down over a walk
		// that is admittedly short of records.
		highWater, err := self.ownHighWaterLocked(walk.ownNow)
		if err != nil {
			// NOT reconciled, so Send stays refused. A reserver that will not answer is
			// not evidence that this device is alone with its identity.
			return fmt.Errorf("urmessage: this device's own stream position could not be read, so this restored group cannot reconcile: %w", err)
		}
		if highWater < self.ownIndexSeen {
			self.identityInUse = fmt.Errorf(
				"%w: group %x epoch %d: the server holds a record this group's keys authenticated at stream index %d under this device's own sender_handle, and this device's durable reserver has never allocated past %d",
				ErrIdentityInUse, self.id, self.epoch, self.ownIndexSeen, highWater)
		}
		self.reconciled = true
	}
	if self.identityInUse == nil && walk.foreignIndex != 0 {
		if walk.foreignBody {
			// THE COLLISION ITSELF, AFTER THE FACT. Two records under one
			// (epoch, sender_handle, stream_index) is one record_key and one nonce, and
			// this device is holding the OTHER plaintext.
			self.identityInUse = fmt.Errorf(
				"%w: group %x epoch %d: record %d opened under this device's own sender_handle at stream index %d, and it is NOT the record this device sealed at that index -- two records under one (epoch, sender_handle, stream_index) are one record_key and one nonce",
				ErrIdentityInUse, self.id, self.epoch, walk.foreignRecord, walk.foreignIndex)
		} else {
			self.identityInUse = fmt.Errorf(
				"%w: group %x epoch %d: record %d opened under this device's own sender_handle at stream index %d, and this device never sealed at that index",
				ErrIdentityInUse, self.id, self.epoch, walk.foreignRecord, walk.foreignIndex)
		}
	}
	if self.identityInUse != nil {
		return self.identityInUse
	}
	// THEN THE FLOOR THIS DEVICE'S OWN STREAM HAS TO CLEAR, which is ledger item 245's first
	// piece, and it is here -- below the identity refusal and above everything else -- for a
	// reason that is the identity refusal's read the other way round. A device whose reserver
	// stands BELOW indices the server already holds claims at will produce the collision on its
	// very next Send; the refusal above is the state after that has happened, and this is the one
	// moment before it, on the walk that just saw the claims.
	floorEstablished, seedErr := self.seedOwnStreamLocked(walk)
	if seedErr != nil {
		return seedErr
	}
	// AND THE GATE THAT SEED IS OWED, WHICH IS WHAT MAKES IT A RULE RATHER THAN A REPAIR THAT RUNS
	// WHEN IT HAPPENS TO RUN. Until this line has been reached over a walk that saw the whole
	// history AND ESTABLISHED THE FLOOR, [Group.Send] and every commit door answer
	// [ErrStreamFloorUnheld] -- because a JOINED group cannot know whether the leaf it landed on
	// carries a previous occupant's claims without looking, and the refusal a collision produces is
	// sticky for the life of the process. THE SEED'S OWN VERDICT IS CARRIED IN because the walk's
	// tidiness said nothing about the floor: see [Group.ownFloorHeld] for the two reproductions.
	if self.ownFloorHeldByLocked(walk, floorEstablished) {
		self.ownFloorHeld = true
	}
	// THEN THE WRAP THAT NEVER ARRIVED, ahead of the fetch refusal and of walk.firstFailure, and
	// for the same reason the identity refusal is ahead of all three: it is the CAUSE of what
	// comes after it. A group with no pq_secret for its epoch has the wrong read_key AND the wrong
	// write_key, so the server refuses req_auth before any AEAD is reached and every record of
	// that epoch that IS served fails at the tag -- a walk that reported either would report the
	// symptom and lose the sentence about the wrap, which is exactly the undiagnosable
	// REASON_REJECTED ruling 38 exists to prevent. It is STICKY, so it is answered on every later
	// walk too, AND SINCE THIS COMMIT IT IS ALSO ANSWERED ON EVERY LATER walk's REFUSED FETCH,
	// which is the arm a dark group actually takes from its second Receive onwards. The sticky
	// copy is the whole of why it can be: `resolveErr` was said once, by one walk, a process ago.
	//
	// AND RULING 41's HALT AHEAD OF IT, for the same reason one more time. It is a DIFFERENT state
	// from the dark one and it is answered from a different field, and it is ahead because a
	// halted group never entered the epoch a dark one is stuck in -- if both were somehow set, the
	// one that describes a commit this device REFUSED is the earlier event and the cause of
	// everything after it. Before this line the refusal was returned by exactly ONE walk: the
	// second answered `ratchet generation already consumed` (step (0) of
	// [Group.ingestCommitLocked] consumes the committer's generation before the refusal is
	// reached, so the sentinel cannot be re-derived), the third [ErrRecordAbandoned], and the
	// fourth nil.
	if self.halted != nil {
		return self.halted
	}
	if self.wrapDark != nil {
		return self.wrapDark
	}
	// THEN THE TRANSPORT, which is what stopped THIS walk. Below the two sticky refusals and above
	// the walk's own two, which is exactly where the old code put it for four of the five arms by
	// accident of order: an arm that returned its own fetch error after calling this function
	// skipped 1 and 2 and also skipped 4 and 5. Only the skipping of 1 and 2 was a defect.
	if fetchErr != nil {
		return fetchErr
	}
	if walk.firstFailure != nil {
		return walk.firstFailure
	}
	if walk.omitted != nil {
		return walk.omitted
	}
	return headsErr
}

// walkReconcilesLocked is whether THIS walk is one the clone check may conclude anything from.
//
// IT IS A SEPARATE PREDICATE BECAUSE IT IS A SEPARATE QUESTION, and running the two together in an
// `if` was how the answer came out wrong. "Did the server finish handing over the page" and "did
// this walk see the group's history" are not the same sentence, and only the second one licenses
// [Group.reconciled].
//
// THE CHEAPEST WAY PAST A CHECK IS A FAILURE THE CHECKER ALREADY PRINTED. `walk.complete` means
// only that the server called one page COMPLETE. The same walk carries two fields that say it did
// not see the history, BOTH OF WHICH THIS CLIENT COMPUTED AND RETURNED TO ITS CALLER:
//
//   - `walk.omitted` -- §4.3.4's own `high_water_record_id`, above every record the server handed
//     over. The server's admission, in its own field, that a page it called complete is short.
//   - `walk.firstFailure` -- a record that did not open. An index is read only off a record the
//     aead authenticated, so a record that did not open contributes NO index, and the header is
//     plaintext so this build cannot even tell whether the lost record was its own. A walk with a
//     hole in it is a walk whose missing index could be the one the check exists to find.
//
// Reconciling over either of those is declaring "every index on the server under this handle is
// one my reserver allocated" on the strength of records that were never seen. Both were measured
// past the old gate: a copy two indices BEHIND the original -- the case [Device.Restore]'s header
// says is caught before it seals anything -- reconciled with [Stats.Omitted] at 1 and
// [ErrFetchOmitted] on its way back to the caller, and then sealed at an index the original had
// already used. One bent own record did the same.
//
// WHAT IT COSTS, BOUNDED RATHER THAN HAND-WAVED. A group that has not had a clean walk stays
// [ErrNotReconciled] and the caller calls [Group.Receive] again -- which is what the transport-error
// path above already does, so this is the shape the function already had rather than a new one. The
// cost is NOT unbounded: a record that will not open is retried [maxRecordAttempts] times and then
// ABANDONED, and an abandoned record is resolved past without calling `fail`, so it sets no
// `firstFailure` on any later walk. So one permanently bent record delays the reconciliation by at
// most maxRecordAttempts+1 Receives and then stops delaying it. A server that permanently omits is
// the case that stays refused, and that is the intended reading: this device cannot check itself
// against a server that will not show it its own history.
func (self *Group) walkReconcilesLocked(walk *pageWalk) bool {
	return self.walkSawTheWholeHistoryLocked(walk) && !self.reconciled
}

// walkSawTheWholeHistoryLocked is the three clauses above without the fourth: did this walk see
// everything the server holds for this group.
//
// IT IS ONE FUNCTION BECAUSE TWO GATES ASK IT AND THEY MUST NOT DRIFT APART. The clone check
// ([Group.walkReconcilesLocked]) and this device's own stream floor ([Group.ownFloorHeld]) are
// different questions -- "is another copy writing my stream" and "did somebody else spend indices
// under my sixteen octets before me" -- but the evidence either may conclude from is the same
// walk, and the argument is the one written above: a walk with a hole in it is a walk whose
// missing record could be the one the gate exists to find. A page the server called complete
// while naming a higher high_water, or a record that did not open, are both such holes.
func (self *Group) walkSawTheWholeHistoryLocked(walk *pageWalk) bool {
	return walk.complete && walk.omitted == nil && walk.firstFailure == nil
}

// ownFloorHeldByLocked is whether THIS walk may raise [Group.ownFloorHeld]: three clauses, of which
// the first is the seed's own verdict and the other two are about what the walk could not see.
//
// IT IS A SEPARATE PREDICATE FOR THE REASON [Group.walkReconcilesLocked] IS ONE: the gate used to be
// `if self.walkSawTheWholeHistoryLocked(walk)`, which asks whether the SERVER finished handing over
// a page, and answers it in place of the question the field's own name asks -- whether this device's
// floor now clears the claims the server holds under its sixteen octets. The two reproductions are
// at [Group.ownFloorHeld]; both of them satisfy the old single clause.
//
//  1. `established` -- [Group.seedOwnStreamLocked]'s answer. It is false on all three of the roads
//     that seed returns early on: an unreconciled group (the seed must not launder the clone
//     check's evidence), a reserver with no [StreamIndexSeeder] (nothing can move a floor), and a
//     failed seed. A gate that did not carry it certified the floor on every one of them.
//  2. the whole history, [Group.walkSawTheWholeHistoryLocked]. A page the server called complete
//     while naming a higher high_water, or a record that did not open, is a hole whose missing
//     record could be the claim this gate exists to find.
//  3. THIS GROUP HAS COVERED AT LEAST ONE RECORD, which is the first reproduction: one clean walk
//     over an empty page, from a cursor at zero, with the server holding three claims. A group
//     above epoch zero cannot have an empty history -- the commit that opened its current epoch is
//     sealed at the epoch below and so is served under item 246's ceiling, and a joiner walks from
//     record zero -- so a walk that has covered nothing at all has been told nothing rather than
//     told that there is nothing. It is the group's cursor and not this walk's page, so the
//     ordinary second Receive over an empty page still raises the gate.
//
// THERE WAS A FOURTH CLAUSE AND DELETING IT IS THIS FUNCTION'S MOST IMPORTANT PROPERTY. It was
// `ownFloorBlind == 0`: one record this group gave up on before it could read its header, ANYWHERE
// in its history, took the flag away and nothing put it back. Every other clause here is a fact
// about one walk and clears when a later walk is better; that one was a fact about the GROUP, and
// because the cursor and the attempt counts are not persisted, a restart re-walks the row, re-spends
// [maxRecordAttempts] on it and re-derives the veto -- so the refusal came back every time the app
// opened, in EVERY group, including a group founded by this device alone whose leaf no one else has
// ever stood at. It was reproduced that way: three founding members, three distinct
// sender_handles, nothing removed, one bent row, [Group.Send] allowed before the restart and
// refused for ever after it.
//
// WHAT REPLACES IT IS A READING OF THE ROW AND NOT A CLAUSE HERE. The narrow property is *this
// device must not seal at an index a previous occupant of its leaf may already have claimed*, and a
// record this build cannot parse is evidence about that only if it is a record under this device's
// own sender_handle. §4.3.3 says which: the fetch row carries the server-indexed `sender_handle` and
// `stream_index` BESIDE `record_bytes`, so [Group.noteUnparsedClaimLocked] folds the projection into
// [Group.ownClaimSeen] and the seed raises the floor past it -- the honest case, a previous occupant
// on a record format this build cannot read, ends in a SEND rather than in a brick. A row the server
// will not attribute at all moves [Stats.UnopenedUnattributed] and nothing else; the argument for
// that is written at the counter and at [Group.noteUnparsedClaimLocked].
//
// WHAT IT STILL DOES NOT CATCH, NAMED SO IT IS NOT MISTAKEN FOR CLOSED: a server that hands over
// SOME rows and silently omits others while declaring a high_water no higher than what it sent.
// §4.3.4's high_water_record_id is the only omission detector on this path and it is the server's
// own field, which is item 246's residual and not this gate's to close.
func (self *Group) ownFloorHeldByLocked(walk *pageWalk, established bool) bool {
	return established && self.walkSawTheWholeHistoryLocked(walk) && self.cursor != 0
}

// ownHighWaterLocked is the highest stream index this device's DURABLE reserver has ever allocated
// for this group's own stream. It is the reserver's number and never a recomputed one.
func (self *Group) ownHighWaterLocked(own [16]byte) (uint64, error) {
	key := messagegroup.StreamKey{SenderHandle: own}
	copy(key.GroupId[:], self.id)
	return self.device.reserver.HighWater(key)
}

// StreamIndexSeeder is the one thing this package needs of a reserver that
// [messagegroup.StreamIndexReserver] does not declare: raising a stream's FLOOR without allocating
// anything. It answers the high water the stream carries afterwards.
//
// IT IS AN OPTIONAL INTERFACE AND A RESERVER WITHOUT IT STILL WORKS, which is deliberate rather
// than defensive. Reserve and HighWater are the surface a SENDER RATCHET allocates through and a
// ratchet has no business moving a floor; adding a method there would put a door on the hot path
// of every seal for the sake of one call per walk. [sdk.NewStreamIndexReserver] supplies it; a
// caller that built its own reserver over some other store gets the behaviour of every build
// before ledger item 245, which is named at [Group.seedOwnStreamLocked].
type StreamIndexSeeder interface {
	SeedTo(stream messagegroup.StreamKey, floor uint64) (uint64, error)
}

// seedOwnStreamLocked raises this device's own durable stream floor past every index this walk saw
// CLAIMED under the handle this device seals with. Ledger item 245's first piece.
//
// WHY IT EXISTS AND WHAT IT UNBRICKS. A sender_handle is SenderHandle(group_handle_key, leaf): no
// epoch, no identity, and group_handle_key never rotates. RFC 9420 §7.7 refills the leftmost blank
// leaf, so the next Add after a removal lands a NEWCOMER on the removed member's leaf and under
// the removed member's sixteen octets. That newcomer's reserver has never allocated for that
// stream, so [Group.Send] seals at index 1 -- and index 1 under those octets is an index the
// server already holds a `message_stream_claim` at, carrying different content. The submit is
// answered REASON_STREAM_INDEX_REUSED, [Group.cloneRefusalLocked] latches [ErrIdentityInUse], and
// that is STICKY for the life of the process: a member that has just been added can never send in
// the group it just joined. Seeding past the claims is what makes the two occupants' index ranges
// DISJOINT.
//
// AND IT IS WHAT CLOSES THE message_id COLLISION, WHICH IS A CHECKED FACT AND NOT A HOPE. MASTER
// §8.4.5 expands an id from (group_id, sender_handle, stream_index) and nothing else, so two
// occupants of one leaf collide EXACTLY at equal stream indices -- their group and their handle
// are equal by construction. Disjoint ranges are therefore disjoint ids with no change to any
// preimage and nothing on the wire. That is measured rather than asserted, with the collision at
// an equal index as the control in the same case; see the removal suite.
//
// THE NUMBER IS READ OFF A PLAINTEXT HEADER, WHICH IS THE ONE THING IN THIS PACKAGE THAT IS, AND
// HERE IS THE PRICE. Every other index this group acts on comes off a record the AEAD
// authenticated ([Group.ownIndexSeen], [Group.notePeerHeadLocked]), because those numbers decide
// how far a ratchet walks or whether this device is a clone, and a server that could choose them
// could wedge any client with one forged row. THIS number can be chosen by the server, and what a
// chosen one buys is that this device burns stream indices it did not need to. It cannot make this
// device seal at an index another party has used -- that is the direction the seed moves AWAY from
// -- it cannot make [ErrIdentityInUse] fire, and it cannot rewind anything, because
// [sdk.StreamStore.SeedStreamIndex] is monotone and refuses the last index a u64 holds by name.
// The trade is the one [Group.cloneRefusalLocked] already takes and states: a server can deny
// every submit outright, so what a forged header buys it here is strictly less than what it
// already has.
//
// IT IS NOT GATED ON [Group.walkReconcilesLocked], AND THAT IS THE POINT RATHER THAN AN OVERSIGHT.
// A group JOINED in this process is `reconciled` by construction -- its identity was drawn here --
// so the reconciliation never runs for the one device this repair exists for. What the seed is
// gated on instead is [Group.identityInUse] being unset, checked by the caller: a group already
// refused for a clone must not have its floor moved, because the clone check's own evidence
// ([Group.ownIndexSeen]) is the AUTHENTICATED number and outranks this one.
//
// ── AND IT IS GATED ON [Group.reconciled], WHICH IS THE ONE THING THIS SEED CAN BREAK ────────
//
// THE SEED AND THE CLONE CHECK ASK THE SAME QUESTION AND ANSWER IT DIFFERENTLY. "An index on the
// server under my own sender_handle that my reserver never allocated" is read by the clone check as
// ANOTHER COPY OF THIS FOLDER and by this function as A PREVIOUS OCCUPANT OF THIS LEAF, and the
// handle alone cannot tell them apart -- that is item 245's linkability residual seen from the
// inside. What DOES tell them apart is already here: a clone's record was sealed by this device's
// own leaf key at an epoch this device stands in, so it AUTHENTICATES and raises
// [Group.ownIndexSeen]; a previous occupant's record is below this device's admission, so it
// answers [GapOutOfWindow] and raises nothing. The clone check compares the reserver's high water
// against that authenticated number -- so a seed taken BEFORE that comparison LAUNDERS IT, by
// raising the high water past the evidence.
//
// MEASURED, and by cp3b rather than by argument: without this gate,
// cp3b.TestACopyWhoseEvidenceArrivedInADirtyWalkIsStillCaught turns RED -- "the copy's clean
// Receive answered <nil>, want ErrIdentityInUse". The copy's FIRST walk is dirty, so
// [Group.walkReconcilesLocked] is false and the clone check does not run; an ungated seed fires on
// that same walk, and the clean walk that follows finds a high water it has already moved.
//
// THE GATE IS ONE CONDITION AND IT COSTS THE NEWCOMER NOTHING. A restored group is NOT reconciled
// until a clean, complete walk has held its own indices against its reserver -- so the seed waits
// for that walk and, inside it, runs AFTER the check, on the same call, because the check is above
// this line in [Group.commitWalkLocked] and sets the flag itself. A group FOUNDED or JOINED in this
// process is reconciled by construction, so the newcomer this repair exists for seeds on its very
// first walk.
//
// IT RUNS ON EVERY WALK AND NOT ONCE, because a "first walk" that a transport failure cut short is
// still a first walk, and a device that seeded off half a page and then never looked again would
// brick exactly as before. The call is idempotent and costs nothing when there is nothing to do:
// in the ordinary life of a group the highest index claimed under this device's own handle is one
// this device allocated, so the floor never moves and no durable write happens.
//
// A RESERVER THAT CANNOT SEED IS NOT AN ERROR, and what such a device does is every build before
// this one: it starts at index 1 on a reused leaf and is refused at the submit. That is stated
// here rather than hidden behind a nil check, and it is the behaviour of any caller that supplied
// its own [messagegroup.StreamIndexReserver] instead of [sdk.NewStreamIndexReserver]. IT DOES NOT
// GET THE GATE'S BENEFIT OF THE DOUBT EITHER: the verdict below is false for such a reserver, so a
// device that cannot move a floor is refused at [Group.Send] instead of sealing into a stream it
// cannot position -- which is a refusal it can report rather than a sticky [ErrIdentityInUse].
//
// ── WHAT IT ANSWERS, AND WHY THE ANSWER IS THE GATE'S FIRST CLAUSE ───────────────────────────
//
// THE BOOL IS "THIS DEVICE'S FLOOR NOW CLEARS EVERY CLAIM THIS GROUP HAS SEEN UNDER THESE OCTETS",
// and it exists because [Group.ownFloorHeld] used to be raised by a predicate that said nothing
// about the floor while this function returned early on three separate roads. It is true on exactly
// two: nothing is claimed, or the reserver already stands at or above the claim -- including the
// case where this call moved it there. It is false on all three early returns. The two measurements
// that made it necessary are at [Group.ownFloorHeld].
func (self *Group) seedOwnStreamLocked(walk *pageWalk) (bool, error) {
	claimed := self.ownClaimedLocked(walk.ownNow)
	if !self.reconciled {
		return false, nil
	}
	seeder, canSeed := self.device.reserver.(StreamIndexSeeder)
	if !canSeed {
		return false, nil
	}
	if claimed == 0 {
		// NOTHING IS CLAIMED UNDER THESE OCTETS, so every floor this stream could have clears the
		// set, INCLUDING the floor it already stands at, and there is nothing to write. The answer
		// is `true` and it is a statement about the claims this group has SEEN -- which is what
		// clauses 2 to 4 of [Group.ownFloorHeldByLocked] are for. It is answered before the
		// reserver is read because this is the arm every ordinary walk of every ordinary group
		// takes, and a durable read per Receive to compare a number against zero is a cost with no
		// question behind it.
		return true, nil
	}
	highWater, err := self.ownHighWaterLocked(walk.ownNow)
	if err != nil {
		return false, fmt.Errorf("%w: group %x: this device's own stream position could not be read, so its floor cannot be held against the claim at stream index %d the server holds under its own sender_handle: %w",
			ErrStreamFloor, self.id, claimed, err)
	}
	if claimed <= highWater {
		return true, nil
	}
	key := messagegroup.StreamKey{SenderHandle: walk.ownNow}
	copy(key.GroupId[:], self.id)
	if _, err := seeder.SeedTo(key, claimed); err != nil {
		return false, fmt.Errorf("%w: group %x: the server holds a claim at stream index %d under this device's own sender_handle and this device's reserver has only reached %d, and the floor could not be moved, so this device's next send would collide with it: %w",
			ErrStreamFloor, self.id, claimed, highWater, err)
	}
	self.stats.StreamFloorSeeded += 1
	return true, nil
}

// ownClaimedLocked is the highest stream index this group has seen claimed under `own`, and zero for
// any other handle. See [Group.ownClaimSeen] for why the handle is part of the answer.
func (self *Group) ownClaimedLocked(own [16]byte) uint64 {
	if self.ownClaimHandle != own {
		return 0
	}
	return self.ownClaimSeen
}

// noteOwnClaimLocked folds one record's plaintext header into [Group.ownClaimSeen], which is the
// number [Group.seedOwnStreamLocked] raises this device's own stream floor to.
//
// THE GROUP ID IS COMPARED BECAUSE NOTHING HAS COMPARED IT YET ON THIS PATH: a row from another
// group is not evidence about this stream.
func (self *Group) noteOwnClaimLocked(walk *pageWalk, header *message.RecordHeader) {
	if !bytes.Equal(header.GroupId[:], self.id) {
		return
	}
	self.foldOwnClaimLocked(walk, header.SenderHandle, header.StreamIndex)
}

// noteUnparsedClaimLocked folds THE SERVER'S OWN §4.3.3 PROJECTION of a row this build could not
// parse into [Group.ownClaimSeen], and answers whether that row was attributed to a stream at all.
//
// WHY THE PROJECTION AND NOT NOTHING. `protocol.Record` is `record_bytes` BESIDE the server-indexed
// projection of its header -- "the server MUST verify that each equals the corresponding field of
// ParseRecord(record_bytes)" -- and every deployed server fills it from the same `projectionOf` the
// submit path checks a client's against (message-server `api/fetch.go`, which re-encodes the row it
// serves out of the very columns it indexed). So a row whose octets this build cannot read still
// arrives WITH the two facts the floor question needs: whose stream it is on, and at what index. The
// alternative was the state this function exists to delete -- one unreadable row anywhere in a
// group's history refusing every [Group.Send] of every later process, in every group, on the
// strength of a question the row itself answers.
//
// IT IS EXACTLY AS TRUSTWORTHY AS THE NUMBER THE SEED ALREADY ACTS ON, which is the whole of the
// argument and is not a new trade. [Group.ownClaimSeen] is read off a PLAINTEXT header that the
// server relays and could rewrite; this is read off a plaintext projection of the same header on the
// same wire. What either buys a hostile server is stream indices this device did not need to spend
// ([Stats.StreamFloorSeeded]); neither can make a floor go DOWN, because
// [sdk.StreamStore.SeedStreamIndex] is monotone.
//
// AND WHAT A SERVER THAT LIES THE OTHER WAY BUYS IS NOTHING IT DOES NOT ALREADY HOLD. A projection
// that names some other leaf while the unreadable octets are really this device's leaves the floor
// below a claim, and the seal that follows is answered REASON_STREAM_INDEX_REUSED and latched as
// [ErrIdentityInUse] -- which that same server can answer any submit with directly and for no
// reason at all, as [Group.cloneRefusalLocked] states and accepts. That refusal also DIES WITH THE
// PROCESS, where the clause this replaces came back at every restart: the conservative direction was
// the more expensive one.
//
// NO GROUP ID IS COMPARED HERE AND THAT IS SAID RATHER THAN HIDDEN. The projection carries none --
// §4.3.3 indexes a row inside a group -- so what scopes this row to this group is the fetch that
// asked for it (§4.3.1 names the group) and the record id it came back under. A server that answers
// one group's fetch with another group's row is the same server that could choose the index outright,
// which is the trade priced two paragraphs up.
func (self *Group) noteUnparsedClaimLocked(walk *pageWalk, row *protocol.Record) bool {
	var handle [16]byte
	projected := row.GetSenderHandle()
	if len(projected) != len(handle) {
		return false
	}
	copy(handle[:], projected)
	if handle != walk.ownNow {
		// ATTRIBUTED, AND NOT TO THIS DEVICE'S STREAM. It is a claim on somebody else's
		// numbering and no floor of this device's has to clear it.
		return true
	}
	index := row.GetStreamIndex()
	if index == 0 {
		// §5.6 numbers a stream from one, so a projection naming this handle with no index is
		// the server declining to say WHERE on this device's own stream the row sits -- which is
		// the same silence as no projection at all.
		return false
	}
	self.foldOwnClaimLocked(walk, handle, index)
	return true
}

// foldOwnClaimLocked is the one writer of [Group.ownClaimSeen], because the two readings that reach
// it -- a parsed header and §4.3.3's projection of one this build could not parse -- must not drift
// apart on WHICH stream a claim belongs to or on how the number moves.
func (self *Group) foldOwnClaimLocked(walk *pageWalk, handle [16]byte, index uint64) {
	if handle != walk.ownNow {
		return
	}
	if self.ownClaimHandle != walk.ownNow {
		self.ownClaimHandle = walk.ownNow
		self.ownClaimSeen = 0
	}
	if self.ownClaimSeen < index {
		self.ownClaimSeen = index
	}
}

// openPageLocked walks one page's records: it advances the two positions, counts what it skips,
// and opens what is a message.
//
// It is a method rather than the body of the loop above so that "one page" is a thing with a name
// -- and so that the paging decisions and the record decisions are not one forty-line block where
// a `continue` could mean either.
//
// THIS DEVICE'S OWN RECORDS ARE SHOWN HERE AND ARE NOT SKIPPED, which is the repair for the worst
// user-facing defect the durable store introduced. A restored group's log starts EMPTY and the
// cursor is not persisted, so a restarted device re-reads its whole history -- and while this
// method skipped every record whose sender_handle was its own, a user who closed the app and
// reopened it got the other side's half of the conversation and none of their own, with a nil
// error and one counter that moved on the ordinary echo case too.
//
// AND THEY ARE SHOWN FROM THE COPY THIS DEVICE KEPT, NOT OPENED, which is a change connect forced
// rather than one this method chose. Until connect 4c030dc an own record opened under a receiver
// ladder derived from the class key every member holds. Since it, the body is an MLS PrivateMessage
// and a member cannot open its own (MG-4), and at d368fea every own record here answered "mls:
// ratchet generation already consumed" -- which, while this method still asked for it, turned every
// restart, every clone case and the lost answer red in sdk/cp3b. So an own record now takes one of
// three roads, in this order, and each is its own counter:
//
//   - [Group.openOwnFromCopyLocked]: this device sealed at that index, kept what it sealed, and the
//     record carries that body_hash over that ct_body. Shown from the copy. [Stats.OpenedOwn].
//   - OpenRecord OPENS it: sealed by something else holding this leaf's signature key at a
//     generation this device never spent, which is a copy of this folder ahead of this one. Shown,
//     and read as the clone evidence it is. [Stats.OpenedOwn].
//   - OpenRecord refuses it at the spent generation: authenticated to this group's keys and not
//     showable. [Stats.OwnWithoutCopy], resolved past, and read as evidence the same way; see
//     ownFrameAlreadySpent for exactly how strong that evidence is.
//
// Every other refusal of an own record is an ordinary record that did not open. What keeps any of
// it from delivering a message twice is [Group.delivered] and, for the copy, the record id it was
// shown under.
func (self *Group) openPageLocked(fetched *protocol.FetchResponse, walk *pageWalk) {
	resolve := func(recordId uint64) {
		if !walk.blocked && walk.resolvedTo < recordId {
			walk.resolvedTo = recordId
		}
	}
	fail := func(recordId uint64, err error) {
		self.stats.FailedOpen += 1
		self.attempts[recordId] += 1
		if self.attempts[recordId] < maxRecordAttempts {
			if walk.firstFailure == nil {
				walk.firstFailure = err
			}
			// and the cursor stops here, so the NEXT Receive asks for this record again.
			walk.blocked = true
			return
		}
		// THE BOUND. The record is given up on, and an abandonment OUTRANKS whatever
		// retryable failure was already held: a hole that will not be filled is worse news
		// than one that might be, and the caller gets the worse of the two.
		self.unopened = append(self.unopened, recordId)
		self.stats.Unopened += 1
		walk.firstFailure = fmt.Errorf("%w: record %d, after %d attempts: %w",
			ErrRecordAbandoned, recordId, self.attempts[recordId], err)
		resolve(recordId)
	}
	for _, row := range fetched.GetRecords() {
		self.stats.Fetched += 1
		recordId := row.GetRecordId()
		if walk.from < recordId {
			walk.from = recordId
		}
		if walk.reached < recordId {
			walk.reached = recordId
		}
		if maxRecordAttempts <= self.attempts[recordId] {
			// already given up on. It is here only as a passenger of a rewind over some
			// earlier record, and it must not block the cursor a second time.
			resolve(recordId)
			continue
		}
		parsed, err := message.ParseRecord(row.GetRecordBytes())
		if err != nil {
			// THE ONE ROAD THAT LOSES THE RECORD'S OWN HEADER, SO THE FLOOR TAKES THE SERVER'S
			// PROJECTION OF IT. Every other failure below is downstream of a header this function
			// has already read, so the index it claims is folded into [Group.ownClaimSeen] whatever
			// happens to the record afterwards. This one has no header this build can read -- and
			// §4.3.3 puts `sender_handle` and `stream_index` on the row BESIDE `record_bytes`, so
			// the two facts the floor needs are here anyway. [Group.noteUnparsedClaimLocked] takes
			// them and argues exactly what trusting them costs; a row the server will not attribute
			// at all is counted and is not a refusal, because the refusal that used to stand here
			// was re-derived at every restart and no [Group.Receive] cleared it.
			//
			// ASKED AT THE ABANDONMENT AND NOT AT THE FAILURE, because a record that parses on the
			// second attempt has lost nothing and its own header is the better reading -- and asked
			// through `fail`'s OWN decision rather than by re-testing [maxRecordAttempts] here, so
			// the two cannot come apart.
			//
			// A CHEAPER HEADER READ IS NOT AVAILABLE AND THAT IS MEASURED, NOT ASSUMED.
			// message.ParseRecordHeader is decodeRecord with the ciphertexts dropped
			// (connect/message/codec.go) -- deliberately the SAME acceptance set, because two entry
			// points that disagree about which records exist is the defect that file makes
			// unrepresentable, and its codec test asserts the agreement. A more permissive reader
			// written here would be that second entry point; and for the class that motivates one --
			// a record_format_version this build does not know -- every field past the version byte
			// sits at a different offset, so a permissive read would produce a WRONG index rather
			// than no index. The server's projection is a number the server INDEXED the row on; a
			// guess made by re-reading octets at the wrong offsets is not.
			unopenedBefore := len(self.unopened)
			fail(recordId, fmt.Errorf("%w: record %d does not parse: %w", ErrRecordOpen, recordId, err))
			if unopenedBefore < len(self.unopened) && !self.noteUnparsedClaimLocked(walk, row) {
				self.stats.UnopenedUnattributed += 1
			}
			continue
		}
		header := &parsed.Header
		// THE FLOOR THIS DEVICE'S OWN STREAM HAS TO CLEAR, NOTED BEFORE ANY BRANCH BELOW TAKES
		// THE RECORD AWAY. Every record here -- a message, a wrap, a marker, a commit -- was
		// sealed through [messagegroup.GroupSession.SealRecord] and therefore SPENT a stream
		// index under the handle its header names, so a floor built out of the application
		// records alone would sit below indices the server already holds claims at.
		// [Group.seedOwnStreamLocked] is what this number is for, and is where reading it off a
		// PLAINTEXT header is argued.
		//
		// IT IS STILL BELOW THE two-roads-above SKIP OF A RECORD ALREADY GIVEN UP ON, and what that
		// ordering costs is now nothing rather than unmeasured: [Group.ownClaimSeen] is cumulative
		// on the group, so a row abandoned on an earlier walk contributed its index on the walk that
		// PARSED it and is never forgotten, and a row that never parsed contributed §4.3.3's
		// projection of its index on the walk that ABANDONED it, through
		// [Group.noteUnparsedClaimLocked]. Re-parsing a passenger row here could only re-derive a
		// number this group already holds.
		self.noteOwnClaimLocked(walk, header)
		if header.IsCommit {
			// A5: AN is_commit RECORD IS INGESTED, NOT SKIPPED -- but only the ONE that opens the
			// epoch this session is at, which is the commit whose header names this epoch (a
			// commit sealed at epoch E opens E+1, and this member at epoch E is the member that
			// must follow it). Every other is_commit record is ceremony this walk reads past: the
			// FOUNDING commit once this device is past epoch one (header epoch 0), and any commit
			// this device has already ingested and moved beyond on an earlier walk. Both would be
			// refused by OpenCeremonyRecord under this session's epoch check anyway (§8.4.1), so the
			// guard here and that check are the one rule; deciding it here keeps a stale commit off
			// the ingest path and out of a fail().
			if header.Epoch != self.epoch {
				self.stats.SkippedCeremony += 1
				resolve(recordId)
				continue
			}
			// A GROUP THAT HAS HALTED DOES NOT PROCESS THIS RECORD AGAIN, AND THAT IS RULING 41's
			// REFUSAL BEING AS DURABLE AS THE DARK STATE IT IS CONTRASTED WITH. The commit that
			// halted this group is still the first record above the cursor and it always will be,
			// so every later walk meets it. Re-ingesting it cannot re-derive the refusal --
			// [Group.ingestCommitLocked]'s step (0) has already consumed the committer's ratchet
			// generation, so the second attempt answers `ratchet generation already consumed` --
			// and it is not a record that "did not open", so it must not be spent through
			// fail()'s three attempts into [ErrRecordAbandoned] and a cursor resolved PAST it.
			// MEASURED before this clause: walks 1 to 5 over one refused commit answered the
			// sentinel, the ratchet error, ErrRecordAbandoned, nil and nil.
			if self.halted != nil {
				walk.blocked = true
				continue
			}
			// AND A GROUP THIS COMMIT REMOVED DOES NOT PROCESS IT AGAIN EITHER -- RULING 52, and
			// the clause is the halt's for one reason it shares and one it does not. Shared: the
			// removing commit is the first record above this cursor and always will be, so every
			// later walk meets it, and re-processing cannot re-derive the answer -- mls closed the
			// group and zeroized its epoch secrets when it answered, so the second Process answers
			// `the group is closed and its epoch secrets have been zeroized`. Not shared, and it is
			// the sharper half: a removal is the ONE record a device cannot open and must not
			// retry. Three attempts and a cursor bump is the shape of a transient, and what it
			// produced here was measured -- [ErrRecordAbandoned] on the third walk and nil on every
			// walk after it, so the device that had been thrown out of the group read as caught up
			// and silent for ever. See [ErrRemovedFromGroup].
			if self.removed != nil {
				walk.blocked = true
				continue
			}
			if err := self.ingestCommitLocked(walk, parsed); err != nil {
				if errors.Is(err, ErrRemovedFromGroup) {
					// REMOVED BY A VALID COMMIT, WHICH IS NOT A RECORD THAT DID NOT OPEN. It
					// opened, it verified and it was authorized; what it did was end this
					// device's membership. The walk's sentence is [Group.removed], answered by
					// [Group.commitWalkLocked] above every other refusal, and what this arm owes
					// is that the cursor stays BELOW the commit -- so `fail` is not called, no
					// attempt is spent, [Stats.Unopened] does not move and the record is never
					// abandoned. The clause above is what meets it on every later walk.
					walk.blocked = true
					continue
				}
				if errors.Is(err, ErrRemovalWithoutRotation) {
					// REFUSED BY RULE, WHICH IS NOT A RECORD THAT DID NOT OPEN. The walk's
					// sentence is [Group.halted], answered by [Group.commitWalkLocked] above
					// every other refusal; what this arm owes is only that the cursor stays
					// BELOW the refused commit, so the halt is re-met rather than resolved past.
					walk.blocked = true
					continue
				}
				fail(recordId, err)
				continue
			}
			self.stats.SkippedCeremony += 1
			resolve(recordId)
			continue
		}
		if len(header.ServerAttachment) != 0 {
			// THE CEREMONY RECORDS AROUND A COMMIT: the epoch's wrap fan-out and the epoch-complete
			// marker.
			//
			// THE WRAP IS NO LONGER SKIPPED, and that clause is the whole of item 243's receive
			// leg. It used to be: "in the alpha a wrap carries no key material ([alphaWrapBody])
			// ... so there is nothing in either for a receiving member to read: the epoch's key
			// schedule comes off the MLS exporter the COMMIT moved, not off these." That is true
			// of the founding fan-out and of every build before rotation, and it is false of every
			// fan-out a rotating committer writes: those carry pq_secret[n+1] X-Wing-sealed to
			// each leaf, and a member that reads past its own is a member that will not be able to
			// open or write anything at the next epoch.
			//
			// THE ORDER IS THE WIRE'S AND NOT THIS LOOP'S. Ruling 37 puts the wraps on the server
			// BEFORE the commit -- they must be, since a write is accepted only at the current
			// epoch -- so they carry lower record ids and this branch meets them first, stages
			// them, and the commit below judges them. A wrap that is not this device's, or is for
			// an epoch already entered, falls through to the skip exactly as before and costs one
			// handle derivation.
			attachment, attachmentErr := message.ParseServerAttachment(header.ServerAttachment)
			if attachmentErr == nil && attachment.Kind == message.AttachmentWrap {
				self.ingestWrapLocked(walk, recordId, parsed, attachment)
			}
			// AND IT IS STILL A CEREMONY RECORD WHATEVER CAME OF THAT. A wrap is not a message,
			// it delivers no [Message], and a wrap that did not open must NOT hold this group's
			// cursor: the commit is what decides whether the epoch it is for was ever opened, and
			// a fan-out for an epoch that never happened would otherwise block every later record
			// behind three retries and an abandonment. The counters and [Group.wrapDark] are where
			// a failure is said, not here.
			self.stats.SkippedCeremony += 1
			resolve(recordId)
			continue
		}
		// THE PRE-FILTER AND NOT THE ATTRIBUTION, AND THE NAME SAYS WHICH. It is true of every
		// record carrying a handle this device has ever held -- which, since a removed member's
		// leaf is refilled by the next Add (RFC 9420 §7.7) and the handle is a function of the
		// LEAF alone, includes records the PREVIOUS occupant of this device's leaf wrote. What it
		// buys is that the two cheap own-record roads are tried; each has its own proof under it,
		// and neither can show a stranger's record as this device's: the copy road compares a
		// body_hash this device sealed, and the MG-4 arm needs MLS's own spent-generation refusal
		// on a frame this device's leaf signed. What a record IS is decided below, after the open,
		// by [Group.recordIsOwnLocked].
		maybeMine := walk.own[header.SenderHandle]
		if self.delivered[recordId] {
			// this group's log already holds it. The two readings are counted apart: the
			// ordinary echo of a send this process made, and a record re-read because a
			// rewind over an earlier failure passed back over it.
			if maybeMine {
				self.stats.SkippedOwn += 1
			} else {
				self.stats.SkippedSeen += 1
			}
			resolve(recordId)
			continue
		}
		if header.RetentionClass != message.RetentionDurable {
			self.stats.SkippedClass += 1
			resolve(recordId)
			continue
		}
		if self.withoutCopy[recordId] {
			// authenticated as this device's own on an earlier walk, and still not showable. Its
			// evidence is already in [Group.ownIndexSeen] and, when it was a copy's, already in
			// identityInUse, so nothing is taken from the header re-fetched here.
			resolve(recordId)
			continue
		}
		if header.Epoch < self.epoch && !self.pastEpochOpenableLocked(walk, header.Epoch) {
			// A RECORD SEALED AT AN EPOCH NO SCHEDULE ON THIS DEVICE REACHES: below the past epoch
			// window, or one this walk has already been told this device holds no state for. It
			// is a VISIBLE GAP and not a fail(): see [GapOutOfWindow]. Every OTHER prior-epoch
			// record falls through to the open below, which the session routes to that epoch's
			// own schedule (ledger item 241): the own-copy road, the ladder track and OpenRecord
			// all take the record's epoch off its header and never this group's.
			self.noteEpochGapLocked(walk, recordId, header)
			resolve(recordId)
			continue
		}
		// pastEpochGap is the one refusal on the roads below that is a gap and not a failure: the
		// session could not obtain the record's epoch because this device never stood in it, or
		// because it is below the window. Either way no re-fetch repairs it. The store's own
		// not-found is what says "never stood in it" -- a store that would not READ is a failure
		// like any other and is retried, because a broken disk must not render as history that
		// was never there.
		pastEpochGap := func(err error) bool {
			if errors.Is(err, messagegroup.ErrEpochOutOfWindow) {
				return true
			}
			if errors.Is(err, messagegroup.ErrPastEpochUnobtainable) && errors.Is(err, ErrStateNotFound) {
				walk.unobtainable[header.Epoch] = true
				return true
			}
			// AND THE SAME FACT ARRIVING FROM ITEM 243's TABLE RATHER THAN FROM THE STORE. A
			// derivation for an epoch this session holds no pq_secret for answers
			// ErrPqSecretUnknownEpoch, and for a PAST epoch that is the same news as the two
			// above: either this device never stood in that epoch, or the epoch fell out of
			// messagegroup.PastEpochWindow. Both are history no key here reaches, which is
			// GapOutOfWindow's own definition, and no re-fetch repairs either.
			//
			// FOUND BY liveprobe STEP 11 AND BY NOTHING ELSE, ledger item 262: a device removed
			// at epoch 8 that joined at epoch 2 came back from a restart, re-walked from a cursor
			// nothing persists, and counted its 602 PRE-JOIN records as failed opens -- the same
			// records it had counted as out_of_window gaps when it walked them at epoch 2. The two
			// answers differ because the roads do: at epoch 2 the derivation missed in the STORE
			// and answered the sentinel above, while at epoch 8 it missed in the per-epoch
			// pq_secret table item 243 added and answered this one, which this predicate did not
			// know. Item 243 built a new road to an old fact and this line is the road's arrival.
			//
			// THE BOUND `header.Epoch < self.epoch` IS NOT DRIVEN BY ANY CASE IN THIS REPOSITORY
			// AND IS KEPT ANYWAY: dropping it leaves every test in urmessage and cp3b green, which
			// is measured and not assumed. It is kept because this arm SILENCES a record -- it
			// notes a gap and resolves the id, so the cursor moves past it for ever -- and the
			// shape above the bound is one that repairs itself: a peer that sends at epoch n+1
			// puts a record above a reader still standing at n, which holds no pq_secret for n+1
			// until it ingests that commit, and a fetch page truncated between the two delivers
			// exactly that. Resolved as history, such a record is LOST a second before it would
			// have opened; left a failure, walk.blocked holds the cursor and the next Receive gets
			// it. A silencing rule must be no wider than the fact that justifies it, and the fact
			// here is about PAST epochs. The case that would drive it needs a page boundary placed
			// between a commit and the records above it; it is owed, not claimed.
			if errors.Is(err, messagegroup.ErrPqSecretUnknownEpoch) && header.Epoch < self.epoch {
				walk.unobtainable[header.Epoch] = true
				return true
			}
			return false
		}
		if maybeMine {
			shown, err := self.openOwnFromCopyLocked(walk, recordId, parsed)
			if err != nil {
				fail(recordId, err)
				continue
			}
			if shown {
				resolve(recordId)
				continue
			}
		}
		// THE TABLE FOR THE RECORD'S OWN EPOCH, and it used to be the table for THIS group's.
		// Ledger item 245: a leaf removed at n+1 is out of the current membership, so every
		// record it sealed at n resolved to nothing here and was abandoned after three fetches.
		// See [Group.leavesAtLocked].
		leaves, err := self.walkLeavesLocked(walk, header.Epoch)
		if err != nil {
			fail(recordId, fmt.Errorf("%w: record %d: the membership at epoch %d: %w",
				ErrRecordOpen, recordId, header.Epoch, err))
			continue
		}
		leaf, known := leaves[header.SenderHandle]
		if !known {
			fail(recordId, fmt.Errorf("%w: record %d names sender_handle %x, which is no leaf of this group at epoch %d",
				ErrRecordOpen, recordId, header.SenderHandle, header.Epoch))
			continue
		}
		if err := self.trackLocked(leaf, header); err != nil {
			if pastEpochGap(err) {
				self.noteEpochGapLocked(walk, recordId, header)
				resolve(recordId)
				continue
			}
			fail(recordId, err)
			continue
		}
		if maybeMine {
			if err := self.advanceOwnLadderLocked(leaf, header); err != nil {
				if pastEpochGap(err) {
					self.noteEpochGapLocked(walk, recordId, header)
					resolve(recordId)
					continue
				}
				fail(recordId, err)
				continue
			}
		}
		headPlain, bodyPlain, err := self.session.OpenRecord(parsed)
		if err != nil {
			if pastEpochGap(err) {
				self.noteEpochGapLocked(walk, recordId, header)
				resolve(recordId)
				continue
			}
			if maybeMine && ownFrameAlreadySpent(err) {
				// connect MG-4, and the ONE refusal on this path that is not a failure. See
				// ownFrameAlreadySpent for exactly what it establishes and what it does not.
				//
				// THE HANDLE IS THE PRE-FILTER AND THE REFUSAL IS THE PROOF, which is why this
				// arm is gated on the SET and not on an attribution. What establishes that the
				// frame came from this device's own leaf is MLS refusing a generation of that
				// leaf's ratchet that has already been spent -- a fact about a signature this
				// device's key made -- and a record from another occupant of the same leaf at
				// another epoch cannot reach it: it either opens (and is attributed below off
				// its own signing leaf) or refuses for some other reason.
				//
				// ONE RECORD UNDER TWO RECORD IDS IS STILL ONE RECORD SHOWN TWICE, whether it is shown
				// from a copy or only counted: the same (index, body_hash) already accounted for under
				// another number is refused, as openOwnFromCopyLocked refuses it.
				if known, held := self.ownIndices[header.StreamIndex]; held && known.bodyHash == header.BodyHash &&
					known.recordId != 0 && known.recordId != recordId {
					fail(recordId, fmt.Errorf("%w: record %d carries this device's own record at stream index %d, which this group already holds as record %d",
						ErrRecordOpen, recordId, header.StreamIndex, known.recordId))
					continue
				}
				self.stats.OwnWithoutCopy += 1
				self.withoutCopy[recordId] = true
				self.noteOwnIndexLocked(walk, recordId, header.StreamIndex, header.BodyHash)
				if known, held := self.ownIndices[header.StreamIndex]; held && known.bodyHash == header.BodyHash && known.recordId == 0 {
					known.recordId = recordId
				}
				resolve(recordId)
				continue
			}
			fail(recordId, fmt.Errorf("%w: record %d from leaf %d: %w", ErrRecordOpen, recordId, leaf, err))
			continue
		}
		// ── R4: WHO SENT IT AND WHAT ROLE THEY HELD, CAPTURED HERE AND NOWHERE ELSE ─────────
		//
		// IT IS ONE ASK AND NOT TWO, which is ledger item 245's first repair and item 242's
		// ruling 24 applied to the OTHER field beside the role. The identity and the role come
		// off the same leaf at the same epoch through the same door, so a build cannot attribute
		// a line to one member and its role to another; and the identity is what a caller
		// attributes BY, because sixteen plaintext octets of sender_handle are a function of the
		// LEAF alone and two occupants of one leaf carry the same ones.
		//
		// THE ORDERING IS THE WHOLE POINT AND IT IS TWO ORDERINGS AT ONCE.
		//
		// FIRST, IT IS AFTER THE OPEN. Above this line `leaf` is a value resolved from
		// header.SenderHandle through walk.leaves, which is keyed on a PLAINTEXT CLAIM any member
		// can write -- [messagegroup.GroupHandle.PeekSender] "authenticates nothing and is never
		// the answer", and a lookup in a table built from handles is the same reading. OpenRecord
		// returning nil is what makes that leaf the SIGNED one: MASTER §8.4.3's R1 refuses any
		// frame whose signing leaf's SenderHandle is not the one the record carries. So the role
		// asked for here is the role of the member that really wrote this, and a role read one
		// clause earlier would be a role read off a forgeable claim (item 242's ruling 24).
		//
		// SECOND, IT IS IN THE SAME LOOP ITERATION AS THE OPEN, and that is measured rather than
		// tidy. RoleAt reads the same handle the open read, so it cannot fail where the open
		// succeeded -- ON THE CONDITION that no epoch INSTALL has run in between. One can:
		// [Group.ingestCommitLocked] is called from this very loop, over a commit row, and the
		// install behind it closes every held past handle and re-makes the schedule map wholesale.
		// A capture deferred to render time, or to the end of this walk, would be asking a
		// question this session may by then have to answer with a fresh load -- which CAN refuse
		// at the window edge where the open did not. Ruling 21 capturing the role AT THE OPEN is
		// exactly what keeps the ask inside that window.
		senderIdentity, senderRoleAtSend := self.senderAtSendLocked(header.Epoch, leaf)
		mine := self.recordIsOwnLocked(senderIdentity, maybeMine)
		sentAtMs, err := decodeHead(headPlain)
		if err != nil {
			fail(recordId, fmt.Errorf("%w: record %d: %w", ErrRecordOpen, recordId, err))
			continue
		}
		// THE CONTENT ENVELOPE, read by the ONE codec both sides of this package use. What this
		// used to do was `Text: string(bodyPlain)` with no branch, which is what headVersion 0x02
		// exists to keep a pre-kinds record away from.
		//
		// THE THREE NON-PARSED ANSWERS ARE THREE DIFFERENT ACCOUNTINGS and that is the whole
		// reason the codec answers a verdict rather than an error:
		//
		//   - MALFORMED and UNSUPPORTED are both GAPS, and they are gaps with two different
		//     [GapReason]s. Neither is a fail(). Both are handled BELOW, with the parsed case,
		//     because a gap is a first-class ENTRY: it keeps its position and its message_id, it
		//     does not count toward [ErrRecordAbandoned], and it becomes one closed placeholder.
		//   - DROPPED is a transient on EPH(0): nothing was persisted, so there is nothing to
		//     render and nothing to be a hole. UNREACHABLE FROM THIS WALK TODAY -- the class skip
		//     above resolves every non-DURABLE record before this point -- and it is here because
		//     the codec can answer it and a reader of this switch should not have to prove that.
		//     IT IS THE ONE VERDICT THIS CHANGE DID NOT TOUCH.
		//
		// WHY MALFORMED IS NO LONGER A fail(), WHICH IS LEDGER ITEM 224 AND IS A BOUNDARY RATHER
		// THAN A PREFERENCE. Everything above this line is a refusal that a RE-FETCH CAN REPAIR:
		// a record that did not parse, a sender_handle that is no leaf of this epoch, a ladder
		// that would not install, an AEAD that would not open, a head this build did not write.
		// Every refusal raised from HERE DOWN is raised AFTER OpenRecord returned -- so the key
		// schedule and the ratchet are already satisfied AND COMMITTED, the signature verified,
		// and what is left in dispute is GRAMMAR. Asking the server for the same octets a second
		// time cannot change the answer, and the old path spent [maxRecordAttempts] fetches
		// finding that out before naming the record in [ErrRecordAbandoned].
		//
		// WHAT THAT COSTS AND WHAT IT BUYS, both stated because the trade is real. It buys the
		// record its POSITION: a malformed body used to hold the cursor for three Receives and
		// then become a hole this build had given up on, and it is now one visible gap the walk
		// moves past. It costs LOUDNESS: [Group.Receive] answered ErrContentMalformed through
		// ErrRecordOpen, and then ErrRecordAbandoned, and it now answers nil. What is left to be
		// loud with is [Stats.GapMalformed] and [Message.Gap], which is why both exist.
		//
		// AND WHY THE ERROR IS NOT KEPT AS WELL, which is the obvious third option and is WRONG:
		// [Group.walkReconcilesLocked] gates the clone check on `walk.firstFailure == nil`, and
		// the cursor is not persisted, so a restored group re-walks its whole history on every
		// launch. A malformed record that set firstFailure would set it on EVERY launch, for
		// ever, and that group would never reconcile -- so [Group.Send] would stay
		// [ErrNotReconciled] permanently. One malformed record from any member would take away
		// every restarted device's ability to send, which is a wedge handed to any member. The
		// bound that keeps the EXISTING fail() path from doing this is abandonment, and a record
		// that is no longer retried never reaches it. A record that opened also contributes its
		// own stream index as clone evidence, which is what firstFailure's presence in that gate
		// is protecting; so a gap is not evidence-poor and has no business in it.
		//
		// THE CODEC'S REFUSAL SENTENCE IS DISCARDED HERE AND THAT IS A NAMED LOSS. It used to
		// reach a caller wrapped in [ErrRecordOpen], and this package has no logger to put it
		// in; spec C §5.1's copy for a gap is one sentence with NO error code, deliberately, so
		// putting it on the [Message] would be an error code by another name. What survives is
		// which reason it was, in [Message.Gap] and in [Stats].
		entry, verdict, _ := ParseContent(bodyPlain, header.RetentionClass, header.EphBucket)
		if verdict == ContentDropped {
			self.stats.SkippedClass += 1
			resolve(recordId)
			continue
		}
		gap := GapReason("")
		switch verdict {
		case ContentMalformed:
			// THE CODEC ANSWERS NO ENTRY FOR A MALFORMED PLAINTEXT and the gap still owes a
			// reader the code the record arrived under, so one is built from octet 0 -- and
			// from NOTHING ELSE, because a body this build refused is a body it must not
			// quote.
			gap = GapMalformed
			self.stats.GapMalformed += 1
			entry = &Content{Kind: contentKindOf(bodyPlain)}
		case ContentUnsupported:
			gap = GapUnsupported
			self.stats.GapUnsupported += 1
		}
		messageId, err := self.session.MessageIdOf(header)
		if err != nil {
			// UNREACHABLE HERE AND CARRIED ANYWAY. MessageIdOf refuses a nil header, a closed
			// session and a record whose group_id is not this session's, and this record has
			// just been OPENED by this session -- both AEADs bound group_id. Measured: deleting
			// this branch leaves ./urmessage and ./cp3b green, so it defends nothing a test can
			// see. It is here because the alternative to a branch is a [Message] with a nil
			// MessageId delivered as though it had one.
			fail(recordId, fmt.Errorf("%w: record %d: its message_id could not be derived: %w", ErrRecordOpen, recordId, err))
			continue
		}
		self.stats.Opened += 1
		if header.Epoch < self.epoch {
			self.stats.OpenedPastEpoch += 1
		}
		// THE STREAM BOOKKEEPING BRANCHES ON THE HANDLE AND THE LINE BRANCHES ON THE IDENTITY,
		// AND THAT IS NOT AN INCONSISTENCY. A stream is named by (group_id, sender_handle) on the
		// server and a receiver ladder is named by the LEAF -- neither carries an identity, and
		// both of the tables below are statements about a stream: "the highest index anything has
		// spent under my own handle" and "the head this leaf's ladder resumes at". A line, by
		// contrast, is a statement about a PERSON, and since item 245 the two questions have two
		// different answers whenever a leaf has changed hands.
		if maybeMine {
			// A RECORD UNDER A HANDLE THIS DEVICE HOLDS, THAT OPENED. This device cannot open
			// what IT sealed (MG-4), so what just opened was sealed by something else holding
			// this leaf's signature key at a generation this device has not spent: a copy of the
			// folder, ahead of this one. noteOwnIndexLocked reads it as exactly that. It is still
			// shown, because it is a message somebody in this group really wrote.
			self.stats.OpenedOwn += 1
			self.noteOwnIndexLocked(walk, recordId, header.StreamIndex, header.BodyHash)
		} else {
			// A PEER RECORD THAT OPENED, so its stream index is one this group's keys
			// authenticated: raise the head this peer's ladder will be re-tracked at after the
			// next epoch change. See [Group.notePeerHeadLocked] and [Group.crossEpochLadderLocked].
			self.notePeerHeadLocked(leaf, header)
		}
		var received *Message
		if gap == "" {
			received = newMessage(entry, recordId, header.SenderHandle[:], senderIdentity, mine,
				sentAtMs, messageId[:], senderRoleAtSend)
		} else {
			received = newGap(gap, entry, recordId, header.SenderHandle[:], senderIdentity, mine,
				sentAtMs, messageId[:], senderRoleAtSend)
		}
		// WHAT A RECORD BECOMES IS ONE DECISION AND IT IS TAKEN IN ONE PLACE. A reaction, a
		// tombstone and a COVER are records that add no line, and [Group.deliverLocked] is what
		// says so -- for this walk, for the own-copy path and for [Group.Send] alike, because
		// three sites that each decided it would be three sites to keep level.
		if self.deliverLocked(received, entry) {
			walk.opened = append(walk.opened, received)
		}
		self.delivered[recordId] = true
		resolve(recordId)
	}
}

// noteOwnIndexLocked accounts for one record that opened under this device's own sender_handle.
//
// THE FOUR READINGS, AND THEY ARE NOT THE SAME EVENT.
//
//   - THIS RECORD, AT AN INDEX THIS PROCESS SEALED IT AT. The ordinary case, and it includes a
//     record whose submit response never arrived: [Group.Send] notes index and body_hash at the
//     SEAL, so the record comes back as this device's own rather than as a stranger holding its
//     keys.
//   - AN INDEX FOUND DURING THE RECONCILING WALK of a restored group. This is this lineage's own
//     history and it is absorbed; commitWalkLocked holds the maximum of it against the reserver
//     afterwards, which is the check that needs the whole walk rather than one record.
//   - A DIFFERENT RECORD AT AN INDEX THIS DEVICE SEALED AT. This is the hard case and it is the
//     one an index-only check could not see: two copies of one folder that were EXACTLY level
//     both seal at this index, one submission wins, and the loser finds the winner's record here.
//     Conclusive: this device knows what it sealed at this index and this is not it.
//   - AN INDEX THIS DEVICE NEVER SEALED, after this group has reconciled. A copy that is ahead.
//
// WHY THE BODY HASH CAN BE TRUSTED HERE: this runs only after the record OPENED, and §3.1's
// body_hash is inside aad_head and is compared against the ciphertext before either AEAD runs. A
// party without this group's keys cannot produce a record that opens at all, let alone one whose
// body_hash it chose.
func (self *Group) noteOwnIndexLocked(walk *pageWalk, recordId uint64, index uint64, bodyHash [32]byte) {
	if self.ownIndexSeen < index {
		self.ownIndexSeen = index
	}
	sealed, mine := self.ownIndices[index]
	switch {
	case mine && sealed.bodyHash == bodyHash:
	case mine:
		if walk.foreignIndex == 0 {
			walk.foreignIndex, walk.foreignRecord = index, recordId
			walk.foreignBody = true
		}
	case !walk.reconciled:
		self.ownIndices[index] = &ownSealed{bodyHash: bodyHash}
	case walk.foreignIndex == 0:
		walk.foreignIndex, walk.foreignRecord = index, recordId
	}
}

// openOwnFromCopyLocked shows one record of this device's own from the copy [Group.Send] kept, and
// answers whether it did.
//
// WHY THERE IS A COPY TO SHOW IT FROM. Since connect 4c030dc an application record's body is an MLS
// PrivateMessage, and a member cannot open its own: Protect spends a generation of this leaf's own
// ratchet and MLS keeps no receiving ratchet for a leaf's own messages, so OpenRecord of a record
// this device sealed answers "mls: ratchet generation already consumed". That is connect's
// messagegroup OPENITEMS MG-4, and of the three answers it lists this is the first -- a sender
// renders its own message from the copy it kept -- because the second is a change to connect and
// the third, exempting self-attributed records from the inner open, re-opens the forgery 4c030dc
// closed and is written down there as refused.
//
// WHAT HAS TO HOLD BEFORE A COPY IS SHOWN, and why each clause is there:
//
//   - THIS DEVICE SEALED AT THIS INDEX AND KEPT WHAT IT SEALED. An index it only learned from the
//     server has no copy and is not shown from one.
//   - THE RECORD'S body_hash IS THE ONE SEALED THERE. Two copies of one folder both seal at an
//     index; the one whose record the server kept is not this device's, and it is not shown as
//     this device's text. It falls through to OpenRecord, which is what authenticates it and what
//     lets the clone check read it.
//   - THE HASH IS THE HASH OF THIS ct_body, and the group and epoch are this group's. A header is
//     plaintext; checking the hash against the ciphertext in hand is what ties the claim to
//     octets this device produced, because nobody can produce a second ct_body under one SHA-256.
//     What is NOT checked is ct_head, and nothing is lost by it: the head this device wrote is
//     the clock reading it kept, and it is shown from the copy.
//   - THE COPY HAS NOT ALREADY BEEN SHOWN UNDER ANOTHER RECORD ID. An honest server numbers one
//     record once -- an honest resubmission is answered with the record id it already holds -- so
//     a second number for the same (index, body_hash) is one record shown twice. It is refused as
//     a record that did not open rather than delivered again, which is what the receiver ladder
//     used to do for it when this device could still open its own records.
//
// A record that fails any of the first three is not an error here: it answers false, and the
// ordinary open path decides what it is.
func (self *Group) openOwnFromCopyLocked(walk *pageWalk, recordId uint64, parsed *message.Record) (bool, error) {
	header := &parsed.Header
	sealed, found := self.ownIndices[header.StreamIndex]
	if !found || !sealed.hasCopy || sealed.bodyHash != header.BodyHash {
		return false, nil
	}
	// THE GROUP AND EPOCH HALF OF THIS DEFENDS NOTHING A TEST CAN SEE, measured by deleting it over
	// urmessage and cp3b with nothing going red, and that is argued rather than hoped: a ct_body this
	// device sealed in another group or epoch hashes to a body_hash no copy in THIS group's table
	// holds, so the hash half already refuses it. It stays because it is free and says what a copy
	// is for.
	//
	// AND SINCE LEDGER ITEM 241 THE EPOCH HALF ADMITS A PRIOR EPOCH. A restarted device re-walks
	// its own lines from epochs it has left, and a copy path that refused them sent each one down
	// the open road to be authenticated at the spent generation and counted as a line this device
	// cannot show -- while holding the copy. The hash half is what identifies the copy; the epoch
	// half refuses only what no schedule could reach, a record from the FUTURE.
	if sha256.Sum256(parsed.CtBody) != header.BodyHash || !bytes.Equal(header.GroupId[:], self.id) ||
		self.epoch < header.Epoch {
		return false, nil
	}
	if sealed.recordId != 0 && sealed.recordId != recordId {
		return false, fmt.Errorf("%w: record %d carries this device's own record at stream index %d, which this group already holds as record %d",
			ErrRecordOpen, recordId, header.StreamIndex, sealed.recordId)
	}
	// THE ID IS DERIVED FROM THE SERVER'S RECORD AND NOT FROM THE COPY, which is the only reason
	// this line is above the two counters rather than inside the literal below. The copy carries
	// the TEXT and the clock reading; the three inputs to message_id are header fields, and the
	// header in hand is the one whose body_hash has just been checked against the ciphertext. So
	// the id this device shows for its own message is computed from the same octets every other
	// member computes it from, and a restarted device that shows a line from its copy names it
	// the same way the group does.
	//
	// THE ERROR BRANCH DEFENDS NOTHING A TEST HERE CAN SEE, and it is the same unreachable clause
	// the ordinary open path carries, measured the same way: deleting it leaves ./urmessage and
	// ./cp3b green. MessageIdOf refuses a nil header, a closed session and a record whose group_id
	// is not this session's, and the clauses above have already compared this record's group and
	// epoch against this group's. It is kept because the alternative to a branch is a [Message]
	// delivered with a nil MessageId and nothing saying so.
	// THE COPY IS AN APPLICATION PLAINTEXT AND IS READ BY THE SAME CODEC EVERY OTHER MEMBER READS
	// THIS RECORD WITH. [SentRecord.Body] is "what was sealed, octets, never interpreted", and what
	// [Group.Send] seals is `kind ‖ body` -- so this device's own reply, its own reaction and its
	// own tombstone come back through this path with the same meaning the group gives them, rather
	// than as a line of text that happens to start with an 0x02.
	//
	// A COPY THIS BUILD CANNOT READ IS NOT SHOWN AND IS NOT A SECOND KIND OF SILENCE. The one
	// population that reaches it is a state directory written by a PRE-KINDS build, whose copies
	// are raw text with no code, so the first octet of somebody's sentence is read as a kind.
	//
	// WHAT THAT COSTS, MEASURED OVER THE WHOLE FIRST-OCTET SPACE ON DURABLE AND NOT REASONED. Of
	// the 95 printable ASCII characters, 63 ARE MALFORMED and 32 render as a placeholder:
	//
	//	MALFORMED   0x40..0x7E -- '@', EVERY LETTER, and [ \ ] ^ _ ` { | } ~
	//	PLACEHOLDER 0x20..0x3F -- space, the punctuation of the first column, and the digits
	//
	// The 63 are malformed because 0x40..0x7F is the TRANSIENT range, which is legal on EPH(0) and
	// on no stored class, and a transient code on DURABLE is a rule the code alone decides. THAT IS
	// EVERY ENGLISH SENTENCE: a line beginning "h" is kind 0x68, which is 104, which is inside that
	// range -- not an unassigned code, and not a placeholder. This comment said the opposite and
	// said it for four reviews; the split is now a number that
	// TestTheMeasuredSplitOfAPrintableFirstOctetOnADurableCopy re-measures.
	//
	// SO THE COMMON CASE IS THE FALL-THROUGH, WHICH IS THE HONEST ONE. A copy that is malformed
	// under the codec is not shown here at all: it falls to the ordinary path, where it is
	// authenticated as this device's own and counted in [Stats.OwnWithoutCopy] -- a hole this device
	// NAMES, which is what "this build cannot read what it wrote" honestly is. The 32 that do render
	// render as one closed placeholder under the unknown-kind rule, which is the same answer every
	// other member's build gives an unknown code.
	//
	// AND NO OLD LINE IS EVER SILENTLY REINTERPRETED, which is the half of this that would have been
	// the real failure. The six codes this build has a grammar for are 0x01, 0x02, 0x04, 0x05, 0x06
	// and 0x07 -- all non-printable -- so NO printable-ASCII first character parses as a known kind
	// AT ANY LENGTH, swept and not argued. A pre-kinds line can be a named hole or a placeholder; it
	// cannot come back as a reply, a tombstone or a reaction.
	//
	// The local copy has no version byte of its own to refuse on, and buying one would cost the
	// state store's single version lever -- which every OTHER record in the directory, the device
	// identity included, is read under.
	entry, verdict, _ := ParseContent(sealed.body, header.RetentionClass, header.EphBucket)
	if verdict == ContentMalformed || verdict == ContentDropped {
		return false, nil
	}
	messageId, err := self.session.MessageIdOf(header)
	if err != nil {
		return false, fmt.Errorf("%w: record %d is this device's own at stream index %d and its message_id could not be derived: %w",
			ErrRecordOpen, recordId, header.StreamIndex, err)
	}
	sealed.recordId = recordId
	self.stats.OpenedOwn += 1
	self.noteOwnIndexLocked(walk, recordId, header.StreamIndex, header.BodyHash)
	// AND THE PLACEHOLDER HALF OF THE SPLIT ABOVE IS A GAP LIKE ANY OTHER. The 32 printable first
	// octets that answer [ContentUnsupported] reach a caller from HERE, not from the walk, and if
	// this site alone left [Message.Gap] empty then this device's own pre-kinds line would be the one
	// blank message in a build that has no others -- the exact failure this whole change is for,
	// surviving at the one call site nobody was looking at. The malformed 63 never reach this line:
	// they answered false above and are counted in [Stats.OwnWithoutCopy] by the ordinary path.
	// R4: THE ROLE ON THIS ROAD IS THIS DEVICE'S OWN, AT THE RECORD'S OWN EPOCH. There is no
	// authenticated leaf to read here because there is no open here -- what identifies the record
	// is that its body_hash is one THIS device sealed, over this group, and the leaf that follows
	// from that is [messagegroup.GroupHandle.OwnLeafIndex] and not anything off the header. A
	// restarted device re-walks its own lines from epochs it has left, so the epoch asked for is
	// the header's and never this group's: a line this device wrote as a MEMBER before being made
	// an admin still says "member", exactly as every peer's copy of it does.
	//
	// AND THE IDENTITY ON THIS ROAD IS THIS DEVICE'S OWN, FOR A STRONGER REASON THAN THE ROLE'S.
	// The role is a fact about a LEAF at an epoch and is asked of the tree; the identity is a fact
	// about WHO, and this road's whole premise is that the record's body_hash is one this device
	// sealed and kept -- which is a statement about this device and not about a leaf. Reading it
	// off the tree would also make it empty exactly where the epoch has aged out, on the one road
	// whose records this device is certain of.
	senderRoleAtSend := self.roleAtSendLocked(header.Epoch, self.handle.OwnLeafIndex())
	received := newMessage(entry, recordId, header.SenderHandle[:], self.device.identityPub, true,
		sealed.sentAtMs, messageId[:], senderRoleAtSend)
	if verdict == ContentUnsupported {
		self.stats.GapUnsupported += 1
		received = newGap(GapUnsupported, entry, recordId, header.SenderHandle[:],
			self.device.identityPub, true, sealed.sentAtMs, messageId[:], senderRoleAtSend)
	}
	if self.deliverLocked(received, entry) {
		walk.opened = append(walk.opened, received)
	}
	self.delivered[recordId] = true
	return true, nil
}

// ── ingesting a commit: §6.1's membership change on the receiving side (A5) ───────────────────

// CommitMember is one member of a group as it stands on one side of a commit: the leaf a role is
// read at, the sender_handle that names it on the wire, the credential identity the leaf carries,
// and the role the policy on that side gives that identity.
type CommitMember struct {
	// Leaf is the member's leaf index in the ratchet tree.
	Leaf uint32

	// SenderHandle is [messagegroup.SenderHandle] for this leaf: the 16 octets its records carry.
	// group_handle_key is the epoch-zero expansion and never moves, so a post-commit leaf's handle
	// is as derivable as a pre-commit one's. A copy.
	SenderHandle []byte

	// IdentityPub is the credential identity this leaf carries -- the member's Ed25519 identity
	// public key, which is what urmessage_group_policy keys a role by (MASTER §6) and, in this
	// build, the leaf's own signer ([NewDevice]). A copy.
	IdentityPub []byte

	// Role is the role the policy on this side of the commit gives IdentityPub, as the mls
	// Role.String() name: "owner", "admin", "member" or "observer". An identity the policy does
	// not name is "member" (MASTER §11, ruling 8). For [CommitAuthorization.Members] it is read off
	// the PRE-commit policy; for [CommitAuthorization.MembersAfter] off the post-commit one.
	Role string

	// HasLeafKeys is whether this leaf carries a urmessage_leaf_keys extension (0xF002) an epoch
	// wrap can reach. For [CommitAuthorization.MembersAfter] it is the seam's own reading off the
	// staged tree ([messagegroup.ProcessedMember.HasLeafKeys]); for [CommitAuthorization.Members]
	// it is always true, because the membership door that builds that side, MemberAt, refuses a
	// keyless leaf outright rather than reporting it. R6d refuses a commit whose post-commit tree
	// holds a leaf with false here.
	HasLeafKeys bool
}

// CommitAuthorization is everything a receiving client's authorization decision is handed about one
// ingested commit, BEFORE it is applied.
//
// IT IS THE RECEIVING ARM OF MASTER §11. §11 rules that a bad commit "is refused by the committing
// client, and is rejected by every receiving client on validation," and the receiving-client arm is
// the commit-ingest path: [authorizeCommit] reads this value there, before ApplyCommit, on every
// commit. Everything the rules need is on this struct -- who committed and with what role, what the
// commit does to the leaves and to their identities, and the policy and the whole extension list on
// both sides of the commit -- so the rule function is pure and is tested without a device.
type CommitAuthorization struct {
	// GroupId is the 32-octet group this commit is in. A copy.
	GroupId []byte

	// Epoch is the epoch this commit OPENS -- the current epoch plus one. The membership below and
	// the committer are as they stood at the epoch that is closing, which is where a role is read.
	Epoch uint64

	// CommitterLeaf is the AUTHENTICATED leaf that authored the commit: the commit's signature has
	// been verified against this leaf by [messagegroup.GroupHandle.Process] before this value is
	// built.
	CommitterLeaf uint32

	// CommitterIdentity is the credential identity CommitterLeaf carried in the PRE-commit tree --
	// the identity the signature was verified against -- and CommitterRole is the role the
	// pre-commit policy gives it. Every proposal the commit carries, by value or by reference, is
	// judged against this role and never against a proposer's (ruling 3). The committer's own path
	// can rewrite its leaf's identity and mls accepts it, so the rules compare this against the
	// identity MembersAfter holds at CommitterLeaf (R6c).
	CommitterIdentity []byte
	CommitterRole     string

	// AddedLeaves, RemovedLeaves and UpdatedLeaves are where the commit's proposals landed, off the
	// staged commit rather than off any header.
	AddedLeaves   []uint32
	RemovedLeaves []uint32
	UpdatedLeaves []uint32

	// Members is the membership as it stands BEFORE the commit is applied, with each identity and
	// its role under PolicyBefore. It is the pre-commit tree because the decision is taken before
	// ApplyCommit, which is the only order under which a commit that removes the owner can be
	// refused by reading the owner's role.
	Members []CommitMember

	// MembersAfter is every occupied leaf of the tree the commit ENTERS, with its identity and its
	// role under PolicyAfter -- read off the staged commit's own tree and never off a membership
	// diff. The added identities, identity continuity, the phantom check and the caps are all
	// computed from it.
	MembersAfter []CommitMember

	// PolicyBefore and PolicyAfter are the decoded urmessage_group_policy (0xF001) of the group
	// context on each side of the commit, parsed and validated through mls.GroupPolicyOf. Each is
	// nil when that side carries no policy or one that does not parse or validate, and the matching
	// error field says why (mls.ErrNoGroupPolicy for the absence). A nil PolicyBefore reads every
	// member as unnamed -- MEMBER -- and so lets nobody do anything but their own device changes;
	// a nil PolicyAfter is refused outright (R0a).
	PolicyBefore    *mls.GroupPolicyExtension
	PolicyAfter     *mls.GroupPolicyExtension
	PolicyBeforeErr error
	PolicyAfterErr  error

	// ExtensionsBefore and ExtensionsAfter are the FULL group context extension lists on each side
	// of the commit, in list order, as the seam spells them. A policy commit may touch 0xF001 and
	// nothing else (R0b): 0x0003 required_capabilities above all must be byte identical.
	ExtensionsBefore []messagegroup.ExtensionBytes
	ExtensionsAfter  []messagegroup.ExtensionBytes
}

// CommitAuthorizer is a device's ADDITIONAL receiving-client decision on an ingested commit. It
// returns nil to allow, or an error to REFUSE -- which [Group.Receive] surfaces as
// [ErrCommitUnauthorized] with the returned cause carried, and which leaves this group at the epoch
// it was already at.
//
// IT RUNS AFTER THE ROLE MODEL'S OWN RULES AND MAY ONLY REFUSE MORE. Since ledger item 242's R1
// [authorizeCommit] runs on every ingested commit whether or not a device configures one of these,
// and a commit the rules refuse is refused before this is asked; a configured authorizer that
// answers nil allows nothing the rules do not. Nil here therefore no longer means "allow every
// commit" -- it means "nothing beyond MASTER §11". It is a function value rather than a method on
// an interface so that a product's extra rule is a config field and not a new seam.
type CommitAuthorizer func(*CommitAuthorization) error

// ingestCommitLocked follows one received commit into the epoch it opens: the whole of A5, in the
// order A5's plan fixes and in one place.
//
//	OpenCeremonyRecord -> Process -> authorization (the rules, then the hook) -> ApplyCommit ->
//	AdvanceEpoch -> A4's re-track -> enterEpochLocked (A3's persist)
//
// THE ORDER IS NOT INTERCHANGEABLE. The authorization is BEFORE ApplyCommit because a decision
// taken after the commit is applied cannot refuse a commit that removes the owner. ApplyCommit is BEFORE
// AdvanceEpoch because the session installs the epoch the HANDLE is at, so the handle must have
// moved first -- and mls persists the new epoch's MLS state inside ApplyCommit, which is the first
// of the two writers of epoch state. AdvanceEpoch reuses this group's LIFETIME pq_secret (item 243):
// the same value re-extracts the new epoch's storage root, which is why item 243 was a prerequisite.
// crossEpochLadderLocked (A4) runs in the same block as that install, and enterEpochLocked (A3) is
// LAST -- the second writer -- so a persist that named the new epoch never outruns the ladders that
// serve it.
//
// THE RECEIVER HOLDS THE GROUP MUTEX ACROSS THIS. Nothing here re-enters it: the handle and the
// session run their own loops, and the persist is the durable store's own lock. What it can block on
// is bounded -- one commit's worth of MLS work and one disk write -- so it does not stall the walk.
func (self *Group) ingestCommitLocked(walk *pageWalk, parsed *message.Record) (err error) {
	header := &parsed.Header
	// (0) track the committer's ladder at the commit's own retention class, so the ceremony open
	// below has a receiver ratchet to peek. A commit is a PERMANENT record and this device may only
	// ever have tracked this sender's DURABLE ladder (its ordinary messages), so the class the
	// commit rides is one no ordinary record installed. The committer is a member of this group at
	// the epoch that is closing, so its sender_handle is a leaf of walk.leaves; a record whose
	// committer is not is refused rather than opened. The ceremony arm commits no ratchet, so this
	// peek costs nothing this device's own next record needs.
	leaves, err := self.walkLeavesLocked(walk, header.Epoch)
	if err != nil {
		return fmt.Errorf("%w: the membership at epoch %d: %w", ErrCommitIngest, header.Epoch, err)
	}
	committerLeaf, known := leaves[header.SenderHandle]
	if !known {
		return fmt.Errorf("%w: the commit names sender_handle %x, which is no leaf of this group at epoch %d",
			ErrCommitIngest, header.SenderHandle, header.Epoch)
	}
	// (0a) THE EPOCH DIGEST, READ OFF THE RECORD BEFORE ANYTHING IS APPLIED. It is the only thing
	// in this system that can say which pq_secret the epoch this commit opens actually runs on,
	// and it arrives already authenticated twice over: LP(H(server_attachment)) is inside AAD_head
	// and inside the write_auth preimage, so a bent digest is a record that does not open and a
	// bent key is a digest that does not match. Parsed here rather than at (4a) so that a commit
	// carrying no digest attachment at all is named as the thing it is before the group has moved.
	//
	// A KIND 0x0001 COMMIT ANSWERS nil AND THAT IS NOT AN ERROR YET. Spec B section 5.4's
	// acceptance window is dated and open, so a commit sealed before ruling 27 is a record this
	// build can still meet; it carries its epoch keys in the clear instead of a digest and nothing
	// here can bind a wrap to it. Such a commit is followed on the compatibility path -- the
	// secret this group already holds -- and the refusal below fires only if that path has been
	// refuted, which is [Group.resolvePqSecretLocked]'s own arm.
	commitDigest, err := epochDigestOf(header)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCommitIngest, err)
	}
	if err := self.trackLocked(committerLeaf, header); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitIngest, err)
	}
	// (1) open the ceremony record. It authenticates NOBODY -- the body is judged by mls below,
	// not by the sender_handle the record claims. The epoch check inside it has already been
	// satisfied by the caller's guard (header.Epoch == self.epoch).
	_, commitBytes, err := self.session.OpenCeremonyRecord(parsed)
	if err != nil {
		return fmt.Errorf("%w: opening the commit ceremony record: %w", ErrCommitIngest, err)
	}
	// (2) process the commit against this member's own tree, staging it. This is where the
	// commit's signature is verified against its committer and where its proposals are resolved.
	processed, err := self.handle.Process(commitBytes)
	if err != nil {
		return fmt.Errorf("%w: processing the commit: %w", ErrCommitIngest, err)
	}
	// THE STAGED EPOCH IS ERASED ON EVERY EXIT BUT THE INSTALL, through the seam's own door. A
	// processed commit is a fully derived second epoch -- key schedule, secret tree, leaf private
	// state -- and a refusal below, or an ApplyCommit that fails, would otherwise leave it in the
	// heap for the collector to move around. DiscardProcessed after a SUCCESSFUL ApplyCommit is a
	// no-op that answers nil (the seam detaches the staged half on the install), which is what
	// lets this be one deferred call rather than a discard on each of the exits. Its own error is
	// surfaced only when nothing else is: a refusal is the sentence a caller needs, and the erase
	// is what it costs. Held at RUNTIME, with the seam's three doors counted over real devices,
	// by TestEveryStagedEpochTheIngestPathDoesNotInstallIsErasedThroughTheSeam -- a source pin
	// over this function's statement positions stood here before it and passed a return placed
	// in the else branch of the Process check above.
	defer func() {
		if discardErr := self.handle.DiscardProcessed(processed); discardErr != nil && err == nil {
			err = fmt.Errorf("%w: erasing the staged epoch: %w", ErrCommitIngest, discardErr)
		}
	}()
	if processed.Kind != messagegroup.EngineProcessedCommit {
		return fmt.Errorf("%w: a record marked is_commit processed as kind %d rather than a commit",
			ErrCommitIngest, processed.Kind)
	}
	// (3) THE AUTHORIZATION, before anything is applied: MASTER §11's rules on every commit, then
	// this device's own hook. A refusal is counted, the staged epoch is erased by the defer above,
	// and this group stays at the epoch it was at -- the commit is neither applied nor crashed on.
	//
	// THE DECISION IS CARRIED OUT OF THIS CALL RATHER THAN REBUILT, because (3a) and (4a) below
	// both read what the commit REMOVES and a second read off `processed` would be a second
	// source for one fact about one commit. Both arms of one predicate must not disagree.
	decision, err := self.authorizeCommitLocked(processed)
	if err != nil {
		self.stats.CommitRefused += 1
		return err
	}
	// (3a) AND THE REMOVAL RULE ON WHAT THE RECORD ITSELF SAYS, STILL BEFORE ApplyCommit — RULING
	// 41. A commit that removes a leaf and carries NO epoch digest could only ever be followed on
	// a pq_secret this device already holds -- there is no authenticator for anything it delivers
	// -- which makes it an INVALID commit, and an invalid commit is refused the way an
	// unauthorized one is: counted, not applied, and the group stays at the epoch it is at. That
	// is the shape a client on an OLDER BUILD emits, and refusing it here is what stops such a
	// client bricking every up-to-date member by removing somebody. Every removal that DOES carry
	// a digest is let past to (4b), where the digest can be asked --
	// [Group.refuseUnrotatedRemovalLocked] carries why a set of staged wrap candidates is not
	// evidence about a commit, and what moving that decision one epoch later costs.
	if err := self.refuseUnrotatedRemovalLocked(commitDigest, decision.RemovedLeaves); err != nil {
		self.stats.CommitRefused += 1
		return self.haltLocked(err)
	}
	// (4) apply it: the handle enters the epoch the commit opens, and mls persists that epoch's
	// state HERE -- the first of the two writers of epoch state.
	//
	// AND THE ONE FAILURE HERE THAT IS NOT A FAILURE: RULING 52. mls.ErrRemovedFromGroup is what
	// [mls.Group.ApplyCommit] answers when the commit it just validated removed THIS client's own
	// leaf -- a valid commit, correctly signed, authorized by every §11 rule at step (3), which
	// this device cannot enter because it is not in the tree the commit produced. Naming it as
	// "could not follow a commit" is naming the wrong subject: nothing failed, the membership
	// ended. [Group.removedLocked] takes it, and it is taken HERE rather than at the walk because
	// this is the one place the answer is still mls's own: mls has closed the group and zeroized
	// its epoch secrets by the time this returns, so a second ask answers `the group is closed`
	// and the fact is gone for the rest of the process.
	//
	// THE STAGED EPOCH IS STILL ERASED, by the defer above, which runs on this return like any
	// other. DiscardProcessed after a failed ApplyCommit is the arm that door exists for.
	if err := self.handle.ApplyCommit(processed); err != nil {
		if errors.Is(err, mls.ErrRemovedFromGroup) {
			return self.removedLocked(err)
		}
		return fmt.Errorf("%w: applying the commit: %w", ErrCommitIngest, err)
	}
	newEpoch := self.handle.Epoch()
	// (4a) WHICH pq_secret THIS EPOCH RUNS ON, decided against the commit's own authenticated
	// H(epoch_keys) and against nothing else. This is item 243's receive leg and item 132's
	// detector in one call: the candidates are the wraps this device opened for this epoch --
	// staged at (0a) below, because ruling 37 puts them on the wire AHEAD of the commit -- plus
	// the secret this group already holds, which is the arm every group built before rotation
	// takes. [Group.resolvePqSecretLocked] carries the argument and names the three failures.
	//
	// IT IS AFTER ApplyCommit BECAUSE IT NEEDS mls_secret[n+1], which is the exporter of an epoch
	// the handle has to be standing in: the seam's PendingExport reads a handle's OWN staged
	// commit and there is no exporter over a PROCESSED one. So the applied commit is not undone on
	// a miss -- the epoch is open, the membership has changed, and pretending otherwise would
	// leave the handle at n+1 and this group at n. What a miss costs is said out loud instead.
	newMlsSecret, err := self.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		return fmt.Errorf("%w: the exporter at epoch %d: %w", ErrCommitIngest, newEpoch, err)
	}
	pqNext, resolveErr := self.resolvePqSecretLocked(newMlsSecret, newEpoch, commitDigest, decision.RemovedLeaves)
	zeroizeState(newMlsSecret)
	// (4b) THE ONE REFUSAL THAT IS NOT A DARK STATE, WHICH IS RULING 41 REACHING AS FAR AS IT CAN
	// FROM HERE. An unrotated removal is an INVALID commit, and the answer to an invalid commit is
	// to not follow it -- not to advance into a permanent brick on a commit just judged invalid.
	// (3a) takes this decision before the apply for the one shape the RECORD itself settles, a
	// removal carrying no digest at all; everything else reaches here, because judging it needs the
	// commit's own digest and therefore mls_secret[n+1] -- and the most this point can still do is the
	// rest of ruling 41's outcome: the epoch field does not move, no pq_secret is filed for it, the
	// session is not advanced and [Group.wrapDark] is NOT set. [Group.haltLocked] is what runs
	// instead, and it is the same call (3a) makes, so the two refusal sites produce ONE state and
	// not two spellings of one. The group is HALTED at the epoch it is at, with its session and its
	// record agreeing with each other there, and the next process reads the halt off that record
	// rather than re-deriving it -- which it cannot do, because step (0) above has already consumed
	// the committer's ratchet generation by the time this line is reached and a second walk over the
	// same record answers `ratchet generation already consumed` instead. What has moved and cannot
	// be moved back is the MLS handle, and that is the residual rather than a claim this is
	// indistinguishable from a pre-apply refusal.
	//
	// IT IS errors.Is AND NOT A SECOND FLAG, so the two outcomes are told apart by the sentinel
	// that already names one of them and there is nothing to keep in agreement.
	if resolveErr != nil && errors.Is(resolveErr, ErrRemovalWithoutRotation) {
		self.stats.CommitRefused += 1
		return self.haltLocked(resolveErr)
	}
	if resolveErr != nil {
		// THE EPOCH STILL MOVES, AND THE GROUP IS MARKED DARK BY NAME. A member with no
		// pq_secret[n+1] is dark at n+1 whatever this function does -- read_key[n+1] and
		// write_key[n+1] both hang off storage_root[n+1] -- so refusing to advance would not make
		// it less dark, it would make it dark AND leave the handle one epoch ahead of the session
		// and of the persisted record. The last secret this device holds is used so the three stay
		// in step, and [Group.wrapDark] is what every later refusal says instead of the AEAD tag.
		pqNext = self.pqSecretLocked()
		if self.wrapDark == nil {
			self.wrapDark = resolveErr
			self.wrapDarkEpoch = newEpoch
		}
	}
	self.filePqSecretLocked(newEpoch, pqNext)
	// (5) advance the session onto the epoch the handle is now at, with the epoch's own secret.
	// The table above and this call take ONE value: connect's AdvanceEpoch refuses a differing one
	// at an epoch already filed ([messagegroup.ErrPqSecretEpochConflict]), which is the refusal
	// that replaced the silent destruction of exactly this wrap's secret.
	if err := self.session.AdvanceEpoch(pqNext); err != nil {
		return fmt.Errorf("%w: advancing the session to epoch %d: %w", ErrCommitIngest, newEpoch, err)
	}
	// Every candidate for this epoch and below has been judged; what is left is orphan material.
	self.dropWrapCandidatesLocked(newEpoch)
	// (5a) WHAT THIS COMMIT TOOK OUT OF THE GROUP, FILED AND PRUNED -- LEDGER ITEM 245's SECOND AND
	// THIRD PIECES, AND THE ORDER OF THESE TWO LINES AGAINST (6) IS THE WHOLE OF THE SECOND.
	//
	// The prune MUST run before [Group.crossEpochLadderLocked], because that function re-tracks a
	// ladder for every entry of [Group.peerHeads] at the NEW epoch -- so a head left behind here is
	// a ladder installed for a leaf that no longer stands in this group, positioned at the removed
	// member's last index. RFC 9420 §7.7 then refills that very leaf with the next Add, [ladderKey]
	// is keyed on the LEAF, and the newcomer's first record -- at stream index 1 of a stream that
	// starts here -- meets a receiver ratchet standing at somebody else's head and is refused,
	// silently, for as long as it takes the newcomer to write past a history it had no part in.
	//
	// The filing, by contrast, is about what the removed member ALREADY WROTE: its records sit
	// BELOW this commit in record order and are the whole of its half of this conversation, and
	// without the table [Group.leavesAtLocked] cannot resolve the handle they carry once the leaf
	// is out of the membership. Both survive a restart, in part nine of [GroupRecord], because the
	// cursor does not and every restart re-walks all of it.
	self.noteDepartedLeavesLocked(decision.RemovedLeaves, newEpoch)
	self.pruneRemovedLaddersLocked(decision.RemovedLeaves)
	// (5b) AND THE HANDLE THIS DEVICE SEALS UNDER, RE-READ AT THE NEW EPOCH. On the ordinary path
	// it is the same sixteen octets and the set does not grow; the one shape it catches is a leaf
	// that moved under this device, which nothing in RFC 9420 does to a standing member but which
	// a re-Add of this device to a group it was removed from does.
	self.noteOwnLeafLocked()
	// (6) A4: the ladder bookkeeping crosses the epoch here, in the same block as the install above.
	if err := self.crossEpochLadderLocked(newEpoch); err != nil {
		return err
	}
	// (7) A3: persist the new epoch through the one door -- the second writer, after mls.
	if err := self.enterEpochLocked(); err != nil {
		return err
	}
	// (8) THE MEMBERSHIP HAS CHANGED, AND THE WALK IS STILL RUNNING. The walk's tables were built
	// from the membership as it stood before this commit, so a record from a member this commit
	// ADDED -- whose leaf did not exist then -- would fail "no leaf of this group" if it arrives
	// later in the same page.
	//
	// WHAT IS DROPPED IS THE ENTRY FOR THIS EPOCH AND UP, AND NOT THE WHOLE CACHE, which is ledger
	// item 245's third piece meeting this step. A commit changes who stands in the group FROM the
	// epoch it opens; the tables for the epochs BELOW it are statements about membership that has
	// already happened and that this commit cannot move, and they are exactly the tables the
	// removed member's own records are about to be resolved through. Clearing all of them would
	// rebuild them identically at a cost; clearing none would resolve the new member's records
	// against a table that has never heard of it.
	for epoch := range walk.leaves {
		if newEpoch <= epoch {
			delete(walk.leaves, epoch)
		}
	}
	self.stats.Ingested += 1
	// AND THE DIAGNOSIS IS RETURNED LAST, after everything this function CAN do has been done. It
	// is returned rather than swallowed because a member that followed a commit into an epoch it
	// holds no secret for has not really followed it, and the caller's own error channel is where
	// the first sentence about it belongs. [Group.wrapDark] is the sticky copy, because this one
	// is said once and the state is permanent for this process.
	if resolveErr != nil {
		return resolveErr
	}
	return nil
}

// authorizeCommitLocked takes the receiving-client decision on one processed commit. It is A5's
// hook, and it runs BEFORE ApplyCommit.
//
// DEFAULT ON, AND NOTHING CAN LOOSEN IT. [authorizeCommit] -- MASTER §11's rules, ledger item
// 242's R1 -- runs on every commit this group ingests, whether or not the device configured a
// [CommitAuthorizer]. A configured one runs AFTER the rules, over the same [CommitAuthorization],
// and may only refuse more: it is never asked about a commit the rules refused, and nil from it
// allows nothing the rules did not. Before R1 a nil authorizer allowed every commit and the
// membership was not even read; that arm is gone.
//
// A refusal, from either, is surfaced as [ErrCommitUnauthorized] with the rule or the hook's own
// cause carried, so a caller can errors.Is both.
// IT ANSWERS THE DECISION IT TOOK, and the caller reads what the commit removes off THAT value
// rather than off `processed` a second time. The two would agree today -- one is a clone of the
// other -- which is exactly why a second read is the kind of thing that stops agreeing later.
func (self *Group) authorizeCommitLocked(processed *messagegroup.EngineProcessed) (*CommitAuthorization, error) {
	decision, err := self.commitAuthorizationLocked(processed)
	if err != nil {
		return nil, fmt.Errorf("%w: the inputs the authorization check reads: %w", ErrCommitIngest, err)
	}
	if err := authorizeCommit(decision); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCommitUnauthorized, err)
	}
	if authorizer := self.device.commitAuthorizer; authorizer != nil {
		if err := authorizer(decision); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCommitUnauthorized, err)
		}
	}
	return decision, nil
}

// commitAuthorizationLocked builds the [CommitAuthorization] for one processed commit: the
// committer and what the commit does off [messagegroup.EngineProcessed] (authenticated by
// Process), the PRE-commit membership and group context off the handle, and the POST-commit
// membership and extension list off the staged value the seam reported.
//
// Every slice is this value's own -- cloned out of the seam's answer or freshly derived -- so a
// configured authorizer that keeps the value keeps nothing that aliases the epoch ApplyCommit is
// about to enter.
func (self *Group) commitAuthorizationLocked(processed *messagegroup.EngineProcessed) (*CommitAuthorization, error) {
	extensionsBefore, err := self.contextExtensionsLocked()
	if err != nil {
		return nil, err
	}
	policyBefore, policyBeforeErr := mls.GroupPolicyOf(mlsExtensionsOf(extensionsBefore))
	extensionsAfter := cloneExtensionBytes(processed.ContextExtensionsAfter)
	policyAfter, policyAfterErr := mls.GroupPolicyOf(mlsExtensionsOf(extensionsAfter))
	members, err := self.membershipLocked(policyBefore)
	if err != nil {
		return nil, err
	}
	membersAfter := make([]CommitMember, 0, len(processed.MembersAfter))
	for _, member := range processed.MembersAfter {
		membersAfter = append(membersAfter, self.commitMemberLocked(member.Leaf, member.Identity, member.HasLeafKeys, policyAfter))
	}
	return &CommitAuthorization{
		GroupId:           append([]byte(nil), self.id...),
		Epoch:             self.epoch + 1,
		CommitterLeaf:     processed.CommitterLeaf,
		CommitterIdentity: append([]byte(nil), processed.CommitterIdentity...),
		CommitterRole:     roleNameIn(policyBefore, processed.CommitterIdentity),
		AddedLeaves:       append([]uint32(nil), processed.AddedLeaves...),
		RemovedLeaves:     append([]uint32(nil), processed.RemovedLeaves...),
		UpdatedLeaves:     append([]uint32(nil), processed.UpdatedLeaves...),
		Members:           members,
		MembersAfter:      membersAfter,
		PolicyBefore:      policyBefore,
		PolicyAfter:       policyAfter,
		PolicyBeforeErr:   policyBeforeErr,
		PolicyAfterErr:    policyAfterErr,
		ExtensionsBefore:  extensionsBefore,
		ExtensionsAfter:   extensionsAfter,
	}, nil
}

// membershipLocked is this group's members as they stand right now, one [CommitMember] per member,
// with each sender_handle DERIVED from the group_handle_key and the leaf rather than read off any
// record, the identity MemberAt answers, and the role the policy handed in gives that identity. It
// is the pre-commit membership when the caller is [Group.commitAuthorizationLocked], and the policy
// is then the pre-commit one; nil reads every member as unnamed.
func (self *Group) membershipLocked(policy *mls.GroupPolicyExtension) ([]CommitMember, error) {
	members := make([]CommitMember, 0, self.handle.MemberCount())
	for at := 0; at < self.handle.MemberCount(); at += 1 {
		// MemberAt refuses a member whose leaf carries no urmessage_leaf_keys, so every member it
		// answers has them: the pre-commit side reads true by the door it came through
		leaf, identity, _, err := self.handle.MemberAt(at)
		if err != nil {
			return nil, fmt.Errorf("urmessage: the group's member %d: %w", at, err)
		}
		members = append(members, self.commitMemberLocked(leaf, identity, true, policy))
	}
	return members, nil
}

// commitMemberLocked is one [CommitMember]: the leaf, its derived sender_handle, a copy of its
// identity, whether the leaf carries leaf keys, and the role the policy gives that identity.
func (self *Group) commitMemberLocked(leaf uint32, identity []byte, hasLeafKeys bool, policy *mls.GroupPolicyExtension) CommitMember {
	handle := messagegroup.SenderHandle(self.groupHandleKey, leaf)
	return CommitMember{
		Leaf:         leaf,
		SenderHandle: append([]byte(nil), handle[:]...),
		IdentityPub:  append([]byte(nil), identity...),
		Role:         roleNameIn(policy, identity),
		HasLeafKeys:  hasLeafKeys,
	}
}

// contextExtensionsLocked is the group context extension list at this group's CURRENT epoch, as
// the seam spells it, decoded out of the same octets [messagegroup.GroupHandle.GroupContextBytes]
// answers -- the pre-commit list when the caller is the authorization check.
func (self *Group) contextExtensionsLocked() ([]messagegroup.ExtensionBytes, error) {
	contextBytes, err := self.handle.GroupContextBytes()
	if err != nil {
		return nil, fmt.Errorf("urmessage: the group context: %w", err)
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		return nil, fmt.Errorf("urmessage: decoding the group context: %w", err)
	}
	out := make([]messagegroup.ExtensionBytes, 0, len(context.Extensions))
	for _, extension := range context.Extensions {
		out = append(out, messagegroup.ExtensionBytes{
			Type: uint16(extension.ExtensionType),
			Data: append([]byte(nil), extension.ExtensionData...),
		})
	}
	return out, nil
}

// mlsExtensionsOf is the seam's extension list as mls's own type, for the one door that parses a
// policy out of a list: mls.GroupPolicyOf, which refuses a list carrying 0xF001 twice and validates
// what it finds. The bodies are shared, not copied; the caller owns both slices.
func mlsExtensionsOf(extensions []messagegroup.ExtensionBytes) []mls.Extension {
	out := make([]mls.Extension, 0, len(extensions))
	for _, extension := range extensions {
		out = append(out, mls.Extension{
			ExtensionType: mls.ExtensionType(extension.Type),
			ExtensionData: extension.Data,
		})
	}
	return out
}

// cloneExtensionBytes deep copies a seam extension list, body by body.
func cloneExtensionBytes(extensions []messagegroup.ExtensionBytes) []messagegroup.ExtensionBytes {
	out := make([]messagegroup.ExtensionBytes, 0, len(extensions))
	for _, extension := range extensions {
		out = append(out, messagegroup.ExtensionBytes{
			Type: extension.Type,
			Data: append([]byte(nil), extension.Data...),
		})
	}
	return out
}

// roleNameIn is the role a policy gives one identity, as the name [CommitMember.Role] carries. A
// nil policy, and an identity the policy does not name, both answer MEMBER (MASTER §11, ruling 8).
func roleNameIn(policy *mls.GroupPolicyExtension, identity []byte) string {
	if policy == nil {
		return mls.RoleMember.String()
	}
	role, _ := policy.RoleOf(identity)
	return role.String()
}

// noteEpochGapLocked delivers one pre-change record as a [GapOutOfWindow] gap: something is at this
// record id, it was sealed at an epoch this device has left, and no key on this single-epoch session
// opens it. See [GapReason].
//
// IT READS NO BODY, because it cannot -- the body is under the old epoch's key -- so the gap carries
// no kind and no text, only its position and its message_id. message_id is a function of the header
// and the group_handle_key, both of which this session holds whatever epoch it is at, so the gap is
// still NAMED. A record whose id cannot be derived is counted and resolved past rather than retried:
// the epoch will not come back, so there is nothing a re-fetch repairs.
//
// AND IT READS NO ROLE, WHICH IS A RULE AND NOT AN OMISSION (item 242's R4). Nothing opened, so
// nothing signed, and the only thing this record says about its sender is the handle it wrote into
// its own plaintext header. A role tag here would be a role read off a forgeable claim -- and there
// is no epoch to read it at, because the whole reason this record is a gap is that no schedule on
// this device reaches the epoch it was sealed at. [Message.SenderRoleAtSend] is "" here, which is
// the value that says so, and [Stats.RoleUndeterminable] does NOT move: nothing was asked.
//
// AND IT DOES NOT ATTRIBUTE THE RECORD TO THE HANDLE EITHER, which is this function's own half of
// ledger item 245's fourth piece and is argued at the line that decides it below.
func (self *Group) noteEpochGapLocked(walk *pageWalk, recordId uint64, header *message.RecordHeader) {
	self.stats.GapOutOfWindow += 1
	messageId, err := self.session.MessageIdOf(header)
	if err != nil {
		return
	}
	// AND `mine` IS DECIDED ON AN OCTET-FOR-OCTET MATCH AGAINST WHAT THIS DEVICE SEALED, NEVER ON
	// THE HANDLE. This used to be `walk.own[header.SenderHandle]` -- the sixteen octets, taken at
	// face value -- and that is exactly the harm [Message.Mine]'s own doc says this item's repair
	// prevents: SenderHandle(group_handle_key, leaf) takes no epoch and no identity, so every
	// record the PREVIOUS OCCUPANT of this device's leaf ever wrote carries this device's own
	// handle, and a newcomer on a reused leaf showed that member's whole history as its own. Every
	// one of those records reaches this road and no other, because they are all below the
	// newcomer's admission: measured, three records of the removed member came back at the
	// newcomer as gaps with `mine` true and an empty sender_identity.
	//
	// WHAT DECIDES IT INSTEAD IS THE EVIDENCE THE COPY ROAD ALREADY USES, one clause of it: this
	// device sealed at this stream index, and the record in hand carries the body_hash it sealed
	// there. Nobody can produce a second ct_body under one SHA-256, and a previous occupant's
	// record matches neither the index -- the index space is one run per (group_id, sender_handle)
	// and the newcomer's floor is seeded past everything that occupant spent -- nor the hash.
	// [Group.openOwnFromCopyLocked] is the road that SHOWS such a record when the epoch is still
	// reachable and the copy is still held; this is the same question asked where neither is true.
	//
	// AND THE IDENTITY GOES WITH IT, FOR THE COPY ROAD'S REASON. That road sets
	// [Message.SenderIdentity] to this device's own without an open, because "the record's
	// body_hash is one this device sealed" is a statement about THIS DEVICE and not about a leaf.
	// The same statement is what was just checked here. A gap that is NOT this device's still
	// carries no identity, which is what [Message.SenderIdentity]'s doc says of this road, and
	// `mine` is false there -- so [Message.Mine] and that field agree on every record of this road
	// rather than disagreeing on a whole member's history.
	// AND THE HANDLE IS NOT EVEN A PRE-FILTER HERE, WHICH WAS MEASURED RATHER THAN ARGUED. A
	// first draft asked `walk.own[header.SenderHandle]` first, as the roads above do. Mutating
	// that clause to `true` left all of this package green, and it cannot be otherwise: a
	// body_hash is SHA-256(ct_body), a ct_body is sealed under a key derived from the SENDER's
	// leaf, and a record from any other leaf therefore hashes to something no row of this
	// device's own table holds. The octet test already implies the handle test, so the handle is
	// gone from this road entirely rather than kept as a line no input can distinguish.
	mine := false
	var senderIdentity []byte
	if sealed, found := self.ownIndices[header.StreamIndex]; found && sealed.bodyHash == header.BodyHash {
		mine, senderIdentity = true, self.device.identityPub
	}
	entry := &Content{}
	received := newGap(GapOutOfWindow, entry, recordId, header.SenderHandle[:], senderIdentity, mine, 0,
		messageId[:], "")
	if self.deliverLocked(received, entry) {
		walk.opened = append(walk.opened, received)
	}
	self.delivered[recordId] = true
}

// roleAtSendLocked is the ONE place a receiving road asks what role a leaf held at the epoch a
// record was sealed at, and its answer is what [Message.SenderRoleAtSend] carries.
//
// THE LEAF MUST BE ONE THE RECORD'S OWN AUTHENTICATION PRODUCED. On the peer road that is the leaf
// OpenRecord signed at; on the own-copy road it is this device's own leaf, which no header chose.
// It is never a leaf resolved from a header's claimed sender_handle alone (item 242's ruling 24),
// and the seam's own door carries the same sentence.
//
// THE EPOCH IS THE RECORD'S AND NEVER THIS GROUP'S. Spec A: "read from the transcript-covered
// group-context extension of the SENDING EPOCH -- never from current membership."
//
// A REFUSAL IS AN EMPTY ROLE AND A COUNTER, AND NEVER A FAILED RECORD. The record has opened by the
// time this is asked: its position, its message_id, its text and its effects are all facts, and a
// build that dropped it because a second read came back short would be inventing a disappearance
// out of a metadata miss -- which is the one thing this package's whole gap design exists to
// prevent. So the message arrives with no role, a UI that has nothing to say says nothing, and
// [Stats.RoleUndeterminable] is the number that says how often. It is expected to stay zero: RoleAt
// reads the handle the open read.
//
// THE IDENTITY RoleAt ALSO ANSWERS IS NO LONGER DISCARDED, AND THE SENTENCE THAT STOOD HERE WAS
// WRONG BEFORE IT WAS OUT OF DATE. It said the identity was dropped because "the alpha has no
// identity system, [Message] carries a sender_handle and says in its own doc that it is not a
// name" -- and the handle is not merely not a name, it is not even a DISCRIMINATOR: ledger item
// 245 measured a newcomer on a removed member's leaf carrying that member's sixteen octets byte
// for byte, so a build that attributed by the handle showed two people's lines as one person's and
// a device's own lines as a stranger's. The credential identity is what MLS SIGNS and it is the
// only value on this road that separates two occupants of one leaf.
func (self *Group) roleAtSendLocked(epoch uint64, leaf uint32) string {
	_, role := self.senderAtSendLocked(epoch, leaf)
	return role
}

// senderAtSendLocked is the ONE ask, and [Group.roleAtSendLocked] is one projection of it: the
// credential identity of the member standing at `leaf` at `epoch`, and the role that member held
// there. Ledger item 242's ruling 21 and item 245's first repair.
//
// IT IS ONE CALL AND NOT TWO BECAUSE THE TWO ANSWERS MUST NOT BE ABLE TO DISAGREE. The seam's
// RoleAt reads one snapshot of one epoch's tree and projects both fields out of it; two calls
// would be two snapshots, with [messagegroup.GroupSession] free to install an epoch between them,
// and a [Message] attributed to one member carrying another member's role is a worse answer than
// either field being empty.
//
// A REFUSAL IS AN EMPTY PAIR AND A COUNTER. The two are empty together, never one of them, so
// "this device could not say who wrote this" is one state and not two.
func (self *Group) senderAtSendLocked(epoch uint64, leaf uint32) ([]byte, string) {
	identityPub, role, err := self.session.RoleAt(epoch, leaf)
	if err != nil {
		self.stats.RoleUndeterminable += 1
		return nil, ""
	}
	return identityPub, role
}

// recordIsOwnLocked is [Message.Mine]: whether the member the OPEN authenticated is this device.
//
// THE ANSWER IS AN IDENTITY COMPARISON AND NOT A HANDLE COMPARISON, which is the whole of ledger
// item 245's misattribution repair. `mine := header.SenderHandle == walk.own` was true of every
// record the PREVIOUS occupant of this device's leaf ever wrote, because
// SenderHandle(group_handle_key, leaf) takes no epoch and no identity and RFC 9420 §7.7 refills the
// leftmost blank leaf. A device that joined on a removed member's leaf showed that member's whole
// history as its own.
//
// THE FALLBACK IS THE PRE-FILTER, AND IT IS THE RESIDUAL STATED RATHER THAN HIDDEN. When the ask
// came back empty -- [Stats.RoleUndeterminable], which happens only when an epoch INSTALL ran
// between this record's open and this line and pushed its epoch out of the window -- there is no
// identity to compare, and the answer falls back to the handle: exactly what every build before
// this one answered, for a population of records that is expected to be empty. It is a fallback
// and not a second rule: it is reached only where this device can say nothing at all about who
// wrote the record, and in that state the handle is the only claim there is.
//
// ── AND THE HONEST BOUND: THIS COMPARISON IS UNREACHABLE IN THIS BUILD, AND IT IS A THEOREM ──
//
// MEASURED, not assumed. A mutant that replaces this whole body with `return maybeMine` -- which
// is exactly the pre-repair line -- leaves the removal suite and all 175 cases of this package
// GREEN. What the mutant survives on is not a missing test; it is two facts that together make the
// difference unproducible by any input this build can construct:
//
//  1. A DEVICE OPENS A RECORD ONLY AT AN EPOCH IT HOLDS STATE FOR. [Device.Join] files state from
//     its admission on, so a newcomer on a reused leaf cannot open ONE record the previous
//     occupant wrote -- every one of them is below its admission and answers [GapOutOfWindow].
//  2. AT EVERY EPOCH A DEVICE HOLDS STATE FOR, IT STANDS AT ITS OWN LEAF. RFC 9420 does not move a
//     standing member's leaf index, and MASTER §8.4.3's R1 binds the record's handle to the
//     SIGNING leaf -- so a record under this device's own handle, at an epoch this device can
//     open, was signed by this device's own leaf and carries this device's own identity.
//
// So the two answers differ only for a device whose handle has MOVED: one removed from a group and
// re-added at a different leaf, whose own earlier records are under the earlier leaf's octets.
// That is the same shape [Group.ownHandles] is a SET for.
//
// AND THAT SHAPE IS NOW BUILDABLE, WHICH IS THIS PARAGRAPH'S CORRECTION OF ITSELF. It used to read
// "this build cannot produce it -- the sdk exposes no product method over the seam's CommitRemove",
// which ledger item 242's R1 was true about and [Group.RemoveMember] is not.
// TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory removes
// a device and adds it back, and its second row admits a newcomer first so RFC 9420 §7.7 refills the
// blank leaf with somebody else and the re-add lands one along: the handle moves.
//
// THE THEOREM SURVIVES ITS OWN PREMISE GOING STALE, ON FACT 1 ALONE, AND THAT IS MEASURED TOO. The
// re-added device holds state from its admission on, so its own pre-removal records answer
// [ErrRecordOpen] on each of [maxRecordAttempts] walks and are then abandoned -- the same case
// counts them -- and this comparison is never reached for one of them.
//
// ON FACT 1 ALONE, AND THE SECOND LINE OF DEFENCE THAT STOOD HERE WAS FALSE IN THE UNSAFE
// DIRECTION. It read: "a re-add does not carry part nine forward either, so `maybeMine` is false
// for those records in the bargain." It is not false; it is TRUE. `maybeMine` is
// [Group.ownHandles] read through walk.own, and [Group.noteOwnLeafLocked] seeds that set with
// SenderHandle(group_handle_key, own leaf) -- a function of the KEY and the LEAF and of nothing
// else. A device removed and added back with nobody joining in between lands on its OWN old leaf
// (RFC 9420 §7.7 refills the leftmost blank one), so the key and the leaf are both unchanged, the
// fresh set holds the OLD sixteen octets byte for byte, and NOTHING had to be carried forward for
// this device's own pre-removal records to answer `mine` here. In that row the fallback WOULD
// attribute them to this device; what refuses them is fact 1, and fact 1 is the whole of it. In
// the newcomer row the handle does move and `maybeMine` is false -- for the other reason, that it
// is a different leaf's octets. Both directions are asserted, and the answer printed, by
// TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory.
//
// IT IS WRITTEN RATHER THAN DELETED FOR THE REASON THE SET IS DURABLE: the day a re-added device
// can reach an epoch below its admission, a line that read `mine` off sixteen octets would show
// the NEXT
// occupant of its old leaf its own history. What is claimed for this line today is exactly that: it
// is correct, it is unreachable on fact 1, and what would measure it is a past-epoch door a joiner
// does not have.
func (self *Group) recordIsOwnLocked(senderIdentity []byte, maybeMine bool) bool {
	if len(senderIdentity) == 0 {
		return maybeMine
	}
	return bytes.Equal(senderIdentity, self.device.identityPub)
}

// ── what a record becomes ────────────────────────────────────────────────────────────────────

// messageKeyOf is a message_id as a map key. It TRUNCATES NOTHING and pads nothing: an id of any
// other width is not a message_id, and every caller here has one off a [32]byte the derivation
// answered.
func messageKeyOf(messageId []byte) [MessageIdBytes]byte {
	key := [MessageIdBytes]byte{}
	copy(key[:], messageId)
	return key
}

// heldLocked is the [Message] this group holds under one message_id, READ OUT OF THE LOG rather
// than out of a table of its own.
//
// IT IS ONE FUNCTION BECAUSE THE POSITION MUST NEVER BE DEREFERENCED TWO WAYS. [Group.logIndex]
// answers where a message sits and [Group.log] holds it, and the whole of ledger item 227's repair
// is that the log slot is the only holder -- so a caller that resolved a target by indexing the log
// itself would be one more place to fix the day the pair grows a case. Every answer is the CURRENT
// message at that position, which after a rebuild is the replacement and not the message the
// rebuild froze.
func (self *Group) heldLocked(messageId []byte) (*Message, bool) {
	at, found := self.logIndex[messageKeyOf(messageId)]
	if !found {
		return nil, false
	}
	return self.log[at], true
}

// newMessage builds one [Message] from one parsed envelope. It is the only constructor, so the walk,
// the own-copy path and [Group.Send] cannot fill a [Message] three different ways.
//
// AN UNSUPPORTED ENTRY BECOMES A PLACEHOLDER HERE AND NOT SOMEWHERE ELSE: its Kind is the code that
// arrived, its Text is empty, and nothing else is set -- because a build that does not know a code
// must not guess at its layout, and a Text filled from a body it could not parse is exactly the
// failure headVersion 0x02 was bumped to prevent.
//
// senderRoleAtSend IS A PARAMETER AND NOT SOMETHING THIS FUNCTION CAN READ, which is item 242's
// ruling 21 expressed in a signature: the role belongs to the epoch the record was SEALED at, this
// constructor holds no epoch, and a constructor that went looking for one would find this group's
// CURRENT epoch -- the one answer spec C §5.6 says is wrong for a historical message. Every caller
// captures it beside the open that authenticated the sender, and [Group.noteEpochGapLocked] passes
// "" because its record never opened.
// senderIdentity IS A PARAMETER FOR senderRoleAtSend's REASON AND FOR ONE MORE. It is a fact about
// the epoch the record was sealed at, which this constructor holds none of; and it is the field
// [Message.Mine] is decided from, so a constructor that derived either from the sixteen octets
// beside it would be item 245's misattribution written into the one place every road passes
// through. Both come out of [Group.senderAtSendLocked], at the open, in one ask.
func newMessage(entry *Content, recordId uint64, senderHandle []byte, senderIdentity []byte,
	mine bool, sentAtMs int64, messageId []byte, senderRoleAtSend string) *Message {

	received := &Message{
		RecordId:         recordId,
		SenderHandle:     append([]byte(nil), senderHandle...),
		SenderIdentity:   append([]byte(nil), senderIdentity...),
		Mine:             mine,
		SenderRoleAtSend: senderRoleAtSend,
		Text:             entry.Text,
		SentAtMs:         sentAtMs,
		MessageId:        append([]byte(nil), messageId...),
		Kind:             entry.Kind,
	}
	if entry.Kind == KindReply {
		received.ReplyToId = append([]byte(nil), entry.Target...)
	}
	return received
}

// newGap builds the [Message] one record that OPENED and cannot be SHOWN becomes: spec A section
// 7.4's gap entry, with the [GapReason] that says which of the two this build can produce it is.
//
// IT IS A SECOND CONSTRUCTOR AND NOT A SECOND WAY TO FILL A [Message]: it builds through
// [newMessage], so every field a gap shares with a message is still filled in exactly one place, and
// all it adds is the one field that makes it a gap. A gap built by assigning [Message.Gap] at a call
// site would be a gap somebody can forget to mark, and [Group.deliverLocked] and
// [Group.reactableLocked] both branch on that field -- an unmarked malformed REACTION_ADD would be
// read as an effect on a target of thirty-two zero octets and never shown at all.
//
// A GAP'S TEXT AND ReplyToId ARE EMPTY BY CONSTRUCTION AND ARE NOT CLEARED HERE. Both come off the
// entry, and the only two entries a gap is ever built from carry neither: [ContentUnsupported]
// answers the code and the raw body and nothing else, and a malformed gap's entry is synthesised in
// [Group.openPageLocked] from octet 0 alone. A line that cleared them would be a line no mutation
// could kill, so there is none.
// AND A GAP'S ROLE IS THE CALLER'S TO PASS, for the same reason a message's is. The two gaps that
// OPENED -- malformed and unsupported -- carry the role the open authenticated, because they are
// records this device read the sender of; [GapOutOfWindow] carries "" because its record never
// opened and no epoch this device holds says anything about it.
func newGap(reason GapReason, entry *Content, recordId uint64, senderHandle []byte,
	senderIdentity []byte, mine bool, sentAtMs int64, messageId []byte,
	senderRoleAtSend string) *Message {

	received := newMessage(entry, recordId, senderHandle, senderIdentity, mine, sentAtMs,
		messageId, senderRoleAtSend)
	received.Gap = reason
	return received
}

// deliverLocked folds one opened record into this group and answers whether it became a LINE of the
// conversation.
//
// THREE KINDS OF RECORD ADD NO LINE and each answers false for a different reason:
//
//   - A REACTION and a TOMBSTONE are changes to another message. They are noted as effects and
//     applied to their target, now or whenever it arrives.
//   - A COVER is a change to nothing: "discarded, never receipted". It exists to be
//     indistinguishable from a real message on the wire, and a COVER that produced an entry would
//     be cover traffic the user can see.
//
// IT IS ONE FUNCTION BECAUSE THREE CALLERS ASK IT. [Group.openPageLocked], [Group.openOwnFromCopyLocked]
// and [Group.sendContentLocked] each hold a [Message] and an entry, and a rule about what a record
// becomes that lived in three places would be three rules the day one of them grew a case.
//
// A GAP IS ALWAYS A LINE, AND THAT CLAUSE IS LOAD-BEARING RATHER THAN TIDY. [Message.Kind] on a gap
// is the code the record ARRIVED under, not what the record is, and both rules below read that code:
// a malformed REACTION_ADD body -- a target shorter than thirty-two octets, say -- carries
// [KindReactionAdd], so without this clause effectOf would turn it into an effect standing on a
// target of thirty-two zero octets, deliverLocked would answer false, and THE GAP WOULD NEVER BE
// SHOWN. A malformed COVER would disappear the same way, and "discarded, never receipted" is a
// promise about a COVER this build could READ. A gap that is silent is the one thing a gap may not
// be, and the sender chooses the octets that decide which of these two rules it would have hit.
func (self *Group) deliverLocked(received *Message, entry *Content) bool {
	if received.Gap == "" {
		if effect, isEffect := effectOf(received, entry); isEffect {
			self.noteEffectLocked(effect)
			return false
		}
		if received.Kind == KindCover {
			return false
		}
	}
	// R4 AND ITS RULING 16: AN OBSERVER'S MESSAGE IS COUNTED AND KEPT, AND THE KEEPING IS THE
	// DECISION. It goes into the log below like any other line, with its text intact and its
	// message_id and its position, because a record dropped here is indistinguishable from a
	// record that never arrived -- and this package's whole gap design exists because silent
	// omission is the one thing a messenger may not do. It is NOT an eighth [GapReason] either:
	// that set is closed at seven, and a gap is a record this build could not SHOW, while this is
	// one it read perfectly well and has been asked to collapse. What a UI does with it is spec C
	// §5.6's, and [Stats.HiddenObserver] is the only thing this layer owes.
	//
	// IT IS COUNTED WHERE IT IS DECIDED, which is why the clause is here and not at the capture:
	// this is the one place a record becomes a LINE, and only a line has a row to collapse. An
	// observer's reaction or tombstone has already answered false above -- it is an effect, not an
	// entry -- and neither is hidden or counted HERE. What happens to those two is
	// [contentEffect.isObserverReaction]'s: the reaction is refused and counted by
	// [Stats.ObserverReactionRefused] (ruling 25), and the tombstone is applied (ruling 26).
	if received.SenderRoleAtSend == mls.RoleObserver.String() {
		self.stats.HiddenObserver += 1
	}
	key := messageKeyOf(received.MessageId)
	self.logIndex[key] = len(self.log)
	self.log = append(self.log, received)
	// AND THE EFFECTS THAT WERE WAITING FOR IT. A reaction or a tombstone that arrived before its
	// target has been held since, and this is the moment it applies.
	self.reapplyLocked(key)
	return true
}

// contentEffect is one record that changes ANOTHER message rather than adding one.
type contentEffect struct {
	kind ContentKind

	// The EFFECT RECORD's own message_id, which is the key it is held under. One record is one
	// effect however many times a rewind walks back over it.
	messageId [MessageIdBytes]byte

	// The server's number for it, and zero for a record this device has sealed and the server
	// has not answered yet. See [effectOrder] for what a zero sorts as and why.
	recordId uint64

	// Who sealed it, which is T-b's operand and a reaction's reactor.
	senderHandle []byte
	mine         bool

	// THE ROLE THAT SENDER HELD AT THE EPOCH IT SEALED THIS RECORD, carried across from
	// [Message.SenderRoleAtSend] and never read again afterwards.
	//
	// IT IS COPIED RATHER THAN LOOKED UP FOR THE REASON RULING 21 GIVES FOR THE FIELD IT COPIES:
	// the role belongs to the record's OWN epoch, an effect outlives that epoch in
	// [Group.effectsOn] for as long as its target is missing, and an epoch aged past
	// [messagegroup.PastEpochWindow] is one whose state mls has deleted. A lookup at apply time
	// would be asking a question this device may no longer be able to answer, about an epoch that
	// is not the one it would answer for.
	senderRoleAtSend string

	// The message it names.
	target [MessageIdBytes]byte

	// A reaction's emoji, raw. Empty on a tombstone.
	emoji string
}

// effectOf reads one parsed envelope as an effect, and answers false for a kind that is a line of
// the conversation rather than a change to one.
func effectOf(received *Message, entry *Content) (*contentEffect, bool) {
	switch entry.Kind {
	case KindTombstone, KindReactionAdd, KindReactionRemove:
	default:
		return nil, false
	}
	return &contentEffect{
		kind:             entry.Kind,
		messageId:        messageKeyOf(received.MessageId),
		recordId:         received.RecordId,
		senderHandle:     append([]byte(nil), received.SenderHandle...),
		mine:             received.Mine,
		senderRoleAtSend: received.SenderRoleAtSend,
		target:           messageKeyOf(entry.Target),
		emoji:            entry.Emoji,
	}, true
}

// isObserverReaction reports whether this effect is a REACTION whose sender was an OBSERVER at the
// epoch it sealed it -- the record item 242's ruling 25 refuses to apply, and the one
// [Stats.ObserverReactionRefused] counts.
//
// RULING 25: AN OBSERVER'S REACTION IS NOT APPLIED. "Hiding a reaction has no design" (ruling 19)
// is true of a ROW; NOT APPLYING one needs no design at all. The asymmetry with an observer's
// MESSAGE is statable in one sentence and that is why it is the rule: a message is KEPT because
// dropping it would hide that something was said, and this build's gap design exists because
// silent omission is the one thing a messenger may not do -- but a reaction that is not applied
// hides nothing, because the message it names is right there, whole. No position in the
// conversation goes blank. And a reaction that DID land would be "read only" failing in the most
// visible way this product has, on another member's line: [Message.Reactions] carries a
// sender_handle and no role, so a UI drawing reaction chips has no way to know the reactor was an
// observer and no way to collapse one the way §5.1 collapses a message.
//
// BOTH REACTION ARMS, AND THE REMOVE ARM IS NOT REDUNDANT. A REACTION_REMOVE cancels only the
// (reactor, emoji) pair its own sender_handle names, so refusing it changes nothing TODAY -- an
// observer's ADD was never applied, so its own REMOVE has nothing to cancel and it cannot reach
// anybody else's row. It is refused anyway because the alternative makes this rule rest on
// [contentEffect.applyTo]'s same-sender filter rather than on the sender's role, and that filter
// is exactly what D7 is written to widen (see the REMOVE arm: "a second device of one person
// cannot take back the first's reaction" -- until it is ruled). The day it widens, a rule that
// refused only the ADD would hand an observer a way to take a member's reaction off a member's
// line. The rule is about the RECORD CLASS and holds whatever applyTo does with it.
//
// RULING 26: AN OBSERVER'S TOMBSTONE IS APPLIED, AND THAT IS DELIBERATE. It is NOT an inconsistency
// to be tidied away by the next reader, which is why the reason is written here rather than
// inferred from the switch. A tombstone only ever removes the observer's OWN content --
// [contentEffect.applyTo]'s T-b requires the tombstone to come from the target's own sender, and
// T-a requires the target to be a stored CONTENT message -- so refusing one would keep VISIBLE
// something its author asked to retract. The role model exists to stop an observer ADDING to the
// group, not to trap its own words there. An observer's message is already hidden by ruling 16 and
// a retraction of it is the observer removing itself further, which is the direction this model
// wants.
func (self *contentEffect) isObserverReaction() bool {
	if self.senderRoleAtSend != mls.RoleObserver.String() {
		return false
	}
	switch self.kind {
	case KindReactionAdd, KindReactionRemove:
		return true
	}
	return false
}

// noteEffectLocked holds one reaction or tombstone and applies everything standing on its target.
//
// WHY IT IS HELD AND NOT APPLIED. A reaction, a tombstone and their target are three records and
// THE WALK'S ORDER IS NOT THE CONVERSATION'S ORDER: a record that fails to open holds the cursor
// back and is re-delivered on a LATER fetch, after record ids above it have already been shown, and
// after [maxRecordAttempts] it is abandoned and never delivered at all. So "the target is already
// here" is a thing this package may not assume, in either direction -- the target may arrive after
// the effect, and an effect may arrive after another effect that was written later.
//
// SO EVERY EFFECT IS KEPT AND THE TARGET'S STATE IS REBUILT, rather than each effect being applied
// once as it lands. The difference is not theoretical: an ADD at record 5 and a REMOVE at record 6
// that arrive in the order 6, 5 -- which is exactly what one failed open produces -- leave the
// reaction STANDING under apply-as-it-lands and REMOVED under a replay, and the second is what
// server order says. See [Group.reapplyLocked].
func (self *Group) noteEffectLocked(effect *contentEffect) {
	if held, seen := self.effects[effect.messageId]; seen {
		// ONE RECORD IS ONE EFFECT. The same record re-delivered behind an earlier failure is
		// the same effect with a record id the server may only now have given it, so the held
		// copy is updated in place rather than appended beside itself.
		*held = *effect
	} else {
		// AND THE REFUSED ONES ARE COUNTED HERE, WHICH IS THE ONE PLACE A RECORD BECOMES AN
		// EFFECT. Ruling 25's refusal itself is in [Group.reapplyLocked], where an effect becomes
		// a CHANGE -- and that is a per-rebuild event, so a counter there would count this
		// device's walk order. This branch is one record exactly once (the branch above is the
		// same record re-delivered behind an earlier failure), so the number is a fact about the
		// group: how many reaction records an observer sent that this build would not apply,
		// whether or not their targets ever arrive.
		if effect.isObserverReaction() {
			self.stats.ObserverReactionRefused += 1
		}
		self.effects[effect.messageId] = effect
		self.effectsOn[effect.target] = append(self.effectsOn[effect.target], effect)
	}
	// AND THE TARGET IS MARKED, NOT REBUILT. A rebuild here is a rebuild PER EFFECT, which is the
	// outer factor of the cube [Group.dirtyTargets] exists to remove: n effects on one message
	// rebuild that message n times, and each rebuild reads all n effects. The rebuild happens once
	// per walk instead, in [Group.rebuildDirtyLocked], from the same full sorted effect set.
	self.dirtyTargets[effect.target] = struct{}{}
}

// rebuildDirtyLocked runs the rebuild every effect noted since the last drain is owed, once per
// target rather than once per effect, and empties the set.
//
// THE ORDER IT WALKS THE SET IN IS A MAP'S ORDER AND THAT IS NOT A HAZARD: one rebuild reads one
// message's own effects and writes that message's own fields, so no two of them can see each other.
// The order INSIDE a rebuild is server order and is decided by [effectOrder], which is the ordering
// that is load-bearing and is held by TestEffectsAreAppliedInServerOrderAndNotArrivalOrder.
//
// A DIRTY TARGET THIS GROUP DOES NOT HOLD IS DROPPED FROM THE SET AND NOTHING IS LOST.
// [Group.reapplyLocked] answers nothing for a target that has not arrived, and the effects stay
// held under [Group.effectsOn]; the rebuild that owes them is the one [Group.deliverLocked] runs at
// the moment the target is indexed.
func (self *Group) rebuildDirtyLocked() {
	for target := range self.dirtyTargets {
		self.reapplyLocked(target)
	}
	clear(self.dirtyTargets)
}

// reapplyLocked rebuilds one message's effects from every effect record this group holds for it, in
// SERVER ORDER.
//
// IT REBUILDS RATHER THAN ACCUMULATES, which is the whole of why [Message.Deleted] and
// [Message.Reactions] are cleared first: an effect that arrives out of order has to be able to
// change the answer that an effect already applied gave, and an accumulator cannot be walked
// backwards. The cost is one pass over one message's effects per effect record, and the effects on
// one message are a number a human produced.
//
// THE CLEARING ITSELF DEFENDS NOTHING A TEST CAN SEE, measured by deleting the two lines and
// running ./urmessage and ./cp3b with nothing going red, and it is kept for a reason rather than
// from habit. Every [contentEffect.applyTo] is idempotent TODAY -- an ADD dedupes on
// (reactor, emoji), a REMOVE filters, and a tombstone sets a bool nothing else clears -- so
// replaying onto the previous answer happens to reach the same state as replaying onto an empty
// one. The clearing is what makes that a PROPERTY of this function rather than a coincidence of
// those three, and the day one of them is not idempotent it is the line that keeps this a rebuild.
// The SORT beside it is not in the same position: deleting that turns
// TestEffectsAreAppliedInServerOrderAndNotArrivalOrder red.
//
// A TARGET THAT IS NOT HERE IS NOT AN ERROR AND NOT A DROP. The effects stay held; this is what
// runs again when [Group.deliverLocked] indexes the target.
//
// ── IT REPLACES THE MESSAGE AND NEVER WRITES THROUGH ONE (msgrepo ledger item 227) ───────────
//
// THE REBUILD USED TO WRITE `held.Deleted = false` AND `held.Reactions = nil` INTO THE MESSAGE
// ALREADY IN THE LOG, and [Group.Messages] hands that same *Message to every caller: it copies the
// SLICE under this group's mutex and shares the VALUES. So a caller rendering the conversation it
// had already been given shared those two fields with a rebuild running under a lock it has no way
// to take, and the promise in Messages's own doc comment -- "are not written after they are
// appended" -- was false. MEASURED, not inferred: a probe rendering Group.Messages on one goroutine
// while another called Group.Receive reported TWO data races under -race, both of them these two
// writes, reached through Receive -> commitWalkLocked -> rebuildDirtyLocked.
//
// SO THE REBUILD BUILDS A NEW [Message] AND PUTS IT AT THE OLD ONE'S POSITION. Every pointer this
// package has ever handed out is frozen at the instant it was handed out, for ever, WITHOUT a lock
// held across anybody's rendering -- which is the only shape that works for a UI, since a UI paints
// on its own schedule and cannot hold this group's mutex while it does.
//
// WHAT IT COSTS: one [Message] per REBUILT message per walk. Not per effect -- [Group.dirtyTargets]
// made the rebuild once-per-target-per-walk -- and not per message, since a target with no effect
// records at all returns below without copying anything, which is every message in a conversation
// nobody has reacted to.
//
// THE SHALLOW COPY IS SOUND AND THAT IS A CLAIM ABOUT [Message], NOT A HOPE. Of its fields only
// Deleted and Reactions are ever written after construction (this function and
// [contentEffect.applyTo] are the only writers of either); SenderHandle, MessageId and ReplyToId
// are []byte built by [newMessage] with append-onto-nil and never written again, so the copy and
// the frozen original share arrays that nothing mutates. Reactions is set to nil on the copy before
// a single effect is applied, so the two never share a reaction array either.
//
// WHERE THE REPLACEMENT HAS TO BE PICKED UP: [Group.commitWalkLocked], which re-reads the messages
// a walk is about to hand back, and [Group.sendContentLocked], which re-reads the one it just sent.
// A caller of either would otherwise be given the frozen copy of a message this same call had
// rebuilt.
func (self *Group) reapplyLocked(target [MessageIdBytes]byte) {
	at, found := self.logIndex[target]
	if !found {
		return
	}
	// A TARGET NOTHING HAS EVER NAMED IS NOT REBUILT AND IS NOT COPIED. effectsOn only ever
	// GROWS -- a cancelled reaction is a REMOVE record beside its ADD and not a deletion from
	// this table -- so an empty effect set means no effect has ever touched this message, its
	// Deleted is false and its Reactions are nil, and the rebuild below would replace it with an
	// identical copy. That is every message in a conversation nobody has reacted to, and
	// [Group.deliverLocked] rebuilds each of them once as it arrives.
	effectsOn := self.effectsOn[target]
	if len(effectsOn) == 0 {
		return
	}
	held := self.log[at]
	effects := append([]*contentEffect(nil), effectsOn...)
	slices.SortStableFunc(effects, effectOrder)
	rebuilt := *held
	rebuilt.Deleted = false
	rebuilt.Reactions = nil
	held = &rebuilt
	// THE DEDUPE SET IS THE REBUILD'S AND IS BUILT BESIDE THE SLICE IT MIRRORS. The ADD arm used to
	// answer "has this reactor already reacted with this emoji" by SCANNING [Message.Reactions],
	// which is a scan of everything the rebuild had appended so far: m reactions cost m^2 comparisons
	// inside one rebuild, and that is the inner factor of the cube [Group.dirtyTargets] removes the
	// outer one of. With the set, one rebuild costs the sort it already paid for and nothing more.
	//
	// THE KEY IS A CONCATENATED STRING AND NOT A STRUCT, DELIBERATELY: a struct with a []byte field
	// is not comparable and cannot be a map key at all, and a string of the handle with a separator
	// is the same equality the scan computed -- bytes.Equal on the handle AND equality on the emoji.
	// The separator is 0x00, which no emoji tail can carry and no sender_handle ends on ambiguously,
	// so (handle, emoji) pairs cannot collide across the join.
	//
	// IT IS HANDED DOWN RATHER THAN HELD ON THE GROUP because it is only ever true of ONE rebuild:
	// the slice it mirrors is cleared two lines above, so a set that outlived this call would be a
	// set describing reactions that no longer exist.
	seen := make(map[string]struct{}, len(effects))
	for _, effect := range effects {
		// RULING 25, AND IT IS HERE BECAUSE THIS IS WHERE AN EFFECT BECOMES A CHANGE. An
		// observer's reaction is refused at every application and not only at the one that
		// happens to be its first: an effect whose target had not arrived is HELD under
		// [Group.effectsOn] and applies whenever the target is indexed, and a refusal that lived
		// on the arrival path would let exactly those through -- which is the ordinary shape of a
		// walk, since a record that fails to open is re-delivered behind ids above it. The skip
		// costs the dedupe set nothing: a refused effect appends no row, so it adds no key, so an
		// honest member's later ADD of the same emoji is not deduped against a reaction that was
		// never applied. See [contentEffect.isObserverReaction] for the rule and for why a
		// TOMBSTONE goes through (ruling 26).
		if effect.isObserverReaction() {
			continue
		}
		effect.applyTo(held, seen)
	}
	// AND IT IS PUBLISHED LAST, WHICH IS THE ONE LINE THAT MAKES THE COPY WORTH ANYTHING. Until
	// here the rebuilt message is reachable from this frame alone; after it, it is the message
	// [Group.log] holds and [Group.heldLocked] answers, and the one it replaced is frozen in
	// whatever [Group.Messages] copy already carries it.
	self.log[at] = held
}

// reactionKey is the (reactor, emoji) pair [Group.reapplyLocked]'s dedupe set is keyed on, and it is
// the SAME equality the scan it replaced computed: the raw sender_handle octets and the raw emoji
// octets, joined by a separator neither can contain.
func reactionKey(senderHandle []byte, emoji string) string {
	return string(senderHandle) + "\x00" + emoji
}

// effectOrder is server order: `record_id` ascending, which is what §5.3 says decides which of two
// reactions came last.
//
// A RECORD THE SERVER HAS NOT NUMBERED SORTS LAST, and that is a decision rather than a fallback.
// The only records with a zero id are ones THIS DEVICE has just sealed and not yet had answered, so
// "the newest thing that happened" is the true reading of one; sorting it first would let a
// half-submitted reaction be cancelled by a REMOVE the server numbered before it existed.
//
// THE TIE-BREAK IS THE EFFECT'S OWN message_id, so that two effects the server has not numbered
// have an order at all, and the SAME order on every device that holds them.
func effectOrder(first *contentEffect, second *contentEffect) int {
	if first.recordId != second.recordId {
		switch {
		case first.recordId == 0:
			return 1
		case second.recordId == 0:
			return -1
		case first.recordId < second.recordId:
			return -1
		}
		return 1
	}
	return bytes.Compare(first.messageId[:], second.messageId[:])
}

// applyTo is one effect, against the message it names.
//
// `seen` IS THE REBUILD'S DEDUPE SET AND BOTH REACTION ARMS OWE IT AN UPDATE. It mirrors
// [Message.Reactions] exactly -- a key is added where a reaction is appended and deleted where one
// is filtered out -- because the two are read against each other across a replay: an ADD replayed
// AFTER a REMOVE of the same (reactor, emoji) has to land, and it only lands if the REMOVE took the
// key out as well as the row. See [Group.reapplyLocked], which is the only caller and which owns
// the set's lifetime.
func (self *contentEffect) applyTo(target *Message, seen map[string]struct{}) {
	switch self.kind {
	case KindTombstone:
		// T-b, THE SAME-SENDER RULE, and it is what MASTER §12.1's "a deletion cannot be
		// forged" needs beyond R1: R1 proves who sealed the TOMBSTONE and nothing in it proves
		// they sealed the target. A tombstone from anybody else is ignored -- not refused,
		// because the record is a legal record and a receiver that failed the walk over one
		// would be handing any member a way to wedge the conversation.
		if !bytes.Equal(self.senderHandle, target.SenderHandle) {
			return
		}
		// T-a: only a stored CONTENT message can be deleted. A reaction, a tombstone and a
		// COVER are not entries and never reach here; a kind this build does not know is an
		// entry, and it is not one this build can say is deletable.
		switch target.Kind {
		case KindText, KindReply:
		default:
			return
		}
		target.Deleted = true
	case KindReactionAdd:
		key := reactionKey(self.senderHandle, self.emoji)
		if _, standing := seen[key]; standing {
			return
		}
		seen[key] = struct{}{}
		target.Reactions = append(target.Reactions, Reaction{
			SenderHandle: append([]byte(nil), self.senderHandle...),
			Emoji:        self.emoji,
			Mine:         self.mine,
		})
	case KindReactionRemove:
		// A REMOVE CANCELS AN ADD WITH THE SAME (reactor, target, emoji) AND NOBODY ELSE'S.
		// The reactor is the sender_handle: D7 is what would make it a person rather than a
		// leaf, and until it is ruled a second device of one person cannot take back the
		// first's reaction.
		//
		// AND IT CANCELS THE KEY AS WELL AS THE ROW. Without the delete, an ADD that sorts
		// after this REMOVE -- which is the ordinary shape of react, un-react, react again --
		// would find its key still standing and return without appending, and the reaction the
		// user made last would not be shown.
		delete(seen, reactionKey(self.senderHandle, self.emoji))
		kept := make([]Reaction, 0, len(target.Reactions))
		for _, standing := range target.Reactions {
			if standing.Emoji == self.emoji && bytes.Equal(standing.SenderHandle, self.senderHandle) {
				continue
			}
			kept = append(kept, standing)
		}
		target.Reactions = kept
	}
}

// ownFrameAlreadySpent reports whether OpenRecord refused a record for exactly one reason: its inner
// MLS frame names a generation of this device's own leaf that this device has already spent. It is
// asked only of a record under this device's own sender_handle.
//
// WHAT THAT REFUSAL ESTABLISHES, READ OFF connect AT d368fea RATHER THAN ASSUMED, because the whole
// clone check leans on it. OpenRecord reaches the inner frame only after body_hash matched ct_body,
// the record key derived for this sender_handle at this stream_index, and BOTH record AEADs opened
// (messagegroup/seal.go, openRecordOnLoop) -- so a record that gets as far as ErrRecordInnerFrame
// was written by a holder of this group's class keys, at this position, with this body_hash. The
// frame's sender data then opened under the group's sender_data_secret, MASTER section 8.4.3's R1
// found the frame's leaf to be the one this sender_handle belongs to and R2 found its aad to be this
// record's own position (messagegroup/mlsframe.go, unframeBodyOnLoop, the PEEK half), and only then
// did mls refuse the generation (mls/secret_tree.go, classify, reached from MessageKey) -- BEFORE
// the content AEAD and BEFORE the signature.
//
// SO IT IS EXACTLY AS STRONG AS "IT OPENED" WAS BEFORE 4c030dc, AND NO STRONGER. The signature is
// never checked on this path and cannot be: the key it would need is the one this device erased
// when it sealed. Any MEMBER of the group can build a record that reaches this refusal at this
// device's handle -- which any member could also do, and have OPEN, before the ruling. The clone
// check therefore goes on resting on "a holder of this group's keys wrote this", which is what it
// rested on; what it does NOT get is "this device's own signature key wrote this", and a member that
// wants to wedge this device as a copy of itself can still do so for the price of one record. That
// is the residue connect's TestAnyMemberCanStillSquatAnotherLeafsStreamIndex already measures one
// layer down, reached from the clone check's side.
//
// cp3b.TestAnOwnRecordBentInFlightIsAFailureAndNotAnOwnRecord holds the half of this that a
// reordering inside connect would break: an own record whose ciphertext does not open never
// reaches this refusal and is a failure, not an own record.
//
// EACH HALF OF THE CONJUNCTION ALONE DEFENDS NOTHING A TEST HERE CAN SEE, measured by deleting each
// over urmessage and cp3b with nothing going red. Without the mls half, an own record whose inner
// frame fails for another reason -- a peek that does not parse, a generation too far ahead -- would
// be counted as this device's own; reaching those needs a record built at this device's handle with
// group keys and a bad frame, which nothing in sdk can build. Without the messagegroup half nothing
// changes at all, because no refusal on OpenRecord's path wraps the mls sentinel except through
// ErrRecordInnerFrame. The conjunction is kept because it names MG-4's one refusal exactly.
func ownFrameAlreadySpent(err error) bool {
	return errors.Is(err, messagegroup.ErrRecordInnerFrame) && errors.Is(err, mls.ErrRatchetGenerationConsumed)
}

// checkAttestationLocked performs the two halves of 4.3.4 that need no key, and counts the half
// that does. See [Group.Receive] for the whole of the decision and for S2-27.
//
// `readEpoch` IS THE EPOCH THE REQUEST WAS AUTHENTICATED UNDER, PASSED IN RATHER THAN RE-READ.
// It is `request.ReadEpoch`, the field `req_auth` was computed over, and not [Group.epoch] read a
// second time: a walk can cross an epoch between pages -- refreshReadKey exists for exactly that
// -- so a second read of the group's own epoch would be a different number from the one this page
// was asked under, and the comparison below would be about the wrong request.
func (self *Group) checkAttestationLocked(since uint64, readEpoch uint64,
	fetched *protocol.FetchResponse) error {
	attestation := fetched.GetAttestation()
	if attestation == nil {
		// THE DOWNGRADE CHECK, and it reads the server's OWN advertisement rather than a
		// setting of ours: a server that says it signs and then does not is refused, and a
		// server that never claimed to is counted.
		if self.device.transport.Capabilities().GetAttestationSupported() {
			return fmt.Errorf("%w: this server advertises attestation_supported and answered a page with no attestation",
				ErrFetchAttestation)
		}
		self.stats.Unattested += 1
		return nil
	}
	if !bytes.Equal(attestation.GetGroupId(), self.id) {
		return fmt.Errorf("%w: it names group %x and this fetch was for %x",
			ErrFetchAttestation, attestation.GetGroupId(), self.id)
	}
	if attestation.GetSinceRecordId() != since {
		return fmt.Errorf("%w: it names since_record_id %d and this fetch asked from %d",
			ErrFetchAttestation, attestation.GetSinceRecordId(), since)
	}
	// RULING 32's read_epoch, HELD AGAINST THE EPOCH THIS REQUEST AUTHENTICATED UNDER. F0 made
	// `high_water_record_id` ceiling-relative, so the ceiling is a third filter beside `class_mask`
	// and `heads_only` -- and §4.3.4's stated purpose for those two is "so that a filtered fetch is
	// not byte-indistinguishable from a withholding one". This is the half of that which costs no
	// key: the client sent `ReadEpoch: self.epoch` and it knows what it sent, so a server that
	// names a DIFFERENT ceiling in the attestation is contradicting the request in a field the
	// client already holds. No fleet key, no signature, no PKI.
	//
	// WHAT IT CATCHES, AND IT IS NOT ITEM 244's READ-SIDE WITHHOLDING. A previous hand-off note
	// claimed this comparison "catches ruling 32's measurement" and that claim is FALSE; it was
	// corrected in connect 69bf704c's own commit message and the true sentence goes here, at the
	// site, rather than being left to a reader to reconstruct. What this catches is a server
	// TRUTHFULLY naming a ceiling below the one the request was MAC'd under -- a misconfigured,
	// misrouted or mis-sharded server, a real class and a free one to close. What it CANNOT catch
	// is the measurement ruling 32 was taken on: a server that clamps every reader to epoch 1,
	// answers a `read_epoch = 3` request, and writes 3 in the attestation anyway. That answer is
	// BYTE-IDENTICAL to an honest one -- seven of twelve records and two whole epochs withheld with
	// every field agreeing -- and the only thing that separates them is the signature over the
	// preimage `read_epoch` now sits in, which is unbuilt here and unbuilt on the server. A LYING
	// CLAMP IS NOT DETECTABLE BY THIS CHECK OR BY ANY OTHER KEYLESS ONE.
	//
	// IT IS A REFUSAL AND NOT A COUNTER, which is the opposite of the high-water omission arm one
	// page up, and the difference is what the two are about. The omission arm compares a server's
	// number against what this device managed to open, and an honest server doing §7.2 retention
	// will one day trip it. This compares a server's own statement against a value the client put
	// on the wire itself; there is no retention, no pruning and no legitimate sweep that makes
	// those two disagree, so there is no future in which refusing is wrong.
	if attestation.GetReadEpoch() != readEpoch {
		return fmt.Errorf("%w: it names read_epoch %d and this fetch was authenticated under %d",
			ErrFetchAttestation, attestation.GetReadEpoch(), readEpoch)
	}
	if attestation.GetHighWaterRecordId() != fetched.GetHighWaterRecordId() {
		return fmt.Errorf("%w: it names high_water %d and the response carries %d",
			ErrFetchAttestation, attestation.GetHighWaterRecordId(), fetched.GetHighWaterRecordId())
	}
	attested := attestation.GetRecordIds()
	records := fetched.GetRecords()
	if len(attested) != len(records) {
		return fmt.Errorf("%w: it lists %d record ids and the page carries %d records",
			ErrFetchAttestation, len(attested), len(records))
	}
	for at, record := range records {
		if attested[at] != record.GetRecordId() {
			return fmt.Errorf("%w: its record id %d at position %d is not the page's record %d",
				ErrFetchAttestation, attested[at], at, record.GetRecordId())
		}
		if attestation.GetHighWaterRecordId() < record.GetRecordId() {
			return fmt.Errorf("%w: it names high_water %d and the page carries record %d",
				ErrFetchAttestation, attestation.GetHighWaterRecordId(), record.GetRecordId())
		}
	}
	// AND THE SIGNATURE IS NOT CHECKED. Counted, never claimed. S2-27.
	self.stats.Unattested += 1
	return nil
}

// initTables allocates the per-group bookkeeping every constructor owes, IN ONE PLACE.
//
// THREE CONSTRUCTORS BUILD A [Group] -- [Device.CreateGroup], [Device.Join] and
// [Device.restoreOne] -- and each of them used to spell its own map literals. A fourth that
// forgot one would not fail to compile and would not fail a type check: it would panic on the
// first write to a nil map, inside [Group.Receive], on a device in somebody's hand. Four maps
// spelled in three places is the drift this removes.
//
// It is called AFTER the literal rather than replacing it, because the fields that differ between
// the three -- the founding session, the epoch, the opened bit, whether the group is reconciled --
// are the interesting ones and belong where a reader can see all of them at once.
func (self *Group) initTables() {
	self.tracked = map[trackedKey]bool{}
	self.delivered = map[uint64]bool{}
	self.attempts = map[uint64]int{}
	self.ownIndices = map[uint64]*ownSealed{}
	self.withoutCopy = map[uint64]bool{}
	// THE LEAF LEDGER'S TWO HALVES, ALLOCATED HERE AND FILLED BY [Group.noteOwnLeafLocked] AND BY
	// [Device.restoreOne]. A constructor that forgot either would not fail to compile: it would
	// show this device's own lines as a stranger's, or abandon a departed member's records after
	// three attempts, on a device in somebody's hand.
	self.ownHandles = map[[16]byte]bool{}
	self.departedAt = map[uint32]uint64{}
	self.noteOwnLeafLocked()
	self.ownHeads = map[trackedKey]uint64{}
	self.peerHeads = map[ladderKey]uint64{}
	self.peerHeadsAt = map[epochLadderKey]uint64{}
	self.persistedHeads = map[epochLadderKey]uint64{}
	self.logIndex = map[[MessageIdBytes]byte]int{}
	self.effects = map[[MessageIdBytes]byte]*contentEffect{}
	self.effectsOn = map[[MessageIdBytes]byte][]*contentEffect{}
	self.dirtyTargets = map[[MessageIdBytes]byte]struct{}{}
	// THE ONE TABLE THIS FUNCTION MAY NOT CLEAR, AND THE ONE IT MUST TAKE OWNERSHIP OF.
	//
	// It may not clear it because every other map here is derived state a walk rebuilds, while
	// pq_secrets holds values that ARRIVED and that nothing in this package can re-derive; each
	// constructor fills it BEFORE calling this (the founder's draw, the joiner's invite, the
	// restorer's record), so an unconditional assignment would erase a founded group's epoch-zero
	// secret between the literal and the first seal.
	//
	// IT MUST COPY BECAUSE THE ENTRIES ARE ERASED IN PLACE. [Group.dropPqSecretsBelowWindowLocked]
	// zeroizes an evicted entry rather than dropping it, and [Group.Close] zeroizes all of them --
	// which is the right discipline for a retired epoch's post-quantum half and is a live grenade
	// under any entry the constructor did not own. MEASURED, not imagined: a world that built
	// three members' groups from ONE shared scalar had that scalar blanked, for every member at
	// once, the moment the first member's window moved past its oldest epoch -- at epoch 34 of 35,
	// so the symptom was two members disagreeing about the storage root thirty epochs after the
	// value was shared, and the record that reported it was the one sealed at epoch 35. The copy
	// here is what makes "an entry of this table is this group's to erase" true of every
	// construction rather than of the three that remembered.
	owned := make(map[uint64][]byte, len(self.pqSecrets))
	for epoch, secret := range self.pqSecrets {
		owned[epoch] = append([]byte(nil), secret...)
	}
	self.pqSecrets = owned
	// AND THE WITNESS IS SEEDED FROM WHAT THE CONSTRUCTOR FILLED, which is the floor and not the
	// whole of it. Every row this group holds at construction is a value it has held, so it is a
	// value a removal may not be followed on; [Device.restoreOne] then adds the rows the record
	// carries for epochs the window has already moved past, which is the half that makes the rule's
	// subject survive a restart. A constructor that filled no table leaves an empty witness, which
	// is the truth about a group that has held nothing.
	self.pqSecretWitness = map[uint64][sha256.Size]byte{}
	for epoch, secret := range self.pqSecrets {
		self.witnessPqSecretLocked(epoch, secret)
	}
	self.wrapsFor = map[uint64][]wrapCandidate{}
	self.wrapsUnreadable = map[uint64]int{}
}

// advanceOwnLadderLocked moves the receiver ladder over this device's OWN leaf up to the position
// this group has already authenticated, when the own record about to be opened lies past that
// ladder's window.
//
// WHY IT EXISTS, MEASURED AND NOT REASONED. Before MG-4 every own record was OPENED, in order, and
// each open committed a rung, so the ladder over this device's own leaf walked along behind them.
// Since MG-4 an own record shown from the copy, or authenticated at the spent generation, commits
// nothing -- so the own ladder stayed at its root, and the first own record that DID need opening
// past index 1,024 (messagegroup.DefaultRecordWindowSize) was ErrOutOfWindow. Measured over 1,030
// lines: a copy of the folder that was behind the original by one line reconciled cleanly and was
// NOT caught before it sealed -- the evidence record was retried three times, abandoned, and the
// next walk was clean -- and a restart with no copies left six abandoned holes.
//
// THE HEAD IS [Group.ownIndexSeen] + 1, WHICH IS CALLER STATE AND NOT A HEADER. TrackSender's
// head is walked from the root, so a number a server wrote would be a number of expansions a server
// chose; ownIndexSeen is read only off own records the group's keys authenticated or that this
// device sealed. The header's stream_index decides only WHETHER to move, never where to, and a
// move that would not raise the head is not made -- so a server that writes far-ahead indices buys
// nothing but a comparison. What moving costs is the indices below the new head: an own record
// there no longer opens. In a walk those are behind it already, because a server refuses a stream
// index that regresses.
func (self *Group) advanceOwnLadderLocked(leaf uint32, header *message.RecordHeader) error {
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordOpen, err)
	}
	// THE RECORD'S EPOCH AND NOT THIS GROUP'S, since ledger item 241: an own record from a prior
	// epoch that has no copy is authenticated under that epoch's schedule, so the ladder it moves
	// is that epoch's. trackSessionLadderLocked is what routes the install.
	key := trackedKey{epoch: header.Epoch, ladderKey: ladderKey{leaf: leaf, retentionWire: retentionWire, ephWindow: header.EphWindow}}
	head := self.ownHeads[key]
	if header.StreamIndex <= head+uint64(messagegroup.DefaultRecordWindowSize) {
		return nil
	}
	next := self.ownIndexSeen + 1
	if next <= head {
		return nil
	}
	if err := self.trackSessionLadderLocked(header.Epoch, leaf, header, next); err != nil {
		return fmt.Errorf("%w: moving this device's own ladder to index %d: %w", ErrRecordOpen, next, err)
	}
	self.ownHeads[key] = next
	self.tracked[key] = true
	return nil
}

// trackSessionLadderLocked installs one receiver ladder in the session, at the CURRENT epoch
// through TrackSender or at a PRIOR one through TrackSenderAt, and it is the only place this
// package chooses between the two. Ledger item 241.
//
// The prior-epoch arm's refusals are the session's own sentinels and are passed through
// unwrapped, because the walk branches on them: [messagegroup.ErrEpochOutOfWindow] and
// [messagegroup.ErrPastEpochUnobtainable] carrying [ErrStateNotFound] are gaps, and everything
// else is a failure.
func (self *Group) trackSessionLadderLocked(epoch uint64, leaf uint32, header *message.RecordHeader, head uint64) error {
	if epoch < self.epoch {
		return self.session.TrackSenderAt(epoch, leaf, header.RetentionClass, header.EphBucket, header.EphWindow, head)
	}
	return self.session.TrackSender(leaf, header.RetentionClass, header.EphBucket, header.EphWindow, head)
}

// pastEpochOpenableLocked is whether the walk should hand a record from one PRIOR epoch to the
// session at all: inside the window, and not an epoch this walk was already told this device
// holds no state for. The session makes the same window check itself and refuses by name; this
// is the cheaper answer taken first, so that a later joiner draining hundreds of pre-admission
// records costs one store read and not hundreds.
func (self *Group) pastEpochOpenableLocked(walk *pageWalk, epoch uint64) bool {
	if self.epoch-epoch > messagegroup.PastEpochWindow {
		return false
	}
	return !walk.unobtainable[epoch]
}

// pastHeadLocked is the head a PRIOR epoch's ladder is tracked at: the highest index this
// process has authenticated on that ladder AT OR BELOW that epoch, or the highest index the disk
// held for it STRICTLY BELOW that epoch -- whichever is higher.
//
// THE TWO INEQUALITIES DIFFER AND BOTH ARE LOAD-BEARING. A head this process authenticated at
// epoch n was raised by records that are in [Group.delivered] and will not be opened again, so
// the ladder may stand above them; a head the disk held at epoch n was raised by records a
// restart is about to open AGAIN, from the first, so the ladder must stand below all of them --
// at the head as of the START of n, which is the highest head of any epoch before it. [PeerHead]
// carries the same argument from the disk's side.
//
// It never reads [Group.peerHeads], which a later epoch's records may have raised above every
// record of this one. A member never seen at or before this epoch answers 0, which is where a
// stream a member just started stands.
//
// IT SPANS BOTH OCCUPANTS OF A LEAF THAT CHANGED HANDS, AND THAT IS REQUIRED RATHER THAN A BUG.
// A review of the commit that added this function read the span as a defect and asked for rows
// below the leaf's departure to be skipped. They must not be. A stream index is allocated per
// (group_id, sender_handle) and the server refuses any index the previous occupant spent, so the
// NEWCOMER's first accepted index is that occupant's high water plus one -- and a ladder that
// skipped the previous occupant's rows would stand at 0 and answer ErrOutOfWindow for every
// record the newcomer wrote, the moment the previous occupant got past
// [messagegroup.DefaultRecordWindowSize]. Measured both ways; see
// [Group.pruneRemovedLaddersLocked] and [Group.leafStreamFloorLocked], which is this same read
// with the class dropped out of the key.
func (self *Group) pastHeadLocked(ladder ladderKey, epoch uint64) uint64 {
	head := uint64(0)
	for key, at := range self.peerHeadsAt {
		if key.ladderKey == ladder && key.epoch <= epoch && head < at {
			head = at
		}
	}
	for key, at := range self.persistedHeads {
		if key.ladderKey == ladder && key.epoch < epoch && head < at {
			head = at
		}
	}
	return head
}

// leafStreamFloorLocked is the lowest head a ladder over `leaf` may be installed at for a record
// sealed at `epoch`: the highest stream index this device has AUTHENTICATED under that leaf's
// sender_handle, on any ladder, at or below that epoch. Ledger item 245's second piece, corrected.
//
// WHY IT IS A FACT ABOUT THE LEAF AND NOT ABOUT THE LADDER, which is the whole of it. A receiver
// ratchet is one per (leaf, retention class, eph window) and its head is an index; the INDEX
// SPACE is one per (group_id, sender_handle) and a sender_handle is
// SenderHandle(group_handle_key, leaf) with no epoch and no identity. So every ladder over one
// leaf, at every epoch, under every occupant, allocates out of ONE run of numbers -- and the
// server enforces that: MASTER section 8's monotonicity is keyed on (group_id, sender_handle) and
// refuses an index at or below the last one it accepted there, whoever wrote it.
//
// WHAT THAT BUYS, AND IT IS THE REPAIR FOR A BLOCKER THIS ITEM'S OWN SECOND PIECE OPENED. A leaf
// that changes hands gives the newcomer a stream that CONTINUES the removed member's numbering --
// [Group.seedOwnStreamLocked] is the client half of the same fact -- so a survivor that positions
// the newcomer's ladder at 0 is positioning it however far the previous occupant got below the
// truth, and [messagegroup.ReceiverRatchet] refuses anything more than
// [messagegroup.DefaultRecordWindowSize] ahead of its head. With a previous occupant of 1,025
// lines the newcomer's first record is at 1,026 and every survivor answered "index 1026 is 1026
// ahead of head 0, and the window is 1024" until it abandoned it.
//
// IT IS A FLOOR AND NEVER A CEILING. [Group.trackLocked] takes the MAXIMUM of this and the head it
// already had, so it can only move a ladder UP and only to an index this device has already
// authenticated -- never past a record it has not seen, which is the property that keeps a peer
// from choosing how far this device walks.
//
// THE TWO INEQUALITIES ARE [Group.pastHeadLocked]'s, for its reasons, and they are not repeated
// here: this is that function with the class and the eph window dropped out of the key.
func (self *Group) leafStreamFloorLocked(leaf uint32, epoch uint64) uint64 {
	floor := uint64(0)
	for key, at := range self.peerHeadsAt {
		if key.leaf == leaf && key.epoch <= epoch && floor < at {
			floor = at
		}
	}
	for key, at := range self.persistedHeads {
		if key.leaf == leaf && key.epoch < epoch && floor < at {
			floor = at
		}
	}
	return floor
}

// crossEpochLadderLocked carries this group's receiver-ladder bookkeeping across an epoch change.
// It is A4, and it has TWO callers -- the commit-ingest path ([Group.ingestCommitLocked]) and, since
// [Group.RemoveMember] gave a committer removed leaves of its own, the publish path
// ([Group.publishCommitLocked]). In each it runs in the same statement block as the
// [messagegroup.GroupSession] install that zeroized the ratchets it describes -- not before it,
// because a peer record could still be opened against the epoch that is closing, and not after A3's
// persist, because a persist that named the new epoch with the ladders still describing the old one
// would come back from a restart tracked at nothing. Both callers run the departed-leaf filing and
// the ladder prune BEFORE it, for the reason the loop below states; that both of them do is held by
// TestBothArmsThatEnterAnEpochFileAndPruneWhatTheirOwnCommitRemoved.
//
// TWO THINGS ARE CLEARED AND ONE IS KEPT. [Group.tracked] and [Group.ownHeads] both name receiver
// ratchets [messagegroup.GroupSession.AdvanceEpoch] has just zeroized, so they are cleared -- and
// [trackedKey] now carries the epoch, so a memo that survived would also be a memo at the epoch
// before, which is the same rule read the other way. [Group.peerHeads] is NOT cleared: it is the
// head this device authenticated for each peer, the stream index is continuous across epochs, and
// it is the whole of what stops a re-track at 0 from starving a busy peer.
//
// EACH PEER LADDER IS RE-TRACKED AT ITS AUTHENTICATED HEAD, keyed by the NEW epoch. A member just
// added has no head here and is tracked lazily at 0 by [Group.trackLocked] on first sight, which is
// correct: its stream starts at this epoch. The own ladder is not re-tracked here -- it is rebuilt
// lazily by [Group.advanceOwnLadderLocked] off [Group.ownIndexSeen], which survives the clear for
// peerHeads' reason.
//
// newEpoch is [Group.handle]'s own Epoch, which [messagegroup.GroupHandle.ApplyCommit] has already
// advanced; [Group.epoch] does not move until [Group.enterEpochLocked] runs after this, so the key
// is built off the handle rather than the field. [Group.trackLocked]'s later keys use
// [Group.epoch], which enterEpochLocked then sets equal to this, so the two agree.
func (self *Group) crossEpochLadderLocked(newEpoch uint64) error {
	clear(self.tracked)
	clear(self.ownHeads)
	for ladder, head := range self.peerHeads {
		class, ephBucket, err := message.RetentionClassOf(ladder.retentionWire)
		if err != nil {
			return fmt.Errorf("%w: re-tracking leaf %d across the change to epoch %d: %w",
				ErrRecordOpen, ladder.leaf, newEpoch, err)
		}
		// THE SAME FLOOR [Group.trackLocked] TAKES, AND IT IS HERE FOR THE REASON THE MEMO
		// EXISTS: this re-track sets [Group.tracked], so a ladder installed too low here is one
		// trackLocked will not re-position on first sight. A leaf whose occupant has just been
		// removed has no row in this table at all (it was pruned two statements ago) and is
		// tracked lazily; a leaf that was REFILLED at some earlier commit has a row whose head is
		// this occupant's, which the floor can only agree with or raise.
		//
		// IT CANNOT RAISE ANYTHING IN THIS BUILD, AND THAT IS A THEOREM AND A MEASUREMENT RATHER
		// THAN A HOPE. The mutant that disables this clause leaves every case of this package
		// GREEN, because [Group.notePeerHeadLocked] raises the per-ladder head and the per-epoch
		// head from the same index in the same call, and [Group.openPageLocked] opens DURABLE
		// records ONLY -- so a leaf has exactly one ladder here and its head IS the leaf's floor.
		// The two separate the day this walk opens a second retention class: the index space is
		// one run per (group_id, sender_handle) across classes, so a leaf's first record in a new
		// class would meet a ladder at 0 while its stream stood at N, and N past
		// [messagegroup.DefaultRecordWindowSize] is the same abandonment this floor exists to
		// stop. It is written rather than deleted for that day, with what is claimed for it today
		// stated exactly: it is correct, it is unmeasurable, and the mutant that says so survives.
		if floor := self.leafStreamFloorLocked(ladder.leaf, newEpoch); head < floor {
			head = floor
		}
		if err := self.session.TrackSender(ladder.leaf, class, ephBucket, ladder.ephWindow, head); err != nil {
			return fmt.Errorf("%w: re-tracking leaf %d at head %d across the change to epoch %d: %w",
				ErrRecordOpen, ladder.leaf, head, newEpoch, err)
		}
		self.tracked[trackedKey{epoch: newEpoch, ladderKey: ladder}] = true
	}
	return nil
}

// trackLocked installs this sender's receiver ladder once and only once, AT THIS GROUP'S EPOCH.
//
// THE HEAD IS THE ONE THIS DEVICE HAS AUTHENTICATED FOR THIS SENDER AND NEVER 0. It used to be a
// literal 0, which was the only honest answer WHILE a group had one epoch: a ladder followed from
// its root opens the sender's whole stream. It stops being honest at the first membership change.
// The receiver ratchets are zeroized at every epoch install and the stream index is continuous
// across epochs, so a peer that had reached index N by the epoch before is at N+1 now -- and a
// ladder re-tracked at 0 answers ErrOutOfWindow for it once N passes
// [messagegroup.DefaultRecordWindowSize]. [Group.peerHeads] holds that authenticated head, keyed
// epoch-independent, and is 0 for a ladder never seen -- a member just added, or a fresh group at
// epoch one -- so this collapses to the old literal in every case that used to reach it and only
// differs after an epoch change. It is CALLER STATE and never a number off the record: TrackSender
// walks one expansion per index below the head, and peerHeads is only ever raised off a record
// this group's keys AUTHENTICATED (see [Group.notePeerHeadLocked]), so a peer cannot choose how far
// this device walks.
//
// AT THE RECORD'S EPOCH, since ledger item 241, which is this group's epoch for every record but
// one from an epoch this device has left; for those the ladder is installed in that epoch's own
// schedule, at the head [Group.pastHeadLocked] answers rather than the current one -- which a later
// epoch's records may already have raised above every record of the earlier one. The memo carries
// the epoch, so an epoch-one ladder installed during a walk at epoch two is not mistaken for the
// epoch-two one, and [Group.crossEpochLadderLocked] clears both at the next change in the same
// block as the session erases both.
//
// A refusal of the prior-epoch arm is passed through UNWRAPPED for the walk to branch on; see
// [Group.trackSessionLadderLocked].
func (self *Group) trackLocked(leaf uint32, header *message.RecordHeader) error {
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordOpen, err)
	}
	ladder := ladderKey{leaf: leaf, retentionWire: retentionWire, ephWindow: header.EphWindow}
	key := trackedKey{epoch: header.Epoch, ladderKey: ladder}
	if self.tracked[key] {
		return nil
	}
	head := self.peerHeads[ladder]
	if header.Epoch < self.epoch {
		head = self.pastHeadLocked(ladder, header.Epoch)
	}
	// AND THE FLOOR UNDER BOTH OF THEM, WHICH IS WHAT A LADDER OVER A LEAF THAT CHANGED HANDS
	// STANDS AT. Neither number above knows about the OTHER OCCUPANT of this leaf: the first is
	// pruned when a commit removes one ([Group.pruneRemovedLaddersLocked]) and the second is a
	// per-ladder read. The index space is per (group_id, sender_handle) and the server refuses
	// every index the previous occupant spent, so a newcomer's first record is above all of them
	// and a ladder at 0 is out of window as soon as that occupant passed
	// [messagegroup.DefaultRecordWindowSize]. See [Group.leafStreamFloorLocked].
	if floor := self.leafStreamFloorLocked(leaf, header.Epoch); head < floor {
		head = floor
	}
	if err := self.trackSessionLadderLocked(header.Epoch, leaf, header, head); err != nil {
		if errors.Is(err, messagegroup.ErrEpochOutOfWindow) || errors.Is(err, messagegroup.ErrPastEpochUnobtainable) {
			return err
		}
		return fmt.Errorf("%w: tracking leaf %d at head %d for epoch %d: %w", ErrRecordOpen, leaf, head, header.Epoch, err)
	}
	self.tracked[key] = true
	return nil
}

// notePeerHeadLocked raises [Group.peerHeads] for one peer ladder to a stream index this group's
// keys have just AUTHENTICATED, which is the head a later epoch change re-tracks that ladder at.
//
// IT IS RAISED ONLY OFF AN OPENED RECORD, never off a header, for [Group.trackLocked]'s reason: the
// head decides how far TrackSender walks, so a number a server chose would be a number of
// expansions a server chose. §3.1's stream_index is inside both AEADs, so a record that opened is
// one whose index this device's own key schedule agreed to.
func (self *Group) notePeerHeadLocked(leaf uint32, header *message.RecordHeader) {
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		// unreachable past an open: the class already went through the AEAD. Left as a nil-op
		// rather than a panic, because a head not raised is a re-track one index too low, which
		// the window absorbs, and a panic here would take down a Receive over a record that opened.
		return
	}
	ladder := ladderKey{leaf: leaf, retentionWire: retentionWire, ephWindow: header.EphWindow}
	// A RECORD FROM A PREVIOUS OCCUPANT OF THIS LEAF RAISES THIS HEAD LIKE ANY OTHER, AND THE
	// CLAUSE THAT USED TO REFUSE IT IS DELETED. It said the stream index "is not continuous across
	// two OCCUPANTS of one leaf: they are two streams that happen to share a name", and that
	// sentence is measurably false. An index is allocated per (group_id, sender_handle), a
	// sender_handle is SenderHandle(group_handle_key, leaf), and the server's monotonicity refuses
	// any index the previous occupant spent -- so the two occupants are ONE run of numbers, which
	// is the same fact [Group.seedOwnStreamLocked] makes the newcomer's own reserver obey. Holding
	// this head down left a survivor's ladder for a refilled leaf at 0, and a ladder at 0 is out of
	// window for every record the newcomer writes once the previous occupant passed
	// [messagegroup.DefaultRecordWindowSize] lines. See [Group.leafStreamFloorLocked], which is
	// where that floor is read back, and [Group.pruneRemovedLaddersLocked] for the measurement.
	if self.peerHeads[ladder] < header.StreamIndex {
		self.peerHeads[ladder] = header.StreamIndex
	}
	// AND THE PER-EPOCH HEAD, under the RECORD's epoch: a prior-epoch open raises the head of the
	// epoch it was sealed at and never the current epoch's, and it is this table that the disk
	// gets and a restart reads back. See [Group.pastHeadLocked] and [PeerHead].
	at := epochLadderKey{epoch: header.Epoch, ladderKey: ladder}
	if self.peerHeadsAt[at] < header.StreamIndex {
		self.peerHeadsAt[at] = header.StreamIndex
		self.headsDirty = true
	}
}

// persistPeerHeadsLocked writes [Group.peerHeadsAt] to the disk when it has risen since the last
// write, and is a no-op otherwise and on a store that is not durable.
//
// WHAT IS WRITTEN IS THE UNION OF WHAT THE DISK HELD AND WHAT THIS PROCESS AUTHENTICATED, per
// key at the higher of the two, so that a restart that opened nothing at some epoch does not
// write a table that forgets that epoch's head. The dirty bit stays set on a failed write, so
// the next walk tries again; the failure is returned so that a caller can see the restart it
// would cost, and it is returned AFTER the walk's own answer is committed because a head not
// written is a weaker fact than a record not opened.
func (self *Group) persistPeerHeadsLocked() error {
	if !self.headsDirty {
		return nil
	}
	merged := map[epochLadderKey]uint64{}
	for key, head := range self.persistedHeads {
		merged[key] = head
	}
	for key, head := range self.peerHeadsAt {
		if merged[key] < head {
			merged[key] = head
		}
	}
	heads := make([]PeerHead, 0, len(merged))
	for key, head := range merged {
		heads = append(heads, PeerHead{
			Epoch:         key.epoch,
			Leaf:          key.leaf,
			RetentionWire: key.retentionWire,
			EphWindow:     key.ephWindow,
			Head:          head,
		})
	}
	if err := self.device.persistPeerHeads(self.id, heads); err != nil {
		return fmt.Errorf("%w: group %x: %w", ErrPeerHeadsPersist, self.id, err)
	}
	self.headsDirty = false
	return nil
}

// leavesLocked is every member's sender_handle at THIS group's epoch, mapped to its leaf index.
func (self *Group) leavesLocked() (map[[16]byte]uint32, error) {
	return self.leavesAtLocked(self.epoch)
}

// leavesAtLocked is the handle table for ONE RECORD EPOCH: every leaf that stood in this group at
// `epoch`, mapped from the sender_handle its records carry. Ledger item 245's third piece.
//
// It is DERIVED and never read off a record: SenderHandle(group_handle_key, leaf) is the only
// thing that says which leaf a handle belongs to, and a table built from what arrived would let a
// sender name any leaf it liked.
//
// WHY IT TAKES AN EPOCH AT ALL, WHICH IS THE DEFECT. This function used to build the table from
// the CURRENT epoch's membership and hand the one answer to every record of a walk. A commit that
// removes a leaf at epoch n+1 takes that leaf out of the membership, so every record that leaf
// SEALED at epoch n -- records this device can still open, whose ladders it still holds, sitting
// below the commit in record order and re-met on every restart because the cursor is not
// persisted -- resolved to no leaf, took the fail() road with "which is no leaf of this group",
// and was abandoned after [maxRecordAttempts]. That is the ordinary first day of Remove and not a
// corner: the removing commit is the LAST record of the removed member's history, so its whole
// conversation is behind it.
//
// THE TWO SOURCES, AND EACH SAYS SOMETHING THE OTHER CANNOT. The current membership is the tree
// this device holds and is exact for the epoch it stands at. [Group.departedAt] is what this
// device watched leave, with the epoch the LAST removing commit OPENED, so a leaf whose occupants
// ended at `departed` had one of them standing at some epochs strictly below it -- which is the
// only fact about a leaf that is no longer in the tree that a receiver can hold without asking a
// party that could lie about it. A leaf that changed hands twice has one row and it carries the
// LAST departure, because the question here is "may a record at epoch e carry this leaf's handle"
// and that is true of every occupant, not only of the first
// ([Group.noteDepartedLeavesLocked]).
//
// WHAT IT IS AND WHAT IT IS NOT, because the difference is what keeps this cheap. It is a
// PRE-FILTER: it answers which leaf a handle names, and a handle for no leaf this group knows is
// refused here instead of being spent on three fetches. It is NOT an authenticator and the table
// is deliberately not exact -- a leaf ADDED after `epoch` carries the same handle at every epoch,
// so its row is present in an earlier epoch's table too, and a leaf REFILLED after a removal is
// one row that serves both occupants because the two derive the same sixteen octets. Neither
// widens anything: what decides that a record was really written by the leaf it names is MASTER
// section 8.4.3's R1 inside the open, which refuses any frame whose signing leaf's SenderHandle is
// not the one the record carries, and the identity this walk attributes the line to is read from
// THAT leaf at THAT epoch (see [Group.senderAtSendLocked]).
func (self *Group) leavesAtLocked(epoch uint64) (map[[16]byte]uint32, error) {
	leaves := map[[16]byte]uint32{}
	for at := 0; at < self.handle.MemberCount(); at += 1 {
		leaf, _, _, err := self.handle.MemberAt(at)
		if err != nil {
			return nil, fmt.Errorf("urmessage: the group's member %d: %w", at, err)
		}
		leaves[messagegroup.SenderHandle(self.groupHandleKey, leaf)] = leaf
	}
	for leaf, departed := range self.departedAt {
		if epoch < departed {
			leaves[messagegroup.SenderHandle(self.groupHandleKey, leaf)] = leaf
		}
	}
	return leaves, nil
}

// walkLeavesLocked is [Group.leavesAtLocked] with THE WALK'S OWN CACHE in front of it: one table
// per record epoch, built on the first record of that epoch and held for the rest of the walk.
//
// THE CACHE IS THE WALK'S AND NOT THE GROUP'S, and that is the same rule [pageWalk.unobtainable]
// follows one field up. A table is a fact about the tree AS THIS DEVICE HOLDS IT RIGHT NOW, and
// this device's tree moves mid-walk -- [Group.ingestCommitLocked] runs from the same loop -- so a
// table cached on the group would answer a later walk with a membership the commit has changed.
// [Group.ingestCommitLocked]'s step (8) drops the entries a commit can have moved rather than
// rebuilding one table, because the epochs BELOW the commit are exactly the ones it cannot move.
func (self *Group) walkLeavesLocked(walk *pageWalk, epoch uint64) (map[[16]byte]uint32, error) {
	if held, found := walk.leaves[epoch]; found {
		return held, nil
	}
	leaves, err := self.leavesAtLocked(epoch)
	if err != nil {
		return nil, err
	}
	walk.leaves[epoch] = leaves
	return leaves, nil
}

// noteOwnLeafLocked adds the handle this device's CURRENT leaf derives to [Group.ownHandles].
//
// IT IS CALLED AT EVERY CONSTRUCTION AND AT EVERY EPOCH INSTALL, and it only ever ADDS. RFC 9420
// does not move a member's leaf index under it, so on the ordinary path this is the same sixteen
// octets every time and the set has one entry for the life of the group -- which is exactly what
// makes the set free in the common case. The call at the epoch install is what catches the one
// shape that is not ordinary: a device re-admitted to a group it was removed from lands at
// whatever leaf the tree gives it, and its own earlier records are still under the old handle.
//
// A group with no handle yet (a [Group] built by a test with no MLS handle) notes nothing rather
// than panicking, which is the same guard [Group.initTables] has always needed for that population.
func (self *Group) noteOwnLeafLocked() {
	if self.handle == nil || len(self.groupHandleKey) == 0 || self.ownHandles == nil {
		return
	}
	self.ownHandles[messagegroup.SenderHandle(self.groupHandleKey, self.handle.OwnLeafIndex())] = true
}

// noteDepartedLeavesLocked files every leaf one commit REMOVED against the epoch that commit
// OPENED, so [Group.leavesAtLocked] can still resolve the records those leaves sealed BELOW it.
//
// THE HIGHEST EPOCH WINS, AND THE REASON IS WHAT THIS TABLE IS ASKED. A leaf can be removed,
// refilled and removed again, and the question [Group.leavesAtLocked] puts to it is "may a record
// sealed at epoch e carry this leaf's handle" -- which is TRUE for every occupant of it, not only
// the first. Keeping the LOWEST epoch, which this function used to do, answered `false` for the
// records the SECOND occupant sealed: with a leaf removed at epoch 2 and again at epoch 3, the
// middle occupant's own lines resolved to no leaf at all, took the fail() road and were abandoned
// after [maxRecordAttempts] at every survivor whose tree no longer carries that leaf.
//
// WHAT THE ENTRY MEANS, STATED AS THE ONE SENTENCE IT HAS TO KEEP TRUE: no occupant of this leaf
// stood at any epoch AT OR ABOVE this number, so a record above it cannot carry this leaf's
// handle unless the tree says the leaf is filled again. Below it the table over-claims -- it says
// nothing about the gaps between occupancies, when the leaf was blank -- and over-claiming is
// what this table already does for a leaf ADDED later ([Group.leavesAtLocked] says so in as many
// words): it is a PRE-FILTER, and what decides that a record was really written by the leaf it
// names is MASTER section 8.4.3's R1 inside the open.
//
// AND IT HAS EXACTLY ONE READER NOW. It used to have two, asking OPPOSITE questions of one number
// -- [Group.leavesAtLocked] wanted the first departure and [Group.notePeerHeadLocked] wanted the
// last -- and a single uint64 can only answer one of them. That second reader is deleted, for its
// own reasons; see [Group.notePeerHeadLocked].
func (self *Group) noteDepartedLeavesLocked(removed []uint32, opensEpoch uint64) {
	for _, leaf := range removed {
		if at, filed := self.departedAt[leaf]; filed && opensEpoch <= at {
			continue
		}
		self.departedAt[leaf] = opensEpoch
	}
}

// pruneRemovedLaddersLocked drops every piece of receiver-ladder bookkeeping this group holds for
// a leaf one commit has just REMOVED. Ledger item 245's second piece.
//
// WHY IT IS OWED, AND IT IS OWED WHETHER OR NOT ANYBODY EVER REFILLS THE LEAF. [Group.peerHeads]
// is kept across epoch changes by design -- it is the head [Group.crossEpochLadderLocked] re-tracks
// each peer at, and dropping it would starve a busy peer at the next commit -- and NOTHING pruned
// it. So the very next line of [Group.crossEpochLadderLocked] re-tracked a ladder for a leaf that
// no longer stands in the group, at the removed member's head, in a schedule that exists only to
// open records nobody can write any more.
//
// THE KEY STAYS THE LEAF AND THE HANDLE IS NOT ADDED TO IT, which is this item's own correction to
// its own text: [ladderKey] names a RECEIVER RATCHET, the session keys one per leaf, and a ladder
// key carrying a handle would be a second name for a thing that has one.
//
// ── ONE TABLE GOES AND TWO STAY, AND THAT CORRECTION IS MEASURED ─────────────────────────────
//
// THIS FUNCTION DROPPED THREE TABLES, AND DROPPING THE OTHER TWO ABANDONED THE NEWCOMER'S WHOLE
// HISTORY AT EVERY SURVIVOR. Its argument for them was that the newcomer's ladder is then
// "installed lazily at 0, which is the correct head for a stream that starts here". THE NEWCOMER'S
// STREAM DOES NOT START HERE. A stream index is allocated per (group_id, sender_handle), and
// MASTER section 8's monotonicity -- enforced by the server, per (group_id, sender_handle), with
// no epoch -- refuses every index the PREVIOUS OCCUPANT of this leaf already spent. So the
// newcomer's first ACCEPTED index is that occupant's high water plus one, which is exactly what
// [Group.seedOwnStreamLocked] makes the newcomer's own reserver agree to. A ladder at 0 is then
// not merely stale: it is too low by however far that occupant got, and
// [messagegroup.ReceiverRatchet] refuses an index more than
// [messagegroup.DefaultRecordWindowSize] ahead of its head. MEASURED, with a previous occupant of
// 1,025 lines: the newcomer's first record is at index 1,026 and the survivor answers "index 1026
// is 1026 ahead of head 0, and the window is 1024" -- three times, and then abandons it.
// [Group.leafStreamFloorLocked] is where a new occupant's ladder now takes its head from, and it
// reads [Group.peerHeadsAt] and [Group.persistedHeads]: which is why those two are no longer
// dropped here.
//
// WHAT IS STILL DROPPED, AND WHY. [Group.peerHeads] is the table [Group.crossEpochLadderLocked]
// iterates to RE-TRACK a ladder eagerly at every epoch change, and a row for a leaf whose occupant
// has left is a ratchet installed at every future epoch in a schedule nobody can write to. That is
// the cost this item's second piece names, and "prune `peerHeads` by `RemovedLeaves` at
// ApplyCommit" is the rule in its own words. The HEAD is not lost with the row: the per-epoch
// tables hold the same number and [Group.leafStreamFloorLocked] is what reads it back.
//
// AND [Group.tracked] IS NOT ONE OF THEM, WHICH WAS MEASURED RATHER THAN REASONED. This function
// had a fourth loop over that map, and it was UNDRIVEN: [Group.crossEpochLadderLocked] runs two
// statements after this one on the only path that reaches it, and its first line is
// `clear(self.tracked)`. Deleting the loop left all 177 cases of this package green, including the
// one that asserts no memo survives for the removed leaf -- because the clear is what empties it.
// A line no mutation can kill is a line that says something the code does not do.
func (self *Group) pruneRemovedLaddersLocked(removed []uint32) {
	if len(removed) == 0 {
		return
	}
	gone := map[uint32]bool{}
	for _, leaf := range removed {
		gone[leaf] = true
	}
	for ladder := range self.peerHeads {
		if gone[ladder.leaf] {
			delete(self.peerHeads, ladder)
		}
	}
}

// MemberWrapKey is one member of this group at its current epoch: the leaf it occupies, and the
// X-Wing encapsulation key that leaf publishes in its urmessage_leaf_keys extension.
//
// IT IS THE WIRE VALUE AND NOT A LOCAL ONE. [Group.MemberWrapKeys] reads it out of the ratchet
// tree through the seam's MemberAt, so a member's key here is the key that member's KeyPackage
// actually carried into this group and that every other member agrees on -- which is the only
// form of it a wrap could be addressed to. A device's own row is therefore the round trip that
// matters: the key in it must be the one [Device.DecapsulateToOwnLeaf] holds the seed for.
type MemberWrapKey struct {
	// The leaf index, which is what [messagegroup.WrapTargetHandle] and
	// [messagegroup.SenderHandle] are derived over.
	Leaf uint32

	// The encapsulation key, [messagegroup.XwingPublicKeySize] octets, ready for
	// [messagegroup.ParseXwingPublicKey]. A copy.
	XwingPub []byte
}

// MemberWrapKeys is one [MemberWrapKey] per member of this group at its current epoch.
//
// WHY IT EXISTS NOW, ahead of the wrap it will be used by. S2-26's property -- a device can open
// an encapsulation addressed to the leaf it publishes -- is only worth measuring against the key
// that TRAVELLED. A test that encapsulated to the device's own local [Device.leafKeys] would pass
// against a device whose published leaf carried something else entirely, which is the one failure
// the property exists to exclude. This is the read side of that measurement and it is the same
// read the epoch fan-out will make.
//
// A MEMBER WITH NO READABLE KEY IS A REFUSAL AND NOT A SKIPPED ROW, because a list that quietly
// dropped a member is ledger item 132's undercount arriving as a shorter slice.
//
// THE PARSE IS A SECOND COPY OF A CHECK THE SEAM ALREADY MAKES, and saying so is the point: the
// seam refuses a leaf carrying no urmessage_leaf_keys at all -- [Group.leavesLocked] and
// [Group.wrapTargetsAtLocked] lean on exactly that -- and the body it hands back was produced by
// `mls.LeafKeysExtension.Encode`, which refuses a wrong alg_id and a wrong length. So this parse
// cannot fail against this build's engine, and it is here to NARROW the value rather than to
// refuse one: what this method answers is a validated 1216-octet encapsulation key and not the
// octets it came in. It gets no sentinel of its own for that reason; see [ErrRestore]'s
// neighbours in errors.go.
func (self *Group) MemberWrapKeys() ([]MemberWrapKey, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	keys := make([]MemberWrapKey, 0, self.handle.MemberCount())
	for at := 0; at < self.handle.MemberCount(); at += 1 {
		leaf, _, leafKeys, err := self.handle.MemberAt(at)
		if err != nil {
			return nil, fmt.Errorf("urmessage: the group's member %d: %w", at, err)
		}
		parsed, err := parseLeafWrapKey(leafKeys)
		if err != nil {
			return nil, fmt.Errorf("urmessage: the group's member %d at leaf %d publishes a leaf keys body this build cannot read: %w",
				at, leaf, err)
		}
		keys = append(keys, MemberWrapKey{
			Leaf:     leaf,
			XwingPub: parsed,
		})
	}
	return keys, nil
}

// ── the rest of what a caller reads ──────────────────────────────────────────────────────────

// Id is this group's 32 octet identifier. A copy.
func (self *Group) Id() []byte {
	return append([]byte(nil), self.id...)
}

// Epoch is the epoch this group's session is at.
func (self *Group) Epoch() uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.epoch
}

// Open reports whether this device has published the group on the server.
func (self *Group) IsOpen() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.opened
}

// Messages is every message this group has sent or received, in the order it learned them.
//
// IT IS A SNAPSHOT AND THE WORD IS LOAD-BEARING (msgrepo ledger item 227). The slice is a copy, and
// so is every [Message] in it in the only sense that matters to a caller: NOTHING IN THIS PACKAGE
// EVER WRITES A [Message] AFTER HANDING IT OUT. A reaction or a tombstone arriving on a later
// [Group.Receive] builds a REPLACEMENT message and puts it in this group's log; what this call
// answered keeps saying what the conversation said at the instant it was asked.
//
// WHY THAT IS THE CONTRACT AND NOT "hold the lock while you render". This slice is what a UI
// paints, on its own thread and on its own schedule, while another thread polls [Group.Receive] --
// and a renderer cannot hold this group's mutex across a paint. Before the repair those two shared
// [Message.Deleted] and [Message.Reactions], which is a data race the detector reports and which
// every Go caller that rendered while it polled had.
//
// SO A CALLER THAT WANTS THE LATEST STATE ASKS AGAIN, which is what a render loop does anyway.
// Holding one of these messages and expecting a reaction to appear IN it is the one reading this
// method does not support.
func (self *Group) Messages() []*Message {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]*Message(nil), self.log...)
}

// UnopenedRecords is the record ids this group has GIVEN UP on: fetched [maxRecordAttempts] times,
// refused by the AEAD or the parser every time, and no longer asked for. Ascending, a copy.
//
// IT EXISTS SO THAT A HOLE IN A CONVERSATION HAS A NAME. [Stats.Unopened] is how many; this is
// which. A caller that shows nothing here is showing a conversation with records silently missing
// from it, which is the reading this whole method set exists to prevent.
func (self *Group) UnopenedRecords() []uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]uint64(nil), self.unopened...)
}

// IdentityInUse is the refusal this group is wedged on, or nil.
//
// NON-NIL MEANS ANOTHER DEVICE IS SEALING UNDER THIS DEVICE'S IDENTITY IN THIS GROUP -- a copy of
// the app-data folder. It is sticky, every [Group.Send] answers it, and a caller that shows it has
// the only sentence a user can act on: one of the two copies has to stop. See [Device.Restore] for
// what is detected and what is not.
func (self *Group) IdentityInUse() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.identityInUse
}

// Removal is RULING 52's state: the epoch this device was removed at, and the refusal that says so,
// or (0, nil) while this device is still a member.
//
// NON-NIL MEANS A VALID COMMIT TOOK THIS DEVICE OUT OF THE GROUP. It is sticky, persisted and
// permanent -- every [Group.Receive], [Group.Send] and commit verb answers it, this process and
// every one after it -- and it is [ErrRemovedFromGroup] wrapping mls's own answer, so a caller can
// errors.Is either. It is the one refusal in this package that will never clear: the repair is to be
// added to the group again, which is a new leaf at a new epoch and therefore a different group value.
//
// THE EPOCH IS THE LAST ONE THIS DEVICE WAS A MEMBER OF, which is also the highest one item 246's
// ceiling will serve it, so the history at and below it still fetches and still opens. That is what
// makes Spec C screen 10's read-only variant renderable rather than a blank pane: the transcript is
// there, the composer is not.
//
// IT IS ONE CALL AND NOT TWO BECAUSE THE TWO VALUES ARE ONE FACT. A separate epoch getter would be a
// second lock acquisition, and a caller that read the flag in one and the epoch in the other would
// be rendering a pair this group never held at once. The state is written once, so today they could
// not disagree; the signature is what keeps that true of tomorrow.
func (self *Group) Removal() (uint64, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.removed == nil {
		return 0, nil
	}
	return self.removedEpoch, self.removed
}

// Reconciled reports whether this group has compared its own stream position against the server's
// rows. A group created or joined in this process is reconciled from birth; a RESTORED one is not
// until [Group.Receive] has completed once, and [Group.Send] refuses until it has.
func (self *Group) Reconciled() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.reconciled
}

// Stats is what this group has seen.
func (self *Group) Stats() Stats {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.stats
}

// Close closes both sessions and the MLS handle under them.
func (self *Group) Close() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return nil
	}
	self.closed = true
	// THE POST-QUANTUM MATERIAL GOES FIRST AND IT GOES WHATEVER ELSE FAILS. The table is every
	// epoch's pq_secret and the staged candidates are other epochs' -- key material this type
	// holds in fields of its own, which is what connect/mls's erase gate refuses one package over
	// -- and a Close that returned early on a session error would leave all of it in the heap for
	// the collector to move around. Both erases are unconditional and neither can fail.
	self.zeroizePqSecretsLocked()
	self.zeroizeWrapCandidatesLocked()
	var first error
	if self.session != nil {
		if err := self.session.Close(); err != nil && first == nil {
			first = err
		}
	}
	if self.founding != nil {
		if err := self.founding.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := self.handle.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// rebind is [Group.rebindLocked] for a caller that does not hold the lock.
func (self *Group) rebind() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return nil
	}
	return self.rebindLocked()
}

// rebindLocked moves this group's sessions onto the connection's current nonce, when and only when
// the Hello count has moved since they were last bound.
//
// A FAILED REBIND IS RETURNED AND IS NEVER SWALLOWED. A session still MAC'ing under a nonce the
// server has destroyed produces records that are refused on the wire, and a caller that was told
// its send succeeded would have a message that silently never arrives.
func (self *Group) rebindLocked() error {
	nonce, nonceEpoch, err := self.device.nonce()
	if err != nil {
		return err
	}
	if self.founding != nil && self.foundingBound != nonceEpoch {
		if err := self.founding.RebindServerNonce(nonce); err != nil {
			return fmt.Errorf("%w: the founding session at epoch 0: %w", ErrNonceRebind, err)
		}
		self.foundingBound = nonceEpoch
	}
	if self.session != nil && self.sessionBound != nonceEpoch {
		if err := self.session.RebindServerNonce(nonce); err != nil {
			return fmt.Errorf("%w: the session at epoch %d: %w", ErrNonceRebind, self.epoch, err)
		}
		self.sessionBound = nonceEpoch
	}
	return nil
}
