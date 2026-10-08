//go:build windows

package urmessage

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A WRITE WHOSE DESTINATION A THIRD PARTY HOLDS OPEN FAILS, LOUDLY, AND THE SAME WRITE SUCCEEDS
// THE MOMENT THE HANDLE IS RELEASED. THIS IS A CHARACTERISATION OF A DEFECT, NOT AN ENDORSEMENT.
//
// WHY THIS CASE EXISTS: `sdk/cp3b` is intermittently red on Windows, and the reason is here rather
// than in cp3b. One run in 100 failed at `alice's AddMember` with:
//
//	urmessage: the durable state store could not be read or written: ...\epoch\.writing-2475656454
//	could not be renamed onto ...\epoch\0000000000000000: rename ...: Access is denied.
//
// MEASURED IN BOTH DIRECTIONS, because "it is pre-existing" and "I caused it" are two claims and
// neither was checked when the flake appeared: `sdk/cp3b` on this tree, 100 `-race` runs -> 1
// failure; the same suite at 1141236 extracted into a sibling directory, 110 `-race` runs -> 0.
// ONE EVENT DISTINGUISHES NOTHING between a per-write probability and a difference between the
// trees, and this tree's suite is 22 cases against 12 and copies a live store's whole directory
// into %TEMP% six times, so it does several times the file churn a scanner reacts to. What IS
// settled is the SITE and the MECHANISM, below, and that writeRecord's rename is not touched by
// the commit this case ships in.
//
// A flake with no named cause is a flake somebody will eventually "fix" by re-running, so the
// cause is pinned here.
//
// THE MECHANISM, REPRODUCED EXACTLY BY THIS CASE AND NOT INFERRED. Windows `MoveFileEx` with
// MOVEFILE_REPLACE_EXISTING -- which is what `os.Rename` is -- answers ERROR_ACCESS_DENIED when
// ANY other handle is open on the DESTINATION without FILE_SHARE_DELETE. That is precisely what a
// virus scanner, the search indexer or a backup agent does to a file that has just been created,
// for a few milliseconds, at a moment nothing in this process controls. The case below opens the
// destination with `dwShareMode = 0`, which is that condition with the timing removed.
//
// WHAT IS **NOT** WRONG HERE, and it is the reason this is a defect and not a disaster:
// [DurableStateStore.writeRecord]'s ordering is intact. The value was fsync'd into the temp file
// before the rename was attempted, the rename either happened or did not, the `defer` removes the
// temp, and the CALLER IS TOLD. Nothing is half-written, nothing is silently lost, and the
// previous value is still under the name. This is the store failing closed, which is what it is
// built to do.
//
// WHAT IS WRONG: it is TRANSIENT and it is treated as terminal. On a real client this surfaces as
// an MLS commit or a message send failing at random, on Windows, for a reason the user cannot act
// on -- and `mls` has no retry above it. **FILED AS S2-29: writeRecord's rename is not retried,
// and on Windows a third party's momentary handle on the destination makes a durable write fail.**
//
// THE REPAIR IS NAMED AND IS DELIBERATELY NOT TAKEN HERE, because it is a change to the one path
// that makes a value observable and it needs a budget somebody owns: retry the rename on
// ERROR_ACCESS_DENIED and ERROR_SHARING_VIOLATION only, a bounded number of times, with a short
// backoff, and keep the refusal by name when the budget runs out -- so that a real permission
// problem stays a refusal rather than becoming a hang. The second half of this case is what would
// hold the bound: a destination held for longer than the budget must still fail.
//
// WHEN THAT LANDS, THIS CASE CHANGES. That is the point of a characterisation test: it is the
// thing that goes red when the behaviour it pins is repaired, and it names the item that repaired
// it.
func TestAWriteWhoseDestinationIsHeldOpenFailsLoudlyAndIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupState(testGroupId, 0, testState); err != nil {
		t.Fatalf("the first write, with nothing holding anything: %v", err)
	}
	target := store.epochDir(testGroupId) + `\` + stateEpochName(0)

	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Fatalf("%s is not a path this platform can name: %v", target, err)
	}
	// EXACTLY WHAT A SCANNER DOES: open the destination with dwShareMode = 0. It is a READ
	// handle -- this case is not writing to the file, it is merely holding it, which is the
	// whole point.
	//
	// RETRIED, AND THE IRONY IS THE REASON. This open can itself lose to the very thing this
	// case is about: a scanner holding the file for a few milliseconds makes an exclusive open
	// answer ERROR_SHARING_VIOLATION. A case that documents an intermittent failure and is
	// itself intermittent would be worse than no case at all, so the SETUP is retried and the
	// ASSERTIONS are not.
	var handle syscall.Handle
	for attempt := 0; ; attempt += 1 {
		handle, err = syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil,
			syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			break
		}
		if 50 <= attempt {
			t.Fatalf("holding the destination open, after %d attempts: %v", attempt, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	held := true
	defer func() {
		if held {
			syscall.CloseHandle(handle)
		}
	}()

	replacement := []byte("a second epoch state, which must not half-land")
	err = store.PutGroupState(testGroupId, 0, replacement)
	if err == nil {
		t.Fatal("the write succeeded over a held destination; the mechanism this case pins is gone and its documentation is now wrong")
	}
	if !errors.Is(err, ErrStateStoreState) {
		t.Fatalf("the refusal is %v, want one wrapping ErrStateStoreState", err)
	}
	if !strings.Contains(err.Error(), "could not be renamed onto") {
		t.Fatalf("the write failed somewhere other than the rename, so this case is pinning the wrong site: %v", err)
	}
	t.Logf("held destination: %v", err)

	// NO TEMP FILE WAS LEFT BEHIND: the `defer` ran, because the CALL returned. This is the
	// one assertion that can be made while the handle is still held -- a share mode of zero
	// blocks readers too, so the value under the name cannot be read until it is released,
	// which is itself worth knowing: a scanner holding this file makes GetGroupState fail as
	// well as PutGroupState.
	for entry := range epochNamesOnDisk(t, store, testGroupId) {
		if strings.HasPrefix(entry, stateTempPrefix) {
			t.Errorf("%s was left behind by a rename that failed", entry)
		}
	}

	syscall.CloseHandle(handle)
	held = false

	// FAILED CLOSED. With the handle gone, the value under the name is the PREVIOUS one --
	// not half of the new one, and not absent.
	back, err := store.GetGroupState(testGroupId, 0)
	if err != nil {
		t.Fatalf("after the failed write the previous value cannot be read: %v", err)
	}
	if !bytes.Equal(back, testState) {
		t.Fatalf("after the failed write the value under this name is %q, so the write half-landed", back)
	}

	// AND IT IS TRANSIENT, which is the half that makes it a defect. The same call, nothing
	// else changed, succeeds now.
	if err := store.PutGroupState(testGroupId, 0, replacement); err != nil {
		t.Fatalf("with the handle released the same write still fails, so the cause is not the handle: %v", err)
	}
	back, err = store.GetGroupState(testGroupId, 0)
	if err != nil {
		t.Fatalf("GetGroupState after the successful retry: %v", err)
	}
	if !bytes.Equal(back, replacement) {
		t.Fatalf("the retry wrote %q", back)
	}
	t.Log("the same write succeeds the moment the handle is released: the failure is TRANSIENT and the rename is the site (S2-29)")
}
