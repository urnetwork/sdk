package cp3b

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// ONE COPY OF THE APP-DATA FOLDER IS TWO DEVICES ON ONE IDENTITY, AND THE COPY IS STOPPED BEFORE
// IT SEALS ANYTHING.
//
// WHY THIS HAZARD IS NEW. Before the durable state store, a restarted device drew a FRESH Ed25519
// identity and a fresh MLS group, so a copied directory was harmless -- the copy was a different
// leaf with a different sender_handle and collided with nothing. Persisting the identity is the
// whole point of S2-14 and it is also what makes a folder copy dangerous: the copy is a second
// device at the SAME leaf, the SAME sender_handle, the SAME epoch and the SAME stream counter.
// Two records under one (epoch, sender_handle, stream_index) are ONE record_key and ONE nonce,
// which spec A section 5.6 calls a total break of both AEADs for that record.
//
// THE SINGLE-WRITER EXCLUSION DOES NOT REACH IT. It is held per DIRECTORY, by the operating
// system; a copy is a second directory, so both opens are granted and neither knows about the
// other. This case copies the folders while the original is LIVE and holding both of its
// exclusions, which is what a backup utility does and is what proves the exclusion is not the
// defence here.
//
// AND THE SERVER REFUSING THE DUPLICATE IS NOT THE DEFENCE EITHER. The message server answers
// REASON_STREAM_INDEX_REUSED to the second record under one index -- this comment said REGRESSED
// until it was read out of `msgrepo/store/memory.go:452`, and REGRESSED is a different gate (step
// (3)'s monotonicity check at `memory.go:610`, inside the transaction) that a clone never reaches,
// because step (0)'s idempotency probe answers first. Either way it is real defence in depth for
// the server's own rows and far too late for this: the sealing has already happened and the two
// ciphertexts exist. So what is asserted below is that the clone never SEALS, not that the server
// refused it.
//
// WHAT WOULD GO RED WITHOUT THE FIX: the clone's Send succeeds, and the server's own rows then
// hold two records for one sender at one stream index.
func TestACopiedAppDataFolderIsRefusedBeforeItSealsAnything(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	if _, err := bobGroup.Send(ctx, "bob, before the folder was copied"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}

	// ── the copy, taken while the original is running and holding both exclusions ────────
	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)

	// the original goes on being the original, which is what puts the clone behind.
	if _, err := bobGroup.Send(ctx, "bob, after the copy, from the device that made it"); err != nil {
		t.Fatalf("bob's Send after the copy: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	before := streamIndicesOf(t, world, groupId, senderHandleOf(t, aliceGroup))
	t.Logf("the server's own stream indices for bob's handle before the clone runs: %v", before)

	// ── the clone ────────────────────────────────────────────────────────────────────────
	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the clone restored %d group(s)", len(restored))
	}
	cloneGroup := restored[0]

	// A RESTORED GROUP WILL NOT SEAL BEFORE IT HAS LISTENED. This is the clause that covers a
	// copy which never reaches the server at all: the seal is the irreversible half, so it does
	// not happen until the reconciliation has.
	if _, err := cloneGroup.Send(ctx, "the clone speaks before it listens"); !errors.Is(err, urmessage.ErrNotReconciled) {
		t.Fatalf("a restored group sealed before it reconciled: %v", err)
	}

	// and now it listens, which is where it finds out.
	_, err = cloneGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the clone's reconciling Receive answered %v, want one wrapping ErrIdentityInUse", err)
	}
	t.Logf("the clone is refused by name: %v", err)
	if cloneGroup.IdentityInUse() == nil {
		t.Error("the refusal is not sticky, so the next Send will seal")
	}

	if sent, err := cloneGroup.Send(ctx, "the clone speaks after it listened"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the clone sealed record %v with err %v, and the ciphertext exists whatever the server then said", sent, err)
	}

	// THE MEASUREMENT IS ON THE SERVER'S OWN ROWS, because a client that wedged itself and
	// sealed anyway would still report a refusal here. No index is used twice and the clone
	// added nothing.
	after := streamIndicesOf(t, world, groupId, senderHandleOf(t, aliceGroup))
	if len(after) != len(before) {
		t.Fatalf("the clone was refused and the server's rows went from %v to %v", before, after)
	}
	seen := map[uint64]bool{}
	for _, index := range after {
		if seen[index] {
			t.Fatalf("stream index %d is used twice by one sender in one epoch: %v", index, after)
		}
		seen[index] = true
	}

	// ── the control: the ORIGINAL is untouched by any of this ────────────────────────────
	const afterwards = "and the device that was there first still works"
	if _, err := bobGroup.Send(ctx, afterwards); err != nil {
		t.Fatalf("the original bob's Send after the clone was refused: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(back) != 1 || back[0].Text != afterwards {
		t.Fatalf("alice read %v for the one line the original sent afterwards", textsOf(back))
	}
}

// THE CHEAPER AND MORE LIKELY VARIANT, SAME MECHANISM: THE STATE DIRECTORY CAME BACK AND THE
// STREAM DIRECTORY DID NOT.
//
// A partial backup, two configured roots, a wiped cache. The identity and the MLS state restore
// perfectly and the durable reserver answers HighWater 0, so the ratchet resumes at index 1 -- an
// index the server already holds for this sender. Before this check the device was simply BRICKED
// as a sender: every send refused by the server for ever, with a REASON naming none of this and no
// recovery path. Now it is refused at the reconciliation, by a sentence that says what happened.
func TestARestoreThatBroughtTheStateAndNotTheStreamDirectoryIsRefusedByName(t *testing.T) {
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
	if _, err := bobGroup.Send(ctx, "a line bob sent while he still had his stream directory"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}

	stateDir := bob.stateDir
	bob.kill()

	// the state directory survives; the stream directory does not.
	revived := world.durablePersona(t, "bob", stateDir, filepath.Join(t.TempDir(), "a-fresh-stream-directory"))
	if err := revived.device.Connect(ctx); err != nil {
		t.Fatalf("the revived bob's Connect: %v", err)
	}
	restored, err := revived.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the revived bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("%d group(s) came back", len(restored))
	}
	group := restored[0]
	if _, err := group.Receive(ctx); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("a device whose reserver has been rewound to zero reconciled with %v, want ErrIdentityInUse", err)
	}
	if _, err := group.Send(ctx, "and it must not seal at index 1 again"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("it sealed anyway: %v", err)
	}
	t.Logf("a half-restored device is refused by name rather than bricked: %v", group.IdentityInUse())
}

// AND THE CONTROL ON ALL OF IT: AN ORDINARY RESTART RECONCILES AND SENDS.
//
// Without this, every assertion above is equally well explained by "a restored group never sends",
// which would be a durability story that deletes the product. This is the same code path, the same
// two directories, nothing copied -- and it reconciles, does not wedge, and speaks.
func TestAnOrdinaryRestartReconcilesAndIsNotMistakenForACopy(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := bobGroup.Send(ctx, "one line before the ordinary restart"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}

	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	bobGroup = restored[0]
	if bobGroup.Reconciled() {
		t.Error("a freshly restored group says it has already reconciled, so the check it gates runs over nothing")
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted bob's reconciling Receive: %v", err)
	}
	if !bobGroup.Reconciled() {
		t.Fatal("a completed Receive did not reconcile the group, so every later Send is refused")
	}
	if err := bobGroup.IdentityInUse(); err != nil {
		t.Fatalf("an ordinary restart was read as a copy: %v", err)
	}
	const afterwards = "and the ordinary restart speaks"
	if _, err := bobGroup.Send(ctx, afterwards); err != nil {
		t.Fatalf("the restarted bob's Send: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(back) != 1 || back[0].Text != afterwards {
		t.Fatalf("alice read %v for the one line the restarted device sent", textsOf(back))
	}
}

// TWO COPIES THAT ARE EXACTLY LEVEL CONTEST ONE INDEX, AND THE SIDE THAT LOST THE RACE FINDS OUT.
//
// THIS IS THE RESIDUAL AND IT IS WRITTEN DOWN AS A TEST RATHER THAN ONLY AS PROSE. The check in
// [TestACopiedAppDataFolderIsRefusedBeforeItSealsAnything] catches every copy that is BEHIND the
// original, before it seals. A copy taken at the exact current index is behind nothing: its
// reserver's high water and the server's rows agree, so it reconciles CLEANLY, and if it seals
// before it fetches again it seals at the index the original will also seal at. One record under
// one (epoch, sender_handle, stream_index) on each side is one record_key and one nonce -- spec A
// §5.6's total break of both AEADs for that record -- and no client-side check can prevent it,
// because at the moment of the seal neither copy has any evidence the other exists.
//
// WHAT THIS BUILD DOES IS STOP IT AT ONE, AND IT STOPS IT AT THE SUBMIT RATHER THAN AT THE NEXT
// FETCH. The server accepts one of the two submissions and answers the other
// REASON_STREAM_INDEX_REUSED, which is its statement that it already holds DIFFERENT content at
// that (sender_handle, stream_index); [urmessage.Group.cloneRefusalLocked] reads that as the
// finding it is and the loser stops sealing there and then. The winner is then the only writer of
// that stream and produces no further collision.
//
// THIS CASE USED TO ASSERT THE REFUSAL AS AN ORDINARY ErrSubmitRefused AND THEN CALL Receive, and
// that ordering is why it could not see the real defect: a user types the next line, they do not
// fetch first, and until the check reached the seal path two level copies collided on EVERY index
// rather than once. [TestTwoCopiesThatKeepSendingCollideAtExactlyOneIndex] is the case that never
// fetches at all; this one keeps the fetch, because the RECEIVE arm is still what catches a copy
// that never submits.
//
// AN INDEX-ONLY CHECK COULD NOT SEE THE RECEIVE ARM, which is why [Group.ownIndices] carries the
// hash: both copies sealed at that index, so "is this an index I sealed at?" answers yes on both
// sides.
//
// WHAT WOULD CLOSE IT PROPERLY: a copy that came back under a DIFFERENT LEAF, which is an MLS
// Update commit. J1-8 is CLOSED -- `messagegroup.GroupEngine.LoadGroup` landed and a restored group
// now ingests a commit through the same handle a live one does -- so the blocker this comment used
// to name is gone. What is NOT closed is everything above the handle: the alpha still has exactly
// one epoch ([urmessage.ErrAlphaOneAdd]), nothing drives a second, and the ladders do not survive
// an epoch change (ledger item 239). **S2-28.**
func TestTwoCopiesThatAreExactlyLevelCollideOnceAndTheLoserFindsOut(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)

	// ── the copy, taken with both sides at exactly the same index ────────────────────────
	root := t.TempDir()
	cloneState := filepath.Join(root, "clone", "state")
	cloneStream := filepath.Join(root, "clone", "stream")
	copyAppData(t, bob.stateDir, cloneState)
	copyAppData(t, bob.streamDir, cloneStream)

	clone := world.durablePersona(t, "bob-clone", cloneState, cloneStream)
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]

	// IT RECONCILES CLEANLY, AND THAT IS THE HOLE: it agrees with the server and the server
	// agrees with it. This assertion is here so that the hole is a measured fact rather than a
	// paragraph -- if a later change made this wedge, the residual would be closed and this line
	// is where somebody would find out.
	if _, err := cloneGroup.Receive(ctx); err != nil {
		t.Fatalf("a copy taken at the exact current index did not reconcile cleanly: %v", err)
	}
	if err := cloneGroup.IdentityInUse(); err != nil {
		t.Fatalf("a copy taken at the exact current index was caught before it sealed, so the residual this case measures is gone: %v", err)
	}

	// ── the one collision ────────────────────────────────────────────────────────────────
	const byTheClone = "sealed by the CLONE at the contested index"
	const byTheOriginal = "sealed by the ORIGINAL at the same contested index"
	if _, err := cloneGroup.Send(ctx, byTheClone); err != nil {
		t.Fatalf("the clone's Send: %v", err)
	}
	// THE ORIGINAL SEALS AT THE SAME INDEX -- that ciphertext exists before any server has said
	// anything and nothing below is a mitigation of it -- and its SUBMISSION is where it finds
	// out. The refusal must be the identity finding and not a bare ErrSubmitRefused: a
	// non-sticky refusal is what let the next Send seal at the next index and collide there too.
	if _, err := bobGroup.Send(ctx, byTheOriginal); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the original's colliding Send answered %v, want ErrIdentityInUse", err)
	}
	if bobGroup.IdentityInUse() == nil {
		t.Fatal("the loser's refusal is not sticky, so its next Send seals again")
	}
	indices := streamIndicesOf(t, world, groupId, bobHandle)
	t.Logf("the server's rows for this sender after the collision: %v (it stored one of the two)", indices)

	// ── and its next Receive says the same thing rather than a different one ──────────────
	_, err = bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the side that lost the race answered %v at its next Receive, want ErrIdentityInUse", err)
	}
	t.Logf("the loser finds out: %v", err)
	if _, err := bobGroup.Send(ctx, "and the loser must not seal again"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the loser sealed again: %v", err)
	}

	// and the WINNER carries on, which is correct: it is now the only writer of this stream, so
	// it produces no further collision. A build that wedged both would have turned one bad
	// record into a dead conversation.
	if _, err := cloneGroup.Send(ctx, "the winner is the only writer now"); err != nil {
		t.Fatalf("the side whose record the server took was wedged too: %v", err)
	}
}

// AND A COPY THAT RECONCILES CLEANLY AND THEN LISTENS, RATHER THAN SPEAKING, IS CAUGHT WITH NO
// COLLISION AT ALL.
//
// Same copy, same clean reconciliation -- and this time the ORIGINAL speaks first. The copy then
// meets a record under its own sender_handle at an index it never sealed, which is the other half
// of the post-reconciliation check and the half that costs nothing: no ciphertext was produced by
// the copy, so there is no two-time pad, and the copy is stopped before it can make one.
func TestACopyThatListensBeforeItSpeaksIsCaughtWithNoCollisionAtAll(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)

	root := t.TempDir()
	copyAppData(t, bob.stateDir, filepath.Join(root, "clone", "state"))
	copyAppData(t, bob.streamDir, filepath.Join(root, "clone", "stream"))
	clone := world.durablePersona(t, "bob-clone",
		filepath.Join(root, "clone", "state"), filepath.Join(root, "clone", "stream"))
	if err := clone.device.Connect(ctx); err != nil {
		t.Fatalf("the clone's Connect: %v", err)
	}
	restored, err := clone.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the clone's Restore: %v", err)
	}
	cloneGroup := restored[0]
	if _, err := cloneGroup.Receive(ctx); err != nil {
		t.Fatalf("the clone's reconciling Receive: %v", err)
	}

	// the ORIGINAL speaks, at the index both of them would have used.
	if _, err := bobGroup.Send(ctx, "the original speaks while the copy is listening"); err != nil {
		t.Fatalf("the original's Send: %v", err)
	}
	before := streamIndicesOf(t, world, groupId, bobHandle)

	if _, err := cloneGroup.Receive(ctx); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the copy's Receive answered %v, want ErrIdentityInUse", err)
	}
	t.Logf("caught with no ciphertext produced by the copy: %v", cloneGroup.IdentityInUse())
	if _, err := cloneGroup.Send(ctx, "the copy must not seal"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the copy sealed anyway: %v", err)
	}
	after := streamIndicesOf(t, world, groupId, bobHandle)
	if len(after) != len(before) {
		t.Fatalf("the copy added rows: %v -> %v", before, after)
	}
	// and no index is used twice, which is the property all of this is for.
	seen := map[uint64]bool{}
	for _, index := range after {
		if seen[index] {
			t.Fatalf("stream index %d is used twice by one sender in one epoch: %v", index, after)
		}
		seen[index] = true
	}
}

// copyAppData copies one app-data directory the way a backup utility does: everything but the
// guard entry, which the operating system is holding open with no sharing while the original runs.
//
// THE GUARD IS SKIPPED BY NAME AND NOT BY "WHATEVER WOULD NOT OPEN", which matters: a copier that
// swallowed every unreadable file would quietly produce a partial copy and the case over it would
// be about a directory nobody can describe. Anything else this cannot read fails the test.
func copyAppData(t *testing.T, from string, to string) {
	t.Helper()
	copied := 0
	skipped := []string{}
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Name() == "single-writer.lock" {
			// both stores name their guard this, and both hold it open exclusively. It
			// carries nothing: nothing is written into it and nothing is read out of it.
			skipped = append(skipped, relative)
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		copied += 1
		return os.WriteFile(target, content, 0o600)
	})
	if err != nil {
		t.Fatalf("copying %s to %s: %v", from, to, err)
	}
	if copied == 0 {
		t.Fatalf("copying %s copied no file at all, so the case below is over an empty directory", from)
	}
	t.Logf("copied %d file(s) of %s, skipping the guard entries %v", copied, filepath.Base(from),
		strings.Join(skipped, " "))
}
