// Storage controls interrupt the real writer in a child process, then reopen
// a fresh LocalState. No wall-clock sleep is evidence of an ordering.
package sdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Named locations are synthetic; the optional peer flag exercises a value
// that must not silently become best-available during read failure.
func testingStoredLocation(name string) *ConnectLocation {
	return &ConnectLocation{
		Name:              name,
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
}

// Both independent records share the same persistence implementation.
func testingLocationAccess(state *LocalState, name string) (
	func(*ConnectLocation) error, func() (*ConnectLocation, error), func() *ConnectLocation,
) {
	if name == localConnectLocationFileName {
		return state.SetConnectLocation, state.LoadConnectLocation, state.GetConnectLocation
	}
	return state.SetDefaultLocation, state.LoadDefaultLocation, state.GetDefaultLocation
}

// A true absence remains distinguishable without creating or repairing state.
func TestCheckedLocationAbsentAndExplicitDisconnect(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		state := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(state.Close)
		set, load, _ := testingLocationAccess(state, name)
		if location, err := load(); err != nil || location != nil {
			t.Fatalf("%s: missing location was not absent", name)
		}
		if err := set(testingStoredLocation("saved")); err != nil {
			t.Fatal("could not persist location")
		}
		if err := set(nil); err != nil {
			t.Fatal("explicit removal failed")
		}
		if err := set(nil); err != nil {
			t.Fatal("repeated explicit removal failed")
		}
		fresh := newLocalState(context.Background(), filepath.Dir(state.localStorageDir))
		t.Cleanup(fresh.Close)
		_, freshLoad, _ := testingLocationAccess(fresh, name)
		if location, err := freshLoad(); err != nil || location != nil {
			t.Fatalf("%s: cold restore revived an explicit disconnect", name)
		}
	}
}

// Decoder failures return no partially decoded value and expose no contents.
func TestCheckedLocationMalformedIsNotAbsent(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		for _, data := range []string{"", "null", "{}", "[]", `{"name":"private-marker","connect_location_id":`, `{"connect_location_id":{"best_available":"private-marker"}}`} {
			state := newLocalState(context.Background(), t.TempDir())
			t.Cleanup(state.Close)
			path := filepath.Join(state.localStorageDir, name)
			if err := os.WriteFile(path, []byte(data), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not write malformed fixture")
			}
			_, load, _ := testingLocationAccess(state, name)
			location, err := load()
			if err == nil || location != nil || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("%s: malformed location was absent, partial, or exposed contents", name)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(after, []byte(data)) {
				t.Fatal("checked read changed malformed location")
			}
		}
	}
}

// Legacy successful getters retain their behavior; valid unknown fields and
// readable symlinks are accepted by the additive checked loader too.
func TestCheckedLocationHealthyCompatibility(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		state := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(state.Close)
		path := filepath.Join(state.localStorageDir, name)
		data := []byte(`{"name":"retained","connect_location_id":{"best_available":true},"future":1}`)
		if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not write compatible record")
		}
		_, load, get := testingLocationAccess(state, name)
		location, err := load()
		if err != nil || location == nil || !connectLocationValuesEqual(location, get()) {
			t.Fatal("checked location disagrees with healthy compatibility getter")
		}
		location.Name = "mutated-return"
		if get().Name != "retained" {
			t.Fatal("returned location mutated storage")
		}
		target := filepath.Join(t.TempDir(), "saved-location")
		if err := os.Rename(path, target); err != nil {
			t.Fatal("could not move symlink target")
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal("could not create readable symlink fixture")
		}
		if location, err := load(); err != nil || location == nil || location.Name != "retained" {
			t.Fatal("checked read rejected a readable compatible symlink")
		}
	}
}

// Deterministic filesystem failures do not depend on the test runner's uid.
func TestCheckedLocationTypeAndStorageFailures(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		state := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(state.Close)
		path := filepath.Join(state.localStorageDir, name)
		_, load, _ := testingLocationAccess(state, name)
		if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
			t.Fatal("could not install directory fixture")
		}
		if location, err := load(); err == nil || location != nil {
			t.Fatal("non-file location was treated as absent")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal("could not remove directory fixture")
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
			t.Fatal("could not install broken symlink fixture")
		}
		if location, err := load(); err == nil || location != nil {
			t.Fatal("broken symlink was treated as absent")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal("could not remove symlink fixture")
		}
		if err := os.Rename(state.localStorageDir, state.localStorageDir+".held"); err != nil {
			t.Fatal("could not make store unavailable")
		}
		if location, err := load(); err == nil || location != nil {
			t.Fatal("missing parent directory was treated as an absent leaf")
		}
	}
}

// A failed explicit removal is visible instead of claiming disconnection.
func TestCheckedLocationDisconnectReportsRemovalFailure(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	path := filepath.Join(state.localStorageDir, localConnectLocationFileName)
	if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal("could not create refusal fixture")
	}
	if err := os.WriteFile(filepath.Join(path, "retained"), []byte{1}, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not populate refusal fixture")
	}
	if err := state.SetConnectLocation(nil); err == nil {
		t.Fatal("failed removal was presented as a completed disconnect")
	}
}

func TestCheckedLocationDisconnectReportsUnavailableParent(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		state := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(state.Close)
		set, _, _ := testingLocationAccess(state, name)
		if err := os.Rename(state.localStorageDir, state.localStorageDir+".unavailable"); err != nil {
			t.Fatal("could not make store unavailable")
		}
		if err := set(nil); err == nil {
			t.Fatal("missing parent was reported as a durable disconnect")
		}
	}
}

// An empty selector is decodable legacy state, not authority to synthesize a
// best-available choice. Selection policy remains with the native owner.
func TestCheckedLocationRetainsEmptySelectorWithoutChoosingDefault(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	if err := state.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{}}); err != nil {
		t.Fatal("could not save legacy empty selector")
	}
	location, err := state.LoadConnectLocation()
	if err != nil || location == nil || location.ConnectLocationId == nil ||
		location.ConnectLocationId.BestAvailable || location.ConnectLocationId.ClientId != nil ||
		location.ConnectLocationId.LocationId != nil || location.ConnectLocationId.LocationGroupId != nil {
		t.Fatal("checked loader reinterpreted a legacy empty selector")
	}
}

// This child is inert during ordinary discovery. The parent selects the
// exact writer and directory; Exit terminates before deferred cleanup/rename.
func TestLocationInterruptedWriteChild(t *testing.T) {
	name := os.Getenv("SDK_LOCATION_CRASH_RECORD")
	if name == "" {
		return
	}
	if name != localConnectLocationFileName && name != localDefaultLocationFileName {
		t.Fatal("unsupported interrupted-write record")
	}
	state := newLocalState(context.Background(), os.Getenv("SDK_LOCATION_CRASH_HOME"))
	state.testingBeforeLocationCommit = func(actual string) error {
		if actual != name {
			t.Fatal("writer reached a different commit boundary")
		}
		os.Exit(73)
		return nil
	}
	set, _, _ := testingLocationAccess(state, name)
	_ = set(testingStoredLocation("interrupted-replacement"))
	t.Fatal("writer did not reach the interruption boundary")
}

// Runs a test-only child with a hard deadlock bound and a required exit code.
func testingLocationCrashChild(t *testing.T, testName string, variables ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$", "-test.count=1")
	command.Env = append(os.Environ(), variables...)
	err := command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 73 {
		t.Fatal("child did not terminate at the intended storage boundary")
	}
}

// The original complete bytes must survive even when defers never execute.
func testingInterruptedLocationWrite(t *testing.T, name string) {
	t.Helper()
	home := t.TempDir()
	state := newLocalState(context.Background(), home)
	set, _, _ := testingLocationAccess(state, name)
	if err := set(testingStoredLocation("original-destination")); err != nil {
		t.Fatal("could not save original destination")
	}
	path := filepath.Join(state.localStorageDir, name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read original destination")
	}
	state.Close()
	testingLocationCrashChild(t, "TestLocationInterruptedWriteChild",
		"SDK_LOCATION_CRASH_RECORD="+name, "SDK_LOCATION_CRASH_HOME="+home)
	fresh := newLocalState(context.Background(), home)
	t.Cleanup(fresh.Close)
	_, load, _ := testingLocationAccess(fresh, name)
	location, err := load()
	if err != nil || location == nil || location.Name != "original-destination" {
		t.Fatal("cold restore lost the last committed destination after interrupted write")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("interrupted replacement altered the committed destination bytes")
	}
}

func TestInterruptedConnectLocationWritePreservesColdRestore(t *testing.T) {
	testingInterruptedLocationWrite(t, localConnectLocationFileName)
}

func TestInterruptedDefaultLocationWritePreservesColdRestore(t *testing.T) {
	testingInterruptedLocationWrite(t, localDefaultLocationFileName)
}

// An ordinary staged-write failure also preserves the original and reports
// failure; the abrupt-exit controls above cover loss of deferred cleanup.
func TestLocationCommitFailurePreservesExistingDestination(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	if err := state.SetConnectLocation(testingStoredLocation("original")); err != nil {
		t.Fatal("could not save original")
	}
	state.testingBeforeLocationCommit = func(string) error { return errors.New("controlled interruption") }
	if err := state.SetConnectLocation(testingStoredLocation("replacement")); err == nil {
		t.Fatal("interrupted commit was accepted")
	}
	location, err := state.LoadConnectLocation()
	if err != nil || location == nil || location.Name != "original" {
		t.Fatal("failed commit changed the saved destination")
	}
}

func TestCheckedStorageErrorsExposeStageWithoutPrivatePath(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	state.testingBeforeLocationCommit = func(string) error {
		return &os.PathError{Op: "rename", Path: "/private-marker/secret-home", Err: os.ErrPermission}
	}
	err := state.SetConnectLocation(testingStoredLocation("private-marker-destination"))
	if err == nil || err.Error() != "commit saved location" || !errors.Is(err, os.ErrPermission) {
		t.Fatal("checked writer leaked a path or lost its underlying error class")
	}
	if err := os.Rename(state.localStorageDir, state.localStorageDir+".unavailable"); err != nil {
		t.Fatal("could not prepare unavailable key store")
	}
	keys, err := state.LoadDeviceLocalKeyMaterial()
	if keys != nil || err == nil || err.Error() != "inspect device key directory" || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checked key error exposed a path or lost its cause")
	}
}
