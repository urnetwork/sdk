package cp3b

import (
	"context"
	"testing"
)

// RENDERING A CONVERSATION WHILE POLLING IT IS NOT A DATA RACE (msgrepo ledger item 227).
//
// THIS CASE ONLY SEES ANYTHING UNDER `-race`, AND THAT IS WHY IT IS HERE RATHER THAN IN
// urmessage's own suite: `sdk/test.sh` runs every submodule with `-race ./...`, so this module is
// one of the places a race pass actually happens. The deterministic half of the same property --
// a [urmessage.Message] a caller is already holding is never written again -- is
// TestAMessageAlreadyHandedToACallerIsNeverWrittenAgain in urmessage, which goes red with no race
// detector at all.
//
// WHAT WAS MEASURED BEFORE THE FIX, at sdk bf4674b: two data races, both WRITES in
// Group.reapplyLocked (`held.Deleted = false` and `held.Reactions = nil`, urmessage/group.go:2511
// and :2512) reached through Receive -> commitWalkLocked -> rebuildDirtyLocked, against reads on
// the rendering goroutine. Group.Messages copied the SLICE under the group's mutex and handed back
// the LIVE *Message values, so a caller rendering what it already took shared those two fields with
// a rebuild running under a lock the caller has no way to take. Any Go caller that renders while it
// polls had it, which is every UI.
//
// WHY THE GOROUTINES ARE GATED THE WAY THEY ARE. The race detector reports a pair of accesses with
// no happens-before edge BETWEEN THEM, whether or not they physically overlap. Both goroutines are
// released by one close(start) and joined afterwards, so each is ordered against the test and
// neither is ordered against the other -- which makes the report a property of the code rather
// than of how the scheduler happened to run.
func TestRenderingAConversationWhilePollingItIsNotADataRace(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, newGroupId(t))

	line, err := aliceGroup.Send(ctx, "a line somebody is about to react to")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the line: %v", err)
	}

	// THE RENDERER'S OWN COPY, taken exactly the way a UI takes one: ask for the conversation,
	// then paint it without holding anything.
	snapshot := bobGroup.Messages()
	if len(snapshot) != 1 {
		t.Fatalf("bob holds %d entries after one line", len(snapshot))
	}

	// Each round puts one more effect on the server for the poll to fold in, so every round has
	// a rebuild to race against rather than only the first.
	for round, emoji := range []string{"👍", "🎉", "🙏", "👀"} {
		if _, err := aliceGroup.React(ctx, line.MessageId, emoji); err != nil {
			t.Fatalf("round %d: alice's React: %v", round, err)
		}

		start := make(chan struct{})
		painted := make(chan int)
		polled := make(chan error)

		// THE RENDERER. It reads the two fields the rebuild writes, off the pointer it was
		// handed before the poll started.
		go func() {
			<-start
			seen := 0
			for step := 0; step < 4096; step += 1 {
				if snapshot[0].Deleted {
					seen += 1
				}
				seen += len(snapshot[0].Reactions)
			}
			painted <- seen
		}()

		// THE POLL, which is the real thing and not a stand-in: a fetch off a running server,
		// through the same commitWalkLocked -> rebuildDirtyLocked the ledger item names.
		go func() {
			<-start
			_, err := bobGroup.Receive(ctx)
			polled <- err
		}()

		close(start)
		seen := <-painted
		if err := <-polled; err != nil {
			t.Fatalf("round %d: bob's Receive: %v", round, err)
		}
		t.Logf("round %d: the renderer read %d reaction rows off its own copy while the poll ran",
			round, seen)
	}

	// AND THE CONTROL, which is what says the rounds above had work to race against: the poll
	// really did fold four reactions onto the line, in the group rather than in the copy.
	if standing := messageById(t, bobGroup, line.MessageId).Reactions; len(standing) != 4 {
		t.Fatalf("bob's group carries %d reactions after four rounds, so this case raced nothing",
			len(standing))
	}
	// The renderer's copy is frozen at the instant it was taken, which is what Group.Messages
	// promises and what the repair makes true.
	if len(snapshot[0].Reactions) != 0 {
		t.Fatalf("the copy taken before any reaction arrived carries %d of them", len(snapshot[0].Reactions))
	}
}

// WHAT Group.Receive HANDS BACK CARRIES THE EFFECTS THAT ARRIVED IN THE SAME PAGE.
//
// THIS IS THE OTHER HALF OF LEDGER ITEM 227'S REPAIR AND IT IS THE HALF THAT COULD HAVE BROKEN A
// CALLER SILENTLY. Receive answers the messages a walk opened, collected per record as each one is
// delivered -- which is BEFORE the rebuild that runs when the walk commits. While the rebuild wrote
// THROUGH the message, that ordering did not matter: the entry in the returned slice and the entry
// in the log were one object. They are two now, so the walk re-reads what it is about to return. A
// client that appends Receive's answer to its own view -- rather than re-reading Group.Messages --
// would otherwise lose every reaction and every deletion that arrived on the same page as the line
// it is about, which on a first launch over a whole history is most of them.
//
// IT IS HERE AND NOT ONLY IN urmessage BECAUSE THIS DRIVES THE REAL Receive. urmessage's
// TestAnEffectInTheSamePageAsItsTargetIsInWhatTheWalkAnswers drives openPageLocked and
// commitWalkLocked directly; what it cannot show is that Group.Receive's own
// `return walk.opened, self.commitWalkLocked(walk)` hands the caller the re-read values.
//
// WHAT GOES RED ON THE MUTATION: delete the re-read loop at the top of Group.commitWalkLocked.
func TestReceiveAnswersTheEffectsThatArrivedInTheSamePage(t *testing.T) {
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
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, newGroupId(t))

	// THE LINE, ITS REACTION AND ITS TOMBSTONE, ALL BEFORE BOB FETCHES ANYTHING. Bob's first
	// Receive is therefore one page carrying a target and two effects on it, which is exactly what
	// a first launch over an existing conversation looks like.
	line, err := aliceGroup.Send(ctx, "a line, reacted to and deleted before anyone fetched it")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := aliceGroup.React(ctx, line.MessageId, "👍"); err != nil {
		t.Fatalf("alice's React: %v", err)
	}
	if _, err := aliceGroup.Delete(ctx, line.MessageId); err != nil {
		t.Fatalf("alice's Delete: %v", err)
	}

	received, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	// a reaction and a tombstone add no line of their own
	if len(received) != 1 {
		t.Fatalf("bob received %d entries for one line, one reaction and one tombstone", len(received))
	}
	if len(received[0].Reactions) != 1 {
		t.Errorf("what Receive answered carries %d reactions and the same page carried one for it: %v",
			len(received[0].Reactions), received[0].Reactions)
	}
	if !received[0].Deleted {
		t.Error("what Receive answered is not deleted, and the same page carried its tombstone")
	}
	// and it is the SAME message the group holds, which is what says this is one instant
	held := messageById(t, bobGroup, line.MessageId)
	if held != received[0] {
		t.Errorf("Receive answered %p and bob's group holds %p, so the two can disagree",
			received[0], held)
	}
}
