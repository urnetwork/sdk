package cp3b

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// THE THREE LINES THESE CASES TURN ON. Typed here so a case comparing against a literal at the
// point of comparison cannot pass with one of two spellings wrong.
const (
	firstLine  = "alice line one -- this is the record that will not open on the first try"
	secondLine = "alice line two"
	thirdLine  = "alice line three"
	fourthLine = "alice line four, typed after the record before it was given up on"
)

// A RECORD THAT DID NOT OPEN IS FETCHED AGAIN, AND ONE TRANSIENT DOES NOT COST THE CONVERSATION A
// MESSAGE FOR EVER.
//
// WHAT SHIPPED. `openPageLocked` advanced the cursor for EVERY row before the fail paths. So the
// first Receive returned the error and the cursor was already past the record; the next Receive
// asked from above it, the server never sent it again, and the call answered no messages and a NIL
// ERROR while the record sat on the server. Measured at the commit this repairs: first Receive 2
// messages and a named AEAD error, second Receive 0 messages and nil.
//
// THE ORDINARY RESPONSE TO AN ERROR IS TO RETRY, and that is what made this so bad: the retry was
// the thing that hid it. A user who pulled to refresh got a clean, empty, successful answer over a
// conversation that was missing a line.
//
// THE BEND IS ON THE RESULT AND NOT ON THE STORE. The record on the server is untouched, so the
// second fetch asks for a record that is perfectly good -- which is what makes "the client stopped
// asking" the finding rather than "the record was destroyed".
func TestARecordThatDidNotOpenIsFetchedAgainRatherThanDroppedForEver(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchBendsOneRecord})
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

	sent := []uint64{}
	for _, text := range []string{firstLine, secondLine, thirdLine} {
		one, err := aliceGroup.Send(ctx, text)
		if err != nil {
			t.Fatalf("alice's Send %q: %v", text, err)
		}
		sent = append(sent, one.RecordId)
	}
	// exactly one record, on exactly one fetch.
	world.shaped.bend(sent[0], 1)

	first, err := bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("the first Receive answered %v, want one wrapping ErrRecordOpen naming record %d", err, sent[0])
	}
	t.Logf("first  Receive: %d message(s) %s, err=%v", len(first), textsOf(first), err)
	if len(first) != 2 {
		t.Fatalf("the first Receive answered %d message(s) beside its refusal, want the two that did open", len(first))
	}

	// THE RETRY, AND IT IS AN ORDINARY ONE: the same call, nothing else changed.
	second, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the second Receive answered %v", err)
	}
	t.Logf("second Receive: %d message(s) %s, err=%v", len(second), textsOf(second), err)
	if len(second) != 1 || second[0].Text != firstLine {
		t.Fatalf("the second Receive answered %s, want the one record that failed the first time", textsOf(second))
	}
	if second[0].RecordId != sent[0] {
		t.Errorf("the record that came back is %d and the one that failed is %d", second[0].RecordId, sent[0])
	}

	// and the conversation is whole, with nothing delivered twice.
	held := bobGroup.Messages()
	if len(held) != 3 {
		t.Fatalf("bob's log holds %d message(s) after the retry: %s", len(held), textsOf(held))
	}
	seen := map[uint64]bool{}
	for _, one := range held {
		if seen[one.RecordId] {
			t.Errorf("record %d is in the log twice; the rewind delivered a message a second time", one.RecordId)
		}
		seen[one.RecordId] = true
	}
	stats := bobGroup.Stats()
	t.Logf("stats after both calls: %+v", stats)
	if stats.SkippedSeen == 0 {
		t.Error("nothing was re-read, so the fetch did not rewind and this case passed for the wrong reason")
	}
	if stats.Unopened != 0 {
		t.Errorf("%d record(s) were given up on and this case's record should have opened on its second try", stats.Unopened)
	}
	if unopened := bobGroup.UnopenedRecords(); len(unopened) != 0 {
		t.Errorf("UnopenedRecords is %v", unopened)
	}
}

// A RECORD THAT NEVER OPENS IS GIVEN UP ON BY NAME, COUNTED, AND LISTED -- AND THE CONVERSATION
// CARRIES ON.
//
// THE RETRY ABOVE HAS TO BE BOUNDED OR IT IS ITS OWN DEFECT: a group that re-reads its whole tail
// on every fetch for ever is one bent record turned into a permanent cost, which is a shape an
// unfriendly peer would reach for. So the bound is [urmessage] internal maxRecordAttempts, and
// what happens AT the bound is the point -- the record is named with ErrRecordAbandoned, counted
// in Stats.Unopened and listed by UnopenedRecords, so a hole in a conversation is a thing a UI can
// show rather than a silence.
//
// THE THIRD ASSERTION IS THE ONE THAT MATTERS MOST: after the record is abandoned the cursor moves
// past it, so a LATER message still arrives. A client wedged behind one bad record would be a
// conversation that stopped for ever, which is the failure the unbounded retry would have caused.
func TestARecordThatNeverOpensIsGivenUpOnByNameAndTheConversationCarriesOn(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchBendsOneRecord})
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

	doomed, err := aliceGroup.Send(ctx, firstLine)
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := aliceGroup.Send(ctx, secondLine); err != nil {
		t.Fatalf("alice's second Send: %v", err)
	}
	world.shaped.bend(doomed.RecordId, 1000)

	var last error
	attempts := 0
	for attempts = 1; attempts <= 8; attempts += 1 {
		_, last = bobGroup.Receive(ctx)
		if errors.Is(last, urmessage.ErrRecordAbandoned) {
			break
		}
		if !errors.Is(last, urmessage.ErrRecordOpen) {
			t.Fatalf("Receive %d answered %v, want ErrRecordOpen while it is still retrying", attempts, last)
		}
	}
	if !errors.Is(last, urmessage.ErrRecordAbandoned) {
		t.Fatalf("after %d Receives the record is still being retried and was never given up on: %v", attempts, last)
	}
	t.Logf("given up on after %d Receive(s): %v", attempts, last)

	stats := bobGroup.Stats()
	if stats.Unopened != 1 {
		t.Errorf("Stats.Unopened is %d after one record was given up on", stats.Unopened)
	}
	unopened := bobGroup.UnopenedRecords()
	if len(unopened) != 1 || unopened[0] != doomed.RecordId {
		t.Errorf("UnopenedRecords is %v and the record given up on is %d", unopened, doomed.RecordId)
	}

	// AND THE CONVERSATION CARRIES ON. A later message arrives, with a nil error: the cursor
	// moved past the hole rather than standing in front of it for ever.
	if _, err := aliceGroup.Send(ctx, fourthLine); err != nil {
		t.Fatalf("alice's Send after the abandonment: %v", err)
	}
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the Receive after the abandonment answered %v, so this client is wedged behind one bad record", err)
	}
	if len(got) != 1 || got[0].Text != fourthLine {
		t.Fatalf("the Receive after the abandonment answered %s", textsOf(got))
	}
}

// A SERVER THAT HOLDS RECORDS BACK IS CAUGHT BY THE FIELD IT HANDED OVER ITSELF.
//
// §4.3.4's `high_water_record_id` is the server's own statement of the highest record it holds for
// this group. It was read in EXACTLY ONE PLACE -- inside checkAttestationLocked, which returns at
// its first branch when there is no attestation, which on the deployed server is every page. So
// the one field that reveals a server silently omitting records was ignored on every real page,
// and the client answered an empty conversation with a nil error.
//
// THE SERVER BELOW IS TRUTHFUL AND STILL CAUGHT, which is the whole point: nothing is tampered
// with, `complete` and `high_water_record_id` are the real store's own numbers, and the records are
// simply not handed over. The AEAD cannot see that. This field can, and it needs no key, no fleet
// root and no attestation -- which is the half of S2-27 that is not blocked on key custody.
func TestAServerThatHoldsRecordsBackIsCaughtByItsOwnHighWater(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchDropsMessages})
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
	for _, text := range []string{firstLine, secondLine, thirdLine} {
		if _, err := aliceGroup.Send(ctx, text); err != nil {
			t.Fatalf("alice's Send %q: %v", text, err)
		}
	}

	got, err := bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrFetchOmitted) {
		t.Fatalf("a complete page that named a high water above everything it carried answered %v, want ErrFetchOmitted; three messages exist on the server and %d came back",
			err, len(got))
	}
	t.Logf("the omission is named: %v", err)
	if len(got) != 0 {
		t.Fatalf("this server handed over %d message(s) and was supposed to hand over none", len(got))
	}
	if omitted := bobGroup.Stats().Omitted; omitted != 1 {
		t.Errorf("Stats.Omitted is %d after one omitting page", omitted)
	}
}

// AND THE CONTROL, WHICH IS WHAT KEEPS THE CHECK ABOVE FROM BEING A CHECK THAT ALWAYS FIRES.
//
// The same conversation over a server that hands everything over: no refusal, and Stats.Omitted
// does not move. Without this, "the omission is detected" is equally well explained by a client
// that reports an omission on every page -- which is the shape Stats.Unattested already has, and
// which is why that counter distinguishes nothing.
func TestAnHonestServerMovesNeitherTheOmissionCounterNorTheRefusal(t *testing.T) {
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
	for _, text := range []string{firstLine, secondLine, thirdLine} {
		if _, err := aliceGroup.Send(ctx, text); err != nil {
			t.Fatalf("alice's Send %q: %v", text, err)
		}
	}
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("an honest server's complete page answered %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("an honest server handed over %s", textsOf(got))
	}
	if omitted := bobGroup.Stats().Omitted; omitted != 0 {
		t.Fatalf("Stats.Omitted is %d over an honest server, so the counter distinguishes nothing", omitted)
	}
	// and a second Receive over a server with nothing new: still no omission. This is the
	// boundary case the check is most likely to get wrong, because the page carries no records
	// at all and the high water is exactly where the cursor already is.
	again, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("a Receive with nothing new answered %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a Receive with nothing new answered %s", textsOf(again))
	}
	if omitted := bobGroup.Stats().Omitted; omitted != 0 {
		t.Fatalf("Stats.Omitted is %d after an empty complete page", omitted)
	}
}
