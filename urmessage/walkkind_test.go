package urmessage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/protocol"
)

// ── the walk, over records that carry a content envelope ─────────────────────────────────────
//
// WHY THESE DRIVE THE REAL WALK AND NOT A PARSER. The unknown-kind rule is an ACCOUNTING rule --
// "keeps its position, is NOT a fail(), does not count toward ErrRecordAbandoned" -- and none of
// those words are about the codec. They are about [Group.openPageLocked], which is the only place
// fail(), resolve() and the cursor exist. A case over ParseContent alone cannot see any of it.
//
// The records are sealed by a REAL second member through a real GroupSession and opened by a real
// one, so what is measured is a record crossing the same seal/open path a server would carry it
// over. What is NOT here is the server: that is the cp3b module, and cp3b's own kinds case drives
// the same properties end to end.

// kindWalk is two members of one group, with the far side's [Group] available to drive a page
// through.
type kindWalk struct {
	t *testing.T

	groupId []byte
	alice   *messagegroup.GroupSession
	bob     *Group

	// the server's numbering, which is this harness's to assign because no server is here.
	nextRecordId uint64
}

// newKindWalk founds a group, adds a second member, and answers the pair.
func newKindWalk(t *testing.T) *kindWalk {
	t.Helper()
	root := t.TempDir()
	alice := openCrossProcessDevice(t, filepath.Join(root, "alice"))
	t.Cleanup(alice.close)
	bob := openCrossProcessDevice(t, filepath.Join(root, "bob"))
	t.Cleanup(bob.close)

	groupId := make([]byte, GroupIdBytes)
	if _, err := rand.Read(groupId); err != nil {
		t.Fatalf("drawing a group id: %v", err)
	}
	aliceHandle := alice.createGroup(t, groupId)
	t.Cleanup(func() { aliceHandle.Close() })

	mlsSecret, err := aliceHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("pq_secret: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

	keyPackage, err := bob.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("bob's key package: %v", err)
	}
	if _, err := aliceHandle.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := aliceHandle.Commit(nil)
	if err != nil {
		aliceHandle.ClearPendingCommit()
		t.Fatalf("Commit: %v", err)
	}
	if err := aliceHandle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	bobHandle, err := bob.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("bob's JoinFromWelcome: %v", err)
	}
	t.Cleanup(func() { bobHandle.Close() })

	aliceSession := newCrossProcessSession(t, aliceHandle, pqSecret, groupHandleKey, alice.reserver, "alice's nonce")
	t.Cleanup(func() { aliceSession.Close() })
	bobSession := newCrossProcessSession(t, bobHandle, pqSecret, groupHandleKey, bob.reserver, "bob's nonce")
	t.Cleanup(func() { bobSession.Close() })

	// BOB'S GROUP IS BUILT FIELD BY FIELD AND NOT THROUGH [Device.Join], because Join needs a
	// transport and a server nonce and this case needs neither: everything below the fetch is
	// what is under test. `reconciled` is true for the reason a joined group's is -- this
	// device's stream in this group starts here -- and it also keeps commitWalkLocked off the
	// reserver, which a group with no device could not reach.
	//
	// IT HAS A DEVICE SINCE LEDGER ITEM 241, and the smallest one: the walk's commit persists
	// the receiver heads it authenticated through [Device.persistPeerHeads], which reaches the
	// device's store and nothing else of it. The store is bob's own durable one, so the heads
	// land in a directory this case owns.
	bobGroup := &Group{
		device:         &Device{stateStore: bob.store, engine: bob.engine, reserver: bob.reserver},
		id:             append([]byte(nil), groupId...),
		handle:         bobHandle,
		groupHandleKey: groupHandleKey,
		pqSecrets:      map[uint64][]byte{bobHandle.Epoch(): pqSecret},
		session:        bobSession,
		epoch:          bobHandle.Epoch(),
		opened:         true,
		reconciled:     true,
		// this member is named in the FOUNDING commit, so no leaf here was ever occupied by
		// anybody else and its stream floor is known without asking the server.
		ownFloorHeld: true,
	}
	bobGroup.initTables()
	return &kindWalk{t: t, groupId: groupId, alice: aliceSession, bob: bobGroup, nextRecordId: 1}
}

// sealed is one of alice's records and the message_id every member derives for it.
type sealed struct {
	recordId  uint64
	record    *message.Record
	messageId []byte
}

// seal seals one application plaintext as alice, DURABLE, and gives it the next record id.
func (self *kindWalk) seal(plaintext []byte) *sealed {
	self.t.Helper()
	record, err := self.alice.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), plaintext, 0, nil)
	if err != nil {
		self.t.Fatalf("alice's SealRecord: %v", err)
	}
	messageId, err := self.alice.MessageIdOf(&record.Header)
	if err != nil {
		self.t.Fatalf("alice's MessageIdOf: %v", err)
	}
	one := &sealed{recordId: self.nextRecordId, record: record, messageId: messageId[:]}
	self.nextRecordId += 1
	return one
}

// deliver walks one page of already sealed records through bob's group, in the order given, the way
// [Group.Receive] walks one: a fresh [pageWalk] from the group's cursor, [Group.openPageLocked] over
// the page, then [Group.commitWalkLocked] to fold it back in. It answers what the caller would see.
func (self *kindWalk) deliver(page ...*sealed) ([]*Message, error) {
	self.t.Helper()
	own, err := self.bob.session.SenderHandle()
	if err != nil {
		self.t.Fatalf("bob's sender handle: %v", err)
	}
	self.bob.ownHandles[own] = true
	walk := &pageWalk{
		// the same three lines [Group.Receive] writes; the handle table is per record epoch
		// (ledger item 245) and [Group.walkLeavesLocked] fills it as the walk meets each epoch.
		own:        self.bob.ownHandles,
		ownNow:     own,
		leaves:     map[uint64]map[[16]byte]uint32{},
		opened:     []*Message{},
		from:       self.bob.cursor,
		reached:    self.bob.cursor,
		resolvedTo: self.bob.cursor,
		reconciled: self.bob.reconciled,
		complete:   true,
	}
	rows := []*protocol.Record{}
	for _, one := range page {
		encoded, err := message.EncodeRecord(one.record)
		if err != nil {
			self.t.Fatalf("encoding record %d: %v", one.recordId, err)
		}
		rows = append(rows, &protocol.Record{RecordId: one.recordId, RecordBytes: encoded})
	}
	self.bob.openPageLocked(&protocol.FetchResponse{Records: rows}, walk)
	// nil for the reason pqrotation_test.go names: no fetch, so no transport refusal.
	return walk.opened, self.bob.commitWalkLocked(walk, nil)
}

// held answers the [Message] bob's group holds under one message_id, RIGHT NOW.
//
// IT IS CALLED AFTER EVERY DELIVERY AND NEVER HELD ACROSS ONE, which is a requirement of the cases
// below rather than a style: since ledger item 227 a rebuild REPLACES the message it rebuilt, so a
// pointer taken before an effect arrives is a snapshot from before it -- which is the property
// TestAMessageAlreadyHandedToACallerIsNeverWrittenAgain exists to hold.
func (self *kindWalk) held(messageId []byte) *Message {
	self.t.Helper()
	held, _ := self.bob.heldLocked(messageId)
	return held
}

// A KIND THIS BUILD DOES NOT KNOW KEEPS ITS POSITION AND IS NOT A FAILURE.
//
// This is the half of the unknown-kind rule that is about the WALK rather than about the codec, and
// every clause of it is a separate thing that could be got wrong:
//
//   - it is NOT a fail(): [Stats.FailedOpen] does not move, so the cursor is not held back and the
//     next fetch does not ask for the record again;
//   - it does not count toward [ErrRecordAbandoned]: [Stats.Unopened] stays zero and Receive
//     answers a nil error;
//   - it keeps its POSITION: the cursor resolves past it, and the records after it are delivered;
//   - it keeps its message_id, which is what a later kind naming it would quote;
//   - it renders as one closed PLACEHOLDER: a [Message] carrying [GapUnsupported], the code that
//     arrived, and no text. The reason is what makes it a placeholder rather than a blank line: a
//     UI holding an entry with an unknown Kind and an empty Text cannot tell a newer feature from
//     somebody sending nothing, and that was the whole of ledger item 221's sdk half;
//   - AND IT IS NEVER PARSED AS A KNOWN KIND, which is why the body below would be a perfectly
//     good REPLY if anything guessed.
//
// WHAT WOULD GO RED IF THE RULE WERE AN ORDINARY REFUSAL: every clause but the last.
func TestAnUnknownKindKeepsItsPositionAndIsNotAFailure(t *testing.T) {
	world := newKindWalk(t)
	target := aTarget(0x99)

	before := world.seal(mustEncodeText(t, "a line before"))
	unknown := world.seal(append(append([]byte{byte(KindEdit)}, target...), "a body a later build reads"...))
	after := world.seal(mustEncodeText(t, "a line after"))

	opened, err := world.deliver(before, unknown, after)
	if err != nil {
		t.Fatalf("the walk answered %v, and an unknown kind is not a failure", err)
	}
	if len(opened) != 3 {
		t.Fatalf("the walk delivered %d message(s) over three records", len(opened))
	}
	stats := world.bob.stats
	if stats.FailedOpen != 0 || stats.Unopened != 0 {
		t.Errorf("an unknown kind moved FailedOpen to %d and Unopened to %d, and it is neither",
			stats.FailedOpen, stats.Unopened)
	}
	if world.bob.cursor != after.recordId {
		t.Errorf("the cursor resolved to %d and the page ended at record %d, so the unknown kind held it back",
			world.bob.cursor, after.recordId)
	}

	placeholder := opened[1]
	if placeholder.Gap != GapUnsupported {
		t.Errorf("the placeholder carries gap reason %q, want %q: without it a UI cannot tell a newer feature from a blank message",
			placeholder.Gap, GapUnsupported)
	}
	if stats.GapUnsupported != 1 || stats.GapMalformed != 0 {
		t.Errorf("GapUnsupported %d and GapMalformed %d over one unknown kind; a future kind is not malformed",
			stats.GapUnsupported, stats.GapMalformed)
	}
	if placeholder.Kind != KindEdit {
		t.Errorf("the placeholder carries kind %s, want the code that arrived", placeholder.Kind)
	}
	if placeholder.Text != "" {
		t.Errorf("an unknown kind was rendered as text %q", placeholder.Text)
	}
	if placeholder.ReplyToId != nil {
		t.Errorf("an unknown kind's body was read as a reply_to: %x", placeholder.ReplyToId)
	}
	if !bytes.Equal(placeholder.MessageId, unknown.messageId) {
		t.Errorf("the placeholder's message_id is %x and the record's is %x",
			placeholder.MessageId, unknown.messageId)
	}
	if placeholder.RecordId != unknown.recordId {
		t.Errorf("the placeholder is record %d and the record is %d", placeholder.RecordId, unknown.recordId)
	}
	// and the records either side of it arrived as themselves
	if opened[0].Text != "a line before" || opened[2].Text != "a line after" {
		t.Errorf("the lines around the unknown kind came back as %q and %q", opened[0].Text, opened[2].Text)
	}
}

// A MALFORMED BODY IS A GAP, RESOLVED ONCE, AND IS NEVER RETRIED. THIS IS LEDGER ITEM 224.
//
// THE CASE THIS REPLACES asserted the opposite and is worth stating, because the two properties are
// mutually exclusive and only one of them can be the contract. It was
// TestAMalformedBodyIsARefusalThatHoldsTheCursor, and it asserted a fail(): FailedOpen at 1, the
// cursor held at the record BEFORE the malformed one, [ErrContentMalformed] through [ErrRecordOpen]
// on the way back, then [maxRecordAttempts] re-deliveries ending in [ErrRecordAbandoned] with
// Unopened at 1. Every one of those is now false, deliberately.
//
// THE BOUNDARY IS STRUCTURAL AND IT IS WHY. A malformed body is raised AFTER OpenRecord returned, so
// the AEADs opened, the signature verified and the ratchet generation is already spent. What is left
// in dispute is grammar, and the server has no other octets to hand over -- so the three re-fetches
// the old path spent could not have changed the answer, and the loud failure at the end of them was
// a hole where a readable gap belongs.
//
// AND THE LOUDNESS IS WHAT THIS CASE GUARDS, because that is what the repair spends. [Group.Receive]
// now answers nil for this record, so a caller watching only the error learns nothing. Two things
// are left to be loud with and BOTH are asserted here: [Stats.GapMalformed] moves, and the entry
// itself comes back carrying [GapMalformed]. Delete either and a malformed record becomes silent,
// which is the regression this repair must not be.
func TestAMalformedBodyIsAGapResolvedOnceAndIsNotRetried(t *testing.T) {
	world := newKindWalk(t)

	good := world.seal(mustEncodeText(t, "a line"))
	// a TEXT whose required tail is empty: one octet of plaintext, and R-d's third clause
	malformed := world.seal([]byte{byte(KindText)})
	after := world.seal(mustEncodeText(t, "a line after"))

	opened, err := world.deliver(good, malformed, after)
	if err != nil {
		t.Fatalf("the walk answered %v, and a malformed body is a gap rather than a failure", err)
	}
	if len(opened) != 3 {
		t.Fatalf("the walk delivered %d message(s) over three records; the gap is an entry and not a silence",
			len(opened))
	}

	// ── it is LOUD: the two carriers the error used to be ───────────────────────────────────
	gap := opened[1]
	if gap.Gap != GapMalformed {
		t.Errorf("the gap carries reason %q, want %q", gap.Gap, GapMalformed)
	}
	if world.bob.stats.GapMalformed != 1 {
		t.Errorf("Stats.GapMalformed is %d, want 1; with the error gone this counter is half of what is left to be loud with",
			world.bob.stats.GapMalformed)
	}
	if world.bob.stats.GapUnsupported != 0 {
		t.Errorf("a malformed body moved Stats.GapUnsupported to %d; the two reasons are counted apart because they are two different sentences",
			world.bob.stats.GapUnsupported)
	}
	if gap.Text != "" {
		t.Errorf("a body this build refused was quoted back as text %q", gap.Text)
	}
	if !bytes.Equal(gap.MessageId, malformed.messageId) {
		t.Errorf("the gap's message_id is %x and the record's is %x", gap.MessageId, malformed.messageId)
	}
	if gap.RecordId != malformed.recordId {
		t.Errorf("the gap is record %d and the record is %d", gap.RecordId, malformed.recordId)
	}

	// ── and it is NOT a fail(): no retry, no abandonment, the cursor moves ───────────────────
	if world.bob.stats.FailedOpen != 0 || world.bob.stats.Unopened != 0 {
		t.Errorf("a malformed body moved FailedOpen to %d and Unopened to %d, and a record that OPENED is neither",
			world.bob.stats.FailedOpen, world.bob.stats.Unopened)
	}
	if world.bob.cursor != after.recordId {
		t.Errorf("the cursor resolved to %d and the page ended at record %d, so the malformed record held it back",
			world.bob.cursor, after.recordId)
	}
	if attempts := world.bob.attempts[malformed.recordId]; attempts != 0 {
		t.Errorf("the malformed record was counted as %d attempt(s); a disagreement about grammar has nothing to retry",
			attempts)
	}
	if opened[0].Text != "a line" || opened[2].Text != "a line after" {
		t.Errorf("the lines around the gap came back as %q and %q", opened[0].Text, opened[2].Text)
	}

	// AND NO LATER WALK ASKS FOR IT AGAIN. Under the old contract this loop was the abandonment
	// bound and each turn of it answered an error; the record is now already resolved, so a rewind
	// that passes back over it skips it as one this group holds.
	for attempt := 2; attempt <= maxRecordAttempts+1; attempt += 1 {
		again, err := world.deliver(malformed)
		if err != nil {
			t.Fatalf("re-delivery %d of the malformed record answered %v", attempt, err)
		}
		if len(again) != 0 {
			t.Fatalf("re-delivery %d of the malformed record delivered it a second time", attempt)
		}
	}
	if world.bob.stats.GapMalformed != 1 {
		t.Errorf("Stats.GapMalformed is %d after %d re-deliveries, want 1: one record is one gap",
			world.bob.stats.GapMalformed, maxRecordAttempts)
	}
	if world.bob.stats.Unopened != 0 {
		t.Errorf("Unopened is %d, and past the old bound there is now nothing to abandon", world.bob.stats.Unopened)
	}
}

// THE TWO GAP REASONS ARE NOT INTERCHANGEABLE, AND THE DISTINCTION IS LOAD-BEARING IN BOTH
// DIRECTIONS.
//
// Spec A §7.4 states it as a rule about what a user is TOLD, which is why a build may not pick
// either value and be done: a build that reported a future kind as "malformed" would ACCUSE CORRECT
// SENDERS, and one that reported a genuinely malformed body as "unsupported" would tell a user to
// upgrade out of a bug that no upgrade fixes. Spec C §5.1 carries the two sentences and only the
// unsupported one has an upgrade affordance.
//
// ONE PAGE, BOTH RECORDS, SO NEITHER VALUE CAN BE A CONSTANT. A build that hard-coded either reason
// passes every case that carries one record and fails this one.
func TestAFutureKindAndABrokenBodyAreTwoDifferentGaps(t *testing.T) {
	world := newKindWalk(t)
	target := aTarget(0x77)

	// 0x08 is EDIT: in the registry, no grammar in this build, legal on a stored class
	future := world.seal(append(append([]byte{byte(KindEdit)}, target...), "a body a later build reads"...))
	// a REACTION_ADD whose target is one octet short: a rule that is already written, broken
	broken := world.seal(append([]byte{byte(KindReactionAdd)}, target[:MessageIdBytes-1]...))

	opened, err := world.deliver(future, broken)
	if err != nil {
		t.Fatalf("the walk answered %v", err)
	}
	if len(opened) != 2 {
		t.Fatalf("two gaps delivered %d entr(ies)", len(opened))
	}
	if opened[0].Gap != GapUnsupported {
		t.Errorf("a kind this build does not know came back as %q, and a future kind is not malformed",
			opened[0].Gap)
	}
	if opened[1].Gap != GapMalformed {
		t.Errorf("a body that broke a rule already written came back as %q, which would tell a user to upgrade out of a bug no upgrade fixes",
			opened[1].Gap)
	}
	stats := world.bob.stats
	if stats.GapUnsupported != 1 || stats.GapMalformed != 1 {
		t.Errorf("GapUnsupported %d and GapMalformed %d over one of each", stats.GapUnsupported, stats.GapMalformed)
	}
}

// A MALFORMED RECORD WHOSE CODE IS AN EFFECT'S IS STILL A VISIBLE GAP.
//
// THIS IS THE CLAUSE A READER WOULD DELETE AS TIDY-UP. [Message.Kind] on a gap is the code the
// record ARRIVED under and not what the record is, so a REACTION_ADD with a short target carries
// [KindReactionAdd] -- and the sender chooses those octets. Without the gap check at the head of
// [Group.deliverLocked], effectOf reads it as an effect standing on a target of thirty-two zero
// octets, deliverLocked answers false, and the gap is never shown: a record any member can make
// disappear from any other member's screen by sending a broken reaction body.
//
// The COVER arm is the same hole through the other rule: a malformed COVER would be swallowed by
// "a COVER adds no line", which is a promise about a COVER this build could read.
func TestAMalformedEffectAndAMalformedCoverAreGapsAndNotSilences(t *testing.T) {
	world := newKindWalk(t)
	target := aTarget(0x88)

	shortTarget := world.seal(append([]byte{byte(KindReactionAdd)}, target[:MessageIdBytes-1]...))
	tombstoneWithATail := world.seal(append(append([]byte{byte(KindTombstone)}, target...), 0x00))
	fatCover := world.seal([]byte{byte(KindCover), 0x00})

	opened, err := world.deliver(shortTarget, tombstoneWithATail, fatCover)
	if err != nil {
		t.Fatalf("the walk answered %v", err)
	}
	if len(opened) != 3 {
		kinds := []ContentKind{}
		for _, one := range opened {
			kinds = append(kinds, one.Kind)
		}
		t.Fatalf("three malformed records whose codes are effect and cover codes delivered %d entr(ies): %v; a gap that is swallowed is a record made to vanish",
			len(opened), kinds)
	}
	for index, one := range opened {
		if one.Gap != GapMalformed {
			t.Errorf("entry %d carries reason %q, want %q", index, one.Gap, GapMalformed)
		}
	}
	if world.bob.stats.GapMalformed != 3 {
		t.Errorf("Stats.GapMalformed is %d over three malformed records", world.bob.stats.GapMalformed)
	}
	// AND NONE OF THEM LANDED AS AN EFFECT. effectsOn is keyed by target, and a gap read as an
	// effect would be held under thirty-two zero octets.
	if held := world.bob.effectsOn[messageKeyOf(make([]byte, MessageIdBytes))]; len(held) != 0 {
		t.Errorf("%d malformed record(s) were held as effects on a target of zero octets", len(held))
	}
}

// A GAP REPORTS THE CODE THE RECORD ARRIVED UNDER, AND 0x00 WHEN IT ARRIVED UNDER NONE.
//
// The codec answers NO entry for a malformed plaintext, so the walk builds one from octet 0 -- and
// the two arms of that are separately wrong-able. A gap that reported no code at all would leave a
// caller with "something is here" and nothing else; a gap that reported a code for a plaintext with
// no octet 0 would be reporting a code nobody sent.
//
// AN EMPTY APPLICATION PLAINTEXT ANSWERS [KindReserved] AND THAT IS NOT A SUBSTITUTION. 0x00 is the
// code refused on every retention class always, and kind.go's registry already rules that "the
// plaintext is empty" and "the plaintext says nothing" are ONE refusal rather than two -- so 0x00 is
// exactly what was concluded. IT IS REACHABLE: an empty plaintext seals and fetches like any other,
// measured here rather than assumed.
func TestAGapReportsTheCodeItArrivedUnderAndZeroWhenThereWasNone(t *testing.T) {
	world := newKindWalk(t)

	empty := world.seal([]byte{})
	shortText := world.seal([]byte{byte(KindText)})

	opened, err := world.deliver(empty, shortText)
	if err != nil {
		t.Fatalf("the walk answered %v", err)
	}
	if len(opened) != 2 {
		t.Fatalf("two malformed records delivered %d entr(ies)", len(opened))
	}
	if opened[0].Gap != GapMalformed || opened[0].Kind != KindReserved {
		t.Errorf("an EMPTY application plaintext came back as a %q gap of kind %s, want a %q gap of %s",
			opened[0].Gap, opened[0].Kind, GapMalformed, KindReserved)
	}
	if opened[1].Gap != GapMalformed || opened[1].Kind != KindText {
		t.Errorf("a TEXT with an empty tail came back as a %q gap of kind %s, want a %q gap of %s: a gap says what arrived",
			opened[1].Gap, opened[1].Kind, GapMalformed, KindText)
	}
	if world.bob.stats.GapMalformed != 2 {
		t.Errorf("Stats.GapMalformed is %d over two malformed records", world.bob.stats.GapMalformed)
	}
}

// A GAP CANNOT BE REACTED TO OR DELETED, WHATEVER CODE IT ARRIVED UNDER.
//
// T-a is that a tombstone and a reaction apply to a stored CONTENT message, and a gap is by
// definition the absence of one. The clause is load-bearing for the same reason the one in
// deliverLocked is: a malformed REPLY body carries [KindReply], which is a kind the switch in
// reactableLocked ALLOWS, so without the gap check a member could seal a reaction against a record
// this device could not read -- quoting a message_id whose content no two members can agree on.
func TestAGapIsNotReactableWhateverCodeItArrivedUnder(t *testing.T) {
	world := newKindWalk(t)
	target := aTarget(0x66)

	// a REPLY with a whole reply_to and an empty text: malformed, and its code is KindReply
	brokenReply := world.seal(append([]byte{byte(KindReply)}, target...))
	line := world.seal(mustEncodeText(t, "a line that is reactable"))

	opened, err := world.deliver(brokenReply, line)
	if err != nil {
		t.Fatalf("the walk answered %v", err)
	}
	if len(opened) != 2 {
		t.Fatalf("the walk delivered %d entr(ies)", len(opened))
	}
	if opened[0].Gap != GapMalformed || opened[0].Kind != KindReply {
		t.Fatalf("this case needs a gap whose code is REPLY, and it is a %q gap of kind %s",
			opened[0].Gap, opened[0].Kind)
	}

	world.bob.mutex.Lock()
	defer world.bob.mutex.Unlock()
	if _, err := world.bob.reactableLocked(brokenReply.messageId); !errors.Is(err, ErrNoSuchMessage) {
		t.Errorf("a malformed REPLY gap answered %v to reactableLocked, want a refusal", err)
	}
	// THE CONTROL, which is what says the refusal is about the GAP and not about this group holding
	// nothing: the TEXT beside it in the same page is reactable.
	if _, err := world.bob.reactableLocked(line.messageId); err != nil {
		t.Errorf("the control: the TEXT in the same page answered %v", err)
	}
}

// A RECORD THAT CHANGES ANOTHER MESSAGE IS NOT A LINE OF THE CONVERSATION.
//
// A reaction, a tombstone and a COVER each open, each resolve the cursor, and each add NO entry.
// The reaction and the tombstone land on the message they name; the COVER lands nowhere, which is
// the whole of what it is for -- a COVER that produced an entry would be cover traffic the user can
// see.
func TestAReactionATombstoneAndACoverAddNoLineOfTheirOwn(t *testing.T) {
	world := newKindWalk(t)

	line := world.seal(mustEncodeText(t, "a line worth reacting to"))
	opened, err := world.deliver(line)
	if err != nil || len(opened) != 1 {
		t.Fatalf("the line: %d message(s), %v", len(opened), err)
	}

	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "👍"))
	tombstone := world.seal(mustEncodeTombstone(t, line.messageId))
	cover := world.seal(encodeCover())

	opened, err = world.deliver(add, tombstone, cover)
	if err != nil {
		t.Fatalf("the walk answered %v", err)
	}
	if len(opened) != 0 {
		kinds := []ContentKind{}
		for _, one := range opened {
			kinds = append(kinds, one.Kind)
		}
		t.Fatalf("three records that add no line delivered %d message(s): %v", len(opened), kinds)
	}
	if world.bob.cursor != cover.recordId {
		t.Errorf("the cursor resolved to %d and the page ended at record %d", world.bob.cursor, cover.recordId)
	}
	if world.bob.stats.FailedOpen != 0 {
		t.Errorf("FailedOpen is %d and none of the three is a failure", world.bob.stats.FailedOpen)
	}

	held := world.held(line.messageId)
	if held == nil {
		t.Fatal("the line is not in this group's index")
	}
	if len(held.Reactions) != 1 || held.Reactions[0].Emoji != "👍" {
		t.Errorf("the line carries %v, want one 👍", held.Reactions)
	}
	if !held.Deleted {
		t.Error("the tombstone was sealed by the line's own sender and the line is not marked deleted")
	}
	// AND THE TEXT IS STILL THERE. A tombstone is a mark, not an erase: this package refuses to
	// decide what a UI does with a deleted line, and the record is on the server either way.
	if held.Text != "a line worth reacting to" {
		t.Errorf("the deleted line's text came back as %q", held.Text)
	}
}

// AN EFFECT WHOSE TARGET HAS NOT ARRIVED IS HELD, NOT DROPPED.
//
// The survey measured that the walk's order is not the conversation's order: a record that fails to
// open holds the cursor back and is re-delivered AFTER record ids above it, and after
// [maxRecordAttempts] it is abandoned and never delivered at all. So a reaction can arrive before
// the message it names, and a receiver that dropped it would lose a reaction permanently for a
// transient this build is designed to recover from.
func TestAnEffectThatArrivesBeforeItsTargetIsHeldAndApplied(t *testing.T) {
	world := newKindWalk(t)

	line := world.seal(mustEncodeText(t, "a line that arrives second"))
	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "🎯"))

	// the reaction alone, naming a message this group has never seen
	opened, err := world.deliver(add)
	if err != nil {
		t.Fatalf("a reaction for a target that has not arrived answered %v", err)
	}
	if len(opened) != 0 {
		t.Fatalf("the reaction delivered %d message(s)", len(opened))
	}
	if world.bob.cursor != add.recordId {
		t.Errorf("the cursor resolved to %d and the reaction is record %d", world.bob.cursor, add.recordId)
	}

	// and now the target
	opened, err = world.deliver(line)
	if err != nil {
		t.Fatalf("the target answered %v", err)
	}
	if len(opened) != 1 {
		t.Fatalf("the target delivered %d message(s)", len(opened))
	}
	if len(opened[0].Reactions) != 1 || opened[0].Reactions[0].Emoji != "🎯" {
		t.Errorf("the target arrived carrying %v, and a reaction sealed before it was held for it",
			opened[0].Reactions)
	}
}

// EFFECTS ARE APPLIED IN SERVER ORDER AND NOT IN ARRIVAL ORDER.
//
// THIS IS THE CASE THE SURVEY'S MEASUREMENT IS FOR. An ADD at record 2 and a REMOVE at record 3 that
// arrive in the order 3, 2 -- which is exactly what one failed open produces, because the failure
// holds the cursor and the record is re-delivered behind ids above it -- leave the reaction STANDING
// under apply-as-it-lands and REMOVED under a replay in record_id order. §5.3 says records are
// ordered by server order, so the second is the answer.
//
// WHAT WOULD GO RED: apply each effect once as it lands ([Group.reapplyLocked] deleted, its body
// inlined into noteEffectLocked) and this case sees a 👍 that the group's own records say was taken
// back.
func TestEffectsAreAppliedInServerOrderAndNotArrivalOrder(t *testing.T) {
	world := newKindWalk(t)

	line := world.seal(mustEncodeText(t, "a line reacted to and un-reacted to"))
	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "👍"))
	remove := world.seal(mustEncodeReaction(t, KindReactionRemove, line.messageId, "👍"))
	if !(add.recordId < remove.recordId) {
		t.Fatalf("this case needs the ADD to be numbered before the REMOVE, and they are %d and %d",
			add.recordId, remove.recordId)
	}

	// the line and the REMOVE arrive; the ADD is the record that did not open the first time
	if _, err := world.deliver(line, remove); err != nil {
		t.Fatalf("the first page answered %v", err)
	}
	if held := world.held(line.messageId); len(held.Reactions) != 0 {
		t.Fatalf("a REMOVE with no ADD before it left %v standing", held.Reactions)
	}

	// and now the ADD, with a LOWER record id, after them
	if _, err := world.deliver(add); err != nil {
		t.Fatalf("the re-delivered ADD answered %v", err)
	}
	held := world.held(line.messageId)
	if len(held.Reactions) != 0 {
		t.Errorf("the ADD (record %d) arrived after the REMOVE (record %d) and left %v standing; in server order the REMOVE is last",
			add.recordId, remove.recordId, held.Reactions)
	}

	// THE CONTROL, which is what keeps the assertion above from passing for a build that drops
	// every reaction: the same two records in the other order leave the reaction standing.
	second := newKindWalk(t)
	secondLine := second.seal(mustEncodeText(t, "a line reacted to"))
	secondRemove := second.seal(mustEncodeReaction(t, KindReactionRemove, secondLine.messageId, "👍"))
	secondAdd := second.seal(mustEncodeReaction(t, KindReactionAdd, secondLine.messageId, "👍"))
	if _, err := second.deliver(secondLine, secondAdd); err != nil {
		t.Fatalf("the control's first page answered %v", err)
	}
	if _, err := second.deliver(secondRemove); err != nil {
		t.Fatalf("the control's second page answered %v", err)
	}
	if held := second.held(secondLine.messageId); len(held.Reactions) != 1 {
		t.Errorf("the control: the REMOVE is record %d and the ADD is record %d, so in server order the ADD is last and the reaction stands; it carries %v",
			secondRemove.recordId, secondAdd.recordId, held.Reactions)
	}
}

// ONE RECORD IS ONE EFFECT, however many times a rewind walks back over it, AND THERE ARE TWO
// LINES OF DEFENCE RATHER THAN ONE.
//
// A record that did not open holds the cursor back, so the NEXT fetch re-reads every record after
// it -- which is the ordinary shape of this build's retry. A reaction counted twice would show one
// member reacting twice, and a REMOVE counted twice would be harmless only by accident.
//
// THE FIRST DEFENCE IS THE WALK'S [Group.delivered] MAP, which skips a record id this group has
// already shown, and it is what the first half below measures. THE SECOND IS [Group.effects],
// keyed by the effect record's own message_id, and the second half reaches it directly because the
// first makes it unreachable through the walk. MEASURED: with the effects map's dedupe deleted and
// only the append left, the walk half of this case stays GREEN -- so it is the second half, and
// nothing else in either suite, that holds the codec-level half of the rule.
func TestAReDeliveredEffectIsStillOneEffect(t *testing.T) {
	world := newKindWalk(t)
	line := world.seal(mustEncodeText(t, "a line"))
	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "👍"))

	if _, err := world.deliver(line, add); err != nil {
		t.Fatalf("the first page answered %v", err)
	}
	// the same record again, which is what a rewind over an earlier failure delivers
	if _, err := world.deliver(add); err != nil {
		t.Fatalf("the rewind answered %v", err)
	}
	held := world.held(line.messageId)
	if len(held.Reactions) != 1 {
		t.Errorf("one reaction record delivered twice left %d reactions: %v", len(held.Reactions), held.Reactions)
	}
	if count := len(world.bob.effectsOn[messageKeyOf(line.messageId)]); count != 1 {
		t.Errorf("one reaction record is held %d times", count)
	}
	if world.bob.stats.SkippedSeen == 0 {
		t.Error("the re-delivered record was not skipped as one this group already holds, so the first defence is not the one that fired")
	}

	// ── and the second defence, reached past the first ──────────────────────────────────────
	group := &Group{}
	group.initTables()
	sender := bytes.Repeat([]byte{0x01}, 16)
	lineId := aTarget(0xE1)
	effectId := aTarget(0xE2)
	text := &Content{Kind: KindText, Text: "a line"}
	deliverOneThroughAWalk(group, newMessage(text, 10, sender, nil, false, 0, lineId, "member"), text)

	reaction := &Content{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"}
	deliverOneThroughAWalk(group, newMessage(reaction, 11, sender, nil, false, 0, effectId, "member"), reaction)
	deliverOneThroughAWalk(group, newMessage(reaction, 11, sender, nil, false, 0, effectId, "member"), reaction)
	if count := len(group.effectsOn[messageKeyOf(lineId)]); count != 1 {
		t.Errorf("one effect record delivered twice is held %d times", count)
	}
	if standing := heldIn(t, group, lineId).Reactions; len(standing) != 1 {
		t.Errorf("one effect record delivered twice left %d reactions: %v", len(standing), standing)
	}
}

// ── the effect rules, at the unit the walk cannot reach ──────────────────────────────────────

// deliverOneThroughAWalk is one record through a ONE-RECORD WALK: [Group.deliverLocked] to fold the
// record in, then [Group.rebuildDirtyLocked] to run the rebuild the effects it noted are owed. It
// answers what deliverLocked answers.
//
// IT IS TWO CALLS BECAUSE THE PRODUCTION PATH IS TWO, and it was one until the replay was measured
// at n^3. An effect MARKS its target dirty as it lands and the rebuild happens ONCE per walk, at
// [Group.commitWalkLocked] -- see [Group.dirtyTargets] for what a rebuild per effect cost. The cases
// below drive deliverLocked BELOW the walk, because each needs something no session in this suite
// can produce -- a third party's sender_handle, a record id of zero, a kind this build cannot send
// -- so each owes itself the commit step a walk would have run for it. Nothing they assert is about
// WHEN the rebuild happens.
//
// WHAT IT STILL CATCHES: delete the dirty mark in [Group.noteEffectLocked] and this helper drains an
// empty set, so every case below goes red. Delete the drain from [Group.commitWalkLocked] instead
// and these stay green while every walk-driven case above goes red -- which is the split that says
// these cases are under the walk and those are through it.
func deliverOneThroughAWalk(group *Group, received *Message, entry *Content) bool {
	line := group.deliverLocked(received, entry)
	group.rebuildDirtyLocked()
	return line
}

// heldIn is the [Message] a group holds under one message_id AT THE MOMENT IT IS ASKED, and every
// case below that asserts on an effect asks through it rather than keeping the pointer it delivered.
//
// THAT IS A REQUIREMENT OF THE CODE AND NOT A PREFERENCE (msgrepo ledger item 227). A rebuild
// REPLACES the message it rebuilt -- so that every [Message] this package has handed a caller stays
// frozen and [Group.Messages] can be read without this group's lock -- and a case holding the
// pointer it passed to deliverOneThroughAWalk is holding the message as it was BEFORE the effect.
// Six cases here did exactly that and went red on the repair, which is the repair working: they were
// each reading a value a real caller would also have been reading.
func heldIn(t *testing.T, group *Group, messageId []byte) *Message {
	t.Helper()
	held, found := group.heldLocked(messageId)
	if !found {
		t.Fatalf("this group holds no message under %x", messageId)
	}
	return held
}

// A TOMBSTONE FROM ANYBODY BUT THE TARGET'S OWN SENDER IS IGNORED (T-b), AND IT IS IGNORED RATHER
// THAN REFUSED.
//
// R1 proves who sealed the TOMBSTONE and nothing in it proves they sealed the target, so without
// this rule MASTER §12.1's "a deletion cannot be forged" is false: any member could delete any
// message. It is IGNORED and not a walk failure because the record is a legal record -- failing the
// walk over one would hand any member a way to wedge the conversation.
//
// This is at the unit rather than through the walk because the walk has two members and this needs
// a third party's handle, which no session in this suite can produce.
func TestATombstoneFromAnotherSenderIsIgnored(t *testing.T) {
	group := &Group{}
	group.initTables()

	mine := bytes.Repeat([]byte{0x01}, 16)
	theirs := bytes.Repeat([]byte{0x02}, 16)
	mineId := bytes.Repeat([]byte{0xA1}, 32)
	theirId := bytes.Repeat([]byte{0xA2}, 32)
	lineId := aTarget(0xA1)

	line := newMessage(&Content{Kind: KindText, Text: "a line"}, 10, mine, mineId, false, 0, lineId, "member")
	if !deliverOneThroughAWalk(group, line, &Content{Kind: KindText, Text: "a line"}) {
		t.Fatal("a TEXT did not become a line of the conversation")
	}

	stranger := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(stranger, 11, theirs, theirId, false, 0, aTarget(0xA2), "member"), stranger)
	if heldIn(t, group, lineId).Deleted {
		t.Errorf("a tombstone sealed by %x deleted a message sealed by %x", theirs, mine)
	}

	owner := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(owner, 12, mine, mineId, false, 0, aTarget(0xA3), "member"), owner)
	if !heldIn(t, group, lineId).Deleted {
		t.Error("a tombstone sealed by the line's own sender did not delete it")
	}
	// AND THE MESSAGE THE WALK WAS GIVEN IS UNTOUCHED, which is the other half of the same rule:
	// `line` is the [Message] this case delivered, and a tombstone that reached back into it
	// would be reaching into every [Group.Messages] copy that already carries it.
	if line.Deleted {
		t.Error("the tombstone was written INTO the message this case had already been handed")
	}
}

// A REMOVE CANCELS THE SAME REACTOR'S ADD AND NOBODY ELSE'S.
//
// Two members reacting with one emoji are two reactions, and one of them taking theirs back leaves
// the other's standing. A remove written as "drop every reaction with this emoji" passes every
// single-member case and fails this one.
func TestAReactionIsPerReactorAndARemoveTakesBackOnlyItsOwn(t *testing.T) {
	group := &Group{}
	group.initTables()

	alice := bytes.Repeat([]byte{0x01}, 16)
	bob := bytes.Repeat([]byte{0x02}, 16)
	lineId := aTarget(0xB1)

	line := &Content{Kind: KindText, Text: "a line"}
	deliverOneThroughAWalk(group, newMessage(line, 10, alice, nil, false, 0, lineId, "member"), line)

	react := func(kind ContentKind, who []byte, emoji string, recordId uint64, id byte) {
		entry := &Content{Kind: kind, Target: lineId, Emoji: emoji}
		deliverOneThroughAWalk(group, newMessage(entry, recordId, who, nil, false, 0, aTarget(id), "member"), entry)
	}
	// STANDING IS ASKED FOR EACH TIME AND NEVER CACHED: see heldIn.
	standing := func() []Reaction { return heldIn(t, group, lineId).Reactions }

	react(KindReactionAdd, alice, "👍", 11, 0xB2)
	react(KindReactionAdd, bob, "👍", 12, 0xB3)
	react(KindReactionAdd, bob, "🎯", 13, 0xB4)
	if len(standing()) != 3 {
		t.Fatalf("three reactions from two members left %d: %v", len(standing()), standing())
	}
	// the same member's same emoji twice is one reaction
	react(KindReactionAdd, bob, "👍", 14, 0xB5)
	if len(standing()) != 3 {
		t.Errorf("one member's same emoji twice left %d reactions: %v", len(standing()), standing())
	}

	react(KindReactionRemove, bob, "👍", 15, 0xB6)
	if len(standing()) != 2 {
		t.Fatalf("a REMOVE left %d reactions: %v", len(standing()), standing())
	}
	for _, one := range standing() {
		if one.Emoji == "👍" && bytes.Equal(one.SenderHandle, bob) {
			t.Error("bob's 👍 survived bob's own REMOVE")
		}
	}
	kept := 0
	for _, one := range standing() {
		if one.Emoji == "👍" && bytes.Equal(one.SenderHandle, alice) {
			kept += 1
		}
	}
	if kept != 1 {
		t.Errorf("bob's REMOVE took back alice's 👍 as well: %v", standing())
	}
}

// A KIND THIS BUILD CANNOT READ IS AN ENTRY AND IS NOT A THING TO REACT TO OR DELETE.
//
// T-a is the rule: a tombstone applies to a stored CONTENT message -- TEXT, REPLY, and ATTACHMENT
// when the blob plane exists -- and to nothing else. A placeholder is the case that makes it a
// clause rather than a property of the index: it IS a [Message], it IS in this group's view, and
// this build does not know what it is, so it can neither say that deleting it is meaningful nor
// hand a UI a reaction attached to something it cannot render.
//
// The send side of the same rule is reactableLocked, driven here directly: it needs no server, and
// the API cannot reach it otherwise because nothing in this package can SEND an unknown kind.
func TestAKindThisBuildCannotReadIsNotReactableAndIsNotDeletable(t *testing.T) {
	group := &Group{}
	group.initTables()

	sender := bytes.Repeat([]byte{0x01}, 16)
	placeholderId := aTarget(0xC1)
	lineId := aTarget(0xC2)

	placeholder := &Content{Kind: KindEdit}
	senderId := bytes.Repeat([]byte{0xC9}, 32)
	if !deliverOneThroughAWalk(group, newMessage(placeholder, 10, sender, senderId, true, 0, placeholderId, "member"), placeholder) {
		t.Fatal("a placeholder is an entry of the conversation and this build dropped it")
	}
	line := &Content{Kind: KindText, Text: "a line"}
	deliverOneThroughAWalk(group, newMessage(line, 11, sender, senderId, true, 0, lineId, "member"), line)

	// the send side
	if _, err := group.reactableLocked(placeholderId); !errors.Is(err, ErrNoSuchMessage) {
		t.Errorf("a placeholder answered %v to reactableLocked, want a refusal", err)
	}
	if _, err := group.reactableLocked(lineId); err != nil {
		t.Errorf("the control: a TEXT this device holds answered %v", err)
	}

	// and the receipt side: a tombstone from the placeholder's OWN sender, which passes T-b
	tombstone := &Content{Kind: KindTombstone, Target: placeholderId}
	deliverOneThroughAWalk(group, newMessage(tombstone, 12, sender, senderId, true, 0, aTarget(0xC3), "member"), tombstone)
	if heldIn(t, group, placeholderId).Deleted {
		t.Error("a tombstone deleted a record whose kind this build cannot read")
	}
	// the control, which is what says the clause above is about the KIND and not about the
	// tombstone being ignored altogether
	onTheLine := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(onTheLine, 13, sender, senderId, true, 0, aTarget(0xC4), "member"), onTheLine)
	if !heldIn(t, group, lineId).Deleted {
		t.Error("the control: a tombstone on this sender's own TEXT did not delete it")
	}
}

// AN EFFECT THE SERVER HAS NOT NUMBERED YET IS THE NEWEST THING THIS DEVICE DID.
//
// The only records with a zero record id are ones THIS DEVICE has just sealed and whose submit has
// not been answered. Sorting one FIRST would let a half-submitted reaction be cancelled by a REMOVE
// the server numbered before it existed, which is a reaction a user made and watched disappear.
func TestAnEffectWithNoRecordIdYetSortsAfterEveryNumberedOne(t *testing.T) {
	group := &Group{}
	group.initTables()

	sender := bytes.Repeat([]byte{0x01}, 16)
	lineId := aTarget(0xD1)
	line := &Content{Kind: KindText, Text: "a line"}
	deliverOneThroughAWalk(group, newMessage(line, 10, sender, nil, true, 0, lineId, "member"), line)

	remove := &Content{Kind: KindReactionRemove, Target: lineId, Emoji: "👍"}
	deliverOneThroughAWalk(group, newMessage(remove, 15, sender, nil, true, 0, aTarget(0xD2), "member"), remove)
	add := &Content{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"}
	deliverOneThroughAWalk(group, newMessage(add, 0, sender, nil, true, 0, aTarget(0xD3), "member"), add)

	if standing := heldIn(t, group, lineId).Reactions; len(standing) != 1 {
		t.Errorf("a reaction this device has just sealed was cancelled by a REMOVE the server numbered before it: %v",
			standing)
	}
	// and the control: the same two with the ADD numbered BELOW the remove leave nothing standing
	second := &Group{}
	second.initTables()
	deliverOneThroughAWalk(second, newMessage(line, 10, sender, nil, true, 0, lineId, "member"), line)
	numbered := &Content{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"}
	deliverOneThroughAWalk(second, newMessage(numbered, 14, sender, nil, true, 0, aTarget(0xD3), "member"), numbered)
	deliverOneThroughAWalk(second, newMessage(remove, 15, sender, nil, true, 0, aTarget(0xD2), "member"), remove)
	if standing := heldIn(t, second, lineId).Reactions; len(standing) != 0 {
		t.Errorf("the control: an ADD at record 14 and a REMOVE at 15 left %v standing", standing)
	}
}

// ── small helpers, so a case reads as what it is measuring ───────────────────────────────────

func mustEncodeText(t *testing.T, text string) []byte {
	t.Helper()
	plaintext, err := encodeText(text)
	if err != nil {
		t.Fatalf("encodeText(%q): %v", text, err)
	}
	return plaintext
}

func mustEncodeReaction(t *testing.T, kind ContentKind, target []byte, emoji string) []byte {
	t.Helper()
	plaintext, err := encodeReaction(kind, target, emoji)
	if err != nil {
		t.Fatalf("encodeReaction: %v", err)
	}
	return plaintext
}

func mustEncodeTombstone(t *testing.T, target []byte) []byte {
	t.Helper()
	plaintext, err := encodeTombstone(target)
	if err != nil {
		t.Fatalf("encodeTombstone: %v", err)
	}
	return plaintext
}
