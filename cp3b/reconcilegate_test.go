package cp3b

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// ── the reconciliation may only conclude from a walk that saw the history ────────────────────
//
// THE DEFECT THESE CASES PIN, SAID ONCE FOR ALL THREE. The clone check's clause 1 rests on
// "[urmessage.Group.Receive] walks the group's WHOLE history". `commitWalkLocked` gated it on
// `walk.complete && !self.reconciled` -- and `walk.complete` means only that the SERVER called one
// page complete. The same walk carries two fields that say the history was not seen, BOTH OF WHICH
// THIS CLIENT COMPUTED AND RETURNED TO ITS CALLER: `walk.omitted` (§4.3.4's high_water_record_id
// above everything handed over) and `walk.firstFailure` (a record that did not open). Neither was
// consulted, so the two cheapest ways past the clone check were failures the client had already
// detected and printed. Both were measured, both let a copy that was BEHIND the original -- the
// case [urmessage.Device.Restore]'s header says is caught before it seals anything -- seal at an
// index the original had already used.
//
// THE REPAIR IS [urmessage.Group.walkReconcilesLocked] and it is one condition in the shape the
// function already had: a group that has not had a clean walk stays [urmessage.ErrNotReconciled],
// which is exactly what the transport-error path two lines above already does.
//
// AND THE THIRD CASE IS THE ONE THAT KEEPS IT FROM BEING A WEDGE. A gate that never opens is not a
// repair, so [TestAGroupReconcilesOnceTheWalkIsCleanAgain] drives the transient away and holds that
// the group then reconciles and sends.

// A COPY IS NOT RECONCILED OVER A PAGE THE SERVER ITSELF SAID WAS SHORT.
//
// The server here tampers with nothing. `fetchDropsMessages` hands over the ceremony, holds the
// MESSAGES back, and leaves `complete` and `high_water_record_id` exactly as the real store
// computed them -- so it is TRUTHFUL about what it holds and simply does not hand it over, which is
// §7.2's retention sweep on a good day and an omitting server on a bad one. The client already
// detects it: [urmessage.ErrFetchOmitted] comes back and Stats.Omitted moves.
//
// WHAT WOULD GO RED WITHOUT THE FIX: `Reconciled()` is true beside that very error, and the next
// Send seals at an index the original already used.
func TestACopyIsNotReconciledOverAPageTheServerSaidWasShort(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchDropsMessages})
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

	// two lines, then the copy is taken, then more: the copy is BEHIND.
	for _, text := range []string{"bob 1", "bob 2"} {
		if _, err := bobGroup.Send(ctx, text); err != nil {
			t.Fatalf("bob's Send: %v", err)
		}
	}
	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)
	for _, text := range []string{"bob 3, after the copy", "bob 4, after the copy"} {
		if _, err := bobGroup.Send(ctx, text); err != nil {
			t.Fatalf("bob's Send after the copy: %v", err)
		}
	}

	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]

	_, err = cloneGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrFetchOmitted) {
		t.Fatalf("the omitting server was answered %v, want ErrFetchOmitted; this case is not over the state it names", err)
	}
	t.Logf("the copy's reconciling Receive answered: %v", err)
	if omitted := cloneGroup.Stats().Omitted; omitted != 1 {
		t.Errorf("Stats.Omitted is %d, so the client did not detect the omission it is being held to", omitted)
	}

	// THE FINDING, AS AN ASSERTION: the group must not call itself checked over that walk.
	if cloneGroup.Reconciled() {
		t.Fatal("the group reconciled over a page the server said was short, so the next Send seals")
	}
	if _, err := cloneGroup.Send(ctx, "the copy speaks after an admittedly short walk"); !errors.Is(err, urmessage.ErrNotReconciled) {
		t.Fatalf("the copy sealed after an admittedly short walk: %v", err)
	}

	// AND THE MEASUREMENT THAT DOES NOT DEPEND ON THIS CLIENT TELLING THE TRUTH ABOUT ITSELF:
	// the server's own rows carry no (sender_handle, stream_index) pair twice. It is read off
	// the REAL store rather than through the shape, so the omission this case is built on does
	// not hide the thing being measured.
	noIndexIsUsedTwice(t, world, groupId)
}

// noIndexIsUsedTwice holds §5.6's property directly against the server's own rows: no
// (sender_handle, stream_index) pair appears more than once in this group's records.
//
// IT TAKES NO sender_handle, which is why it exists beside [streamIndicesOf]: a case whose server
// hands the CLIENT nothing has no opened foreign message to read a handle off, and the property is
// about every sender rather than about one.
func noIndexIsUsedTwice(t *testing.T, world *world, groupId []byte) {
	t.Helper()
	records := world.allRows(t, groupId)
	// THE CONTROL THIS CLAUSE DID NOT HAVE. It is an absence -- no (sender, index) pair appears
	// twice -- and an absence over an empty row set is satisfied by every build there is. It ran
	// for as long as F0 has been landed over the founding commit alone, one row, one pair.
	if len(records) == 0 {
		t.Fatal("the server holds no rows for this group, so 'no index is used twice' is vacuous")
	}
	type key struct {
		sender string
		index  uint64
	}
	seen := map[key]uint64{}
	for _, row := range records {
		at := key{sender: string(row.SenderHandle), index: row.StreamIndex}
		if first, found := seen[at]; found {
			t.Fatalf("sender %x used stream index %d twice, in records %d and %d",
				row.SenderHandle, row.StreamIndex, first, row.RecordId)
		}
		seen[at] = row.RecordId
	}
}

// A COPY IS NOT RECONCILED OVER A WALK THAT LOST A RECORD.
//
// ONE BENT CIPHERTEXT IS ENOUGH, AND IT IS NOT ONLY A HOSTILE-SERVER STORY. A record that fails to
// open transiently -- a body a middlebox chewed, a truncated response -- is exactly what
// `maxRecordAttempts` exists for, and this build re-fetches it on the next Receive. So the
// reconciliation only ever had to WAIT for a clean walk, and it did not.
//
// WHY A LOST RECORD MATTERS AT ALL, since the missing record here is one of this device's own: an
// index is read ONLY off a record the aead authenticated, never off a header, because a header is
// plaintext and a server could wedge any client it liked with one forged row. So a record that did
// not open contributes NO index -- and this build cannot even tell whether the lost record was its
// own. A walk with a hole in it is a walk whose missing index could be the one the check exists to
// find.
//
// WHAT WOULD GO RED WITHOUT THE FIX: `Reconciled()` is true beside [urmessage.ErrRecordOpen], and
// the copy seals at an index the original already used.
func TestACopyIsNotReconciledOverAWalkThatLostARecord(t *testing.T) {
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

	for _, text := range []string{"bob 1", "bob 2"} {
		if _, err := bobGroup.Send(ctx, text); err != nil {
			t.Fatalf("bob's Send: %v", err)
		}
	}
	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)
	third, err := bobGroup.Send(ctx, "bob 3, the record the server bends")
	if err != nil {
		t.Fatalf("bob's Send after the copy: %v", err)
	}
	// the ONE record that carries the evidence, bent on every fetch this copy makes.
	world.shaped.bend(third.RecordId, 1000)

	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]

	_, err = cloneGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("the bending server was answered %v, want ErrRecordOpen; this case is not over the state it names", err)
	}
	t.Logf("the copy's reconciling Receive answered: %v", err)
	if failed := cloneGroup.Stats().FailedOpen; failed != 1 {
		t.Errorf("Stats.FailedOpen is %d, so the client did not detect the record it is being held to", failed)
	}

	if cloneGroup.Reconciled() {
		t.Fatal("the group reconciled over a walk that lost a record, so the next Send seals")
	}
	if _, err := cloneGroup.Send(ctx, "the copy speaks after a walk with a hole in it"); !errors.Is(err, urmessage.ErrNotReconciled) {
		t.Fatalf("the copy sealed after a walk with a hole in it: %v", err)
	}
}

// AND THE GATE OPENS AGAIN THE MOMENT THE WALK IS CLEAN, WHICH IS WHAT MAKES IT A GATE AND NOT A
// WEDGE.
//
// THIS IS THE CONTROL FOR THE TWO CASES ABOVE and it is the one that would catch the obvious way to
// get them passing: refuse to reconcile, ever. A device on a flaky link must not be permanently
// mute, so the transient here is BOUNDED -- one bend, then the same record fetched again is the
// record -- and the ordinary device on the ordinary bad network reconciles on its next Receive and
// sends.
//
// THE COST IS ALSO BOUNDED IN THE OTHER DIRECTION, which is the answer to "what about a record that
// never opens?": `fail` gives up at `maxRecordAttempts` and an ABANDONED record is resolved past
// without setting `firstFailure` on any later walk. So a permanently bent record delays the
// reconciliation by at most maxRecordAttempts+1 Receives and then stops delaying it. That is not
// asserted here -- it is a property of [urmessage.Group] rather than of the seam -- and it is why
// the gate is not a mute button.
//
// WHAT WOULD GO RED IF THE GATE NEVER OPENED: the Send below answers ErrNotReconciled for ever.
func TestAGroupReconcilesOnceTheWalkIsCleanAgain(t *testing.T) {
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

	typed, err := aliceGroup.Send(ctx, "alice's line, bent exactly once on its way to bob")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	// ONE bend. The record on the server is untouched -- the RESULT is what is bent -- so the
	// next fetch of the same record is the record.
	world.shaped.bend(typed.RecordId, 1)

	// ── an ORDINARY restart, no copy anywhere: this is a healthy device on a bad link ─────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]

	// the first walk loses the record, so the group does NOT reconcile and does NOT send.
	if _, err := bobGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRecordOpen) {
		t.Fatalf("the bent walk answered %v, want ErrRecordOpen", err)
	}
	if bobGroup.Reconciled() {
		t.Fatal("the group reconciled over the walk that lost the record")
	}
	if _, err := bobGroup.Send(ctx, "too early"); !errors.Is(err, urmessage.ErrNotReconciled) {
		t.Fatalf("the group sealed before a clean walk: %v", err)
	}

	// ── and the very next Receive is clean, so the gate opens ────────────────────────────
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the second, clean walk answered %v; a healthy device on a bad link is now mute", err)
	}
	if len(got) != 1 {
		t.Fatalf("the re-fetch of the bent record answered %v", textsOf(got))
	}
	if !bobGroup.Reconciled() {
		t.Fatal("the group did not reconcile over a clean, complete walk, so this gate is a wedge")
	}
	const afterwards = "and the device on the bad link speaks once the walk is clean"
	if _, err := bobGroup.Send(ctx, afterwards); err != nil {
		t.Fatalf("the healthy device could not send after a clean walk: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(back) != 1 || back[0].Text != afterwards {
		t.Fatalf("alice read %v", textsOf(back))
	}
}
