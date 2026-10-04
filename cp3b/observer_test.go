package cp3b

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// OBSERVER READ-ONLY OVER A RUNNING SERVER: item 242's R4, through the public verbs and nothing
// below them.
//
// READ-ONLY MEANS BOTH WORDS, and this case measures both. An observer's [urmessage.Group.Send] is
// refused before anything is sealed -- no stream index, no MLS generation, no submit, and the
// server's own row count does not move -- and the SAME group goes on receiving everything the
// group says. A build that took "may not send" as "is not in the group" would pass the first half
// and fail the second, and it would be the worse failure: a member who can neither speak nor
// listen has been removed, not demoted.
//
// AND THE ROLE IS ON THE LINES BOTH WAYS. The owner's lines carry "owner" at the observer, and
// the observer's own earlier lines -- sent while it was still a member -- go on carrying "member"
// after the demotion, which is ruling 21 over a real server rather than over a test seam.
//
// WHAT IS NOT HERE, said rather than implied: an observer that SENDS ANYWAY. Spec C §5.6's premise
// is that a modified client can, because an observer holds the group keys and the server enforces
// nothing -- but every send door in this module runs the refusal above, so no device here can
// produce such a record. That half is held in sdk/urmessage, where a member seals through the seam
// with no verb in front of it and every honest receiver hides the result
// (TestAnObserversMessageIsHiddenAndNotDroppedAndKeepsItsContent).
//
// WHAT WOULD GO RED: drop the send clause (carol's Send lands and the server moves); refuse by
// role somewhere other than sendableLocked (one of the four verbs still sends); read the role off
// the current roster (carol's pre-demotion line reads "observer" afterwards).
func TestAnObserverMayNotSendOverTheServerAndGoesOnReading(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	carol := world.newPersona(t, "carol")
	for _, who := range []*persona{alice, carol} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}

	groupId := newGroupId(t)
	aliceGroup, carolGroup := openPair(t, ctx, alice, carol, groupId)
	carolId := rolesIdentityOf(t, carolGroup)

	// ── epoch 1: carol is an ordinary member and says so ────────────────────────────────────────
	const beforeTheDemotion = "carol while she was still a member"
	sent, err := carolGroup.Send(ctx, beforeTheDemotion)
	if err != nil {
		t.Fatalf("carol's Send as a member: %v", err)
	}
	if sent.SenderRoleAtSend != "member" {
		t.Fatalf("carol's own line before the demotion is stamped %q, want member", sent.SenderRoleAtSend)
	}

	// ── epoch 2: the owner makes carol an OBSERVER, WITHOUT HAVING FETCHED HER LINE ─────────────
	//
	// THE ORDER HERE IS THE MEASUREMENT AND NOT A CONVENIENCE. Alice has not fetched, so when she
	// does, carol's epoch-one line opens under a PRIOR epoch's schedule at a session standing at
	// epoch two -- and carol's role at epoch two is "observer" while her role at epoch one was
	// "member". A build that read the role at the session's LIVE epoch answers "observer" here
	// and is indistinguishable from a correct one in every other step of this module, because
	// every other step fetches at the epoch it sent in.
	if err := aliceGroup.SetRole(ctx, carolId, "observer"); err != nil {
		t.Fatalf("alice's SetRole demoting carol: %v", err)
	}
	got := gcReceiveTextMessage(t, ctx, "alice", aliceGroup, beforeTheDemotion)
	if got.SenderRoleAtSend != "member" {
		t.Fatalf("alice, standing at epoch %d, reads carol's EPOCH ONE line as sent by a %q, want member; "+
			"the role belongs to the epoch the record was sealed at", aliceGroup.Epoch(), got.SenderRoleAtSend)
	}
	if got := aliceGroup.Stats().OpenedPastEpoch; got != 1 {
		t.Fatalf("alice opened %d record(s) under a prior epoch's schedule, want 1; this step is not "+
			"measuring the prior-epoch road", got)
	}
	if _, err := carolGroup.Receive(ctx); err != nil {
		t.Fatalf("carol's Receive over her own demotion: %v", err)
	}
	if carolGroup.Epoch() != 2 || aliceGroup.Epoch() != 2 {
		t.Fatalf("carol is at epoch %d and alice at %d after the demotion, want 2 and 2",
			carolGroup.Epoch(), aliceGroup.Epoch())
	}
	if role, err := carolGroup.MyRole(); err != nil || role != "observer" {
		t.Fatalf("carol's MyRole after the demotion is %q, %v; want observer", role, err)
	}

	// ── THE REFUSAL, ON ALL FOUR SENDABLE KINDS, AND NOTHING REACHES THE SERVER ─────────────────
	// THE SERVER'S OWN ROW COUNT, read off its store rather than off any client's bookkeeping: a
	// refusal that sealed and submitted and then threw the answer away would still be a record on
	// the server, and no client-side counter would say so.
	rows := func() int {
		return len(world.allRows(t, groupId))
	}
	before := rows()
	if before == 0 {
		t.Fatal("the server holds no rows for this group, so an unchanged count would measure nothing")
	}
	target := carolGroup.Messages()[0].MessageId
	if len(target) == 0 {
		t.Fatal("carol's log holds no message to name, so React and Delete would refuse for the wrong reason")
	}
	refusals := []struct {
		name string
		err  error
	}{
		{"Send", func() error { _, err := carolGroup.Send(ctx, "a line an observer may not send"); return err }()},
		{"SendReply", func() error { _, err := carolGroup.SendReply(ctx, target, "an answer"); return err }()},
		{"React", func() error { _, err := carolGroup.React(ctx, target, "x"); return err }()},
		{"Unreact", func() error { _, err := carolGroup.Unreact(ctx, target, "x"); return err }()},
		{"Delete", func() error { _, err := carolGroup.Delete(ctx, target); return err }()},
	}
	for _, one := range refusals {
		if !errors.Is(one.err, urmessage.ErrObserverMayNotSend) {
			t.Errorf("an observer's %s over the server answered %v, want ErrObserverMayNotSend", one.name, one.err)
		}
	}
	if after := rows(); after != before {
		t.Errorf("the server holds %d rows for this group after five refused sends, and held %d before; "+
			"a refusal must seal nothing and submit nothing", after, before)
	}
	if stats := carolGroup.Stats(); stats.Submitted != 1 {
		t.Errorf("carol's Stats.Submitted is %d; only her one pre-demotion line should ever have been submitted",
			stats.Submitted)
	}

	// ── AND SHE GOES ON READING, WHICH IS THE OTHER HALF OF READ-ONLY ───────────────────────────
	const afterTheDemotion = "alice, after making carol an observer"
	if _, err := aliceGroup.Send(ctx, afterTheDemotion); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	owners := gcReceiveTextMessage(t, ctx, "carol", carolGroup, afterTheDemotion)
	if owners.SenderRoleAtSend != "owner" {
		t.Errorf("carol reads the owner's line as sent by a %q, want owner", owners.SenderRoleAtSend)
	}

	// ── AND HER OWN EARLIER LINE STILL SAYS MEMBER, AT BOTH DEVICES ─────────────────────────────
	for name, group := range map[string]*urmessage.Group{"alice": aliceGroup, "carol": carolGroup} {
		found := false
		for _, held := range group.Messages() {
			if held.Text != beforeTheDemotion {
				continue
			}
			found = true
			if held.SenderRoleAtSend != "member" {
				t.Errorf("%s reads carol's pre-demotion line as %q after the demotion, want member; "+
					"a role is a fact about the epoch it was sent at", name, held.SenderRoleAtSend)
			}
		}
		if !found {
			t.Errorf("%s's log lost carol's pre-demotion line", name)
		}
		stats := group.Stats()
		if stats.HiddenObserver != 0 {
			t.Errorf("%s hid %d message(s): no observer in this group ever succeeded in sending one", name, stats.HiddenObserver)
		}
		if stats.RoleUndeterminable != 0 {
			t.Errorf("%s opened %d record(s) whose sender's role it could not read", name, stats.RoleUndeterminable)
		}
	}
	t.Logf("observer: carol's five verbs refused with the server's row count unmoved at %d, her own "+
		"pre-demotion line still reading member at both devices, and the owner's new line reaching her", before)
}
