package urmessage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A KILL BETWEEN CreateTemp AND Rename LEAVES A COMPLETE EPOCH STATE, AND NOTHING USED TO REMOVE IT.
//
// [DurableStateStore.writeRecord] is temp file, fsync, rename, and the `defer` that removes the
// temp on failure runs only if the CALL RETURNS. A process killed in between returns from nothing,
// and what it leaves in the epoch directory is a `.writing-XXXXXXXX` holding the whole value --
// for an epoch state, this member's leaf HPKE private key and its TreeKEM path-secret ladder.
//
// THE STATE IS CONSTRUCTED HERE RATHER THAN RACED FOR, and that is the honest way round: the
// leftover is a FILE with a known name shape in a known directory, so a case that produced it by
// killing a child process at the right microsecond would be testing the scheduler. What is under
// test is what the store does with the file, and the file is the same file however it got there.
// The control below is that the octets really are findable on the disk before the discard, so a
// green result cannot mean "this case wrote nothing".
//
// WHAT GOES RED WITHOUT THE FIX: delete the sweepStateTempFiles call in OpenDurableStateStore and
// the reopened store still holds the leftover.
func TestAnUnfinishedWriteLeftByAKillIsSweptAtTheNextOpen(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupState(testGroupId, 1, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	epochDir := store.epochDir(testGroupId)

	// what a process killed between CreateTemp and Rename leaves: a complete, decodable record
	// under a .writing- name. It is written through the store's own encoder, so this is the
	// value writeRecord would have renamed rather than a hand-rolled lookalike.
	record, err := encodeStateRecord(stateKindGroupState, testGroupId, make([]byte, 8), testState)
	if err != nil {
		t.Fatalf("encodeStateRecord: %v", err)
	}
	leftover := filepath.Join(epochDir, stateTempPrefix+"1402314117")
	if err := os.WriteFile(leftover, record, 0o600); err != nil {
		t.Fatalf("planting the leftover: %v", err)
	}
	// THE CONTROL ON THE CONTROL: it decodes. If this failed, every assertion below would be
	// about debris rather than about a readable copy of a discarded epoch.
	parts, err := decodeStateRecord(record, stateKindGroupState)
	if err != nil {
		t.Fatalf("the leftover this case plants is not a decodable epoch state, so it is holding nothing: %v", err)
	}
	if len(parts) != 3 || !bytes.Equal(parts[2], testState) {
		t.Fatalf("the leftover decodes to %d part(s) and its state is not the one written", len(parts))
	}
	if found := filesHoldingOctets(t, filepath.Join(dir, stateDataDirName), testState); found < 2 {
		t.Fatalf("the epoch state octets are on the disk in %d file(s) before the sweep; this case needs the record AND the leftover", found)
	}
	store.Close()

	// the sweep
	reopened := openTestStore(t, dir)
	names := epochNamesOnDisk(t, reopened, testGroupId)
	for name := range names {
		if strings.HasPrefix(name, stateTempPrefix) {
			t.Errorf("%s survived the open; a process that died mid-write leaves a complete epoch state and nothing swept it", name)
		}
	}
	// and the real record is untouched, which is what says the sweep removed the leftover and
	// not the value.
	back, err := reopened.GetGroupState(testGroupId, 1)
	if err != nil {
		t.Fatalf("the epoch state this store did finish writing is gone after the sweep: %v", err)
	}
	if !bytes.Equal(back, testState) {
		t.Fatalf("the epoch state came back as %q", back)
	}
	t.Logf("the epoch directory after the open: %v", names)
}

// AND A DISCARD REMOVES IT RATHER THAN REPORTING SUCCESS OVER SOMETHING IT CANNOT SEE.
//
// This is the same leftover, met by the other half of the repair. Section 5.12's erase is sold on
// a RE-READ -- "the clause that makes deleted a measurement rather than an intention" -- and that
// re-read used to consider only entries parsing as sixteen hex digits. So the one file the discard
// could not delete was also the one file its measurement could not see, and the total erase
// reported success with the leaf private key still on the disk.
//
// TWO ANSWERS ARE BOTH CORRECT AND THE CASE TAKES EITHER: the discard removes the leftover,
// because writeRecord made it in this very directory and it holds this group's own epoch state; or
// it refuses by name. What is NOT acceptable is the third answer, which is what shipped: success,
// with the octets still there.
func TestADiscardThatCannotSeeAnUnfinishedWriteRefusesRatherThanReportingSuccess(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupState(testGroupId, 1, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	record, err := encodeStateRecord(stateKindGroupState, testGroupId, make([]byte, 8), testState)
	if err != nil {
		t.Fatalf("encodeStateRecord: %v", err)
	}
	leftover := filepath.Join(store.epochDir(testGroupId), stateTempPrefix+"465954513")
	if err := os.WriteFile(leftover, record, 0o600); err != nil {
		t.Fatalf("planting the leftover: %v", err)
	}

	if err := store.DeleteGroupRecord(testGroupId); err != nil {
		if !errors.Is(err, ErrStateStoreState) {
			t.Fatalf("DeleteGroupRecord refused with %v, want one wrapping ErrStateStoreState", err)
		}
		t.Logf("the discard refused rather than reporting success: %v", err)
	}
	if found := filesHoldingOctets(t, filepath.Join(dir, stateDataDirName), testState); found != 0 {
		t.Errorf("DeleteGroupRecord reported success and the epoch state octets are still on the disk in %d file(s); section 5.12's erase is total or it is not an erase", found)
	}
	if _, err := os.Stat(leftover); err == nil {
		t.Errorf("%s survived a discard that reported success", leftover)
	}
	// and the epoch directory itself is gone, which it could not be while a temp file stood in
	// it: os.Remove of a non-empty directory fails and that failure is swallowed as best effort.
	if _, err := os.Stat(store.epochDir(testGroupId)); err == nil {
		t.Errorf("%s survives the discard, so something is still standing in it", store.epochDir(testGroupId))
	}
}

// AN ENTRY IN THE EPOCH DIRECTORY THAT IS NEITHER AN EPOCH NOR AN UNFINISHED WRITE IS A FINDING.
//
// [DurableStateStore]'s header says "an entry in the data directory that is not a record is a
// finding" and the code did not enforce it: the discard's re-read walked past every name that did
// not parse as an epoch. This is that sentence made executable, and it is what stops the repair
// above from being narrowed back to "sweep the names we happen to make".
func TestADiscardRefusesAnEntryInTheEpochDirectoryItDoesNotRecognise(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupState(testGroupId, 1, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	stranger := filepath.Join(store.epochDir(testGroupId), "not-an-epoch")
	if err := os.WriteFile(stranger, []byte("something a third party put here"), 0o600); err != nil {
		t.Fatalf("planting the stranger: %v", err)
	}
	err := store.DeleteGroupStateBefore(testGroupId, 2)
	if !errors.Is(err, ErrStateStoreState) {
		t.Fatalf("a discard over an epoch directory holding %q answered %v, want a refusal wrapping ErrStateStoreState",
			filepath.Base(stranger), err)
	}
	if !strings.Contains(err.Error(), "not-an-epoch") {
		t.Errorf("the refusal %v does not name the entry it refused", err)
	}
	t.Logf("the refusal names the entry: %v", err)
}

// filesHoldingOctets is how many files under dir contain these octets. It is a byte grep of the
// whole tree and not a stat of a path, so a copy under a name this test did not predict is still
// counted -- which is the entire point: the defect was a file nothing looked at.
//
// It is pointed at the DATA directory rather than the store directory, and that is not a way of
// avoiding an inconvenient file: the guard entry lives beside the data directory precisely so that
// "an entry in the data directory that is not a record is a finding" stays categorical, and on
// Windows the exclusion holds it open with share mode zero so no reader can open it at all.
func filesHoldingOctets(t *testing.T, dir string, needle []byte) int {
	t.Helper()
	found := 0
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, needle) {
			found += 1
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}
