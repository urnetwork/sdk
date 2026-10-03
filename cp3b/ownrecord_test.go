package cp3b

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/urmessage"
)

// ── this device's own records, after connect 4c030dc ────────────────────────────────────────────
//
// WHAT CHANGED UNDER THIS FILE. An application record's body is now an MLS PrivateMessage, and a
// member cannot open its own: Protect spends a generation of the leaf's own ratchet and MLS keeps no
// receiving ratchet for a leaf's own messages, so OpenRecord of a record this device sealed answers
// "mls: ratchet generation already consumed" (connect messagegroup OPENITEMS MG-4). At d368fea,
// before sdk moved, that one sentence turned 12 of this package's 30 cases red -- every restart,
// every clone case and the lost answer -- while the CP3b case itself stayed green, because a text
// crossing to the OTHER device never asks anybody to open their own record.
//
// sdk's answer is MG-4's first option: this device shows its own lines from the copy it kept, and a
// record of its own that it holds no copy of is AUTHENTICATED by the refusal MLS gives it and counted
// rather than failed. The cases below hold the edges of that answer that the twelve do not.

// AN OWN RECORD THIS DEVICE HOLDS NO COPY OF IS COUNTED, IS NOT A FAILURE, AND DOES NOT STOP THE
// DEVICE RECONCILING.
//
// It is the state of a directory written before this build kept copies, reproduced exactly: the
// copies are removed from the disk between the kill and the restart and nothing else is touched.
// Before sdk moved, this record was a FailedOpen on every Receive, the walk was never clean, and a
// restored group could never send again.
//
// WHAT WOULD GO RED: take the spent-generation clause out of openPageLocked and the Receive answers
// ErrRecordOpen, FailedOpen moves, and the Send is refused as unreconciled; take the withoutCopy
// table out and the re-read after alice's bent record counts bob's record twice.
func TestAnOwnRecordThisDeviceKeptNoCopyOfIsCountedAndIsNotAFailure(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchBendsOneRecord})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	// alice's line goes FIRST and is bent once on its way to the restarted bob, so his first
	// Receive stops the cursor in front of his own record and his second RE-READS it. That re-read
	// is what the withoutCopy table exists for: without it the record is authenticated and counted
	// a second time.
	bent, err := aliceGroup.Send(ctx, "alice, bent once, in front of bob's own line")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	const bobsLine = "a line bob sent before this build kept copies of what it sealed"
	if _, err := bobGroup.Send(ctx, bobsLine); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}

	stateDir, streamDir := bob.stateDir, bob.streamDir
	bob.kill()
	removed := removeSentCopies(t, stateDir)
	if removed != 1 {
		t.Fatalf("bob sent one line and %d copy file(s) were on the disk, so this case is not over the state it names", removed)
	}
	bob = world.durablePersona(t, "bob", stateDir, streamDir)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]
	world.shaped.bend(bent.RecordId, 1)

	// the dirty walk: alice's record fails once, bob's own record behind it is authenticated
	if _, err := bobGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("the first Receive answered %v, want ErrRecordOpen for alice's bent record", err)
	}
	if first := bobGroup.Stats().OwnWithoutCopy; first != 1 {
		t.Fatalf("after the dirty walk Stats.OwnWithoutCopy is %d, want 1", first)
	}
	// the clean walk re-reads bob's own record from the stopped cursor
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("a restored device meeting its own record with no copy answered %v", err)
	}
	stats := bobGroup.Stats()
	if stats.OwnWithoutCopy != 1 {
		t.Errorf("Stats.OwnWithoutCopy is %d after a re-read and one own record had no copy", stats.OwnWithoutCopy)
	}
	if stats.FailedOpen != 1 || stats.OpenedOwn != 0 {
		t.Errorf("FailedOpen %d and OpenedOwn %d, want 1 (alice's bend) and 0: bob's record is his and it cannot be shown", stats.FailedOpen, stats.OpenedOwn)
	}
	for _, one := range bobGroup.Messages() {
		if one.Text == bobsLine {
			t.Errorf("a line this device holds no copy of was shown anyway")
		}
	}
	if !bobGroup.Reconciled() || bobGroup.IdentityInUse() != nil {
		t.Fatalf("reconciled %v, identity in use %v: an ordinary restart without copies was not let back in",
			bobGroup.Reconciled(), bobGroup.IdentityInUse())
	}

	// a second Receive re-reads nothing it has resolved and counts nothing twice
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("the second Receive: %v", err)
	}
	if again := bobGroup.Stats().OwnWithoutCopy; again != 1 {
		t.Errorf("a second Receive moved OwnWithoutCopy to %d", again)
	}
	const afterwards = "and the device that lost its copies still speaks"
	if _, err := bobGroup.Send(ctx, afterwards); err != nil {
		t.Fatalf("the restarted bob's Send: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	texts := []string{}
	for _, one := range back {
		texts = append(texts, one.Text)
	}
	if len(back) == 0 || back[len(back)-1].Text != afterwards {
		t.Fatalf("alice read %q", texts)
	}
}

// AN OWN RECORD BENT IN FLIGHT IS A RECORD THAT DID NOT OPEN, AND NOT AN OWN RECORD.
//
// THIS IS THE CASE THAT HOLDS WHAT ownFrameAlreadySpent LEANS ON. The spent-generation refusal is
// read as "a holder of this group's keys wrote this at this position" only because connect reaches
// the inner frame AFTER both record AEADs have opened. A record whose ciphertext does not open must
// therefore never be counted as this device's own -- neither shown from the copy nor absorbed as
// evidence -- and must be the ordinary failure that is retried.
//
// WHAT WOULD GO RED: read any refusal of an own record as OwnWithoutCopy, or drop the check that the
// body_hash is the hash of THIS ct_body before a copy is shown, and the bent record is resolved on
// the first Receive with no failure at all.
func TestAnOwnRecordBentInFlightIsAFailureAndNotAnOwnRecord(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchBendsOneRecord})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)
	const bobsLine = "bob's own line, bent once on its way back to him"
	sent, err := bobGroup.Send(ctx, bobsLine)
	if err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	world.shaped.bend(sent.RecordId, 1)

	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]

	if _, err := bobGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("an own record whose ct_body was bent answered %v, want ErrRecordOpen", err)
	}
	stats := bobGroup.Stats()
	if stats.FailedOpen != 1 || stats.OwnWithoutCopy != 0 || stats.OpenedOwn != 0 {
		t.Fatalf("FailedOpen %d, OwnWithoutCopy %d, OpenedOwn %d after a bent own record, want 1, 0 and 0",
			stats.FailedOpen, stats.OwnWithoutCopy, stats.OpenedOwn)
	}
	if bobGroup.Reconciled() {
		t.Fatal("the group reconciled over a walk whose own record did not open")
	}

	// and the transient is repaired the ordinary way: the same record, unbent, is shown from the copy
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("the Receive after the bend ran out: %v", err)
	}
	shown := 0
	for _, one := range bobGroup.Messages() {
		if one.Text == bobsLine && one.Mine {
			shown += 1
		}
	}
	if shown != 1 {
		t.Errorf("bob's own line is in his log %d time(s) after the bend ran out, want once", shown)
	}
	if !bobGroup.Reconciled() {
		t.Error("the walk after the transient was clean and the group did not reconcile")
	}
}

// A COPY WHOSE EVIDENCE ARRIVED IN A DIRTY WALK IS STILL CAUGHT ON THE CLEAN ONE AFTER IT.
//
// THIS IS NOT MG-4's AND IT WAS FOUND WHILE ADAPTING TO MG-4. The reconciliation holds the highest
// own stream index it has authenticated against the reserver's high water, on the first walk that is
// complete and clean -- and that number used to belong to ONE walk. A clean walk that follows a
// dirty one starts from the resolved cursor and skips every record the dirty walk already delivered,
// so the evidence the dirty walk found was simply not in the number the clean walk compared.
//
// THE SHAPE: a copy taken after bob's first line; alice's next record bent exactly once; bob's second
// line, which the copy never sealed, after it. The copy's first Receive finds bob's second line --
// the evidence -- AND fails on alice's bent record, so it does not reconcile. Its second Receive is
// clean, re-reads from alice's record, skips bob's second line as delivered, and used to reconcile at
// high water 1 with the index-2 evidence forgotten.
//
// WHAT WOULD GO RED: make Group.ownIndexSeen one walk's number again.
func TestACopyWhoseEvidenceArrivedInADirtyWalkIsStillCaught(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchBendsOneRecord})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := bobGroup.Send(ctx, "bob 1, before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}

	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)

	bent, err := aliceGroup.Send(ctx, "alice, bent once on its way to the copy")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := bobGroup.Send(ctx, "bob 2, which the copy never sealed"); err != nil {
		t.Fatalf("bob's second Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)
	world.shaped.bend(bent.RecordId, 1)

	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]

	// the dirty walk: it holds the evidence AND a record that did not open
	if _, err := cloneGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("the copy's first Receive answered %v, want ErrRecordOpen; the case is not over a dirty walk", err)
	}
	if cloneGroup.Reconciled() {
		t.Fatal("the copy reconciled over a dirty walk, which is a different defect")
	}

	// the clean walk: it must remember what the dirty one authenticated
	_, err = cloneGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the copy's clean Receive answered %v, want ErrIdentityInUse", err)
	}
	before := streamIndicesOf(t, world, groupId, bobHandle)
	if _, err := cloneGroup.Send(ctx, "the copy must not seal at index 2"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the copy sealed after its evidence was forgotten: %v", err)
	}
	if after := streamIndicesOf(t, world, groupId, bobHandle); len(after) != len(before) {
		t.Fatalf("the copy added rows: %v -> %v", before, after)
	}
	t.Logf("caught on the clean walk: %v", cloneGroup.IdentityInUse())
}

// A SEND WHOSE COPY CANNOT BE PERSISTED IS NOT SUBMITTED.
//
// The copy is the only place a restart can show this line from, so a record that reached the server
// without one is a line the user typed that their own device can never show them again. The refusal
// costs a stream index and an MLS generation, both legal gaps.
//
// WHAT WOULD GO RED: persist the copy after the submit, or not at all, and the server holds a row at
// an index this device has no copy of.
func TestASendWhoseCopyCannotBePersistedIsNotSubmitted(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	root := t.TempDir()
	bob := world.durablePersona(t, "bob", filepath.Join(root, "state"), filepath.Join(root, "stream"))
	bob.device.Close()

	// bob again, over the SAME two stores, through a store that refuses the next copy and nothing else
	refusing := &refusingSentStore{DurableStateStore: bob.stateStore}
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport:  bob.transport,
		Reserver:   sdk.NewStreamIndexReserver(bob.streamStore),
		StateStore: refusing,
	})
	if err != nil {
		t.Fatalf("NewDevice over the refusing store: %v", err)
	}
	bob.device = device
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := bobGroup.Send(ctx, "bob's first line"); err != nil {
		t.Fatalf("bob's first Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)
	before := streamIndicesOf(t, world, groupId, bobHandle)

	refusing.refuseNext()
	const refused = "a line whose copy the disk would not take"
	if sent, err := bobGroup.Send(ctx, refused); err == nil {
		t.Fatalf("a send whose copy was refused reported success as record %d", sent.RecordId)
	} else {
		t.Logf("refused: %v", err)
	}
	if after := streamIndicesOf(t, world, groupId, bobHandle); len(after) != len(before) {
		t.Fatalf("the server's rows for bob went from %v to %v, so a record with no copy was submitted", before, after)
	}
	got, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	for _, one := range got {
		if one.Text == refused {
			t.Fatal("alice read a line whose copy was never persisted")
		}
	}
	// and the next line goes, at the next index
	if _, err := bobGroup.Send(ctx, "and the next line goes"); err != nil {
		t.Fatalf("bob's Send after the refusal: %v", err)
	}
}

// refusingSentStore is a DurableStateStore that refuses exactly the next PutSentRecord.
type refusingSentStore struct {
	*urmessage.DurableStateStore
	mutex  sync.Mutex
	refuse bool
}

func (self *refusingSentStore) refuseNext() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.refuse = true
}

func (self *refusingSentStore) PutSentRecord(groupId []byte, record *urmessage.SentRecord) error {
	self.mutex.Lock()
	refuse := self.refuse
	self.refuse = false
	self.mutex.Unlock()
	if refuse {
		return errors.New("this disk would not take the copy")
	}
	return self.DurableStateStore.PutSentRecord(groupId, record)
}

// ONE OWN RECORD SHOWN UNDER TWO RECORD IDS IS REFUSED, NOT DELIVERED TWICE.
//
// Before MG-4 the receiver ladder refused a second copy of one record, because opening it committed
// the rung. A record shown from the copy commits no rung, so the copy remembers the record id it was
// shown under instead, and a server that hands the same record back under a second number is
// answered the way a replay was.
//
// WHAT WOULD GO RED: drop the record-id clause in openOwnFromCopyLocked and the line is in the log
// twice.
func TestOneOwnRecordHandedBackUnderTwoRecordIdsIsNotShownTwice(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchRepeatsOneRecord})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)
	const bobsLine = "bob's own line, which the server hands back twice"
	sent, err := bobGroup.Send(ctx, bobsLine)
	if err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	world.shaped.repeat(sent.RecordId)

	_, err = bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("a second record id for bob's own record answered %v, want ErrRecordOpen", err)
	}
	shown := 0
	for _, one := range bobGroup.Messages() {
		if one.Text == bobsLine {
			shown += 1
		}
	}
	if shown != 1 {
		t.Fatalf("bob's own line is in his log %d times", shown)
	}
	if failed := bobGroup.Stats().FailedOpen; failed != 1 {
		t.Errorf("Stats.FailedOpen is %d", failed)
	}

	// ── and the same server game against a device that holds NO copy of the record ────────
	//
	// The record is then only COUNTED, never shown, and it has to be counted once: the index is
	// learned off the first record id, and the second id is refused the same way. It is also what
	// holds the `hasCopy` clause in openOwnFromCopyLocked -- without it the learned index, which has
	// no body, is shown as an empty line of bob's own.
	stateDir, streamDir := bob.stateDir, bob.streamDir
	bob.kill()
	if removed := removeSentCopies(t, stateDir); removed != 1 {
		t.Fatalf("bob sent one line and %d copies were on the disk", removed)
	}
	bob = world.durablePersona(t, "bob", stateDir, streamDir)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]
	if _, err := bobGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("a second record id for an own record with no copy answered %v, want ErrRecordOpen", err)
	}
	for _, one := range bobGroup.Messages() {
		if one.Mine {
			t.Errorf("a device with no copy of its own record showed %q as its own", one.Text)
		}
	}
	if stats := bobGroup.Stats(); stats.OwnWithoutCopy != 1 || stats.FailedOpen != 1 {
		t.Errorf("OwnWithoutCopy %d and FailedOpen %d, want 1 and 1", stats.OwnWithoutCopy, stats.FailedOpen)
	}
}

// removeSentCopies deletes every sent copy under one state directory and answers how many it
// removed. It is the disk a build that kept no copies would have left.
func removeSentCopies(t *testing.T, stateDir string) int {
	t.Helper()
	removed := 0
	err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(filepath.Dir(path)) != "sent" || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		removed += 1
		return os.Remove(path)
	})
	if err != nil {
		t.Fatalf("removing the sent copies under %s: %v", stateDir, err)
	}
	return removed
}

// PAST THE RECEIVER LADDER'S WINDOW, A COPY IS STILL CAUGHT AND A RESTART WITHOUT COPIES IS STILL
// WHOLE.
//
// THIS IS THE COST OF NOT OPENING OWN RECORDS THAT NOTHING ELSE HERE REACHES, because it needs more
// than 1,024 of them. Every other case in this file seals a handful of lines; the ladder over a
// leaf's record keys has a window of messagegroup.DefaultRecordWindowSize, and since MG-4 nothing
// commits a rung of this device's OWN ladder while its own records are shown from copies. So the
// first own record that did need opening past index 1,024 was ErrOutOfWindow -- MEASURED over 1,030
// lines before Group.advanceOwnLadderLocked existed: a copy one line behind the original reconciled
// cleanly on its fourth Receive with the evidence record abandoned, and a restart with no copies
// left six abandoned holes over three Receives.
//
// ONE SETUP, TWO READERS, because 1,030 real sends is the expensive part: a copy of bob's folder
// taken after all of them, which must be caught at its FIRST Receive; and bob himself restarted with
// every copy removed, whose first Receive must come back clean with every own record counted.
//
// WHAT WOULD GO RED: take advanceOwnLadderLocked's call out of openPageLocked.
func TestPastTheLaddersWindowACopyIsStillCaughtAndARestartIsStillWhole(t *testing.T) {
	const lines = 1030
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)
	for at := 0; at < lines; at += 1 {
		if _, err := bobGroup.Send(ctx, "bob, one of many"); err != nil {
			t.Fatalf("bob's Send %d: %v", at, err)
		}
	}

	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)
	if _, err := bobGroup.Send(ctx, "bob, one line past the copy and past the window"); err != nil {
		t.Fatalf("bob's Send after the copy: %v", err)
	}

	// ── the copy, one line behind, at an index past the window ─────────────────────────────
	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]
	if _, err := cloneGroup.Receive(ctx); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("a copy one line behind at index %d answered %v at its first Receive, want ErrIdentityInUse", lines+1, err)
	}
	// lines+1 own records became messages: the copy's own 1,030 from its copies, and the original's
	// line past the copy, which OPENED -- the copy never spent that generation -- and is the evidence.
	if stats := cloneGroup.Stats(); stats.FailedOpen != 0 || stats.OpenedOwn != lines+1 {
		t.Errorf("the copy's walk: FailedOpen %d, OpenedOwn %d, want 0 and %d", stats.FailedOpen, stats.OpenedOwn, lines+1)
	}
	clone.kill()

	// ── bob himself, restarted over a folder with every copy removed ───────────────────────
	stateDir, streamDir := bob.stateDir, bob.streamDir
	bob.kill()
	if removed := removeSentCopies(t, stateDir); removed != lines+1 {
		t.Fatalf("bob sent %d lines and %d copies were on the disk", lines+1, removed)
	}
	bob = world.durablePersona(t, "bob", stateDir, streamDir)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err = bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("a restart with %d own records and no copies answered %v", lines+1, err)
	}
	stats := bobGroup.Stats()
	if stats.OwnWithoutCopy != lines+1 || stats.FailedOpen != 0 || stats.Unopened != 0 {
		t.Errorf("OwnWithoutCopy %d, FailedOpen %d, Unopened %d, want %d, 0 and 0",
			stats.OwnWithoutCopy, stats.FailedOpen, stats.Unopened, lines+1)
	}
	if !bobGroup.Reconciled() || bobGroup.IdentityInUse() != nil {
		t.Errorf("reconciled %v, identity in use %v", bobGroup.Reconciled(), bobGroup.IdentityInUse())
	}
}
