package urmessage

import (
	"testing"
)

// ── what a caller is handed stays what it was handed (msgrepo ledger item 227) ────────────────

// A [Message] THIS PACKAGE HAS ALREADY HANDED OUT IS NEVER WRITTEN AGAIN.
//
// WHAT WAS WRONG. [Group.Messages] copies the SLICE under this group's mutex and shares the
// *Message VALUES, and [Group.reapplyLocked] used to write `Deleted` and `Reactions` THROUGH those
// pointers every time a reaction or a tombstone arrived for a message already in the log. So the
// word "snapshot" was false: a caller holding what Messages answered had two fields rewritten
// under it, from a goroutine holding a lock the caller has no way to take.
//
// IT IS A DATA RACE AND IT WAS MEASURED AS ONE, at sdk bf4674b: a probe rendering Group.Messages on
// one goroutine while another called Group.Receive reported TWO data races under `-race`, both of
// them those two writes, reached through Receive -> commitWalkLocked -> rebuildDirtyLocked. Any Go
// caller that renders while it polls had it, which is every UI, and the C ABI's list handle
// (cgo/exports_message.go, messageEntry) had to take a defensive copy of exactly those two fields
// to keep its own accessors from answering from two instants.
//
// WHY THIS CASE NEEDS NO RACE DETECTOR, WHICH IS WHY IT IS HERE AND NOT ONLY IN cp3b's -race pass.
// The race is the CONSEQUENCE; the defect is that the value changes at all. A rebuild that REPLACES
// the message leaves every pointer ever handed out frozen, and "frozen" is a property one goroutine
// can check. cp3b's TestRenderingAConversationWhilePollingItIsNotADataRace holds the concurrency
// half, through a real server and a real Receive, under `-race`.
//
// WHAT GOES RED ON THE MUTATION: put `held.Deleted = false` / `held.Reactions = nil` back against
// `self.log[at]` -- that is, delete the copy in [Group.reapplyLocked] and write through the message
// the log already holds -- and both halves of this case fail.
func TestAMessageAlreadyHandedToACallerIsNeverWrittenAgain(t *testing.T) {
	world := newKindWalk(t)

	line := world.seal(mustEncodeText(t, "a line the group is about to react to and then delete"))
	opened, err := world.deliver(line)
	if err != nil {
		t.Fatalf("the page carrying the line answered %v", err)
	}
	if len(opened) != 1 {
		t.Fatalf("one line delivered %d entries", len(opened))
	}

	// THE CALLER'S COPY, taken the way a UI takes one: ask for the conversation, then render it
	// without holding anything.
	rendered := world.bob.Messages()
	if len(rendered) != 1 {
		t.Fatalf("Messages answered %d entries for one line", len(rendered))
	}
	frozen := rendered[0]
	if frozen.Deleted || len(frozen.Reactions) != 0 {
		t.Fatalf("the line arrived already deleted (%v) or already reacted to (%v)",
			frozen.Deleted, frozen.Reactions)
	}

	// ── AND THEN THE CONVERSATION MOVES, ON A LATER PAGE ─────────────────────────────────────
	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "👍"))
	tombstone := world.seal(mustEncodeTombstone(t, line.messageId))
	if _, err := world.deliver(add, tombstone); err != nil {
		t.Fatalf("the page carrying the reaction and the tombstone answered %v", err)
	}

	// THE PROPERTY, BOTH FIELDS, ON BOTH THINGS THIS PACKAGE HAD ALREADY HANDED OVER: the
	// Messages copy and the slice the earlier walk answered.
	for _, one := range []struct {
		what string
		held *Message
	}{
		{"the copy Group.Messages answered", frozen},
		{"the slice the walk that delivered the line answered", opened[0]},
	} {
		if len(one.held.Reactions) != 0 {
			t.Errorf("%s grew %d reaction(s) after it was handed over: %v",
				one.what, len(one.held.Reactions), one.held.Reactions)
		}
		if one.held.Deleted {
			t.Errorf("%s was marked deleted after it was handed over", one.what)
		}
	}

	// ── AND THE CONTROL, WITHOUT WHICH THE ABOVE PASSES ON A BUILD THAT APPLIES NOTHING ──────
	//
	// The effects really did land. A rebuild that had been gutted -- or a page that was quietly
	// dropped -- would leave the two assertions above true for the wrong reason, which is the
	// only way this case can lie.
	now := world.held(line.messageId)
	if len(now.Reactions) != 1 {
		t.Fatalf("the group holds %d reactions on the line after one REACTION_ADD: %v",
			len(now.Reactions), now.Reactions)
	}
	if !now.Deleted {
		t.Fatal("the group does not hold the line as deleted after a tombstone from its own sender")
	}
	// and asking again is how a caller sees the move, which is what the doc comment on
	// [Group.Messages] tells it to do
	again := world.bob.Messages()
	if len(again) != 1 {
		t.Fatalf("Messages answered %d entries after two effect records, which add no lines", len(again))
	}
	if len(again[0].Reactions) != 1 || !again[0].Deleted {
		t.Errorf("a fresh Messages answered %d reactions and Deleted=%v, so the move is not visible to a caller that re-asks",
			len(again[0].Reactions), again[0].Deleted)
	}
	if again[0] == frozen {
		t.Error("the re-ask answered the SAME *Message the first ask did, so nothing was replaced and the two cannot differ")
	}
}

// AN EFFECT THAT ARRIVES IN THE SAME PAGE AS ITS TARGET IS IN WHAT THAT WALK ANSWERS.
//
// THIS IS THE OTHER SIDE OF THE REPLACEMENT AND IT IS THE ONE EASY TO GET WRONG. [Group.Receive]
// answers `walk.opened`, which is filled per record as each one is delivered -- BEFORE
// [Group.commitWalkLocked] runs the rebuild the effects in the same page are owed. While the
// rebuild wrote through the message, that ordering did not matter: the entry in walk.opened and the
// entry in the log were one object. Now they are two, so the walk has to RE-READ what it is about
// to hand back, or a caller that renders Receive's answer -- rather than re-reading
// [Group.Messages] -- silently loses every reaction that arrived on the same page as its target.
//
// WHAT GOES RED ON THE MUTATION: delete the re-read loop at the top of [Group.commitWalkLocked].
func TestAnEffectInTheSamePageAsItsTargetIsInWhatTheWalkAnswers(t *testing.T) {
	world := newKindWalk(t)

	line := world.seal(mustEncodeText(t, "a line and its reaction, on one page"))
	add := world.seal(mustEncodeReaction(t, KindReactionAdd, line.messageId, "🎯"))
	tombstone := world.seal(mustEncodeTombstone(t, line.messageId))

	opened, err := world.deliver(line, add, tombstone)
	if err != nil {
		t.Fatalf("the page answered %v", err)
	}
	// a reaction and a tombstone add no line of their own
	if len(opened) != 1 {
		t.Fatalf("a line, a reaction and a tombstone on one page delivered %d entries", len(opened))
	}
	if len(opened[0].Reactions) != 1 || opened[0].Reactions[0].Emoji != "🎯" {
		t.Errorf("the walk answered a line carrying %v, and the same page carried a 🎯 for it",
			opened[0].Reactions)
	}
	if !opened[0].Deleted {
		t.Error("the walk answered a line that is not deleted, and the same page carried its tombstone")
	}
	// and the group agrees with what the walk answered, which is what says this is one instant
	// and not two
	if held := world.held(line.messageId); held != opened[0] {
		t.Errorf("the walk answered a different *Message from the one the group holds: %p against %p",
			opened[0], held)
	}
}
