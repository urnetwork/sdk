package cp3b

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// MASTER SECTION 8.4.5's message_id, THROUGH THE WHOLE STACK: THE SENDER'S, THE RECEIVER'S, AND
// THE ONE A RESTARTED DEVICE SHOWS FROM THE COPY IT KEPT.
//
// WHY IT NEEDED A CASE AT ALL. connect exported MessageIdOf and its free function MessageId, both
// correct and both with NO CALLER anywhere in connect or in sdk: the derivation was a property of
// a function rather than of the build. Every kind the product owes next -- a reply, a reaction, a
// tombstone, a read cursor -- has to say WHICH message it is about, and record_id cannot be that
// name: record_id is the SERVER's per-group counter, so a sender does not have it until the submit
// is answered and a device whose submit response was lost holds a message at record_id zero.
//
// THE PROPERTY THAT MATTERS IS AGREEMENT AND IT CANNOT BE SEEN FROM ONE DEVICE. An id both sides
// compute the same way from the same header is a name; an id one side computes from its own
// bookkeeping is a number. So this case reads the SAME record from the sender and from the
// receiver and compares, and it does the same for the third path -- a restarted device rendering
// its own line from the copy it kept, which is the only way a sender ever sees its own record
// again since connect 4c030dc.
//
// AND THE POSITION HALF, which is what stops "agreement" being satisfied by a constant: two
// messages from one sender, and one from each of two senders, must all have different ids.
func TestEveryMessageCarriesTheIdBothDevicesDeriveForIt(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	// the ids seen so far, by hex, so that a repeat is named with both record ids rather than
	// counted
	seen := map[string]uint64{}
	note := func(who string, one *urmessage.Message) {
		t.Helper()
		if len(one.MessageId) != 32 {
			t.Fatalf("%s: record %d carries a %d octet message_id, want 32", who, one.RecordId, len(one.MessageId))
		}
		if bytes.Equal(one.MessageId, make([]byte, 32)) {
			t.Errorf("%s: record %d carries thirty two zero octets as its message_id", who, one.RecordId)
		}
		key := hex.EncodeToString(one.MessageId)
		if at, repeat := seen[key]; repeat && at != one.RecordId {
			t.Errorf("%s: record %d has the same message_id as record %d, so the id does not name a position", who, one.RecordId, at)
		}
		seen[key] = one.RecordId
	}

	const aliceFirst = "alice, at her first position"
	const aliceSecond = "alice again, one position further on"
	const bobsLine = "and bob, at a position of his own"

	sentOne, err := aliceGroup.Send(ctx, aliceFirst)
	if err != nil {
		t.Fatalf("alice's first Send: %v", err)
	}
	note("alice's own send", sentOne)
	sentTwo, err := aliceGroup.Send(ctx, aliceSecond)
	if err != nil {
		t.Fatalf("alice's second Send: %v", err)
	}
	note("alice's own send", sentTwo)

	// ── the receiver's half: the same records, opened by the other device ───────────────────
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	byRecord := map[uint64]*urmessage.Message{}
	for _, one := range got {
		note("bob's receive", one)
		byRecord[one.RecordId] = one
	}
	for _, sent := range []*urmessage.Message{sentOne, sentTwo} {
		opened := byRecord[sent.RecordId]
		if opened == nil {
			t.Fatalf("bob did not open record %d at all", sent.RecordId)
		}
		if !bytes.Equal(opened.MessageId, sent.MessageId) {
			t.Errorf("record %d: alice derived message_id %x and bob derived %x; the two sides do not agree on the name of one message",
				sent.RecordId, sent.MessageId, opened.MessageId)
		}
	}

	sentBob, err := bobGroup.Send(ctx, bobsLine)
	if err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	note("bob's own send", sentBob)
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	matched := false
	for _, one := range back {
		note("alice's receive", one)
		if one.RecordId != sentBob.RecordId {
			continue
		}
		matched = true
		if !bytes.Equal(one.MessageId, sentBob.MessageId) {
			t.Errorf("record %d: bob derived message_id %x for his own line and alice derived %x",
				sentBob.RecordId, sentBob.MessageId, one.MessageId)
		}
	}
	if !matched {
		t.Fatalf("alice read %d message(s) and none of them is bob's record %d", len(back), sentBob.RecordId)
	}

	// ── AND THE THIRD PATH, WHICH NOTHING ABOVE REACHES ─────────────────────────────────────
	//
	// A sender cannot open its own record (connect MG-4) and shows it from the copy it kept, and
	// a LIVE group never takes that path because the record is already in its log. A RESTARTED
	// one does: its log starts empty and every one of its own records comes back off the server
	// as ciphertext it will never read again. That path builds its Message from the copy -- the
	// text and the clock reading -- and must derive the id from the SERVER's header rather than
	// from anything the copy carries, or a device's own line would be named one thing before a
	// restart and another after it.
	stateDir, streamDir := alice.stateDir, alice.streamDir
	alice.kill()
	alice = world.durablePersona(t, "alice", stateDir, streamDir)
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted alice's Connect: %v", err)
	}
	restored, err := alice.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the restarted alice restored %d group(s), want 1", len(restored))
	}
	restoredGroup := restored[0]
	if _, err := restoredGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted alice's Receive: %v", err)
	}
	fromCopy := map[uint64]*urmessage.Message{}
	for _, one := range restoredGroup.Messages() {
		fromCopy[one.RecordId] = one
	}
	if stats := restoredGroup.Stats(); stats.OpenedOwn != 2 {
		t.Fatalf("the restarted alice showed %d of her own records from copies, want 2; this case's third path was not taken", stats.OpenedOwn)
	}
	for _, sent := range []*urmessage.Message{sentOne, sentTwo} {
		shown := fromCopy[sent.RecordId]
		if shown == nil {
			t.Errorf("the restarted alice does not hold record %d", sent.RecordId)
			continue
		}
		if !bytes.Equal(shown.MessageId, sent.MessageId) {
			t.Errorf("record %d: alice named it %x when she sent it and %x after a restart, so a reply written before the restart names a message that no longer exists",
				sent.RecordId, sent.MessageId, shown.MessageId)
		}
	}
	t.Logf("%d distinct message_ids over 3 records seen from 4 seats (sender, receiver, the other sender, and a restarted sender's own copy)", len(seen))
	if len(seen) != 3 {
		t.Errorf("3 records produced %d distinct message_ids", len(seen))
	}
}
