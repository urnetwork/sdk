//go:build windows

package urmessage

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A LEFTOVER WRITE THAT A THIRD PARTY IS HOLDING OPEN DOES NOT STOP THE DEVICE STARTING.
//
// THE DEFECT THIS PINS. [sweepStateTempFiles] runs inside [OpenDurableStateStore] and `os.Remove`s
// every `.writing-*` it finds. It used to RETURN any failure that was not `os.ErrNotExist`, so the
// store did not open at all -- and on Windows `os.Remove` of a file another handle holds without
// FILE_SHARE_DELETE answers ERROR_ACCESS_DENIED, which is the same mechanism, from the same causes
// (a scanner, the search indexer, a backup agent), that
// [TestAWriteWhoseDestinationIsHeldOpenFailsLoudlyAndIsNotRetried] pins for MoveFileEx.
//
// BOTH PRECONDITIONS CO-OCCUR BY CONSTRUCTION, which is why this is not a curiosity: a `.writing-*`
// exists BECAUSE the process died, and reopening after a crash is exactly when a scanner is walking
// freshly-changed files. S2-29 as filed surfaces as one message send failing at random; here the
// same transient was the app not starting, which is strictly worse than the state the sweep exists
// to repair -- before the sweep existed, the store opened over the leftover quite happily.
//
// WHAT IS ASSERTED, AND WHAT IS DELIBERATELY NOT. The store MUST open. Whether this particular
// platform's `os.Remove` actually refuses is not asserted: this machine's Windows build may or may
// not, and an assertion that the removal FAILED would be a case about a scanner rather than about
// this store. So the case holds the property that matters on every platform -- the open succeeds --
// and reports which of the two paths it took.
//
// WHAT WOULD GO RED WITHOUT THE FIX: `OpenDurableStateStore` returns ErrStateStoreState and the
// device has no state store at all.
func TestALeftoverWriteHeldOpenByAThirdPartyStillLetsTheStoreOpen(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupState(testGroupId, 1, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	epochDir := store.epochDir(testGroupId)
	store.Close()

	// the leftover a killed process leaves, planted with the store closed. It decodes as a
	// complete epoch state, which is what makes it worth sweeping at all.
	record, err := encodeStateRecord(stateKindGroupState, testGroupId, make([]byte, 8), testState)
	if err != nil {
		t.Fatalf("encodeStateRecord: %v", err)
	}
	leftover := filepath.Join(epochDir, stateTempPrefix+"999999")
	if err := os.WriteFile(leftover, record, 0o600); err != nil {
		t.Fatalf("planting the leftover: %v", err)
	}

	// EXACTLY WHAT A SCANNER DOES, and the same primitive the committed rename case uses: a READ
	// handle with dwShareMode = 0, which is what makes a delete of this path ERROR_ACCESS_DENIED.
	name, err := syscall.UTF16PtrFromString(leftover)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	var handle syscall.Handle
	for attempt := 0; ; attempt += 1 {
		handle, err = syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil,
			syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			break
		}
		if 50 <= attempt {
			t.Fatalf("holding the leftover open, after %d attempts: %v", attempt, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	held := true
	defer func() {
		if held {
			syscall.CloseHandle(handle)
		}
	}()

	// ── THE ASSERTION: the device starts ─────────────────────────────────────────────────
	reopened, err := OpenDurableStateStore(dir)
	if err != nil {
		t.Fatalf("a third party's momentary handle on a crash leftover stopped the device starting: %v", err)
	}
	unswept := reopened.UnsweptWrites()
	reopened.Close()

	switch len(unswept) {
	case 0:
		t.Log("os.Remove tolerated the handle on this platform, so the leftover was swept anyway")
	default:
		t.Logf("the leftover could not be removed and is NAMED rather than fatal: %v", unswept)
		if unswept[0] != leftover {
			t.Errorf("UnsweptWrites named %v and the leftover is %s", unswept, leftover)
		}
		// and it is gone at the next open, once the third party has let go, which is the
		// whole of why carrying on is safe: the sweep bounds how long a leftover lives to
		// one open, not to zero.
		syscall.CloseHandle(handle)
		held = false
		again, err := OpenDurableStateStore(dir)
		if err != nil {
			t.Fatalf("the open after the handle was released: %v", err)
		}
		if still := again.UnsweptWrites(); len(still) != 0 {
			t.Errorf("the next open did not sweep the leftover either: %v", still)
		}
		again.Close()
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("the leftover survived the next open: %v", err)
		}
		t.Log("and the next open removed it, so the leftover's life is bounded by ONE open and not by this one")
	}
}

// AND A WALK THAT CANNOT BE PERFORMED AT ALL IS STILL FATAL, which is the other half of the
// decision and is the clause that keeps "non-fatal" from meaning "ignored".
//
// THE DIFFERENCE IS WHAT THE FAILURE MEANS. A leftover that will not delete is one file this store
// does not need; a data directory that will not enumerate is the store's own READING path
// answering, and every [DurableStateStore.GetGroupState] after it is going to meet the same thing.
// Opening over that would be handing the caller a store that cannot answer.
//
// IT DRIVES THE FUNCTION AND NOT [OpenDurableStateStore], AND THAT IS DECLARED RATHER THAN HIDDEN.
// At the open site the walk error is unreachable on this platform without ACL games: `os.MkdirAll`
// runs on the same path two lines above and refuses first, so a case that planted a file at the
// data directory's name would pass on the MkdirAll refusal and assert NOTHING about the walk --
// which is exactly what the first draft of this case did, and its failure message named MkdirAll.
// The propagation at the open site is the one line `if err != nil { return nil, err }` and it is
// read rather than driven; what is driven here is the decision itself.
//
// WHAT WOULD GO RED IF THE WALK ERROR WERE SWALLOWED TOO: this returns a nil error.
func TestAWalkThatCannotBePerformedIsStillFatalToTheSweep(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-data-directory")
	unswept, err := sweepStateTempFiles(missing)
	if err == nil {
		t.Fatalf("a data directory that cannot be walked was swept clean, answering %v", unswept)
	}
	if !errors.Is(err, ErrStateStoreState) {
		t.Errorf("the refusal does not wrap ErrStateStoreState: %v", err)
	}
	if unswept != nil {
		t.Errorf("a failed walk answered a leftover list as well as an error: %v", unswept)
	}
	t.Logf("refused, by name: %v", err)

	// THE CONTROL, so that this is a case about the WALK and not about any error at all: the
	// same function over a directory that exists answers nothing and no error.
	present := t.TempDir()
	unswept, err = sweepStateTempFiles(present)
	if err != nil || len(unswept) != 0 {
		t.Fatalf("an ordinary empty data directory answered %v / %v", unswept, err)
	}
}
