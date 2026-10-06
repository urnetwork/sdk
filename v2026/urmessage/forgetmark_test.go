package urmessage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// filesUnder is every regular file under one directory, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	out := []string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

// A LEAVE THAT STOPS PART WAY IS FINISHED, NOT LOST (msgrepo ledger §7, 2026-10-03, review H1). The
// store half, on every platform: the erase stops after the epoch states and before everything
// else, which is what a file another process holds open, or a crash, leaves behind. Without the
// mark that group was refused by every restore after it and named by nothing.
//
// WHAT WOULD GO RED: a GroupRecords that answers a marked group (the restore that fails at every
// launch), and a store that cannot list the groups being left (nothing finishes them). An erase
// that removes the mark FIRST is not caught here, because this case never interrupts
// DeleteGroupRecord itself: cp3b's held-open test catches it, on Windows only.
func TestAnEraseThatStopsAfterTheKeysIsFinishedAndNeverRestored(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	for epoch := uint64(0); epoch < 3; epoch += 1 {
		if err := store.PutGroupState(testGroupId, epoch, testState); err != nil {
			t.Fatalf("PutGroupState %d: %v", epoch, err)
		}
	}
	if err := store.PutGroupRecord(&GroupRecord{
		GroupId: testGroupId, PqSecret: testPriv, GroupHandleKey: testPub, Epoch: 2, Opened: true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	if err := store.PutSentRecord(testGroupId, testSentRecord(1, "a line this device sent")); err != nil {
		t.Fatalf("PutSentRecord: %v", err)
	}
	groupDir := store.groupDir(testGroupId)
	if records, err := store.GroupRecords(); err != nil || len(records) != 1 {
		t.Fatalf("CONTROL FAILED: before any mark the store answers %d record(s), %v", len(records), err)
	}

	if err := store.MarkGroupForgetting(testGroupId); err != nil {
		t.Fatalf("MarkGroupForgetting: %v", err)
	}
	// THE ERASE STOPS HERE: the keys are gone, and the sent copy, the record and the mark are not
	store.lock.Lock()
	err := store.deleteEpochsLocked(testGroupId, ^uint64(0))
	store.lock.Unlock()
	if err != nil {
		t.Fatalf("deleteEpochsLocked: %v", err)
	}
	if len(filesUnder(t, groupDir)) == 0 {
		t.Fatalf("CONTROL FAILED: the interrupted erase left nothing to finish")
	}

	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("a group being left was answered as a group to restore, and its keys are gone")
	}
	marked, err := store.GroupBeingForgotten(testGroupId)
	if err != nil || !marked {
		t.Errorf("GroupBeingForgotten answered %v, %v for a marked group", marked, err)
	}
	leaving, err := store.GroupsBeingForgotten()
	if err != nil || len(leaving) != 1 || string(leaving[0]) != string(testGroupId) {
		t.Fatalf("GroupsBeingForgotten answered %d id(s), %v", len(leaving), err)
	}

	if err := store.DeleteGroupRecord(leaving[0]); err != nil {
		t.Fatalf("finishing the erase: %v", err)
	}
	if left := filesUnder(t, groupDir); len(left) != 0 {
		t.Errorf("the finished erase left %v", left)
	}
	if leaving, err := store.GroupsBeingForgotten(); err != nil || len(leaving) != 0 {
		t.Errorf("after the erase, %d group(s) are still being left, %v", len(leaving), err)
	}
}

// A GROUP WITH EPOCH STATES AND NO RECORD -- founded and never opened -- is marked and erased like
// any other: its epoch states are key material. And a mark copied into another group's directory
// is refused rather than obeyed: it would send the erase to a group nobody left.
func TestAMarkNeedsNoRecordAndMustNameTheDirectoryItSitsIn(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	// a group founded and never opened, so it has no record
	lone := bytes.Repeat([]byte{0x5A}, GroupIdBytes)
	if err := store.PutGroupState(lone, 0, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	if err := store.MarkGroupForgetting(lone); err != nil {
		t.Fatalf("marking a group with no record: %v", err)
	}
	if err := store.DeleteGroupRecord(lone); err != nil {
		t.Fatalf("erasing it: %v", err)
	}
	if left := filesUnder(t, store.groupDir(lone)); len(left) != 0 {
		t.Errorf("the erase of a group with no record left %v", left)
	}
	for _, groupId := range [][]byte{testGroupId, testGroupId2} {
		if err := store.PutGroupRecord(&GroupRecord{
			GroupId: groupId, PqSecret: testPriv, GroupHandleKey: testPub, Epoch: 1, Opened: true,
		}); err != nil {
			t.Fatalf("PutGroupRecord: %v", err)
		}
	}
	if err := store.MarkGroupForgetting(testGroupId); err != nil {
		t.Fatalf("MarkGroupForgetting: %v", err)
	}
	if leaving, err := store.GroupsBeingForgotten(); err != nil || len(leaving) != 1 {
		t.Fatalf("CONTROL FAILED: one honest mark answered %d, %v", len(leaving), err)
	}
	source, err := os.Open(store.forgettingPath(testGroupId))
	if err != nil {
		t.Fatalf("opening the mark: %v", err)
	}
	raw, err := io.ReadAll(source)
	source.Close()
	if err != nil {
		t.Fatalf("reading the mark: %v", err)
	}
	if err := os.WriteFile(store.forgettingPath(testGroupId2), raw, 0o600); err != nil {
		t.Fatalf("copying the mark: %v", err)
	}
	if _, err := store.GroupsBeingForgotten(); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("a mark naming another group's directory answered %v, want ErrStateStoreFormat", err)
	}
}
