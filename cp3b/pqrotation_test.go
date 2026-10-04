package cp3b

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/message-server/store"
)

// RULING 37's ORDER, AGAINST A REAL SERVER: THE EPOCH FAN-OUT IS ON THE WIRE BEFORE THE COMMIT.
//
// WHY THIS CASE IS IN cp3b AND NOT IN urmessage. The order is a SERVER-SIDE fact and no page
// assembled in a test can produce it: a write is accepted only at the group's current epoch, so
// the moment the server takes the commit its current_epoch is n+1 and a wrap sealed at n is
// answered REASON_EPOCH_STALE. The fan-out therefore has to go first, and "has to" is a property
// of the server this case drives rather than of a comment.
//
// WHAT RULING 37 IS FOR, because the ordering looks arbitrary without it. Item 246's F0 ceiling
// serves a reader standing at epoch n only rows with epoch <= n. Publish the fan-out after the
// merge and the wrap rows carry n+1, so a member at n is served the commit and NOT its own wrap:
// read_key[n+1] needs pq_secret[n+1] needs the wrap needs read_key[n+1]. Circular, with no server
// change able to break it that does not re-open ruled item 246. Submitting at n breaks it, and
// what this case measures is that the break is real: ONE page at ReadEpoch=n carries the commit
// AND every wrap it depends on.
//
// AND THE TWO CLIENT-DECLARED NUMBERS AGREE FOR A REASON. Item 132's complaint is that the only
// fan-out coverage check is `wrap_count` against `expected_wrap_count`, two numbers a client
// chooses. They are now taken from ONE expression -- the length of the target list -- so a client
// cannot disagree with itself; what a client cannot fix is that neither store counts a wrap row,
// which is why the receive side binds the rows to the epoch through H(epoch_keys) instead
// (urmessage's TestTheThreeWaysADeviceWrapFailsAreThreeSentinelsAndThreeCounters).
//
// WHAT WOULD GO RED: submit the fan-out after MergePendingCommit (every wrap is answered
// REASON_EPOCH_STALE and AddMemberAndPublish returns it); seal the wraps at the epoch the group is
// leaving rather than for the one it opens (the WrapTag epochs below are n, not n+1); take
// expected_wrap_count from the staged member count again (it counts the added leaf, which gets no
// wrap, and the marker never closes the fan-out).
func TestTheEpochFanOutIsSubmittedBeforeTheCommitAndCarriesTheEpochItOpens(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	// the founding fan-out's rows, so the second epoch's can be told from them by record id.
	beforeTheCommit := uint64(0)
	for _, row := range world.allRows(t, groupId) {
		if beforeTheCommit < row.RecordId {
			beforeTheCommit = row.RecordId
		}
	}

	// ── THE SECOND EPOCH ────────────────────────────────────────────────────────────────────
	hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, "carol"))
	if aliceGroup.Epoch() != 2 {
		t.Fatalf("alice is at epoch %d after the add, want 2", aliceGroup.Epoch())
	}

	// ── WHAT THE SERVER HOLDS, IN THE ORDER IT ALLOCATED RECORD IDS ─────────────────────────
	commitId, wrapIds, markerId := uint64(0), []uint64{}, uint64(0)
	wrapEpochs := map[uint64]uint64{}
	expected, declared := uint32(0), uint32(0)
	for _, row := range world.allRows(t, groupId) {
		if row.RecordId <= beforeTheCommit {
			continue
		}
		if row.IsCommit {
			commitId = row.RecordId
			attachment, err := message.ParseServerAttachment(row.ServerAttachment)
			if err != nil {
				t.Fatalf("the commit's attachment: %v", err)
			}
			if attachment.Kind != message.AttachmentEpochDigest || attachment.EpochDigest == nil {
				t.Fatalf("the commit carries attachment kind %#04x, want the epoch digest", uint16(attachment.Kind))
			}
			expected = attachment.EpochDigest.ExpectedWrapCount
			continue
		}
		switch {
		case row.Attachment != nil && row.Attachment.Kind == store.AttachmentWrap:
			wrapIds = append(wrapIds, row.RecordId)
			wrapEpochs[row.RecordId] = row.Epoch
		case row.Attachment != nil && row.Attachment.Kind == store.AttachmentEpochComplete:
			markerId = row.RecordId
			declared = row.Attachment.EpochComplete.WrapCount
		}
	}

	if commitId == 0 || markerId == 0 {
		t.Fatalf("the second epoch's commit or marker is not on the server (commit %d, marker %d)", commitId, markerId)
	}
	if len(wrapIds) == 0 {
		t.Fatalf("the second epoch published no wrap at all, so the order below is vacuous")
	}

	// THE ORDER. Every wrap row is BEFORE the commit row, and the marker is after it.
	for _, id := range wrapIds {
		if commitId < id {
			t.Fatalf("wrap record %d was allocated after the commit at %d; ruling 37 puts the "+
				"fan-out on the wire first, and a wrap after the commit is a write at a stale epoch", id, commitId)
		}
		// AND EACH ONE IS SEALED AT THE EPOCH THE GROUP WAS LEAVING, which is what makes item
		// 246's ceiling serve it to a reader that has not yet followed the commit.
		if wrapEpochs[id] != 1 {
			t.Fatalf("wrap record %d is sealed at epoch %d; the fan-out for epoch 2 is submitted "+
				"at epoch 1, pre-merge", id, wrapEpochs[id])
		}
	}
	if markerId < commitId {
		t.Fatalf("the epoch-complete marker at %d was allocated before the commit at %d", markerId, commitId)
	}

	// THE TWO NUMBERS AGREE, AND THEY COUNT THE LEAVES THAT WERE ALREADY MEMBERS. carol joins at
	// epoch 2 and takes her secret out of the Welcome, so the fan-out addresses alice and bob.
	if expected != declared {
		t.Fatalf("expected_wrap_count is %d and the marker declares %d", expected, declared)
	}
	if expected != uint32(len(wrapIds)) {
		t.Fatalf("expected_wrap_count is %d and %d wrap row(s) were written", expected, len(wrapIds))
	}
	if expected != 2 {
		t.Fatalf("the fan-out for epoch 2 addressed %d leaves, want 2 (alice and bob); carol is "+
			"admitted BY this commit and her copy travels in the Welcome", expected)
	}

	// ── AND THE CIRCULARITY IS BROKEN: bob, still at epoch 1, is served the commit AND its wraps
	//    in one page, follows it, and lands on the same storage root as alice ─────────────────
	if bobGroup.Epoch() != 1 {
		t.Fatalf("bob is at epoch %d before his walk, want 1", bobGroup.Epoch())
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive that must ingest the commit and its wrap: %v", err)
	}
	if bobGroup.Epoch() != 2 {
		t.Fatalf("bob is at epoch %d after the walk, want 2", bobGroup.Epoch())
	}
	stats := bobGroup.Stats()
	if stats.WrapOpened != 1 {
		t.Fatalf("bob opened %d wrap(s) across the epoch change, want 1", stats.WrapOpened)
	}
	for what, got := range map[string]uint64{
		"WrapMissing":    stats.WrapMissing,
		"WrapUnreadable": stats.WrapUnreadable,
		"WrapOrphaned":   stats.WrapOrphaned,
	} {
		if got != 0 {
			t.Errorf("bob's Stats.%s is %d after an ordinary epoch change", what, got)
		}
	}

	// THE PROPERTY, THROUGH THE ONLY OBSERVABLE A SERVER-BACKED CASE HAS: both members write and
	// read at the new epoch. Two members that disagreed about pq_secret[2] would disagree about
	// storage_root[2], so write_key[2] would differ and the server would refuse the write_auth
	// before any AEAD -- which is exactly the undiagnosable REASON_REJECTED ruling 38 names.
	const fromAlice = "alice at the rotated epoch"
	const fromBob = "bob at the rotated epoch"
	if _, err := aliceGroup.Send(ctx, fromAlice); err != nil {
		t.Fatalf("alice's send at the rotated epoch: %v", err)
	}
	if _, err := bobGroup.Send(ctx, fromBob); err != nil {
		t.Fatalf("bob's send at the rotated epoch: %v", err)
	}
	gcReceiveText(t, ctx, "bob", bobGroup, fromAlice)
	gcReceiveText(t, ctx, "alice", aliceGroup, fromBob)
}
