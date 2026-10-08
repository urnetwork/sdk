package urmessage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"
)

// ── the replay's cost, as a complexity gate ──────────────────────────────────────────────────

// REPLAYING n EFFECTS ON ONE MESSAGE COSTS SUB-QUADRATIC ALLOCATIONS.
//
// WHAT THIS IS ABOUT. Every effect a walk delivers rebuilds its target from that target's whole
// effect set. Before [Group.dirtyTargets] the rebuild ran once PER EFFECT, so n reactions on one
// message cost n rebuilds of n effects. It is not a benchmark's problem: the records are ANOTHER
// member's, the cursor is not persisted so history is a re-fetch, and the victim therefore pays the
// whole of it ON EVERY LAUNCH while the sender paid once. Measured on the real codec before the
// dirty set existed: 4,000 REACTION_ADD records on one message cost 41.0s and ~1.53 GB on every
// other member's client.
//
// WHY THE METRIC IS `Mallocs` AND NOT WALL TIME. Wall time on that measurement ran 11.6ms / 75.8ms /
// 626ms / 5.21s / 41.0s across four doublings on ONE machine and would run differently on another,
// so a wall clock bound is either loose enough to pass a regression or tight enough to flake in CI.
// The allocation COUNT is a property of the code and not of the CPU: the old shape copied one
// sender_handle per standing reaction per rebuild, which is sum(k, k=1..n) = n^2/2 allocations, and
// the published numbers -- 35,090 / 133,406 / 518,105 / 2,039,370 / 8,087,950 -- are that formula
// to within the linear term. `runtime.GC()` runs before each window because Mallocs is cumulative
// and a collection inside the window would otherwise be charged to it.
//
// WHAT THIS CATCHES: a return to a rebuild per effect. Reverting [Group.dirtyTargets] -- putting
// `self.reapplyLocked(effect.target)` back at the end of [Group.noteEffectLocked] -- takes the
// measured ratios from 1.98x/1.99x/2.00x to 3.94x/3.97x/3.98x and ALL THREE doublings go red, while
// the control below stays at 2.00x. The counts on the same ladder are 2,042 / 4,051 / 8,068 /
// 16,105 repaired against 258,870 / 1,018,900 / 4,043,022 / 16,105,593 reverted -- 1,000x at
// n=4,000. That was run, not argued.
//
// WHAT THIS WOULD MISS, and the first item is the one that matters most:
//
//   - THE DEDUPE SCAN, WHICH IS THE OTHER HALF OF THE FIX, AND THIS GATE DOES NOT DEFEND IT.
//     [contentEffect.applyTo]'s ADD arm used to answer "has this reactor already reacted with this
//     emoji" by SCANNING the reactions the same rebuild had just appended: m^2 comparisons inside
//     one rebuild. THAT SCAN ALLOCATES NOTHING. Reverting the `seen` set alone leaves this case
//     GREEN -- 1.98x / 1.99x / 1.99x -- and leaves it green at a LOWER count than the repaired
//     build (12,098 against 16,105 at n=4,000), because the set costs one key per ADD and the scan
//     costs none. Measured by running it, not predicted.
//
//     WHAT THE SCAN COSTS, SO THE GAP IS PRICED RATHER THAN ONLY ADMITTED. One rebuild of m
//     reactions, scan against set: 17.2ms/1.2ms at m=4,000, 68.7ms/1.1ms at 8,000, 276ms/3.7ms at
//     16,000, 1.30s/8.0ms at 32,000 -- 4.0x per doubling against 2.1x, and 162x at the top. It bites
//     for real when the SERVER chooses one record per page, which restores a commit per record and
//     so defeats the dirty set: n reactions arriving one page at a time cost 65.6ms / 546ms / 4.97s
//     / 37.4s over n = 500 / 1,000 / 2,000 / 4,000 with the scan (8x per doubling, a cube) against
//     15.2ms / 62.6ms / 303ms / 1.21s with the set (4x, a square).
//
//     NO ALLOCATION METRIC CAN GATE THAT, and a wall-clock one would be reading 4x against 8x
//     through CI noise. What holds the set's CORRECTNESS is
//     TestAReactionReAddedAfterItsRemoveStandsAgain, below, which is the one way the set can be
//     wrong; what holds its COST is this paragraph and nothing executable.
//
//   - A CONSTANT FACTOR. Five allocations per reaction and five hundred are both linear and both
//     pass. This is about the SHAPE and says nothing about the coefficient.
//
//   - THE WALK'S OWN COST. The window is the replay -- fold n effect records in, then commit -- and
//     not the page walk around it, because OpenRecord's ~380 allocations per record are a constant
//     per record that a quadratic term has to climb over before it is legible. Measured through the
//     full walk at these n the old shape reads 2.49x / 2.79x / 3.14x, which a 3.0 bound catches on
//     one doubling out of three; measured here it reads 3.98x / 3.99x / 3.99x. The cost of that
//     choice is that a walk which itself became quadratic would not be seen here.
//
//   - ANYTHING BELOW ~n=500, where the fixed cost of the first rebuild is still a visible share.
func TestReplayingManyEffectsOnOneMessageIsSubQuadratic(t *testing.T) {
	// THREE DOUBLINGS IS THE LEAST THAT TELLS A LINE FROM A CURVE: two points fit anything, three
	// ratios do not.
	ladder := []int{500, 1000, 2000, 4000}

	mallocs := make([]uint64, len(ladder))
	for index, n := range ladder {
		mallocs[index] = mallocsToReplayOnOneMessage(t, n)
		t.Logf("one message, n=%-5d mallocs=%d", n, mallocs[index])
	}
	assertSubQuadratic(t, "n reactions on ONE message", ladder, mallocs)

	// ── THE CONTROL, WHICH IS WHAT LOCALISES THE COST ────────────────────────────────────────
	//
	// The same n effect records spread over n DIFFERENT targets was ALREADY linear before either
	// edit (0.55 / 2.17 / 2.18 / 4.44 ms over the same ladder), because each target rebuilds from
	// its own one-effect set. So the cost was never in the walk, in the effect table or in the
	// number of records: it was in the PER-TARGET rebuild, which is the only thing the fix
	// touched. Without this half, a gate that went green because the whole replay had been
	// gutted would read the same as one that went green because it was repaired.
	spread := make([]uint64, len(ladder))
	for index, n := range ladder {
		spread[index] = mallocsToReplayOverDistinctMessages(t, n)
		t.Logf("n messages,   n=%-5d mallocs=%d", n, spread[index])
	}
	assertSubQuadratic(t, "n reactions over n DIFFERENT messages (the control)", ladder, spread)
}

// ── the one way the rebuild's dedupe set can be wrong ────────────────────────────────────────

// A REACTION RE-ADDED AFTER ITS OWN REMOVE STANDS AGAIN.
//
// THIS IS THE TRAP THE DEDUPE SET SETS, and it is the whole of what the set can get wrong. The ADD
// arm used to scan [Message.Reactions], which the REMOVE arm had just filtered, so the two were in
// step by construction: whatever the REMOVE took out, the next ADD could no longer find. A set
// beside the slice is a SECOND copy of that state, and it is in step only because the REMOVE arm
// deletes the key as well as the row. Drop that one `delete` and everything a single-shot case
// measures still passes -- one ADD stands, one REMOVE cancels, two reactors stay apart -- while
// react, un-react, react again silently loses the last one.
//
// THE ORDER THAT REACHES IT IS SERVER ORDER AND NOT ARRIVAL ORDER, which is why this is a replay
// case and not an API one: [Group.reapplyLocked] sorts by record id and applies ADD(1), REMOVE(2),
// ADD(3) in that order into ONE set, whatever order the three records were delivered in. The second
// half below delivers them BACKWARDS to say exactly that.
//
// WHAT WOULD GO RED: delete `delete(seen, reactionKey(...))` from the REMOVE arm of
// [contentEffect.applyTo]. Both halves.
func TestAReactionReAddedAfterItsRemoveStandsAgain(t *testing.T) {
	sender := bytes.Repeat([]byte{0x01}, 16)
	lineId := aTarget(0xC7)

	replay := func(t *testing.T, order []int) *Message {
		t.Helper()
		group := &Group{}
		group.initTables()
		line := &Content{Kind: KindText, Text: "a line reacted to, taken back, and reacted to again"}
		if !deliverOneThroughAWalk(group, newMessage(line, 1, sender, nil, false, 0, lineId, "member"), line) {
			t.Fatal("a TEXT did not become a line of the conversation")
		}
		// record 11 ADD, record 12 REMOVE, record 13 ADD -- one reactor, one emoji.
		steps := []*Content{
			{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"},
			{Kind: KindReactionRemove, Target: lineId, Emoji: "👍"},
			{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"},
		}
		for _, step := range order {
			entry := steps[step]
			group.deliverLocked(newMessage(entry, uint64(11+step), sender, nil, false, 0, countedId(0xC8, step), "member"), entry)
		}
		group.rebuildDirtyLocked()
		// THE GROUP'S ANSWER AND NOT THE MESSAGE THIS HELPER DELIVERED: the rebuild REPLACES the
		// message rather than writing through it, so the delivered pointer is a snapshot from
		// before the three records. See heldIn.
		return heldIn(t, group, lineId)
	}

	held := replay(t, []int{0, 1, 2})
	if len(held.Reactions) != 1 {
		t.Errorf("ADD, REMOVE, ADD of one (reactor, emoji) left %d reactions and the last word was an ADD: %v",
			len(held.Reactions), held.Reactions)
	}

	// THE SAME THREE RECORDS BACKWARDS, which is what one failed open produces. Server order is
	// the same order, so the answer is the same answer.
	backwards := replay(t, []int{2, 1, 0})
	if len(backwards.Reactions) != 1 {
		t.Errorf("the same three records delivered in the order 13, 12, 11 left %d reactions; server order is what decides and the ADD at record 13 is last: %v",
			len(backwards.Reactions), backwards.Reactions)
	}

	// THE CONTROL, which is what keeps the two above from passing for a build that never removes
	// anything: the same run without the trailing ADD leaves nothing standing.
	cancelled := replay(t, []int{0, 1})
	if len(cancelled.Reactions) != 0 {
		t.Errorf("the control: ADD then REMOVE of one (reactor, emoji) left %v standing", cancelled.Reactions)
	}
}

// assertSubQuadratic holds one ladder of allocation counts to a growth below the quadratic answer.
//
// THE BOUND IS ON THE RATIO PER DOUBLING, which is what makes it machine-independent: linear work
// doubles (2.0) and quadratic work quadruples (4.0) on every machine. 3.0 sits between them with
// room on both sides -- well clear of the 2.00x this build gives and well under the 3.99x the old
// one gave.
func assertSubQuadratic(t *testing.T, what string, ladder []int, mallocs []uint64) {
	t.Helper()
	const worstRatio = 3.0
	ratios := []string{}
	for index := 1; index < len(mallocs); index += 1 {
		if mallocs[index-1] == 0 {
			t.Fatalf("%s: n=%d allocated nothing, so this case measured no work", what, ladder[index-1])
		}
		ratio := float64(mallocs[index]) / float64(mallocs[index-1])
		ratios = append(ratios, fmt.Sprintf("%d->%d: %.2fx", ladder[index-1], ladder[index], ratio))
		if worstRatio <= ratio {
			t.Errorf("%s: n went from %d to %d and allocations went from %d to %d, a ratio of %.2fx. A doubling that costs %.1fx or more is a QUADRATIC replay, which is what a rebuild PER EFFECT costs; linear is 2.0x",
				what, ladder[index-1], ladder[index], mallocs[index-1], mallocs[index], ratio, worstRatio)
		}
	}
	t.Logf("%s: allocation growth per doubling %v (linear is 2.00x, quadratic is 4.00x)", what, ratios)
}

// mallocsToReplayOnOneMessage folds n REACTION_ADD records naming ONE message into a group and then
// commits, and answers what that cost in allocations.
//
// IT IS THE REPLAY AND NOT THE WALK, and the two calls are the two the walk makes:
// [Group.deliverLocked] per record, which is [Group.openPageLocked]'s inner step, and one
// [Group.rebuildDirtyLocked], which is [Group.commitWalkLocked]'s. Everything a walk would do
// BESIDE them -- the fetch, the AEADs, the [Message] construction -- is built before the window or
// is not this property's business. TestReplayingManyEffectsOnOneMessageIsSubQuadratic's comment says
// what that costs the gate.
//
// THE REACTIONS ARE DISTINCT AND THAT IS THE POINT, not a convenience. n copies of one
// (reactor, emoji) pair dedupe to ONE standing reaction, so the scan the old ADD arm ran would have
// been a scan of a one-element slice and this case would have measured a straight line over a build
// with the defect in it. What a flooder sends is n reactions that all STAND, and a distinct emoji
// per record is the cheapest way to say that with one sender_handle.
func mallocsToReplayOnOneMessage(t *testing.T, n int) uint64 {
	t.Helper()
	group := &Group{}
	group.initTables()
	sender := bytes.Repeat([]byte{0x01}, 16)

	lineId := aTarget(0xF1)
	line := &Content{Kind: KindText, Text: "one message, and every reaction in the group"}
	held := newMessage(line, 1, sender, nil, false, 0, lineId, "member")
	if !deliverOneThroughAWalk(group, held, line) {
		t.Fatal("a TEXT did not become a line of the conversation")
	}

	entries, records := reactionRecords(n, sender, func(int) []byte { return lineId })

	runtime.GC()
	before := mallocsNow()
	for index := range records {
		group.deliverLocked(records[index], entries[index])
	}
	group.rebuildDirtyLocked()
	after := mallocsNow()

	// THE CONTROL ON THE MEASUREMENT: a window over a replay that dropped the reactions on the
	// floor would be cheap and linear and would say nothing. All n of them stand.
	//
	// IT IS READ OUT OF THE GROUP AND NOT OFF `held`, WHICH IS NOT A TIDY-UP. `held` is the
	// [Message] this helper delivered, and since ledger item 227 a rebuild REPLACES the message it
	// rebuilt rather than writing through it -- so `held` is frozen at the instant it was
	// delivered and carries no reactions at all. Reading it would make this control a control on
	// nothing, and it would say so by failing.
	standing, found := group.heldLocked(lineId)
	if !found {
		t.Fatalf("n=%d: the line this case reacted to is not in the group", n)
	}
	if len(standing.Reactions) != n {
		t.Fatalf("n=%d: %d reactions stand on the line, so the replay this case measured did not do the work",
			n, len(standing.Reactions))
	}
	return after - before
}

// mallocsToReplayOverDistinctMessages is the same n records over n DIFFERENT targets: one reaction
// each. It is the control that says the cost this gate bounds lives in the per-target rebuild.
func mallocsToReplayOverDistinctMessages(t *testing.T, n int) uint64 {
	t.Helper()
	group := &Group{}
	group.initTables()
	sender := bytes.Repeat([]byte{0x01}, 16)

	targets := make([][]byte, n)
	for index := range targets {
		targets[index] = countedId(0xA0, index)
		line := &Content{Kind: KindText, Text: "a line"}
		if !deliverOneThroughAWalk(group, newMessage(line, uint64(index+1), sender, nil, false, 0, targets[index], "member"), line) {
			t.Fatalf("line %d did not become a line of the conversation", index)
		}
	}
	entries, records := reactionRecords(n, sender, func(index int) []byte { return targets[index] })

	runtime.GC()
	before := mallocsNow()
	for index := range records {
		group.deliverLocked(records[index], entries[index])
	}
	group.rebuildDirtyLocked()
	after := mallocsNow()

	for index, target := range targets {
		held, found := group.heldLocked(target)
		if !found {
			t.Fatalf("n=%d: message %d is not in the group", n, index)
		}
		if standing := held.Reactions; len(standing) != 1 {
			t.Fatalf("n=%d: message %d carries %d reactions and one record named it", n, index, len(standing))
		}
	}
	return after - before
}

// reactionRecords builds n REACTION_ADD records, each with its own message_id and its own emoji, so
// that constructing them is not charged to the window that replays them.
func reactionRecords(n int, sender []byte, targetOf func(index int) []byte) ([]*Content, []*Message) {
	entries := make([]*Content, n)
	records := make([]*Message, n)
	for index := 0; index < n; index += 1 {
		entries[index] = &Content{
			Kind:   KindReactionAdd,
			Target: targetOf(index),
			Emoji:  distinctEmoji(index),
		}
		records[index] = newMessage(entries[index], uint64(index+1_000_000), sender, nil, false, 0,
			countedId(0xE0, index), "member")
	}
	return entries, records
}

// countedId is one distinct message_id per index: a fill octet so two families cannot collide, and
// the index itself in the last eight octets.
func countedId(family byte, index int) []byte {
	id := bytes.Repeat([]byte{family}, MessageIdBytes)
	binary.BigEndian.PutUint64(id[MessageIdBytes-8:], uint64(index))
	return id
}

// mallocsNow is the cumulative allocation COUNT, and it is a count rather than a byte total on
// purpose: the old shape's square was a square in the NUMBER of objects -- one sender_handle copy
// per standing reaction per rebuild -- and a byte total folds that together with the sizes the
// allocator rounds to.
func mallocsNow() uint64 {
	stats := runtime.MemStats{}
	runtime.ReadMemStats(&stats)
	return stats.Mallocs
}

// distinctEmoji is one emoji per index, and it is honest about which half of spec A section 7.4a it
// leans on. [checkEmoji] validates UTF-8 and the 64 octet cap and NOTHING ELSE -- Go has no UAX-29
// segmentation, which that function's own comment states as the named gap -- so a run of codepoints
// from the emoticons block onward is accepted whether or not every one of them is assigned. That is
// exactly what a flooder with a patched client sends, so it is the right operand for a cost
// measurement even though a stricter validator would refuse the tail of the range.
func distinctEmoji(index int) string {
	return string(rune(0x1F600 + index))
}
