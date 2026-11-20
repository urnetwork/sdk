// Key retention is exercised through real files and process termination,
// including partial destructive failure after owners have been retired.
package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPairedResetKeyReadFailureNeverDeletesState(t *testing.T) {
	for _, failure := range []string{"malformed", "directory", "symlink", "broken-symlink"} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		fixture.startLocal(t)
		testingSeedPreservedState(t, fixture)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		path := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
		if err := os.Remove(path); err != nil {
			t.Fatal("could not prepare key failure fixture")
		}
		switch failure {
		case "malformed":
			if err := os.WriteFile(path, []byte(`{"client_key_seed":"private-marker`), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not write corrupt key fixture")
			}
		case "directory":
			if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
				t.Fatal("could not write directory fixture")
			}
		case "symlink", "broken-symlink":
			target := filepath.Join(t.TempDir(), "target")
			if failure == "symlink" {
				if err := os.WriteFile(target, []byte(`{}`), LocalStorageFilePermissions); err != nil {
					t.Fatal("could not write symlink target")
				}
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal("could not write symlink fixture")
			}
		}
		before, err := fixture.localState.loadAuthState()
		if err != nil {
			t.Fatal("could not read retained auth")
		}
		result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
		if err == nil || result != nil {
			t.Fatal("failed key observation authorized cleanup")
		}
		testingRequireAuthUnchanged(t, fixture, before, fixture.initialJwt)
		if location, err := fixture.localState.LoadConnectLocation(); err != nil || location == nil {
			t.Fatal("failed key read destroyed saved destination")
		}
		if fixture.localState.deviceAuthOwner != snapshot.localOwner || !fixture.api.deviceOwnsAuth(snapshot.localOwner) {
			t.Fatal("non-destructive key failure retired the current owner")
		}
	}
}

func TestPairedResetAuthReadFailureNeverDeletesKeys(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	original := testingSeedPreservedState(t, fixture)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	if err := os.WriteFile(fixture.localState.authStatePath(), []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not interrupt auth envelope")
	}
	if result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot); err == nil || result != nil {
		t.Fatal("failed current-auth read authorized cleanup")
	}
	after, err := os.ReadFile(filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material"))
	if err != nil || !bytes.Equal(original, after) || fixture.api.GetByJwt() != fixture.initialJwt {
		t.Fatal("auth read error changed keys or the API owner")
	}
}

// Serialization covers the public key setter, not just a private reset helper.
// The reset result remains the original material even after a later real save.
func TestPairedResetResultRetainsActualKeysAcrossSerializedPublicSave(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	testingSeedPreservedState(t, fixture)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.localState.testingAfterPairedReset = func() { close(entered); <-release }
	done := make(chan struct{})
	var result *LocalStateResetResult
	var resetErr error
	go func() { defer close(done); result, resetErr = fixture.networkSpace.ResetLocalStateIfCurrent(snapshot) }()
	testingAwaitAuthBoundary(t, entered)
	if fixture.localState.authStateLock.TryLock() {
		fixture.localState.authStateLock.Unlock()
		t.Fatal("reset released key serialization before selecting its result")
	}
	if !fixture.api.mutex.TryLock() {
		t.Fatal("destructive storage work held the short API mutex")
	}
	fixture.api.mutex.Unlock()
	saveDone := make(chan struct{})
	var saveErr error
	go func() {
		defer close(saveDone)
		saveErr = fixture.localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial([]byte{9}, nil, nil))
	}()
	resume()
	testingAwaitAuthBoundary(t, done)
	testingAwaitAuthBoundary(t, saveDone)
	if resetErr != nil || saveErr != nil || result == nil || !result.GetReset() {
		t.Fatal("serialized reset and public key save did not finish")
	}
	if keys := result.GetDeviceLocalKeyMaterial(); keys == nil || !bytes.Equal(keys.GetClientKeySeed(), []byte{1, 2, 3}) {
		t.Fatal("reset result used a separate post-reset reread")
	}
	if keys, err := fixture.localState.LoadDeviceLocalKeyMaterial(); err != nil || keys == nil || !bytes.Equal(keys.GetClientKeySeed(), []byte{9}) {
		t.Fatal("later public key save did not commit after reset")
	}
}

// Make the exact parent a non-directory after one actual removal. Subsequent
// removal fails deterministically even for root, without permission tricks.
func TestPairedResetPartialRemovalRetiresOwnersAndPreservesKeys(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	original := testingSeedPreservedState(t, fixture)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	path := fixture.localState.localStorageDir
	held := path + ".held-for-partial-reset"
	interrupted := false
	fixture.localState.testingAfterPairedResetRemove = func(name string) {
		if name != localConnectLocationFileName {
			return
		}
		if err := os.Rename(path, held); err != nil {
			t.Fatal("could not move store at removal boundary")
		}
		if err := os.WriteFile(path, []byte{1}, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not install non-directory boundary")
		}
		interrupted = true
	}
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if !interrupted {
		t.Fatal("reset never reached the real removal boundary")
	}
	if removeErr := os.Remove(path); removeErr != nil {
		t.Fatal("could not remove boundary fixture")
	}
	if restoreErr := os.Rename(held, path); restoreErr != nil {
		t.Fatal("could not restore store for cold observation")
	}
	if err == nil || result != nil {
		t.Fatal("partially failed cleanup was reported as a completed reset")
	}
	if fixture.api.GetByJwt() != "" || fixture.api.deviceOwnsAuth(snapshot.apiState.owner) || fixture.localState.deviceAuthOwner != nil {
		t.Fatal("partial destructive failure left an admitted auth owner")
	}
	fixture.api.mutex.Lock()
	retainedBinding := fixture.api.httpPostRawOwner == snapshot.apiState.owner || fixture.api.httpGetRawOwner == snapshot.apiState.owner
	fixture.api.mutex.Unlock()
	if retainedBinding {
		t.Fatal("partial reset retained remote-owned request bindings")
	}
	fresh := newLocalState(context.Background(), filepath.Dir(path))
	t.Cleanup(fresh.Close)
	after, readErr := os.ReadFile(filepath.Join(path, ".device_local_key_material"))
	if readErr != nil || !bytes.Equal(original, after) {
		t.Fatal("partial failure lost original key bytes")
	}
	if keys, readErr := fresh.LoadDeviceLocalKeyMaterial(); readErr != nil || keys == nil {
		t.Fatal("cold key read failed after partial removal")
	}
	if location, readErr := fresh.LoadConnectLocation(); readErr != nil || location != nil {
		t.Fatal("partial fixture did not remove its first routing record")
	}
	if fresh.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("partial reset erased auth before clearing all old routing")
	}
	// A real delayed old-device publication cannot repopulate either owner.
	fixture.capturedRefresh(testingRefreshableJwtWithMarker(t, "late-after-partial"))
	if fixture.api.GetByJwt() != "" || fresh.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("retired callback published after partial destructive failure")
	}
}

func TestPairedResetInterruptedChild(t *testing.T) {
	home := os.Getenv("SDK_RESET_CRASH_HOME")
	if home == "" {
		return
	}
	fixture := testingPairedAuthSpaceAt(t, home)
	fixture.localState.testingAfterPairedResetRemove = func(name string) {
		if name == localConnectLocationFileName {
			os.Exit(73)
		}
	}
	_, _ = fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	t.Fatal("reset did not reach the intended process interruption")
}

func TestInterruptedPairedResetPreservesColdKeysAndAccountBoundary(t *testing.T) {
	home := t.TempDir()
	fixture := testingPairedAuthSpaceAt(t, home)
	fixture.seedDistinctLogin(t)
	original := testingSeedPreservedState(t, fixture)
	testingLocationCrashChild(t, "TestPairedResetInterruptedChild", "SDK_RESET_CRASH_HOME="+home)
	fresh := newLocalState(context.Background(), home)
	t.Cleanup(fresh.Close)
	after, err := os.ReadFile(filepath.Join(fresh.localStorageDir, ".device_local_key_material"))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("process interruption deleted or rewrote identity")
	}
	if keys, err := fresh.LoadDeviceLocalKeyMaterial(); err != nil || keys == nil {
		t.Fatal("fresh process cannot restore original keys")
	}
	if fresh.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("interruption erased account boundary ahead of routing cleanup")
	}
	if location, err := fresh.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("process did not interrupt after the expected real removal")
	}
}

// Explicit user logout retains its old full-removal semantics. Conditional
// preservation is not silently applied to this different user action.
func TestExplicitLogoutStillRemovesKeysAndRouting(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	testingSeedPreservedState(t, fixture)
	if err := fixture.localState.Logout(); err != nil {
		t.Fatal("explicit logout failed")
	}
	if keys, err := fixture.localState.LoadDeviceLocalKeyMaterial(); err != nil || keys != nil {
		t.Fatal("explicit logout began preserving keys")
	}
	if location, err := fixture.localState.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("explicit logout retained routing")
	}
}
