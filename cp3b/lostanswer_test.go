package cp3b

import (
	"context"
	"testing"
)

// A RECORD WHOSE SUBMIT ANSWER NEVER CAME BACK COMES BACK AS ITS SENDER'S OWN, AND IS NOT READ AS
// A SECOND WRITER.
//
// THIS IS THE ONE CASE THAT EXERCISES THE BODY-HASH ARM OF THE CLONE CHECK ON A HEALTHY DEVICE,
// which is why it exists. [urmessage.Group.Send] notes the stream index and the body_hash AT THE
// SEAL rather than at the submit, and the clone check refuses when a record opens under this
// device's own sender_handle at an index it sealed carrying a body_hash that is not the one it
// sealed. Every ordinary own record is short-circuited before that comparison, because the log
// already holds it by record id -- so the comparison only ever runs on a record the device sealed
// and does NOT hold.
//
// THERE IS EXACTLY ONE WAY TO BE IN THAT STATE WITHOUT A COPY EXISTING: the server committed the
// record and the answer did not reach the client. The send failed, the record is on the server,
// and the device's log has never held it. If the index were noted at the SUBMIT instead of at the
// seal, or if the hash were taken from anywhere but the sealed record, this device would meet its
// own lost message on its next fetch and wedge itself permanently with a sentence accusing the
// user of running a copy of their app data. That is a false accusation that stops a working
// device from ever sending again, and it is reachable by one dropped TCP connection.
//
// THE SHAPE IS ORDERED THE WAY THE HAZARD IS. [shapedStore.Submit] calls through to the REAL §6.1
// transaction and loses the answer AFTERWARDS: a double that refused before writing would leave
// the server with nothing, which is the opposite of the state under test.
//
// AND THE RECOVERY IS A BONUS RATHER THAN THE POINT: the message the user thought had failed
// appears in their own log the next time they fetch, exactly once. That falls out of opening own
// records and it is asserted here so that a later change cannot quietly lose it.
func TestARecordWhoseSubmitAnswerWasLostComesBackAsItsSendersOwn(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchLosesOneSubmitAnswer})
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

	// S2-2's recovery buys ONE resubmission, so two answers have to go for the send to fail:
	// the first submission and the one the recovery makes. Both land on the server; the second
	// is refused as a stream index regression because the first is already there.
	const lost = "a line whose answer never came back, and which is on the server anyway"
	world.shaped.loseSubmitAnswers(2)
	sent, err := aliceGroup.Send(ctx, lost)
	if err == nil {
		t.Fatalf("the send reported success as record %d, so no answer was lost and this case is over nothing", sent.RecordId)
	}
	t.Logf("the send failed: %v", err)
	for _, one := range aliceGroup.Messages() {
		if one.Text == lost {
			t.Fatal("a send that failed put its message in the log, so the state this case needs does not exist")
		}
	}

	// THE CONTROL: the record really is on the server. If it were not, everything below would
	// pass over an empty conversation.
	if got, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	} else if len(got) != 1 || got[0].Text != lost {
		t.Fatalf("the far side read %s, so the record this case is about is not on the server", textsOf(got))
	}

	// ── the sender meets its own lost record ─────────────────────────────────────────────
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the sender's Receive over its own lost record: %v", err)
	}
	if err := aliceGroup.IdentityInUse(); err != nil {
		t.Fatalf("a device met its OWN lost record and wedged itself as a copy: %v", err)
	}
	if len(back) != 1 || back[0].Text != lost {
		t.Fatalf("the sender read %s for its own lost record", textsOf(back))
	}
	if !back[0].Mine {
		t.Error("the sender's own lost record came back marked as somebody else's")
	}
	if own := aliceGroup.Stats().OpenedOwn; own != 1 {
		t.Errorf("Stats.OpenedOwn is %d", own)
	}

	// EXACTLY ONCE. A second fetch must not deliver it again: it is in the log now.
	again, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the sender's second Receive: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the recovered record was delivered a second time: %s", textsOf(again))
	}

	// and the device is not wedged: it can still send.
	const afterwards = "and the device that lost an answer still works"
	if _, err := aliceGroup.Send(ctx, afterwards); err != nil {
		t.Fatalf("the sender's next Send: %v", err)
	}
	heard, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if len(heard) != 1 || heard[0].Text != afterwards {
		t.Fatalf("bob read %s", textsOf(heard))
	}
}
