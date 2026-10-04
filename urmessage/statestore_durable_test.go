package urmessage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
)

// The values every case below round-trips. They are spelled once so that a case comparing against
// a literal at the point of comparison cannot pass with one of two spellings wrong.
var (
	testGroupId  = bytes.Repeat([]byte{0x71}, GroupIdBytes)
	testGroupId2 = bytes.Repeat([]byte{0x27}, GroupIdBytes)
	testState    = []byte("an epoch state: this stands in for a leaf private key, a path secret ladder and a restore secret")
	testPub      = bytes.Repeat([]byte{0xA1}, 32)
	testPriv     = []byte("the private half of a leaf hpke key")
	testRef      = bytes.Repeat([]byte{0xB2}, 32)
	testKp       = []byte("an encoded key package")
	testInit     = []byte("the init private key")
	testEnc      = []byte("the encryption private key")

	// The device identity's fourth part. It is a REAL length -- messagegroup.XwingSeedSize --
	// because PutDeviceIdentity refuses every other one, which is the whole of what that check
	// is for.
	testWrapSeed = bytes.Repeat([]byte{0x3D}, messagegroup.XwingSeedSize)
)

func openTestStore(t *testing.T, dir string) *DurableStateStore {
	t.Helper()
	store, err := OpenDurableStateStore(dir)
	if err != nil {
		t.Fatalf("OpenDurableStateStore(%s): %v", dir, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// THE STORE'S OWN HALF OF S2-14: EVERY VALUE IS THERE AFTER THE PROCESS THAT WROTE IT IS GONE.
//
// One store writes each of the five kinds, is CLOSED -- which releases the exclusion, so the
// second open has to acquire it again and therefore cannot be the same object -- and a second
// store over the same directory answers every one of them.
//
// It is five kinds and not one because they have five different shapes: a group state is keyed by
// a pair, a private key and a key package by one opaque id, the identity by nothing, and a group
// record by a group id. A store that persisted only the shape somebody remembered would pass a
// case that checked only that shape.
func TestEveryValueADurableStoreHoldsIsThereAfterItIsClosedAndReopened(t *testing.T) {
	dir := t.TempDir()
	first := openTestStore(t, dir)
	if err := first.PutGroupState(testGroupId, 7, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	if err := first.PutPrivateKey(testPub, testPriv); err != nil {
		t.Fatalf("PutPrivateKey: %v", err)
	}
	if err := first.PutKeyPackage(testRef, testKp, testInit, testEnc); err != nil {
		t.Fatalf("PutKeyPackage: %v", err)
	}
	if err := first.PutDeviceIdentity(testPub, testPriv, []byte("a leaf keys body"), testWrapSeed); err != nil {
		t.Fatalf("PutDeviceIdentity: %v", err)
	}
	if err := first.PutGroupRecord(&GroupRecord{
		GroupId:        testGroupId,
		PqSecret:       bytes.Repeat([]byte{0x2A}, 32),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
		Epoch:          7,
		Opened:         true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openTestStore(t, dir)
	state, err := second.GetGroupState(testGroupId, 7)
	if err != nil {
		t.Fatalf("GetGroupState after a reopen: %v", err)
	}
	if !bytes.Equal(state, testState) {
		t.Errorf("the group state came back as %q", state)
	}
	priv, err := second.GetPrivateKey(testPub)
	if err != nil {
		t.Fatalf("GetPrivateKey after a reopen: %v", err)
	}
	if !bytes.Equal(priv, testPriv) {
		t.Errorf("the private key came back as %q", priv)
	}
	kp, init, enc, err := second.TakeKeyPackage(testRef)
	if err != nil {
		t.Fatalf("TakeKeyPackage after a reopen: %v", err)
	}
	if !bytes.Equal(kp, testKp) || !bytes.Equal(init, testInit) || !bytes.Equal(enc, testEnc) {
		t.Errorf("the key package came back as %q / %q / %q", kp, init, enc)
	}
	pub, signPriv, leafKeys, wrapSeed, err := second.GetDeviceIdentity()
	if err != nil {
		t.Fatalf("GetDeviceIdentity after a reopen: %v", err)
	}
	if !bytes.Equal(pub, testPub) || !bytes.Equal(signPriv, testPriv) || string(leafKeys) != "a leaf keys body" {
		t.Errorf("the identity came back as %x / %q / %q", pub, signPriv, leafKeys)
	}
	if !bytes.Equal(wrapSeed, testWrapSeed) {
		t.Errorf("the identity's x-wing seed came back as %x, want %x", wrapSeed, testWrapSeed)
	}
	records, err := second.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords after a reopen: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("one group record was written and %d came back", len(records))
	}
	if !bytes.Equal(records[0].GroupId, testGroupId) || records[0].Epoch != 7 || !records[0].Opened {
		t.Errorf("the group record came back as %+v", records[0])
	}
}

// A FRESH DIRECTORY HOLDS NO IDENTITY AND SAYS SO BY NAME.
//
// [NewDevice] branches on exactly this value: [ErrNoDeviceIdentity] means mint, and anything else
// means the disk would not answer and must not be replaced. A store that returned a nil error and
// four nil slices would make every restart a silent new device.
func TestAFreshDirectoryHoldsNoIdentityAndNoGroups(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	_, _, _, _, err := store.GetDeviceIdentity()
	if !errors.Is(err, ErrNoDeviceIdentity) {
		t.Errorf("a fresh store answered %v for its identity, want ErrNoDeviceIdentity", err)
	}
	records, err := store.GroupRecords()
	if err != nil {
		t.Errorf("a fresh store answered %v for its groups", err)
	}
	if len(records) != 0 {
		t.Errorf("a fresh store holds %d group record(s)", len(records))
	}
	if _, err := store.GetGroupState(testGroupId, 0); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("a fresh store answered %v for a group state, want ErrStateNotFound", err)
	}
}

// TWO STORES OVER ONE DIRECTORY IS TWO DEVICES WRITING ONE DEVICE'S MLS STATE.
//
// The refusal is the operating system's and not a map in this process: the second open is refused
// while the first is live, and it SUCCEEDS once the first has closed -- which is the clause that
// says the hold is released rather than that the refusal is unconditional.
func TestASecondDurableStoreOverOneDirectoryIsRefusedUntilTheFirstCloses(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenDurableStateStore(dir)
	if err != nil {
		t.Fatalf("the first open: %v", err)
	}
	second, err := OpenDurableStateStore(dir)
	if err == nil {
		second.Close()
		first.Close()
		t.Fatal("a second store opened the same directory while the first held it")
	}
	if !errors.Is(err, ErrStateStoreLocked) {
		first.Close()
		t.Fatalf("the second open answered %v, want ErrStateStoreLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first: %v", err)
	}
	third, err := OpenDurableStateStore(dir)
	if err != nil {
		t.Fatalf("a store could not open the directory after the holder closed: %v", err)
	}
	third.Close()
}

// DeleteGroupStateBefore ACTUALLY DELETES, MEASURED ON THE DISK AND NOT THROUGH THE API.
//
// §5.12 discards a whole epoch's worth of material at once and the hazard it names is the HALF
// erase -- a surviving half that looks exactly like a value somebody may use. So this case reads
// the DIRECTORY afterwards: a Get that answers "not found" over a file that is still there would
// pass an API-only case and leave the epoch's leaf key and path-secret ladder on the disk.
//
// AND THE EPOCHS AT AND ABOVE THE CUTOFF SURVIVE, which is the other half of "actually": a
// discard that removed everything would also pass a case that only checked the removed ones, and
// would delete the epoch the device is running on.
func TestDeleteGroupStateBeforeRemovesTheDiscardedEpochsFromTheDiskAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	for epoch := uint64(0); epoch < 6; epoch += 1 {
		if err := store.PutGroupState(testGroupId, epoch, []byte(fmt.Sprintf("epoch %d", epoch))); err != nil {
			t.Fatalf("PutGroupState %d: %v", epoch, err)
		}
	}
	// the control on the control: all six ARE on the disk before the discard
	if names := epochNamesOnDisk(t, store, testGroupId); len(names) != 6 {
		t.Fatalf("six epochs were written and the disk holds %d: %v", len(names), names)
	}
	if err := store.DeleteGroupStateBefore(testGroupId, 3); err != nil {
		t.Fatalf("DeleteGroupStateBefore: %v", err)
	}
	names := epochNamesOnDisk(t, store, testGroupId)
	if len(names) != 3 {
		t.Fatalf("three epochs were discarded and the disk holds %d: %v", len(names), names)
	}
	for epoch := uint64(0); epoch < 3; epoch += 1 {
		if _, err := store.GetGroupState(testGroupId, epoch); !errors.Is(err, ErrStateNotFound) {
			t.Errorf("epoch %d answered %v after it was discarded, want ErrStateNotFound", epoch, err)
		}
		if _, held := names[stateEpochName(epoch)]; held {
			t.Errorf("epoch %d is still a file on the disk after it was discarded", epoch)
		}
	}
	for epoch := uint64(3); epoch < 6; epoch += 1 {
		state, err := store.GetGroupState(testGroupId, epoch)
		if err != nil {
			t.Errorf("epoch %d was at or above the cutoff and answered %v", epoch, err)
			continue
		}
		if string(state) != fmt.Sprintf("epoch %d", epoch) {
			t.Errorf("epoch %d came back as %q", epoch, state)
		}
	}
	// AND IT SURVIVES A REOPEN, which is what says the unlink reached the filesystem and not
	// only this process's view of it.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openTestStore(t, dir)
	if _, err := reopened.GetGroupState(testGroupId, 2); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("a discarded epoch came back after a reopen: %v", err)
	}
	// a discard with nothing below the cutoff is success and removes nothing
	if err := reopened.DeleteGroupStateBefore(testGroupId, 3); err != nil {
		t.Errorf("a second discard at the same cutoff answered %v", err)
	}
	if names := epochNamesOnDisk(t, reopened, testGroupId); len(names) != 3 {
		t.Errorf("a discard below a cutoff nothing is under removed %d epoch(s)", 3-len(names))
	}
	// and a group this store has never held is not an error either
	if err := reopened.DeleteGroupStateBefore(testGroupId2, 99); err != nil {
		t.Errorf("a discard over a group with no states answered %v", err)
	}
}

// DeleteGroupRecord LEAVES NO KEY MATERIAL BEHIND.
//
// A record with no epoch state is a restore that fails at LoadGroup; an epoch state with no record
// is this member's leaf private key and its whole path-secret ladder still on the disk for a group
// the device has left. This holds BOTH halves gone, on the disk.
func TestDeleteGroupRecordRemovesTheRecordAndEveryEpochStateBesideIt(t *testing.T) {
	store := openTestStore(t, t.TempDir())
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
	if names := epochNamesOnDisk(t, store, testGroupId); len(names) != 3 {
		t.Fatalf("this case wrote three epochs and the disk holds %d", len(names))
	}
	if err := store.DeleteGroupRecord(testGroupId); err != nil {
		t.Fatalf("DeleteGroupRecord: %v", err)
	}
	if names := epochNamesOnDisk(t, store, testGroupId); len(names) != 0 {
		t.Errorf("%d epoch state(s) survive a group this device has left: %v", len(names), names)
	}
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("%d group record(s) survive a DeleteGroupRecord", len(records))
	}
}

// A GROUP DIRECTORY WITH EPOCH STATES AND NO RECORD IS SKIPPED, NOT REFUSED AND NOT ANSWERED.
//
// It is a real state and not a corruption: mls writes an epoch state at NewGroup, before
// [Group.AddMember] has a pq_secret to write beside it, so a crash between the two leaves exactly
// this. What it is NOT is a restorable group -- there is no pq_secret, so no session can be
// rebuilt -- and answering a record with an empty secret would be the silent zero.
func TestAGroupWithEpochStatesAndNoRecordIsSkippedByGroupRecords(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.PutGroupState(testGroupId, 0, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	if err := store.PutGroupState(testGroupId2, 1, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	if err := store.PutGroupRecord(&GroupRecord{
		GroupId: testGroupId2, PqSecret: testPriv, GroupHandleKey: testPub, Epoch: 1,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("one of the two groups has a record and %d came back", len(records))
	}
	if !bytes.Equal(records[0].GroupId, testGroupId2) {
		t.Errorf("the record that came back names group %x, want %x", records[0].GroupId, testGroupId2)
	}
}

// A RECORD THAT WAS ALTERED ON THE DISK IS REFUSED RATHER THAN DECODED.
//
// Three shapes, because they have three causes: a record cut short, a record with an octet
// changed inside it, and a file that is not a record of this build at all. None of them may be
// handed to `mls.LoadGroup` as an epoch: a corrupted restore secret rebuilds a complete, well
// formed key schedule that agrees with nobody.
func TestAStateRecordAlteredOnTheDiskIsRefusedRatherThanDecoded(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	path := filepath.Join(store.epochDir(testGroupId), stateEpochName(4))
	write := func(t *testing.T, mutate func([]byte) []byte) {
		t.Helper()
		if err := store.PutGroupState(testGroupId, 4, testState); err != nil {
			t.Fatalf("PutGroupState: %v", err)
		}
		// the control: it reads back before the mutation
		if _, err := store.GetGroupState(testGroupId, 4); err != nil {
			t.Fatalf("the untouched record does not read back: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the record: %v", err)
		}
		if err := os.WriteFile(path, mutate(raw), 0o600); err != nil {
			t.Fatalf("writing the mutated record: %v", err)
		}
	}
	for _, one := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"cut short", func(raw []byte) []byte { return raw[:len(raw)-1] }},
		{"one octet changed inside it", func(raw []byte) []byte {
			raw[len(raw)/2] ^= 0x01
			return raw
		}},
		{"not a record of this build", func(raw []byte) []byte { return []byte("not a record at all") }},
	} {
		t.Run(one.name, func(t *testing.T) {
			write(t, one.mutate)
			state, err := store.GetGroupState(testGroupId, 4)
			if !errors.Is(err, ErrStateStoreFormat) {
				t.Fatalf("a record %s answered (%q, %v), want ErrStateStoreFormat", one.name, state, err)
			}
		})
	}
}

// A RECORD MOVED UNDER ANOTHER KEY'S NAME IS REFUSED.
//
// A file's name is a HASH of its key, so without this clause a directory copied from another
// device -- or two keys that collided -- would be read as the value that was asked for. The key
// octets are inside the record and are compared, which is what makes the name a name and never an
// identity.
func TestARecordMovedUnderAnotherKeysNameIsRefused(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.PutGroupState(testGroupId, 2, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(store.epochDir(testGroupId), stateEpochName(2)))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	// the same whole, checksum-valid record, under the OTHER group's name at the same epoch
	other := store.epochDir(testGroupId2)
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(other, stateEpochName(2)), raw, 0o600); err != nil {
		t.Fatalf("planting the record: %v", err)
	}
	state, err := store.GetGroupState(testGroupId2, 2)
	if !errors.Is(err, ErrStateStoreFormat) {
		t.Fatalf("one group's epoch state read under another group's name answered (%q, %v), want ErrStateStoreFormat",
			state, err)
	}
	// and under the right name at the WRONG EPOCH, which is the other half of the key
	if err := os.WriteFile(filepath.Join(store.epochDir(testGroupId), stateEpochName(9)), raw, 0o600); err != nil {
		t.Fatalf("planting the record: %v", err)
	}
	if _, err := store.GetGroupState(testGroupId, 9); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("an epoch 2 state read as epoch 9 answered %v, want ErrStateStoreFormat", err)
	}
	// the control: a private key file read as a group state is refused for its KIND
	if err := store.PutPrivateKey(testPub, testPriv); err != nil {
		t.Fatalf("PutPrivateKey: %v", err)
	}
	privRaw, err := os.ReadFile(store.privatePath(testPub))
	if err != nil {
		t.Fatalf("reading the private key record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.epochDir(testGroupId), stateEpochName(11)), privRaw, 0o600); err != nil {
		t.Fatalf("planting the record: %v", err)
	}
	if _, err := store.GetGroupState(testGroupId, 11); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("a private key read as a group state answered %v, want ErrStateStoreFormat", err)
	}
}

// TakeKeyPackage IS DESTRUCTIVE AND IT IS DESTRUCTIVE ON THE DISK.
//
// A key package is single use; a second join off one published package is a second device deriving
// the same init secret. A store that deleted only its in-memory view would re-publish it at the
// next restart, which is the one state this case is for.
func TestTakeKeyPackageIsDestructiveAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	first := openTestStore(t, dir)
	if err := first.PutKeyPackage(testRef, testKp, testInit, testEnc); err != nil {
		t.Fatalf("PutKeyPackage: %v", err)
	}
	if _, _, _, err := first.TakeKeyPackage(testRef); err != nil {
		t.Fatalf("the first take: %v", err)
	}
	if _, _, _, err := first.TakeKeyPackage(testRef); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("a second take answered %v, want ErrStateNotFound", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := openTestStore(t, dir)
	if _, _, _, err := second.TakeKeyPackage(testRef); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("a take after a restart answered %v, want ErrStateNotFound", err)
	}
}

// THE STORE RETAINS NO ARRAY ITS CALLER OWNS, IN BOTH DIRECTIONS.
//
// `mls.StateStore`'s header states one half: a store must not RETAIN a slice it was handed,
// because a write through it rewrites a group id every secret of that group was derived over and
// nothing downstream would report it. J1-5 is the other half and it is the one that bites a
// production store: `mls.JoinKeyMaterial.Zeroize` ERASES exactly the three arrays TakeKeyPackage
// answers, so a store that handed back storage it kept would have its own records wiped by a
// correct caller.
//
// A store that reads the file on every call cannot make either mistake. This case is what says it
// is that store and not a cache somebody added later.
func TestTheDurableStoreRetainsNoArrayItsCallerOwns(t *testing.T) {
	store := openTestStore(t, t.TempDir())

	// the caller's array, mutated AFTER the put
	groupId := append([]byte(nil), testGroupId...)
	state := append([]byte(nil), testState...)
	if err := store.PutGroupState(groupId, 1, state); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	for at := range groupId {
		groupId[at] ^= 0xFF
	}
	for at := range state {
		state[at] = 0
	}
	got, err := store.GetGroupState(testGroupId, 1)
	if err != nil {
		t.Fatalf("GetGroupState after the caller mutated its own arrays: %v", err)
	}
	if !bytes.Equal(got, testState) {
		t.Errorf("the stored state moved with the caller's array: %q", got)
	}

	// and the store's answer, erased by the caller the way JoinKeyMaterial.Zeroize erases it
	if err := store.PutKeyPackage(testRef, testKp, testInit, testEnc); err != nil {
		t.Fatalf("PutKeyPackage: %v", err)
	}
	if err := store.PutPrivateKey(testPub, testPriv); err != nil {
		t.Fatalf("PutPrivateKey: %v", err)
	}
	priv, err := store.GetPrivateKey(testPub)
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	for at := range priv {
		priv[at] = 0
	}
	again, err := store.GetPrivateKey(testPub)
	if err != nil {
		t.Fatalf("the second GetPrivateKey: %v", err)
	}
	if !bytes.Equal(again, testPriv) {
		t.Errorf("a caller erasing what GetPrivateKey answered destroyed the stored key: %q", again)
	}
}

// A CLOSED STORE REFUSES RATHER THAN ANSWERING AN EMPTY VALUE.
//
// A closed store that answered "no group state" would send a live device off to re-join a group it
// is already in, and one that answered a nil error from a Put would report a seal's new ratchet
// position durable after the exclusion was released.
func TestAClosedDurableStoreRefusesRatherThanAnsweringAnEmptyValue(t *testing.T) {
	store, err := OpenDurableStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenDurableStateStore: %v", err)
	}
	if err := store.PutGroupState(testGroupId, 0, testState); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("a second Close answered %v; it is meant to be idempotent", err)
	}
	for _, one := range []struct {
		name string
		call func() error
	}{
		{"PutGroupState", func() error { return store.PutGroupState(testGroupId, 1, testState) }},
		{"GetGroupState", func() error { _, err := store.GetGroupState(testGroupId, 0); return err }},
		{"DeleteGroupStateBefore", func() error { return store.DeleteGroupStateBefore(testGroupId, 1) }},
		{"PutPrivateKey", func() error { return store.PutPrivateKey(testPub, testPriv) }},
		{"GetPrivateKey", func() error { _, err := store.GetPrivateKey(testPub); return err }},
		{"DeletePrivateKey", func() error { return store.DeletePrivateKey(testPub) }},
		{"PutKeyPackage", func() error { return store.PutKeyPackage(testRef, testKp, testInit, testEnc) }},
		{"TakeKeyPackage", func() error { _, _, _, err := store.TakeKeyPackage(testRef); return err }},
		{"GetDeviceIdentity", func() error { _, _, _, _, err := store.GetDeviceIdentity(); return err }},
		{"PutDeviceIdentity", func() error { return store.PutDeviceIdentity(testPub, testPriv, testKp, testWrapSeed) }},
		{"GroupRecords", func() error { _, err := store.GroupRecords(); return err }},
		{"PutGroupRecord", func() error { return store.PutGroupRecord(&GroupRecord{GroupId: testGroupId}) }},
		{"DeleteGroupRecord", func() error { return store.DeleteGroupRecord(testGroupId) }},
	} {
		if err := one.call(); !errors.Is(err, ErrStateStoreState) {
			t.Errorf("%s on a closed store answered %v, want ErrStateStoreState", one.name, err)
		}
	}
}

// A GROUP RECORD WITH A HALF MISSING IS REFUSED AT THE WRITE.
//
// Every field of it is one a restore cannot be performed without: a record with no pq_secret is a
// group whose storage root cannot be rebuilt, and one with a short group id names no row the
// server keys. Refusing at the write is what keeps the refusal near the mistake rather than at the
// next restart.
func TestAGroupRecordMissingAHalfIsRefusedAtTheWrite(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	whole := func() *GroupRecord {
		return &GroupRecord{
			GroupId:        append([]byte(nil), testGroupId...),
			PqSecret:       bytes.Repeat([]byte{0x2A}, 32),
			GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
			Epoch:          1,
		}
	}
	// the control: the whole one is taken
	if err := store.PutGroupRecord(whole()); err != nil {
		t.Fatalf("a whole group record was refused: %v", err)
	}
	for _, one := range []struct {
		name  string
		empty func(*GroupRecord)
	}{
		{"group_id", func(r *GroupRecord) { r.GroupId = nil }},
		{"a short group_id", func(r *GroupRecord) { r.GroupId = r.GroupId[:GroupIdBytes-1] }},
		{"pq_secret", func(r *GroupRecord) { r.PqSecret = nil }},
		{"group_handle_key", func(r *GroupRecord) { r.GroupHandleKey = nil }},
	} {
		record := whole()
		one.empty(record)
		if err := store.PutGroupRecord(record); !errors.Is(err, ErrStateStoreFormat) {
			t.Errorf("a group record with no %s answered %v, want ErrStateStoreFormat", one.name, err)
		}
	}
	if err := store.PutGroupRecord(nil); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("a nil group record answered %v, want ErrStateStoreFormat", err)
	}
}

// epochNamesOnDisk is the set of epoch file names under a group, read off the FILESYSTEM.
//
// Every assertion about a discard goes through this rather than through the store's own Get,
// because "the API says it is gone" and "the octets are gone" are two statements and §5.12 is
// about the second one.
func epochNamesOnDisk(t *testing.T, store *DurableStateStore, groupId []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(store.epochDir(groupId))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return names
		}
		t.Fatalf("reading %s: %v", store.epochDir(groupId), err)
	}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	return names
}

// §5.12'S "ACTUALLY DELETE" IS MEASURED AND NOT ASSUMED, AND HERE IS THE STATE THAT PROVES IT.
//
// The discard removes by unlink and then RE-READS the directory: an epoch below the cutoff that is
// still readable is a refusal, not a success. No filesystem this suite can run on produces "remove
// returned nil and the entry survived", so the clause is unreachable without an injected failure
// -- and a §5.12 clause nobody can tell is still there is a §5.12 clause that will quietly go.
// [DurableStateStore.skipRemove] is that injection, it is [sdk.StreamStore]'s own `interrupt`
// discipline, and this case is the only thing that sets it.
//
// WHY IT MATTERS RATHER THAN BEING TIDY: §5.12 discards storage_root[n+1], write_key[n+1],
// eph_root[n+1] and every X-Wing wrap together, and the hazard it names is the HALF erase -- a
// surviving half looks exactly like a value somebody may use, and nothing downstream reports it.
// A discard that reported success over a surviving epoch is that hazard with a green test on top.
func TestADiscardThatDidNotHappenIsRefusedRatherThanReported(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	for epoch := uint64(0); epoch < 4; epoch += 1 {
		if err := store.PutGroupState(testGroupId, epoch, testState); err != nil {
			t.Fatalf("PutGroupState %d: %v", epoch, err)
		}
	}
	// the control: with the erase working, the discard is a success
	if err := store.DeleteGroupStateBefore(testGroupId, 1); err != nil {
		t.Fatalf("a working discard answered %v", err)
	}
	store.skipRemove = true
	err := store.DeleteGroupStateBefore(testGroupId, 3)
	if !errors.Is(err, ErrStateStoreState) {
		t.Fatalf("a discard that removed nothing answered %v, want ErrStateStoreState", err)
	}
	if !strings.Contains(err.Error(), "still readable") {
		t.Errorf("the refusal is %q and does not say the epoch is still readable", err)
	}
	// and the epochs really are still there, which is what says the refusal is about the disk
	if names := epochNamesOnDisk(t, store, testGroupId); len(names) != 3 {
		t.Errorf("the injected failure left %d epoch(s) on the disk, want 3", len(names))
	}
	// DeleteGroupRecord carries the same refusal, because its first step IS this one: a record
	// removed over surviving key material is the half erase seen from the other end.
	if err := store.DeleteGroupRecord(testGroupId); !errors.Is(err, ErrStateStoreState) {
		t.Errorf("DeleteGroupRecord over an erase that did not happen answered %v", err)
	}
}
