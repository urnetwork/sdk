// THE EPOCH IS PERSISTED WHEN IT CHANGES, AND A RESTART COMES BACK AT IT. This is ledger item 239's
// step A3, and the case below is the one that was silently wrong before it existed.
//
// WHAT WAS WRONG, precisely. [GroupRecord.Epoch] is the epoch `GroupEngine.LoadGroup` is asked for
// at the next restart, and it was written at exactly two moments: [Device.Join] and [Group.Open],
// both of which are epoch one. Step A1 made a restored group able to INGEST a commit and stand at
// epoch two -- and left the record naming one. mls keeps thirty two past epochs of state, so the
// restart after that did not refuse anything: LoadGroup was asked for epoch one, answered epoch
// one, and the device came back internally consistent at an epoch every peer had left. Every
// record it then sealed was under a schedule nobody else held, and nothing on that path says so.
//
// SO THE CASE DRIVES EXACTLY THAT SEQUENCE and asserts the restart comes back at TWO: a group
// persisted at epoch one, restored, moved to epoch two by another member's commit, the process
// killed, and a Device that did not exist when any of that happened restoring it. Then it SEALS
// at epoch two and the committer opens it, which is the assertion an epoch number cannot make.
//
// THIS CASE STANDS WHERE THE INGEST SITE WILL STAND. No production path of this package yet
// processes a commit into a [Group] -- [Group.Receive] skips every is_commit record, counted as
// SkippedCeremony -- so the case drives the handle the way that site will (Process, ApplyCommit,
// a session over the new epoch) and then calls [Group.enterEpochLocked], which is the door that
// site will call. The gate at the bottom of this file is what keeps the door the ONLY door, so
// that when the site is written it cannot move the epoch any other way.
package urmessage

import (
	"bytes"
	"crypto/rand"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
)

func TestAnEpochChangeIsPersistedAndTheRestartComesBackAtItAndSeals(t *testing.T) {
	root := t.TempDir()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "epoch-persist-restart")

	// ── the first process: found, add BY VALUE, persist at epoch one ─────────────────────────
	alice := openRestoreDevice(t, filepath.Join(root, "alice"))
	bob := openRestoreDevice(t, filepath.Join(root, "bob"))
	defer bob.close()

	handle, err := alice.device.createMlsGroup(groupId)
	if err != nil {
		t.Fatalf("createMlsGroup: %v", err)
	}
	pqSecret, err := messagegroup.NewPqSecret(rand.Reader)
	if err != nil {
		t.Fatalf("NewPqSecret: %v", err)
	}
	mlsSecret, err := handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch zero exporter: %v", err)
	}
	groupHandleKey := messagegroup.GroupHandleKey(messagegroup.StorageRoot(mlsSecret, pqSecret))
	bobKeyPackage, err := bob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	// the arm [Group.AddMember] now takes, driven at the handle for the reason restoreingest
	// gives: NewDevice refuses without a transport and the founding needs none.
	_, welcome, ratchetTree, err := handle.CommitAdd([][]byte{bobKeyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	bobHandle, err := bob.device.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("bob's JoinFromWelcome: %v", err)
	}
	defer bobHandle.Close()
	if err := alice.store.PutGroupRecord(&GroupRecord{
		GroupId:        groupId,
		PqSecret:       pqSecret,
		GroupHandleKey: groupHandleKey,
		Epoch:          1,
		Opened:         true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("closing the founding handle: %v", err)
	}
	alice.close()

	// ── the second process: restore at one, follow bob's commit to two ───────────────────────
	revived := openRestoreDevice(t, filepath.Join(root, "alice"))
	record := epochPersistTheOneRecord(t, revived.store, groupId)
	if record.Epoch != 1 {
		t.Fatalf("the disk names epoch %d before anything moved, want 1", record.Epoch)
	}
	restored, err := revived.device.restoreOne(revived.store, record, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne: %v", err)
	}
	if restored.Epoch() != 1 || !restored.opened {
		t.Fatalf("the restored group is at epoch %d, opened=%v; want 1 and opened", restored.Epoch(), restored.opened)
	}

	commit, _, _, err := bobHandle.Commit(nil)
	if err != nil {
		t.Fatalf("bob's Commit(nil): %v", err)
	}
	if err := bobHandle.MergePendingCommit(); err != nil {
		t.Fatalf("bob's MergePendingCommit: %v", err)
	}
	processed, err := restored.handle.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := restored.handle.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if epoch := restored.handle.Epoch(); epoch != 2 {
		t.Fatalf("the handle stands at epoch %d after the ingest, want 2", epoch)
	}
	// what the ingest site will do: a session over the new epoch, then the door.
	session, err := messagegroup.NewGroupSession(restored.handle, restored.pqSecretLocked(), restored.groupHandleKey,
		revived.device.reserver, revived.device.nowMs, restoreTestNonce())
	if err != nil {
		t.Fatalf("the session at epoch 2: %v", err)
	}
	restored.mutex.Lock()
	restored.session.Close()
	restored.session = session
	// and what the ingest site ALSO does since ledger item 251's ruling 40: file the epoch's own
	// pq_secret, so the record the door writes carries a table covering the epoch it names. This
	// world does not rotate, so the value is the one the group already held.
	restored.filePqSecretLocked(restored.handle.Epoch(), restored.pqSecretLocked())
	err = restored.enterEpochLocked()
	restored.mutex.Unlock()
	if err != nil {
		t.Fatalf("enterEpochLocked: %v", err)
	}
	if restored.Epoch() != 2 {
		t.Fatalf("Group.Epoch answers %d after the door, want 2", restored.Epoch())
	}
	// THE DISK, READ BACK IN THE SAME PROCESS: the record moved with the epoch and not later.
	if moved := epochPersistTheOneRecord(t, revived.store, groupId); moved.Epoch != 2 {
		t.Fatalf("the record on the disk names epoch %d after the group entered 2; the next restart would be asked for the epoch before", moved.Epoch)
	}
	// THE PROCESS DIES, at the epoch the record names.
	if err := restored.Close(); err != nil {
		t.Fatalf("closing the restored group: %v", err)
	}
	revived.close()

	// ── the third process: a Device that never saw epoch two derived ─────────────────────────
	third := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer third.close()
	record = epochPersistTheOneRecord(t, third.store, groupId)
	if record.Epoch != 2 {
		t.Fatalf("the third process read epoch %d off the disk, want 2", record.Epoch)
	}
	again, err := third.device.restoreOne(third.store, record, restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne in the third process: %v", err)
	}
	defer again.Close()
	if again.Epoch() != 2 || again.handle.Epoch() != 2 {
		t.Fatalf("the third process came back at Group.Epoch %d / handle %d, want 2 / 2",
			again.Epoch(), again.handle.Epoch())
	}

	// ── AND IT SENDS: a record sealed at epoch two that the committer opens ──────────────────
	//
	// [Group.Send] is not reachable here -- a restored group refuses to send until [Group.Receive]
	// has walked its history against a server, and there is none -- so this is the seal that Send
	// performs, through the session restoreOne built, opened by bob's session at the same epoch.
	// A restart that had come back with the right number and the wrong schedule seals a
	// well-formed record here that the next line refuses.
	bobSession, err := messagegroup.NewGroupSession(bobHandle, pqSecret, groupHandleKey,
		bob.device.reserver, bob.device.nowMs, restoreTestNonce())
	if err != nil {
		t.Fatalf("bob's session at epoch 2: %v", err)
	}
	defer bobSession.Close()
	const line = "sealed at epoch two, by a device that restarted into it"
	sealed, err := again.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), []byte(line), 0, nil)
	if err != nil {
		t.Fatalf("the restarted device could not seal at epoch 2: %v", err)
	}
	if sealed.Header.Epoch != 2 {
		t.Fatalf("the record's header names epoch %d, want 2", sealed.Header.Epoch)
	}
	leaf, known := crossProcessLeafOf(t, bobHandle, groupHandleKey, sealed.Header.SenderHandle)
	if !known {
		t.Fatalf("the record names sender_handle %x, which is no leaf of bob's group", sealed.Header.SenderHandle)
	}
	if err := bobSession.TrackSender(leaf, sealed.Header.RetentionClass, sealed.Header.EphBucket,
		sealed.Header.EphWindow, 0); err != nil {
		t.Fatalf("bob's TrackSender(%d): %v", leaf, err)
	}
	_, opened, err := bobSession.OpenRecord(sealed)
	if err != nil {
		t.Fatalf("the committer could not open what the restarted device sealed at epoch 2: %v", err)
	}
	if !bytes.Equal(opened, []byte(line)) {
		t.Fatalf("the record opened to %q", opened)
	}
}

// epochPersistTheOneRecord reads the single group record a store holds and refuses any other count.
func epochPersistTheOneRecord(t *testing.T, store *DurableStateStore, groupId []byte) *GroupRecord {
	t.Helper()
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the disk holds %d group record(s) and one was written", len(records))
	}
	if !bytes.Equal(records[0].GroupId, groupId) {
		t.Fatalf("the disk holds group %x and %x was founded", records[0].GroupId, groupId)
	}
	return records[0]
}

// TestAnUnopenedGroupEntersItsEpochWithoutWritingARecord is the door's other arm, and the paragraph
// in [Group.AddMember] it keeps true: a founder that has added and not yet opened writes nothing,
// so a founder that dies before Open does not come back as a conversation it can never send in.
func TestAnUnopenedGroupEntersItsEpochWithoutWritingARecord(t *testing.T) {
	root := t.TempDir()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "epoch-persist-unopened")
	alice := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer alice.close()
	bob := openRestoreDevice(t, filepath.Join(root, "bob"))
	defer bob.close()

	handle, err := alice.device.createMlsGroup(groupId)
	if err != nil {
		t.Fatalf("createMlsGroup: %v", err)
	}
	defer handle.Close()
	bobKeyPackage, err := bob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	if _, _, _, err := handle.CommitAdd([][]byte{bobKeyPackage}); err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	if err := handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	unopened := &Group{
		device:         alice.device,
		id:             groupId,
		handle:         handle,
		groupHandleKey: make([]byte, 32),
		pqSecrets:      map[uint64][]byte{handle.Epoch(): make([]byte, messagegroup.PqSecretBytes)},
	}
	if err := unopened.enterEpochLocked(); err != nil {
		t.Fatalf("enterEpochLocked on an unopened group: %v", err)
	}
	if unopened.epoch != 1 {
		t.Fatalf("the unopened group's epoch is %d after the door, want 1", unopened.epoch)
	}
	records, err := alice.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("an unopened group wrote %d record(s); a founder that dies before Open would come back as a group it can never send in", len(records))
	}
}

// TestTheDoorReportsAStoreThatWouldNotTakeTheRecord is the refusal arm: a store that will not write
// the record is an error the caller sees, and not a group that moved in memory and stayed on the
// disk with nothing said. The in-memory epoch HAS moved by then and the error names it.
func TestTheDoorReportsAStoreThatWouldNotTakeTheRecord(t *testing.T) {
	root := t.TempDir()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "epoch-persist-refused")
	alice := openRestoreDevice(t, filepath.Join(root, "alice"))
	defer alice.streamStore.Close()
	defer alice.device.Close()

	handle, err := alice.device.createMlsGroup(groupId)
	if err != nil {
		t.Fatalf("createMlsGroup: %v", err)
	}
	defer handle.Close()
	opened := &Group{
		device:         alice.device,
		id:             groupId,
		handle:         handle,
		groupHandleKey: make([]byte, 32),
		pqSecrets:      map[uint64][]byte{handle.Epoch(): make([]byte, messagegroup.PqSecretBytes)},
		opened:         true,
	}
	// the control: the same door over the same store, open, takes the record.
	if err := opened.enterEpochLocked(); err != nil {
		t.Fatalf("enterEpochLocked over an open store: %v", err)
	}
	// THE STORE CLOSES UNDERNEATH THE GROUP, which is the one refusal a durable store answers
	// without a filesystem fault to stage.
	if err := alice.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	err = opened.enterEpochLocked()
	if err == nil {
		t.Fatal("the door answered nil over a store that refused the record; a restart would come back at the epoch before with nothing having said so")
	}
	if !strings.Contains(err.Error(), "could not be persisted") {
		t.Errorf("the refusal does not say what it is about: %v", err)
	}
}

// TestEveryAssignmentToGroupEpochIsInsideTheOneDoor is the gate: [Group.epoch] is assigned in
// [Group.enterEpochLocked] and nowhere else in this package's production source.
//
// IT READS THE SYNTAX TREE AND NOT THIS FILE'S OPINION. A composite literal that sets `epoch:` --
// [Device.Join] and [Device.restoreOne] build a Group that way, and both write or read the record
// in the same block -- is a construction and not a change, and is left alone. What is refused is
// an assignment statement whose left side is `<anything>.epoch`, outside the door, because that
// is the shape of a site that moved the epoch and may or may not have remembered the record.
//
// THE POSITIVE CONTROL IS INLINE: the walk has to FIND the door's own assignment, or it has read
// nothing and a refusal from it would be vacuous.
func TestEveryAssignmentToGroupEpochIsInsideTheOneDoor(t *testing.T) {
	const door = "enterEpochLocked"
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	foundTheDoor := false
	filesRead := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		filesRead += 1
		for _, declaration := range parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, isAssignment := node.(*ast.AssignStmt)
				if !isAssignment {
					return true
				}
				for _, left := range assignment.Lhs {
					selector, isSelector := left.(*ast.SelectorExpr)
					if !isSelector || selector.Sel.Name != "epoch" {
						continue
					}
					if function.Name.Name == door {
						foundTheDoor = true
						continue
					}
					t.Errorf("%s assigns .epoch at %s; Group.epoch moves only inside %s, where the record is written in the same block",
						function.Name.Name, fileSet.Position(assignment.Pos()), door)
				}
				return true
			})
		}
	}
	if filesRead == 0 {
		t.Fatal("no production source was read, so this gate held nothing")
	}
	if !foundTheDoor {
		t.Fatalf("the walk did not find %s's own assignment to .epoch, so it is not reading what it claims to", door)
	}
}
