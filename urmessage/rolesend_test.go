// OBSERVER READ-ONLY, BOTH ENDS OF IT: item 242's R4, over the same real devices, real sessions
// and real commits [roleWorld] gives the receiving and committing arms.
//
// THE FOUR PROPERTIES, AND THE MUTATION EACH ONE DIES TO:
//
//  1. THE ROLE IS THE ROLE AT THE EPOCH OF SEND. One sender, three epochs, three different roles
//     on three of its own lines -- and two receivers that reach those records by two different
//     roads, one with an epoch install between each record and the next, one opening all three
//     under PRIOR epochs' schedules. Read the LIVE epoch instead of the record's and both
//     receivers answer the sender's newest role for every line; read it BEFORE OpenRecord and the
//     leaf is a plaintext claim rather than a signature.
//  2. NON-EMPTY IF AND ONLY IF THE RECORD OPENED, with the two ways a record does not open
//     covered apart, because they are per-DEVICE and not per-group: an epoch aged past
//     PastEpochWindow (the arithmetic refusal, measured AT THE EDGE -- 32 behind opens and 33
//     behind does not) and an epoch beneath a joiner's own Welcome (the store's own not-found).
//     R4 introduces no new disappearance: the records whose role is underivable are a SUBSET of
//     the records that do not open, and the case prints both sets.
//  3. AN OBSERVER'S MESSAGE IS KEPT, WITH ITS CONTENT, AND COUNTED ONCE. Not dropped, not a gap,
//     not an eighth GapReason (ruling 16). Turn the hide into a drop and the log is short.
//  4. AN OBSERVER MAY NOT SEND, ON ALL FOUR VERBS, AND THE OWNER MAY, IN THE SAME RUN -- with the
//     clause where ruling 19 and the ordering argument put it: AFTER identityInUse and AFTER the
//     reconciled check, which the two ordering arms hold by making an observer that is ALSO a
//     second writer say the second-writer sentence.
package urmessage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
)

// ── what a role world needs beyond commits: a line, and a member admitted later ──────────────

// say seals one TEXT record through the sender's OWN SESSION and never through [Group.Send].
//
// IT IS THE HOSTILE BUILD, AND IT IS THE ONLY WAY TO MEASURE THE RECEIVING HALF OF R4. Spec C
// §5.6's whole premise is that OBSERVER is enforced in the client and by the MLS proposal rules
// and NOT by the server: an observer holds the group keys and a modified client can encrypt a
// valid application message. [Group.Send] is the honest client and refuses one after R4, so a
// case that drove the refusal could never produce a record for the receiving side to hide. This
// seals exactly what [Group.sendContentLocked] seals -- the same content envelope, the same
// class, the same head -- with no clause in front of it.
func (self *roleWorld) say(sender *roleMember, text string) *sealed {
	self.t.Helper()
	plaintext, err := encodeText(text)
	if err != nil {
		self.t.Fatalf("%s encoding %q: %v", sender.name, text, err)
	}
	record, err := sender.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), plaintext, 0, nil)
	if err != nil {
		self.t.Fatalf("%s sealing %q at epoch %d: %v", sender.name, text, sender.handle.Epoch(), err)
	}
	one := &sealed{recordId: self.nextRecordId, record: record}
	self.nextRecordId += 1
	return one
}

// addAndEnroll has committer add ONE brand new device in one CommitAdd, publishes the commit
// record, and stands the joiner up as a [roleMember] at the epoch the add opened.
//
// THE JOINER'S STORE HOLDS NO EPOCH BELOW ITS WELCOME, which is the whole reason this helper
// exists beside [roleWorld.commitAndPublish]: "obtainable" is a per-DEVICE property, so the second
// way a record fails to open -- LoadGroup answering "this state store holds no such value" -- can
// only be reached by a member that was not there.
func (self *roleWorld) addAndEnroll(committer *roleMember, name string) (*roleMember, *sealed) {
	self.t.Helper()
	dev := self.device(name)
	keyPackage, err := dev.engine.NewKeyPackage()
	if err != nil {
		self.t.Fatalf("%s's key package: %v", name, err)
	}
	commit, welcome, ratchetTree, err := committer.handle.CommitAdd([][]byte{keyPackage})
	if err != nil {
		self.t.Fatalf("%s's CommitAdd of %s: %v", committer.name, name, err)
	}
	if err := committer.handle.MergePendingCommit(); err != nil {
		self.t.Fatalf("%s's MergePendingCommit: %v", committer.name, err)
	}
	record := self.publish(committer, commit)
	handle, err := dev.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		self.t.Fatalf("%s's JoinFromWelcome: %v", name, err)
	}
	self.t.Cleanup(func() { handle.Close() })
	return self.enroll(name, dev, handle), record
}

// setRoleAndPublish is one policy commit by committer that gives target's identity one role.
func (self *roleWorld) setRoleAndPublish(committer *roleMember, target *roleMember, role mls.Role) *sealed {
	self.t.Helper()
	policy := self.policyOf(committer)
	policy.SetRole(target.dev.identityPub, role)
	return self.commitAndPublish(committer, fmt.Sprintf("CommitPolicy setting %s to %s", target.name, role),
		func() ([]byte, []byte, []byte, error) {
			return committer.handle.CommitPolicy(self.policyBody(policy))
		})
}

// bump is one PATH-ONLY commit: no membership change, no policy change. Ruling 12 -- every role
// may make one, because RFC 9420 §12.4 forbids a committer carrying its own Update, so a commit
// whose path refreshes the leaf is a member's only PCS self-heal. Here it is the cheapest legal
// way to age a group past its past-epoch window.
func (self *roleWorld) bump(committer *roleMember) *sealed {
	self.t.Helper()
	return self.commitAndPublish(committer, "a path-only Commit(nil)", func() ([]byte, []byte, []byte, error) {
		return committer.handle.Commit(nil)
	})
}

// roleOfLine is the SenderRoleAtSend on the one message in receiver's log whose text is `text`,
// and whether the log holds it at all.
func roleOfLine(receiver *roleMember, text string) (*Message, bool) {
	for _, held := range receiver.group.Messages() {
		if held.Text == text {
			return held, true
		}
	}
	return nil, false
}

// ── 1. the role is the role at the epoch of send ─────────────────────────────────────────────

// ONE SENDER, THREE EPOCHS, THREE ROLES, AND TWO RECEIVERS THAT REACH THEM BY DIFFERENT ROADS.
//
// carol is an unnamed member at epoch 1, an OBSERVER at epoch 2 and an ADMIN at epoch 3, and she
// writes one line at each. What every receiver must end up holding is member / observer / admin,
// in that order, FOR EVER -- the promotion at epoch 3 does not reach back and relabel the line she
// wrote as an observer, and the demotion at epoch 2 does not reach back and relabel the line she
// wrote as a member.
//
// THE TWO ROADS ARE THE POINT, because they are the two orderings a real walk produces:
//
//   - bob takes ONE PAGE holding the records and the commits interleaved in server order. Each
//     line opens at the session's OWN epoch, and between one line and the next an epoch INSTALL
//     runs inside the same loop -- which is the hazard connect measured: the install closes every
//     held past handle and re-makes the schedule map. A capture deferred past its own loop
//     iteration would be asking a question this session may by then have to answer with a fresh
//     load.
//   - erin takes the COMMITS FIRST and the lines afterwards, so all three lines open under PRIOR
//     epochs' schedules at a session standing at epoch 3. This is the road on which reading the
//     LIVE epoch instead of the record's answers "admin" three times.
//
// The two roads must agree, field for field, and that agreement is asserted rather than assumed.
//
// WHAT WOULD GO RED: read the role at self.epoch rather than header.Epoch (erin reads admin three
// times; bob still reads correctly, which is exactly why erin is here). Capture the role before
// OpenRecord (the leaf is then a plaintext claim -- see the sentinel case below for what that
// costs). Derive the role at render time instead of at the open (carol's epoch-2 line becomes
// "admin" the moment she is promoted).
func TestTheRoleOnALineIsTheRoleAtTheEpochItWasSealedAtAtEveryReceiver(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol", "erin")
	alice, bob, carol, erin := world.member("alice"), world.member("bob"), world.member("carol"), world.member("erin")

	// epoch 1: carol is UNNAMED, which every reading takes as MEMBER (ruling 8/20)
	atOne := world.say(carol, "carol at epoch one")

	// epoch 1 -> 2: the owner demotes carol to OBSERVER
	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(carol, alice, demotion)
	atTwo := world.say(carol, "carol at epoch two")

	// epoch 2 -> 3: the owner promotes carol to ADMIN
	promotion := world.setRoleAndPublish(alice, carol, mls.RoleAdmin)
	world.ingest(carol, alice, promotion)
	atThree := world.say(carol, "carol at epoch three")

	want := []struct {
		text string
		role string
	}{
		{"carol at epoch one", "member"},
		{"carol at epoch two", "observer"},
		{"carol at epoch three", "admin"},
	}

	// ROAD ONE: one page, server order, an epoch install between each line and the next.
	if err := world.deliver(bob, atOne, demotion, atTwo, promotion, atThree); err != nil {
		t.Fatalf("bob's walk over the interleaved page: %v", err)
	}
	if bob.group.Epoch() != 3 {
		t.Fatalf("bob is at epoch %d after the interleaved page, want 3", bob.group.Epoch())
	}
	// ROAD TWO: the commits first, then every line as a PRIOR epoch's record.
	if err := world.deliver(erin, demotion, promotion); err != nil {
		t.Fatalf("erin's walk over the commits: %v", err)
	}
	if erin.group.Epoch() != 3 {
		t.Fatalf("erin is at epoch %d after the commits, want 3", erin.group.Epoch())
	}
	if err := world.deliver(erin, atOne, atTwo, atThree); err != nil {
		t.Fatalf("erin's walk over the lines: %v", err)
	}
	if got := erin.group.Stats().OpenedPastEpoch; got != 2 {
		t.Errorf("erin opened %d record(s) under a prior epoch's schedule, want 2 (epochs one and two); "+
			"this case is not measuring the prior-epoch road", got)
	}

	for _, receiver := range []*roleMember{bob, erin} {
		for _, one := range want {
			held, found := roleOfLine(receiver, one.text)
			if !found {
				t.Fatalf("%s's log does not hold %q at all; an observer's message is HIDDEN and never dropped",
					receiver.name, one.text)
			}
			if held.SenderRoleAtSend != one.role {
				t.Errorf("%s reads %q as sent by a %q, want %q", receiver.name, one.text,
					held.SenderRoleAtSend, one.role)
			}
			if held.Gap != "" {
				t.Errorf("%s reads %q as a %s gap; an observer's message is not a gap", receiver.name, one.text, held.Gap)
			}
		}
		stats := receiver.group.Stats()
		if stats.HiddenObserver != 1 {
			t.Errorf("%s's Stats.HiddenObserver is %d, want 1 (carol's epoch-two line and nothing else)",
				receiver.name, stats.HiddenObserver)
		}
		if stats.RoleUndeterminable != 0 {
			t.Errorf("%s opened %d record(s) whose sender's role it could not read", receiver.name, stats.RoleUndeterminable)
		}
	}

	// THE TWO ROADS AGREE, which an epoch counter cannot say and which a per-road bug would break.
	for _, one := range want {
		fromBob, _ := roleOfLine(bob, one.text)
		fromErin, _ := roleOfLine(erin, one.text)
		if fromBob.SenderRoleAtSend != fromErin.SenderRoleAtSend {
			t.Errorf("bob reads %q as %q and erin reads it as %q; two roads, two answers",
				one.text, fromBob.SenderRoleAtSend, fromErin.SenderRoleAtSend)
		}
	}

	// AND THE PROMOTION DOES NOT REACH BACK. carol is an ADMIN now, at every receiver, and the
	// line she wrote while she was an observer still says observer.
	if role, _ := world.policyOf(bob).RoleOf(carol.dev.identityPub); role != mls.RoleAdmin {
		t.Fatalf("bob reads carol as %s, so the promotion this case rests on did not land", role)
	}
	hidden, _ := roleOfLine(bob, "carol at epoch two")
	if hidden.SenderRoleAtSend != "observer" {
		t.Errorf("after carol's promotion her epoch-two line reads %q; a role is a fact about an epoch",
			hidden.SenderRoleAtSend)
	}

	// THE COUNTER MOVES ONCE PER RECORD AND NOT ONCE PER WALK. The same page again is the ordinary
	// shape of a rewind over an earlier failure.
	before := bob.group.Stats()
	if err := world.deliver(bob, atOne, atTwo, atThree); err != nil {
		t.Fatalf("bob's re-walk: %v", err)
	}
	after := bob.group.Stats()
	if after.HiddenObserver != before.HiddenObserver {
		t.Errorf("a second delivery of the same records moved Stats.HiddenObserver %d -> %d",
			before.HiddenObserver, after.HiddenObserver)
	}
	if after.SkippedSeen != before.SkippedSeen+3 {
		t.Errorf("the re-walk skipped %d already-held record(s), want 3", after.SkippedSeen-before.SkippedSeen)
	}
	t.Logf("carol: member at 1, observer at 2, admin at 3; bob (interleaved) and erin (prior-epoch) "+
		"agree on all three, hidden_observer=%d role_undeterminable=%d at each",
		after.HiddenObserver, after.RoleUndeterminable)
}

// ── 2. non-empty if and only if the record opened ────────────────────────────────────────────

// THE ONE THE RULINGS REST ON. For every record in a re-walked log, [Message.SenderRoleAtSend] is
// non-empty IF AND ONLY IF the record opened -- and the only records that do not open are the ones
// whose epoch no schedule on the reading device reaches, which is [GapOutOfWindow] and nothing
// else. So R4 introduces NO NEW DISAPPEARANCE: the set of records whose role is underivable is a
// subset of the set that do not open, and this case prints both sets rather than asserting into
// the air.
//
// "OBTAINABLE" IS A PER-DEVICE PROPERTY AND THE TWO WAYS TO MISS ARE COVERED APART:
//
//   - BENEATH A JOINER'S OWN WELCOME. dave is added at epoch 3; the epoch-2 records are INSIDE
//     the window by arithmetic and his store answers "this state store holds no such value" for
//     the epoch itself. Measured at epoch 3, where the arithmetic refusal cannot fire.
//   - BELOW THE WINDOW, AT THE EDGE. The group is then aged to epoch 35 with path-only commits,
//     and bob -- who was there for all of it and has fetched no line -- walks the whole history.
//     35 - 3 == 32 is NOT more than PastEpochWindow, so the epoch-3 records OPEN; 35 - 2 == 33 is,
//     so the epoch-2 records do not. mls's own DeleteGroupStateBefore cuts at epoch - 32, the same
//     line, so the two halves agree by construction rather than by luck.
//
// WHAT WOULD GO RED: fill the role on an out_of_window gap from the CURRENT policy (the "only if"
// half fails, and the value would be a role read off an unauthenticated sender_handle at an epoch
// nobody can read); leave the role empty on a record that opened under a prior epoch's schedule
// (the "if" half fails); move Stats.RoleUndeterminable on a gap.
func TestARoleIsCarriedByExactlyTheRecordsThatOpenedAtBothEdgesOfObtainability(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// THE SENDERS ARE ALICE AND CAROL AND THE READERS ARE BOB AND DAVE, and they are kept apart
	// deliberately: [roleWorld.say] seals through the session and not through [Group.Send], so the
	// sender's own [Group] never learns the stream index it spent, and a walk that met one of its
	// own records that way would read it as a copy of the folder ([ErrIdentityInUse]) rather than
	// as history. That is the right answer to a record a device did not know it had written, and
	// it is not what this case is about.
	//
	// epoch 1 -> 2: carol becomes an OBSERVER, so the log carries a hidden line at every epoch.
	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(bob, alice, demotion)
	world.ingest(carol, alice, demotion)

	// the epoch-2 lines
	atTwo := []*sealed{
		world.say(alice, "alice at epoch two"),
		world.say(carol, "carol the observer at epoch two"),
	}

	// epoch 2 -> 3: dave is admitted. His store holds nothing below this.
	dave, admission := world.addAndEnroll(alice, "dave")
	world.ingest(bob, alice, admission)
	world.ingest(carol, alice, admission)
	atThree := []*sealed{
		world.say(alice, "alice at epoch three"),
		world.say(carol, "carol the observer at epoch three"),
	}

	// ── EDGE ONE: BENEATH THE JOINER'S OWN WELCOME, measured while the window is not in play ──
	if err := world.deliver(dave, append(append([]*sealed{}, atTwo...), atThree...)...); err != nil {
		t.Fatalf("dave's walk: %v", err)
	}
	assertRoleIffOpened(t, "dave, admitted at epoch three", dave)
	if got := dave.group.Stats().GapOutOfWindow; got != 2 {
		t.Fatalf("dave gapped %d pre-admission record(s), want 2; the below-admission arm is not exercised", got)
	}
	if got := dave.group.Epoch() - 2; got > messagegroup.PastEpochWindow {
		t.Fatalf("dave read the epoch-two records %d behind, which the WINDOW already refuses; "+
			"the below-admission arm would be indistinguishable from the below-window one", got)
	}

	// ── AGE THE GROUP TO EPOCH 35, WHICH PUTS EPOCH TWO ONE STEP PAST THE WINDOW AND LEAVES
	//    EPOCH THREE EXACTLY ON IT ──────────────────────────────────────────────────────────
	const aged = uint64(35)
	bumps := []*sealed{}
	for alice.group.Epoch() < aged {
		bumps = append(bumps, world.bump(alice))
	}
	if err := world.deliver(bob, bumps...); err != nil {
		t.Fatalf("bob's walk over %d path-only commits: %v", len(bumps), err)
	}
	if bob.group.Epoch() != aged {
		t.Fatalf("bob is at epoch %d after the ageing, want %d", bob.group.Epoch(), aged)
	}
	if aged-3 != messagegroup.PastEpochWindow || aged-2 != messagegroup.PastEpochWindow+1 {
		t.Fatalf("the ageing does not land on the window edge: epoch three is %d behind and epoch two is %d, "+
			"and PastEpochWindow is %d", aged-3, aged-2, messagegroup.PastEpochWindow)
	}
	topLine := world.say(alice, "alice at the top epoch")

	// ── EDGE TWO: BELOW THE WINDOW, AT THE EDGE, for a member that was there the whole time ──
	page := append(append([]*sealed{}, atTwo...), atThree...)
	page = append(page, topLine)
	if err := world.deliver(bob, page...); err != nil {
		t.Fatalf("bob's walk over the whole history at epoch %d: %v", aged, err)
	}
	assertRoleIffOpened(t, fmt.Sprintf("bob, a member since epoch one, reading at epoch %d", aged), bob)
	if got := bob.group.Stats().GapOutOfWindow; got != 2 {
		t.Errorf("bob gapped %d record(s), want 2 (the two epoch-two lines, one step past the window)", got)
	}
	// AND THE EDGE ITSELF: epoch three, exactly PastEpochWindow behind, still opens and still
	// carries its role. Without this the case would pass with the whole history gapped.
	edge, found := roleOfLine(bob, "carol the observer at epoch three")
	if !found || edge.Gap != "" {
		t.Fatalf("bob does not hold the epoch-three line as a message (found=%v); a record exactly "+
			"PastEpochWindow behind is inside the window", found)
	}
	if edge.SenderRoleAtSend != "observer" {
		t.Errorf("the epoch-three line at the window edge reads %q, want observer", edge.SenderRoleAtSend)
	}
}

// assertRoleIffOpened is the property, with BOTH SETS PRINTED. A narrowing whose complement is
// empty is a narrowing that measured nothing: a log with no gaps would satisfy "every gap has no
// role" vacuously, and a log with no messages would satisfy "every message has a role" the same
// way, so both are required to be non-empty and both are named in the log line.
func assertRoleIffOpened(t *testing.T, who string, receiver *roleMember) {
	t.Helper()
	withRole, withoutRole := []string{}, []string{}
	for _, held := range receiver.group.Messages() {
		name := fmt.Sprintf("record %d %s", held.RecordId, held.Gap)
		if held.SenderRoleAtSend != "" {
			withRole = append(withRole, name+" "+held.SenderRoleAtSend)
		} else {
			withoutRole = append(withoutRole, name)
		}
		opened := held.Gap != GapOutOfWindow
		if opened != (held.SenderRoleAtSend != "") {
			t.Errorf("%s: record %d has gap %q and role %q; a role is carried by exactly the records that opened",
				who, held.RecordId, held.Gap, held.SenderRoleAtSend)
		}
	}
	if len(withRole) == 0 || len(withoutRole) == 0 {
		t.Fatalf("%s: %d record(s) carry a role and %d do not; one of the two sets is empty, so this "+
			"property is vacuous here", who, len(withRole), len(withoutRole))
	}
	if got := receiver.group.Stats().RoleUndeterminable; got != 0 {
		t.Errorf("%s: Stats.RoleUndeterminable is %d; a record that opened must not lose its sender's role", who, got)
	}
	t.Logf("%s: WITH a role %v || WITHOUT %v", who, withRole, withoutRole)
}

// ── 3. the hide is a hide, and the content is intact ─────────────────────────────────────────

// AN OBSERVER'S MESSAGE IS KEPT IN THE LOG WITH ITS CONTENT INTACT (ruling 16).
//
// IT IS NOT A GAP AND NOT AN EIGHTH GapReason. The set is closed at seven, and a gap means "this
// build could not SHOW it" -- this is a record this build read perfectly well and has been asked
// to COLLAPSE. Dropping it would be indistinguishable from a record that never arrived, which is
// the one thing this package's whole gap design exists to prevent.
//
// AND THE LINE KEEPS EVERY OTHER FACT ABOUT IT: its position in the log, its message_id, its
// sender_handle, its clock reading and its text -- so a UI can expand it, and a later reply can
// still quote it.
//
// WHAT WOULD GO RED: drop the record in deliverLocked; turn it into a gap; blank its Text; count
// it twice.
func TestAnObserversMessageIsHiddenAndNotDroppedAndKeepsItsContent(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(bob, alice, demotion)
	world.ingest(carol, alice, demotion)

	const hidden = "an observer that sent anyway"
	line := world.say(carol, hidden)
	fromOwner := world.say(alice, "and one line from the owner")
	if err := world.deliver(bob, line, fromOwner); err != nil {
		t.Fatalf("bob's walk: %v", err)
	}

	log := bob.group.Messages()
	if len(log) != 2 {
		t.Fatalf("bob's log holds %d entries, want 2; an observer's message is hidden and never dropped", len(log))
	}
	held := log[0]
	if held.Text != hidden {
		t.Errorf("the hidden line's text is %q, want %q; the content is kept intact", held.Text, hidden)
	}
	if held.Gap != "" {
		t.Errorf("the hidden line is a %q gap; hiding is not gapping", held.Gap)
	}
	if held.SenderRoleAtSend != "observer" {
		t.Errorf("the hidden line reads %q", held.SenderRoleAtSend)
	}
	if held.Kind != KindText || len(held.MessageId) != MessageIdBytes || held.SentAtMs == 0 {
		t.Errorf("the hidden line lost a field every other message keeps: kind=%v message_id=%d sent_at_ms=%d",
			held.Kind, len(held.MessageId), held.SentAtMs)
	}
	// it can still be quoted, which is what "it is an entry" means
	if _, found := bob.group.heldLocked(held.MessageId); !found {
		t.Error("the hidden line is not addressable by its message_id, so nothing can reply to it")
	}
	if log[1].SenderRoleAtSend != "owner" {
		t.Errorf("the owner's own line beside it reads %q, want owner", log[1].SenderRoleAtSend)
	}
	stats := bob.group.Stats()
	if stats.HiddenObserver != 1 {
		t.Errorf("Stats.HiddenObserver is %d over one observer line beside one owner line, want 1", stats.HiddenObserver)
	}
	if stats.Opened != 2 {
		t.Errorf("Stats.Opened is %d, want 2: a hidden record is an OPENED record", stats.Opened)
	}
}

// ── 4. the send refusal, all four verbs, and where the clause sits ───────────────────────────

// AN OBSERVER MAY NOT SEND AND THE OWNER MAY, IN THE SAME RUN, FOR ALL FOUR SENDABLE KINDS.
//
// TEXT, REPLY, REACTION_ADD/REMOVE and TOMBSTONE is the whole askable set (ruling 19), and every
// one of them funnels through [Group.sendableLocked], so the refusal is one clause and one
// sentence. The four verbs are driven through their PUBLIC entry points and not through the clause
// directly: a verb that forgot to call sendableLocked would pass a test of the clause.
//
// THE OWNER'S ARM IS IN THE SAME RUN AND IS NOT OPTIONAL. Without it a clause that refused
// everybody would pass every assertion about the observer. The owner gets past sendableLocked and
// stops at the connection, which is a DIFFERENT error and never this one.
//
// WHAT WOULD GO RED: drop the clause (the observer sends); refuse by role in one verb rather than
// in sendableLocked (the other three still send); refuse everybody (the owner's arm fails).
func TestAnObserverMayNotSendOnAnyVerbAndTheOwnerMayInTheSameRun(t *testing.T) {
	world := newRoleWorld(t, "alice", "carol")
	alice, carol := world.member("alice"), world.member("carol")
	ctx := context.Background()

	// A TARGET IN EACH ONE'S OWN LOG, so React and Delete have something they may name: the role
	// refusal under test must be taken BEFORE the target is looked at, and a case whose target did
	// not exist could not tell the two refusals apart. Each device reads the OTHER's line, never
	// its own -- [roleWorld.say] seals through the session, so a device meeting a record it does
	// not know it wrote reads it as a copy of the folder, which is a different refusal entirely.
	fromAlice := world.say(alice, "a line from alice to react to")
	fromCarol := world.say(carol, "a line from carol to react to")
	if err := world.deliver(carol, fromAlice); err != nil {
		t.Fatalf("carol's walk: %v", err)
	}
	if err := world.deliver(alice, fromCarol); err != nil {
		t.Fatalf("alice's walk: %v", err)
	}
	carolTarget := carol.group.Messages()[0].MessageId

	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(carol, alice, demotion)
	if role, err := carol.group.MyRole(); err != nil || role != "observer" {
		t.Fatalf("carol's MyRole after the demotion is %q, %v; want observer", role, err)
	}
	if role, err := alice.group.MyRole(); err != nil || role != "owner" {
		t.Fatalf("alice's MyRole is %q, %v; want owner", role, err)
	}

	verbs := []struct {
		name string
		kind ContentKind
		run  func(group *Group, target []byte) error
	}{
		{"Send", KindText, func(group *Group, _ []byte) error { _, err := group.Send(ctx, "a line"); return err }},
		{"SendReply", KindReply, func(group *Group, target []byte) error {
			_, err := group.SendReply(ctx, target, "an answer")
			return err
		}},
		{"React", KindReactionAdd, func(group *Group, target []byte) error {
			_, err := group.React(ctx, target, "x")
			return err
		}},
		{"Unreact", KindReactionRemove, func(group *Group, target []byte) error {
			_, err := group.Unreact(ctx, target, "x")
			return err
		}},
		{"Delete", KindTombstone, func(group *Group, target []byte) error { _, err := group.Delete(ctx, target); return err }},
	}
	for _, verb := range verbs {
		// THE OBSERVER GOES THROUGH THE PUBLIC VERB, which is what makes this a test of the
		// PRODUCT and not of one clause: a verb that forgot to call sendableLocked would pass any
		// test of the clause alone.
		err := verb.run(carol.group, carolTarget)
		if !errors.Is(err, ErrObserverMayNotSend) {
			t.Errorf("an observer's %s answered %v, want ErrObserverMayNotSend", verb.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "you can read this group but not send to it") {
			t.Errorf("an observer's %s does not say spec C's sentence: %v", verb.name, err)
		}
		// THE OWNER, IN THE SAME RUN, AT THE CLAUSE -- because this world has no transport and an
		// allowed send would walk straight into one. Without this arm a clause that refused
		// EVERYBODY would satisfy every assertion above. cp3b's case is where the owner's send
		// goes all the way to a server.
		role, err := alice.group.sendableLocked(verb.kind)
		if err != nil {
			t.Errorf("the OWNER's %s was refused at the clause: %v", verb.name, err)
		}
		if role != "owner" {
			t.Errorf("the owner's %s would be stamped %q", verb.name, role)
		}
		// and the observer at the same clause, for the same kind: one clause, all four kinds
		if role, err := carol.group.sendableLocked(verb.kind); !errors.Is(err, ErrObserverMayNotSend) || role != "" {
			t.Errorf("the observer's clause for %s answered (%q, %v)", verb.kind, role, err)
		}
	}
	// THE EXCEPTION LIST IS EMPTY, WHICH IS RULING 19 AND IS THE REASON THE CLAUSE READS A LIST AT
	// ALL. A later per-kind exception is an entry here and nothing else; a list that had silently
	// grown an entry would make one of the four verbs above pass for the wrong reason.
	if len(kindsAnObserverMaySend) != 0 {
		t.Errorf("kindsAnObserverMaySend holds %d exception(s): %v; ruling 19 refuses all four "+
			"sendable kinds and an entry here is a ruling, not an edit", len(kindsAnObserverMaySend),
			kindsAnObserverMaySend)
	}
	if got := carol.group.Stats().Submitted; got != 0 {
		t.Errorf("a refused send submitted %d record(s)", got)
	}
}

// THE SIXTH CLAUSE IS AFTER THE TWO STICKY ONES, AND THAT ORDER IS THE ASSERTION.
//
// A group that has seen a SECOND WRITER under this device's identity, and a restored group that
// has NOT RECONCILED, must each say THAT and not report a fact about the caller's role: the first
// two are about a key this device is about to reuse -- the irreversible half -- and a permission
// refusal in front of them would send a user looking for an admin instead of for their other
// device. Both arms are set up on a group that is ALSO an observer's, so the only thing deciding
// which sentence comes back is the ORDER of the clauses.
//
// WHAT WOULD GO RED: move the role clause above identityInUse or above the reconciled check --
// which is exactly the mutation this case exists for.
func TestTheObserverClauseComesAfterTheSecondWriterAndTheReconcileRefusals(t *testing.T) {
	world := newRoleWorld(t, "alice", "carol")
	alice, carol := world.member("alice"), world.member("carol")

	demotion := world.setRoleAndPublish(alice, carol, mls.RoleObserver)
	world.ingest(carol, alice, demotion)
	if _, err := carol.group.sendableLocked(KindText); !errors.Is(err, ErrObserverMayNotSend) {
		t.Fatalf("carol is not refused as an observer to begin with (%v), so neither arm below is measuring an order", err)
	}

	// ARM ONE: a second writer. The group is an observer's AND has seen a copy of itself.
	carol.group.identityInUse = fmt.Errorf("%w: group %x", ErrIdentityInUse, carol.group.id)
	_, err := carol.group.sendableLocked(KindText)
	if !errors.Is(err, ErrIdentityInUse) {
		t.Errorf("an observer whose identity is in use answered %v, want the second-writer refusal", err)
	}
	if errors.Is(err, ErrObserverMayNotSend) {
		t.Errorf("the role clause was taken ahead of the second-writer refusal: %v", err)
	}
	carol.group.identityInUse = nil

	// ARM TWO: a restored group that has not reconciled.
	carol.group.reconciled = false
	_, err = carol.group.sendableLocked(KindText)
	if !errors.Is(err, ErrNotReconciled) {
		t.Errorf("an observer that has not reconciled answered %v, want the reconcile refusal", err)
	}
	if errors.Is(err, ErrObserverMayNotSend) {
		t.Errorf("the role clause was taken ahead of the reconcile refusal: %v", err)
	}
	carol.group.reconciled = true
}

// ── the residual: a role that cannot be read ─────────────────────────────────────────────────

// A ROLE THAT CANNOT BE READ IS AN EMPTY ROLE AND A COUNTER, AND NEVER A LOST RECORD.
//
// WHAT IS AND IS NOT REACHABLE, said plainly rather than implied. On the walk this is a state the
// session does not produce: RoleAt goes through the same three-armed schedule lookup OpenRecord
// takes, so a door that answered the record cannot refuse to say who sent it -- with the one
// measured condition connect wrote on the method, that no epoch INSTALL has run between the open
// and the ask, which this package guarantees by asking in the same loop iteration. So the walk-level
// arm of this case is NOT DRIVEN HERE, and saying so is better than building a fake seam to drive
// it through.
//
// WHAT IS DRIVEN IS THE ONE SITE ITSELF, at a leaf nobody stands at -- the seam's own
// ErrEngineMemberLeaf, which is a refusal the session CAN answer at an epoch it holds. The property
// is the whole of what the site owes: an empty role, exactly one count, no error escaping, and a
// message built with an empty role that still reaches the log.
//
// WHAT WOULD GO RED: fail the record on an unreadable role; return a role anyway; forget the
// counter; count it on the out_of_window path (which never asks).
func TestAnUnreadableRoleLeavesAnEmptyRoleAndOneCountAndTheRecordStillArrives(t *testing.T) {
	world := newRoleWorld(t, "alice", "bob")
	bob := world.member("bob")

	const noSuchLeaf = uint32(9)
	before := bob.group.Stats().RoleUndeterminable
	role := bob.group.roleAtSendLocked(bob.group.epoch, noSuchLeaf)
	if role != "" {
		t.Errorf("a leaf nobody stands at answered role %q", role)
	}
	if got := bob.group.Stats().RoleUndeterminable; got != before+1 {
		t.Errorf("Stats.RoleUndeterminable went %d -> %d over one unreadable role, want one more", before, got)
	}
	// the positive control, in the same call shape: a leaf somebody DOES stand at answers a role,
	// and moves nothing. Without it the case above would pass against a site that always refused.
	if role := bob.group.roleAtSendLocked(bob.group.epoch, bob.leaf); role != "member" {
		t.Errorf("bob's own leaf answered role %q, want member; this case's refusal arm is not measuring a refusal", role)
	}
	if got := bob.group.Stats().RoleUndeterminable; got != before+1 {
		t.Errorf("a readable role moved Stats.RoleUndeterminable to %d", got)
	}

	// AND A MESSAGE WITH NO ROLE IS STILL A MESSAGE. deliverLocked keeps it, indexes it and does
	// not count it as hidden: "" is not "observer".
	entry := &Content{Kind: KindText, Text: "a line whose sender's role could not be read"}
	received := newMessage(entry, 77, make([]byte, 16), nil, false, 5, aTarget(0xF4), "")
	hiddenBefore := bob.group.Stats().HiddenObserver
	if !bob.group.deliverLocked(received, entry) {
		t.Fatal("a message with no role was not delivered as a line")
	}
	if got := bob.group.Stats().HiddenObserver; got != hiddenBefore {
		t.Errorf("an empty role was counted as a hidden observer (%d -> %d)", hiddenBefore, got)
	}
	if _, found := bob.group.heldLocked(received.MessageId); !found {
		t.Error("a message with no role is not in the log")
	}
}
