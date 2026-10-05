// Ledger item 241's restart half at the store: the [PeerHead] table round-trips, a directory a
// build before this one wrote reads back with NO table and no error, and a table that is not this
// build's is refused by name rather than read as a head.
package urmessage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestThePeerHeadTableRoundTripsAndAnOldDirectoryReadsBackEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurableStateStore(dir)
	if err != nil {
		t.Fatalf("OpenDurableStateStore: %v", err)
	}
	defer store.Close()
	groupId := make([]byte, GroupIdBytes)
	copy(groupId, "peer-heads-round-trip")

	// AN OLD DIRECTORY: a group with a record and no table answers an EMPTY table and no error,
	// which is what keeps every group persisted before item 241 restorable.
	if err := store.PutGroupRecord(&GroupRecord{
		GroupId: groupId, PqSecret: make([]byte, 32), GroupHandleKey: make([]byte, 32), Epoch: 3, Opened: true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	heads, err := store.PeerHeads(groupId)
	if err != nil {
		t.Fatalf("PeerHeads over a group with no table answered %v, want an empty table and no error", err)
	}
	if len(heads) != 0 {
		t.Fatalf("PeerHeads over a group with no table answered %d row(s)", len(heads))
	}

	// THE ROUND TRIP, in the order a map would not give.
	written := []PeerHead{
		{Epoch: 2, Leaf: 1, RetentionWire: 0x02, EphWindow: 0, Head: 1031},
		{Epoch: 1, Leaf: 1, RetentionWire: 0x02, EphWindow: 0, Head: 1030},
		{Epoch: 1, Leaf: 0, RetentionWire: 0x01, EphWindow: 7, Head: 4},
	}
	if err := store.PutPeerHeads(groupId, written); err != nil {
		t.Fatalf("PutPeerHeads: %v", err)
	}
	heads, err = store.PeerHeads(groupId)
	if err != nil {
		t.Fatalf("PeerHeads: %v", err)
	}
	want := []PeerHead{written[2], written[1], written[0]}
	if len(heads) != len(want) {
		t.Fatalf("PeerHeads answered %d row(s), want %d: %+v", len(heads), len(want), heads)
	}
	for i := range want {
		if heads[i] != want[i] {
			t.Errorf("row %d is %+v, want %+v", i, heads[i], want[i])
		}
	}
	// REPLACED WHOLE: a second write with one row leaves one row.
	if err := store.PutPeerHeads(groupId, written[:1]); err != nil {
		t.Fatalf("the second PutPeerHeads: %v", err)
	}
	if heads, err = store.PeerHeads(groupId); err != nil || len(heads) != 1 || heads[0] != written[0] {
		t.Errorf("after a second write the table is %+v / %v, want exactly %+v", heads, err, written[0])
	}

	// AND IT IS THE STORE'S OWN RECORD: a file under the table's name that this build did not write
	// is refused by name, and a row of the wrong width is refused by name.
	path := filepath.Join(dir, stateDataDirName, "group", stateNameOf(groupId), "heads")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the table is not at %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("not a record"), 0o600); err != nil {
		t.Fatalf("overwriting the table: %v", err)
	}
	if _, err := store.PeerHeads(groupId); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("a table that is not this store's record answered %v, want ErrStateStoreFormat", err)
	}
	short, err := encodeStateRecord(stateKindPeerHeads, groupId, make([]byte, peerHeadOctets-1))
	if err != nil {
		t.Fatalf("encoding a short row: %v", err)
	}
	if err := os.WriteFile(path, short, 0o600); err != nil {
		t.Fatalf("overwriting the table: %v", err)
	}
	if _, err := store.PeerHeads(groupId); !errors.Is(err, ErrStateStoreFormat) {
		t.Errorf("a row of %d octets answered %v, want ErrStateStoreFormat", peerHeadOctets-1, err)
	}

	// AND LEAVING THE GROUP TAKES THE TABLE WITH IT.
	if err := store.PutPeerHeads(groupId, written); err != nil {
		t.Fatalf("PutPeerHeads before the delete: %v", err)
	}
	if err := store.DeleteGroupRecord(groupId); err != nil {
		t.Fatalf("DeleteGroupRecord: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the head table survived DeleteGroupRecord: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the group directory survived DeleteGroupRecord, so something in it was left behind: %v", err)
	}
}
