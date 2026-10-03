package cp3b

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/sdk/urmessage"
)

// THE TEXT CEILING, AND THE THING THAT MAKES IT WORTH A CASE: WHAT A REFUSED SEND COSTS.
//
// WHAT WAS WRONG. connect's sealer takes a cheap half of the size-ladder refusal before it
// reserves anything -- bucketForBody over the CALLER's own length -- and that half passes for
// every body up to 65,532, because 65,532 is what the 64 KiB rung holds. Since MASTER section 8.4
// the real bucket is chosen over the MLS frame, which is 193 to 198 octets longer and does not
// exist until a stream index has been reserved and Protect has consumed a generation. So a text
// of 65,335..65,532 octets -- connect's ledger open item 203, whose
// TestABodyNoRungCouldHoldCostsNeitherAnIndexNorAGeneration measures the band from below -- used
// to walk all the way through urmessage.Group.Send, spend one DURABLE stream index and one MLS
// generation, and only then be answered ErrTextTooLong. Neither is recoverable. A caller that
// retried spent another of each per attempt, which is the whole reason "a failed send must not be
// retried in a loop" was ever a rule rather than a preference.
//
// WHAT THE COST IS MEASURED AS, and it is the sdk-side store rather than a counter this package
// keeps: sdk.StreamStore.StreamHighWater is the highest index ever ALLOCATED for this stream,
// answered from the fsynced row on disk. The allocation happens inside SealRecord and nothing
// above it can undo one. So "the refusal spent nothing" is "the number on the disk did not move",
// read off the same file a restart would read.
//
// WHAT WOULD GO RED IF THE REFUSAL MOVED BACK INTO THE SEALER: the band's high-water clause below,
// on the FIRST over-long attempt -- and the ErrBodyTooLong clause beside it, which says the text
// never reached the sealer at all.
func TestTheTextCeilingRefusesBeforeItSpendsAnythingIrreversible(t *testing.T) {
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

	// one ordinary line, so that this case knows alice's sender_handle -- the stream the store
	// keys its row by -- and so that the high water below is a number a send really moved.
	first, err := aliceGroup.Send(ctx, "one ordinary line, before the ceiling")
	if err != nil {
		t.Fatalf("alice's first Send: %v", err)
	}
	handle := append([]byte(nil), first.SenderHandle...)
	highWater := func() uint64 {
		t.Helper()
		at, err := alice.streamStore.StreamHighWater(groupId, handle)
		if err != nil {
			t.Fatalf("reading alice's durable stream high water: %v", err)
		}
		return at
	}
	spent := highWater()
	if spent == 0 {
		t.Fatalf("one line was sealed and the durable high water is 0, so this case cannot see an index being spent")
	}

	// ── the refusals, and the two things each of them must be ───────────────────────────────
	//
	// The band is 65,335..65,532 -- the lengths that fitted the 64 KiB rung before MASTER
	// section 8.4 and do not fit after -- plus one length above it, where the sealer's own
	// cheap refusal would also have fired. Both must now be refused by THIS package, and the
	// test for "by this package" is that the answer does NOT wrap messagegroup.ErrBodyTooLong:
	// that sentinel exists only inside SealRecord, so an error carrying it is an error that
	// reached the sealer, and reaching the sealer inside the band is what spends the index.
	for _, octets := range []int{
		urmessage.MaxTextOctets + 1,
		urmessage.MaxTextOctets + 2,
		message.SizeBucketBytes(message.SizeBucket64K) - 4, // 65,532: the old ceiling, top of the band
		message.SizeBucketBytes(message.SizeBucket64K),     // above every rung under any framing
		1 << 20,
	} {
		sent, err := aliceGroup.Send(ctx, strings.Repeat("u", octets))
		if !errors.Is(err, urmessage.ErrTextTooLong) {
			t.Errorf("a %d octet text answered %v, want ErrTextTooLong", octets, err)
		}
		if sent != nil {
			t.Errorf("a %d octet text was refused AND answered a message", octets)
		}
		if errors.Is(err, messagegroup.ErrBodyTooLong) {
			t.Errorf("a %d octet text was refused by the SEALER (it carries messagegroup.ErrBodyTooLong), which means it reached SealRecord and spent a stream index and an MLS generation on its way to the same error",
				octets)
		}
		if after := highWater(); after != spent {
			t.Errorf("a %d octet text was refused and alice's durable stream high water moved from %d to %d; the refusal spent an index that can never be used",
				octets, spent, after)
		}
	}

	// ── AND A CALLER'S RETRY LOOP, WHICH IS THE SHAPE THE RULE IS ABOUT ─────────────────────
	//
	// A refusal that costs nothing once and something the tenth time is not a refusal that
	// costs nothing. This is the same length ten times through the same door.
	const attempts = 10
	tooLong := strings.Repeat("u", urmessage.MaxTextOctets+1)
	for at := 0; at < attempts; at += 1 {
		if _, err := aliceGroup.Send(ctx, tooLong); !errors.Is(err, urmessage.ErrTextTooLong) {
			t.Fatalf("attempt %d of the same over-long text answered %v", at+1, err)
		}
	}
	if after := highWater(); after != spent {
		t.Errorf("%d retries of one over-long text moved alice's durable high water from %d to %d, which is %d stream indices and %d MLS generations burnt on a text that was never sealed",
			attempts, spent, after, after-spent, after-spent)
	}

	// ── AND THE CONSTANT IS NOT MERELY SMALL ENOUGH ─────────────────────────────────────────
	//
	// Every clause above passes for MaxTextOctets = 0, which would refuse the whole product.
	// So the boundary is held from BOTH sides: the longest accepted text seals, lands on the
	// 64 KiB rung, spends exactly one index, and is read back whole on the far side.
	ceiling := strings.Repeat("u", urmessage.MaxTextOctets)
	sent, err := aliceGroup.Send(ctx, ceiling)
	if err != nil {
		t.Fatalf("a text of exactly MaxTextOctets (%d) was refused: %v", urmessage.MaxTextOctets, err)
	}
	if after := highWater(); after != spent+1 {
		t.Errorf("the accepted text moved the high water from %d to %d, want %d", spent, after, spent+1)
	}
	rung := message.SizeBucket(0xFF)
	for _, row := range world.allRows(t, groupId) {
		if row.RecordId == sent.RecordId {
			rung = message.SizeBucket(row.SizeBucket)
		}
	}
	// the sentinel above is the control: a row set that does not contain the record just sent
	// leaves `rung` at 0xFF and the comparison below fails, rather than passing over nothing.
	if rung != message.SizeBucket64K {
		t.Errorf("the longest accepted text landed on rung %d, want the 64 KiB rung %d", rung, message.SizeBucket64K)
	}
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	found := false
	for _, one := range got {
		if one.RecordId == sent.RecordId {
			found = true
			if one.Text != ceiling {
				t.Errorf("the longest accepted text came back at %d octets, want %d", len(one.Text), len(ceiling))
			}
		}
	}
	if !found {
		t.Errorf("bob read %d message(s) and none of them is record %d", len(got), sent.RecordId)
	}
	t.Logf("MaxTextOctets = %d: it seals, lands on rung %d and reads back whole; every length above it is refused for free, %d retries included",
		urmessage.MaxTextOctets, rung, attempts)
	assertNothingFailedToOpen(t, "bob", bobGroup)
}
