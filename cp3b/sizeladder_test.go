package cp3b

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk/urmessage"
)

// WHAT urmessage SEALS AT EVERY RECORD TYPE, AND WHICH RUNG EACH ONE LANDS ON, READ OFF THE
// SERVER'S OWN ROWS.
//
// WHY THIS CASE EXISTS. connect 4c030dc made an APPLICATION record's ct_body plaintext an MLS
// PrivateMessage, and the frame sits INSIDE the size rung: connect's own
// TestTheSizeLadderCostOfTheInnerFrameIsMeasuredHere measures usable body falling 252 to 59,
// 1,020 to 826, 4,092 to 3,898, 16,380 to 16,186 and 65,532 to 65,334. That measurement is over
// messagegroup.GroupSession directly. THIS one is over what a user types, through
// urmessage.Group.Send, a real server, and the row the server stored -- so a text length that used
// to cost 272 stored octets and now costs 1,040 is a fact about THIS client rather than a column
// transcribed from the layer below.
//
// AND IT HOLDS THE THREE CEREMONY TYPES APART FROM THE TEXT, because MASTER section 8.4.1's table
// frames only one arm: a founding commit (is_commit), a wrap and an epoch-complete marker (each
// carrying a server attachment) have NO inner frame, so the ruling should not have moved any of
// them. If a later change framed them, the rung each lands on is where it shows.
//
// EVERY TEXT IS READ BACK ON THE FAR SIDE AND COMPARED WHOLE, so a boundary that sealed but did not
// open is red here rather than a rung number that looks right.
func TestEveryRecordTypeUrmessageSealsLandsOnTheRungItsBodyNeeds(t *testing.T) {
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

	rows := func() map[uint64]*store.Record {
		t.Helper()
		byId := map[uint64]*store.Record{}
		for _, row := range world.allRows(t, groupId) {
			byId[row.RecordId] = row
		}
		return byId
	}

	// ── the ceremony: founding commit, one wrap per member, the marker ──────────────────────
	ceremony := rows()
	commits, wraps, markers := 0, 0, 0
	for _, row := range ceremony {
		kind := "text"
		want := message.SizeBucket(0xFF)
		switch {
		case row.IsCommit:
			// the founding commit's body is the MLS commit that added bob. IT MOVED ON 2026-09-18
			// AND THE CAUSE IS A BIGGER COMMIT AND NOT A FRAME, which is the alternative the
			// sentence that stood here anticipated: urmessage.Group.AddMember now commits the add
			// BY VALUE (GroupHandle.CommitAdd, ledger item 239 step A2), so the commit carries
			// bob's whole key package inline -- the X-Wing public key in urmessage_leaf_keys is
			// 1,216 octets on its own -- where the by-reference arm carried a 32 octet proposal
			// reference. Measured: 272 octets of ct_body on the 256 rung before, 4,112 on the
			// 4,096 rung after. The wraps and the marker below did not move, which is what says
			// the founding commit grew rather than that the ceremony was framed.
			kind, commits = "founding commit", commits+1
			want = message.SizeBucket4K
		case row.Attachment != nil && row.Attachment.Kind == store.AttachmentWrap:
			kind, wraps = "epoch wrap", wraps+1
			want = message.SizeBucket256
		case row.Attachment != nil && row.Attachment.Kind == store.AttachmentEpochComplete:
			kind, markers = "epoch complete marker", markers+1
			want = message.SizeBucket256
		}
		t.Logf("record %d: %s, size_bucket %d (%d octets rung), ct_body %d octets",
			row.RecordId, kind, row.SizeBucket, message.SizeBucketBytes(message.SizeBucket(row.SizeBucket)), len(row.CtBody))
		// a wrap's body is 91 octets of prose and a marker's 33, and neither carries a frame: both
		// fit the 256 rung with the old 252 usable octets and must still.
		if want != 0xFF && message.SizeBucket(row.SizeBucket) != want {
			t.Errorf("record %d, a %s, landed on rung %d and carries no inner frame, so it should be on rung %d",
				row.RecordId, kind, row.SizeBucket, want)
		}
	}
	if commits != 1 || wraps != 2 || markers != 1 {
		t.Fatalf("the founding left %d commit(s), %d wrap(s) and %d marker(s), want 1, 2 and 1", commits, wraps, markers)
	}

	// ── the text ladder, at every edge the ruling moved and at the edges it used to be ───────
	type edge struct {
		octets int
		rung   message.SizeBucket
	}
	// EVERY EDGE MOVED DOWN BY ONE OCTET WHEN THE CONTENT ENVELOPE LANDED, and that is the whole
	// of what the 2026-09-17 ruling costs a stored record. What connect measures is the
	// APPLICATION PLAINTEXT's capacity -- 59 / 826 / 3,898 / 16,186 / 65,334 -- and the plaintext
	// is now `kind ‖ body`, so a text of 58 octets is 59 of plaintext and still fits the 256 rung
	// while one of 59 is 60 and does not. The column below is therefore the measured one less the
	// kind octet, and it stays literals for the reason it always did: two tables agreeing with
	// each other measure nothing.
	edges := []edge{
		{1, message.SizeBucket256},
		{58, message.SizeBucket256},
		{59, message.SizeBucket1K}, // fitted the 256 rung before the kind octet
		{252, message.SizeBucket1K},
		{825, message.SizeBucket1K},
		{826, message.SizeBucket4K}, // fitted the 1K rung before the kind octet
		{1020, message.SizeBucket4K},
		{3897, message.SizeBucket4K},
		{3898, message.SizeBucket16K},
		{16185, message.SizeBucket16K},
		{16186, message.SizeBucket64K},
		{65333, message.SizeBucket64K},
	}
	for _, one := range edges {
		text := strings.Repeat("u", one.octets)
		sent, err := aliceGroup.Send(ctx, text)
		if err != nil {
			t.Fatalf("a %d octet text: %v", one.octets, err)
		}
		row := rows()[sent.RecordId]
		if row == nil {
			t.Fatalf("a %d octet text was answered record %d and the server holds no such row", one.octets, sent.RecordId)
		}
		if message.SizeBucket(row.SizeBucket) != one.rung {
			t.Errorf("a %d octet text landed on rung %d and the measured ladder puts it on %d",
				one.octets, row.SizeBucket, one.rung)
		}
		got, err := bobGroup.Receive(ctx)
		if err != nil {
			t.Fatalf("bob's Receive of the %d octet text: %v", one.octets, err)
		}
		if len(got) != 1 || got[0].Text != text {
			lengths := []int{}
			for _, message := range got {
				lengths = append(lengths, len(message.Text))
			}
			t.Fatalf("bob read %d message(s) of lengths %v for one %d octet text", len(got), lengths, one.octets)
		}
		t.Logf("a %5d octet text: rung %d, ct_body %5d octets, read back whole", one.octets, row.SizeBucket, len(row.CtBody))
	}

	// ── and the ceiling: 65,334 is one past the largest inline rung and is refused by name ───
	//
	// It used to fit twice over: 65,532 octets were usable before 4c030dc and 65,334 before the
	// kind octet. The 198 octet band between the first two is the one connect's ledger open item
	// 203 names, and a blob rung is its only destination.
	//
	// WHERE THE REFUSAL NOW HAPPENS IS NOT WHERE IT DID, and this case deliberately does not say
	// so: urmessage.MaxTextOctets refuses all of these BEFORE SealRecord, and what that costs --
	// or rather what it stops costing -- is
	// TestTheTextCeilingRefusesBeforeItSpendsAnythingIrreversible's. The literals below stay
	// literals on purpose: they are this file's own measured column, so a MaxTextOctets that
	// drifted away from the ladder would show up here as a text that is refused and should not be,
	// or accepted and should not be, rather than as two tables agreeing with each other.
	for _, octets := range []int{65334, 65335, 65532} {
		if _, err := aliceGroup.Send(ctx, strings.Repeat("u", octets)); !errors.Is(err, urmessage.ErrTextTooLong) {
			t.Errorf("a %d octet text answered %v, want ErrTextTooLong", octets, err)
		}
	}
	// AND THE FLOOR, WHICH MOVED THE OTHER WAY AND IS A PRODUCT CHANGE RATHER THAN A RUNG ONE.
	// TEXT's body is a tail of at least one octet (rule R-d), so an empty line is a text the
	// format has no encoding for and Send refuses it. Before the envelope it sealed a record with
	// an empty body. It is asserted here so the change is gated rather than discovered.
	if _, err := aliceGroup.Send(ctx, ""); !errors.Is(err, urmessage.ErrContentMalformed) {
		t.Errorf("an empty text answered %v, want ErrContentMalformed", err)
	}
	assertNothingFailedToOpen(t, "bob", bobGroup)
}
