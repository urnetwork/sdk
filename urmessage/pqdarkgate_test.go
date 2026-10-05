// A GROUP THAT GOES DARK IS DARK FOR EVER, AND THE CODE IS NOW HELD TO SAYING SO.
//
// WHAT THIS FILE IS. Three production sentences claimed that a dark group repairs itself -- the
// resolution's own header ("nobody's fault and resolves itself at the next commit"), errors.go's
// ErrOrphanWrap ("IT IS NOT A FAULT AND IT REPAIRS ITSELF"), and cgo's wrap_orphaned doc ("which
// repairs itself"). All three were FALSE rather than merely unmeasured, and the third is in a
// different package from the first two, which is why the prose gate below does not read a file by
// name: a gate scoped to a bug's current address is one this project has already been bitten by.
//
// AND THE DIAGNOSIS IS NOW DURABLE, which is ruling 38 surviving a restart: a device that went
// dark used to come back with a pq_secret no peer agrees with, no sentence anywhere, and a
// pq_secret table that reads as healthy -- pqSecretsShowRotation compares OCTETS and the fallback
// wrote the same octets as the epoch below, so nothing in the table says anything is wrong.
//
// AND A RECEIVER DOES NOT FOLLOW A REMOVAL ONTO A SECRET IT HAS HELD, which is item 243's own
// property arriving inverted. It was written here as a rule about the two arms that return the
// identifier `held`, and the arm that returns a WRAP CANDIDATE reaches the same value off the wire
// and had no guard: a committer that removed a leaf and fanned out the secret every member already
// had was followed by every survivor with a nil error and no dark state. The gate below was scoped
// to the identifier, printed `candidate.secret` in its own complement, and passed. Both halves are
// repaired here -- the rule is on the VALUE at one exit, and the gate is on the SHAPE of every
// return that can carry one, asserted rather than printed.
//
// AND THE HEADING OF THIS FILE USED TO READ "A REMOVAL MAY NOT BE FOLLOWED ON A SECRET THIS GROUP
// ALREADY HOLDS", WHICH IS A GROUP PROPERTY NOTHING HERE DELIVERS -- LEDGER RULINGS 42-45. The
// subject that is actually checked is ONE RECEIVER's own history ([Group.pqSecretWitness]);
// [Device.Join] files one row, so a member admitted later refuses strictly less, and if every
// survivor joined after the reused epoch then NOBODY refuses. The rule is kept and the promise is
// corrected: the three things it does not deliver are written out at [refuseRemovalOnHeldSecret]
// and held there, as prose and by class, by
// TestTheRemovalRuleIsDocumentedAsAReceiverPropertyAndNeverAsAGroupOne in section 7.
//
// AND ITEM 251's RULING 41: an unrotated removal is an INVALID COMMIT, refused the way an
// unauthorized one is -- the receiver stays at epoch n and does NOT go dark. Refused-and-halted
// and valid-and-dark are two outcomes, separately reachable, separately named and separately
// tested below.
//
// ── AND WHAT THE 2026-09-24 FIX PASS FOUND, WHICH IS THREE MORE OF THE SAME CLASS ────────────
//
//  1. THE RULE'S SUBJECT WAS STILL A SET THAT SHRINKS. It was the LIVE pq_secret table, and the
//     window prunes that table at 32 epochs -- local hygiene a removed member does not run. After
//     33 honest rotations a removal fanned out on pq_secret[1] was followed with a nil error.
//     [Group.pqSecretWitness] is the set that does not shrink and it is persisted;
//     TestARemovalFannedOutOnAnEvictedEpochsSecretIsRefusedToo is the reproduction.
//  2. RULING 41's SECOND OUTCOME WAS UNREACHABLE FOR A REMOVAL. The pre-apply refusal fired on the
//     ABSENCE of a wrap candidate, and an absence is what an honest rotated removal looks like to
//     the member whose wrap was omitted -- so both of the cases the ruling calls valid-and-dark
//     landed in refused-and-halted under a sentence that was false about them.
//     TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap.
//  3. REFUSED-AND-HALTED WAS ONE WALK DEEP. The sentinel was lost on the first retry, the record
//     was abandoned on the third, the cursor moved past the commit and every later walk answered
//     nil over a disk reading healthy. [Group.halted] is sticky and persisted now, and
//     TestARefusedRemovalHaltsTheGroupForEveryLaterWalkAndAcrossARestart walks it five times.
//
// AND THE GATE BELOW WAS STILL SCOPED TO A SPELLING, one level further down: it skipped every
// return whose result list was empty, so a fourth arm under NAMED RESULTS with a bare `return` was
// outside its subject by construction and passed. Its subject is now held against an independent
// count of every return statement, the signature's results are asserted UNNAMED, and the floor is
// a written disposition of the arms rather than the number 3.
//
// ── AND WHAT THE SECOND 2026-09-24 PASS FOUND: TWO REPAIRS THAT WERE SCOPED TO ONE PAGE ──────
//
//  4. "AN ABSENCE IS LET PAST" WAS SPELLED `len(candidates) == 0`, so it was let past only on a
//     page with nothing else on it. One extra wrap record -- addressed to the victim's own handle,
//     sealed to the victim's own leaf key, carrying pq_secret[n], all three of which any member can
//     produce -- put the honest committer back under the removal sentinel and HALTED the receiver
//     for ever, on a VALID commit. So a permanent halt rested on what a bystander wrote. The clause
//     is gone rather than narrowed and the argument is in [Group.refuseUnrotatedRemovalLocked]: a
//     set of staged candidates is not evidence about a commit. Reproduced in both spellings by
//     TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap.
//  5. "EVERY ARM COUNTS THE ORPHAN" WAS TRUE OF THE ARMS THE CANDIDATE LOOP REACHES. The two that
//     return above it -- the digest-less commit, which is every group on the deployed alpha, and
//     the epoch-mismatch arm -- still read zero, and [Stats.WrapUnreadable] had the same defect in
//     a third dress: it sat on the arm that reports it, which the orphan arm returns ahead of. Both
//     are in a deferred block now, and TestEveryCounterTheResolutionMovesIsMovedWhereItsSubjectIsFound
//     is the gate for the RULE -- records counted where they are FOUND, epochs on the arm that
//     decides them, held both ways.
//
// ── AND WHAT THE THIRD 2026-09-24 PASS FOUND: THE GUARD CLAUSE, AND THE PROMISE ──────────────
//
//  6. THE ONE-EXIT GATE ASKED WHETHER THE REFUSAL IS CALLED, NOT WHETHER IT IS RETURNED. An exit
//     spelled `_ = refuseRemovalOnHeldSecret(...)` inside the same guard passed it with the whole
//     property gone -- reproduced before it was repaired, with the two behavioural cases red
//     beside a green gate. [removalGuardDefect] holds the RETURNED value now, and one of its
//     mutants -- the refusal returned BESIDE the secret -- is caught by that reading and by
//     nothing else in this package, which is the whole of what it is still for.
//  7. AND THE PROMISE WAS A GROUP PROPERTY THE CODE CANNOT DELIVER -- ledger rulings 42-45. It is
//     corrected everywhere it was written, and section 7's gate holds it by class rather than by
//     banned phrase: the noun doing the holding, in every production sentence about this rule.
//
// ── AND WHAT THE FIFTH 2026-09-24 PASS FOUND: THE GUARD WAS A NAME ───────────────────────────
//
//  8. THE GUARD WAS HELD BY ITS NAME AND NEVER BY ITS VALUE. `if removesLeaves` was accepted for
//     being an [ast.Ident] spelled `removesLeaves`, and the gate was handed only the exit's
//     block, so it could not read what that name was bound to even in principle. Narrowing the
//     BINDING by one token -- `0 < len(removedLeaves) && len(removedLeaves) < 3` -- left the exit
//     byte-identical, passed the gate, passed all 169 cases, and logged the same complement as
//     the correct code, with the removal rule gone for every commit removing three or more
//     leaves. THE REPAIR IS A DELETION: `pqepoch.go` no longer has the bool and the predicate is
//     written whole at the guard. Four rounds of widening a reading and one round of removing the
//     thing being read -- the adversary was the INDIRECTION, not the editor.
//
// ── AND WHAT THE SIXTH ROUND DECIDED: STOP READING, AND DRIVE THE INPUTS (LEDGER RULING 46) ───
//
//  9. THE SIXTH ROUND WAS DEFEATED IN THE CALLER, one segment further out again, and that was the
//     signal. Enforcement is a semantic property: a gate that must prove *this refusal fires for
//     every input it should* is deciding a runtime question out of syntax, and each reading it
//     adds is one more surface to route around. What the readings were standing in for is a SET
//     OF INPUTS THAT MUST BE REFUSED, and those are driven now, through the production receive
//     path, by
//     TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath
//     -- twenty-one input shapes, both doors, the arity interval [0, 4], honest rows and
//     refused rows in one function. SIX of the gate's eight clauses are DELETED, each one
//     measured being caught by a driven row instead ([removalGuardDefect]'s header carries
//     that table mutant by mutant), and the three readings that are KEPT are kept because a
//     mutant was measured PASSING the driven table: the refusal returned beside the secret,
//     a predicate narrowed one arity above what the table drives, and that predicate hidden
//     behind a NAME. One reading was ADDED while six went -- the same predicate rule asked
//     of the OTHER door, which an adversary found had no gate on its own predicate at all,
//     and where the one-arity-above narrowing was caught by nothing whatever until it
//     existed.
package urmessage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
)

// ── 1. PERMANENCE, DRIVEN RATHER THAN ASSERTED ───────────────────────────────────────────────

// A SECOND, CLEAN, COMPLETE ROTATION DOES NOT REPAIR A DARK GROUP, AND THIS IS THE CLAIM'S OWN
// COUNTEREXAMPLE FAILING TO EXIST.
//
// The deleted sentence said the orphan "resolves itself at the next commit". This case builds
// exactly that: bob's wrap for epoch 2 is bent to a payload the epoch was not opened with, so bob
// takes ErrOrphanWrap and follows on the stale secret; then alice publishes a SECOND rotation with
// nothing wrong with it, including bob's own wrap, and bob is served the whole page.
//
// THE CONTROL IS IN THE SAME LOOP AND IT FIRES FOR ITS OWN REASON: carol, who was never bent,
// takes the same two pages and follows both, so what the case measures is bob's state and not a
// harness that stopped delivering. Without it "bob is still dark" would also be satisfied by a
// world in which nothing was delivered to anybody.
func TestADarkGroupIsStillDarkAfterTheNextCleanRotation(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	first := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	}, rotBend{leaf: bob.leaf, payload: make([]byte, messagegroup.PqSecretBytes)})

	darkErr := world.deliver(bob, first.page()...)
	if !errors.Is(darkErr, ErrOrphanWrap) {
		t.Fatalf("bob's walk over the bent fan-out answered %v, want ErrOrphanWrap", darkErr)
	}
	if bob.group.wrapDark == nil {
		t.Fatalf("bob took ErrOrphanWrap and was not marked dark")
	}
	if bob.group.epoch != first.opens {
		t.Fatalf("bob stands at epoch %d after the bent rotation, want %d", bob.group.epoch, first.opens)
	}
	if err := world.deliver(carol, first.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: carol's walk over the SAME page answered %v, so the page is "+
			"broken for everybody and bob's darkness says nothing about the bend", err)
	}
	t.Logf("CONTROL HELD: the same page that made bob dark was followed by carol")

	// ── THE SECOND ROTATION, CLEAN AND COMPLETE ─────────────────────────────────────────────
	second := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	bobsWrap := 0
	for _, target := range second.targets {
		if target.leaf == bob.leaf {
			bobsWrap += 1
		}
	}
	if bobsWrap != 1 {
		t.Fatalf("CONTROL FAILED: the second fan-out addresses bob's leaf %d times, want 1; a "+
			"rotation that left bob out would prove nothing about repair", bobsWrap)
	}
	if err := world.deliver(carol, second.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: carol's walk over the second rotation answered %v, so that "+
			"rotation is not the clean one this case needs", err)
	}

	healed := world.deliver(bob, second.page()...)
	if healed == nil {
		t.Fatalf("bob's walk over the NEXT, clean, complete rotation answered nil: the deleted " +
			"sentence would be true and this file is wrong")
	}
	if !errors.Is(healed, ErrOrphanWrap) {
		t.Fatalf("bob's walk over the next rotation answered %v, want the same sticky ErrOrphanWrap", healed)
	}
	if !strings.Contains(healed.Error(), fmt.Sprintf("epoch %d", first.opens)) {
		t.Fatalf("bob's answer after the second rotation is %v, which does not name epoch %d -- "+
			"the sticky sentence is supposed to be the one taken at the epoch it went dark at",
			healed, first.opens)
	}
	if bob.group.epoch != first.opens {
		t.Fatalf("bob moved to epoch %d across the second rotation; a dark group is not supposed "+
			"to be able to follow anything", bob.group.epoch)
	}
	if carol.group.epoch != second.opens {
		t.Fatalf("CONTROL FAILED: carol stands at epoch %d after the second rotation, want %d",
			carol.group.epoch, second.opens)
	}
	if bytes.Equal(world.storageRootOf(bob), world.storageRootOf(carol)) {
		t.Fatalf("bob and carol derive one storage root, so bob is not dark at all")
	}
	if _, err := bob.group.sendableLocked(KindText); !errors.Is(err, ErrOrphanWrap) {
		t.Fatalf("bob's Send after the clean rotation is refused with %v, want the same ErrOrphanWrap", err)
	}
}

// ── 2. THE DIAGNOSIS SURVIVES THE PROCESS ────────────────────────────────────────────────────

// A DARK GROUP COMES BACK DARK, BY NAME, WITH THE EPOCH IN THE SENTENCE.
//
// bob is served the commit and NOT its wrap -- item 132's omission at the victim -- so it takes
// ErrNoWrapForEpoch and files the stale fallback at epoch 2. Then the process ends and a second
// one opens over the same disk.
//
// THE CONTROL IS carol, WHO TAKES THE WHOLE PAGE AND RESTARTS TOO: her restored group is NOT dark.
// Without it "the restored group is dark" would also be satisfied by a restore that marked every
// group dark, which is the failure in the other direction and is the one that would take the alpha
// down.
func TestADarkGroupComesBackDarkAndAHealthyOneDoesNot(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	bobLeaf, carolLeaf := bob.leaf, carol.leaf

	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	// THE OMISSION: bob is handed the commit and none of the wraps. carol is handed everything.
	darkErr := world.deliver(bob, published.commit)
	if !errors.Is(darkErr, ErrNoWrapForEpoch) {
		t.Fatalf("bob's walk over a commit with no wrap answered %v, want ErrNoWrapForEpoch", darkErr)
	}
	if err := world.deliver(carol, published.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: carol's walk over the whole page answered %v", err)
	}

	// THE DISK, BEFORE THE RESTART, because the claim is about what was WRITTEN and not about
	// what a second process reconstructs.
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("bob's disk holds %d group record(s), want 1", len(records))
	}
	if records[0].WrapDarkKind != wrapDarkNoWrap {
		t.Fatalf("bob's persisted record carries wrap_dark kind %d, want %d (ErrNoWrapForEpoch)",
			records[0].WrapDarkKind, wrapDarkNoWrap)
	}
	if records[0].WrapDarkEpoch != published.opens {
		t.Fatalf("bob's persisted record names dark epoch %d, want %d",
			records[0].WrapDarkEpoch, published.opens)
	}
	// AND THE TABLE STILL READS AS HEALTHY, which is why the kind has to be there at all. The
	// fallback wrote the SAME octets as the epoch below, so the only evidence a restart could
	// otherwise have -- two different values -- does not exist.
	table, _, err := restoredPqSecrets(records[0])
	if err != nil {
		t.Fatalf("bob's restored table: %v", err)
	}
	if pqSecretsShowRotation(table) {
		t.Fatalf("bob's persisted table reads as rotated, so this case is not measuring the " +
			"state it was written for -- the fallback is supposed to be indistinguishable in the octets")
	}

	carolRecords, err := carol.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("carol's GroupRecords: %v", err)
	}
	if len(carolRecords) != 1 || carolRecords[0].WrapDarkKind != wrapDarkNone {
		t.Fatalf("CONTROL FAILED: carol's persisted record carries wrap_dark kind %d, want 0",
			carolRecords[0].WrapDarkKind)
	}

	// ── THE RESTART ─────────────────────────────────────────────────────────────────────────
	revivedBob := restoredRotDevice(t, bob)
	restoredBob, err := revivedBob.device.restoreOne(revivedBob.store, records[0], restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over bob's dark record: %v", err)
	}
	defer restoredBob.Close()
	restoredBob.reconciled = true
	if restoredBob.wrapDark == nil {
		t.Fatalf("the restored group carries NO diagnosis although its record names dark epoch %d",
			records[0].WrapDarkEpoch)
	}
	if !errors.Is(restoredBob.wrapDark, ErrNoWrapForEpoch) {
		t.Fatalf("the restored diagnosis is %v, want an ErrNoWrapForEpoch", restoredBob.wrapDark)
	}
	if !strings.Contains(restoredBob.wrapDark.Error(), fmt.Sprintf("epoch %d", published.opens)) {
		t.Fatalf("the restored diagnosis is %v and does not name epoch %d", restoredBob.wrapDark, published.opens)
	}
	if _, err := restoredBob.sendableLocked(KindText); !errors.Is(err, ErrNoWrapForEpoch) {
		t.Fatalf("the restored group's Send is refused with %v, want ErrNoWrapForEpoch", err)
	}
	if err := restoredBob.committableLocked(); !errors.Is(err, ErrNoWrapForEpoch) {
		t.Fatalf("the restored group's commit path is refused with %v, want ErrNoWrapForEpoch", err)
	}
	_ = bobLeaf

	revivedCarol := restoredRotDevice(t, carol)
	restoredCarol, err := revivedCarol.device.restoreOne(revivedCarol.store, carolRecords[0], restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over carol's healthy record: %v", err)
	}
	defer restoredCarol.Close()
	if restoredCarol.wrapDark != nil {
		t.Fatalf("CONTROL FAILED: a group that was never dark came back dark: %v", restoredCarol.wrapDark)
	}
	_ = carolLeaf
}

// A SIX-PART RECORD RESTORES AS NOT DARK, AND A SEVENTH PART THIS BUILD DID NOT WRITE IS REFUSED.
//
// The first clause is the compatibility direction -- every disk written before this commit -- and
// the second is what keeps it from becoming "anything short is healthy": a part of the wrong
// length, or naming a kind this build does not know, refuses the group by name rather than
// answering the safest-sounding thing about a file that has been altered.
func TestTheWrapDarkPartIsOptionalAndRefusesWhatThisBuildDidNotWrite(t *testing.T) {
	base := func(parts int, dark []byte) [][]byte {
		rows := [][]byte{
			make([]byte, GroupIdBytes),
			bytes.Repeat([]byte{0x11}, messagegroup.PqSecretBytes),
			bytes.Repeat([]byte{0x22}, 32),
			{0, 0, 0, 0, 0, 0, 0, 3},
			{1},
		}
		if 6 <= parts {
			table, err := encodePqSecretTable([]EpochPqSecret{{Epoch: 3, PqSecret: bytes.Repeat([]byte{0x11}, messagegroup.PqSecretBytes)}})
			if err != nil {
				t.Fatalf("encodePqSecretTable: %v", err)
			}
			rows = append(rows, table)
		}
		if 7 <= parts {
			rows = append(rows, dark)
		}
		if 8 <= parts {
			witness, err := encodePqSecretWitness([]EpochPqSecretWitness{
				{Epoch: 1, Digest: bytes.Repeat([]byte{0x44}, sha256.Size)},
			})
			if err != nil {
				t.Fatalf("encodePqSecretWitness: %v", err)
			}
			rows = append(rows, witness)
		}
		if 9 <= parts {
			rows = append(rows, encodeLeafOccupancy([]LeafOccupancy{
				{Leaf: 1, DepartedEpoch: 3, Own: false},
				{Leaf: 2, DepartedEpoch: 0, Own: true},
			}))
		}
		// PART TEN, LEDGER RULING 52's REMOVAL, and it is EMPTY at this arity: a ten-part record with
		// an empty tenth part is what this build writes about a device that is STILL A MEMBER, which
		// is the ordinary case and belongs in this table beside the removed one.
		if 10 <= parts {
			rows = append(rows, nil)
		}
		return rows
	}
	for _, one := range []struct {
		name         string
		parts        [][]byte
		kind         uint8
		epoch        uint64
		witness      int
		leaves       int
		removed      uint8
		removedEpoch uint64
		bad          bool
	}{
		{name: "five parts: the deployed alpha's disk", parts: base(5, nil), kind: wrapDarkNone},
		{name: "six parts: written before the wrap_dark part", parts: base(6, nil), kind: wrapDarkNone},
		{name: "seven parts, empty: written before the pq_secret witness, not dark",
			parts: base(7, nil), kind: wrapDarkNone},
		{name: "seven parts, dark", parts: base(7, []byte{wrapDarkOrphan, 0, 0, 0, 0, 0, 0, 0, 3}),
			kind: wrapDarkOrphan, epoch: 3},
		{name: "eight parts, empty witness: this build, a group that has witnessed nothing",
			parts: base(8, nil), kind: wrapDarkNone, witness: 1},
		{name: "eight parts, halted", parts: base(8, []byte{wrapDarkRemoval, 0, 0, 0, 0, 0, 0, 0, 3}),
			kind: wrapDarkRemoval, epoch: 3, witness: 1},
		{name: "a wrap_dark part of the wrong width", parts: base(7, []byte{wrapDarkOrphan}), bad: true},
		{name: "a wrap_dark kind this build does not name",
			parts: base(7, []byte{0x7f, 0, 0, 0, 0, 0, 0, 0, 3}), bad: true},
		// THE WITNESS PART'S OWN REFUSAL, in the same table as the wrap_dark one's: a row is
		// u64(epoch) ‖ 32 octets and there is no length prefix, so a tail of any other width is a
		// record this build did not write and must not be read as a witness that ends early.
		{name: "a pq_secret witness row of the wrong width",
			parts: append(base(7, nil), bytes.Repeat([]byte{0x44}, 8+16)), bad: true},
		// PART NINE, LEDGER ITEM 245's LEAF LEDGER, and its three shapes in this same table for
		// the witness part's reason: the arity is what tells an old disk from a new one, so each
		// arity has to be driven here or the compatibility claim is a sentence.
		{name: "nine parts, empty ledger: this build, a group that has removed nobody and whose " +
			"caller supplied no rows", parts: append(base(8, nil), nil),
			kind: wrapDarkNone, witness: 1},
		{name: "nine parts, a ledger", parts: base(9, nil), kind: wrapDarkNone, witness: 1, leaves: 2},
		{name: "a leaf ledger row of the wrong width",
			parts: append(base(8, nil), bytes.Repeat([]byte{0x55}, leafOccupancyRowBytes-1)), bad: true},
		// A FLAGS OCTET THIS BUILD DOES NOT DEFINE. Part nine carries one bit today and a later
		// build's second bit must arrive as a named refusal rather than as a leaf whose ownership
		// this build has silently mis-read -- a leaf wrongly marked `own` is this device claiming
		// another member's records, which is exactly the defect part nine exists to close.
		{name: "a leaf ledger row with a flag this build does not define",
			parts: append(base(8, nil), []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 3, 0x02}), bad: true},
		// PART TEN, LEDGER RULING 52's REMOVAL, in this same table for part nine's reason: the
		// arity is what tells an old disk from a new one, so each arity is driven here or the
		// compatibility claim is a sentence. "Ten parts" USED TO BE THE `bad` ROW in this table,
		// which is what an arity switch looks like the commit before it moves.
		{name: "ten parts, empty removal: this build, a device that is still a member",
			parts: base(10, nil), kind: wrapDarkNone, witness: 1, leaves: 2, removed: removedNone},
		{name: "ten parts, removed", parts: append(base(9, nil), []byte{removedByCommit, 0, 0, 0, 0, 0, 0, 0, 3}),
			kind: wrapDarkNone, witness: 1, leaves: 2, removed: removedByCommit, removedEpoch: 3},
		// AND EPOCH ZERO, which is why that part carries a kind octet rather than keying "not
		// removed" off a zero epoch the way part nine's DepartedEpoch does: this field holds the
		// epoch a device was STANDING at, and a founder stands at epoch zero.
		{name: "ten parts, removed at epoch zero", parts: append(base(9, nil), []byte{removedByCommit, 0, 0, 0, 0, 0, 0, 0, 0}),
			kind: wrapDarkNone, witness: 1, leaves: 2, removed: removedByCommit, removedEpoch: 0},
		{name: "a removal part of the wrong width",
			parts: append(base(9, nil), []byte{removedByCommit, 0, 0, 0, 0}), bad: true},
		{name: "a removal kind this build does not name",
			parts: append(base(9, nil), []byte{0x7f, 0, 0, 0, 0, 0, 0, 0, 3}), bad: true},
		{name: "eleven parts", parts: append(base(10, nil), nil), bad: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			record, err := groupRecordOf("a-group", one.parts)
			if one.bad {
				if err == nil {
					t.Fatalf("a record this build did not write decoded to %+v", record)
				}
				if !errors.Is(err, ErrStateStoreFormat) {
					t.Fatalf("the refusal is %v, want an ErrStateStoreFormat", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("groupRecordOf: %v", err)
			}
			if record.WrapDarkKind != one.kind || record.WrapDarkEpoch != one.epoch {
				t.Fatalf("decoded kind %d epoch %d, want kind %d epoch %d",
					record.WrapDarkKind, record.WrapDarkEpoch, one.kind, one.epoch)
			}
			if wrapDarkErrorOf(record.WrapDarkKind, record.WrapDarkEpoch) == nil && one.kind != wrapDarkNone {
				t.Fatalf("a record naming kind %d rebuilt no diagnosis", one.kind)
			}
			if len(record.PqSecretWitness) != one.witness {
				t.Fatalf("decoded %d witness row(s), want %d: a record written before that part "+
					"witnesses NOTHING and one written by this build witnesses what it carries; "+
					"collapsing the two would invent a witness for a disk that has none",
					len(record.PqSecretWitness), one.witness)
			}
			if len(record.Leaves) != one.leaves {
				t.Fatalf("decoded %d leaf ledger row(s), want %d: a record written before part "+
					"nine carries NO ledger and one written by this build carries what it was "+
					"given; collapsing the two would invent an occupancy table for a disk that "+
					"has none, and [Device.restoreOne] acts on that distinction",
					len(record.Leaves), one.leaves)
			}
			// AND RULING 52's PART, THE SAME WAY: a record written before it says nothing about a
			// removal, and one written by this build says what it was given. Collapsing the two
			// would hand a restore a device that reads as a member because its disk is old.
			if record.RemovedKind != one.removed || record.RemovedEpoch != one.removedEpoch {
				t.Fatalf("decoded removal kind %d at epoch %d, want kind %d at epoch %d",
					record.RemovedKind, record.RemovedEpoch, one.removed, one.removedEpoch)
			}
			if (removedErrorOf(record.RemovedKind, record.RemovedEpoch) == nil) != (one.removed == removedNone) {
				t.Fatalf("a record naming removal kind %d rebuilt the wrong state", one.removed)
			}
			if one.leaves == 2 {
				// AND THE ROWS COME BACK AS THEY WENT IN, BOTH FIELDS, BOTH VALUES. A decoder
				// that read the flags octet as the low byte of the epoch, or that dropped the
				// `own` bit, would pass the count above and would hand a restore a device that
				// does not recognise its own handle.
				want := []LeafOccupancy{
					{Leaf: 1, DepartedEpoch: 3, Own: false},
					{Leaf: 2, DepartedEpoch: 0, Own: true},
				}
				if !slices.Equal(record.Leaves, want) {
					t.Fatalf("the leaf ledger decoded %+v, want %+v", record.Leaves, want)
				}
			}
			// AND THE HALT COMES BACK AS A HALT, IN THE FIELD IT WAS WRITTEN FROM. One kind
			// column, two fields: a kind that is the removal refusal restores as [Group.halted]
			// and NEVER as [Group.wrapDark], and every other kind the other way round.
			dark, halted := restoredDiagnosisOf(record.WrapDarkKind, record.WrapDarkEpoch)
			if one.kind == wrapDarkRemoval {
				if halted == nil || dark != nil {
					t.Fatalf("kind %d restored dark=%v halted=%v; ruling 41's refusal is a HALT "+
						"and restoring it as a dark state is this build reading back a diagnosis "+
						"it did not write", one.kind, dark, halted)
				}
				if !errors.Is(halted, ErrRemovalWithoutRotation) {
					t.Fatalf("the restored halt is %v, want ErrRemovalWithoutRotation", halted)
				}
			} else if halted != nil {
				t.Fatalf("kind %d restored a HALT (%v); only the removal refusal is one", one.kind, halted)
			}
		})
	}
}

// ── 3. THIS RECEIVER DOES NOT FOLLOW A REMOVAL ONTO A SECRET THIS RECEIVER HAS HELD ──────────

// EVERY CASE IN THIS SECTION IS A RECEIVER THAT DID HOLD THE REPLAYED VALUE, AND THAT IS THE
// PROPERTY'S SCOPE RATHER THAN A CONVENIENCE OF THE FIXTURE. Every case below founds its world
// with [newRotWorld], which admits every member in the founding commit, so every receiver in this
// section is as old as the group. A member admitted LATER holds a strictly smaller history
// ([Device.Join] files one row) and answers *not held* for the same octets; that is ledger ruling
// 43's residual, it is a theorem rather than a bug, and it IS driven -- by the `late-joiner` row
// of TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath,
// over [rotWorld.admit], which admits a member after the founding commit. The sentence that stood
// here said this harness has no such door and that nothing drives the residual; both stopped being
// true when that row landed. Ruling 42's clause (b) -- *a receiver that itself held the reused
// secret refuses and halts* -- is what this section holds, and it is the whole of what it holds.

// THE COMMIT SHAPE EVERY BUILD BEFORE THIS ONE EMITTED, REFUSED: a CommitRemove whose digest was
// computed over the HELD secret.
//
// THE COUNTERFACTUAL IS THE FINDING AND IT IS ASSERTED, not described: the removed member's
// retained pq_secret, mixed with the exporter of the epoch it was removed at, reproduces the
// committer's own storage root for that epoch. That is item 243's whole subject. So the refusal is
// not a fussy rule -- it is the only thing standing between this build and a removal that removes
// nothing.
//
// AND IT IS REFUSED AT THE RESOLUTION AND NOT BEFORE THE APPLY, WHICH THIS CASE MEASURES RATHER
// THAN ARGUES. This shape carries a digest, and every removal that carries one is judged at the
// resolution -- because nothing available before ApplyCommit is a fact about the COMMIT rather than
// about what reached this device, and a removal with no fan-out is in any case indistinguishable
// there from an honest rotated removal whose wrap was omitted at this victim, which is ruling 41's
// OTHER outcome and must go dark rather than be refused (see
// TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap). So what the group
// does not do is FOLLOW it: the epoch does not move, no secret is filed for the epoch it opens,
// the halt is persisted, and Send and Commit answer it by name. What HAS moved is the MLS handle,
// and that is asserted as the residual. The shape an older build actually emits carries no digest
// at all and is still refused before the apply.
func TestARemovalThatDoesNotRotateIsRefusedAndTheGroupDoesNotFollowIt(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	retained := append([]byte(nil), carol.group.pqSecretLocked()...)
	published := world.advanceWithoutRotating(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	})
	if published.opens != 2 {
		t.Fatalf("the removal opens epoch %d, want 2", published.opens)
	}

	// ── THE COUNTERFACTUAL, FIRST, so the refusal below is about something ──────────────────
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-2 exporter: %v", err)
	}
	committerRoot := messagegroup.StorageRoot(granted, published.pqSecret)
	withRetained := messagegroup.StorageRoot(granted, retained)
	if !bytes.Equal(withRetained, committerRoot) {
		t.Fatalf("CONTROL FAILED: this fixture's removal DID rotate, so it is not the shape this " +
			"case refuses and the refusal below would be about nothing")
	}
	t.Logf("the removed member's retained pq_secret reproduces the committer's storage_root[2]: " +
		"that is what the refusal below prevents this group from following")

	// ── THE REFUSAL ─────────────────────────────────────────────────────────────────────────
	refused := world.deliver(bob, published.page()...)
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over an unrotated removal answered %v, want ErrRemovalWithoutRotation", refused)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob stands at epoch %d after refusing the removal, want 1: the refusal is "+
			"supposed to run BEFORE ApplyCommit", bob.group.epoch)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark over a commit it refused before applying: %v", bob.group.wrapDark)
	}
	if !bytes.Equal(bob.group.pqSecretLocked(), retained) {
		t.Fatalf("bob's pq_secret moved although it did not follow the commit")
	}
	if held, isHeld := bob.group.pqSecretAtLocked(published.opens); isHeld {
		t.Fatalf("bob filed a pq_secret for epoch %d (%d octets) although it refused the commit "+
			"that opens it", published.opens, len(held))
	}
	// ── WHERE THE REFUSAL IS TAKEN, AND IT IS THE RESIDUAL AND NOT THE PRE-APPLY ARM ────────
	//
	// This shape is a removal with NO fan-out at all and a digest that names the held secret, and
	// nothing available before ApplyCommit tells it apart from an HONEST rotated removal whose
	// wrap was omitted at this victim -- both are "a removal, a digest, and no candidate". Judging
	// it needs mls_secret[n+1], so it is judged at the resolution and the MLS handle has moved by
	// then. That is asserted rather than glossed. What stays pre-apply is the shape an OLDER BUILD
	// actually emits, which carries no digest at all: see
	// TestADigestLessRemovalIsRefusedBeforeTheApplyAndOnlyANonRemovingOneReachesTheNoDigestArm, where the handle does not move.
	if bob.handle.Epoch() != published.opens {
		t.Fatalf("bob's MLS handle stands at epoch %d, want %d; if the pre-apply refusal now covers "+
			"a removal with no fan-out, move this case and say what evidence it uses",
			bob.handle.Epoch(), published.opens)
	}
	// ── THE HALT IS STICKY, PERSISTED AND ANSWERED BY SEND AND BY COMMIT ────────────────────
	if _, err := bob.group.sendableLocked(KindText); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's Send answers %v; a halted group stands at an epoch the server has already "+
			"left, so leaving Send open hands the user the undiagnosable REASON_REJECTED by another "+
			"road", err)
	}
	if err := bob.group.committableLocked(); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's Commit answers %v, want the halt", err)
	}
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(records) != 1 || records[0].Epoch != 1 || records[0].WrapDarkKind != wrapDarkRemoval {
		t.Fatalf("bob's disk names epoch %d and kind %d, want 1 and %d: a refusal that halts a "+
			"group has to be at least as durable as the dark state it is contrasted with",
			records[0].Epoch, records[0].WrapDarkKind, wrapDarkRemoval)
	}

	// ── THE POSITIVE CONTROL: a removal that DOES rotate is followed ────────────────────────
	//
	// In a second world, because bob above is now behind alice and cannot be handed anything.
	// Without this clause the rule would be satisfied by a build that refuses every removal.
	clean := newRotWorld(t, "alice", "bob", "carol")
	cleanAlice, cleanBob, cleanCarol := clean.member("alice"), clean.member("bob"), clean.member("carol")
	rotated := clean.rotate(cleanAlice, func() ([]byte, []byte, []byte, error) {
		return cleanAlice.handle.CommitRemove([]uint32{cleanCarol.leaf})
	})
	if err := clean.deliver(cleanBob, rotated.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: a removal that DOES rotate was refused with %v, so the rule "+
			"refuses everything and proves nothing", err)
	}
	if cleanBob.group.epoch != rotated.opens {
		t.Fatalf("CONTROL FAILED: bob stands at epoch %d after a clean removal, want %d",
			cleanBob.group.epoch, rotated.opens)
	}
}

// THE SHAPE THAT DEFEATED ITEM 243's OWN PURPOSE: a removal with a COMPLETE, OPENABLE FAN-OUT
// CARRYING THE SECRET THE GROUP ALREADY HELD.
//
// WHAT IT IS. The committer removes carol and writes one well-formed device wrap per survivor,
// each sealed to that survivor's own published X-Wing key and each carrying pq_secret[1] -- the
// value carol holds by construction. Every wrap OPENS. So the resolution's FIRST arm, the
// wrap-candidate arm, reproduces the commit's digest and answers that candidate -- which is where
// this page is refused. Until this commit that arm carried no removal guard at all: it returned
// `candidate.secret, nil`, every survivor followed with no error, no dark state and no refusal,
// and carol's retained secret reproduced their storage_root[2] exactly.
//
// THE COUNTERFACTUAL IS ASSERTED AND NOT DESCRIBED, first, so the refusal below is about
// something: the removed member's retained pq_secret, mixed with the exporter of the epoch it was
// removed at, reproduces the committer's own storage root for that epoch.
//
// THREE CONTROLS, ALL INLINE, EACH FIRING FOR ITS OWN REASON:
//
//  1. THE FAN-OUT IS REAL. bob's wrap is in the page, it OPENS, and the candidate it stages is the
//     one the digest names -- asserted through Stats.WrapOpened and the staged candidate itself.
//     Without it "bob refused" would also be satisfied by a page bob could not read, which is a
//     different sentinel and a different bug.
//  2. THE HONEST ROTATED REMOVAL, in the same test, in a second world: it is FOLLOWED. Without it
//     the rule would be satisfied by a build that refuses every removal, which removes the feature
//     rather than the member.
//  3. THE GROUP DOES NOT FOLLOW IT, asserted by the epoch, by the table, by the absence of a dark
//     state and by Send and Commit refusing by name -- which is ruling 41: an unrotated removal is
//     an INVALID commit and is refused the way an unauthorized one is. It does not advance into a
//     permanent brick on a commit it has just judged invalid.
//
// AND WHERE THE REFUSAL IS TAKEN MOVED ON 2026-09-24, WHICH IS A WEAKENING IN ONE AXIS AND A
// STRENGTHENING IN ANOTHER, BOTH ASSERTED HERE. It used to be taken BEFORE ApplyCommit, by a clause
// that asked whether every candidate staged for the epoch carried a held value. That clause was
// removed, because the candidate set is not evidence about the commit -- one decoy from a bystander
// plus one ordinary delivery failure at the victim made it refuse an HONEST removal and halt the
// receiver for ever (TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap).
// So this page is now refused at the resolution, against the commit's OWN authenticated digest, and
// the MLS handle has moved by then: that is asserted as the residual rather than glossed. The
// STRENGTHENING is the second world at the bottom -- the same unrotated fan-out with a FRESH decoy
// beside it, which the deleted clause was structurally unable to refuse and which the digest
// comparison refuses without noticing the difference.
func TestARemovalFannedOutOnTheHeldSecretIsRefusedAndTheGroupStaysAtItsEpoch(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	retained := append([]byte(nil), carol.group.pqSecretLocked()...)
	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{})
	if published.opens != 2 {
		t.Fatalf("the removal opens epoch %d, want 2", published.opens)
	}

	// ── THE COUNTERFACTUAL ──────────────────────────────────────────────────────────────────
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-2 exporter: %v", err)
	}
	if !bytes.Equal(messagegroup.StorageRoot(granted, retained), messagegroup.StorageRoot(granted, published.pqSecret)) {
		t.Fatalf("CONTROL FAILED: this fixture's removal DID rotate, so it is not the shape this " +
			"case refuses and the refusal below would be about nothing")
	}
	t.Logf("the REMOVED member's retained pq_secret reproduces the committer's storage_root[2]; " +
		"the fan-out that delivers it is complete and every wrap opens")

	// ── CONTROL 1: THE FAN-OUT IS REAL, measured on the receiver before the commit is met ───
	//
	// The wraps are delivered on their own, so what is asserted is that they OPENED -- a page in
	// which bob simply could not read anything would produce the same refusal below for an
	// entirely different reason.
	if err := world.deliver(bob, published.wraps...); err != nil {
		t.Fatalf("CONTROL FAILED: bob's walk over the fan-out alone answered %v", err)
	}
	if opened := bob.group.Stats().WrapOpened; opened != 1 {
		t.Fatalf("CONTROL FAILED: bob opened %d wrap(s) of this fan-out, want 1; a refusal below "+
			"would then be about a wrap that did not arrive and not about the value it carries", opened)
	}
	staged := bob.group.wrapsFor[published.opens]
	if len(staged) != 1 || !bytes.Equal(staged[0].secret, retained) {
		t.Fatalf("CONTROL FAILED: bob staged %d candidate(s) for epoch %d and this case needs exactly "+
			"one carrying the value the removed member holds", len(staged), published.opens)
	}

	// ── THE REFUSAL ─────────────────────────────────────────────────────────────────────────
	refused := world.deliver(bob, published.commit)
	if refused == nil {
		t.Fatalf("bob FOLLOWED a removal fanned out on the secret the removed member holds. carol's "+
			"retained pq_secret reproduces bob's storage_root[%d] and the removal removed nothing",
			published.opens)
	}
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over the unrotated fan-out answered %v, want ErrRemovalWithoutRotation", refused)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob stands at epoch %d after refusing the removal, want 1: ruling 41 refuses an "+
			"invalid commit the way an unauthorized one is refused, and the receiver stays at n", bob.group.epoch)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went DARK over a commit it refused: %v. Ruling 41 is that the two are "+
			"different outcomes -- refused-and-halted is not valid-and-dark", bob.group.wrapDark)
	}
	// WHAT "DID NOT FOLLOW IT" IS ASSERTED ON, AND IT IS NO LONGER THE STORAGE ROOT. That comparison
	// read this member's own exporter, which is the MLS HANDLE's, so it conflated "bob did not
	// follow the commit" with "bob's handle did not move" -- one property standing in for two. The
	// two are now asserted separately and both by name: the post-quantum half is unchanged and no
	// row was filed for the epoch the commit opens (here), and the handle DID move (below, as the
	// residual).
	if !bytes.Equal(bob.group.pqSecretLocked(), retained) {
		t.Fatalf("bob's pq_secret moved although it did not follow the commit")
	}
	if held, isHeld := bob.group.pqSecretAtLocked(published.opens); isHeld {
		t.Fatalf("bob filed a pq_secret for epoch %d (%d octets) although it refused the commit that "+
			"opens it", published.opens, len(held))
	}
	// ── AND THE HALT IS A STATE AND NOT ONE SENTENCE ────────────────────────────────────────
	//
	// Send used to be asserted OPEN here, on the reading that "a group that halted is still a
	// working group at the epoch it is at". That reading is false and the falseness is
	// measurable: the server accepted the commit, so its current_epoch is n+1 while this device's
	// write_auth is MAC'd under write_key[n], and every send is REASON_REJECTED with nothing to
	// read behind it. The refusal by NAME is what replaces it, and with it the three things that
	// make a halt as durable as the dark state ruling 41 contrasts it with: Commit refuses too,
	// the kind reaches the disk, and the next walk over the same record answers the same sentence
	// instead of a ratchet error (TestARefusedRemovalHaltsTheGroupForEveryLaterWalkAndAcrossARestart).
	if _, err := bob.group.sendableLocked(KindText); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's Send answers %v, want the halt: a halted group stands at an epoch the "+
			"server has already left", err)
	}
	if err := bob.group.committableLocked(); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's Commit answers %v, want the halt", err)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("the halt was written into wrapDark (%v); ruling 41's two outcomes are two "+
			"fields, and one field would make them one state with two names", bob.group.wrapDark)
	}
	haltRecords, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(haltRecords) != 1 || haltRecords[0].Epoch != 1 || haltRecords[0].WrapDarkKind != wrapDarkRemoval {
		t.Fatalf("bob's disk names epoch %d and kind %d, want 1 and %d",
			haltRecords[0].Epoch, haltRecords[0].WrapDarkKind, wrapDarkRemoval)
	}
	// THE RESIDUAL, ASSERTED AND NOT GLOSSED: the MLS handle HAS moved, because this refusal is
	// taken at the resolution and ApplyCommit runs before it. Judging the commit's digest needs
	// mls_secret[n+1] and there is no exporter over a PROCESSED commit, so there is no earlier
	// moment at which this page can be judged at all. The group, its session and its persisted
	// record all stand at 1 and agree with each other; the handle is at n+1 and cannot be moved
	// back. The shape that IS still refused before the apply is the digest-less removal -- an older
	// build's -- and TestADigestLessRemovalIsRefusedBeforeTheApplyAndOnlyANonRemovingOneReachesTheNoDigestArm asserts the handle
	// does not move there.
	if bob.handle.Epoch() != published.opens {
		t.Fatalf("bob's MLS handle stands at epoch %d, want %d; if a pre-apply refusal now covers "+
			"this shape, move this case and say what evidence about the COMMIT it rests on",
			bob.handle.Epoch(), published.opens)
	}

	// ── AND THE STRENGTHENING: THE SAME PAGE WITH A FRESH DECOY BESIDE IT IS REFUSED TOO ────
	//
	// WHY THIS IS STRICTLY STRONGER THAN THE CLAUSE IT REPLACES. The deleted pre-apply clause
	// refused a fan-out every one of whose candidates carried a held value; one FRESH candidate
	// beside them satisfied it and the page went past -- that was the documented residual. The
	// digest comparison does not have that shape: it asks whether the value the commit's own
	// authenticated H(epoch_keys) NAMES is one this group has held, so a decoy changes nothing. The
	// control is inline and fires for its own reason: the decoy is genuinely not a value bob holds.
	strong := newRotWorld(t, "alice", "bob", "carol")
	strongAlice, strongBob, strongCarol := strong.member("alice"), strong.member("bob"), strong.member("carol")
	strongRetained := append([]byte(nil), strongCarol.group.pqSecretLocked()...)
	fresh := make([]byte, messagegroup.PqSecretBytes)
	for at := range fresh {
		fresh[at] = 0x6F
	}
	strongPublished := strong.fanOutOnTheHeldSecret(strongAlice,
		func() ([]byte, []byte, []byte, error) {
			return strongAlice.handle.CommitRemove([]uint32{strongCarol.leaf})
		}, unrotatedFanOut{decoy: fresh})
	if _, alreadyHeld := strongBob.group.pqSecretHeldAtLocked(fresh); alreadyHeld {
		t.Fatalf("CONTROL FAILED: bob already holds the 'fresh' decoy, so this page is the one above " +
			"again and the deleted clause would have refused it too")
	}
	if len(strongPublished.decoys) != len(strongPublished.targets) {
		t.Fatalf("CONTROL FAILED: the fixture wrote %d decoy row(s) for %d target(s)",
			len(strongPublished.decoys), len(strongPublished.targets))
	}
	strongRefused := strong.deliver(strongBob, strongPublished.page()...)
	if !errors.Is(strongRefused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over an unrotated removal with ONE FRESH candidate beside its fan-out "+
			"answered %v, want ErrRemovalWithoutRotation. The clause this replaces asked whether "+
			"EVERY candidate was held, so one fresh row took the whole page past it", strongRefused)
	}
	if strongBob.group.epoch != 1 || strongBob.group.wrapDark != nil || strongBob.group.halted == nil {
		t.Fatalf("bob stands at epoch %d dark=%v halted=%v, want epoch 1, halted and not dark",
			strongBob.group.epoch, strongBob.group.wrapDark, strongBob.group.halted)
	}
	if !bytes.Equal(strongBob.group.pqSecretLocked(), strongRetained) {
		t.Fatalf("bob's pq_secret moved although it refused the commit")
	}

	// ── CONTROL 2: THE HONEST ROTATED REMOVAL IS FOLLOWED ───────────────────────────────────
	clean := newRotWorld(t, "alice", "bob", "carol")
	cleanAlice, cleanBob, cleanCarol := clean.member("alice"), clean.member("bob"), clean.member("carol")
	cleanRetained := append([]byte(nil), cleanCarol.group.pqSecretLocked()...)
	rotated := clean.rotate(cleanAlice, func() ([]byte, []byte, []byte, error) {
		return cleanAlice.handle.CommitRemove([]uint32{cleanCarol.leaf})
	})
	if err := clean.deliver(cleanBob, rotated.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: a removal that DOES rotate was refused with %v, so the new guard "+
			"refuses everything and proves nothing", err)
	}
	if cleanBob.group.epoch != rotated.opens {
		t.Fatalf("CONTROL FAILED: bob stands at epoch %d after a clean removal, want %d",
			cleanBob.group.epoch, rotated.opens)
	}
	if !bytes.Equal(cleanBob.group.pqSecretLocked(), rotated.pqSecret) {
		t.Fatalf("CONTROL FAILED: bob followed the clean removal onto a secret that is not the " +
			"epoch's own, so the control is not measuring the honest path")
	}
	if bytes.Equal(cleanBob.group.pqSecretLocked(), cleanRetained) {
		t.Fatalf("CONTROL FAILED: the clean removal's own secret IS the retained one, so the two " +
			"arms of this case are the same arm")
	}
}

// AND THE RULE HOLDS WHEN THE COMMIT REMOVES MORE THAN ONE LEAF, WHICH IS AN AXIS AND NOT A CASE.
//
// WHY THIS EXISTS, AND IT IS THE 2026-09-24 (FOURTH PASS) FINDING RATHER THAN A VARIANT. Every
// other removal case in this file removes EXACTLY ONE leaf, so `len(removedLeaves) == 1` was a
// silent premise of the whole behavioural surface. A bypass planted in the exit --
// `if len(removedLeaves) == 1 { <the refusal> }` -- is therefore TRUE on every path any of them
// drives, and all 168 cases in this package ran GREEN with it in the tree. A gate clause now
// refuses that shape by reading the statement path, but a gate clause reads TEXT: what stops the
// axis being a premise is a case that VARIES it, and this is that case. With the bypass planted
// this case goes red, so the clause and the behaviour hold one defect from two directions.
//
// connect fills `RemovedLeaves` with one entry per Remove proposal, so len == 2 is an ordinary
// commit and not a shape nobody writes: it is one ADMIN ejecting two devices at once.
func TestARemovalOfTwoLeavesFannedOutOnTheHeldSecretIsRefusedTheSameWay(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol", "dave")
	alice, bob := world.member("alice"), world.member("bob")
	carol, dave := world.member("carol"), world.member("dave")

	retained := append([]byte(nil), carol.group.pqSecretLocked()...)
	removing := []uint32{carol.leaf, dave.leaf}
	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove(removing)
	}, unrotatedFanOut{})

	// ── CONTROL 1: THE COMMIT REALLY DOES REMOVE TWO DISTINCT LEAVES ────────────────────────
	//
	// Without this, a harness that quietly dropped the second leaf would make this a slower copy
	// of the one-leaf case and the axis would still be unvaried.
	if len(removing) != 2 || removing[0] == removing[1] {
		t.Fatalf("CONTROL FAILED: this case removes %v, which is not two distinct leaves", removing)
	}
	if published.opens != 2 {
		t.Fatalf("the removal opens epoch %d, want 2", published.opens)
	}

	// ── CONTROL 2: THE FAN-OUT IS UNROTATED, so the refusal below is about the value ────────
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-2 exporter: %v", err)
	}
	if !bytes.Equal(messagegroup.StorageRoot(granted, retained), messagegroup.StorageRoot(granted, published.pqSecret)) {
		t.Fatalf("CONTROL FAILED: this fixture's removal DID rotate, so it is not the shape this " +
			"case refuses and the refusal below would be about nothing")
	}

	// ── CONTROL 3: BOB OPENS THE WRAP, so the refusal is not an absence ─────────────────────
	if err := world.deliver(bob, published.wraps...); err != nil {
		t.Fatalf("CONTROL FAILED: bob's walk over the fan-out alone answered %v", err)
	}
	if opened := bob.group.Stats().WrapOpened; opened != 1 {
		t.Fatalf("CONTROL FAILED: bob opened %d wrap(s) of this fan-out, want 1", opened)
	}

	// ── THE REFUSAL, AND IT IS THE SAME ONE ─────────────────────────────────────────────────
	refused := world.deliver(bob, published.commit)
	if refused == nil {
		t.Fatalf("bob FOLLOWED a TWO-leaf removal fanned out on the secret both removed members " +
			"hold. This is the one-leaf case's defect with the only axis this package never " +
			"varied turned, and a bypass keyed on len(removedLeaves) == 1 passes every other " +
			"case in this file")
	}
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over the two-leaf unrotated fan-out answered %v, want "+
			"ErrRemovalWithoutRotation", refused)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob stands at epoch %d after refusing the two-leaf removal, want 1", bob.group.epoch)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went DARK over a commit it refused: %v; ruling 41's two outcomes are two "+
			"states", bob.group.wrapDark)
	}
	if !bytes.Equal(bob.group.pqSecretLocked(), retained) {
		t.Fatalf("bob's pq_secret moved although it did not follow the commit")
	}
	if _, isHeld := bob.group.pqSecretAtLocked(published.opens); isHeld {
		t.Fatalf("bob filed a pq_secret for epoch %d although it refused the commit that opens it",
			published.opens)
	}
	t.Logf("a removal of %d leaves fanned out on the held secret is refused by name and the "+
		"receiver stays at epoch %d; the len(removedLeaves) axis is no longer a silent premise",
		len(removing), bob.group.epoch)
}

// AND THE RULE IS ON THE WHOLE TABLE, NOT ON THE CURRENT EPOCH'S ROW: a removal fanned out on an
// EARLIER epoch's pq_secret.
//
// THIS IS THE MUTATION OF THE MECHANISM, WRITTEN AS A CASE. The guard's subject is
// [Group.pqSecretHeldAtLocked], and the tempting narrowing -- compare against
// `self.pqSecrets[self.epoch]`, the value the previous rule called "held" -- is defeated by one
// line of adversary: fan out pq_secret[n-1] instead of pq_secret[n]. A different octet string, so
// every equality against the current row answers no; the same removed member holding it, because a
// member keeps every row it ever SAW; and the same storage_root reproduced
// at the epoch it was removed at. Narrow the subject and this case goes red while every other
// removal case in this file stays green.
//
// THE WORLD IS TWO EPOCHS DEEP BECAUSE IT HAS TO BE. A group at epoch 1 has one row, so "the
// current row" and "the whole table" are the same set and the narrowing is invisible. The honest
// rotation that builds the second row is also this case's first control: it is FOLLOWED.
func TestARemovalFannedOutOnAnEarlierEpochsSecretIsRefusedToo(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	atOne := append([]byte(nil), carol.group.pqSecretLocked()...)

	// ── CONTROL 1: AN HONEST ROTATION, FOLLOWED, which is what gives bob a second row ────────
	first := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	for _, member := range []*rotMember{bob, carol} {
		if err := world.deliver(member, first.page()...); err != nil {
			t.Fatalf("CONTROL FAILED: %s's walk over an honest rotation answered %v", member.name, err)
		}
	}
	if len(bob.group.pqSecrets) < 2 {
		t.Fatalf("CONTROL FAILED: bob holds %d row(s) after one rotation; with one row the current "+
			"row and the whole table are the same set and this case measures nothing",
			len(bob.group.pqSecrets))
	}
	if bytes.Equal(bob.group.pqSecretLocked(), atOne) {
		t.Fatalf("CONTROL FAILED: epoch 2's secret IS epoch 1's, so 'an EARLIER epoch's value' is " +
			"not a different octet string here")
	}
	if _, heldAtOne := bob.group.pqSecretHeldAtLocked(atOne); !heldAtOne {
		t.Fatalf("CONTROL FAILED: bob no longer holds epoch 1's secret, so the replay below is of " +
			"a value the removed member does not keep either")
	}

	// ── THE REPLAY: the removal opens epoch 3 on pq_secret[1] ────────────────────────────────
	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{opensOn: atOne})
	if !bytes.Equal(published.pqSecret, atOne) {
		t.Fatalf("CONTROL FAILED: the fixture opened epoch %d on something other than epoch 1's secret",
			published.opens)
	}
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-%d exporter: %v", published.opens, err)
	}
	if !bytes.Equal(messagegroup.StorageRoot(granted, atOne), messagegroup.StorageRoot(granted, published.pqSecret)) {
		t.Fatalf("CONTROL FAILED: the removed member's epoch-1 secret does not reproduce the " +
			"committer's root, so there is nothing here to refuse")
	}

	refused := world.deliver(bob, published.page()...)
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over a removal fanned out on epoch 1's secret answered %v, want "+
			"ErrRemovalWithoutRotation. A guard whose subject is only the CURRENT epoch's row "+
			"answers no to this and carol keeps the post-quantum half of epoch %d", refused, published.opens)
	}
	if bob.group.epoch != first.opens {
		t.Fatalf("bob stands at epoch %d, want %d", bob.group.epoch, first.opens)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark on a commit it refused: %v", bob.group.wrapDark)
	}
}

// AND THE RULE IS NOT "NOT IN MY CURRENT TABLE", BECAUSE THAT SET SHRINKS AND THE ADVERSARY'S DOES
// NOT: a removal fanned out on a pq_secret the WINDOW HAS EVICTED.
//
// THIS IS THE BLOCKER THAT DEFEATED ITEM 243 A SECOND TIME, and it is the mutation of the MECHANISM
// rather than of a parameter. The case next door moved the rule's subject from
// `pqSecrets[self.epoch]` to the whole live table, and the whole live table is pruned:
// [Group.dropPqSecretsBelowWindowLocked] erases every entry more than
// [messagegroup.PastEpochWindow] behind, which is local hygiene this device runs and a REMOVED
// MEMBER DOES NOT. So 33 honest rotations later the survivors no longer hold epoch 1's row and
// carol -- who was handed it at epoch 1 and never ran an eviction in its life -- still does. A
// removal opening epoch 35 on pq_secret[1] was followed with a nil error, and carol's retained
// value was octet for octet the post-quantum half of the survivors' storage_root at the epoch it
// was removed at. The removal removed nothing.
//
// THE CONTROL IS THE EVICTION ITSELF AND IT IS READ OFF THE LIVE TABLE, not off
// [Group.pqSecretHeldAtLocked] -- which is the function under test and which answers TRUE here by
// design now. Without that control this case would pass unchanged on a build whose window never
// evicted anything, and would be measuring the case next door again.
//
// THE NEGATIVE HALF IS ASSERTED TOO: the epoch the group is actually standing at has its own
// secret, and THAT one does not reproduce the removal's root -- so the root genuinely depends on
// the post-quantum half and the counterfactual above is not an arithmetic accident.
func TestARemovalFannedOutOnAnEvictedEpochsSecretIsRefusedToo(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	retained := append([]byte(nil), carol.group.pqSecretLocked()...)

	if !liveTableHolds(bob.group, retained) {
		t.Fatalf("CONTROL FAILED: bob's table does not hold epoch 1's secret before any rotation, " +
			"so the eviction asserted below would mean nothing")
	}

	// ── PastEpochWindow + 1 HONEST ROTATIONS, EVERY ONE OF THEM FOLLOWED ────────────────────
	const rotations = int(messagegroup.PastEpochWindow) + 1
	for at := 0; at < rotations; at += 1 {
		published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
			return alice.handle.Commit(nil)
		})
		for _, member := range []*rotMember{bob, carol} {
			if err := world.deliver(member, published.page()...); err != nil {
				t.Fatalf("CONTROL FAILED: %s's walk over honest rotation %d of %d answered %v",
					member.name, at+1, rotations, err)
			}
		}
	}
	t.Logf("after %d honest rotations bob stands at epoch %d and its LIVE table holds %d row(s)",
		rotations, bob.group.epoch, len(bob.group.pqSecrets))

	// ── THE CONTROL THIS CASE TURNS ON: THE WINDOW HAS THROWN EPOCH 1's ROW AWAY ────────────
	if liveTableHolds(bob.group, retained) {
		t.Fatalf("CONTROL FAILED: bob's LIVE table still holds epoch 1's secret after %d rotations "+
			"with a window of %d, so this case is TestARemovalFannedOutOnAnEarlierEpochsSecretIsRefusedToo "+
			"again and measures nothing new", rotations, messagegroup.PastEpochWindow)
	}
	// AND THE WITNESS DOES NOT AGREE WITH THE TABLE, which is the repair stated as a measurement.
	if _, everHeld := bob.group.pqSecretHeldAtLocked(retained); !everHeld {
		t.Fatalf("bob has no record that it EVER held epoch 1's secret. The removed member keeps " +
			"every row it ever saw, not every row inside this device's window, and a rule whose " +
			"subject is the live table is a rule whose subject shrinks")
	}

	// ── THE REPLAY: a removal opening its epoch on pq_secret[1] ─────────────────────────────
	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{opensOn: retained})
	if !bytes.Equal(published.pqSecret, retained) {
		t.Fatalf("CONTROL FAILED: the fixture opened epoch %d on something other than epoch 1's secret",
			published.opens)
	}
	standing := append([]byte(nil), bob.group.pqSecretLocked()...)
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-%d exporter: %v", published.opens, err)
	}
	if !bytes.Equal(messagegroup.StorageRoot(granted, retained), messagegroup.StorageRoot(granted, published.pqSecret)) {
		t.Fatalf("CONTROL FAILED: the removed member's epoch-1 secret does not reproduce the " +
			"committer's root, so there is nothing here to refuse")
	}
	if bytes.Equal(messagegroup.StorageRoot(granted, standing), messagegroup.StorageRoot(granted, published.pqSecret)) {
		t.Fatalf("CONTROL FAILED: the epoch bob is standing at has the SAME storage root as the " +
			"replay, so the root does not depend on the post-quantum half here and the " +
			"counterfactual above is arithmetic rather than a finding")
	}

	refused := world.deliver(bob, published.page()...)
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over a removal fanned out on an EVICTED epoch's secret answered %v, "+
			"want ErrRemovalWithoutRotation. carol's retained pq_secret is the post-quantum half of "+
			"bob's storage_root[%d] and the removal removed nothing", refused, published.opens)
	}
	if bob.group.epoch != uint64(rotations)+1 {
		t.Fatalf("bob stands at epoch %d, want %d", bob.group.epoch, rotations+1)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark on a commit it refused: %v", bob.group.wrapDark)
	}
}

// A REFUSED REMOVAL HALTS THE GROUP FOR EVERY LATER WALK, ACROSS A RESTART, AND A PROPER RE-COMMIT
// DOES NOT REPAIR IT.
//
// WHAT THIS CASE MEASURES, and it is the half of ruling 41 that was one walk deep. The refusal used
// to be returned once and kept nowhere:
//
//	walk 1 -> ErrRemovalWithoutRotation, cursor held below the commit
//	walk 2 -> ErrCommitIngest "ratchet generation already consumed", because step (0) of
//	          [Group.ingestCommitLocked] tracks the committer's ladder BEFORE the refusal, so the
//	          sentinel cannot be re-derived from the same record
//	walk 3 -> ErrRecordAbandoned at maxRecordAttempts, and the cursor resolved PAST the commit
//	walk 4, 5 -> nil, over a group standing an epoch behind its own log with a record on the disk
//	          reading HEALTHY and Send answering nil
//
// A refusal that halts a group has to be at least as durable as the dark state it is contrasted
// with, so the five walks are driven here and all five must answer the same sentence.
//
// AND THE REPAIR THE PROSE NAMED IS DRIVEN TOO, because it was FALSE. "A committer that re-commits
// properly is followed normally" cannot be true of a log: the refused commit stays in it AHEAD of
// this receiver for ever, so the re-commit lands above it and is never reached. The control for
// that clause is carol, who was never handed the refused commit and who follows the re-commit --
// without it, "bob did not follow the repair" would also be satisfied by a repair nobody could
// follow.
func TestARefusedRemovalHaltsTheGroupForEveryLaterWalkAndAcrossARestart(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{})

	// ── FIVE WALKS OVER ONE REFUSED COMMIT ──────────────────────────────────────────────────
	page := published.page()
	for walk := 1; walk <= 5; walk += 1 {
		refused := world.deliver(bob, page...)
		if !errors.Is(refused, ErrRemovalWithoutRotation) {
			t.Fatalf("walk %d over the refused commit answered %v, want ErrRemovalWithoutRotation. "+
				"A halt that is one walk deep is a halt the next Receive turns into a ratchet error, "+
				"then an abandonment, then nil", walk, refused)
		}
		if bob.group.epoch != 1 {
			t.Fatalf("walk %d left bob at epoch %d, want 1", walk, bob.group.epoch)
		}
		if bob.group.wrapDark != nil {
			t.Fatalf("walk %d made bob dark: %v", walk, bob.group.wrapDark)
		}
	}
	// THE CURSOR NEVER RESOLVED PAST IT, and the record was never given up on. Both are what
	// fail()'s three attempts would have taken away.
	if published.commit.recordId <= bob.group.cursor {
		t.Fatalf("bob's cursor is %d and the refused commit is record %d: the cursor resolved PAST "+
			"a commit this group refused, so the sixth walk would answer nil",
			bob.group.cursor, published.commit.recordId)
	}
	if len(bob.group.unopened) != 0 {
		t.Fatalf("bob gave up on %v; a commit refused BY RULE is not a record that did not open and "+
			"must not be spent through maxRecordAttempts", bob.group.unopened)
	}
	if got := bob.group.Stats().Unopened; got != 0 {
		t.Fatalf("Stats.Unopened is %d, want 0", got)
	}
	// AND IT WAS NEVER COUNTED AS A RECORD THAT DID NOT OPEN, which is a different claim from the
	// two above and is what keeps the two kinds of failure apart in a counter an operator reads.
	// A commit this device REFUSED is not a record that failed to open; counting it as one is the
	// same class of wrong number as [Stats.WrapOrphaned] reading zero on half the race orderings.
	// It is also what the three attempts are spent by, so a refusal counted here is a refusal on
	// its way to an abandonment on some page this test does not build.
	if got := bob.group.Stats().FailedOpen; got != 0 {
		t.Fatalf("Stats.FailedOpen is %d after five walks over a commit refused BY RULE, want 0: "+
			"a refusal is not an open that failed", got)
	}
	if attempts := bob.group.attempts[published.commit.recordId]; attempts != 0 {
		t.Fatalf("bob has spent %d attempt(s) on the refused commit; at maxRecordAttempts (%d) the "+
			"record is abandoned and the cursor resolves past it", attempts, maxRecordAttempts)
	}

	// ── THE REPAIR THE PROSE NAMED, DRIVEN: A PROPER RE-COMMIT IS NOT FOLLOWED ──────────────
	repair := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.Commit(nil)
	})
	// THE CONTROL, FIRST, AND IT FIRES FOR ITS OWN REASON: the same fixture -- one honest
	// rotation by alice, delivered as a page -- IS followed by a member that is not standing
	// behind a refused commit. So what the clause below measures is bob's halt and not a repair
	// nobody could follow. It has to be a second world because every honest member of THIS one is
	// halted at the same record, which is itself the finding: the partition is the whole group's.
	repairable := newRotWorld(t, "alice", "bob", "carol")
	repairableAlice, repairableBob := repairable.member("alice"), repairable.member("bob")
	control := repairable.rotate(repairableAlice, func() ([]byte, []byte, []byte, error) {
		return repairableAlice.handle.Commit(nil)
	})
	if err := repairable.deliver(repairableBob, control.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: an honest rotation of exactly the repair's shape was refused "+
			"with %v, so the clause below is about a broken fixture", err)
	}
	if repairableBob.group.epoch != control.opens {
		t.Fatalf("CONTROL FAILED: bob stands at epoch %d after the control rotation, want %d",
			repairableBob.group.epoch, control.opens)
	}
	repaired := world.deliver(bob, repair.page()...)
	if !errors.Is(repaired, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk after a PROPER re-commit answered %v. The claim that a committer "+
			"which re-commits properly is followed normally is false about a log: the refused "+
			"commit stays in it ahead of this receiver", repaired)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob stands at epoch %d after the repair, want 1", bob.group.epoch)
	}
	t.Logf("THE HALT IS PERMANENT, MEASURED: alice stands at epoch %d, bob at %d, and bob's walk "+
		"over alice's proper re-commit answers the same refusal. The repair is out of band and it "+
		"is a re-Add", alice.group.epoch, bob.group.epoch)

	// ── THE DISK, AND THEN THE RESTART ──────────────────────────────────────────────────────
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(records) != 1 || records[0].WrapDarkKind != wrapDarkRemoval || records[0].Epoch != 1 {
		t.Fatalf("bob's disk names epoch %d kind %d, want 1 and %d (the halt). A record reading "+
			"HEALTHY over a halted group is how the next process answers nil to everything",
			records[0].Epoch, records[0].WrapDarkKind, wrapDarkRemoval)
	}
	revived := restoredRotDevice(t, bob)
	restored, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoreOne over bob's halted record: %v", err)
	}
	defer restored.Close()
	restored.reconciled = true
	if restored.halted == nil || !errors.Is(restored.halted, ErrRemovalWithoutRotation) {
		t.Fatalf("the restored group's halt is %v, want an ErrRemovalWithoutRotation", restored.halted)
	}
	if restored.wrapDark != nil {
		t.Fatalf("the halt came back as a DARK state (%v); one persisted kind column, two fields, "+
			"and ruling 41 is that they are not the same state", restored.wrapDark)
	}
	if !strings.Contains(restored.halted.Error(), fmt.Sprintf("epoch %d", uint64(1))) {
		t.Fatalf("the restored halt is %v and does not name the epoch it stands at", restored.halted)
	}
	if _, err := restored.sendableLocked(KindText); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the restored group's Send answers %v, want the halt", err)
	}
	if err := restored.committableLocked(); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the restored group's Commit answers %v, want the halt", err)
	}
	_ = carol
}

// AN HONEST ROTATED REMOVAL IS NEVER CALLED UNROTATED, WHATEVER ELSE IS STAGED BESIDE THE VICTIM'S
// OWN WRAP. That is ruling 41's second outcome, and the name is the class rather than one page.
//
// WHY THIS CASE EXISTS. Ruling 41 names two outcomes and requires them separately reachable. For a
// REMOVAL the second one was not reachable at all: the pre-apply refusal fired on the ABSENCE of a
// wrap candidate, and an absence is exactly what an honest rotated removal looks like to the member
// whose own wrap was omitted (item 132) or sealed to a key it does not hold. Both landed in
// refused-and-halted, under a sentinel whose sentence says the epoch was opened on the secret the
// group already held -- FALSE of both, because the committer had rotated -- with nothing persisted
// and ruling 38's three-different-causes property collapsed into one.
//
// AND THE NAME THIS CASE USED TO CARRY WAS ITS OWN NEXT DEFECT. It was
// "…WithTheWrapOmittedGoesDark…", scoped to a page with NO OTHER CANDIDATE on it, and the repair it
// held was scoped the same way: the pre-apply refusal let an absence past only through
// `len(candidates) == 0`. One unrelated wrap record beside the victim's missing one turned "this
// device has no wrap" back into "every candidate of the fan-out is held", and the honest committer
// was called unrotated again -- on a page any current member can write, because a decoy carrying
// pq_secret[n] needs only the victim's leaf key, which is public in the tree. (c) and (d) below are
// that page, in both of its spellings, and they are why the clause is gone rather than narrowed:
// what a WRAP RECORD carries is not evidence about a COMMIT, and a permanent halt may not rest on
// it. See [Group.refuseUnrotatedRemovalLocked].
//
// THE CONTROL IS THE COMMITTER'S OWN HONESTY AND IT IS ASSERTED BEFORE ANYTHING ELSE, in every one
// of the four worlds: the value the epoch was opened on is NOT one this receiver has ever held.
// Without it a "dark" answer below would be satisfied by a fixture that never rotated, which is the
// other case entirely.
//
// AND THE SECOND CONTROL IS THE SAME DELIVERY FAILURE ON A COMMIT THAT REMOVES NOBODY, inline, in
// the same test: it answers the SAME sentinel and persists the SAME kind. That is the property in
// one line -- what the receiver says is decided by what went wrong with the delivery, not by
// whether the commit removed somebody -- and it is the comparison that showed the defect.
//
// AND (e) IS THE COMPLEMENT, FIRING FOR ITS OWN REASON: the SAME held-value decoy, beside a wrap
// that DOES open, is followed with nil and counted as an orphan. So what (c) and (d) measure is the
// absence at the victim and not the decoy, and a build that simply ignored decoys would fail (e).
func TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap(t *testing.T) {
	// ── (a) ITEM 132's OMISSION AT THE VICTIM ───────────────────────────────────────────────
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	})
	if _, everHeld := bob.group.pqSecretHeldAtLocked(published.pqSecret); everHeld {
		t.Fatalf("CONTROL FAILED: this removal opened its epoch on a value bob has held, so it is " +
			"the UNROTATED shape and the refusal below would be correct rather than a defect")
	}
	if len(published.wraps) == 0 {
		t.Fatalf("CONTROL FAILED: this fixture wrote no fan-out, so there is nothing to omit")
	}

	// the commit alone: the fan-out never reaches bob.
	dark := world.deliver(bob, published.commit)
	if errors.Is(dark, ErrRemovalWithoutRotation) {
		t.Fatalf("bob answered the REMOVAL sentinel over a commit whose committer DID rotate: %v. "+
			"That sentence is false about what happened, and it spends ruling 41's invalid-commit "+
			"refusal on a delivery failure -- which is the whole of why valid-and-dark was "+
			"unreachable for a removal", dark)
	}
	if !errors.Is(dark, ErrNoWrapForEpoch) {
		t.Fatalf("bob's walk over an honest rotated removal with its wrap omitted answered %v, want "+
			"ErrNoWrapForEpoch -- item 132's omission at the victim", dark)
	}
	if bob.group.epoch != published.opens {
		t.Fatalf("bob stands at epoch %d, want %d: a VALID commit is followed and the group is dark "+
			"at n+1, which is the outcome ruling 41 names for it", bob.group.epoch, published.opens)
	}
	if bob.group.wrapDark == nil || bob.group.halted != nil {
		t.Fatalf("bob's dark=%v halted=%v; this is the valid-and-dark outcome and the two fields "+
			"are how it is told from the other one", bob.group.wrapDark, bob.group.halted)
	}
	if got := bob.group.Stats().WrapMissing; got != 1 {
		t.Fatalf("Stats.WrapMissing is %d, want 1", got)
	}
	if _, err := bob.group.sendableLocked(KindText); !errors.Is(err, ErrNoWrapForEpoch) {
		t.Fatalf("bob's Send answers %v, want the wrap sentinel", err)
	}
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	if len(records) != 1 || records[0].WrapDarkKind != wrapDarkNoWrap || records[0].Epoch != published.opens {
		t.Fatalf("bob's disk names epoch %d kind %d, want %d and %d",
			records[0].Epoch, records[0].WrapDarkKind, published.opens, wrapDarkNoWrap)
	}

	// ── THE CONTROL: THE SAME OMISSION ON A COMMIT THAT REMOVES NOBODY ──────────────────────
	plain := newRotWorld(t, "alice", "bob", "carol")
	plainAlice, plainBob := plain.member("alice"), plain.member("bob")
	ordinary := plain.rotate(plainAlice, func() ([]byte, []byte, []byte, error) {
		return plainAlice.handle.Commit(nil)
	})
	plainDark := plain.deliver(plainBob, ordinary.commit)
	if !errors.Is(plainDark, ErrNoWrapForEpoch) {
		t.Fatalf("CONTROL FAILED: the same omission on a commit removing NOBODY answered %v, so the "+
			"comparison this case rests on is not between two readings of one delivery failure", plainDark)
	}
	plainRecords, err := plainBob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("plain bob's GroupRecords: %v", err)
	}
	if plainRecords[0].WrapDarkKind != records[0].WrapDarkKind {
		t.Fatalf("CONTROL FAILED: one omission persisted kind %d with a removal and kind %d without "+
			"one; what a receiver says is supposed to be decided by what went wrong with the "+
			"delivery and not by whether the commit removed somebody",
			records[0].WrapDarkKind, plainRecords[0].WrapDarkKind)
	}
	t.Logf("ONE DELIVERY FAILURE, ONE ANSWER: the omission answers %s and persists kind %d whether "+
		"or not the commit removes a member", "ErrNoWrapForEpoch", records[0].WrapDarkKind)

	// ── (b) THE WRAP THAT ARRIVED AND DID NOT OPEN ──────────────────────────────────────────
	bent := newRotWorld(t, "alice", "bob", "carol")
	bentAlice, bentBob, bentCarol := bent.member("alice"), bent.member("bob"), bent.member("carol")
	bentPublished := bent.rotate(bentAlice, func() ([]byte, []byte, []byte, error) {
		return bentAlice.handle.CommitRemove([]uint32{bentCarol.leaf})
	}, rotBend{leaf: bentBob.leaf, toAStranger: true})
	if _, everHeld := bentBob.group.pqSecretHeldAtLocked(bentPublished.pqSecret); everHeld {
		t.Fatalf("CONTROL FAILED: this removal did not rotate either")
	}
	unreadable := bent.deliver(bentBob, bentPublished.page()...)
	if errors.Is(unreadable, ErrRemovalWithoutRotation) {
		t.Fatalf("bob answered the REMOVAL sentinel over a rotated removal whose wrap simply did "+
			"not open: %v", unreadable)
	}
	if !errors.Is(unreadable, ErrWrapUnreadable) {
		t.Fatalf("bob's walk over a rotated removal with an unopenable wrap answered %v, want "+
			"ErrWrapUnreadable", unreadable)
	}
	if bentBob.group.epoch != bentPublished.opens || bentBob.group.wrapDark == nil || bentBob.group.halted != nil {
		t.Fatalf("bob stands at epoch %d dark=%v halted=%v, want epoch %d, dark and not halted",
			bentBob.group.epoch, bentBob.group.wrapDark, bentBob.group.halted, bentPublished.opens)
	}
	// THE COUNTER, WHICH READ 0 WHILE THE OLD REFUSAL'S OWN TEXT SAID "1 wrap(s) ... did not open".
	if got := bentBob.group.Stats().WrapUnreadable; got != 1 {
		t.Fatalf("Stats.WrapUnreadable is %d after a wrap arrived and did not open, want 1", got)
	}
	bentRecords, err := bentBob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bent bob's GroupRecords: %v", err)
	}
	if bentRecords[0].WrapDarkKind != wrapDarkUnreadable {
		t.Fatalf("bob's disk names kind %d, want %d", bentRecords[0].WrapDarkKind, wrapDarkUnreadable)
	}

	// ── (c) AND (d): THE SAME TWO DELIVERY FAILURES WITH ONE DECOY BESIDE THEM ──────────────
	//
	// THIS IS THE REPRODUCED BLOCKER. The decoy is one record: a device wrap addressed to the
	// victim's own wrap_target_handle for the epoch the removal opens, sealed to the victim's own
	// published X-Wing key -- which is in the ratchet tree and is public to every member -- carrying
	// pq_secret[n], which every member holds. It OPENS at the victim. So the moment the victim's own
	// wrap is missing or unopenable, the set of candidates this device staged is {the decoy}, every
	// one of them is a value this group has held, and the pre-apply clause that asked exactly that
	// question refused an HONEST commit and halted the receiver for ever with a sentence that was
	// false about what happened.
	//
	// WHAT IS ASSERTED IS RULING 41's OTHER OUTCOME IN BOTH SPELLINGS: dark at n+1, not halted, with
	// the sentinel that names what actually went wrong at this device and BOTH record-shaped
	// counters standing beside it -- so the answer does not rest on which of two sentences was
	// chosen.
	decoyWorld := func(name string, toAStranger bool, omit bool) {
		t.Helper()
		one := newRotWorld(t, "alice", "bob", "carol")
		oneAlice, oneBob, oneCarol := one.member("alice"), one.member("bob"), one.member("carol")
		heldAtOne := append([]byte(nil), oneBob.group.pqSecretLocked()...)
		bends := []rotBend{{leaf: oneBob.leaf, payload: heldAtOne, decoy: true}}
		if toAStranger {
			bends = append([]rotBend{{leaf: oneBob.leaf, toAStranger: true}}, bends...)
		}
		onePublished := one.rotate(oneAlice, func() ([]byte, []byte, []byte, error) {
			return oneAlice.handle.CommitRemove([]uint32{oneCarol.leaf})
		}, bends...)
		// ── THE THREE CONTROLS, BEFORE THE PAGE IS DELIVERED ────────────────────────────────
		if _, everHeld := oneBob.group.pqSecretHeldAtLocked(onePublished.pqSecret); everHeld {
			t.Fatalf("%s CONTROL FAILED: this removal opened its epoch on a value bob has held, so "+
				"it is the UNROTATED shape and a refusal below would be correct", name)
		}
		if _, everHeld := oneBob.group.pqSecretHeldAtLocked(heldAtOne); !everHeld {
			t.Fatalf("%s CONTROL FAILED: the decoy carries a value bob has NEVER held, so it cannot "+
				"make the every-candidate-is-held clause fire and this world measures nothing", name)
		}
		if len(onePublished.decoys) != 1 {
			t.Fatalf("%s CONTROL FAILED: the fixture built %d decoy row(s), want 1", name, len(onePublished.decoys))
		}
		page := append([]*sealed{}, onePublished.decoys...)
		dropped := false
		for at, wrap := range onePublished.wraps {
			if omit && onePublished.targets[at].leaf == oneBob.leaf {
				dropped = true
				continue
			}
			page = append(page, wrap)
		}
		if omit != dropped {
			t.Fatalf("%s CONTROL FAILED: omit=%v and the victim's row was dropped=%v", name, omit, dropped)
		}
		page = append(page, onePublished.commit)

		answered := one.deliver(oneBob, page...)
		if errors.Is(answered, ErrRemovalWithoutRotation) {
			t.Fatalf("%s: bob answered the REMOVAL sentinel over a commit whose committer DID "+
				"rotate: %v. The only thing that changed between this world and the one that is let "+
				"past is ONE wrap record a bystander can write, so a permanent halt rests on what a "+
				"third party put on the wire rather than on anything about the commit", name, answered)
		}
		if oneBob.group.halted != nil {
			t.Fatalf("%s: bob is HALTED (%v) on a valid commit; ruling 41's two outcomes are two "+
				"fields and this is the valid-and-dark one", name, oneBob.group.halted)
		}
		if oneBob.group.wrapDark == nil {
			t.Fatalf("%s: bob has no pq_secret for epoch %d and is not marked dark; the state is "+
				"permanent and a caller with no sentence for it reads an AEAD failure instead",
				name, onePublished.opens)
		}
		if oneBob.group.epoch != onePublished.opens {
			t.Fatalf("%s: bob stands at epoch %d, want %d: a VALID commit is followed and the group "+
				"is dark at n+1", name, oneBob.group.epoch, onePublished.opens)
		}
		// BOTH RECORD-SHAPED COUNTERS, so which of the two sentences is answered is not the only
		// observable this case has. The decoy opened and lost, which is an orphan by
		// [Stats.WrapOrphaned]'s own definition, whatever else went wrong beside it.
		stats := oneBob.group.Stats()
		if stats.WrapOrphaned != 1 {
			t.Fatalf("%s: Stats.WrapOrphaned is %d after a wrap opened and was not the epoch's own "+
				"secret, want 1", name, stats.WrapOrphaned)
		}
		wantUnreadable := uint64(0)
		if toAStranger {
			wantUnreadable = 1
		}
		if stats.WrapUnreadable != wantUnreadable {
			t.Fatalf("%s: Stats.WrapUnreadable is %d, want %d: a wrap at this device's own handle "+
				"that did not open is counted where it is FOUND and not only on the arm that reports it",
				name, stats.WrapUnreadable, wantUnreadable)
		}
		wantSentinel, wantKind := error(ErrOrphanWrap), wrapDarkOrphan
		if toAStranger {
			// THE ORDER OF THE TAIL, AND IT IS A CHOICE THIS CASE MAKES VISIBLE. Two things went
			// wrong at once: a wrap at bob's OWN handle did not open, and a wrap that did open was
			// not the epoch's. The first is what deprived bob and the second is a statement about
			// somebody else's record, so the unreadable sentence is the one answered.
			wantSentinel, wantKind = ErrWrapUnreadable, wrapDarkUnreadable
		}
		if !errors.Is(answered, wantSentinel) {
			t.Fatalf("%s: bob's walk answered %v, want %v", name, answered, wantSentinel)
		}
		if !errors.Is(oneBob.group.wrapDark, wantSentinel) {
			t.Fatalf("%s: bob's sticky diagnosis is %v, want %v", name, oneBob.group.wrapDark, wantSentinel)
		}
		decoyRecords, err := oneBob.dev.store.GroupRecords()
		if err != nil {
			t.Fatalf("%s: bob's GroupRecords: %v", name, err)
		}
		if len(decoyRecords) != 1 || decoyRecords[0].WrapDarkKind != wantKind ||
			decoyRecords[0].Epoch != onePublished.opens {
			t.Fatalf("%s: bob's disk names epoch %d kind %d, want %d and %d", name,
				decoyRecords[0].Epoch, decoyRecords[0].WrapDarkKind, onePublished.opens, wantKind)
		}
		t.Logf("%s: bob epoch=%d halted=%v dark=%v WrapOrphaned=%d WrapUnreadable=%d kind=%d",
			name, oneBob.group.epoch, oneBob.group.halted != nil, oneBob.group.wrapDark != nil,
			stats.WrapOrphaned, stats.WrapUnreadable, decoyRecords[0].WrapDarkKind)
	}
	decoyWorld("(c) the omission at the victim, with one held-value decoy beside it", false, true)
	decoyWorld("(d) the wrap sealed to a stranger, with one held-value decoy beside it", true, false)

	// ── (e) THE COMPLEMENT: THE SAME DECOY, BESIDE A WRAP THAT DOES OPEN, IS FOLLOWED ───────
	//
	// It fires for its own reason and it is what makes (c) and (d) about the ABSENCE at the victim
	// rather than about the decoy: one record of exactly the same shape, carrying exactly the same
	// held value, with the victim's own honest wrap left in the page -- and the walk answers nil.
	beside := newRotWorld(t, "alice", "bob", "carol")
	besideAlice, besideBob, besideCarol := beside.member("alice"), beside.member("bob"), beside.member("carol")
	besideHeld := append([]byte(nil), besideBob.group.pqSecretLocked()...)
	besidePublished := beside.rotate(besideAlice, func() ([]byte, []byte, []byte, error) {
		return besideAlice.handle.CommitRemove([]uint32{besideCarol.leaf})
	}, rotBend{leaf: besideBob.leaf, payload: besideHeld, decoy: true})
	if _, everHeld := besideBob.group.pqSecretHeldAtLocked(besideHeld); !everHeld {
		t.Fatalf("CONTROL FAILED: the decoy in the complement carries a value bob has never held, " +
			"so it is not the same record (c) and (d) are built on")
	}
	if err := beside.deliver(besideBob, besidePublished.page()...); err != nil {
		t.Fatalf("(e) THE COMPLEMENT FAILED: the same held-value decoy, beside a wrap that DOES "+
			"open, answered %v. A build that let (c) and (d) past by ignoring decoys would fail "+
			"here, and without this clause (c) and (d) would not be about the absence at all", err)
	}
	if besideBob.group.epoch != besidePublished.opens || besideBob.group.halted != nil ||
		besideBob.group.wrapDark != nil {
		t.Fatalf("(e) bob stands at epoch %d halted=%v dark=%v, want epoch %d and neither",
			besideBob.group.epoch, besideBob.group.halted, besideBob.group.wrapDark, besidePublished.opens)
	}
	if got := besideBob.group.Stats().WrapOrphaned; got != 1 {
		t.Fatalf("(e) Stats.WrapOrphaned is %d after the decoy opened and lost to bob's own wrap, want 1", got)
	}

	// ── (e2) AND A WRAP THAT DID NOT OPEN, ON A PAGE THAT IS FOLLOWED ───────────────────────
	//
	// THE STRONGEST FORM OF "COUNTED WHERE IT IS FOUND", because no arm reports it: bob is handed a
	// second row at its own wrap_target_handle sealed to a key nobody holds, beside its own honest
	// wrap. The walk answers nil, bob follows onto the epoch's own secret -- and one wrap at this
	// device's handle did not open, which is exactly what [Stats.WrapUnreadable] names. It read 0
	// before the counter moved out of the arm that reports it, on every healthy page.
	unopenable := newRotWorld(t, "alice", "bob", "carol")
	unopenableAlice, unopenableBob, unopenableCarol :=
		unopenable.member("alice"), unopenable.member("bob"), unopenable.member("carol")
	unopenablePublished := unopenable.rotate(unopenableAlice,
		func() ([]byte, []byte, []byte, error) {
			return unopenableAlice.handle.CommitRemove([]uint32{unopenableCarol.leaf})
		}, rotBend{leaf: unopenableBob.leaf, toAStranger: true, decoy: true})
	if len(unopenablePublished.decoys) != 1 {
		t.Fatalf("(e2) CONTROL FAILED: the fixture built %d decoy row(s), want 1",
			len(unopenablePublished.decoys))
	}
	if err := unopenable.deliver(unopenableBob, unopenablePublished.page()...); err != nil {
		t.Fatalf("(e2) bob's walk over its own honest wrap plus one unopenable row answered %v; the "+
			"page carries everything bob needs and is supposed to be followed", err)
	}
	unopenableStats := unopenableBob.group.Stats()
	if unopenableBob.group.epoch != unopenablePublished.opens || unopenableBob.group.wrapDark != nil ||
		unopenableBob.group.halted != nil {
		t.Fatalf("(e2) bob stands at epoch %d dark=%v halted=%v, want epoch %d and neither",
			unopenableBob.group.epoch, unopenableBob.group.wrapDark, unopenableBob.group.halted,
			unopenablePublished.opens)
	}
	if unopenableStats.WrapOpened != 1 {
		t.Fatalf("(e2) CONTROL FAILED: bob opened %d wrap(s) and this clause needs 1 -- its own -- "+
			"or the number below would be about a page that failed", unopenableStats.WrapOpened)
	}
	if unopenableStats.WrapUnreadable != 1 {
		t.Fatalf("(e2) Stats.WrapUnreadable is %d on a page that was FOLLOWED, want 1. A wrap at "+
			"this device's own handle that did not open is a record, and no arm of the resolution "+
			"reports it when the page succeeds -- so a counter added on the arm that names it reads "+
			"zero on every healthy page", unopenableStats.WrapUnreadable)
	}
	if unopenableStats.WrapOrphaned != 0 {
		t.Fatalf("(e2) Stats.WrapOrphaned is %d and nothing here opened and lost, want 0",
			unopenableStats.WrapOrphaned)
	}

	// ── (f) AND THE WHOLE PAGE, DELIVERED, IS STILL FOLLOWED ────────────────────────────────
	//
	// Without this the clauses above would also be satisfied by a build in which no removal is
	// ever followed at all, which removes the feature rather than the member.
	clean := newRotWorld(t, "alice", "bob", "carol")
	cleanAlice, cleanBob, cleanCarol := clean.member("alice"), clean.member("bob"), clean.member("carol")
	cleanPublished := clean.rotate(cleanAlice, func() ([]byte, []byte, []byte, error) {
		return cleanAlice.handle.CommitRemove([]uint32{cleanCarol.leaf})
	})
	if err := clean.deliver(cleanBob, cleanPublished.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: a complete honest rotated removal was refused with %v", err)
	}
	if cleanBob.group.wrapDark != nil || cleanBob.group.halted != nil {
		t.Fatalf("CONTROL FAILED: bob is dark=%v halted=%v after a clean removal",
			cleanBob.group.wrapDark, cleanBob.group.halted)
	}
}

// liveTableHolds is whether a group's LIVE pq_secret table -- the one the window prunes -- carries
// that value at any epoch.
//
// IT IS DELIBERATELY NOT [Group.pqSecretHeldAtLocked]. That function is what the eviction case
// above is testing, and asking it "does bob still hold epoch 1's secret" would be asking the
// subject to be its own control.
func liveTableHolds(group *Group, value []byte) bool {
	for _, secret := range group.pqSecrets {
		if bytes.Equal(secret, value) {
			return true
		}
	}
	return false
}

// THE WRAP-CANDIDATE ARM, DRIVEN DIRECTLY, BOTH WAYS.
//
// The page-level case above is the one that matters in the field, and it is answered BEFORE
// ApplyCommit -- which means the resolution's own first arm is not what refuses it there. This
// drives that arm with the values production would hand it, for the reason the compatibility arm's
// case gives below: a property whose only gate is structural is one refactor away from being
// unmeasured, and this arm's guard is the one that was missing.
//
// BOTH DIRECTIONS, and the control is the whole point: with no removal the arm answers the
// candidate, which is every honest rotation in this package; with one, and the candidate carrying
// a value this receiver has held, it refuses.
func TestTheWrapCandidateArmOfTheResolutionIsClosedToARemoval(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	retained := append([]byte(nil), carol.group.pqSecretLocked()...)

	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{})
	// bob opens its wrap and stops there: the commit is not delivered to it, so what the
	// resolution is asked below is the question production asks at (4a).
	if err := world.deliver(bob, published.wraps...); err != nil {
		t.Fatalf("bob's walk over the fan-out answered %v", err)
	}
	staged := bob.group.wrapsFor[published.opens]
	if len(staged) != 1 || !bytes.Equal(staged[0].secret, retained) {
		t.Fatalf("CONTROL FAILED: bob staged %d candidate(s) and this case needs one carrying the "+
			"value the removed member holds", len(staged))
	}
	digest, err := epochDigestOf(&published.commit.record.Header)
	if err != nil || digest == nil {
		t.Fatalf("the digest on the removal commit: %v %v", digest, err)
	}
	// THE COMMITTER IS THE ONE MEMBER THAT CAN ASK: judging a candidate needs mls_secret at the
	// epoch the commit OPENS, and only a device that has merged holds it. alice has. The
	// candidate is staged on alice's group because that is what the arm reads.
	mlsSecret, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-%d exporter: %v", published.opens, err)
	}

	// THE CONTROL, FIRST: with no removal the arm answers the candidate. That is every honest
	// rotation this package performs, and without it the refusal below would also be satisfied by
	// an arm that refuses everything.
	alice.group.wrapsFor[published.opens] = []wrapCandidate{{recordId: 1, secret: append([]byte(nil), retained...)}}
	answered, err := alice.group.resolvePqSecretLocked(mlsSecret, published.opens, digest, nil)
	if err != nil {
		t.Fatalf("CONTROL FAILED: the wrap-candidate arm refused a non-removing commit with %v", err)
	}
	if !bytes.Equal(answered, retained) {
		t.Fatalf("CONTROL FAILED: the arm answered a value that is not the staged candidate, so the " +
			"clause below is about some other arm")
	}

	// THE PROPERTY. This is the return that was `candidate.secret, nil` with no guard at all.
	alice.group.wrapsFor[published.opens] = []wrapCandidate{{recordId: 1, secret: append([]byte(nil), retained...)}}
	secret, err := alice.group.resolvePqSecretLocked(mlsSecret, published.opens, digest, []uint32{carol.leaf})
	if err == nil {
		t.Fatalf("a commit removing leaf %d was followed on a wrap candidate carrying the value "+
			"the removed member also holds (%d octets of it)", carol.leaf, len(secret))
	}
	if !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the refusal is %v, want ErrRemovalWithoutRotation", err)
	}
}

// AND THE SHAPE NO NARROWING OF THE PRE-APPLY REFUSAL COULD EVER HAVE CAUGHT: a removal whose
// fan-out is FRESH and whose digest still names the held secret.
//
// WHY IT EXISTS AT ALL. The pre-apply refusal cannot evaluate the digest -- judging a candidate
// needs mls_secret at the epoch the commit OPENS and there is no exporter over a PROCESSED commit.
// This page carries a decoy nobody can use, so the clause that asked whether every candidate
// carried a HELD value answered no and let it past, and the digest then said the epoch was opened
// on the held secret after all. That clause is gone now for a different reason -- a candidate set
// is not evidence about a commit -- and this case is kept because it is the page that shows the
// clause could not have been repaired by narrowing: there is nothing about the wraps to narrow ON.
// It is the same refusal the case above takes, reached from the opposite direction.
//
// WHAT IS ASSERTED IS RULING 41's OUTCOME AND THE RESIDUAL BESIDE IT. The commit is refused with
// the same sentinel, the group does NOT go dark and its own epoch does not move -- and the MLS
// handle HAS moved, because ApplyCommit ran before the resolution could be asked. That divergence
// is the residual and it is named here rather than papered over: the group is halted at n, its
// session and its persisted record agree with each other at n, and the next process READS the halt
// off that record. It does not re-derive it and cannot: step (0) of [Group.ingestCommitLocked] has
// consumed the committer's ratchet generation by the time this refusal is reached, so a second walk
// over the same record answers `ratchet generation already consumed` instead.
func TestTheResidualUnrotatedRemovalIsRefusedAfterTheApplyAndStillDoesNotGoDark(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	retained := append([]byte(nil), carol.group.pqSecretLocked()...)

	published := world.fanOutOnAFreshSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	})
	// THE CONTROL, INLINE: the decoy this case rests on is genuinely NOT a value bob holds, which
	// is what makes this page the one no narrowing of a candidate-reading check could have caught.
	if err := world.deliver(bob, published.wraps...); err != nil {
		t.Fatalf("CONTROL FAILED: bob's walk over the decoy fan-out answered %v", err)
	}
	staged := bob.group.wrapsFor[published.opens]
	if len(staged) != 1 {
		t.Fatalf("CONTROL FAILED: bob staged %d candidate(s), want 1", len(staged))
	}
	if bytes.Equal(staged[0].secret, retained) {
		t.Fatalf("CONTROL FAILED: the decoy IS the held secret, so this case is the one next door")
	}
	if _, alreadyHeld := bob.group.pqSecretHeldAtLocked(staged[0].secret); alreadyHeld {
		t.Fatalf("CONTROL FAILED: bob already holds the decoy, so the pre-apply refusal fences this " +
			"commit and the resolution is never reached")
	}

	refused := world.deliver(bob, published.commit)
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over the residual shape answered %v, want ErrRemovalWithoutRotation", refused)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark on an unrotated removal: %v. Ruling 41 says an invalid commit is "+
			"refused and not followed into a brick", bob.group.wrapDark)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob's group stands at epoch %d, want 1", bob.group.epoch)
	}
	if held, isHeld := bob.group.pqSecretAtLocked(published.opens); isHeld {
		t.Fatalf("bob filed a pq_secret for epoch %d (%d octets) although it refused the commit that "+
			"opens it", published.opens, len(held))
	}
	// THE RESIDUAL, ASSERTED RATHER THAN CLAIMED CLOSED: the MLS handle is one epoch ahead,
	// because this refusal is the only one of the two that is taken after ApplyCommit.
	if bob.handle.Epoch() != published.opens {
		t.Fatalf("this case is supposed to be the AFTER-apply refusal and bob's handle stands at "+
			"epoch %d, want %d; if the pre-apply refusal now covers this shape, move this case and "+
			"say so", bob.handle.Epoch(), published.opens)
	}
	t.Logf("THE RESIDUAL, MEASURED: the refusal is taken after ApplyCommit, so bob's MLS handle "+
		"stands at epoch %d while its group, its session and its persisted record stand at %d. "+
		"The group is halted, not dark, and the halt is permanent -- the refused commit stays in "+
		"the log ahead of this receiver, so a proper re-commit lands above it",
		bob.handle.Epoch(), bob.group.epoch)
	records, err := bob.dev.store.GroupRecords()
	if err != nil {
		t.Fatalf("bob's GroupRecords: %v", err)
	}
	// AND THE HALT TAKEN AFTER THE APPLY IS PERSISTED EXACTLY AS THE ONE TAKEN BEFORE IT. Two
	// refusal sites, ONE state: both go through [Group.haltLocked], so a reader of the disk cannot
	// tell which of the two wrote the record and does not need to. This assertion used to be
	// `wrapDarkNone` -- nothing persisted at all -- which is how a halted group came back reading
	// HEALTHY and went on answering nil to every later walk.
	if len(records) != 1 || records[0].Epoch != 1 || records[0].WrapDarkKind != wrapDarkRemoval {
		t.Fatalf("bob's disk names epoch %d and wrap_dark kind %d, want 1 and %d",
			records[0].Epoch, records[0].WrapDarkKind, wrapDarkRemoval)
	}
	if _, err := bob.group.sendableLocked(KindText); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's Send answers %v, want the halt", err)
	}
}

// AND THE OTHER HELD-SECRET ARM: a removal on a commit carrying NO epoch digest at all.
//
// It is a separate case because it is a separate arm. A kind 0x0001 commit -- Spec B section 5.4's
// acceptance window is dated and open -- never reaches the compatibility comparison; it takes the
// held secret at the top of the resolution and returns. A single guard in front of the function
// would have covered both and would ALSO have refused a removal that rotated, which is the case
// this package exists to serve.
//
// IT IS DRIVEN THROUGH THE RESOLUTION DIRECTLY, and WHY is a claim this file got wrong once. It
// used to say that (3a) "catches every removal with no fan-out before ApplyCommit and no page can
// therefore reach the no-digest arm through a walk". That was FALSE, not merely unmeasured, and
// the counterexample was found on the first try: a committer that removes a leaf, writes a
// COMPLETE fan-out and seals its commit with no attachment has a fan-out, passed a check that
// asked only whether one existed, and landed here through an ordinary walk. (3a) asks a different
// question again -- see [Group.refuseUnrotatedRemovalLocked] -- and the claim is no longer argued: the page
// that reached this arm is built and delivered by
// TestADigestLessRemovalIsRefusedBeforeTheApplyAndOnlyANonRemovingOneReachesTheNoDigestArm, which measures where the refusal is
// taken instead of asserting where it cannot be. This case stays a direct call because a property
// whose only driver is a page is one pre-apply repair away from being unmeasured.
func TestTheNoDigestArmOfTheResolutionIsAlsoClosedToARemoval(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	bob, carol := world.member("bob"), world.member("carol")
	mlsSecret, err := bob.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("bob's exporter: %v", err)
	}

	// THE CONTROL FIRST, IN THE SAME CALL SHAPE: with no removal, a digest-less commit is
	// followed on the held secret. That is the compatibility path and every group on the alpha
	// is on it, so a refusal that also caught this would be a refusal nobody could ship.
	held, err := bob.group.resolvePqSecretLocked(mlsSecret, bob.group.epoch+1, nil, nil)
	if err != nil {
		t.Fatalf("CONTROL FAILED: a digest-less commit that removes nobody was refused with %v", err)
	}
	if !bytes.Equal(held, bob.group.pqSecretLocked()) {
		t.Fatalf("CONTROL FAILED: the compatibility arm answered a secret this group does not hold")
	}

	secret, err := bob.group.resolvePqSecretLocked(mlsSecret, bob.group.epoch+1, nil, []uint32{carol.leaf})
	if err == nil {
		t.Fatalf("a digest-less commit that removes leaf %d was followed on the held secret, and "+
			"the value it answered is the one the removed member also holds (%d octets of it)", carol.leaf, len(secret))
	}
	if !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the refusal is %v, want ErrRemovalWithoutRotation", err)
	}
}

// A DIGEST-LESS REMOVAL IS REFUSED BEFORE THE APPLY, AND THE NO-DIGEST ARM IS REACHED ONLY BY A
// COMMIT THAT REMOVES NOBODY.
//
// THE NAME IS THE SECOND CORRECTION OF ONE CLAIM. This case was called
// "TestNoPageReachesTheNoDigestArmOfTheResolutionThroughAWalk" after the sentence it was written to
// refute, and the name was then FALSE in the other direction: the control at the bottom -- a
// digest-less commit removing nobody, which is every kind 0x0001 commit on the deployed alpha --
// reaches that arm through a walk on purpose and is FOLLOWED. What is true of every page is the
// pair: the removing shape never reaches it, the non-removing shape always does, and both halves
// are driven here.
//
// THE SHAPE. A committer removes carol, writes a complete openable fan-out -- so no check on the
// ABSENCE of a candidate can see it -- and seals its commit with no server attachment at all,
// which is what [epochDigestOf] answers nil for. Under the first (3a) this walked straight past the
// pre-apply refusal, applied the commit, and was refused at the resolution: dark at n+1, on a
// commit the build had just judged invalid, which is the outcome ruling 41 took away.
//
// WHAT IS ASSERTED IS WHERE, AND NOT WHETHER. Every build since ruling 41 refuses this page; the
// difference is the epoch the receiver is standing at afterwards and whether it is dark. So this
// case asserts the refusal is taken BEFORE ApplyCommit -- bob's own MLS handle has not moved, which
// the residual case next door shows is a genuinely different observable and not a restatement of
// the epoch. Since 2026-09-24 the digest clause is the ONLY thing that can take it there, so this
// is also the whole of what remains pre-apply.
//
// THE CONTROL IS INLINE AND FIRES FOR ITS OWN REASON: the same committer, the same fan-out, the
// same missing attachment, removing NOBODY, is FOLLOWED. Without it this would also be satisfied by
// a build that refuses every commit carrying no digest, which would take the whole acceptance
// window down with it.
func TestADigestLessRemovalIsRefusedBeforeTheApplyAndOnlyANonRemovingOneReachesTheNoDigestArm(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	published := world.fanOutOnTheHeldSecret(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	}, unrotatedFanOut{noDigest: true})
	if digest, err := epochDigestOf(&published.commit.record.Header); err != nil || digest != nil {
		t.Fatalf("CONTROL FAILED: this commit carries a digest (%v, %v), so it is not the shape "+
			"that reaches the no-digest arm", digest, err)
	}
	if len(published.wraps) == 0 {
		t.Fatalf("CONTROL FAILED: this commit writes no fan-out at all, so it says nothing about " +
			"the arm this case is named for -- the digest clause is what refuses it either way")
	}

	refused := world.deliver(bob, published.page()...)
	if !errors.Is(refused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over a fanned, digest-less removal answered %v, want ErrRemovalWithoutRotation", refused)
	}
	if bob.group.epoch != 1 {
		t.Fatalf("bob stands at epoch %d, want 1", bob.group.epoch)
	}
	if bob.group.wrapDark != nil {
		t.Fatalf("bob went dark on a commit it refused: %v", bob.group.wrapDark)
	}
	// THE MEASUREMENT THIS CASE EXISTS FOR: the MLS handle has NOT moved, so the refusal was taken
	// before ApplyCommit and the resolution was never asked. A refusal at the resolution leaves
	// the handle at n+1 -- which is exactly what the residual case asserts, so the two are
	// distinguishable and this is not a restatement of the epoch check above.
	if bob.handle.Epoch() != 1 {
		t.Fatalf("bob's MLS handle stands at epoch %d, want 1: the commit was APPLIED and the "+
			"refusal was therefore taken at the resolution, which is the arm this page is supposed "+
			"to no longer reach", bob.handle.Epoch())
	}

	// ── THE SECOND SHAPE, AND IT IS HERE BECAUSE A MUTANT SURVIVED ──────────────────────────
	//
	// Deleting the `digest == nil` arm of the pre-apply refusal used to leave the whole suite
	// GREEN: the page above did not need it, because the candidate clause beside it refused that
	// commit anyway and neither the epoch nor the handle could tell the two refusals apart. The
	// clause is gone now, so the page above kills that mutant by itself -- and this second shape is
	// kept, because it is the one whose ONLY pre-apply evidence has ever been the missing digest. A
	// fresh fan-out satisfies any check that reads the candidates, so if the removed clause is ever
	// reintroduced in some narrower dress, this page is what still holds the digest arm to being
	// load-bearing on its own.
	fresh := newRotWorld(t, "alice", "bob", "carol")
	freshAlice, freshBob, freshCarol := fresh.member("alice"), fresh.member("bob"), fresh.member("carol")
	decoy := make([]byte, messagegroup.PqSecretBytes)
	for at := range decoy {
		decoy[at] = 0x3D
	}
	fanned := fresh.fanOutOnTheHeldSecret(freshAlice, func() ([]byte, []byte, []byte, error) {
		return freshAlice.handle.CommitRemove([]uint32{freshCarol.leaf})
	}, unrotatedFanOut{noDigest: true, payload: decoy})
	if err := fresh.deliver(freshBob, fanned.wraps...); err != nil {
		t.Fatalf("CONTROL FAILED: bob's walk over the fresh fan-out answered %v", err)
	}
	if _, alreadyHeld := freshBob.group.pqSecretHeldAtLocked(decoy); alreadyHeld {
		t.Fatalf("CONTROL FAILED: bob already holds the decoy, so the candidate clause refuses this " +
			"commit and the digest clause is not what this case measures")
	}
	if staged := freshBob.group.wrapsFor[fanned.opens]; len(staged) != 1 {
		t.Fatalf("CONTROL FAILED: bob staged %d candidate(s), want 1", len(staged))
	}
	freshRefused := fresh.deliver(freshBob, fanned.commit)
	if !errors.Is(freshRefused, ErrRemovalWithoutRotation) {
		t.Fatalf("bob's walk over a digest-less removal with a FRESH fan-out answered %v, want "+
			"ErrRemovalWithoutRotation", freshRefused)
	}
	if freshBob.handle.Epoch() != 1 || freshBob.group.epoch != 1 || freshBob.group.wrapDark != nil {
		t.Fatalf("bob's handle is at epoch %d, its group at %d, dark %v; the digest clause is what "+
			"keeps this refusal BEFORE ApplyCommit, and without it the commit is applied and refused "+
			"at the resolution instead",
			freshBob.handle.Epoch(), freshBob.group.epoch, freshBob.group.wrapDark)
	}

	// ── THE CONTROL: THE SAME COMMIT SHAPE, REMOVING NOBODY, IS FOLLOWED ────────────────────
	plain := newRotWorld(t, "alice", "bob", "carol")
	plainAlice, plainBob := plain.member("alice"), plain.member("bob")
	ordinary := plain.fanOutOnTheHeldSecret(plainAlice, func() ([]byte, []byte, []byte, error) {
		return plainAlice.handle.Commit(nil)
	}, unrotatedFanOut{noDigest: true})
	if err := plain.deliver(plainBob, ordinary.page()...); err != nil {
		t.Fatalf("CONTROL FAILED: a digest-less commit that removes NOBODY was refused with %v. "+
			"That is the compatibility path and every group on the deployed alpha is on it", err)
	}
	if plainBob.group.epoch != ordinary.opens {
		t.Fatalf("CONTROL FAILED: bob stands at epoch %d after an ordinary digest-less commit, want %d",
			plainBob.group.epoch, ordinary.opens)
	}
}

// AND THE COMPATIBILITY ARM: a removal whose DIGEST names a secret this receiver has held.
//
// IT IS DRIVEN DIRECTLY, AND THE REASON IS A MEASUREMENT RATHER THAN A CONVENIENCE. Mutation row
// M5 -- the digest arm's guard deleted -- was killed by the structural gate below and by NOTHING
// ELSE, on the build where (3a) fenced every removal with no fan-out before ApplyCommit. That is
// no longer true: (3a) reads only the commit's own digest, so EVERY removal carrying one reaches
// this arm or the one beside it through a walk, and
// TestARemovalThatDoesNotRotateIsRefusedAndTheGroupDoesNotFollowIt is the page that does it. The
// direct drive stays anyway, for the reason it was written: a property whose only gate is
// structural is one refactor away from being unmeasured, and this asks the arm with the values
// production hands it rather than through a page that might stop reaching it.
//
// THE COMMITTER IS THE ONE MEMBER THAT CAN ASK. Judging a candidate needs mls_secret at the epoch
// the commit opens, and only a device that has merged holds it; alice has. The digest is read back
// off the commit RECORD rather than rebuilt here, so what this asks the resolution is the same
// question a receiver asks.
//
// BOTH DIRECTIONS: with no removal the arm answers the held secret -- which is every group on the
// deployed alpha and must keep working -- and with one it refuses.
func TestTheCompatibilityArmOfTheResolutionIsClosedToARemoval(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, carol := world.member("alice"), world.member("carol")
	held := append([]byte(nil), alice.group.pqSecretLocked()...)

	published := world.advanceWithoutRotating(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{carol.leaf})
	})
	digest, err := epochDigestOf(&published.commit.record.Header)
	if err != nil {
		t.Fatalf("the digest on the removal commit: %v", err)
	}
	if digest == nil {
		t.Fatalf("the removal commit carries no epoch digest, so this case is driving the OTHER arm")
	}
	mlsSecret, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the epoch-%d exporter: %v", published.opens, err)
	}

	// THE CONTROL, FIRST: with no removal this digest is reproduced by the held secret and the
	// arm answers it. Without this clause the refusal below would also be satisfied by a digest
	// no candidate matches at all, which is a different arm and a different sentinel.
	answered, err := alice.group.resolvePqSecretLocked(mlsSecret, published.opens, digest, nil)
	if err != nil {
		t.Fatalf("CONTROL FAILED: the compatibility arm refused a non-removing commit with %v, so "+
			"this digest is not one the held secret reproduces and the clause below is vacuous", err)
	}
	if !bytes.Equal(answered, held) {
		t.Fatalf("CONTROL FAILED: the compatibility arm answered a secret that is not the held one")
	}

	// THE PROPERTY.
	secret, err := alice.group.resolvePqSecretLocked(mlsSecret, published.opens, digest, []uint32{carol.leaf})
	if err == nil {
		t.Fatalf("a commit removing leaf %d was followed on the held secret, which is the value "+
			"the removed member also holds (%d octets of it)", carol.leaf, len(secret))
	}
	if !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("the refusal is %v, want ErrRemovalWithoutRotation", err)
	}
}

// ── 4. THE STRUCTURAL HALF: EVERY RETURN THAT CAN CARRY A pq_secret LEAVES BY THE ONE EXIT ───

// EVERY RETURN OF THE RESOLUTION THAT CAN CARRY A pq_secret GOES THROUGH THE GUARDED EXIT, AND
// EVERY OTHER RETURN CARRIES NOTHING.
//
// ── WHY THIS GATE WAS REWRITTEN, WHICH IS THE WHOLE POINT OF IT ───────────────────────────────
//
// The gate that stood here walked "every return of the identifier `held`". It found two, both
// guarded, logged
//
//	the resolution returns the held secret at 2 site(s), 2 guarded;
//	the complement is [candidate.secret nil nil ...]
//
// and PASSED -- with `candidate.secret`, the unguarded arm through which a removal was followed on
// the secret the removed member keeps, sitting in its own printed complement. Two failures, and
// both are classes this project has been bitten by before:
//
//  1. THE NARROWING WAS PRINTED AND NOT ASSERTED. The complement went to t.Logf and nothing
//     decided anything about it. A narrowing that only prints is a narrowing nobody can fail.
//  2. THE CLASS WAS THE DEFECT'S CURRENT SPELLING. "Returns the identifier `held`" is not the
//     property; the property is "returns a pq_secret". The wrap-candidate arm returns one under a
//     different name, so it was outside the subject by construction -- a gate whose complement
//     contains the defect is a gate scoped to how the defect happens to be spelled today.
//
// ── WHAT IT ASKS NOW ──────────────────────────────────────────────────────────────────────────
//
// The subject is EVERY return statement in the function, including the exit's own, keyed by the
// source text of whatever sits in the secret position. Each key is held against a written
// disposition BOTH WAYS -- a site with no entry is a refusal, an entry naming no site is a refusal
// -- and the dispositions say, for each, whether that return can carry a pq_secret.
//
// THE ASSERTION IS THE COMPLEMENT ITSELF, and it is one sentence: a return of this function either
// puts `nil` in the secret position, or it is the one guarded exit or a call to it. So a new arm
// spelled `return candidate.secret, nil`, `return self.pqSecrets[e], nil`, `return staged[0], nil`
// or anything else lands as a key with no disposition and the gate goes red BY CLASS -- not
// because a case was added for one site.
//
// AND THE EXIT IS ASSERTED TO BE GUARDED, structurally: there is exactly one local function value
// in the resolution, it is the one every secret leaves by, and its body RETURNS what
// [refuseRemovalOnHeldSecret] builds -- nil in the secret position, the refusal in the error one,
// under a condition read whole. [removalGuardDefect] carries the three clauses that are left, why
// "a call of the refusal appears" was never one of them, and the six that were deleted on
// 2026-09-24 with the driven row that catches each.
//
// ── AND WHY IT WAS REWRITTEN A SECOND TIME, 2026-09-24 ────────────────────────────────────────
//
// THE CLASS WAS STILL A SPELLING, one level further down. The walk skipped any return whose result
// list was EMPTY (`if !isReturn || len(ret.Results) == 0 { return true }`) while the header claimed
// "the subject is EVERY return statement in the function". Under NAMED RESULTS a bare `return`
// carries whatever was assigned above it, so a fourth arm spelled
//
//	func … (answer []byte, failure error) {
//	    if fast, isFast := self.pqSecretAtLocked(opensEpoch + 1); isFast { answer = fast; return }
//
// was outside the subject BY CONSTRUCTION. It was applied as a mutant and this gate PASSED, with
// the new arm in neither its carrying list nor its printed complement, and the whole package green
// beside it. Four things close it, and three of them are properties this gate did not have:
//
//  1. THE SIGNATURE'S RESULTS ARE ASSERTED UNNAMED, so a bare return cannot exist to be skipped.
//     That is the escape hatch removed rather than covered.
//  2. THE WALK'S SUBJECT IS HELD AGAINST AN INDEPENDENT COUNT of every [ast.ReturnStmt] in the
//     function, the exit's own included. A return the keyed walk does not key is a disagreement
//     between two numbers, whatever the reason for it.
//  3. EVERY CALL OF THE EXIT MUST BE IN RETURN POSITION. `secret, failure := answerSecret(...);
//     return secret, failure` would otherwise satisfy every clause above while making the
//     carrying-site count meaningless, and the count is what the old floor rested on.
//  4. THE FLOOR IS A WRITTEN DISPOSITION AND NOT THE NUMBER 3. Each arm is keyed by its own `how`
//     clause -- the sentence the refusal prints -- and held BOTH WAYS: an arm with no row is a
//     refusal, a row naming no arm is a refusal. A hardcoded 3 is the per-site case this gate says
//     it is not, and it could not tell a deleted arm from a renamed one.
//
// ── AND A THIRD TIME, THE SAME DAY: THE GUARD CLAUSE MEASURED A CALL AND NOT AN ANSWER ────────
//
// Everything above holds that every secret leaves by ONE exit. What decided whether that exit is
// GUARDED asked only whether a call of [refuseRemovalOnHeldSecret] appears inside an
// `if removesLeaves` -- never what becomes of the call's RESULT. So an exit that computes the
// refusal and throws it away (`_ = refuseRemovalOnHeldSecret(...)`) passed this gate with the whole
// property gone, and printed a HEALTHIER complement than the correct code does. That is ledger item
// 254's "owed from this pass", it was reproduced before it was repaired, and it is the same class
// as the two rewrites above: the subject was a spelling (*a call appears*) where the property is an
// answer (*the refusal is what the exit returns*). [removalGuardDefect] is the repair.
//
// ── AND WHAT IS LEFT OF IT AFTER LEDGER RULING 46, WHICH IS WHY THIS GATE IS STILL HERE ──────
//
// [removalGuardDefect] went from eight clauses to three on 2026-09-24, because six of them were
// measured being caught by
// TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath
// instead -- one driven input per mutant, table in that function's own header. THIS gate is NOT
// one of the deletions, and the reason is a class rather than a preference: its subject is every
// RETURN OF THE RESOLUTION, held against a written disposition both ways, so a FOURTH ARM added
// later lands here as a key with no row. A driven table cannot cover an arm that does not exist
// yet -- there is no input that reaches it -- and the one-exit property is what makes the removal
// rule a property of the function rather than of the three arms somebody remembered.
func TestEveryReturnOfTheResolutionThatCanCarryAPqSecretGoesThroughTheGuardedExit(t *testing.T) {
	// THE DISPOSITIONS, keyed by the SOURCE TEXT of the expression in the secret position. `why`
	// is a sentence and not a label because a disposition nobody can disagree with is a row that
	// stops being read.
	const exit = "answerSecret"
	dispositions := map[string]struct {
		carries bool
		why     string
	}{
		"nil": {carries: false,
			why: "a refusal. Every failure this function returns puts nil in the secret " +
				"position, which is what makes 'the complement is exactly nil' the whole assertion"},
		exit + "(...)": {carries: true,
			why: "an arm answering through the one exit. Whatever it hands over is compared " +
				"against this group's WHOLE pq_secret table before it leaves"},
		"secret": {carries: true,
			why: "the exit's own answer, which is the single place a pq_secret leaves this " +
				"function at all, and the removal rule is the statement above it"},
	}

	// THE ARMS, KEYED BY THE `how` CLAUSE EACH ONE HANDS THE EXIT. This is the floor, and it is a
	// disposition rather than the number 3: an arm with no row here is a refusal and a row naming
	// no arm is a refusal, so adding an arm or deleting one both go red and neither can be
	// mistaken for the other.
	arms := map[string]string{
		"carries no epoch digest at all": "the kind 0x0001 arm, inside Spec B section 5.4's open " +
			"acceptance window. Every group on the deployed alpha takes it.",
		"delivered, in a device wrap this device opened, a pq_secret this device has held": "the wrap-candidate arm, which reads the WIRE and is the one that had no guard at all.",
		"opened its epoch with a pq_secret this device has held":                           "the compatibility arm, decided by the same digest comparison and not by a version flag.",
	}

	declaration := parseFuncDecl(t, "pqepoch.go", "resolvePqSecretLocked")
	body := declaration.Body

	// ── THE ESCAPE HATCH, CLOSED BY CONSTRUCTION ────────────────────────────────────────────
	//
	// With UNNAMED results a bare `return` does not compile, so there is no return whose result
	// list is empty and the walk below cannot have one outside its subject. This is asserted and
	// not assumed because the gate this replaces was defeated by exactly that edit.
	for _, result := range declaration.Type.Results.List {
		if 0 < len(result.Names) {
			names := []string{}
			for _, name := range result.Names {
				names = append(names, name.Name)
			}
			t.Fatalf("the resolution declares NAMED results %v. Under named results a bare "+
				"`return` carries whatever was assigned above it, so an arm can answer a pq_secret "+
				"with a statement that has no result list at all -- which is outside this walk's "+
				"subject by construction and is how a fourth arm passed this gate with the whole "+
				"package green. Keep the results unnamed, or this gate has to be rewritten to key "+
				"on result POSITIONS and on the assignments that reach them", names)
		}
	}

	// THE EXIT, FOUND BY SHAPE AND REQUIRED TO BE UNIQUE. Two local function values would be two
	// places a secret could leave by and this gate would be measuring one of them.
	exits := []*ast.FuncLit{}
	for _, statement := range body.List {
		assign, isAssign := statement.(*ast.AssignStmt)
		if !isAssign {
			continue
		}
		for at, value := range assign.Rhs {
			literal, isLiteral := value.(*ast.FuncLit)
			if !isLiteral {
				continue
			}
			if name, ok := assign.Lhs[at].(*ast.Ident); !ok || name.Name != exit {
				t.Fatalf("the resolution holds a local function value named %q; this gate is "+
					"written against exactly one, named %q, and a second one is a second way out "+
					"with a secret in hand", exprText(assign.Lhs[at]), exit)
			}
			exits = append(exits, literal)
		}
	}
	if len(exits) != 1 {
		t.Fatalf("the resolution holds %d local function value(s) and this gate needs exactly one, "+
			"the guarded exit %q. If the exit was inlined back into the arms, every arm needs the "+
			"rule again and this gate has to be rewritten to find it there", len(exits), exit)
	}
	// AND THE READING IS LOGGED, WHICH IS THE POINT OF RETURNING IT. Both mutants that defeated
	// the clause this replaces left the complement below looking exactly as healthy as the correct
	// code's, so there was no line anywhere saying what the guard had been narrowed to. There is
	// one now, and it names the predicate the guard decides on.
	reading, defect := removalGuardDefect(exits[0])
	t.Logf("THE REMOVAL GUARD, AS THIS GATE READ IT: %s", reading)
	if defect != "" {
		t.Fatalf("%q does not refuse a removal with refuseRemovalOnHeldSecret before it answers: %s. "+
			"Every secret this function returns leaves by it, so an unguarded exit is every arm "+
			"unguarded at once", exit, defect)
	}

	// THE SUBJECT, COUNTED INDEPENDENTLY FIRST. This walk knows nothing about result lists or
	// about the exit: it counts [ast.ReturnStmt] nodes, full stop. The keyed walk below has to
	// agree with it, and a return the keyed walk cannot key is a disagreement between two numbers
	// rather than a silent skip.
	statements := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if _, isReturn := node.(*ast.ReturnStmt); isReturn {
			statements += 1
		}
		return true
	})

	// THE WALK: every return in the function, the exit's own included.
	sites := map[string]int{}
	refusals := map[string]int{}
	inExit := map[string]int{}
	keyed := 0
	within := false
	var visit func(node ast.Node)
	visit = func(node ast.Node) {
		ast.Inspect(node, func(child ast.Node) bool {
			if child == nil {
				return true
			}
			if literal, isLiteral := child.(*ast.FuncLit); isLiteral && child != node {
				was := within
				within = literal == exits[0]
				visit(literal.Body)
				within = was
				return false
			}
			ret, isReturn := child.(*ast.ReturnStmt)
			if !isReturn {
				return true
			}
			keyed += 1
			if len(ret.Results) == 0 {
				// UNREACHABLE WHILE THE RESULTS ARE UNNAMED, and keyed under its own name rather
				// than skipped so that it can never again be both present and invisible.
				sites["a bare return"] += 1
				return true
			}
			key := exprText(ret.Results[0])
			sites[key] += 1
			if within {
				inExit[key] += 1
			}
			// AND THE SECOND POSITION, so that "carries no secret" means "refuses" and not merely
			// "the first result is nil". A `return nil, nil` is neither a secret nor a refusal and
			// would be this function answering "no pq_secret and nothing wrong".
			if 2 <= len(ret.Results) && exprText(ret.Results[1]) != "nil" {
				refusals[key] += 1
			}
			return true
		})
	}
	visit(body)
	if keyed != statements {
		t.Fatalf("this gate keyed %d return statement(s) and the function has %d. The walk's "+
			"subject is supposed to be EVERY return, and a return it cannot key is one that could "+
			"carry a pq_secret without appearing in either list below", keyed, statements)
	}

	keys := []string{}
	for key := range sites {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	carrying, complement := []string{}, []string{}
	for _, key := range keys {
		entry, dispositioned := dispositions[key]
		if dispositioned && entry.carries {
			carrying = append(carrying, fmt.Sprintf("%s x%d", key, sites[key]))
			continue
		}
		complement = append(complement, fmt.Sprintf("%s x%d", key, sites[key]))
	}
	t.Logf("the resolution returns a pq_secret at %v; THE COMPLEMENT IS %v, and the assertion "+
		"below is that the complement is exactly the refusals", carrying, complement)

	// ── BOTH WAYS ───────────────────────────────────────────────────────────────────────────
	for _, key := range keys {
		entry, dispositioned := dispositions[key]
		if !dispositioned {
			t.Fatalf("the resolution returns %q in the secret position at %d site(s) and this gate "+
				"has no disposition for it. If it can carry a pq_secret it must leave through %q, "+
				"which compares it against this group's whole table before a removal is followed on "+
				"it; if it cannot, say so here. This is the exact reading under which "+
				"`candidate.secret` sat in this gate's printed complement while a removal removed "+
				"nothing", key, sites[key], exit)
		}
		if !entry.carries {
			// THE COMPLEMENT, ASSERTED. Anything that is not the one exit has to be a refusal, and
			// a refusal puts nil in the secret position. There is no third reading.
			if key != "nil" {
				t.Fatalf("%q is dispositioned as carrying no pq_secret (%s) and it is not `nil`; "+
					"the complement of the exit is supposed to be the refusals and nothing else",
					key, entry.why)
			}
			// AND A REFUSAL REFUSES. `return nil, nil` would be in the complement, would satisfy
			// every clause above, and would be this function answering "no pq_secret, nothing
			// wrong" -- which the caller follows into an epoch with no secret filed for it.
			if refusals[key] != sites[key] {
				t.Fatalf("%d of the %d `%s` return(s) put nil in the ERROR position too. A return "+
					"that carries no secret and no failure is this function saying nothing went "+
					"wrong while handing back nothing to follow the epoch with",
					sites[key]-refusals[key], sites[key], key)
			}
			continue
		}
		// A CARRYING SITE IS THE EXIT OR A CALL TO IT, and which one is decided by where it is
		// rather than by the disposition's say-so.
		if key == exit+"(...)" {
			if inExit[key] != 0 {
				t.Fatalf("%q calls itself, which is not a shape this gate can reason about", exit)
			}
			continue
		}
		if inExit[key] != sites[key] {
			t.Fatalf("%q is dispositioned as the exit's own answer (%s) and %d of its %d site(s) "+
				"are OUTSIDE %q, so a secret leaves this function without the removal rule",
				key, entry.why, sites[key]-inExit[key], sites[key], exit)
		}
	}
	for key, entry := range dispositions {
		if sites[key] == 0 {
			t.Fatalf("this gate disposes of %q (%s) and the resolution has no such return. A gate "+
				"that keeps rows for arms nobody has is a gate that has stopped measuring", key, entry.why)
		}
	}

	// ── AND THE FLOOR, WHICH IS THE ARMS THEMSELVES, HELD BOTH WAYS ─────────────────────────
	//
	// It used to be `sites[exit+"(...)"] < 3` -- a hardcoded count, which is the per-site case this
	// gate's own header says it is not, and which cannot tell a DELETED arm from a RENAMED one.
	// The subject is now each arm's `how` clause, which is the sentence its refusal prints, so an
	// arm that is added, removed or re-worded all land here and land differently.
	//
	// EVERY CALL OF THE EXIT IS CHECKED, not every call in return position, because a call bound
	// to a local and returned on the next line would otherwise be an arm this gate never sees.
	// That count is then held against the return-position count, which is what closes the
	// laundering road the carrying-site numbers rest on.
	calls := 0
	seen := map[string]int{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if name, ok := call.Fun.(*ast.Ident); !ok || name.Name != exit {
			return true
		}
		calls += 1
		if len(call.Args) != 2 {
			t.Fatalf("a call of %q takes %d argument(s); this gate reads the SECOND as the arm's "+
				"own `how` clause and cannot key an arm without one", exit, len(call.Args))
		}
		clause, isLiteral := call.Args[1].(*ast.BasicLit)
		if !isLiteral || clause.Kind != token.STRING {
			t.Fatalf("a call of %q hands %q as its `how` clause and this gate needs a string "+
				"literal: a clause assembled at run time is an arm no disposition can name, and "+
				"the refusal an operator reads is that clause", exit, exprText(call.Args[1]))
		}
		text, err := strconv.Unquote(clause.Value)
		if err != nil {
			t.Fatalf("the `how` clause %s does not unquote: %v", clause.Value, err)
		}
		seen[text] += 1
		return true
	})
	if calls != sites[exit+"(...)"] {
		t.Fatalf("%q is called %d time(s) and only %d of those calls are in RETURN position. A "+
			"call bound to a local and returned on the next line is an arm that answers a "+
			"pq_secret without ever appearing as a carrying site, which is what the counts below "+
			"rest on", exit, calls, sites[exit+"(...)"])
	}
	for clause, count := range seen {
		why, dispositioned := arms[clause]
		if !dispositioned {
			t.Fatalf("the resolution answers through %q with the clause %q at %d site(s) and this "+
				"gate has no disposition for that arm. Say what the arm is and why it can carry a "+
				"pq_secret, or the floor below is a number that stopped describing the function",
				exit, clause, count)
		}
		if count != 1 {
			t.Fatalf("the clause %q (%s) is handed to %q at %d sites; each arm's clause is what "+
				"tells one refusal from another in a log, so two arms sharing one is two causes "+
				"an operator cannot separate", clause, why, exit, count)
		}
	}
	for clause, why := range arms {
		if seen[clause] == 0 {
			t.Fatalf("this gate disposes of the arm %q (%s) and the resolution has no such arm. An "+
				"arm that was deleted and a row that was left behind are the same defect, and a "+
				"hardcoded floor could tell neither from a rename", clause, why)
		}
	}
	if sites["nil"] < 5 {
		t.Fatalf("the walk found %d refusal(s) in the resolution, which is fewer than this function "+
			"has; it is not walking the whole body", sites["nil"])
	}
}

// removalGuardDefect says what is WRONG with the resolution's one guarded exit, in one clause, or
// "" when nothing is. It answers (what it READ, what is wrong), so a narrowing shows up in a log
// line rather than in an absence.
//
// ── IT WAS EIGHT CLAUSES AND IT IS THREE. THE DELETION IS LEDGER RULING 46 ────────────────────
//
// Six consecutive rounds built or repaired this function and every one was defeated ONE LEVEL OF
// INDIRECTION FURTHER OUT: the VALUE was named, then the CALL was named and its result was not,
// then the RESULT was named and the path to it was not, then the POSITION was named and the
// binding was not, then the BINDING was deleted and the held test's ARGUMENT took the narrowing,
// and the caller's list after that. Enforcement is a semantic property and this class is not
// statically definable: a gate that must prove *this refusal fires for every input it should* is
// deciding a runtime question out of syntax, and each reading it adds is one more surface to route
// around.
//
// WHAT THE DELETED CLAUSES WERE STANDING IN FOR IS A SET OF INPUTS THAT MUST BE REFUSED, and those
// inputs are driven now, through the production receive path, by
// TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath.
// EACH DELETION BELOW IS A MEASUREMENT AND NOT A JUDGEMENT: the mutant the clause existed for was
// re-applied to `pqepoch.go` (sha256 a8d6faf57786 before and after every run) or to `group.go`
// (45028ef7b883), and the TABLE was measured going red on it.
//
//	DELETED CLAUSE                          THE MUTANT IT EXISTED FOR              THE TABLE
//	3. the guarded return is the exit's     a refusal returned from a nested       CAUGHT
//	   own, not a nested literal's           closure nothing calls
//	4. the refusal sits above every         the guard moved below the answer       CAUGHT
//	   answer
//	5. it is returned on EVERY PATH         `if len(removedLeaves) == 1` around    CAUGHT, by the
//	   through the guard -- one              the refusal; the same ANDed into       two- and
//	   branching statement, the held         the held test; the same as a switch    three-leaf rows
//	   test, refusal last in its body        with no default
//	6a. the polarity                        `if 1 < len(removedLeaves)`            CAUGHT
//	7. every value the guard decides on     `removedLeaves = removedLeaves[:1]`    CAUGHT
//	   is bound once and written             above the exit; the same shadowed
//	   nowhere                               inside it; the exit given its OWN
//	                                         `removedLeaves` parameter narrowed
//	                                         at all three call sites;
//	                                         `self.pqSecretWitness = nil`;
//	                                         `secret = held` above the answer
//	8. the guard is the exit's FIRST        the candidate zeroed through           CAUGHT
//	   statement                             `copy(secret, …)` under a leaf count
//	9. the caller hands BOTH doors the      `decision.RemovedLeaves[:1]` at        CAUGHT, because
//	   whole `decision.RemovedLeaves`        either call site, and a rewrite of     the refusal
//	                                         `decision` above both                  PRINTS the list
//	                                                                                and the table
//	                                                                                reads it back
//
// CLAUSE 8 EARNED ITS OWN LINE ON THE WAY OUT, and it is the sharpest argument for the deletion:
// it was written FOR the copy-through-`secret` mutant, and it does NOT catch that mutant when the
// copy is planted one line lower, inside the guard's own body. Measured: that plant passed this
// function and turned the table red. A clause that does not cover the class its own comment names
// is exactly what ruling 46 is about.
//
// ── AND THE THREE THAT ARE KEPT, EACH BECAUSE THE TABLE MEASURABLY DOES NOT COVER IT ─────────
//
//  1. THE CALL'S RESULT IS RETURNED. Every call of the refusal in the exit is counted, and so is
//     every call sitting in the ERROR position of a return; the two counts must agree. `_ =
//     refuse(...)`, `err := refuse(...)` with no use, and a call handed to a logger are one class.
//
//  2. AND THAT RETURN CARRIES NO SECRET -- the secret position must be exactly `nil`. THIS IS THE
//     CLAUSE THE TABLE CANNOT HAVE. `return secret, refuseRemovalOnHeldSecret(...)` was applied to
//     production and the whole table stayed GREEN, because [Group.ingestCommitLocked] reads the
//     ERROR first (`errors.Is(resolveErr, ErrRemovalWithoutRotation)`) and never looks at the
//     secret on that path. It is not a live defect today; it is a coupling to the CALLER's shape,
//     it becomes live the day that caller changes, and no input can reach it. That is what a
//     static clause is for and it is the whole of what one is for here.
//
//  3. THE GUARD'S OWN CONDITION IS READ WHOLE, AGAINST A WRITTEN DISPOSITION, AND A NAME IS
//     REFUSED FOR BEING A NAME. MEASURED:
//     `0 < len(removedLeaves) && len(removedLeaves) < 5 && alreadyHeld` -- the same narrowing one
//     arity above the interval the table drives -- passes the table and is refused here. A
//     condition that is a NAME is refused because that is the one road that defeated this clause
//     before: narrowing the BINDING by one token leaves the guard byte-identical.
//
//     WHAT THIS CLAUSE COVERS IS THE CONDITION IT READS AND IT IS NOT THE WHOLE ARITY AXIS, and
//     the correction is the 2026-09-24 (SEVENTH PASS) finding. The sentence that stood here said
//     *this clause is what covers the rest of the axis, and it covers it at the moment the
//     predicate is written*, and it was false as written: this walk reads TOP-LEVEL conditionals
//     of the exit, so while the guard was an outer `if 0 < len(removedLeaves)` wrapped around an
//     inner `if ...; alreadyHeld`, the inner condition was read by nothing and
//     `alreadyHeld && len(removedLeaves) < 4` ONE LINE LOWER passed this reading, the table and
//     the package (pqepoch.go sha256 553bd9fffa2c). The guard is ONE condition now, so there is
//     no second condition to hide in and re-nesting it is refused by this clause -- the row
//     `R12 the narrowing planted one line lower` drives exactly that, and the old outer-only
//     predicate is deliberately absent from the map below so a re-nest has no disposition.
//     What is still outside this clause is stated in the residual below.
//
// THE RESIDUAL, NAMED. This function reads the exit's REFUSING CONDITION and nothing else. It does
// not decide that the refusal is REACHED -- that is the table's -- it does not read any other
// statement of the exit, so a narrowing written into one (an early `return secret, nil` above the
// guard, or a rebinding of `alreadyHeld` between its binding and the guard) is invisible to it and
// is caught by the table at every arity the table drives and by NOTHING above that interval; and
// it says nothing about the other door, which has its own reading at [doorPredicateDefect] for its
// own reason: an adversary found that door had no gate on its own predicate at all, and a
// narrowing of it one arity above the table's is caught by neither instrument until that reading
// exists. The row `A2` below is that residual driven rather than described.
func removalGuardDefect(exit *ast.FuncLit) (string, string) {
	const refusal = "refuseRemovalOnHeldSecret"
	block := exit.Body

	// THE PREDICATE, DISPOSITIONED. One row, and a change to the guard's condition lands here as
	// "no disposition" rather than as a reading this gate reasons about on its own.
	//
	// AND THE ROW IS THE WHOLE CONDITION, WHICH IS THE 2026-09-24 (SEVENTH PASS) REPAIR. The row
	// that stood here was `0 < len(removedLeaves)` alone, because the guard was two nested
	// conditionals and this walk reads the top-level one: the held test sat one line lower and
	// nothing read it, so the narrowing this clause exists for passed by being written there. The
	// guard is one condition now and the old outer-only predicate is NOT dispositioned, so a
	// re-nested guard reads as `0 < len(removedLeaves)` and is refused for having no disposition.
	predicates := map[string]string{
		"0 < len(removedLeaves) && alreadyHeld": "the WHOLE of what decides the refusal, at the " +
			"one site that decides it: the commit's own removed-leaf list -- a parameter of the " +
			"resolution, with no binding between it and this condition -- and the held answer " +
			"[Group.pqSecretHeldAtLocked] gives for the value this epoch would be followed on. A " +
			"named bool here was narrowed by one token at its BINDING and this gate could not " +
			"see it, which is why the leaf test is written whole; the held test is a name because " +
			"it is a call's second result, and what that leaves open is in this reading's residual",
	}
	reading := "no guard was found to read"

	// inspect walks `node` without descending into a nested function literal, so a call inside a
	// closure the guard never runs is not counted as the guard's.
	inspect := func(node ast.Node, visit func(ast.Node) bool) {
		ast.Inspect(node, func(child ast.Node) bool {
			if child == nil {
				return true
			}
			if literal, isLiteral := child.(*ast.FuncLit); isLiteral && ast.Node(literal) != node {
				return false
			}
			return visit(child)
		})
	}
	isRefusalCall := func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return false
		}
		name, isIdent := call.Fun.(*ast.Ident)
		return isIdent && name.Name == refusal
	}

	// EVERY CALL OF THE REFUSAL IN THE EXIT, wherever it is and whatever is done with it.
	calls := 0
	inspect(block, func(node ast.Node) bool {
		if isRefusalCall(node) {
			calls += 1
		}
		return true
	})
	if calls == 0 {
		return reading, "it never calls " + refusal + " at all"
	}

	carried, guarded := 0, []token.Pos{}
	inspect(block, func(node ast.Node) bool {
		ret, isReturn := node.(*ast.ReturnStmt)
		if !isReturn || len(ret.Results) < 2 {
			return true
		}
		here := 0
		inspect(ret.Results[1], func(child ast.Node) bool {
			if isRefusalCall(child) {
				here += 1
			}
			return true
		})
		if here == 0 {
			return true
		}
		carried += here
		if exprText(ret.Results[0]) == "nil" {
			guarded = append(guarded, ret.Pos())
		}
		return true
	})

	// ── 1. THE RESULT IS RETURNED ───────────────────────────────────────────────────────────
	if carried != calls {
		return reading, fmt.Sprintf("%d of its %d call(s) of %s put the refusal in the error "+
			"position of a return; the rest compute it and drop it, which passes a gate that only "+
			"looks for the call and leaves the removal following the secret the removed member holds",
			carried, calls, refusal)
	}
	// ── 2. AND CARRIES NO SECRET ────────────────────────────────────────────────────────────
	if len(guarded) != calls {
		return reading, fmt.Sprintf("%d of its %d refusal(s) are returned beside a non-nil secret; "+
			"a guard that refuses AND hands the value over is a refusal the caller can read past",
			calls-len(guarded), calls)
	}
	// ── 3. AND THE CONDITION IT IS UNDER IS READ WHOLE ──────────────────────────────────────
	//
	// THE CONTAINMENT IS POSITIONAL AND THAT IS DELIBERATE NOW. The path walk this replaces
	// existed to refuse a second branching statement between the guard and the refusal; the table
	// drives that class at three arities and the walk is gone with the rest of clause 5. What is
	// left here is only "which conditional's condition am I reading", for which a byte range is
	// the right tool and not a weak version of a stronger one.
	onAPath := 0
	for _, statement := range block.List {
		conditional, isIf := statement.(*ast.IfStmt)
		if !isIf {
			continue
		}
		for _, at := range guarded {
			if at < conditional.Body.Pos() || conditional.Body.End() < at {
				continue
			}
			onAPath += 1
			condition := sourceText(conditional.Cond)
			reading = "the guard `if " + condition + "`"
			why, dispositioned := predicates[condition]
			if !dispositioned {
				if _, isName := boundName(conditional.Cond); isName {
					return reading, fmt.Sprintf("its refusal is guarded by the NAME %q and not by "+
						"a predicate this gate can read. Whatever that name is bound to is a level "+
						"of indirection between the commit and the guard, and narrowing the "+
						"BINDING by one token -- `%s := 0 < len(removedLeaves) && "+
						"len(removedLeaves) < 3` -- leaves this body byte-identical and passes "+
						"every behavioural case in this package. Write the predicate whole at the "+
						"guard", condition, condition)
				}
				return reading, fmt.Sprintf("its refusal is guarded by `%s` and this gate has no "+
					"disposition for that predicate. The guard's condition is the whole of what "+
					"decides whether a removal is checked at all, and it is the one thing the "+
					"driven table cannot cover past the arities it drives -- `0 < "+
					"len(removedLeaves) && len(removedLeaves) < 5 && alreadyHeld` passes every "+
					"row of it. AND A RE-NESTED GUARD LANDS HERE TOO: the old outer-only `0 < "+
					"len(removedLeaves)` is deliberately NOT dispositioned, because while the "+
					"guard was two nested conditionals this walk read the outer one and the "+
					"narrowing was written into the inner one. So a new predicate gets a row "+
					"here saying what it reads and why", condition)
			}
			reading = "the guard `if " + condition + "` (" + why + ")"
		}
	}
	if onAPath != len(guarded) {
		return reading, fmt.Sprintf("%d of its %d refusal(s) are outside a conditional at the top "+
			"level of the exit, so what refuses is not keyed to the commit removing a leaf",
			len(guarded)-onAPath, len(guarded))
	}
	return reading, ""
}

// doorPredicateDefect is [removalGuardDefect]'s clause 3 asked of THE OTHER DOOR,
// [Group.refuseUnrotatedRemovalLocked], and it is the one reading this pass ADDS while deleting
// six.
//
// WHY IT IS ADDED RATHER THAN INHERITED. An adversary found that door had no gate on its own
// predicate at any point in the six rounds: everything written was about the resolution's exit.
// The driven table now covers that door over the arity interval [0, 4], printed by the table
// itself, and MEASURED, the four narrowings an adversary would write there -- `2 <
// len(removedLeaves)` folded into the early return, `!= 1`, the door deleted outright, and the
// door made to refuse every removal digest or not -- are ALL caught by the table and by none of
// the six gates. What the table does not catch is the same narrowing one arity ABOVE what it
// drives: `len(removedLeaves) == 0 || 3 < len(removedLeaves)` was applied to production when the
// interval stopped at three and passed the table AND every gate, which is why this reading was
// added. RE-MEASURED on 2026-09-24 (seventh pass) at the widened interval, both sides: `|| 3 <
// len(removedLeaves)` is now caught by the TABLE -- the four-leaf row at this door is what
// catches it -- and `|| 4 < len(removedLeaves)`, one arity above the interval again, passes the
// table and is refused HERE. The edge moved by one and the shape of the answer did not, which is
// the argument for keeping both instruments rather than choosing. This reading closes it the way
// clause 3 does: the condition is read whole as source text, held against a written disposition
// BOTH WAYS, and a condition that is a NAME is refused for being a name.
//
// ITS SUBJECT IS EVERY CONDITIONAL SHAPE IN THE DOOR'S BODY, because this door decides by
// returning nil early: a predicate added ANYWHERE in it can let a removal past, and "the ones I
// thought to look at" is how the six rounds went.
//
// AND THAT SENTENCE WAS FALSE OF THIS CODE UNTIL 2026-09-24 (EIGHTH PASS), WHICH IS THE CLASS
// LEDGER ITEM 253 NAMES: a gate whose class is a shape the defect does not have. It said "every
// top-level conditional" in capitals and it walked `declaration.Body.List`, casting each element to
// `*ast.IfStmt` and skipping everything else in silence. An `else if` lives in `IfStmt.Else` and is
// not an element of `Body.List` at all; a `switch` is not an `*ast.IfStmt`. MEASURED, each applied
// to production and reverted, all three the SAME narrowing in three spellings:
//
//	D-A  `if 1 < self.epoch { return nil }` at the top level  (sha256 aa6068572817) -> GATES FAIL
//	D-D  `} else if 1 < self.epoch { return nil }` on `digest != nil`  (68d2fbf1c38c) -> GATES PASS
//	D-B  `switch { case 1 < self.epoch: return nil }`  (bdd81e69ac7f) -> GATES PASS
//
// All three passed the driven table, because every digest-less row of it sat at `self.epoch == 1`.
// D-A is the positive control and it fires for its own reason -- "this door decides on `1 <
// self.epoch` and this gate has no disposition for that predicate" -- so the only difference
// between the caught one and the two survivors was the SPELLING.
//
// WHAT WAS REPAIRED, IN BOTH INSTRUMENTS AND NOT ONLY HERE. The table now drives this door at TWO
// values of `self.epoch` and prints that interval beside its arity one, which is where a narrowing
// a driven case can catch belongs (ledger ruling 46). This reading is what covers the same
// narrowing ABOVE that interval, and it is widened rather than added: it walks the WHOLE body with
// [ast.Inspect] -- so an `else` chain, a `switch`, a type switch, a `select` and a loop are all
// reached -- and it refuses every conditional shape that is not one of the two dispositioned `if`s
// by NAME rather than skipping it in silence. Silently skipping a node it has no reading for is the
// one behaviour that made the header's own sentence false.
//
// AND [removalGuardDefect] IS DELIBERATELY NOT WIDENED WITH IT, because it does not have this
// hole and the difference is measured rather than assumed: its clause `onAPath != len(guarded)`
// requires every refusal to sit inside a TOP-LEVEL conditional, so a guard rewritten as `switch {
// case 0 < len(removedLeaves) && alreadyHeld: ... }` is refused there for its own reason -- *1 of
// its 1 refusal(s) are outside a conditional at the top level of the exit* -- while the table
// stays green. This door has no such clause because it decides by returning nil EARLY: there is no
// refusal position to anchor on, and that asymmetry is why only one of the two readings moved.
//
// HONEST BOUND ON WHAT A REGRESSION HERE COSTS, stated rather than inflated: a digest-less removal
// that slips this door is still refused after the apply by the resolution's guard -- `digest ==
// nil`, `isHeld`, so [Group.resolvePqSecretLocked] answers the held secret and the exit refuses it.
// What is lost is ruling 41's PRE-APPLY promise, that the receiver's MLS handle must not move, and
// not the removal rule itself. That promise is what the table's `receiver.handle.Epoch() !=
// handleAt` assertion exists for.
func doorPredicateDefect(declaration *ast.FuncDecl) ([]string, string) {
	predicates := map[string]string{
		"len(removedLeaves) == 0": "a commit that removes NOTHING is untouched by this door. It " +
			"is the complement of the rule and it is driven: a digest-less commit that removes " +
			"nobody reaches the resolution's no-digest arm and is followed",
		"digest != nil": "a removal that CARRIES a digest is let past here and judged at the " +
			"resolution against that digest, which no third party can move. Everything this point " +
			"could still read is a statement about the wire rather than about the commit, and a " +
			"permanent halt may not rest on a record a bystander can write",
	}
	// EVERY `if` THAT IS SOMEBODY'S `else` IS MARKED FIRST, so a refusal taken in an else chain can
	// say WHERE it was found. Without it D-D would be refused with D1's sentence and the row would
	// keep passing after the widening that exists for it was reverted.
	inAnElseChain := map[ast.Node]bool{}
	ast.Inspect(declaration.Body, func(node ast.Node) bool {
		conditional, isIf := node.(*ast.IfStmt)
		if !isIf || conditional.Else == nil {
			return true
		}
		// THE WHOLE ELSE SUBTREE AND NOT ITS ROOT, so a conditional nested one block deeper inside
		// an `else { ... }` is reported where it really is rather than as a top-level one. A
		// location a reading states wrongly is a sentence the next reader has to re-derive.
		ast.Inspect(conditional.Else, func(inner ast.Node) bool {
			inAnElseChain[inner] = true
			return true
		})
		return true
	})
	read, seen, defect := []string{}, map[string]bool{}, ""
	// THE WALK IS THE WHOLE BODY AND NOT ITS TOP LEVEL, which is the 2026-09-24 (eighth pass)
	// repair: `Body.List` holds neither an `else if` (it is `IfStmt.Else`) nor a `switch` case, and
	// both were MEASURED letting the same narrowing through while this walk reported the door
	// clean. Every conditional shape reached here is either one of the two dispositioned `if`s or
	// is REFUSED BY NAME -- a node skipped in silence is what made this function's own header false.
	ast.Inspect(declaration.Body, func(node ast.Node) bool {
		if defect != "" {
			return false
		}
		where := "at the top level of this door"
		if inAnElseChain[node] {
			where = "in an ELSE CHAIN -- which lives in `IfStmt.Else` and is not an element of " +
				"`Body.List` at all, so a walk over the body's top level cannot see it"
		}
		switch shape := node.(type) {
		case *ast.IfStmt:
			condition := sourceText(shape.Cond)
			read = append(read, "`if "+condition+"` ("+where+")")
			if _, isName := boundName(shape.Cond); isName {
				defect = fmt.Sprintf("this door decides on the NAME %q, %s. Whatever that name is "+
					"bound to is a level of indirection between the commit and the decision, and "+
					"it is the road that defeated the resolution's own guard: narrowing the "+
					"BINDING by one token leaves the door byte-identical. Write the predicate "+
					"whole", condition, where)
				return false
			}
			why, dispositioned := predicates[condition]
			if !dispositioned {
				defect = fmt.Sprintf("this door decides on `%s`, %s, and this gate has no "+
					"disposition for that predicate. Every predicate here can let a removal past, "+
					"and the driven table cannot cover one past the intervals it drives -- "+
					"`len(removedLeaves) == 0 || 4 < len(removedLeaves)` passes every row of it, "+
					"and so does `2 < self.epoch`. So a new predicate gets a row saying what it "+
					"reads and why", condition, where)
				return false
			}
			seen[condition] = true
			read[len(read)-1] = "`if " + condition + "` (" + why + ")"
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.ForStmt, *ast.RangeStmt:
			kind := "an unnamed conditional shape"
			switch shape.(type) {
			case *ast.SwitchStmt:
				kind = "a SWITCH statement"
			case *ast.TypeSwitchStmt:
				kind = "a TYPE SWITCH statement"
			case *ast.SelectStmt:
				kind = "a SELECT statement"
			case *ast.ForStmt:
				kind = "a FOR loop"
			case *ast.RangeStmt:
				kind = "a RANGE loop"
			}
			read = append(read, kind)
			defect = fmt.Sprintf("this door holds %s. This gate disposes of PLAIN `if` conditions "+
				"and has no reading for any other conditional shape, and every one of them can "+
				"return early exactly as an `if` can: `switch { case 1 < self.epoch: return nil }` "+
				"was MEASURED passing the driven table AND every gate here (pqepoch.go sha256 "+
				"bdd81e69ac7f) while this walk looked only at `*ast.IfStmt` elements of the body's "+
				"top level. Write the predicate as an `if` and give it a row above", kind)
			return false
		}
		return true
	})
	if defect != "" {
		return read, defect
	}
	for condition, why := range predicates {
		if !seen[condition] {
			return read, fmt.Sprintf("this gate disposes of `%s` (%s) and the door has no such "+
				"condition. A predicate that was deleted and a row left behind are the same "+
				"defect, and a count could tell neither from a rewrite", condition, why)
		}
	}
	return read, ""
}

// boundName answers the identifier a condition is, when the condition is nothing but a name --
// through parentheses and through a negation, because `!removesLeaves` is the same indirection as
// `removesLeaves` and refusing only the second would be a gate scoped to one spelling of one
// bypass, which is the class ledger ruling 46 is about. It answers false for every condition that
// reads something.
func boundName(condition ast.Expr) (string, bool) {
	for {
		switch shape := condition.(type) {
		case *ast.Ident:
			return shape.Name, true
		case *ast.ParenExpr:
			condition = shape.X
		case *ast.UnaryExpr:
			if shape.Op != token.NOT {
				return "", false
			}
			condition = shape.X
		default:
			return "", false
		}
	}
}

// sourceText is a node's own source, printed back from the tree. It is what lets a condition be
// held as TEXT against a disposition instead of being reasoned about node by node.
func sourceText(node ast.Node) string {
	buffer := bytes.Buffer{}
	if err := printer.Fprint(&buffer, token.NewFileSet(), node); err != nil {
		return fmt.Sprintf("%T (unprintable: %v)", node, err)
	}
	return strings.Join(strings.Fields(buffer.String()), " ")
}

// ── 4c. THE TWO SURVIVING READINGS HAVE THEIR OWN MUTANTS, AS ROWS ──────────────────────────

// guardShape is the shape [Group.resolvePqSecretLocked] has, reduced to what
// [removalGuardDefect] reads. It is a stand-in for the production function and it is held against
// the production function on a row below: A0 asserts that this template is ACCEPTED and the real
// resolution is accepted too, so a template that had drifted into some other shape would make
// every refusal under it vacuous and would say so here rather than pass quietly.
//
// It is never compiled -- [parser.ParseFile] does not resolve `message` or `errNoSecret` -- which
// is the point: a mutant that would not compile in production still has to be REFUSED by the
// reading, and that is a stronger bar than "the tree is green with it in".
const guardShape = `package urmessage

func (self *Group) resolvePqSecretLocked(mlsSecret []byte, opensEpoch uint64,
	digest *message.EpochDigestAttachment, removedLeaves []uint32) ([]byte, error) {

	held, isHeld := self.pqSecretAtLocked(self.epoch)
	self.stats.WrapMissing += 1
	answerSecret := func(secret []byte, how string) ([]byte, error) {
		heldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret)
		if 0 < len(removedLeaves) && alreadyHeld {
			return nil, refuseRemovalOnHeldSecret(opensEpoch, removedLeaves, heldAt, how)
		}
		return secret, nil
	}
	if isHeld {
		return answerSecret(held, "carries no epoch digest at all")
	}
	return nil, errNoSecret
}
`

// doorShape is [Group.refuseUnrotatedRemovalLocked] reduced to what [doorPredicateDefect] reads,
// and it stands in the same relation to its subject that [guardShape] does to its own.
const doorShape = `package urmessage

func (self *Group) refuseUnrotatedRemovalLocked(digest *message.EpochDigestAttachment, removedLeaves []uint32) error {
	if len(removedLeaves) == 0 {
		return nil
	}
	if digest != nil {
		return nil
	}
	return fmt.Errorf("%w: the commit that would open epoch %d removes %d leaf/leaves",
		ErrRemovalWithoutRotation, self.epoch+1, len(removedLeaves))
}
`

// BOTH REMOVAL DOORS ARE DECIDED BY A PREDICATE WRITTEN WHOLE, AND THE RESOLUTION'S REFUSAL IS
// RETURNED RATHER THAN COMPUTED.
//
// WHAT THIS TEST IS AFTER LEDGER RULING 46. It held EIGHTEEN synthetic bodies against eight
// clauses and a caller walk. Six of those clauses are deleted, each one measured being caught by
// the driven table instead -- the deletion table is in [removalGuardDefect]'s header, mutant by
// mutant -- so the rows that existed for them are deleted with them. A row kept for a clause that
// is gone would be a refusal nothing needs, and this project has already been bitten by a gate
// whose header named a class its code did not cover.
//
// WHAT IS LEFT IS SIXTEEN ROWS ACROSS TWO READINGS, and every one of them exists because the
// mutant under it was measured PASSING the driven table:
//
//   - the resolution's exit: the refusal computed and dropped, the refusal returned beside the
//     secret, a narrowed predicate, a predicate hidden behind a NAME, and -- the 2026-09-24
//     (seventh pass) addition -- the same narrowing planted ONE LINE LOWER in a re-nested guard,
//     which is the placement that passed both instruments while this walk read only the outer of
//     two conditions;
//   - the other door: a narrowed predicate, a predicate hidden behind a NAME, and -- the
//     2026-09-24 (eighth pass) addition -- the SAME narrowing in two spellings this reading could
//     not see at all, an `else if` and a `switch`, both measured passing every instrument while
//     the identical predicate written as a top-level `if` was caught.
//
// AND EACH REFUSAL NAMES THE CLAUSE IT IS REFUSED BY. Without that, six rows could all be refused
// by clause 1 and the table would be six copies of one measurement.
//
// AND THE ACCEPTED ROWS ARE WHAT MAKE THE REFUSALS MEAN SOMETHING: the two shapes themselves, a
// legitimate statement above the refusal, and -- deliberately -- this reading's OWN RESIDUAL,
// a narrowing of the held answer between its binding and the guard, which is accepted here and
// is the table's to catch. A residual carried as an accepted row is one somebody measures.
func TestBothRemovalDoorsAreDecidedByAPredicateWrittenWholeAndTheRefusalIsReturned(t *testing.T) {
	exitRows := []struct {
		row      string
		was      string
		now      string
		accepted bool
		names    string
		why      string
	}{
		{row: "A0 the shape itself", accepted: true,
			why: "THE CONTROL. Every refusal below is a mutation of this source, so if this row " +
				"were refused the other rows would be refusals of something already broken"},
		{row: "A1 a statement above the refusal, inside the guard's body",
			was:      "\t\t\treturn nil, refuseRemovalOnHeldSecret(",
			now:      "\t\t\t_ = how\n\t\t\treturn nil, refuseRemovalOnHeldSecret(",
			accepted: true,
			why: "nothing between `this value is held` and the refusal is this reading's " +
				"business any more -- the table drives that class at four arities -- and a gate " +
				"that refused this would be refusing a comment or a log line"},
		{row: "A2 the held answer, narrowed between its binding and the guard",
			was: "\t\theldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret)\n",
			now: "\t\theldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret)\n" +
				"\t\talreadyHeld = alreadyHeld && len(removedLeaves) < 5\n",
			accepted: true,
			why: "THIS READING'S RESIDUAL, AS A ROW RATHER THAN AS A PARAGRAPH. The held answer " +
				"is a name because it is a call's second result, and a statement that rewrites it " +
				"before the guard is a statement this reading does not look at. It is ACCEPTED " +
				"here and the table is what catches it. AND THE MEASUREMENT IN THIS ROW USED TO " +
				"BE ENTIRELY ON THE ARITY AXIS, which was the 2026-09-24 (eighth pass) blocker: " +
				"the exit decides on THREE inputs -- `len(removedLeaves)`, the held answer (which " +
				"carries `heldAt`) and the arm -- and this row measured a narrowing on the first " +
				"of them only, so `alreadyHeld = alreadyHeld && heldAt < 3` (pqepoch.go sha256 " +
				"1608f28c11c9) sat at ARITY ONE, inside the printed arity interval, and passed " +
				"the table, both these gates and all 171 cases in the package. MEASURED on " +
				"production, both axes and both sides of each edge: `&& len(removedLeaves) < 4` " +
				"turns the TABLE RED and `< 5` does not; `&& heldAt < 3` turns the TABLE RED now " +
				"that a row drives a refusal at heldAt 3, and `heldAt < 4` does not. Each axis " +
				"has a printed interval and each has an edge one step above it, which is why the " +
				"table prints BOTH and says both are BOUNDED. An accepted row is the honest way " +
				"to carry it, because a residual written only in a header is one nobody measures"},
		{row: "R9 the refusal, computed and dropped",
			was:   "\t\t\treturn nil, refuseRemovalOnHeldSecret(",
			now:   "\t\t\t_ = refuseRemovalOnHeldSecret(",
			names: "compute it and drop it",
			why: "the 2026-09-24 (third pass) blocker and ledger item 254's `owed from this " +
				"pass`. The table catches it too; it is kept because it is the same walk clause 2 " +
				"rests on"},
		{row: "R10 the refusal, returned beside the secret",
			was:   "\t\t\treturn nil, refuseRemovalOnHeldSecret(",
			now:   "\t\t\treturn secret, refuseRemovalOnHeldSecret(",
			names: "beside a non-nil secret",
			why: "MEASURED PASSING THE DRIVEN TABLE, and this row is the whole argument for " +
				"keeping a static reading here at all: ingestCommitLocked reads the ERROR first, " +
				"so no input can reach the secret this hands back. It is a coupling to the " +
				"caller's shape and it becomes live the day that caller changes"},
		{row: "R6 the conjunction, left on the condition",
			was:   "\t\tif 0 < len(removedLeaves) && alreadyHeld {",
			now:   "\t\tif 0 < len(removedLeaves) && len(removedLeaves) < 5 && alreadyHeld {",
			names: "has no disposition for that predicate",
			why: "MEASURED PASSING THE DRIVEN TABLE at `< 5`, which is one arity above the " +
				"interval the table prints. It is refused by the reading rather than by a list " +
				"of banned operators"},
		{row: "R12 the narrowing planted one line lower, in a RE-NESTED guard -- the 2026-09-24 " +
			"(seventh pass) blocker",
			was: "\t\theldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret)\n" +
				"\t\tif 0 < len(removedLeaves) && alreadyHeld {\n" +
				"\t\t\treturn nil, refuseRemovalOnHeldSecret(opensEpoch, removedLeaves, heldAt, how)\n" +
				"\t\t}\n",
			now: "\t\tif 0 < len(removedLeaves) {\n" +
				"\t\t\tif heldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret); alreadyHeld && len(removedLeaves) < 4 {\n" +
				"\t\t\t\treturn nil, refuseRemovalOnHeldSecret(opensEpoch, removedLeaves, heldAt, how)\n" +
				"\t\t\t}\n\t\t}\n",
			names: "has no disposition for that predicate",
			why: "THE SHAPE THIS PASS EXISTS FOR, and it is the guard as production had it plus " +
				"one token. This walk reads TOP-LEVEL conditionals, so while the guard was two " +
				"nested ifs the inner one was read by nothing: MEASURED against production at " +
				"pqepoch.go sha256 553bd9fffa2c, the table was ok, BOTH predicate gates were ok, " +
				"and the rule was gone for every commit removing four or more leaves. It is " +
				"refused now because the re-nested outer condition `0 < len(removedLeaves)` is " +
				"no longer a dispositioned predicate -- which is why the map above holds the " +
				"WHOLE condition and not the leaf test alone"},
		{row: "R11 the held answer under another name",
			was: "\t\theldAt, alreadyHeld := self.pqSecretHeldAtLocked(secret)\n" +
				"\t\tif 0 < len(removedLeaves) && alreadyHeld {",
			now: "\t\theldAt, isHeldValue := self.pqSecretHeldAtLocked(secret)\n" +
				"\t\tif 0 < len(removedLeaves) && isHeldValue {",
			names: "has no disposition for that predicate",
			why: "THE OTHER ROW THAT COSTS SOMETHING. A rename is not a narrowing and this " +
				"reading refuses it anyway, because the disposition is held as TEXT and the held " +
				"answer is now inside the text. That price is paid once per edit, by writing the " +
				"new condition into the map; the alternative is a reading that chases a name, " +
				"which is the road ledger ruling 46 closed"},
		{row: "R1 the predicate behind a name, narrowed by one token -- the 2026-09-24 " +
			"(fifth pass) blocker",
			was: "\t\tif 0 < len(removedLeaves) && alreadyHeld {",
			now: "\t\tremovesHeld := 0 < len(removedLeaves) && len(removedLeaves) < 3 && alreadyHeld\n" +
				"\t\tif removesHeld {",
			names: "guarded by the NAME",
			why: "REPRODUCED against production at pqepoch.go sha256 618428439f27…: gate PASS, " +
				"169/169 cases PASS, and the same complement the correct code logs. This one the " +
				"table DOES catch at three leaves; the row stays because the NAME is the road " +
				"that would put `< 5` back out of clause 3's sight"},
		{row: "R2 the honest named form, narrowed by nothing at all",
			was: "\t\tif 0 < len(removedLeaves) && alreadyHeld {",
			now: "\t\tremovesHeld := 0 < len(removedLeaves) && alreadyHeld\n" +
				"\t\tif removesHeld {",
			names: "guarded by the NAME",
			why: "THE ROW THAT COSTS SOMETHING, and it is deliberate. It is behaviourally " +
				"identical to R1 and is refused because this reading cannot tell the two apart " +
				"without chasing the binding. The indirection is refused, not the narrowing"},
	}

	refused, accepted := 0, 0
	for _, row := range exitRows {
		source := guardShape
		if row.was != "" {
			// THE SWAP ITSELF IS ASSERTED. A mutant that stopped matching its anchor would be a
			// row that passes by not being applied, which is the quietest way a table like this
			// dies.
			if count := strings.Count(source, row.was); count != 1 {
				t.Fatalf("%s: its anchor %q appears %d time(s) in the shape and has to appear "+
					"exactly once; this row is not measuring what it says it is", row.row, row.was, count)
			}
			source = strings.Replace(source, row.was, row.now, 1)
		}
		reading, defect := guardShapeDefect(t, row.row, source)
		t.Logf("%s -> %s | READ: %s", row.row,
			map[bool]string{true: "ACCEPTED", false: "REFUSED: " + defect}[defect == ""], reading)
		if row.accepted {
			accepted += 1
			if defect != "" {
				t.Fatalf("%s is supposed to be ACCEPTED (%s) and this reading refused it: %s. "+
					"Every refusal in this table is a mutation of this source, so a reading that "+
					"refuses the source refuses everything and measures nothing", row.row, row.why, defect)
			}
			continue
		}
		refused += 1
		if defect == "" {
			t.Fatalf("%s is supposed to be REFUSED (%s) and this reading ACCEPTED it. It read: %s",
				row.row, row.why, reading)
		}
		if !strings.Contains(defect, row.names) {
			t.Fatalf("%s is refused, and not for its own reason: this reading answered %q and the "+
				"clause this row exists for says %q. A row refused by some other clause is a row "+
				"that would keep passing after the clause it was written for is deleted",
				row.row, defect, row.names)
		}
	}

	// ── AND THE PRODUCTION EXIT TAKES THE SAME READING ──────────────────────────────────────
	//
	// [guardShape] is a stand-in, and a stand-in that has drifted from its subject is a table
	// measuring a file nobody ships.
	declaration := parseFuncDecl(t, "pqepoch.go", "resolvePqSecretLocked")
	production := (*ast.FuncLit)(nil)
	for _, statement := range declaration.Body.List {
		assign, isAssign := statement.(*ast.AssignStmt)
		if !isAssign {
			continue
		}
		for _, value := range assign.Rhs {
			if literal, isLiteral := value.(*ast.FuncLit); isLiteral {
				production = literal
			}
		}
	}
	if production == nil {
		t.Fatalf("pqepoch.go's resolvePqSecretLocked holds no local function value, so the exit " +
			"this table stands in for does not exist")
	}
	reading, defect := removalGuardDefect(production)
	t.Logf("the PRODUCTION resolution reads: %s", reading)
	if defect != "" {
		t.Fatalf("the production resolution is refused by this reading: %s", defect)
	}

	// ── AND THE OTHER DOOR, WHICH HAD NO READING OF ITS OWN AT ALL ──────────────────────────
	doorRows := []struct {
		row      string
		was      string
		now      string
		accepted bool
		names    string
		why      string
	}{
		{row: "D0 the door itself", accepted: true,
			why: "THE CONTROL, for A0's reason"},
		{row: "D1 the early return widened by one arity above the table",
			was:   "\tif len(removedLeaves) == 0 {",
			now:   "\tif len(removedLeaves) == 0 || 4 < len(removedLeaves) {",
			names: "no disposition for that predicate",
			why: "MEASURED PASSING THE DRIVEN TABLE AND EVERY ONE OF THE SIX GATES when it was " +
				"written at `3 <`, which was then one arity above the table; the table drives " +
				"four leaves at this door now, so the row moved up with it and was re-measured " +
				"at `4 <`. It was the one mutation in the whole set that nothing caught, and it " +
				"is why this reading was added while six clauses were deleted"},
		{row: "D2 the let-past widened",
			was:   "\tif digest != nil {",
			now:   "\tif digest != nil || 4 < len(removedLeaves) {",
			names: "no disposition for that predicate",
			why: "the same narrowing at the door's OTHER predicate. Both can let a removal past, " +
				"so both are read"},
		{row: "D3 the predicate hidden behind a name",
			was:   "\tif len(removedLeaves) == 0 {",
			now:   "\tremovesLeaves := 0 < len(removedLeaves)\n\tif !removesLeaves {",
			names: "decides on the NAME",
			why: "the road that defeated the resolution's own guard four rounds running, refused " +
				"here before it is walked rather than after"},
		{row: "D4 the same narrowing welded on as an ELSE IF -- the 2026-09-24 (eighth pass) blocker",
			was: "\tif digest != nil {\n\t\treturn nil\n\t}\n",
			now: "\tif digest != nil {\n\t\treturn nil\n\t} else if 1 < self.epoch {\n" +
				"\t\treturn nil\n\t}\n",
			names: "in an ELSE CHAIN",
			why: "MEASURED PASSING THE DRIVEN TABLE AND EVERY GATE HERE, against production at " +
				"pqepoch.go sha256 68d2fbf1c38c, with the whole package green at 171 PASS / 0 " +
				"FAIL. Its positive control is D-A, the SAME predicate as a top-level `if` (sha256 " +
				"aa6068572817), which this reading always caught -- so the only difference between " +
				"the caught spelling and this one was that `IfStmt.Else` is not an element of " +
				"`Body.List`. The table drives this door at two epochs now and catches it too; " +
				"this row is what covers the same narrowing ABOVE that interval"},
		{row: "D5 the same narrowing as a SWITCH -- the other half of the same blocker",
			was:   "\tif len(removedLeaves) == 0 {\n\t\treturn nil\n\t}\n",
			now:   "\tif len(removedLeaves) == 0 {\n\t\treturn nil\n\t}\n\tswitch {\n\tcase 1 < self.epoch:\n\t\treturn nil\n\t}\n",
			names: "holds a SWITCH statement",
			why: "the second spelling, MEASURED at pqepoch.go sha256 bdd81e69ac7f: table PASS, " +
				"gates PASS, package 171 PASS / 0 FAIL. A switch is not an `*ast.IfStmt` and was " +
				"skipped in silence, which is the behaviour that made this function's header -- " +
				"`ITS SUBJECT IS EVERY TOP-LEVEL CONDITIONAL` -- false of its own code. It is " +
				"refused for the shape and not for the predicate, so the row survives a rename of " +
				"`self.epoch`"},
	}
	for _, row := range doorRows {
		source := doorShape
		if row.was != "" {
			if count := strings.Count(source, row.was); count != 1 {
				t.Fatalf("%s: its anchor %q appears %d time(s) in the door's shape and has to "+
					"appear exactly once", row.row, row.was, count)
			}
			source = strings.Replace(source, row.was, row.now, 1)
		}
		read, defect := doorShapeDefect(t, row.row, source)
		t.Logf("%s -> %s | READ: %v", row.row,
			map[bool]string{true: "ACCEPTED", false: "REFUSED: " + defect}[defect == ""], read)
		if row.accepted {
			accepted += 1
			if defect != "" {
				t.Fatalf("%s is supposed to be ACCEPTED (%s) and this reading refused it: %s",
					row.row, row.why, defect)
			}
			continue
		}
		refused += 1
		if defect == "" {
			t.Fatalf("%s is supposed to be REFUSED (%s) and this reading ACCEPTED it. It read: %v",
				row.row, row.why, read)
		}
		if !strings.Contains(defect, row.names) {
			t.Fatalf("%s is refused, and not for its own reason: this reading answered %q and the "+
				"clause this row exists for says %q", row.row, defect, row.names)
		}
	}
	doorRead, doorDefect := doorPredicateDefect(parseFuncDecl(t, "pqepoch.go", "refuseUnrotatedRemovalLocked"))
	t.Logf("the PRODUCTION door reads: %v", doorRead)
	if doorDefect != "" {
		t.Fatalf("the production door is refused by this reading: %s", doorDefect)
	}

	if accepted != 4 || refused != 12 {
		t.Fatalf("this table ran %d accepted row(s) and %d refused one(s); it is written as 4 and "+
			"12, and a row that was deleted rather than answered is what this count is here to find",
			accepted, refused)
	}
}

// guardShapeDefect parses one row of the exit table and asks [removalGuardDefect] about it. It
// finds the exit the way the gate itself does -- the one local function value in the resolution --
// so a mutant that moves the exit is refused by the same reading rather than by this helper.
func guardShapeDefect(t *testing.T, row string, source string) (string, string) {
	t.Helper()
	for _, function := range parsedFuncsOf(t, row, source) {
		if function.Name.Name != "resolvePqSecretLocked" {
			continue
		}
		for _, statement := range function.Body.List {
			assign, isAssign := statement.(*ast.AssignStmt)
			if !isAssign {
				continue
			}
			for _, value := range assign.Rhs {
				literal, isLiteral := value.(*ast.FuncLit)
				if !isLiteral {
					continue
				}
				return removalGuardDefect(literal)
			}
		}
		t.Fatalf("%s holds no local function value to read as the exit", row)
	}
	t.Fatalf("%s holds no resolvePqSecretLocked", row)
	return "", ""
}

// doorShapeDefect is guardShapeDefect for the other door.
func doorShapeDefect(t *testing.T, row string, source string) ([]string, string) {
	t.Helper()
	for _, function := range parsedFuncsOf(t, row, source) {
		if function.Name.Name != "refuseUnrotatedRemovalLocked" {
			continue
		}
		return doorPredicateDefect(function)
	}
	t.Fatalf("%s holds no refuseUnrotatedRemovalLocked", row)
	return nil, ""
}

// parsedFuncsOf is the one parse both shape tables go through.
func parsedFuncsOf(t *testing.T, row string, source string) []*ast.FuncDecl {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "shape.go", source, parser.ParseComments)
	if err != nil {
		t.Fatalf("%s does not parse: %v", row, err)
	}
	functions := []*ast.FuncDecl{}
	for _, declaration := range parsed.Decls {
		function, isFunc := declaration.(*ast.FuncDecl)
		if !isFunc || function.Body == nil {
			continue
		}
		functions = append(functions, function)
	}
	return functions
}

// ── 4b. EVERY COUNTER THE RESOLUTION MOVES IS MOVED WHERE ITS SUBJECT IS FOUND ───────────────

// A COUNTER OF RECORDS IS ADDED ON A PATH EVERY RETURN PASSES; A COUNTER OF EPOCHS IS ADDED ON THE
// ARM THAT DECIDES THE EPOCH. BOTH WAYS, AND FOR EVERY COUNTER THE RESOLUTION CAN MOVE.
//
// WHY THIS GATE EXISTS, and it is the same class twice rather than one bug. [Stats.WrapOrphaned]
// was added inside the arm that REPORTS an orphan, so it read 0 on every arm that answers a secret
// -- which is every arm a healthy group takes. That was repaired by moving it below the candidate
// loop, and the repair had the defect's own shape one level down: the two arms that return BEFORE
// the loop (a digest-less commit, which is every group on the deployed alpha, and a commit whose
// digest names another epoch) still read 0. [Stats.WrapUnreadable] had it in a third dress: it sat
// inside the arm that reports an unreadable wrap, which the orphan arm returns ahead of, so a device
// whose own wrap did not open read 0 for it whenever anything else had opened beside it.
//
// SO THE SUBJECT IS NOT A COUNTER, IT IS THE RULE. Every `self.stats.…` assignment the resolution
// makes is keyed by the counter's name and held against a written disposition BOTH WAYS -- a site
// with no row is a refusal, a row naming no site is a refusal -- and each row says which of two
// things that counter counts:
//
//   - RECORDS ("wraps that opened and were not the epoch's own secret", "wraps at this device's own
//     handle that did NOT open"). Its site must be on a path EVERY return passes, which is the
//     deferred block at the top of the function. An arm added later inherits it.
//   - EPOCHS ("epochs this device could not take a pq_secret for"). Its site must NOT be there: the
//     deferred block runs whatever the function answers, so an epoch counter in it would count an
//     epoch that was resolved. The asymmetry is asserted in both directions so that "move
//     everything into the defer" fails too.
//
// AND THE MECHANISM IS CHECKED AND NOT ONLY THE PLACEMENT. The deferred block has to be a direct
// statement of the function body -- a defer nested inside an `if` is registered conditionally, which
// is the every-return property lost in a shape that still reads as a defer -- and it has to be
// registered BEFORE the first return statement in the function, because a defer below an early
// return never runs for that return.
//
// THE RESIDUAL, NAMED: this gate reads placement, not reachability. A deferred block whose own body
// wrapped the additions in a condition would satisfy every clause here. What holds that is the
// behaviour, and it is driven for all four arms of the resolution by
// TestAnOrphanIsCountedOnEveryArmOfTheResolutionAndInBothRaceOrderings and by (d) of
// TestAnHonestRotatedRemovalIsNeverCalledUnrotatedWhateverElseIsStagedBesideTheVictimsWrap, which is
// the case where an unreadable wrap and an orphan are both true at once.
func TestEveryCounterTheResolutionMovesIsMovedWhereItsSubjectIsFound(t *testing.T) {
	dispositions := map[string]struct {
		everyExit bool
		why       string
	}{
		"WrapOrphaned": {everyExit: true,
			why: "RECORDS: wraps that opened and were not the epoch's own secret. Which arm the " +
				"function answers on does not change how many of them there were"},
		"WrapUnreadable": {everyExit: true,
			why: "RECORDS: wraps at this device's own wrap_target_handle that did not open. It is " +
				"a count of rows and not of epochs, so the arm that reports it is not the arm that " +
				"decides how many there are"},
		"WrapMissing": {everyExit: false,
			why: "EPOCHS: epochs this device could not take a pq_secret for at all. Its subject is " +
				"the answer and not a row, so its site is the arm that takes that answer"},
	}

	declaration := parseFuncDecl(t, "pqepoch.go", "resolvePqSecretLocked")
	body := declaration.Body

	// ── THE DEFERRED BLOCK, FOUND BY SHAPE AND REQUIRED TO BE THE ONLY ONE ──────────────────
	deferred := []*ast.DeferStmt{}
	ast.Inspect(body, func(node ast.Node) bool {
		if statement, isDefer := node.(*ast.DeferStmt); isDefer {
			deferred = append(deferred, statement)
		}
		return true
	})
	if len(deferred) != 1 {
		t.Fatalf("the resolution registers %d deferred call(s) and this gate is written against "+
			"exactly one: the every-exit counter block. Two would be two orders to reason about and "+
			"this gate would be measuring one of them", len(deferred))
	}
	atTopLevel := false
	for _, statement := range body.List {
		if statement == ast.Stmt(deferred[0]) {
			atTopLevel = true
		}
	}
	if !atTopLevel {
		t.Fatalf("the resolution's defer is nested inside another statement. A defer inside an `if` " +
			"or a loop is registered CONDITIONALLY, so the every-return property is gone while the " +
			"shape still reads as a defer")
	}
	literal, isLiteral := deferred[0].Call.Fun.(*ast.FuncLit)
	if !isLiteral {
		t.Fatalf("the resolution defers %q rather than a function literal; this gate reads the "+
			"literal's body to find the counter sites", exprText(deferred[0].Call.Fun))
	}
	firstReturn := token.NoPos
	ast.Inspect(body, func(node ast.Node) bool {
		if _, isReturn := node.(*ast.ReturnStmt); isReturn && firstReturn == token.NoPos {
			firstReturn = node.Pos()
		}
		return true
	})
	if firstReturn == token.NoPos {
		t.Fatalf("the resolution has no return statement at all, which this gate cannot be right about")
	}
	if firstReturn < deferred[0].Pos() {
		t.Fatalf("the resolution returns before it registers its deferred counter block. A defer " +
			"below an early return never runs for that return, which is the every-arm property lost " +
			"in a shape that still reads as a defer")
	}

	// ── THE SITES ───────────────────────────────────────────────────────────────────────────
	sites := map[string]int{}
	inDefer := map[string]int{}
	ast.Inspect(body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign {
			return true
		}
		for _, target := range assign.Lhs {
			text := exprText(target)
			if !strings.HasPrefix(text, "self.stats.") {
				continue
			}
			counter := strings.TrimPrefix(text, "self.stats.")
			sites[counter] += 1
			if literal.Body.Pos() <= assign.Pos() && assign.End() <= literal.Body.End() {
				inDefer[counter] += 1
			}
		}
		return true
	})
	if len(sites) == 0 {
		t.Fatalf("this gate found no counter assignment in the resolution at all, so it is not " +
			"reading the function it names")
	}
	t.Logf("the resolution moves %d counter(s): %v, of which inside the every-exit block: %v",
		len(sites), sites, inDefer)

	// ── BOTH WAYS ───────────────────────────────────────────────────────────────────────────
	for counter, count := range sites {
		entry, dispositioned := dispositions[counter]
		if !dispositioned {
			t.Fatalf("the resolution moves Stats.%s at %d site(s) and this gate has no disposition "+
				"for it. Say whether it counts RECORDS -- in which case it belongs in the deferred "+
				"block that every return passes -- or EPOCHS, in which case it belongs on the arm "+
				"that decides the epoch. A counter added to an arm because that arm is the one that "+
				"reports it is this project's defect three times over", counter, count)
		}
		if entry.everyExit && inDefer[counter] != count {
			t.Fatalf("Stats.%s counts %s and %d of its %d site(s) are OUTSIDE the deferred block, so "+
				"it reads zero on every arm that returns without passing them", counter, entry.why,
				count-inDefer[counter], count)
		}
		if !entry.everyExit && inDefer[counter] != 0 {
			t.Fatalf("Stats.%s counts %s and %d of its %d site(s) are INSIDE the deferred block, "+
				"which runs whatever this function answers -- so an epoch that WAS resolved is "+
				"counted as one that could not be", counter, entry.why, inDefer[counter], count)
		}
	}
	for counter, entry := range dispositions {
		if sites[counter] == 0 {
			t.Fatalf("this gate disposes of Stats.%s (%s) and the resolution never moves it. A row "+
				"for a counter nobody adds is a row that has stopped measuring", counter, entry.why)
		}
	}
}

// ── 5. THE CENSUS: EVERY DIAGNOSIS THE RESOLUTION CAN RETURN IS PERSISTED AS A DARK KIND ─────

// EVERY ERROR THE RESOLUTION CAN RETURN IS DISPOSITIONED, AND EACH ONE SAYS WHETHER IT MAKES THE
// GROUP DARK OR HALTS IT.
//
// [Group.ingestCommitLocked] used to make a group dark on ANY error the resolution returned, and
// this census used to assert exactly that. RULING 41 SPLIT IT IN TWO and the split is the point:
//
//   - A VALID COMMIT whose wrap did not arrive or did not open -> DARK at n+1, diagnosable, with
//     the sentinel that says which of the three states it is. `darkens` is true, the kind reaches
//     [GroupRecord.WrapDarkKind], and a restart comes back dark by name.
//   - AN INVALID COMMIT -- an unrotated removal -- -> REFUSED. The group stays at the epoch it is
//     at and does NOT go dark, because advancing into a permanent brick on a commit just judged
//     invalid is how any client on an older build bricks every up-to-date member by removing
//     somebody. `darkens` is false -- and the halt is just as sticky, just as persisted and just as permanent.
//
// A kind octet with a silent default would persist a genuinely dark group as HEALTHY -- the exact
// failure the durable diagnosis exists to close, arriving through its own default arm -- so the
// kind is still asserted for EVERY entry, darkening or not, and since 2026-09-24 the halting one's
// kind is WRITTEN: [Group.haltLocked] persists it, so a kind that mapped to 'not dark' would be a
// halted group coming back reading as healthy -- which is what it did, and is why the halt is in
// this census with a kind of its own rather than with none.
//
// IT FAILS BOTH WAYS: a return with no entry aborts, an entry naming no site aborts. And the
// `darkens` column is not taken on trust -- [Group.ingestCommitLocked]'s exemption is read off the
// syntax tree and asserted to name exactly the non-darkening sentinels, with the behaviour itself
// driven by TestTheResidualUnrotatedRemovalIsRefusedAfterTheApplyAndStillDoesNotGoDark (halts,
// does not go dark) and TestADarkGroupComesBackDarkAndAHealthyOneDoesNot (goes dark, persists).
func TestEveryDiagnosisTheResolutionCanReturnIsPersistedAsADarkKind(t *testing.T) {
	// THE DISPOSITIONS. `build` is an error of that shape, built here, so the mapping is
	// asserted against the PRODUCTION function rather than re-derived.
	dispositions := map[string]struct {
		kind     uint8
		darkens  bool
		sentinel string
		why      string
		build    func() error
	}{
		"ErrNoWrapForEpoch": {kind: wrapDarkNoWrap, darkens: true,
			why:   "item 132's omission at the victim; the whole reason the wrap sentinels are three",
			build: func() error { return fmt.Errorf("%w: epoch 2", ErrNoWrapForEpoch) }},
		"ErrWrapUnreadable": {kind: wrapDarkUnreadable, darkens: true,
			why:   "a wrap at this device's own handle that did not open",
			build: func() error { return fmt.Errorf("%w: epoch 2", ErrWrapUnreadable) }},
		"ErrOrphanWrap": {kind: wrapDarkOrphan, darkens: true,
			why:   "the loser of a CAS race, and as permanent as the other two",
			build: func() error { return fmt.Errorf("%w: epoch 2", ErrOrphanWrap) }},
		"ErrCommitIngest": {kind: wrapDarkUnfollowable, darkens: true,
			why: "the digest-epoch mismatch arm: a record no server served. The epoch has already " +
				"been applied when it is reached, so the group IS dark and must not persist as healthy",
			build: func() error { return fmt.Errorf("%w: a digest for another epoch", ErrCommitIngest) }},
		"refuseRemovalOnHeldSecret": {kind: wrapDarkRemoval, darkens: false,
			sentinel: "ErrRemovalWithoutRotation",
			why: "RULING 41: an unrotated removal is an INVALID commit, so the group is HALTED " +
				"and not dark. It keeps a kind anyway, because a kind that maps to 'not dark' is " +
				"how a dark group comes back reading as healthy and this sentinel must never be " +
				"the one that does it",
			build: func() error { return refuseRemovalOnHeldSecret(2, []uint32{3}, 1, "did not rotate") }},
		"a foreign error": {kind: wrapDarkUnfollowable, darkens: true,
			why: "the `return nil, err` passthroughs: matchesEpochDigestLocked's own refusal and " +
				"whatever epochDigestGroupId or message.EpochKeysDigest answer. They are not this " +
				"package's sentinels and there is no kind for them by name",
			build: func() error { return errors.New("something connect said") }},
	}
	body := parseFunc(t, "pqepoch.go", "resolvePqSecretLocked")
	sites := map[string][]string{}
	ast.Inspect(body, func(node ast.Node) bool {
		ret, isReturn := node.(*ast.ReturnStmt)
		if !isReturn || len(ret.Results) != 2 {
			return true
		}
		switch failure := ret.Results[1].(type) {
		case *ast.Ident:
			if failure.Name == "nil" {
				return true
			}
			sites["a foreign error"] = append(sites["a foreign error"], exprText(ret.Results[1]))
		case *ast.CallExpr:
			sites[returnedErrorName(failure)] = append(sites[returnedErrorName(failure)], exprText(ret.Results[1]))
		default:
			sites[exprText(ret.Results[1])] = append(sites[exprText(ret.Results[1])], exprText(ret.Results[1]))
		}
		return true
	})
	names := []string{}
	for name := range sites {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Logf("the resolution can return %d distinct failure(s): %v", len(names), names)
	for _, name := range names {
		entry, dispositioned := dispositions[name]
		if !dispositioned {
			t.Fatalf("the resolution can return %q (%d site(s): %v) and this census has no entry "+
				"for it. Give it a wrap_dark kind and a reason, or it is persisted as whatever "+
				"wrapDarkKindOf's default says and a dark group comes back reading as healthy",
				name, len(sites[name]), sites[name])
		}
		sample := entry.build()
		if got := wrapDarkKindOf(sample); got != entry.kind {
			t.Fatalf("%q is dispositioned as kind %d (%s) and wrapDarkKindOf answers %d",
				name, entry.kind, entry.why, got)
		}
		if entry.kind == wrapDarkNone {
			t.Fatalf("%q is dispositioned as kind zero, which is what a record that has NEVER been "+
				"dark carries. A refusal mapping to it is a dark group persisting as healthy", name)
		}
		if wrapDarkErrorOf(entry.kind, 7) == nil {
			t.Fatalf("kind %d (%q) rebuilds no diagnosis at a restart", entry.kind, name)
		}
		if !entry.darkens && entry.sentinel == "" {
			t.Fatalf("%q is dispositioned as NOT darkening (%s) and names no sentinel; the exemption "+
				"in ingestCommitLocked is by sentinel, so a row without one cannot be checked "+
				"against it", name, entry.why)
		}
		// AND THE KIND COMES BACK IN THE FIELD IT WAS WRITTEN FROM. One persisted column, two
		// fields, and [restoredDiagnosisOf] is the one place the octet decides which. A halting
		// kind restored into [Group.wrapDark] would be a halted group coming back as a dark one --
		// this build persisting one diagnosis and reading back another -- and a darkening kind
		// restored into [Group.halted] would be the same defect the other way round.
		restoredDark, restoredHalt := restoredDiagnosisOf(entry.kind, 7)
		if entry.darkens && (restoredDark == nil || restoredHalt != nil) {
			t.Fatalf("%q darkens (%s) and kind %d restores dark=%v halted=%v",
				name, entry.why, entry.kind, restoredDark, restoredHalt)
		}
		if !entry.darkens && (restoredHalt == nil || restoredDark != nil) {
			t.Fatalf("%q HALTS (%s) and kind %d restores dark=%v halted=%v; ruling 41's two "+
				"outcomes are two fields and one column, and the column has to land in the right one",
				name, entry.why, entry.kind, restoredDark, restoredHalt)
		}
	}
	// ── THE `darkens` COLUMN, READ OFF THE PRODUCTION FUNCTION AND NOT TAKEN ON TRUST ───────
	//
	// [Group.ingestCommitLocked] exempts the halting refusals from the dark state with an
	// errors.Is over `resolveErr`. The set of sentinels it names must be exactly the set this
	// census dispositions as non-darkening: a sentinel exempted here and darkening there would persist
	// a halted group as dark, and one darkening here and exempted there would advance into a brick
	// on a commit this build judged invalid, which is the outcome ruling 41 removed.
	exempted := map[string]bool{}
	ast.Inspect(parseFunc(t, "group.go", "ingestCommitLocked"), func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || exprText(call.Fun) != "errors.Is" || len(call.Args) != 2 {
			return true
		}
		if exprText(call.Args[0]) != "resolveErr" {
			return true
		}
		exempted[exprText(call.Args[1])] = true
		return true
	})
	halting := map[string]bool{}
	for name, entry := range dispositions {
		if !entry.darkens {
			halting[entry.sentinel] = true
			t.Logf("HALTING, not dark: %q -> %s (%s)", name, entry.sentinel, entry.why)
		}
	}
	if len(exempted) == 0 {
		t.Fatalf("ingestCommitLocked exempts NO sentinel from the dark state, so every refusal the " +
			"resolution returns advances the group into a permanent brick -- including the unrotated " +
			"removal, which ruling 41 says is an invalid commit and must be refused instead")
	}
	for sentinel := range exempted {
		if !halting[sentinel] {
			t.Fatalf("ingestCommitLocked exempts %s from the dark state and this census dispositions "+
				"it as darkening. One of the two is wrong, and the failure mode of getting it wrong "+
				"this way round is a genuinely dark group that persists as healthy", sentinel)
		}
	}
	for sentinel := range halting {
		if !exempted[sentinel] {
			t.Fatalf("this census dispositions %s as HALTING and ingestCommitLocked does not exempt "+
				"it, so a group takes it and goes dark at n+1 -- which is ruling 41's own defect: "+
				"advancing into a permanent brick on a commit just judged invalid", sentinel)
		}
	}
	// THE OTHER DIRECTION: an entry that names no site is a disposition for a refusal that has
	// been removed, and it is exactly as much of a defect as a site with no entry -- it is the
	// state in which this census keeps passing while measuring less than it says.
	for name := range dispositions {
		if len(sites[name]) == 0 {
			t.Fatalf("this census disposes of %q and the resolution cannot return it; a census "+
				"that keeps rows for refusals nobody makes is a census that has stopped measuring", name)
		}
	}
	// AND THE FLOOR: nil is the ONLY thing that maps to "not dark".
	if wrapDarkKindOf(nil) != wrapDarkNone {
		t.Fatalf("a nil diagnosis maps to kind %d", wrapDarkKindOf(nil))
	}
}

// ── 6. THE PROSE GATE ────────────────────────────────────────────────────────────────────────

// NO PRODUCTION DOC COMMENT IN EITHER PACKAGE MAY CLAIM THAT A DARK OR ORPHANED GROUP REPAIRS
// ITSELF.
//
// THREE FILES CARRIED THE CLAIM AND THEY WERE IN TWO PACKAGES: urmessage/pqepoch.go,
// urmessage/errors.go and cgo/exports_message.go. A gate that read one file by name -- which is a
// shape this project has already been bitten by twice -- would have covered at most one of them.
// So the subject here is EVERY production .go file under the module, and the file list is printed
// with its own controls: the count, and three files that are known to talk about the dark state
// and are known not to claim it heals.
//
// IT MATCHES ON THE CLAIM AND NOT ON THE WORD, AND THE SUBJECT IS A COMMENT BLOCK AND NOT A LINE.
// Two things the first draft of this gate got wrong, both caught by running it:
//
//   - A bare substring search hit `on the prefix itself` in message_stream_store.go. The claims
//     are matched on WORD BOUNDARIES now, and that line is the standing negative control for it.
//   - A sentence that REFUTES the claim has to state the claim, and the refutation is usually a
//     line or two away from it -- "resolves itself at the next commit". The first half is true and
//     the second is FALSE -- so a line-scoped gate refuses exactly the paragraphs that exist to
//     refuse the defect. The subject is the contiguous run of `//` lines, and a block that says
//     the claim must also say IN THE SAME BLOCK that it is false.
//
// BOTH DIRECTIONS ARE CONTROLLED, inline, on synthetic blocks: a block carrying only the claim
// must fire, and the same block with a refutation must not. Without the first the gate could be
// matching nothing; without the second it would be refusing its own documentation.
func TestNoProductionCommentClaimsADarkGroupRepairsItself(t *testing.T) {
	claims := []string{
		"repairs itself", "repair itself", "resolves itself", "resolve itself",
		"heals itself", "heal itself", "fixes itself", "fix itself",
		"self-healing", "self healing", "recovers by itself", "sorts itself out",
	}
	// A BLOCK MAY STATE THE CLAIM IN ORDER TO DENY IT, and these are the denials. They are exact
	// phrases rather than a search for "not", because the difference between the claim and its
	// refusal is three characters and a loose rule here would readmit the defect.
	denials := []string{
		"is false", "was false", "were false", "are false", "false rather than",
		"does not repair", "does not resolve", "does not heal", "cannot repair",
		"used to say", "this comment used to", "the deleted sentence", "used to be one sentence",
	}
	claimed := func(block string) string {
		lowered := strings.ToLower(block)
		for _, claim := range claims {
			at := strings.Index(lowered, claim)
			for 0 <= at {
				before := byte(' ')
				if 0 < at {
					before = lowered[at-1]
				}
				if !isWordOctet(before) {
					return claim
				}
				next := strings.Index(lowered[at+1:], claim)
				if next < 0 {
					break
				}
				at = at + 1 + next
			}
		}
		return ""
	}
	denied := func(block string) bool {
		lowered := strings.ToLower(block)
		for _, denial := range denials {
			if strings.Contains(lowered, denial) {
				return true
			}
		}
		return false
	}

	// ── THE CONTROLS, FIRST, ON SYNTHETIC BLOCKS ────────────────────────────────────────────
	plant := "// the orphan is nobody's fault and resolves itself at the next commit"
	if claimed(plant) == "" {
		t.Fatalf("CONTROL FAILED: the matcher does not fire on the sentence this gate refuses: %q", plant)
	}
	if denied(plant) {
		t.Fatalf("CONTROL FAILED: the bare claim reads as a refutation")
	}
	refuted := plant + "\n// -- and that is FALSE, because its fetches are refused."
	if claimed(refuted) == "" || !denied(refuted) {
		t.Fatalf("CONTROL FAILED: a block that states the claim in order to refute it is not " +
			"recognised as refuting it, so this gate would refuse its own documentation")
	}
	if claimed("// shapes and on the prefix itself.") != "" {
		t.Fatalf("CONTROL FAILED: the matcher fires on \"prefix itself\", which is the word-boundary " +
			"defect this gate was repaired for")
	}

	root := moduleRoot(t)
	scanned, hits := 0, []string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned += 1
		rel, _ := filepath.Rel(root, path)
		lines := strings.Split(string(source), "\n")
		for at := 0; at < len(lines); at += 1 {
			if !strings.HasPrefix(strings.TrimSpace(lines[at]), "//") {
				continue
			}
			from := at
			block := []string{}
			for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "//") {
				block = append(block, strings.TrimSpace(lines[at]))
				at += 1
			}
			joined := strings.Join(block, " ")
			claim := claimed(joined)
			if claim == "" || denied(joined) {
				continue
			}
			hits = append(hits, fmt.Sprintf("%s:%d: the block says %q and nothing in it says otherwise",
				filepath.ToSlash(rel), from+1, claim))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// THE WALK'S OWN CONTROLS. A walk that found nothing because it scanned nothing is the
	// failure mode of every absence, so the file count is held against a floor and the three
	// files that DID carry the claim are asserted to be inside this subject.
	if scanned < 20 {
		t.Fatalf("CONTROL FAILED: this gate scanned %d production file(s) under %s, which is not "+
			"this module; an empty result would mean the walk is wrong and not that the prose is clean",
			scanned, root)
	}
	for _, mustScan := range []string{"urmessage/pqepoch.go", "urmessage/errors.go", "cgo/exports_message.go"} {
		if _, statErr := os.Stat(filepath.Join(root, mustScan)); statErr != nil {
			t.Fatalf("CONTROL FAILED: %s is not under %s, so the three files that carried the "+
				"claim are not in this gate's subject", mustScan, root)
		}
	}
	t.Logf("CONTROLS HELD: %d production files scanned; the matcher fires on the deleted sentence, "+
		"does not fire on \"prefix itself\", and a refutation of the claim is not a claim", scanned)
	if 0 < len(hits) {
		t.Fatalf("a production comment claims a dark or orphaned group repairs itself. It does "+
			"not: the group has followed a commit into an epoch it holds no pq_secret for, its "+
			"fetches are refused before a row is read, and there is nowhere to put a later wrap.\n%s",
			strings.Join(hits, "\n"))
	}
}

// ── 7. THE PROMISE: THE RULE IS A RECEIVER PROPERTY AND IS NEVER WRITTEN AS A GROUP ONE ──────

// NO PRODUCTION SENTENCE ATTRIBUTES THE REMOVAL RULE'S HOLDING TO THE GROUP, AND THE THREE THINGS
// IT DOES NOT DELIVER ARE WRITTEN WHERE THE RULE IS.
//
// ── WHY THIS GATE EXISTS, AND IT IS RULING 44 RATHER THAN TIDINESS ────────────────────────────
//
// The rule was documented, in production comments and in the sentinel a caller reads, as
//
//	no removal may be followed on a secret this group already holds
//
// and it cannot deliver that. The subject it is CHECKED against is one receiver's own history
// ([Group.pqSecretWitness], written by [Group.filePqSecretLocked] and by nothing else), and
// [Device.Join] files exactly one row -- so a member admitted at epoch k answers *not held* for
// octets a founder answers *held* for, and if every survivor joined after epoch k and the
// committer reuses pq_secret[k] then NOBODY refuses. Ledger rulings 42-45 keep the rule and change
// the claim to what is checkable: *this receiver does not follow a removal onto a secret THIS
// RECEIVER has held.* The corpus had never written the honesty caveat for rotation although it had
// written the analogous one for roles, which is the shape of defect this gate is for: a promise
// that is FALSE rather than merely unmeasured, sitting in a block whose heading claims honesty.
//
// ── WHAT IT ASKS, IN THREE ARMS ───────────────────────────────────────────────────────────────
//
//  1. THE SUBJECT OF THE HOLDING, BY CLASS AND NOT BY BANNED PHRASE. Every production comment
//     block that is ABOUT this rule -- it mentions a removal and a pq_secret and says something is
//     held -- is read, and for each holding verb in it the NOUN DOING THE HOLDING is classified
//     off the text immediately before it. A group subject is a refusal; a device or receiver
//     subject is the claim this build can make. So a re-wording that says "the group still has it"
//     is red for the same reason the old sentence was, without this gate carrying a list of the
//     ways to spell it. THE GROUP IS ASKED FIRST, so a window naming a group noun AND a device
//     noun -- "the group THIS DEVICE is in already holds" -- is the GROUP's; the classifier fails
//     closed on an ambiguous window rather than reading it as the claim it is allowed to make.
//  2. AND A BLOCK MAY STATE THE OLD CLAIM IN ORDER TO CORRECT IT -- [refuseRemovalOnHeldSecret]'s
//     own header does exactly that -- but only if the correction is IN THE SENTENCE THE HOLDING
//     IS IN, and only in a block that states the delivered claim somewhere. A denial anywhere in
//     the same block is the claim with an alibi: measured, that form waived all seven group
//     holdings that exist in production, so the arm asserted nothing about any block carrying this
//     rule's prose, and the sentence this gate exists to refuse could be written into
//     [Group.resolvePqSecretLocked]'s own header with the gate still green. What the waiver
//     removes is now counted, PRINTED, and held against a floor of its own.
//
// THE ASYMMETRY BETWEEN THE TWO SUBJECTS, WRITTEN DOWN RATHER THAN LEFT TO BE FOUND. An
// UNATTRIBUTED holding -- one whose window names no holder at all, "a pq_secret it already holds"
// -- is a refusal in a string literal and is not one in a comment. The reason is that a sentinel
// is read ALONE, with nothing around it to say what "it" is, while a comment block is read whole.
// That excuse is not left unmeasured: a block whose holdings are ALL unattributed has no
// antecedent anywhere in it and IS a refusal, and every unattributed holding is printed with its
// sentence. THE RESIDUAL IT LEAVES, NAMED: a block that attributes one holding and leaves another
// unattributed is silent on the second, so a group claim spelled with a noun outside the
// classifier's vocabulary -- "already held anywhere in the cohort" -- is silent as a comment and
// red as a literal. That is the whole of what this arm does not hold, and the literal arm is the
// one that covers the surface an operator actually reads.
//  3. THE THREE THINGS THE RULE DOES NOT DELIVER ARE PRESENT WHERE THE RULE IS, keyed by a
//     fragment of each and held against a written disposition: a clause deleted is a refusal. This
//     is presence and NOT truth, which is this gate's residual and is stated rather than implied --
//     what holds the truth of clause (a) is
//     TestThreeMembersRotateAcrossTwoEpochsAndAMemberRemovedByThatCommitCannotFollow and what
//     holds clause (b) is section 3 above. Clause (c) -- *against a hostile ADMIN or OWNER this
//     rule delivers nothing, structurally* -- is held by NO test and can be held by none: it is the
//     statement that there is no defence here, and a test of it could only assert a tautology.
//     This gate is the only thing that holds it, and holding it as prose is the whole of ruling
//     42's "stated plainly rather than implied".
//
// AND THE STRING LITERALS ARE IN THE SUBJECT AND NOT ONLY THE COMMENTS, because the sentinel is
// what a caller and an operator actually meet: [ErrRemovalWithoutRotation]'s message and the two
// refusals' formats said "this group" too.
func TestTheRemovalRuleIsDocumentedAsAReceiverPropertyAndNeverAsAGroupOne(t *testing.T) {
	// THE HOLDING VERBS. A sentence about this rule says the value is held; these are the ways to
	// say it, and the gate does not care which -- what it reads is the noun in front of one.
	holdings := []string{"already holds", "already held", "has already held", "has ever held",
		"has held", "have held", "already has", "ever held", "still holds"}
	// THE SUBJECT WINDOW: the text immediately before a holding verb, in which the noun doing the
	// holding sits. Twenty-four octets covers "a pq_secret this group " and "value THIS DEVICE "
	// with room to spare, and it is short enough that an unrelated noun two sentences back cannot
	// reach into it.
	const window = 24
	// attributed is one holding verb, where it is in the LOWERED text, and who that text says is
	// doing the holding. The position is carried because the waiver below is scoped to the
	// SENTENCE the verb sits in, and a sentence cannot be found from a count.
	type attributed struct {
		at    int
		verb  string
		class string
	}
	// holdingsIn answers every holding verb in a text, classified.
	//
	// ── THE GROUP IS ASKED FIRST, WHICH IS THE 2026-09-24 (FOURTH PASS) REPAIR ─────────────────
	//
	// What stood here asked `device || receiver` BEFORE `group`, so a window naming BOTH read as
	// the receiver's -- and a plain-English GROUP-property claim that mentions a device anywhere in
	// its twenty-four octets passed. MEASURED, one plant at a time into
	// [Group.refuseUnrotatedRemovalLocked]'s header, which no waiver reaches:
	//
	//	"...on a secret this group already holds"                        -> RED (the control)
	//	"...on a secret the group THIS DEVICE is in already holds"       -> PASS
	//	"...on a secret every device of the group already holds"         -> PASS
	//
	// Both of the last two are group-property claims and the second is the exact shape this gate's
	// header promises to catch. Asking the group first makes an AMBIGUOUS window -- one naming a
	// group noun and a device noun at once -- the GROUP's, which fails closed: the only way to
	// write a receiver holding is to keep the group out of the window, which is what the corrected
	// sentence does. It costs nothing on the prose that exists: no production window in this
	// gate's subject names both, so all eleven blocks and all four literals classify identically
	// under either order, and the change is visible only on a claim that has both.
	//
	// AND `nearest` WAS TRIED AND IS WRONG. Taking the noun closest to the verb -- scanning the
	// window right to left -- leaves "the group THIS DEVICE is in already holds" reading as the
	// receiver's, because "device" is nearer the verb than "group" is. It is the head noun and not
	// the nearest noun that holds, and the head noun cannot be found by distance.
	holdingsIn := func(text string) []attributed {
		lowered := strings.ToLower(text)
		found := []attributed{}
		for _, holding := range holdings {
			at := strings.Index(lowered, holding)
			for 0 <= at {
				from := at - window
				if from < 0 {
					from = 0
				}
				before := lowered[from:at]
				class := "loose"
				switch {
				case strings.Contains(before, "group"):
					class = "group"
				case strings.Contains(before, "device") || strings.Contains(before, "receiver"):
					class = "receiver"
				}
				found = append(found, attributed{at: at, verb: holding, class: class})
				next := strings.Index(lowered[at+len(holding):], holding)
				if next < 0 {
					break
				}
				at = at + len(holding) + next
			}
		}
		return found
	}
	// subjectsOf counts what holdingsIn found, by class.
	subjectsOf := func(text string) (group int, receiver int, loose int) {
		for _, one := range holdingsIn(text) {
			switch one.class {
			case "group":
				group += 1
			case "receiver":
				receiver += 1
			default:
				loose += 1
			}
		}
		return group, receiver, loose
	}
	// sentenceAround answers the sentence of `text` that the octet at `at` sits in.
	//
	// A SENTENCE ENDS AT A FULL STOP AND NOT AT A COLON OR A DASH, and that is measured rather
	// than chosen: this corpus writes the correction as *"this used to be documented as a GROUP
	// property: <the old sentence>"*, so splitting at the colon would cut the denial away from the
	// quotation it introduces and turn five honest blocks red.
	sentenceAround := func(text string, at int) string {
		from := 0
		for cut := 0; cut < len(text); cut += 1 {
			if text[cut] != ' ' || cut == 0 {
				continue
			}
			end := cut - 1
			if end < from {
				continue
			}
			if text[end] != '.' && text[end] != '?' && text[end] != '!' {
				continue
			}
			if at <= end {
				return text[from:cut]
			}
			from = cut + 1
		}
		return text[from:]
	}
	// A BLOCK IS ABOUT THIS RULE when it says all three things. Anything narrower is a gate scoped
	// to one file, and anything wider drags in every comment that mentions a group.
	//
	// "SECRET" AND NOT "pq_secret", WHICH IS A WIDENING TAKEN AFTER THE NARROW FORM MISSED ONE.
	// [Group.resolvePqSecretLocked]'s wrap-candidate arm says "the value the commit's own digest
	// NAMES is one this group has held" and never spells `pq_secret` in that block -- so under the
	// narrower test the arm that had NO GUARD AT ALL was also the arm whose prose was outside this
	// gate's subject, which is the same defect twice in one place.
	about := func(text string) bool {
		lowered := strings.ToLower(text)
		if !strings.Contains(lowered, "remov") {
			return false
		}
		if !strings.Contains(lowered, "secret") {
			return false
		}
		group, receiver, loose := subjectsOf(text)
		return 0 < group+receiver+loose
	}
	// A STRING LITERAL IS IN THE SUBJECT ON A WEAKER TEST, AND THE REASON IS THE `how` CLAUSES.
	// [Group.resolvePqSecretLocked]'s refusal is assembled from two literals -- the format, which
	// names the removal, and the ARM'S OWN CLAUSE, which does not. Under the comment test the arm
	// clauses were outside the subject by construction while being half of the sentence an operator
	// reads, which is the "a gate scoped to how the defect is spelled today" failure one level
	// down. A literal is in the subject when it names a pq_secret and says something is held.
	aboutLiteral := func(text string) bool {
		lowered := strings.ToLower(text)
		if !strings.Contains(lowered, "pq_secret") && !strings.Contains(lowered, "pqsecret") {
			return false
		}
		group, receiver, loose := subjectsOf(text)
		return 0 < group+receiver+loose
	}
	// A DENIAL is a block stating the old claim in order to correct it.
	denials := []string{"used to claim", "used to say", "used to be", "cannot deliver",
		"is false", "was false", "no longer", "rather than the group", "and not the group"}
	denies := func(text string) bool {
		lowered := strings.ToLower(text)
		for _, denial := range denials {
			if strings.Contains(lowered, denial) {
				return true
			}
		}
		return false
	}

	// ── THE CONTROLS, FIRST, ON SYNTHETIC TEXT. The literals are COPIED from the ledger entry
	// and from the source, not retyped from memory, which is the trap this corpus walked into
	// with an en dash.
	deleted := "The rule is \"no removal may be followed on a secret this group already holds\""
	if group, _, _ := subjectsOf(deleted); group != 1 {
		t.Fatalf("CONTROL FAILED: the classifier reads %d group-subject holding(s) in the sentence "+
			"this gate exists to refuse, want 1: %q", group, deleted)
	}
	if !about(deleted + " pq_secret") {
		t.Fatalf("CONTROL FAILED: the deleted sentence is not recognised as being about this rule")
	}
	corrected := "no removal may be followed on a secret THIS RECEIVER has held"
	if group, receiver, _ := subjectsOf(corrected); group != 0 || receiver != 1 {
		t.Fatalf("CONTROL FAILED: the corrected sentence reads as %d group / %d receiver "+
			"subject(s), want 0 / 1: %q", group, receiver, corrected)
	}
	// ── AND THE PRECEDENCE, ASSERTED AND NOT ASSUMED ────────────────────────────────────────
	//
	// Both of these are GROUP-property claims whose window also names a device, and both PASSED
	// this gate before the classifier asked the group first. They are here as rows rather than as
	// a sentence in the header, because a precedence nobody asserts is a precedence a later edit
	// reverses without noticing. Each is the `deleted` sentence above with its subject re-worded,
	// so the three read the same claim three ways.
	for _, ambiguous := range []string{
		"The rule is \"no removal may be followed on a secret the group THIS DEVICE is in already holds\"",
		"The rule is \"no removal may be followed on a secret every device of the group already holds\"",
	} {
		if group, _, _ := subjectsOf(ambiguous); group != 1 {
			t.Fatalf("CONTROL FAILED: the classifier reads %d group-subject holding(s) in %q, want "+
				"1. A window that names a group noun AND a device noun is the GROUP's; reading it "+
				"as the receiver's is how a plain-English group claim passes this gate", group, ambiguous)
		}
	}
	if about("// the pq_secret table is pruned at PastEpochWindow") {
		t.Fatalf("CONTROL FAILED: a block that says nothing about a removal is in this gate's subject")
	}
	// AND THE UNATTRIBUTED CLASS EXISTS AND IS NOT THE GROUP'S. This is the row that makes the
	// asymmetry below a measured thing rather than an implied one: the same sentence is SILENT as
	// a comment inside a block that names a holder elsewhere, and RED as a string literal.
	unnamed := "no removal may be followed on a secret already held anywhere in the cohort"
	if group, receiver, loose := subjectsOf(unnamed); group != 0 || receiver != 0 || loose != 1 {
		t.Fatalf("CONTROL FAILED: a holding with no holder in its window reads as %d group / %d "+
			"receiver / %d unattributed, want 0 / 0 / 1: %q", group, receiver, loose, unnamed)
	}
	if group, _, loose := subjectsOf("a pq_secret " + unnamed); group != 0 || loose != 1 {
		t.Fatalf("CONTROL FAILED: the unattributed sentence is not unattributed in the literal " +
			"subject, so the asymmetry this gate documents cannot be measured")
	}
	// AND THE ARM CLAUSE, WHICH IS HALF A REFUSAL AND NAMES NO REMOVAL, IS IN THE LITERAL SUBJECT.
	// Copied from pqepoch.go rather than retyped: the clause is the sentence an operator reads.
	arm := "opened its epoch with a pq_secret this device has held"
	if !aboutLiteral(arm) {
		t.Fatalf("CONTROL FAILED: an arm's own `how` clause is outside the literal subject, which "+
			"is how half of every refusal this rule prints stayed invisible: %q", arm)
	}
	if about(arm) {
		t.Fatalf("CONTROL FAILED: the arm clause names no removal, so it must not be in the " +
			"COMMENT subject -- the two tests are different on purpose")
	}
	if denies(deleted) {
		t.Fatalf("CONTROL FAILED: the bare claim reads as a correction of itself")
	}

	// ── THE WALK: production comment blocks and production string literals ──────────────────
	root := moduleRoot(t)
	scanned, blocks, literals := 0, 0, 0
	// THE ARM'S OWN BOOKKEEPING, so that what it asserted and what it WAIVED are both numbers this
	// test prints and then holds against a floor. The arm this replaces carved seven of its eleven
	// blocks out and said nothing about it, and its only control counted the subject BEFORE the
	// carve-out -- so it could go fully vacuous while logging "CONTROLS HELD: 11 comment block(s)".
	held, waived := 0, 0
	waivedRows, unattributedRows := []string{}, []string{}
	hits := []string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned += 1
		rel, _ := filepath.Rel(root, path)
		name := filepath.ToSlash(rel)

		lines := strings.Split(string(source), "\n")
		for at := 0; at < len(lines); at += 1 {
			if !strings.HasPrefix(strings.TrimSpace(lines[at]), "//") {
				continue
			}
			from := at
			block := []string{}
			for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "//") {
				block = append(block, strings.TrimSpace(lines[at]))
				at += 1
			}
			joined := strings.Join(block, " ")
			if !about(joined) {
				continue
			}
			blocks += 1
			lowered := strings.ToLower(joined)
			group, receiver, loose := subjectsOf(joined)

			// A BLOCK THAT NEVER SAYS WHO HOLDS IT. An unattributed holding is tolerated in a
			// comment and refused in a literal, and this is the premise under that asymmetry made
			// checkable: a comment block is read WHOLE, so a pronoun is disambiguated by the block
			// around it -- but only if the block attributes something somewhere. A block whose
			// holdings are ALL unattributed has no antecedent anywhere in it and is the shape the
			// asymmetry's excuse does not cover.
			if group == 0 && receiver == 0 && 0 < loose {
				hits = append(hits, fmt.Sprintf("%s:%d: all %d holding(s) in this block leave the "+
					"HOLDER unnamed and nothing in the block names one, so there is no antecedent "+
					"for the pronoun and the sentence reads as a claim about the cohort", name,
					from+1, loose))
				continue
			}
			// EVERY UNATTRIBUTED HOLDING, PRINTED. This is the complement of what the arm asserts
			// and it is in the log rather than implied, because this arm is silent on it.
			for _, one := range holdingsIn(joined) {
				if one.class != "loose" {
					continue
				}
				unattributedRows = append(unattributedRows, fmt.Sprintf("  %s:%d: %q",
					name, from+1, sentenceAround(lowered, one.at)))
			}
			if group == 0 {
				held += receiver + loose
				continue
			}
			// ── THE WAIVER, SCOPED TO THE SENTENCE AND NOT TO THE BLOCK ─────────────────────
			//
			// A block MAY state the old claim in order to correct it -- [refuseRemovalOnHeldSecret]'s
			// own header does exactly that -- but the correction has to be in the SENTENCE the
			// group holding is in, not merely somewhere in the same block.
			//
			// WHY, MEASURED. The block-scoped form waived every group holding in any block that
			// carried a denial phrase and a receiver holding anywhere in it. Seven of this arm's
			// eleven blocks were in that carve-out and ALL SEVEN group-subject holdings that exist
			// in production were inside it, so the arm's live subject contained ZERO group
			// holdings and asserted nothing about any block carrying this rule's prose. The exact
			// sentence this gate exists to refuse was planted into [Group.resolvePqSecretLocked]'s
			// own header and the gate stayed GREEN; the same sentence in
			// [Group.refuseUnrotatedRemovalLocked]'s header -- a block that does not deny -- was
			// RED, naming the line. The waiver did not ask whether the holding it was waiving was
			// the one being corrected, and the sentence is where that question is answerable.
			for _, one := range holdingsIn(joined) {
				if one.class != "group" {
					held += 1
					continue
				}
				sentence := sentenceAround(lowered, one.at)
				if denies(sentence) && 0 < receiver {
					waived += 1
					waivedRows = append(waivedRows, fmt.Sprintf("  %s:%d: %q", name, from+1, sentence))
					continue
				}
				held += 1
				hits = append(hits, fmt.Sprintf("%s:%d: a holding in this block is the GROUP's and "+
					"the sentence it is in does not correct the claim (%d holding(s) in the block "+
					"are the receiver's): %q", name, from+1, receiver, sentence))
			}
		}

		// THE STRING LITERALS, off the syntax tree rather than off the text, so that a sentence
		// split across a `+` is one literal and not two halves neither of which says anything.
		fileSet := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fileSet, path, source, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			text, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil || !aboutLiteral(text) {
				return true
			}
			literals += 1
			group, receiver, loose := subjectsOf(text)
			if group == 0 && loose == 0 {
				return true
			}
			hits = append(hits, fmt.Sprintf("%s:%d: the string %q attributes %d holding(s) to the "+
				"GROUP and leaves %d unattributed (%d name the receiver). A sentinel is what an "+
				"operator reads, and this rule is one receiver's own history",
				name, fileSet.Position(literal.Pos()).Line, text, group, loose, receiver))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// ── THE WALK'S OWN CONTROLS. An absence proves nothing without them ─────────────────────
	//
	// THE COMPLEMENT, PRINTED BEFORE ANYTHING IS ASSERTED. What a narrowing REMOVED is the thing
	// that has to be visible: a waiver nobody can see reads as a clean pass.
	t.Logf("WAIVED (%d group-subject holding(s), each in a sentence that corrects the claim):\n%s",
		waived, strings.Join(waivedRows, "\n"))
	t.Logf("UNATTRIBUTED (%d holding(s) that name no holder; SILENT here and RED in a string "+
		"literal -- see this gate's header):\n%s",
		len(unattributedRows), strings.Join(unattributedRows, "\n"))
	if scanned < 20 {
		t.Fatalf("CONTROL FAILED: this gate scanned %d production file(s) under %s, which is not "+
			"this module", scanned, root)
	}
	if blocks < 5 || literals < 2 {
		t.Fatalf("CONTROL FAILED: the subject is %d comment block(s) and %d string literal(s). "+
			"This rule is documented in more places than that, so a clean result here would mean "+
			"the matcher stopped finding the prose rather than that the prose is right",
			blocks, literals)
	}
	// ── AND THE SAME CONTROL AFTER THE CARVE-OUT, WHICH IS THE ONE THAT WAS MISSING ─────────
	//
	// `blocks` counts the subject BEFORE the waiver removes anything from it, so it stayed at 11
	// while the arm's live subject went to zero. These two count what the arm actually JUDGED.
	if held < 20 {
		t.Fatalf("CONTROL FAILED: this arm judged %d holding(s) after waiving %d. The subject is "+
			"%d block(s) and they carry more prose than that between them, so a clean result here "+
			"would mean the waiver ate the subject rather than that the prose is right",
			held, waived, blocks)
	}
	if waived == 0 {
		t.Fatalf("CONTROL FAILED: not one group-subject holding was waived anywhere in %d block(s). "+
			"This corpus DOES quote the old claim in order to correct it -- at least at "+
			"[ErrRemovalWithoutRotation] and at [refuseRemovalOnHeldSecret] -- so a zero here is "+
			"the classifier having stopped finding group subjects at all, which is this arm going "+
			"blind in the direction it exists to look", blocks)
	}
	t.Logf("CONTROLS HELD: %d production files, %d comment block(s) and %d string literal(s) about "+
		"this rule; %d holding(s) judged and %d waived; the classifier reads the deleted sentence "+
		"as the GROUP's, the corrected one as the receiver's, and both re-wordings that name a "+
		"device inside a group claim as the GROUP's", scanned, blocks, literals, held, waived)
	if 0 < len(hits) {
		t.Fatalf("the removal rule is documented as a GROUP property and it cannot deliver one: "+
			"its subject is ONE RECEIVER's own history, [Device.Join] files one row, and if every "+
			"survivor joined after the reused epoch then nobody refuses (ledger rulings 42-45).\n%s",
			strings.Join(hits, "\n"))
	}

	// ── ARM 3: THE THREE THINGS IT DOES NOT DELIVER, WHERE THE RULE IS ──────────────────────
	//
	// Keyed by a fragment of each clause and held against a written disposition. A clause that is
	// deleted is a refusal; this reads PRESENCE and not truth, which is said out loud in this
	// gate's header beside what does hold each clause's truth.
	//
	// AND ONE OF THE KEYS IS A CITATION, WHICH IS THE 2026-09-24 (SEVENTH PASS) ADDITION. Presence
	// cannot catch a FALSE sentence and this arm does not claim to -- but a citation can be bound
	// to the thing it cites, and that is the shape the miss had. Residual (1)'s header said *that
	// measurement was a probe and no test in this package drives it, because rotWorld admits every
	// member in the founding commit and has no door that admits one later* at a commit where the
	// row AND the door both existed, and where the OTHER door's header -- written in that same
	// commit -- said the opposite in as many words. That is the corpus's own section 6 regression
	// class, section X changed and section Y was not updated, in the dangerous direction: the next
	// round reads *there is no such door* and builds the one already there. Both headers now cite
	// the row by name and the row and its world door are held to exist, so deleting either turns
	// this red and the citation cannot quietly become a claim about nothing.
	owed := map[string]string{
		"late-joiner": "residual 1 is DRIVEN and not merely named, and the row that drives it is " +
			"cited where a reader meets the rule. The sentence this replaced claimed the " +
			"opposite while the row existed",
		"THIS RECEIVER DOES NOT FOLLOW A REMOVAL ONTO A SECRET THIS RECEIVER HAS HELD": "" +
			"the claim itself, in the terms it is checked in",
		"STRICTLY SMALLER": "residual 1 -- a late joiner's history is smaller than a founder's, " +
			"so its false negative is a theorem and not a bug",
		"NOBODY REFUSES": "residual 2 -- every survivor joined after the reused epoch and the " +
			"committer is not a receiver of its own commit",
		"PARTITION BY JOIN EPOCH": "residual 2's group-level effect, which item 242 prices as " +
			"a hostile committer can HALT a group and cannot TAKE it",
		"HOSTILE ADMIN OR OWNER COMMITTER THIS RULE DELIVERS NOTHING": "ruling 42's clause (c), " +
			"which is held by no test and can be held by none",
		"IN ITS OWN PROCESS": "why clause (c) is structural: the committer must hold " +
			"storage_root[n+1] to build the digest at all",
	}
	source, err := os.ReadFile(filepath.Join(root, "urmessage", "pqepoch.go"))
	if err != nil {
		t.Fatalf("reading pqepoch.go: %v", err)
	}
	where := parseFuncDecl(t, "pqepoch.go", "refuseRemovalOnHeldSecret")
	if where.Doc == nil {
		t.Fatalf("refuseRemovalOnHeldSecret carries no doc comment, so the rule has nowhere to " +
			"be written down where a reader meets it")
	}
	rule := where.Doc.Text()
	if !strings.Contains(string(source), "refuseRemovalOnHeldSecret") {
		t.Fatalf("CONTROL FAILED: pqepoch.go does not name the refusal, so this arm is reading " +
			"the wrong file")
	}
	for fragment, why := range owed {
		if !strings.Contains(rule, fragment) {
			t.Fatalf("refuseRemovalOnHeldSecret's header does not say %q (%s). The rule is written "+
				"down where a reader meets it or it is not written down: a residual named in a "+
				"ledger entry and not in the code is a decision that lives only in a commit message",
				fragment, why)
		}
	}
	// ── AND THE CITATION IS BOUND TO THE ROW IT NAMES, AT BOTH DOORS ────────────────────────
	//
	// A reader meets this rule at the pre-apply door as often as at the resolution, so residual
	// (1)'s citation is owed at both -- that asymmetry is exactly what made the stale sentence
	// survive: one door's header was corrected in the commit that left the other's standing.
	door := parseFuncDecl(t, "pqepoch.go", "refuseUnrotatedRemovalLocked")
	if door.Doc == nil || !strings.Contains(door.Doc.Text(), "late-joiner") {
		t.Fatalf("refuseUnrotatedRemovalLocked's header does not cite the `late-joiner` row. Both " +
			"doors carry this rule's residuals, and a residual corrected at one door and left " +
			"stale at the other is how the sentence this arm exists for survived a whole pass")
	}
	// AND WHAT THE CITATIONS POINT AT HAS TO EXIST. Without this the two headers could cite a row
	// that was deleted, which is the same defect pointing the other way.
	table, tableErr := os.ReadFile("removaltable_test.go")
	if tableErr != nil {
		t.Fatalf("reading removaltable_test.go, which both headers cite: %v", tableErr)
	}
	cited := map[string]string{
		"TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath": "" +
			"THE CONTROL: the instrument both headers name. If this is absent the two checks " +
			"below are reading the wrong file and would pass by finding nothing",
		"\"digest/one-leaf/earlier-epoch/late-joiner\"": "the row itself -- one page, two " +
			"receivers, the founder refusing and the member admitted later following",
		"func (self *rotWorld) admit(": "the world door that admits a member AFTER the founding " +
			"commit. The stale sentence's `because` clause was that no such door exists",
	}
	for literal, why := range cited {
		if !strings.Contains(string(table), literal) {
			t.Fatalf("removaltable_test.go does not hold %q (%s), and the removal rule's headers "+
				"cite it. A citation whose subject was deleted is a sentence that has stopped "+
				"being true, which is the defect this check exists for pointing the other way",
				literal, why)
		}
	}
	t.Logf("the rule's own header carries all %d dispositioned clauses of what it does NOT deliver, "+
		"both doors cite the `late-joiner` row, and that row and [rotWorld.admit] are in the tree",
		len(owed))
}

// isWordOctet is whether an octet can be part of a word, for the boundary the claim match needs.
func isWordOctet(octet byte) bool {
	return ('a' <= octet && octet <= 'z') || ('A' <= octet && octet <= 'Z') ||
		('0' <= octet && octet <= '9') || octet == '_'
}

// ── the small shared machinery ───────────────────────────────────────────────────────────────

// parseFunc is one production function's body, off the syntax tree.
func parseFunc(t *testing.T, file string, name string) *ast.BlockStmt {
	t.Helper()
	return parseFuncDecl(t, file, name).Body
}

// parseFuncDecl is the whole declaration, SIGNATURE INCLUDED, for the gate that has to read the
// result list rather than only the statements: whether the results are NAMED decides whether a bare
// `return` can exist, and a bare return is a statement that carries values without naming any.
func parseFuncDecl(t *testing.T, file string, name string) *ast.FuncDecl {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, declaration := range parsed.Decls {
		function, isFunc := declaration.(*ast.FuncDecl)
		if !isFunc || function.Name.Name != name || function.Body == nil {
			continue
		}
		return function
	}
	t.Fatalf("%s holds no function named %s", file, name)
	return nil
}

// returnedErrorName is what a returned call expression should be dispositioned under: the sentinel
// a fmt.Errorf wraps, or the name of the helper that built the error.
func returnedErrorName(call *ast.CallExpr) string {
	if name, ok := call.Fun.(*ast.Ident); ok {
		return name.Name
	}
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "Errorf" {
		return exprText(call.Fun)
	}
	for _, argument := range call.Args[1:] {
		if name, ok := argument.(*ast.Ident); ok && strings.HasPrefix(name.Name, "Err") {
			return name.Name
		}
	}
	return "a fmt.Errorf wrapping no sentinel"
}

// exprText is one expression as source, for a log line and for a disposition key.
func exprText(node ast.Expr) string {
	switch typed := node.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return exprText(typed.X) + "." + typed.Sel.Name
	case *ast.CallExpr:
		return exprText(typed.Fun) + "(...)"
	case *ast.IndexExpr:
		return exprText(typed.X) + "[...]"
	case *ast.SliceExpr:
		return exprText(typed.X) + "[:]"
	default:
		return fmt.Sprintf("%T", node)
	}
}

// moduleRoot is the directory holding this module's go.mod, found by walking up from the package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	at, err := os.Getwd()
	if err != nil {
		t.Fatalf("the working directory: %v", err)
	}
	for depth := 0; depth < 8; depth += 1 {
		if _, statErr := os.Stat(filepath.Join(at, "go.mod")); statErr == nil {
			return at
		}
		parent := filepath.Dir(at)
		if parent == at {
			break
		}
		at = parent
	}
	t.Fatalf("no go.mod above %s", at)
	return ""
}
