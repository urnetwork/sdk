package cp3b

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// "DELETE FOR ME AND LEAVE", [urmessage.Device.ForgetGroup], over a running server. The owner's
// ruling of 2026-10-02 asked for "one party can just locally delete and leave"; ledger item 273
// decided how; ruling 48 (item 257) is why the device half is all a device can do.

// forgetFilesUnder counts the regular files under a persona's state directory, so "nothing of
// the group is left on the disk" is a measurement of the directory and not of an API that might
// itself be forgetting to look.
func forgetFilesUnder(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			count += 1
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return count
}

// A MEMBER WHO LEAVES LEAVES NOTHING OF THE GROUP ON THE DISK, does not come back at a restart,
// and is then removable and addable again: the roster half is an admin's (ruling 48), and R6a
// refuses re-adding an identity until its old leaf is gone, so the remove comes first.
//
// WHAT WOULD GO RED: a ForgetGroup that skipped the store erase (the restart restores the group);
// one that skipped the close (bob's Send after it goes through); one that erased less than the
// group's whole directory (the file count does not return to the device's own).
func TestAMemberWhoLeavesLeavesNothingBehindAndCanBeAddedBack(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	// the files bob's device holds before it is in any group: its identity and nothing else
	bare := forgetFilesUnder(t, bob.stateDir)

	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := aliceGroup.Send(ctx, "a line from alice"); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if _, err := bobGroup.Send(ctx, "a line from bob, whose copy sits on bob's disk"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if held := forgetFilesUnder(t, bob.stateDir); held <= bare {
		t.Fatalf("CONTROL FAILED: bob's state directory holds %d files in a group and %d before it", held, bare)
	}
	bobId := rolesIdentityOf(t, bobGroup)

	if err := bob.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("bob's ForgetGroup: %v", err)
	}
	if got := forgetFilesUnder(t, bob.stateDir); got != bare {
		t.Errorf("after leaving, bob's state directory holds %d files, and before the group it held %d", got, bare)
	}
	if len(bob.device.Groups()) != 0 {
		t.Errorf("after leaving, bob's device still names %d group(s)", len(bob.device.Groups()))
	}
	if _, err := bobGroup.Send(ctx, "after leaving"); err == nil {
		t.Errorf("a group bob left still sent")
	}
	if got := forgetFilesUnder(t, bob.stateDir); got != bare {
		t.Errorf("the refused Send wrote the disk: %d files, want %d", got, bare)
	}
	if err := bob.device.ForgetGroup(groupId); !errors.Is(err, urmessage.ErrGroupNotHeld) {
		t.Errorf("leaving twice answered %v, want ErrGroupNotHeld", err)
	}

	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("a group bob left came back at his restart")
	}

	// the others are not told: alice still holds bob's leaf and still sends
	if _, err := aliceGroup.Send(ctx, "alice, not told"); err != nil {
		t.Fatalf("alice's Send after bob left: %v", err)
	}
	// the roster half is the owner's, and then bob can come back
	if err := aliceGroup.RemoveMember(ctx, bobId); err != nil {
		t.Fatalf("alice's RemoveMember of the bob who left: %v", err)
	}
	bobAgain := hsAddAndJoin(t, ctx, aliceGroup, bob)
	if _, err := bobAgain.Receive(ctx); err != nil {
		t.Fatalf("the re-added bob's Receive: %v", err)
	}
	line, err := bobAgain.Send(ctx, "bob, back")
	if err != nil {
		t.Fatalf("the re-added bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if got := messageById(t, aliceGroup, line.MessageId); got.Text != "bob, back" {
		t.Errorf("alice read %q", got.Text)
	}
}

// AN OWNER IS REFUSED UNTIL OWNERSHIP MOVES, AND THE REFUSAL CHANGES NOTHING: the group still
// sends and its files are all still there. After the transfer the ex-owner is an admin, leaving is
// allowed, and the new owner can remove the leaf, which is H2's whole remedy.
//
// WHAT WOULD GO RED: no refusal (the first ForgetGroup succeeds); a refusal that closed the group
// anyway (the Send after it fails); a refusal checked after the erase (the file count drops).
func TestAnOwnerIsRefusedUntilOwnershipMovesAndTheRefusalChangesNothing(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	aliceId, bobId := rolesIdentityOf(t, aliceGroup), rolesIdentityOf(t, bobGroup)
	before := forgetFilesUnder(t, alice.stateDir)

	if err := alice.device.ForgetGroup(groupId); !errors.Is(err, urmessage.ErrOwnerMustTransfer) {
		t.Fatalf("the owner's ForgetGroup answered %v, want ErrOwnerMustTransfer", err)
	}
	if got := forgetFilesUnder(t, alice.stateDir); got != before {
		t.Errorf("the refused leave changed alice's disk: %d files, want %d", got, before)
	}
	if _, err := aliceGroup.Send(ctx, "still here"); err != nil {
		t.Errorf("the refused leave closed the group: %v", err)
	}

	if err := aliceGroup.TransferOwnership(ctx, bobId); err != nil {
		t.Fatalf("alice's TransferOwnership: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the transfer: %v", err)
	}
	if err := alice.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("the ex-owner's ForgetGroup: %v", err)
	}
	if err := bobGroup.RemoveMember(ctx, aliceId); err != nil {
		t.Fatalf("the new owner's RemoveMember of the ex-owner who left: %v", err)
	}
}

// AN OWNER WITH NOBODY ELSE MAY LEAVE: a group founded and never joined strands nobody.
func TestAnOwnerAloneMayLeave(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	groupId := newGroupId(t)
	if _, err := alice.device.CreateGroup(ctx, groupId); err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	if err := alice.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("the lone owner's ForgetGroup: %v", err)
	}
	alice = world.restart(t, alice)
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted alice's Connect: %v", err)
	}
	restored, err := alice.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Restore: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("a group alice left came back at her restart")
	}
}

// A DEVICE A VALID COMMIT REMOVED MAY LEAVE: it is not in the group, so it strands nobody, and
// what it still holds is the history up to its removal, which is what "delete for me" deletes.
func TestARemovedDeviceMayLeave(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if err := aliceGroup.RemoveMember(ctx, rolesIdentityOf(t, bobGroup)); err != nil {
		t.Fatalf("alice's RemoveMember: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); !errors.Is(err, urmessage.ErrRemovedFromGroup) {
		t.Fatalf("bob's Receive of his removal answered %v", err)
	}
	if err := bob.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("the removed bob's ForgetGroup: %v", err)
	}
}

// AN OWNER WITH ANOTHER DEVICE OF ITS OWN STILL IN THE GROUP MAY LEAVE FROM THIS ONE: the identity
// stays, and so does its ownership, so nobody is stranded. Without this row the exemption in
// ownerMayNotLeaveLocked could be deleted and every test above would stay green.
func TestAnOwnerWithAnotherDeviceOfItsOwnMayLeaveFromThisOne(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	aliceId := rolesIdentityOf(t, aliceGroup)
	laptop := world.seamMemberClaiming(t, ctx, "alice-laptop", aliceId)
	invite, err := aliceGroup.AddMemberAndPublish(ctx, laptop.keyPackage(t))
	if err != nil {
		t.Fatalf("alice's AddMemberAndPublish of her own second device: %v", err)
	}
	laptop.join(t, gcReencodeInvite(t, invite))
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the add: %v", err)
	}
	if got := len(removalLeavesOf(t, aliceGroup, aliceId)); got != 2 {
		t.Fatalf("CONTROL FAILED: alice's identity holds %d leaf/leaves, want 2", got)
	}
	if err := alice.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("the owner's ForgetGroup with her own second device still in the group: %v", err)
	}
	// and her identity is still the owner, at bob
	for _, member := range removalRoster(t, bobGroup) {
		if bytes.Equal(member.IdentityPub, aliceId) && member.Role != "owner" {
			t.Errorf("after alice left from one device, bob reads her identity as %q", member.Role)
		}
	}
}
