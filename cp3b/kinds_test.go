package cp3b

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// THE CONTENT ENVELOPE, END TO END, THROUGH A REAL SERVER.
//
// urmessage's own suite drives the codec and the walk with sealed records and no server. This is
// the other half: a reply, a reaction, a removal and a tombstone sealed by one device, submitted,
// fetched by another, and landing on the message they NAME -- through §4.3.5's submit, §4.3.4's
// fetch, the size ladder and both AEADs.
//
// WHAT IT CATCHES THAT THE UNIT CASES CANNOT: a message_id that is not the same value on both
// sides. Every kind that names another message quotes an id the SENDER derived and the RECEIVER
// re-derives from the header it fetched, and a derivation that disagreed across the seam would
// leave every effect held for a target that never arrives -- which looks exactly like a network
// that is merely slow.
func TestEveryKindCrossesARealServerAndLandsOnTheMessageItNames(t *testing.T) {
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

	// ── a text, and the id both sides name it by ────────────────────────────────────────────
	line, err := aliceGroup.Send(ctx, "a line worth answering")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if line.Kind != urmessage.KindText {
		t.Errorf("alice's own line came back as kind %s", line.Kind)
	}
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("bob read %d message(s) for one line: %s", len(got), textsOf(got))
	}
	if got[0].Kind != urmessage.KindText || got[0].Text != "a line worth answering" {
		t.Errorf("bob read kind %s, text %q", got[0].Kind, got[0].Text)
	}
	if !bytes.Equal(got[0].MessageId, line.MessageId) {
		t.Fatalf("alice names her line %x and bob names it %x; every kind that quotes an id is broken if these differ",
			line.MessageId, got[0].MessageId)
	}

	// ── a reply, which carries its parent's NAME and not its text ──────────────────────────
	reply, err := bobGroup.SendReply(ctx, line.MessageId, "an answer")
	if err != nil {
		t.Fatalf("bob's SendReply: %v", err)
	}
	if reply.Kind != urmessage.KindReply || !bytes.Equal(reply.ReplyToId, line.MessageId) {
		t.Errorf("bob's own reply came back as kind %s naming %x", reply.Kind, reply.ReplyToId)
	}
	got, err = aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive of the reply: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("alice read %d message(s) for one reply: %s", len(got), textsOf(got))
	}
	if got[0].Kind != urmessage.KindReply {
		t.Errorf("alice read the reply as kind %s", got[0].Kind)
	}
	if got[0].Text != "an answer" {
		t.Errorf("the reply's text came back as %q", got[0].Text)
	}
	if !bytes.Equal(got[0].ReplyToId, line.MessageId) {
		t.Errorf("the reply names %x and its parent is %x", got[0].ReplyToId, line.MessageId)
	}

	// ── a reaction, which is not a line and lands on one ────────────────────────────────────
	if _, err := bobGroup.React(ctx, line.MessageId, "👍"); err != nil {
		t.Fatalf("bob's React: %v", err)
	}
	got, err = aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive of the reaction: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a reaction delivered %d message(s) of its own: %s", len(got), textsOf(got))
	}
	held := messageById(t, aliceGroup, line.MessageId)
	if len(held.Reactions) != 1 || held.Reactions[0].Emoji != "👍" {
		t.Fatalf("alice's own line carries %v after bob reacted", held.Reactions)
	}
	if held.Reactions[0].Mine {
		t.Error("bob's reaction is marked as alice's own")
	}
	// and bob's own view of what he sent
	if standing := messageById(t, bobGroup, line.MessageId).Reactions; len(standing) != 1 || !standing[0].Mine {
		t.Errorf("bob's view of the line he reacted to carries %v, want his own reaction", standing)
	}

	// ── and the removal, which cancels it ───────────────────────────────────────────────────
	if _, err := bobGroup.Unreact(ctx, line.MessageId, "👍"); err != nil {
		t.Fatalf("bob's Unreact: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive of the removal: %v", err)
	}
	if standing := messageById(t, aliceGroup, line.MessageId).Reactions; len(standing) != 0 {
		t.Errorf("the reaction was taken back and alice still shows %v", standing)
	}

	// ── a tombstone, from the line's own sender ─────────────────────────────────────────────
	if _, err := aliceGroup.Delete(ctx, line.MessageId); err != nil {
		t.Fatalf("alice's Delete: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the tombstone: %v", err)
	}
	bobsCopy := messageById(t, bobGroup, line.MessageId)
	if !bobsCopy.Deleted {
		t.Error("alice deleted her own line and bob's copy is not marked deleted")
	}
	if bobsCopy.Text != "a line worth answering" {
		t.Errorf("a tombstone erased the text on the receiving side: %q", bobsCopy.Text)
	}

	// ── the refusals a caller gets before anything is sealed ────────────────────────────────
	//
	// K5/K9: a call naming an id that is not a stored message in this device's own view emits no
	// record. T-b on the send side: a tombstone naming somebody else's message is one every
	// honest receiver would ignore, so it is not sealed.
	stranger := make([]byte, 32)
	if _, err := rand.Read(stranger); err != nil {
		t.Fatalf("drawing an id: %v", err)
	}
	if _, err := aliceGroup.React(ctx, stranger, "👍"); !errors.Is(err, urmessage.ErrNoSuchMessage) {
		t.Errorf("a reaction naming a message nothing holds answered %v, want ErrNoSuchMessage", err)
	}
	if _, err := aliceGroup.Delete(ctx, stranger); !errors.Is(err, urmessage.ErrNoSuchMessage) {
		t.Errorf("a tombstone naming a message nothing holds answered %v, want ErrNoSuchMessage", err)
	}
	if _, err := bobGroup.Delete(ctx, line.MessageId); err == nil {
		t.Error("bob deleted a message alice sealed")
	}
	if _, err := aliceGroup.React(ctx, line.MessageId[:16], "👍"); err == nil {
		t.Error("a reaction naming half a message_id was sealed")
	}
	if _, err := aliceGroup.React(ctx, line.MessageId, ""); err == nil {
		t.Error("a reaction with no emoji was sealed")
	}

	// AND NOTHING WAS SEALED BY ANY OF THOSE, read off the server's own rows rather than off a
	// counter this package keeps: a refusal that still submitted would show up here as records
	// nobody asked for.
	assertNothingFailedToOpen(t, "alice", aliceGroup)
	assertNothingFailedToOpen(t, "bob", bobGroup)
	t.Logf("alice holds %d entries, bob holds %d, over a text, a reply, a reaction, a removal and a tombstone",
		len(aliceGroup.Messages()), len(bobGroup.Messages()))
}

// A RESTARTED DEVICE READS ITS OWN REPLY AND ITS OWN REACTION BACK THROUGH THE SAME CODEC.
//
// THIS IS THE OWN-COPY PATH AND IT IS THE ONE EASIEST TO MISS. Since connect 4c030dc a member
// cannot open its own application record -- Protect spends a generation of its own ratchet and MLS
// keeps no receiving ratchet for it (MG-4) -- so a device's own half of a conversation comes back
// from the copy it kept on disk and from nowhere else. What it kept is now an application
// PLAINTEXT, `kind ‖ body`, and a build that read it as raw text would show a restarted user their
// own reply as a line beginning with an 0x02 and would lose their own reaction entirely.
func TestARestartedDeviceReadsItsOwnRepliesAndReactionsBackAsKinds(t *testing.T) {
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

	line, err := aliceGroup.Send(ctx, "a line bob answers and reacts to")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	reply, err := bobGroup.SendReply(ctx, line.MessageId, "bob's own answer, before the restart")
	if err != nil {
		t.Fatalf("bob's SendReply: %v", err)
	}
	if _, err := bobGroup.React(ctx, line.MessageId, "🎯"); err != nil {
		t.Fatalf("bob's React: %v", err)
	}

	// ── the restart: everything in memory is dropped and two directories cross ──────────────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("bob was in one group and %d came back", len(restored))
	}
	bobGroup = restored[0]
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}

	// alice's line and bob's own reply are entries; bob's own reaction is not, and lands on the
	// line instead
	if len(got) != 2 {
		t.Fatalf("the restarted device read %d entries over a line, a reply and a reaction: %s",
			len(got), textsOf(got))
	}
	ownReply := messageById(t, bobGroup, reply.MessageId)
	if ownReply.Kind != urmessage.KindReply {
		t.Errorf("bob's own reply came back as kind %s", ownReply.Kind)
	}
	if ownReply.Text != "bob's own answer, before the restart" {
		t.Errorf("bob's own reply came back as %q", ownReply.Text)
	}
	if !bytes.Equal(ownReply.ReplyToId, line.MessageId) {
		t.Errorf("bob's own reply came back naming %x, want %x", ownReply.ReplyToId, line.MessageId)
	}
	if !ownReply.Mine {
		t.Error("bob's own reply came back as somebody else's")
	}
	standing := messageById(t, bobGroup, line.MessageId).Reactions
	if len(standing) != 1 || standing[0].Emoji != "🎯" || !standing[0].Mine {
		t.Errorf("bob's own reaction came back as %v", standing)
	}
	assertNothingFailedToOpen(t, "the restarted bob", bobGroup)
}

// messageById is one group's view of one message, by the id every member names it with.
func messageById(t *testing.T, group *urmessage.Group, messageId []byte) *urmessage.Message {
	t.Helper()
	for _, one := range group.Messages() {
		if bytes.Equal(one.MessageId, messageId) {
			return one
		}
	}
	t.Fatalf("this group holds no message under %x; it holds %d", messageId, len(group.Messages()))
	return nil
}
