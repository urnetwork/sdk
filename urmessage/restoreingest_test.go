// A RESTORED GROUP INGESTS A COMMIT. This is the operation that REFUSED before this file existed.
//
// WHAT REFUSED, precisely, so the case can be read as the inverse of a known state rather than as a
// new feature. `messagegroup.GroupEngine` declared four methods and none of them opened a persisted
// group, so [Device.restoreOne] called `mls.LoadGroup` itself and wrapped the result in a
// `messagegroup.GroupHandle` this package declared. That handle could delegate twenty four of that
// interface's twenty six methods and could not write the other two: `messagegroup.EngineProcessed`
// carries its staged commit in an UNEXPORTED field, so the only value an implementation outside
// that package can build is one `ApplyCommit` refuses. The copy therefore refused `Process` and
// `ApplyCommit` by name, and a restored group could not follow its own group into a later epoch.
// That was open item J1-8.
//
// `GroupEngine.LoadGroup` closed it, and the shape of the fix is what this case measures: a
// restored group is now the SAME handle type a founded or a joined one is, so the two methods reach
// the same bodies rather than a second implementation. Two assertions carry it -- the epoch moved to
// 2, and the restored member's exporter at epoch 2 equals the COMMITTER's. The second is the one
// that cannot be satisfied by a counter: a member that entered the epoch with a schedule the
// committer does not share agrees about the number and about nothing else, and every record either
// side seals is refused by the other with nothing to say why.
package urmessage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/sdk"
)

// restoreDevice is a [Device] over a durable store, with the transport left nil.
//
// IT IS A STRUCT LITERAL AND NOT [NewDevice], which is a decision this comment owes a reason for.
// NewDevice refuses without a `*sdk.MessageTransport`, `connect` has no inbound listener for client
// frames, and the whole of the restore path this case drives -- LoadGroup, NewGroupSession,
// SentRecords -- touches no transport at all: [Device.restoreOne] takes the server nonce as an
// ARGUMENT rather than reading one, which is exactly what makes it drivable here. Everything else on
// the literal is what NewDevice itself assembles, through this package's own `deviceIdentity` and
// `messagegroup.NewConnectMlsEngine`, so the engine under test is the shipped one.
type restoreDevice struct {
	device      *Device
	store       *DurableStateStore
	streamStore *sdk.StreamStore
}

func openRestoreDevice(t *testing.T, root string) *restoreDevice {
	t.Helper()
	store, err := OpenDurableStateStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("OpenDurableStateStore(%s): %v", root, err)
	}
	streamStore, err := sdk.OpenStreamStore(filepath.Join(root, "stream"))
	if err != nil {
		store.Close()
		t.Fatalf("sdk.OpenStreamStore(%s): %v", root, err)
	}
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatalf("the mls crypto provider: %v", err)
	}
	// THE IDENTITY PATH THE RESTORE DEPENDS ON: this mints and writes the first time and reads
	// back the second, and mls.LoadGroup verifies the restored group's own leaf against whatever
	// key comes out of it -- so a device that came back under a fresh signer is refused outright.
	signer, signerPub, leafKeys, wrapSeed, err := deviceIdentity(crypto, store, rand.Reader)
	if err != nil {
		t.Fatalf("deviceIdentity: %v", err)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, store, signer,
		mls.BasicCredential(signerPub), leafKeys)
	if err != nil {
		t.Fatalf("the mls engine: %v", err)
	}
	return &restoreDevice{
		device: &Device{
			reserver:    sdk.NewStreamIndexReserver(streamStore),
			crypto:      crypto,
			engine:      engine,
			leafKeys:    leafKeys,
			wrapSeed:    wrapSeed,
			stateStore:  store,
			identityPub: append([]byte(nil), signerPub...),
			nowMs:       func() int64 { return time.Now().UnixMilli() },
			random:      rand.Reader,
			groups:      map[string]*Group{},
		},
		store:       store,
		streamStore: streamStore,
	}
}

func (self *restoreDevice) close() {
	self.device.Close()
	self.store.Close()
	self.streamStore.Close()
}

// restoreTestNonce is a server nonce no connection ever issued, and that is the point: the restore
// path takes it as an argument, so nothing below depends on a transport having said Hello.
func restoreTestNonce() []byte {
	return []byte("a nonce this connection issued, which no earlier process ever saw")
}

func TestRestoredGroupIngestsACommitAndEntersEpochTwo(t *testing.T) {
	root := t.TempDir()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "restore-ingests-a-commit")

	// ── the first process: found, add, commit, persist ───────────────────────────────────────
	alice := openRestoreDevice(t, filepath.Join(root, "alice"))
	bob := openRestoreDevice(t, filepath.Join(root, "bob"))
	defer bob.close()

	handle, err := alice.device.createMlsGroup(groupId)
	if err != nil {
		t.Fatalf("createMlsGroup: %v", err)
	}
	if epoch := handle.Epoch(); epoch != 0 {
		t.Fatalf("a freshly founded group is at epoch %d, want 0", epoch)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	// AT EPOCH ZERO AND NOWHERE ELSE, exactly as [Device.CreateGroup] takes it: group_handle_key
	// is the epoch-zero storage root's expansion and a value recomputed at a later epoch gives
	// every epoch a different sender_handle.
	mlsSecret, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))

	bobKeyPackage, err := bob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	if _, err := handle.ProposeAdd(bobKeyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := handle.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) over the add: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if epoch := handle.Epoch(); epoch != 1 {
		t.Fatalf("the founder is at epoch %d after the add commit, want 1", epoch)
	}
	bobHandle, err := bob.device.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("bob's JoinFromWelcome: %v", err)
	}
	defer bobHandle.Close()
	if epoch := bobHandle.Epoch(); epoch != 1 {
		t.Fatalf("the joiner is at epoch %d, want 1", epoch)
	}

	if err := alice.store.PutGroupRecord(&GroupRecord{
		GroupId:        groupId,
		PqSecret:       pqSecret,
		GroupHandleKey: groupHandleKey,
		Epoch:          1,
		Opened:         true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	// THE PROCESS DIES. The live group, the store and the stream store all go; everything below is
	// answered off the disk by a Device that was built after this line.
	if err := handle.Close(); err != nil {
		t.Fatalf("closing the founding handle: %v", err)
	}
	alice.close()

	// ── the second process: restore, then ingest ─────────────────────────────────────────────
	revived := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer revived.close()

	records, err := revived.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the disk holds %d group record(s) and one was written", len(records))
	}
	record := records[0]
	if !bytes.Equal(record.GroupId, groupId) {
		t.Fatalf("the disk holds group %x and %x was founded", record.GroupId, groupId)
	}

	restored, err := revived.device.restoreOne(revived.store, record, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne: %v", err)
	}
	if epoch := restored.handle.Epoch(); epoch != 1 {
		t.Fatalf("the restored group stands at epoch %d and the record names 1", epoch)
	}

	// THE OTHER MEMBER COMMITS. A committer merges its OWN staged commit through
	// MergePendingCommit and never through Process, so the ingest path can only be reached from a
	// commit somebody else made.
	commit, _, _, err := bobHandle.Commit(nil)
	if err != nil {
		t.Fatalf("bob's Commit(nil): %v", err)
	}
	if err := bobHandle.MergePendingCommit(); err != nil {
		t.Fatalf("bob's MergePendingCommit: %v", err)
	}
	if epoch := bobHandle.Epoch(); epoch != 2 {
		t.Fatalf("the committer is at epoch %d after its own merge, want 2", epoch)
	}

	// ── the two calls that used to be refused by name ────────────────────────────────────────
	processed, err := restored.handle.Process(commit)
	if err != nil {
		t.Fatalf("the restored group's Process over a commit: %v; this is the call that refused before GroupEngine.LoadGroup landed", err)
	}
	if processed == nil {
		t.Fatal("Process answered no processed message and no error")
	}
	if processed.Kind != messagegroup.EngineProcessedCommit {
		t.Fatalf("Process discriminated the commit as kind %d, want %d",
			processed.Kind, messagegroup.EngineProcessedCommit)
	}
	if err := restored.handle.ApplyCommit(processed); err != nil {
		t.Fatalf("the restored group's ApplyCommit: %v; this is the second of the two calls that refused", err)
	}
	if epoch := restored.handle.Epoch(); epoch != 2 {
		t.Fatalf("the restored group stands at epoch %d after ingesting one commit, want 2", epoch)
	}

	// AND THE SCHEDULES AGREE, which an epoch counter cannot say.
	restoredSecret, err := restored.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the restored group's exporter at epoch 2: %v", err)
	}
	committerSecret, err := bobHandle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the committer's exporter at epoch 2: %v", err)
	}
	if !bytes.Equal(restoredSecret, committerSecret) {
		t.Errorf("the restored member exports %x at epoch 2 and the committer exports %x; every key of the record layer is a function of this value",
			restoredSecret, committerSecret)
	}
}

// TestRestoreOneRefusesAGroupTheStoreHoldsNoStateFor keeps the wrap honest.
//
// The refusal a caller of [Device.Restore] matches on is [ErrRestore], and the cause is carried
// through it. It is here because restoreOne's own epoch comparison MOVED into
// `messagegroup.GroupEngine.LoadGroup` in the same commit that deleted this package's second
// handle: with nothing left in this function between the load and the session, a wrap that had
// stopped naming the group or stopped wrapping the cause would be invisible.
func TestRestoreOneRefusesAGroupTheStoreHoldsNoStateFor(t *testing.T) {
	root := t.TempDir()
	device := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer device.close()

	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "no-state-was-ever-persisted")
	restored, err := device.device.restoreOne(device.store, &GroupRecord{
		GroupId:        groupId,
		PqSecret:       make([]byte, 32),
		GroupHandleKey: make([]byte, 32),
		Epoch:          7,
	}, restoreTestNonce(), 1)
	if err == nil {
		restored.Close()
		t.Fatal("restoreOne answered a group for an id this store holds no epoch state for")
	}
	if !errors.Is(err, ErrRestore) {
		t.Errorf("restoreOne answered %v, want it to wrap ErrRestore", err)
	}
}
