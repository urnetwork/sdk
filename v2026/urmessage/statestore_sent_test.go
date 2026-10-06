package urmessage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// THE COPIES OF WHAT THIS DEVICE SENT: WHAT [SentRecord] PROMISES, HELD ON THE STORE ALONE.
//
// Since connect 4c030dc these are the only place a device's own half of a conversation can be read
// back from (MG-4), so the store's half of that promise is measured here without a server: every
// copy survives a close and reopen with every field, in index order; a second copy at one index is
// refused rather than replacing the user's words; a copy moved under another index is refused
// rather than shown at a position where the user said something else; and leaving the group takes
// every copy with it.

func testSentRecord(index uint64, body string) *SentRecord {
	return &SentRecord{
		StreamIndex: index,
		BodyHash:    sha256.Sum256([]byte("ct_body of " + body)),
		SentAtMs:    1789500000000 + int64(index),
		Body:        []byte(body),
	}
}

func TestEverySentCopyIsThereAfterTheStoreIsReopenedInStreamIndexOrder(t *testing.T) {
	dir := t.TempDir()
	first := openTestStore(t, dir)
	written := []*SentRecord{
		testSentRecord(3, "the third line"),
		testSentRecord(1, "the first line, with a NUL \x00 in it"),
		testSentRecord(2, ""),
	}
	for _, one := range written {
		if err := first.PutSentRecord(testGroupId, one); err != nil {
			t.Fatalf("PutSentRecord(%d): %v", one.StreamIndex, err)
		}
	}
	if err := first.PutSentRecord(testGroupId2, testSentRecord(1, "another group's line")); err != nil {
		t.Fatalf("PutSentRecord in a second group: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openTestStore(t, dir)
	got, err := second.SentRecords(testGroupId)
	if err != nil {
		t.Fatalf("SentRecords after a reopen: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("three copies were written and %d came back", len(got))
	}
	for at, want := range []uint64{1, 2, 3} {
		if got[at].StreamIndex != want {
			t.Fatalf("copy %d is at stream index %d, want %d: the order is not the index order", at, got[at].StreamIndex, want)
		}
	}
	byIndex := map[uint64]*SentRecord{}
	for _, one := range written {
		byIndex[one.StreamIndex] = one
	}
	for _, one := range got {
		want := byIndex[one.StreamIndex]
		if one.BodyHash != want.BodyHash || one.SentAtMs != want.SentAtMs || !bytes.Equal(one.Body, want.Body) {
			t.Errorf("the copy at index %d came back as %+v, want %+v", one.StreamIndex, one, want)
		}
	}
	other, err := second.SentRecords(testGroupId2)
	if err != nil || len(other) != 1 || string(other[0].Body) != "another group's line" {
		t.Errorf("the second group's copies came back as %v, %v", other, err)
	}
	none, err := second.SentRecords(bytes.Repeat([]byte{0x03}, GroupIdBytes))
	if err != nil || len(none) != 0 {
		t.Errorf("a group with no copies answered %v, %v, want an empty list and no error", none, err)
	}
}

func TestASecondCopyAtOneStreamIndexIsRefusedAndTheFirstIsKept(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if err := store.PutSentRecord(testGroupId, testSentRecord(4, "what the user said at index 4")); err != nil {
		t.Fatalf("PutSentRecord: %v", err)
	}
	err := store.PutSentRecord(testGroupId, testSentRecord(4, "something else at the same index"))
	if !errors.Is(err, ErrStateStoreState) {
		t.Fatalf("a second copy at one index answered %v, want ErrStateStoreState", err)
	}
	got, err := store.SentRecords(testGroupId)
	if err != nil || len(got) != 1 || string(got[0].Body) != "what the user said at index 4" {
		t.Fatalf("after the refused second copy the store holds %v, %v", got, err)
	}
}

func TestASentCopyMovedUnderAnotherIndexOrBesideAStrangerIsRefused(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutSentRecord(testGroupId, testSentRecord(1, "said at index 1")); err != nil {
		t.Fatalf("PutSentRecord: %v", err)
	}
	sent := store.sentDir(testGroupId)
	if err := os.Rename(filepath.Join(sent, stateEpochName(1)), filepath.Join(sent, stateEpochName(9))); err != nil {
		t.Fatalf("moving the copy: %v", err)
	}
	if _, err := store.SentRecords(testGroupId); !errors.Is(err, ErrStateStoreFormat) {
		t.Fatalf("a copy moved under index 9 answered %v, want ErrStateStoreFormat", err)
	}
	if err := os.Rename(filepath.Join(sent, stateEpochName(9)), filepath.Join(sent, stateEpochName(1))); err != nil {
		t.Fatalf("moving the copy back: %v", err)
	}
	if _, err := store.SentRecords(testGroupId); err != nil {
		t.Fatalf("the copy moved back answered %v", err)
	}
	if err := os.WriteFile(filepath.Join(sent, "Thumbs.db"), []byte("not ours"), 0o600); err != nil {
		t.Fatalf("planting a stranger: %v", err)
	}
	if _, err := store.SentRecords(testGroupId); !errors.Is(err, ErrStateStoreFormat) {
		t.Fatalf("a stranger in the sent directory answered %v, want ErrStateStoreFormat", err)
	}
}

func TestDeleteGroupRecordTakesEverySentCopyWithIt(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	if err := store.PutGroupRecord(&GroupRecord{
		GroupId: testGroupId, PqSecret: bytes.Repeat([]byte{0x2A}, 32),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32), Epoch: 1, Opened: true,
	}); err != nil {
		t.Fatalf("PutGroupRecord: %v", err)
	}
	for index := uint64(1); index <= 3; index += 1 {
		if err := store.PutSentRecord(testGroupId, testSentRecord(index, "a line")); err != nil {
			t.Fatalf("PutSentRecord(%d): %v", index, err)
		}
	}
	// and an unfinished write a killed process left, which is a copy too
	leftover := filepath.Join(store.sentDir(testGroupId), stateTempPrefix+"12345678")
	if err := os.WriteFile(leftover, []byte("half of a line"), 0o600); err != nil {
		t.Fatalf("planting the leftover: %v", err)
	}
	if err := store.DeleteGroupRecord(testGroupId); err != nil {
		t.Fatalf("DeleteGroupRecord: %v", err)
	}
	if _, err := os.Stat(store.sentDir(testGroupId)); !errors.Is(err, os.ErrNotExist) {
		entries, _ := os.ReadDir(store.sentDir(testGroupId))
		t.Fatalf("the sent directory survived leaving the group, holding %d entries (stat: %v)", len(entries), err)
	}
	got, err := store.SentRecords(testGroupId)
	if err != nil || len(got) != 0 {
		t.Fatalf("after leaving the group SentRecords answered %v, %v", got, err)
	}

	// THE MEASUREMENT HALF: a discard that removes nothing it can see must not report success over
	// a copy that is still readable.
	if err := store.PutGroupRecord(&GroupRecord{
		GroupId: testGroupId, PqSecret: bytes.Repeat([]byte{0x2A}, 32),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32), Epoch: 1, Opened: true,
	}); err != nil {
		t.Fatalf("PutGroupRecord again: %v", err)
	}
	if err := store.PutSentRecord(testGroupId, testSentRecord(1, "a line that will not go")); err != nil {
		t.Fatalf("PutSentRecord again: %v", err)
	}
	store.skipRemove = true
	if err := store.DeleteGroupRecord(testGroupId); !errors.Is(err, ErrStateStoreState) {
		t.Fatalf("a discard that removed nothing answered %v, want ErrStateStoreState", err)
	}
	store.skipRemove = false
	if records, err := store.GroupRecords(); err != nil || len(records) != 1 {
		t.Fatalf("a refused discard left %d group record(s), %v: the record must outlive a copy that did not go", len(records), err)
	}
}
