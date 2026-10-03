// A4, ASSERTED AS A COMPLEMENT: after an epoch change every trackedKey names the NEW epoch, and the
// old-epoch keys the change removed are PRINTED. An empty complement would be the tell that the
// clear did nothing -- that the map never held an old-epoch key, so "only new-epoch keys survive"
// is vacuously true and this gate is measuring nothing. So the case seeds an old-epoch key first,
// then requires it to be gone AND requires the removal to be non-empty.
//
// WHY THE EPOCH IN THE KEY IS LOAD-BEARING, restated as the thing this measures: the receiver
// ratchets are zeroized at every epoch install, so a memo of "this ladder is tracked" that carried
// no epoch would still say tracked after the change and the first open at the new epoch would fail
// "no receiver ratchet is tracked." [Group.crossEpochLadderLocked] clears the memo in the same block
// as the install; this holds that the memo comes back keyed by the new epoch and by nothing older.
package urmessage

import (
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
)

func TestAfterAnEpochChangeEveryTrackedKeyNamesTheNewEpochAndTheOldOnesArePrinted(t *testing.T) {
	root := t.TempDir()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "ladder-epoch-complement")

	alice := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer alice.close()
	bob := openRestoreDevice(t, filepath.Join(root, "bob"))
	defer bob.close()

	// alice founds and adds bob; both reach epoch one.
	handle, err := alice.device.createMlsGroup(groupId)
	if err != nil {
		t.Fatalf("createMlsGroup: %v", err)
	}
	defer handle.Close()
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	mlsSecret, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))
	bobKeyPackage, err := bob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	_, welcome, ratchetTree, err := handle.CommitAdd([][]byte{bobKeyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	bobHandle, err := bob.device.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("bob's JoinFromWelcome: %v", err)
	}
	defer bobHandle.Close()

	session, err := messagegroup.NewGroupSession(handle, pqSecret, groupHandleKey,
		alice.device.reserver, alice.device.nowMs, restoreTestNonce())
	if err != nil {
		t.Fatalf("the session at epoch 1: %v", err)
	}
	group := &Group{
		device:         alice.device,
		id:             append([]byte(nil), groupId...),
		handle:         handle,
		groupHandleKey: groupHandleKey,
		pqSecrets:      map[uint64][]byte{1: pqSecret},
		session:        session,
		epoch:          1,
		opened:         true,
		reconciled:     true,
		// this member is named in the FOUNDING commit, so no leaf here was ever occupied by
		// anybody else and its stream floor is known without asking the server.
		ownFloorHeld: true,
	}
	group.initTables()
	defer session.Close()

	// SEED THE OLD EPOCH. Bob is leaf 1; give this device an authenticated head for bob's DURABLE
	// ladder and a memo that it is tracked AT EPOCH ONE -- which is the state an ordinary Receive at
	// epoch one leaves behind. crossEpochLadderLocked must remove this key and re-install one at
	// epoch two.
	const bobLeaf = uint32(1)
	const bobHead = uint64(5)
	durableWire, err := message.RetentionClassWire(message.RetentionDurable, 0)
	if err != nil {
		t.Fatalf("the durable wire byte: %v", err)
	}
	oldLadder := ladderKey{leaf: bobLeaf, retentionWire: durableWire, ephWindow: 0}
	oldKey := trackedKey{epoch: 1, ladderKey: oldLadder}
	group.peerHeads[oldLadder] = bobHead
	group.tracked[oldKey] = true
	group.ownHeads[trackedKey{epoch: 1, ladderKey: ladderKey{leaf: group.handle.OwnLeafIndex(), retentionWire: durableWire}}] = 3

	before := map[trackedKey]bool{}
	for key := range group.tracked {
		before[key] = true
	}
	if !before[oldKey] {
		t.Fatal("the seed did not take, so the complement below would be measuring nothing")
	}

	// THE EPOCH CHANGE. Bob commits, this device ingests it to epoch two, and its session advances.
	commit, _, _, err := bobHandle.Commit(nil)
	if err != nil {
		t.Fatalf("bob's Commit(nil): %v", err)
	}
	if err := bobHandle.MergePendingCommit(); err != nil {
		t.Fatalf("bob's MergePendingCommit: %v", err)
	}
	processed, err := group.handle.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := group.handle.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	// the harness files what production's publishCommitLocked files, at the epoch it is entering,
	// so the table and the session agree about the epoch below this line as they do above it.
	carried := group.pqSecretLocked()
	group.filePqSecretLocked(group.handle.Epoch(), carried)
	if err := group.session.AdvanceEpoch(carried); err != nil {
		t.Fatalf("AdvanceEpoch: %v", err)
	}
	newEpoch := group.handle.Epoch()
	if newEpoch != 2 {
		t.Fatalf("the handle stands at epoch %d after the ingest, want 2", newEpoch)
	}

	// A4, then A3's door to complete the move the ingest path takes.
	if err := group.crossEpochLadderLocked(newEpoch); err != nil {
		t.Fatalf("crossEpochLadderLocked: %v", err)
	}
	if err := group.enterEpochLocked(); err != nil {
		t.Fatalf("enterEpochLocked: %v", err)
	}

	// THE NARROWING: only new-epoch keys survive. THE COMPLEMENT: everything the clear removed, which
	// must be non-empty and must not include a single new-epoch key.
	removed := []trackedKey{}
	for key := range before {
		if !group.tracked[key] {
			removed = append(removed, key)
		}
	}
	t.Logf("the change to epoch %d removed %d tracked key(s): %+v", newEpoch, len(removed), removed)
	if len(removed) == 0 {
		t.Fatal("the clear removed nothing, so 'only new-epoch keys survive' is vacuously true and this gate measured nothing")
	}
	for _, key := range removed {
		if key.epoch == newEpoch {
			t.Errorf("the clear removed a key already at the new epoch %d: %+v", newEpoch, key)
		}
	}
	for key := range group.tracked {
		if key.epoch != newEpoch {
			t.Errorf("a trackedKey survived the change at epoch %d, and this group is at %d: %+v", key.epoch, newEpoch, key)
		}
	}
	if group.tracked[oldKey] {
		t.Error("the epoch-one key for bob's ladder survived the change to epoch two")
	}
	// re-tracked at the new epoch, at the head this device authenticated -- never at zero and never
	// at the old epoch.
	newKey := trackedKey{epoch: newEpoch, ladderKey: oldLadder}
	if !group.tracked[newKey] {
		t.Errorf("bob's ladder was not re-tracked at epoch %d: tracked=%+v", newEpoch, group.tracked)
	}
	// ownHeads is cleared and not carried; peerHeads is NOT cleared, because it is the head the
	// re-track reads.
	if len(group.ownHeads) != 0 {
		t.Errorf("ownHeads was not cleared across the epoch change: %+v", group.ownHeads)
	}
	if group.peerHeads[oldLadder] != bobHead {
		t.Errorf("peerHeads lost bob's authenticated head across the change: %d, want %d",
			group.peerHeads[oldLadder], bobHead)
	}
	if group.epoch != newEpoch {
		t.Errorf("the door did not move Group.epoch to %d: it is %d", newEpoch, group.epoch)
	}
}
