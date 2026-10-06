package cp3b

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sdk/v2026/urmessage"
)

// A LEAVE THAT STOPS PART WAY IS FINISHED, NOT LOST (msgrepo ledger §7, 2026-10-03, review H1),
// asked of a real device over a real server. These are the device half of
// urmessage/forgetmark_test.go; leaveresume_windows_test.go stops the erase the way a file another
// process holds open does, and these stop it the way a crash does, on every platform.

// leaveFindGroupFile answers every file under a persona's state directory whose content carries
// `needle`, so "this device's own line is gone from the disk" is a property of the octets.
func leaveFindGroupFile(t *testing.T, dir string, needle []byte) []string {
	t.Helper()
	found := []string{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(raw, needle) {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// leaveStopAfterTheKeys reproduces what a crash in the middle of the erase leaves: the mark down,
// and the epoch directory of every marked group gone, with everything else still on the disk.
func leaveStopAfterTheKeys(t *testing.T, stateDir string) {
	t.Helper()
	groups := filepath.Join(stateDir, "state", "group")
	entries, err := os.ReadDir(groups)
	if err != nil {
		t.Fatalf("reading %s: %v", groups, err)
	}
	stopped := 0
	for _, entry := range entries {
		dir := filepath.Join(groups, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "forgetting")); err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, "epoch")); err != nil {
			t.Fatalf("removing the epochs: %v", err)
		}
		stopped += 1
	}
	if stopped != 1 {
		t.Fatalf("CONTROL FAILED: %d marked group(s) under %s, want 1", stopped, groups)
	}
}

// THE RESTORE AFTER A CRASH FINISHES THE ERASE AND RESTORES NOTHING, and this device's own line is
// gone from the disk. Before the mark, this state was a restore that failed at every launch and a
// group no ForgetGroup could name, with the line still in the sent copies.
func TestACrashMidLeaveIsFinishedByTheNextRestore(t *testing.T) {
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
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)
	needle := []byte("BOB-LINE-THAT-MUST-NOT-OUTLIVE-THE-LEAVE")
	if _, err := bobGroup.Send(ctx, string(needle)); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if len(leaveFindGroupFile(t, bob.stateDir, needle)) == 0 {
		t.Fatalf("CONTROL FAILED: bob's line is not on his disk before the leave")
	}

	// the leave gets as far as its mark and the keys, and the process dies
	if err := bob.stateStore.MarkGroupForgetting(groupId); err != nil {
		t.Fatalf("MarkGroupForgetting: %v", err)
	}
	stateDir := bob.stateDir
	bob.kill()
	leaveStopAfterTheKeys(t, stateDir)

	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Errorf("the Restore that finishes an interrupted leave answered %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("a group bob was leaving came back at his restart")
	}
	if found := leaveFindGroupFile(t, bob.stateDir, needle); len(found) != 0 {
		t.Errorf("bob's own line is still on his disk after the restore finished his leave: %v", found)
	}
	if err := bob.device.ForgetGroup(groupId); !errors.Is(err, urmessage.ErrGroupNotHeld) {
		t.Errorf("leaving again after the erase was finished answered %v, want ErrGroupNotHeld", err)
	}
}

// AND ForgetGroup FINISHES IT TOO, without a Restore and without a connection: the mark is enough
// to name the group, and nothing about a group whose keys are gone is asked of it again.
func TestACrashMidLeaveIsFinishedByLeavingAgain(t *testing.T) {
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
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)
	needle := []byte("ANOTHER-BOB-LINE-THAT-MUST-GO")
	if _, err := bobGroup.Send(ctx, string(needle)); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if err := bob.stateStore.MarkGroupForgetting(groupId); err != nil {
		t.Fatalf("MarkGroupForgetting: %v", err)
	}
	stateDir := bob.stateDir
	bob.kill()
	leaveStopAfterTheKeys(t, stateDir)

	bob = world.restart(t, bob)
	if err := bob.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("leaving again, with no Restore and no connection: %v", err)
	}
	if found := leaveFindGroupFile(t, bob.stateDir, needle); len(found) != 0 {
		t.Errorf("bob's own line is still on his disk: %v", found)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	if restored, err := bob.device.Restore(ctx); err != nil || len(restored) != 0 {
		t.Errorf("after the finished leave, Restore answered %d group(s), %v", len(restored), err)
	}
}

// A GROUP RE-JOINED OVER A STANDING MARK SURVIVES THE NEXT RESTART (the re-check's R1, which it
// reproduced): the mark is the erase's own instruction, so a Join that wrote beside it had its new
// group erased by the next Restore with no warning. Join now finishes the standing erase first.
func TestAGroupRejoinedOverAnUnfinishedLeaveSurvivesTheNextRestart(t *testing.T) {
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
	bobId := rolesIdentityOf(t, bobGroup)
	// the leave gets as far as its mark and the keys, and the process dies
	if err := bob.stateStore.MarkGroupForgetting(groupId); err != nil {
		t.Fatalf("MarkGroupForgetting: %v", err)
	}
	stateDir := bob.stateDir
	bob.kill()
	leaveStopAfterTheKeys(t, stateDir)
	bob = world.restart(t, bob)

	// NO Restore: the owner takes the old leaf out and adds bob back to the SAME group id
	if err := aliceGroup.RemoveMember(ctx, bobId); err != nil {
		t.Fatalf("alice's RemoveMember: %v", err)
	}
	bobAgain := hsAddAndJoin(t, ctx, aliceGroup, bob)
	if _, err := bobAgain.Receive(ctx); err != nil {
		t.Fatalf("the re-joined bob's Receive: %v", err)
	}
	line, err := bobAgain.Send(ctx, "bob, back in the same group")
	if err != nil {
		t.Fatalf("the re-joined bob's Send: %v", err)
	}

	bob = world.restart(t, bob)
	restoredGroup := hsRestoreOne(t, ctx, bob)
	if _, err := restoredGroup.Receive(ctx); err != nil {
		t.Fatalf("the restored bob's Receive: %v", err)
	}
	if got := messageById(t, restoredGroup, line.MessageId); got.Text != "bob, back in the same group" {
		t.Errorf("the restored bob reads %q", got.Text)
	}
}
