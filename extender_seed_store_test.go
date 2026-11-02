package sdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The platform storage of the accepted seed state (extender_seed_store.go):
// one dot file replaced atomically, so the bundle and the sequence it carried
// are never torn apart (EXTENDER-UPDATE2.md §2.2).

func newTestSeedLocalState(t *testing.T) *LocalState {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newLocalState(ctx, testSeedStoragePath(t))
}

// The temporary files a replacement left in the store's directory.
func testSeedStoreTemps(t *testing.T, localState *LocalState) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(localState.localStorageDir, extenderSeedStoreTempPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestExtenderSeedStoreRoundTrip(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)

	if data, err := store.Load(); err != nil || data != nil {
		t.Fatalf("a store never written loads %q, %v", data, err)
	}
	if err := store.Save([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || string(data) != "second" {
		t.Fatalf("the store loads %q, %v, not the last value saved", data, err)
	}
	info, err := os.Stat(filepath.Join(localState.localStorageDir, extenderSeedStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != LocalStorageFilePermissions {
		t.Errorf("the store file is %v, not %v", info.Mode().Perm(), os.FileMode(LocalStorageFilePermissions))
	}
	if temps := testSeedStoreTemps(t, localState); len(temps) != 0 {
		t.Errorf("a replacement left temporary files %v", temps)
	}

	// an empty value removes the document, which is what a reset leaves
	if err := store.Save(nil); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || data != nil {
		t.Fatalf("a cleared store loads %q, %v", data, err)
	}
	if err := store.Save([]byte("third")); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || data != nil {
		t.Fatalf("a cleared store loads %q, %v", data, err)
	}
}

// A replacement that fails at any step before its rename leaves the previous
// document in place, whole, and removes its temporary file. A failure after
// the rename has already committed, so the new document is what loads.
func TestExtenderSeedStoreFailedReplacementKeepsThePreviousDocument(t *testing.T) {
	for _, step := range []string{"create", "write", "sync", "close", "rename"} {
		localState := newTestSeedLocalState(t)
		store := newLocalStateExtenderSeedStore(localState)
		if err := store.Save([]byte("previous")); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("the process ended")
		store.testingCommitStep = func(commitStep string) error {
			if commitStep == step {
				return injected
			}
			return nil
		}
		if err := store.Save([]byte("next")); !errors.Is(err, injected) {
			t.Fatalf("%s: the failed replacement returned %v", step, err)
		}
		store.testingCommitStep = nil
		// a restart reads the store afresh
		restarted := newLocalStateExtenderSeedStore(localState)
		if data, err := restarted.Load(); err != nil || string(data) != "previous" {
			t.Errorf("%s: after the failure the store loads %q, %v, not the previous document", step, data, err)
		}
		if temps := testSeedStoreTemps(t, localState); len(temps) != 0 {
			t.Errorf("%s: the failed replacement left %v", step, temps)
		}
	}

	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	if err := store.Save([]byte("previous")); err != nil {
		t.Fatal(err)
	}
	store.testingCommitStep = func(commitStep string) error {
		if commitStep == "directory sync" {
			return errors.New("the directory did not sync")
		}
		return nil
	}
	if err := store.Save([]byte("next")); err != nil {
		t.Fatalf("a replacement that renamed reported %v", err)
	}
	if data, err := newLocalStateExtenderSeedStore(localState).Load(); err != nil || string(data) != "next" {
		t.Errorf("a renamed replacement loads %q, %v", data, err)
	}
}

// A document over the size limit is no document: it is not one connect wrote,
// and the next accept replaces it. One is never written.
func TestExtenderSeedStoreOversizedDocumentIsNone(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	oversized := bytes.Repeat([]byte{'x'}, extenderSeedStoreMaxByteCount+1)
	if err := store.Save(oversized); !errors.Is(err, errExtenderSeedStoreTooLarge) {
		t.Fatalf("an oversized document was saved: %v", err)
	}
	path := filepath.Join(localState.localStorageDir, extenderSeedStoreFileName)
	if err := os.WriteFile(path, oversized, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || data != nil {
		t.Fatalf("an oversized document loads %d bytes, %v", len(data), err)
	}
	// exactly at the limit is a document
	atLimit := bytes.Repeat([]byte{'x'}, extenderSeedStoreMaxByteCount)
	if err := store.Save(atLimit); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || len(data) != len(atLimit) {
		t.Fatalf("a document at the limit loads %d bytes, %v", len(data), err)
	}
}

// Something other than a regular file at the store's path is an error, which
// the acceptor treats as a refusal rather than as no history.
func TestExtenderSeedStoreNonRegularPathIsAnError(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	path := filepath.Join(localState.localStorageDir, extenderSeedStoreFileName)
	if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, errExtenderSeedStoreNotRegular) {
		t.Errorf("a directory at the store path loads with %v", err)
	}
	if err := store.Save([]byte("value")); !errors.Is(err, errExtenderSeedStoreNotRegular) {
		t.Errorf("a save over a directory returned %v", err)
	}
	acceptor := connect.NewExtenderSeedAcceptor(store, nil)
	if _, err := acceptor.Sequence(testSeedNetworkHost, testSeedRootKeySet(t)); connect.ExtenderSeedErrorCode(err) != connect.ExtenderSeedCodeStore {
		t.Errorf("the acceptor read an unreadable store as %v", err)
	}
}

// A replacement removes the temporary files a process that ended mid-write
// left behind, once they are too old to be another live replacement's, and
// never one that is not.
func TestExtenderSeedStoreRemovesOnlyStaleTemporaryFiles(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	stalePath := filepath.Join(localState.localStorageDir, extenderSeedStoreTempPrefix+"stale")
	livePath := filepath.Join(localState.localStorageDir, extenderSeedStoreTempPrefix+"live")
	for _, path := range []string{stalePath, livePath} {
		if err := os.WriteFile(path, []byte("partial"), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * extenderSeedStoreStaleTempTimeout)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	// a partial temporary file is never what loads
	if data, err := store.Load(); err != nil || data != nil {
		t.Fatalf("a store with only temporary files loads %q, %v", data, err)
	}
	if err := store.Save([]byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale temporary file was kept: %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("a temporary file another replacement may own was removed: %v", err)
	}
}

// A sign-out keeps the accepted seeds and their floors, as it keeps every
// other piece of extender state.
func TestExtenderSeedStoreKeptAcrossSignOut(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	if err := store.Save([]byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if data, err := store.Load(); err != nil || string(data) != "value" {
		t.Fatalf("after a sign-out the seed store loads %q, %v", data, err)
	}
	if !keptAcrossSignOut(extenderSeedStoreFileName) {
		t.Error("the sign-out keep list does not name the seed store")
	}
	if keptAcrossSignOut(extenderSeedStoreTempPrefix + "x") {
		t.Error("the sign-out keep list keeps a temporary file")
	}
}

// The store serves connect's acceptor: an accepted bundle and its floor are
// what a restarted acceptor on the same file reads back, and a store with no
// storage is the zero value, which refuses to save rather than pretending to.
func TestExtenderSeedStoreServesTheAcceptorAcrossARestart(t *testing.T) {
	clock := newTestSeedClock()
	localState := newTestSeedLocalState(t)
	rootKeys := testSeedRootKeySet(t)
	networkHosts := []string{testSeedNetworkHost}

	bundle := testSeedVerifiedBundle(t, clock.Now(), testSeedBundleSpec{sequence: 7})
	acceptor := connect.NewExtenderSeedAcceptor(newLocalStateExtenderSeedStore(localState), nil)
	if outcome, err := acceptor.Accept(bundle, rootKeys); err != nil || outcome != connect.ExtenderSeedOutcomeAccepted {
		t.Fatalf("accept: %s, %v", outcome, err)
	}

	restarted := connect.NewExtenderSeedAcceptor(newLocalStateExtenderSeedStore(localState), nil)
	sequence, err := restarted.Sequence(testSeedNetworkHost, rootKeys)
	if err != nil || sequence != 7 {
		t.Fatalf("the restarted floor is %d, %v", sequence, err)
	}
	stored, err := restarted.Stored(testSeedNetworkHost, rootKeys, networkHosts, clock.Now())
	if err != nil || stored == nil || stored.Sequence != 7 {
		t.Fatalf("the restarted bundle is %v, %v", stored, err)
	}
	older := testSeedVerifiedBundle(t, clock.Now(), testSeedBundleSpec{sequence: 6})
	if _, err := restarted.Accept(older, rootKeys); connect.ExtenderSeedErrorCode(err) != connect.ExtenderSeedCodeRollback {
		t.Errorf("an older bundle after the restart: %v", err)
	}

	var noStorage localStateExtenderSeedStore
	if err := noStorage.Save([]byte("value")); !errors.Is(err, errExtenderSeedStoreClosed) {
		t.Errorf("a store with no storage saved: %v", err)
	}
	if data, err := noStorage.Load(); err != nil || data != nil {
		t.Errorf("a store with no storage loads %q, %v", data, err)
	}
}

func testSeedRootKeySet(t *testing.T) *connect.ExtenderRootKeySet {
	t.Helper()
	rootKeys, err := connect.NewExtenderRootKeySetFromHex(testSeedRootPublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	return rootKeys
}

// A fixture bundle as VerifyExtenderSeed returns it.
func testSeedVerifiedBundle(t *testing.T, now time.Time, spec testSeedBundleSpec) *connect.ExtenderSeedBundle {
	t.Helper()
	envelope := testSeedEnvelope(t, now, spec)
	networkHost := spec.networkHost
	if networkHost == "" {
		networkHost = testSeedNetworkHost
	}
	bundle, err := connect.VerifyExtenderSeed(envelope, testSeedRootKeySet(t), []string{networkHost}, now)
	if err != nil {
		t.Fatalf("the fixture bundle does not verify: %v", err)
	}
	return bundle
}

// The fixture helpers name no path outside the test's storage.
func TestExtenderSeedStorePathIsTheDotFile(t *testing.T) {
	localState := newTestSeedLocalState(t)
	store := newLocalStateExtenderSeedStore(localState)
	if !strings.HasPrefix(store.path(), localState.localStorageDir) ||
		filepath.Base(store.path()) != extenderSeedStoreFileName {
		t.Errorf("the store path is %s", store.path())
	}
}
