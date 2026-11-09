//go:build !ios_extension && !js

package sdk

// §22.5's §18 row at Go level: the app's key store statuses through the adapter one for one, and every odd answer
// Failed, never Absent; then through the core on a real state directory, which creates a root only for Absent with
// nothing recorded, and creates, overwrites and deletes nothing while locked or failed (D-SDK-15, D-CORE-15).

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// How a test key store answers.
const (
	testKeyStoreWorking = ""
	testKeyStoreLocked  = "locked"
	testKeyStoreFailed  = "failed"
	testKeyStorePanic   = "panic"
	testKeyStoreNil     = "nil"
	testKeyStoreUnknown = "unknown"
	testKeyStoreCorrupt = "corrupt"
)

// An app's key store in memory, answering as its mode says, counting writes.
type testSidecarKeyStore struct {
	stateLock   sync.Mutex
	mode        string
	nameValues  map[string]string
	writeCount  int
	deleteCount int
}

func newTestSidecarKeyStore() *testSidecarKeyStore {
	return &testSidecarKeyStore{nameValues: map[string]string{}}
}

func (self *testSidecarKeyStore) setMode(mode string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mode = mode
}

// Puts value under name behind the store's back, as a damaged medium would.
func (self *testSidecarKeyStore) replace(name string, value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nameValues[name] = value
}

// Forgets every item, as a wiped store would.
func (self *testSidecarKeyStore) lose() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nameValues = map[string]string{}
}

// What it holds and how often it was written.
func (self *testSidecarKeyStore) snapshot() (map[string]string, int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	nameValues := map[string]string{}
	for name, value := range self.nameValues {
		nameValues[name] = value
	}
	return nameValues, self.writeCount, self.deleteCount
}

func (self *testSidecarKeyStore) PutSecret(name string, valueBase64 string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testKeyStoreLocked:
		return SidecarKeyStoreLocked
	case testKeyStoreFailed, testKeyStoreNil, testKeyStoreCorrupt:
		return SidecarKeyStoreFailed
	case testKeyStorePanic:
		panic("synthetic key store panic")
	case testKeyStoreUnknown:
		return 99
	}
	self.writeCount += 1
	self.nameValues[name] = valueBase64
	return SidecarKeyStoreOk
}

func (self *testSidecarKeyStore) GetSecret(name string) *SidecarSecret {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testKeyStoreLocked:
		return &SidecarSecret{Status: SidecarSecretLocked}
	case testKeyStoreFailed:
		return &SidecarSecret{Status: SidecarSecretFailed}
	case testKeyStorePanic:
		panic("synthetic key store panic")
	case testKeyStoreNil:
		return nil
	case testKeyStoreUnknown:
		return &SidecarSecret{Status: 99, ValueBase64: self.nameValues[name]}
	case testKeyStoreCorrupt:
		return &SidecarSecret{Status: SidecarSecretPresent, ValueBase64: "not base64!"}
	}
	value, ok := self.nameValues[name]
	if !ok {
		return &SidecarSecret{Status: SidecarSecretAbsent}
	}
	return &SidecarSecret{Status: SidecarSecretPresent, ValueBase64: value}
}

func (self *testSidecarKeyStore) DeleteSecret(name string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testKeyStoreLocked:
		return SidecarKeyStoreLocked
	case testKeyStoreFailed, testKeyStoreNil, testKeyStoreCorrupt:
		return SidecarKeyStoreFailed
	case testKeyStorePanic:
		panic("synthetic key store panic")
	case testKeyStoreUnknown:
		return 99
	}
	self.deleteCount += 1
	delete(self.nameValues, name)
	return SidecarKeyStoreOk
}

// An app store answering exactly what it is told, for the adapter's table.
type testScriptedKeyStore struct {
	writeStatus int32
	secret      *SidecarSecret
}

func (self *testScriptedKeyStore) PutSecret(name string, valueBase64 string) int32 {
	return self.writeStatus
}

func (self *testScriptedKeyStore) GetSecret(name string) *SidecarSecret {
	return self.secret
}

func (self *testScriptedKeyStore) DeleteSecret(name string) int32 {
	return self.writeStatus
}

// Each status crosses the adapter as the core's same status, the value as bytes, and any status the core does not
// know as Failed.
func TestKeyStoreStatusesThroughAdapter(t *testing.T) {
	writes := map[int32]sidecar.SidecarKeyStoreStatus{
		SidecarKeyStoreOk:     sidecar.SidecarKeyStoreOk,
		SidecarKeyStoreLocked: sidecar.SidecarKeyStoreLocked,
		SidecarKeyStoreFailed: sidecar.SidecarKeyStoreFailed,
		// Present and Absent are no answer to a write, and the rest no answer at all
		SidecarSecretPresent: sidecar.SidecarKeyStoreFailed,
		SidecarSecretAbsent:  sidecar.SidecarKeyStoreFailed,
		-1:                   sidecar.SidecarKeyStoreFailed,
		99:                   sidecar.SidecarKeyStoreFailed,
	}
	for status, want := range writes {
		adapter := newSidecarKeyStoreAdapter(&testScriptedKeyStore{writeStatus: status})
		if got := adapter.PutSecret("name", []byte{1}); got != want {
			t.Errorf("put answering %d is %d, not %d", status, got, want)
		}
		if got := adapter.DeleteSecret("name"); got != want {
			t.Errorf("delete answering %d is %d, not %d", status, got, want)
		}
	}

	value := []byte{0x00, 0x01, 0xfe, 0xff}
	reads := []struct {
		secret     *SidecarSecret
		want       sidecar.SidecarKeyStoreStatus
		wantsValue bool
	}{
		{secret: &SidecarSecret{Status: SidecarSecretPresent, ValueBase64: base64.StdEncoding.EncodeToString(value)}, want: sidecar.SidecarKeyStorePresent, wantsValue: true},
		{secret: &SidecarSecret{Status: SidecarSecretAbsent}, want: sidecar.SidecarKeyStoreAbsent},
		{secret: &SidecarSecret{Status: SidecarSecretLocked}, want: sidecar.SidecarKeyStoreLocked},
		{secret: &SidecarSecret{Status: SidecarSecretFailed}, want: sidecar.SidecarKeyStoreFailed},
		// an Absent with a value is still Absent: nothing is read from it
		{secret: &SidecarSecret{Status: SidecarSecretAbsent, ValueBase64: "AAAA"}, want: sidecar.SidecarKeyStoreAbsent},
		{secret: &SidecarSecret{Status: 1}, want: sidecar.SidecarKeyStoreFailed},
		{secret: &SidecarSecret{Status: 99, ValueBase64: base64.StdEncoding.EncodeToString(value)}, want: sidecar.SidecarKeyStoreFailed},
		{secret: &SidecarSecret{Status: -3}, want: sidecar.SidecarKeyStoreFailed},
	}
	for _, read := range reads {
		adapter := newSidecarKeyStoreAdapter(&testScriptedKeyStore{secret: read.secret})
		got, status := adapter.GetSecret("name")
		if status != read.want {
			t.Errorf("get answering %+v is %d, not %d", read.secret, status, read.want)
		}
		if read.wantsValue != (got != nil) || (read.wantsValue && !bytes.Equal(got, value)) {
			t.Errorf("get answering %+v gave %x", read.secret, got)
		}
	}

	// a value crosses as standard base64 both ways
	keyStore := newTestSidecarKeyStore()
	adapter := newSidecarKeyStoreAdapter(keyStore)
	if status := adapter.PutSecret("name", value); status != sidecar.SidecarKeyStoreOk {
		t.Fatalf("put %d", status)
	}
	if stored, _, _ := keyStore.snapshot(); stored["name"] != base64.StdEncoding.EncodeToString(value) {
		t.Errorf("stored %q", stored["name"])
	}
	if got, status := adapter.GetSecret("name"); status != sidecar.SidecarKeyStorePresent || !bytes.Equal(got, value) {
		t.Errorf("read back %x, %d", got, status)
	}
	if status := adapter.DeleteSecret("name"); status != sidecar.SidecarKeyStoreOk {
		t.Errorf("delete %d", status)
	}
	if _, status := adapter.GetSecret("name"); status != sidecar.SidecarKeyStoreAbsent {
		t.Errorf("after the delete %d", status)
	}
}

// A store that answers no secret at all is Failed, never Absent.
func TestNilSecretIsFailed(t *testing.T) {
	adapter := newSidecarKeyStoreAdapter(&testScriptedKeyStore{secret: nil})
	if value, status := adapter.GetSecret("name"); status != sidecar.SidecarKeyStoreFailed || value != nil {
		t.Errorf("a nil secret read %x, %d", value, status)
	}

	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStoreNil)
	local := world.open(t)
	world.requireNothingCreated(t)
	requireTestInstanceState(t, local, sidecar.SidecarInstanceFailed)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
}

// A store that panics, on any call, is Failed, and the panic stops at the adapter.
func TestPanicInKeyStoreIsFailed(t *testing.T) {
	adapter := newSidecarKeyStoreAdapter(newTestSidecarKeyStoreIn(testKeyStorePanic))
	if status := adapter.PutSecret("name", []byte{1}); status != sidecar.SidecarKeyStoreFailed {
		t.Errorf("put %d", status)
	}
	if value, status := adapter.GetSecret("name"); status != sidecar.SidecarKeyStoreFailed || value != nil {
		t.Errorf("get %x, %d", value, status)
	}
	if status := adapter.DeleteSecret("name"); status != sidecar.SidecarKeyStoreFailed {
		t.Errorf("delete %d", status)
	}

	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStorePanic)
	local := world.open(t)
	world.requireNothingCreated(t)
	requireTestInstanceState(t, local, sidecar.SidecarInstanceFailed)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
}

// A store in the given mode.
func newTestSidecarKeyStoreIn(mode string) *testSidecarKeyStore {
	keyStore := newTestSidecarKeyStore()
	keyStore.mode = mode
	return keyStore
}

// A Present value that is not standard base64, or that the core's AEAD refuses, is Failed: nothing is created or
// overwritten, and the store keeps what it held.
func TestCorruptBytesAreFailed(t *testing.T) {
	for _, encoded := range []string{"not base64!", "QR==", "QQ=", "AAAA AAAA"} {
		adapter := newSidecarKeyStoreAdapter(&testScriptedKeyStore{secret: &SidecarSecret{Status: SidecarSecretPresent, ValueBase64: encoded}})
		if value, status := adapter.GetSecret("name"); status != sidecar.SidecarKeyStoreFailed || value != nil {
			t.Errorf("%q read %x, %d", encoded, value, status)
		}
	}

	world := newTestSidecarWorld(t)
	first := world.open(t)
	awaitTestSidecarState(t, first, sidecar.SidecarStateUnenrolled, "")
	world.close(t, first)
	stored, writeCount, deleteCount := world.keyStore.snapshot()
	before := world.stateFiles(t)

	// bytes that decode, but not to the wrap's key: the core's AEAD refuses them
	for name := range stored {
		world.keyStore.replace(name, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
	}
	second := world.open(t)
	requireTestInstanceState(t, second, sidecar.SidecarInstanceFailed)
	awaitTestSidecarState(t, second, sidecar.SidecarStateLocked, "")
	world.close(t, second)

	// bytes that do not decode
	world.keyStore.setMode(testKeyStoreCorrupt)
	third := world.open(t)
	requireTestInstanceState(t, third, sidecar.SidecarInstanceFailed)
	awaitTestSidecarState(t, third, sidecar.SidecarStateLocked, "")
	world.close(t, third)

	if _, nowWrites, nowDeletes := world.keyStore.snapshot(); nowWrites != writeCount || nowDeletes != deleteCount {
		t.Errorf("a failed value wrote the store: %d writes, %d deletes", nowWrites-writeCount, nowDeletes-deleteCount)
	}
	requireSameTestStateFiles(t, before, world.stateFiles(t))
}

// While the store is locked nothing is created, on the store or on disk; once it unlocks, Unlock("")'s wrap opens
// the instance and creates U then.
func TestNothingCreatedWhileLocked(t *testing.T) {
	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStoreLocked)
	local := world.open(t)
	requireTestInstanceState(t, local, sidecar.SidecarInstanceLocked)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
	world.requireNothingCreated(t)
	if sidecarId := local.GetSidecarId(); sidecarId != nil {
		t.Errorf("a locked sidecar names %s", sidecarId)
	}

	world.keyStore.setMode(testKeyStoreWorking)
	if err := local.Core().Unlock(nil); err != nil {
		t.Fatal(err)
	}
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	if stored, writeCount, _ := world.keyStore.snapshot(); len(stored) != 1 || writeCount != 1 {
		t.Errorf("after the unlock the store holds %d items from %d writes", len(stored), writeCount)
	}
}

// A store that locks and unlocks again opens the same U and the same network: nothing is created, overwritten or
// deleted across the lock, on the store or on disk, and the recorded identity stands.
func TestLockedThenUnlockKeepsIdentity(t *testing.T) {
	world := newTestSidecarWorld(t)
	first := world.open(t)
	awaitTestSidecarState(t, first, sidecar.SidecarStateUnenrolled, "")
	record := world.recordNetwork(t, first)
	world.close(t, first)
	stored, writeCount, deleteCount := world.keyStore.snapshot()
	before := world.stateFiles(t)

	world.keyStore.setMode(testKeyStoreLocked)
	second := world.open(t)
	awaitTestSidecarState(t, second, sidecar.SidecarStateLocked, "")
	requireSameTestStateFiles(t, before, world.stateFiles(t))

	world.keyStore.setMode(testKeyStoreWorking)
	// the network's keychain was never made, so the open lands at keystore_lost, as a lost keychain.wrap does; what
	// matters here is that the unlock opened the same U and changed nothing
	if err := second.Core().Unlock(nil); err != nil {
		t.Fatal(err)
	}
	awaitTestSidecarStateMoved(t, second, sidecar.SidecarStateLocked)
	nowStored, nowWrites, nowDeletes := world.keyStore.snapshot()
	if nowWrites != writeCount || nowDeletes != deleteCount || !mapsEqual(stored, nowStored) {
		t.Errorf("the lock changed the store: %d writes, %d deletes", nowWrites-writeCount, nowDeletes-deleteCount)
	}
	requireSameTestStateFiles(t, before, world.stateFiles(t))
	if records := second.instance.Networks(); len(records) != 1 || records[0] != record {
		t.Errorf("the recorded network is now %+v", records)
	}
}

// §5.9's Absent for a network with a recorded sidecar_id creates nothing: no new U, no store write, no file, and the
// sidecar is Unenrolled(keystore_lost).
func TestAbsentWithRecordedIdMakesNoRoot(t *testing.T) {
	world := newTestSidecarWorld(t)
	first := world.open(t)
	awaitTestSidecarState(t, first, sidecar.SidecarStateUnenrolled, "")
	world.recordNetwork(t, first)
	world.close(t, first)
	world.keyStore.lose()
	_, writeCount, _ := world.keyStore.snapshot()
	before := world.stateFiles(t)

	second := world.open(t)
	awaitTestSidecarState(t, second, sidecar.SidecarStateUnenrolled, sidecar.SidecarReasonKeystoreLost)
	if stored, nowWrites, _ := world.keyStore.snapshot(); len(stored) != 0 || nowWrites != writeCount {
		t.Errorf("a loss with an identity recorded wrote the store: %d items, %d writes", len(stored), nowWrites-writeCount)
	}
	requireSameTestStateFiles(t, before, world.stateFiles(t))

	// with nothing recorded, Absent is a first run and makes U
	fresh := newTestSidecarWorld(t)
	local := fresh.open(t)
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	if stored, nowWrites, _ := fresh.keyStore.snapshot(); len(stored) != 1 || nowWrites != 1 {
		t.Errorf("a first run wrote %d items in %d writes", len(stored), nowWrites)
	}
}

// Every file under a state directory, by its path, with its bytes.
func (self *testSidecarWorld) stateFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(self.stateDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(self.stateDir, path)
		files[relative] = content
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return files
}

// Fails unless the two listings hold the same files with the same bytes. A sweep's tmp directory is not a file.
func requireSameTestStateFiles(t *testing.T, before map[string][]byte, after map[string][]byte) {
	t.Helper()
	names := func(files map[string][]byte) []string {
		listed := []string{}
		for name := range files {
			listed = append(listed, name)
		}
		sort.Strings(listed)
		return listed
	}
	if !slices.Equal(names(before), names(after)) {
		t.Fatalf("the state files moved from %v to %v", names(before), names(after))
	}
	for name, content := range before {
		if !bytes.Equal(content, after[name]) {
			t.Errorf("%s changed", name)
		}
	}
}

// Whether two string maps hold the same pairs.
func mapsEqual(a map[string]string, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if other, ok := b[name]; !ok || other != value {
			return false
		}
	}
	return true
}
