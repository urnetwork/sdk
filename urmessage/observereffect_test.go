// AN OBSERVER'S REACTION IS NOT APPLIED, AND ITS TOMBSTONE IS: item 242's rulings 25 and 26, over
// the same real devices, real sessions and real commits [roleWorld] gives the rest of R4.
//
// WHAT R4 SHIPPED AND WHAT THESE CASES CLOSE. R4 hides an observer's MESSAGE and counts it, and
// [Group.deliverLocked] answers false for an effect record before that clause is reached -- so an
// observer's REACTION was applied to another member's message at every honest receiver, and
// [Message.Reactions] carries a sender_handle and no role, which leaves a UI drawing reaction chips
// with no way to know the reactor was an observer and nothing to collapse. Ruling 25 closes it:
// a message is KEPT because dropping it would hide that something was said, and a reaction that is
// not applied hides nothing, because the message it names is right there, whole.
//
// THE THREE PROPERTIES, AND THE MUTATION EACH ONE DIES TO:
//
//  1. AN OBSERVER'S REACTION REACHES NO HONEST RECEIVER'S [Message.Reactions], WHILE A MEMBER'S
//     IDENTICAL ONE DOES -- same emoji, same target, same walk, two receivers by two roads. Delete
//     the refusal in [Group.reapplyLocked] and the target carries two chips instead of one.
//  2. THE SAME FOR AN EFFECT THAT ARRIVES BEFORE ITS TARGET AND APPLIES LATER. Move the refusal to
//     the arrival path and this one goes red on its own: an effect whose target has not arrived is
//     HELD, and [Group.deliverLocked] applies it whenever the target is indexed.
//  3. AN OBSERVER'S TOMBSTONE IS APPLIED (ruling 26), measured in the SAME WALK as a refused
//     reaction from the SAME observer: one sender, one role, two record classes, two answers.
//     Refuse the tombstone too and this goes red, which is the whole of why the reason is written
//     at the clause.
//
// AND THE COUNTER IS PER RECORD AND NOT PER APPLICATION, which is what the two roads of case 1
// measure: one receiver refuses the held effect once and the other refuses it twice, and
// [Stats.ObserverReactionRefused] must read 1 at both.
package urmessage

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
)

// sealAs seals one already-encoded content plaintext through the sender's OWN SESSION, with the
// message_id every member derives for it, and never through a [Group] verb.
//
// IT IS [roleWorld.say] FOR THE KINDS THAT ARE NOT LINES, and it is the hostile build for the same
// reason that one is: after R4 every send door refuses an observer, so a case that drove the
// refusal could never produce the record whose RECEIPT is under test here. This seals exactly what
// [Group.sendContentLocked] seals -- the same content envelope, the same class, the same head --
// with no clause in front of it.
func (self *roleWorld) sealAs(sender *roleMember, plaintext []byte) *sealed {
	self.t.Helper()
	record, err := sender.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), plaintext, 0, nil)
	if err != nil {
		self.t.Fatalf("%s sealing at epoch %d: %v", sender.name, sender.handle.Epoch(), err)
	}
	messageId, err := sender.session.MessageIdOf(&record.Header)
	if err != nil {
		self.t.Fatalf("%s's MessageIdOf: %v", sender.name, err)
	}
	one := &sealed{recordId: self.nextRecordId, record: record, messageId: messageId[:]}
	self.nextRecordId += 1
	return one
}

// handleOf is one member's sender_handle, which is what a [Reaction] is attributed to.
func handleOf(t *testing.T, who *roleMember) []byte {
	t.Helper()
	handle, err := who.session.SenderHandle()
	if err != nil {
		t.Fatalf("%s's sender handle: %v", who.name, err)
	}
	return handle[:]
}

// reactionsOn is every reaction standing on one message RIGHT NOW, as (handle, emoji) pairs, read
// out of the log rather than out of a pointer taken before a walk: since ledger item 227 a rebuild
// REPLACES the message it rebuilt.
func reactionsOn(t *testing.T, receiver *roleMember, messageId []byte) []Reaction {
	t.Helper()
	held, found := receiver.group.heldLocked(messageId)
	if !found {
		t.Fatalf("%s's group holds no message %x", receiver.name, messageId[:8])
	}
	return held.Reactions
}

// ── 1. an observer's reaction is not applied, and a member's identical one is ─────────────────

// AN OBSERVER'S REACTION DOES NOT REACH ANY HONEST RECEIVER, AND A MEMBER'S IDENTICAL REACTION
// DOES, IN THE SAME RUN.
//
// THE CONTROL IS INLINE AND IT IS IDENTICAL IN EVERY RESPECT BUT ONE. Same emoji, same target,
// same page, sealed within a millisecond of each other: the ONLY difference between the reaction
// that lands and the reaction that does not is the role its sender held at the epoch it sealed it.
// A refusal that took everybody's reactions would pass every assertion about the observer and is
// exactly what this arm is here to catch -- and the two are the same (reactor, emoji) SHAPE but
// not the same (reactor, emoji) KEY, so [Group.reapplyLocked]'s dedupe set cannot be what removed
// one of them.
//
// TWO RECEIVERS BY TWO ROADS, because the refusal must not be a property of one walk's shape: bob
// takes one page holding all three records in server order, and erin takes three pages, so the
// target is rebuilt once at bob and twice at erin. The refusal therefore RUNS twice at erin, and
// [Stats.ObserverReactionRefused] reads 1 at both -- a counter at the refusal rather than at the
// record would make the two receivers disagree about the same group.
//
// WHAT WOULD GO RED: delete the skip in [Group.reapplyLocked] (two chips instead of one); refuse
// everybody (the member's arm); count at the refusal (erin reads 2); carry no role onto
// [contentEffect] (nothing is refused at all).
func TestAnObserversReactionIsNotAppliedAndAMembersIdenticalOneIs(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol", "dave", "erin")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")
	dave, erin := world.member("dave"), world.member("erin")

	// carol is an OBSERVER from epoch 2; dave is an unnamed member, which is MEMBER (ruling 8).
	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	for _, who := range []*roleMember{bob, carol, dave, erin} {
		world.ingest(who, alice, demotion)
	}

	const emoji = "👍"
	target := world.sealAs(alice, mustEncodeText(t, "a line two people react to"))
	fromObserver := world.sealAs(carol, mustEncodeReaction(t, KindReactionAdd, target.messageId, emoji))
	fromMember := world.sealAs(dave, mustEncodeReaction(t, KindReactionAdd, target.messageId, emoji))
	observerHandle, memberHandle := handleOf(t, carol), handleOf(t, dave)
	if bytes.Equal(observerHandle, memberHandle) {
		t.Fatal("the observer and the member share a sender_handle, so this case cannot tell their reactions apart")
	}

	// bob: ONE page, in server order.
	if err := world.deliver(bob, target, fromObserver, fromMember); err != nil {
		t.Fatalf("bob's walk: %v", err)
	}
	// erin: THREE pages, so the target is rebuilt once per page and the held observer effect is
	// refused on every rebuild after the first.
	for _, one := range []*sealed{target, fromObserver, fromMember} {
		if err := world.deliver(erin, one); err != nil {
			t.Fatalf("erin's walk over record %d: %v", one.recordId, err)
		}
	}

	for _, receiver := range []*roleMember{bob, erin} {
		log := receiver.group.Messages()
		if len(log) != 1 {
			t.Fatalf("%s's log holds %d entries, want 1: a reaction is a change to a message and never a line",
				receiver.name, len(log))
		}
		if log[0].SenderRoleAtSend != "owner" {
			t.Errorf("%s reads the target's own role as %q, want owner", receiver.name, log[0].SenderRoleAtSend)
		}
		standing := reactionsOn(t, receiver, target.messageId)
		for _, one := range standing {
			t.Logf("%s holds a %q from %x", receiver.name, one.Emoji, one.SenderHandle[:4])
		}
		if len(standing) != 1 {
			t.Fatalf("%s holds %d reaction(s) on the target, want exactly the member's one: an observer's reaction is not applied (ruling 25)",
				receiver.name, len(standing))
		}
		if !bytes.Equal(standing[0].SenderHandle, memberHandle) {
			t.Errorf("%s attributes the standing reaction to %x, and the member's handle is %x",
				receiver.name, standing[0].SenderHandle[:4], memberHandle[:4])
		}
		if standing[0].Emoji != emoji {
			t.Errorf("%s holds %q, want %q: the control is IDENTICAL to the refused reaction but for its sender's role",
				receiver.name, standing[0].Emoji, emoji)
		}
		stats := receiver.group.Stats()
		if stats.ObserverReactionRefused != 1 {
			t.Errorf("%s's Stats.ObserverReactionRefused is %d over one refused reaction record, want 1: it counts RECORDS and not applications",
				receiver.name, stats.ObserverReactionRefused)
		}
		if stats.HiddenObserver != 0 {
			t.Errorf("%s's Stats.HiddenObserver is %d and no observer sent a LINE here; a refused reaction is not a hidden row",
				receiver.name, stats.HiddenObserver)
		}
		if stats.RoleUndeterminable != 0 {
			t.Errorf("%s's Stats.RoleUndeterminable is %d, and every record here opened under a role this device can read",
				receiver.name, stats.RoleUndeterminable)
		}
		if stats.Opened != 3 {
			t.Errorf("%s's Stats.Opened is %d, want 3: a refused reaction is an OPENED record and is not a gap and not a drop",
				receiver.name, stats.Opened)
		}
	}
}

// ── 2. the effect that was waiting for its target is refused when it finally applies ──────────

// AN OBSERVER'S REACTION THAT ARRIVES BEFORE ITS TARGET IS STILL NOT APPLIED WHEN THE TARGET LANDS.
//
// THIS IS THE ORDINARY SHAPE OF A WALK AND NOT AN EXOTIC ONE, which is why ruling 25 names it: a
// record that fails to open holds the cursor back and is re-delivered AFTER record ids above it,
// so an effect routinely reaches a receiver before the message it names. That effect is HELD under
// [Group.effectsOn] and applied by [Group.deliverLocked] at the moment the target is indexed --
// a road that does not pass through the arrival path again.
//
// WHAT WOULD GO RED, AND THIS IS THE CASE THAT CATCHES IT ALONE: a refusal written on the arrival
// path -- "do not mark the target dirty" in [Group.noteEffectLocked], or a drop in
// [effectOf] -- leaves case 1 green and this one red, because nothing on the arrival path runs
// when the target finally arrives.
func TestAnObserversReactionHeldForItsTargetIsRefusedWhenItFinallyApplies(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol", "dave")
	alice, bob, carol, dave := world.member("alice"), world.member("bob"), world.member("carol"), world.member("dave")

	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	for _, who := range []*roleMember{bob, carol, dave} {
		world.ingest(who, alice, demotion)
	}

	const emoji = "🎯"
	target := world.sealAs(alice, mustEncodeText(t, "a line that arrives last"))
	fromObserver := world.sealAs(carol, mustEncodeReaction(t, KindReactionAdd, target.messageId, emoji))
	fromMember := world.sealAs(dave, mustEncodeReaction(t, KindReactionAdd, target.messageId, emoji))

	// the two reactions alone, naming a message this group has never seen
	if err := world.deliver(bob, fromObserver, fromMember); err != nil {
		t.Fatalf("bob's walk over two reactions for a target that has not arrived: %v", err)
	}
	if held := bob.group.Messages(); len(held) != 0 {
		t.Fatalf("bob's log holds %d entries before the target arrived", len(held))
	}
	if got := bob.group.Stats().ObserverReactionRefused; got != 1 {
		t.Errorf("Stats.ObserverReactionRefused is %d after the reaction record arrived, want 1: it counts the RECORD, whether or not the target ever comes",
			got)
	}

	// and now the target, which is the moment every held effect applies
	if err := world.deliver(bob, target); err != nil {
		t.Fatalf("bob's walk over the target: %v", err)
	}
	standing := reactionsOn(t, bob, target.messageId)
	for _, one := range standing {
		t.Logf("bob holds a %q from %x", one.Emoji, one.SenderHandle[:4])
	}
	if len(standing) != 1 {
		t.Fatalf("the target arrived carrying %d reaction(s), want exactly the member's one: a HELD observer reaction is refused when it applies, not only when it arrives",
			len(standing))
	}
	if !bytes.Equal(standing[0].SenderHandle, handleOf(t, dave)) {
		t.Errorf("the standing reaction is attributed to %x and the member's handle is %x",
			standing[0].SenderHandle[:4], handleOf(t, dave)[:4])
	}
	if got := bob.group.Stats().ObserverReactionRefused; got != 1 {
		t.Errorf("Stats.ObserverReactionRefused is %d after the target arrived and the held effect was refused a second time, want 1",
			got)
	}
}

// ── 2b. the record is REMEMBERED once and refused at every rebuild, which is where the rule lives ─

// A REFUSED REACTION IS HELD LIKE ANY OTHER EFFECT AND IS REFUSED WHERE IT IS APPLIED, and the
// counter moves once for the RECORD however many times it is delivered or rebuilt.
//
// WHY THIS CASE EXISTS, AND IT IS A MUTANT THAT SURVIVED THE TWO ABOVE. "Refuse it on the arrival
// path" has two spellings and the cases above kill only one of them. Leaving the effect in
// [Group.effectsOn] and refusing to mark its target dirty is killed by case 1, because
// [Group.reapplyLocked] rebuilds from the WHOLE effect set and another member's reaction on the
// same message drags it in. DROPPING the record at [Group.noteEffectLocked] instead is invisible to
// both: nothing is ever applied either way. What tells them apart is the SECOND defence against a
// re-delivery -- "one record is one effect" -- which a dropped record never reaches, so a rewind
// over an earlier failure counts the same reaction twice.
//
// IT IS BELOW THE WALK FOR THE REASON [deliverOneThroughAWalk] EXISTS. A walk's FIRST defence
// ([Group.delivered], Stats.SkippedSeen) turns a re-delivered record away before
// [Group.deliverLocked] is reached, so the dedupe this case measures can only be driven from here.
//
// WHAT WOULD GO RED: drop a refused reaction at the note (effectsOn is empty and the second
// delivery counts again); count at the refusal rather than at the record (the rebuilds add up);
// refuse the member's reaction too.
func TestARefusedObserverReactionIsHeldOnceAndRefusedAtEveryRebuild(t *testing.T) {
	group := &Group{}
	group.initTables()

	observer := bytes.Repeat([]byte{0x01}, 16)
	member := bytes.Repeat([]byte{0x02}, 16)
	lineId, observerId, memberId := aTarget(0xE1), aTarget(0xE2), aTarget(0xE3)

	text := &Content{Kind: KindText, Text: "a line an observer reacts to"}
	deliverOneThroughAWalk(group, newMessage(text, 10, member, nil, false, 0, lineId, "member"), text)

	reaction := &Content{Kind: KindReactionAdd, Target: lineId, Emoji: "👍"}
	// the same observer record twice, which is what a rewind over an earlier failure delivers
	deliverOneThroughAWalk(group, newMessage(reaction, 11, observer, nil, false, 0, observerId, "observer"), reaction)
	deliverOneThroughAWalk(group, newMessage(reaction, 11, observer, nil, false, 0, observerId, "observer"), reaction)
	// and the member's identical one, which rebuilds the same target a third time
	deliverOneThroughAWalk(group, newMessage(reaction, 12, member, nil, false, 0, memberId, "member"), reaction)

	held := heldIn(t, group, lineId)
	if len(held.Reactions) != 1 || !bytes.Equal(held.Reactions[0].SenderHandle, member) {
		t.Errorf("the line carries %v, want exactly the member's 👍", held.Reactions)
	}
	if count := len(group.effectsOn[messageKeyOf(lineId)]); count != 2 {
		t.Errorf("the target holds %d effect record(s), want 2: a refused reaction is HELD like any other effect and refused where it is APPLIED, so that one record stays one effect",
			count)
	}
	if got := group.stats.ObserverReactionRefused; got != 1 {
		t.Errorf("Stats.ObserverReactionRefused is %d over ONE reaction record delivered twice and refused at three rebuilds, want 1",
			got)
	}
}

// ── 3. the tombstone is applied, and that is deliberate (ruling 26) ───────────────────────────

// AN OBSERVER'S TOMBSTONE IS APPLIED TO THE OBSERVER'S OWN MESSAGE, IN THE SAME WALK AS A REFUSED
// REACTION FROM THE SAME OBSERVER.
//
// ONE SENDER, ONE ROLE, ONE WALK, TWO RECORD CLASSES, TWO ANSWERS -- which is the only shape that
// measures ruling 26 rather than an accident of the fixture: a build that refused every effect
// from an observer would pass case 1 and case 2 and fail here alone.
//
// WHY IT IS NOT AN INCONSISTENCY, in the ruling's own words: a tombstone only ever removes the
// observer's OWN content ([contentEffect.applyTo]'s T-b requires it to come from the target's own
// sender), so refusing it would keep VISIBLE something its author asked to retract. The role model
// exists to stop an observer ADDING to the group, not to trap its own words there.
//
// AND THE HIDE IS STILL A HIDE. The observer's line is in the log with its text intact and
// [Stats.HiddenObserver] counts it (ruling 16); what the tombstone changes is
// [Message.Deleted], which is the author's own retraction of a row a UI was already collapsing.
//
// WHAT WOULD GO RED: add KindTombstone to [contentEffect.isObserverReaction]'s switch.
func TestAnObserversTombstoneIsAppliedAndItsReactionIsNotInTheSameWalk(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(bob, alice, demotion)
	world.ingest(carol, alice, demotion)

	const said = "an observer that sent anyway, and took it back"
	fromOwner := world.sealAs(alice, mustEncodeText(t, "the owner's line, which the observer reacts to"))
	fromObserver := world.sealAs(carol, mustEncodeText(t, said))
	reaction := world.sealAs(carol, mustEncodeReaction(t, KindReactionAdd, fromOwner.messageId, "👀"))
	tombstone := world.sealAs(carol, mustEncodeTombstone(t, fromObserver.messageId))

	if err := world.deliver(bob, fromOwner, fromObserver, reaction, tombstone); err != nil {
		t.Fatalf("bob's walk: %v", err)
	}

	// the reaction: refused, on the owner's line
	if standing := reactionsOn(t, bob, fromOwner.messageId); len(standing) != 0 {
		t.Errorf("the owner's line carries %d reaction(s) from an observer, want 0 (ruling 25)", len(standing))
	}
	// the tombstone: applied, on the observer's own line
	held, found := bob.group.heldLocked(fromObserver.messageId)
	if !found {
		t.Fatal("bob's log does not hold the observer's own line at all; the hide is a hide and never a drop (ruling 16)")
	}
	if !held.Deleted {
		t.Error("the observer's own line is NOT deleted: an observer's tombstone over its own message is APPLIED (ruling 26), because the role model stops an observer ADDING to the group and does not trap its own words there")
	}
	if held.Text != said || held.SenderRoleAtSend != "observer" {
		t.Errorf("the observer's line reads %q as %q; the hide keeps the content and the role",
			held.Text, held.SenderRoleAtSend)
	}
	stats := bob.group.Stats()
	if stats.HiddenObserver != 1 {
		t.Errorf("Stats.HiddenObserver is %d over one observer LINE, want 1", stats.HiddenObserver)
	}
	if stats.ObserverReactionRefused != 1 {
		t.Errorf("Stats.ObserverReactionRefused is %d over one refused reaction and one APPLIED tombstone, want 1: the tombstone is not refused and is not counted here",
			stats.ObserverReactionRefused)
	}
}
