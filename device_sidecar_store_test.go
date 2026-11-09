// The device role's posture store (device_sidecar_store.go): where it is, that absent means only never written, that
// a store it cannot read restores as pending and never legacy, and that records stream rather than sit whole.
package sdk

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
)

// Bytes allocated by everything since the process started; a sum, so a collection cannot lower it.
func testingTotalAllocByteCount() uint64 {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return memStats.TotalAlloc
}

// Absent is only a record never written; a store that exists and cannot be read is a failure, never absent, and a
// device over one restores pending with an alarm, holding its bridge, never legacy: a garbled record, a directory it
// may not read, and a file where the directory should be.
func TestCorruptStoreIsNotAbsent(t *testing.T) {
	role := newTestingDeviceRole(t)
	fresh, err := newSdkPostureStore(t.TempDir(), role.vectors.networkId())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Open("posture"); !errors.Is(err, sidecardevice.ErrPostureStoreAbsent) {
		t.Fatalf("a store never written gave %v, want absent", err)
	}
	if _, err := os.Stat(fresh.directory()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening a store made its directory")
	}

	enforcing := role.device(t, nil)
	role.enforce(t, enforcing, testingBridgeRules())
	testingJoinDeviceRole(t, enforcing)
	restored := func(why string) {
		t.Helper()
		device := role.device(t, nil)
		status, err := device.bridgeStatus()
		if err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		if status.State != sidecardevice.PosturePending || len(status.PostureAlarms) == 0 || device.sendRoute.Load().bridge == nil {
			t.Fatalf("%s: restored %v with alarms %v, held %v; want pending, alarmed, held", why, status.State,
				status.PostureAlarms, device.sendRoute.Load().bridge != nil)
		}
		testingJoinDeviceRole(t, device)
	}

	// every record garbled
	records := role.postureRecords(t)
	if len(records) == 0 {
		t.Fatal("CONTROL FAILED: the enforcing device stored nothing")
	}
	for name := range records {
		if err := os.WriteFile(filepath.Join(role.postureDirectory(), name), []byte("not a posture record"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restored("garbled records")

	// a directory this process may not read
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		if err := os.Chmod(role.postureDirectory(), 0o000); err != nil {
			t.Fatal(err)
		}
		restored("an unreadable directory")
		if err := os.Chmod(role.postureDirectory(), 0o700); err != nil {
			t.Fatal(err)
		}
		store, err := newSdkPostureStore(role.home, role.vectors.networkId())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(role.postureDirectory(), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Open("posture"); errors.Is(err, sidecardevice.ErrPostureStoreAbsent) || !errors.Is(err, sidecardevice.ErrPostureStoreFailed) {
			t.Fatalf("an unreadable record gave %v, want a failure", err)
		}
		if err := os.Chmod(role.postureDirectory(), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// a file where the network's directory should be
	if err := os.RemoveAll(role.postureDirectory()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(role.postureDirectory(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newSdkPostureStore(role.home, role.vectors.networkId())
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		func() error { _, err := store.Open("posture"); return err }(),
		func() error { _, err := store.List(); return err }(),
		store.Write("posture", func(writer io.Writer) error { return nil }),
	} {
		if errors.Is(err, sidecardevice.ErrPostureStoreAbsent) || !errors.Is(err, sidecardevice.ErrPostureStoreFailed) {
			t.Fatalf("a store over a file gave %v, want a failure", err)
		}
	}
	restored("a file in the directory's place")
}

// Records stream both ways: writing and reading 4 MiB, a base's rows at §8.8's bound, in 4 KiB pieces allocates less
// than half the record, so the tunnel never holds it whole (R-SDK-6). The bound is half, not a few pieces, since the
// count is the process's and another test's goroutines may still be winding down.
func TestRowsStreamed(t *testing.T) {
	store, err := newSdkPostureStore(t.TempDir(), connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	const recordByteCount = 4 * 1024 * 1024
	const boundByteCount = recordByteCount / 2
	piece := make([]byte, 4*1024)
	for i := range piece {
		piece[i] = byte(i)
	}
	before := testingTotalAllocByteCount()
	err = store.Write("rows-1", func(writer io.Writer) error {
		for written := 0; written < recordByteCount; written += len(piece) {
			if _, err := writer.Write(piece); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocated := testingTotalAllocByteCount() - before; boundByteCount < allocated {
		t.Fatalf("writing %d B allocated %d B", recordByteCount, allocated)
	}
	before = testingTotalAllocByteCount()
	reader, err := store.Open("rows-1")
	if err != nil {
		t.Fatal(err)
	}
	readByteCount := 0
	for {
		n, err := reader.Read(piece)
		readByteCount += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	reader.Close()
	if allocated := testingTotalAllocByteCount() - before; boundByteCount < allocated {
		t.Fatalf("reading %d B allocated %d B", recordByteCount, allocated)
	}
	connect.AssertEqual(t, readByteCount, recordByteCount)
}

// The posture lives under the network space's storage, beside LocalState's .by and never in it, and never in the
// sidecar role's state directory: the records land where the store says, nothing lands in a sidecar StateDir beside
// it, and a sign-out, which empties .by, keeps them.
func TestStoreOutsideStateDir(t *testing.T) {
	role := newTestingDeviceRole(t)
	// what an app would give sdk.Sidecar as its StateDir on the same host
	stateDir := filepath.Join(t.TempDir(), "sidecar")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	device := role.device(t, nil)
	if err := device.setBridgePending(role.vectors.pending); err != nil {
		t.Fatal(err)
	}
	store, ok := device.bridgeOwner.store.(*sdkPostureStore)
	if !ok {
		t.Fatal("the device role's store is not the sdk's")
	}
	connect.AssertEqual(t, store.directory(), role.postureDirectory())
	localStorageDir := role.space.asyncLocalState.localState.localStorageDir
	for _, outside := range []string{stateDir, localStorageDir} {
		relative, err := filepath.Rel(outside, store.directory())
		if err != nil || !strings.HasPrefix(relative, "..") {
			t.Fatalf("the posture is under %s", outside)
		}
	}
	if len(role.postureRecords(t)) == 0 {
		t.Fatal("the Pending stored nothing under the network space's storage")
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the device role wrote %d entries in the sidecar role's state directory (%v)", len(entries), err)
	}
	testingJoinDeviceRole(t, device)
	stored := role.postureRecords(t)
	if err := role.space.asyncLocalState.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if len(role.postureRecords(t)) != len(stored) {
		t.Fatal("a sign-out took the posture with .by")
	}
}
