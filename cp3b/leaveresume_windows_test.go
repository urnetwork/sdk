//go:build windows

package cp3b

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// AN ERASE STOPPED BY A FILE ANOTHER PROCESS HOLDS OPEN (msgrepo ledger §7, 2026-10-03, review H1;
// the review's own probe, kept as the regression it found). An indexer, a backup tool or a virus
// scanner opens files without FILE_SHARE_DELETE, and Windows then refuses to delete them. The
// review measured the build before the mark: the first ForgetGroup failed after the keys were gone,
// every later one answered ErrGroupNotHeld, and every Restore failed, with this device's own line
// still on the disk.
func TestALeaveStoppedByAHeldOpenFileIsFinishedOnceItIsReleased(t *testing.T) {
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
	needle := []byte("BOB-LINE-HELD-OPEN-BY-A-SCANNER")
	if _, err := bobGroup.Send(ctx, string(needle)); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	var sentFile string
	for _, path := range leaveFindGroupFile(t, bob.stateDir, needle) {
		if filepath.Base(filepath.Dir(path)) == "sent" {
			sentFile = path
		}
	}
	if sentFile == "" {
		t.Fatalf("CONTROL FAILED: bob's line is in no sent copy")
	}
	name, err := syscall.UTF16PtrFromString(sentFile)
	if err != nil {
		t.Fatal(err)
	}
	held, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("holding the sent copy open: %v", err)
	}
	released := false
	defer func() {
		if !released {
			syscall.CloseHandle(held)
		}
	}()

	if err := bob.device.ForgetGroup(groupId); !errors.Is(err, urmessage.ErrForgetUnfinished) {
		t.Fatalf("the leave with a sent copy held open answered %v, want ErrForgetUnfinished", err)
	}
	if n := len(bob.device.Groups()); n != 0 {
		t.Errorf("a device that has left still names %d group(s)", n)
	}
	if err := bob.device.ForgetGroup(groupId); !errors.Is(err, urmessage.ErrForgetUnfinished) {
		t.Errorf("leaving again while the file is still held answered %v, want ErrForgetUnfinished", err)
	}

	syscall.CloseHandle(held)
	released = true
	if err := bob.device.ForgetGroup(groupId); err != nil {
		t.Fatalf("leaving again once the file was released: %v", err)
	}
	if found := leaveFindGroupFile(t, bob.stateDir, needle); len(found) != 0 {
		t.Errorf("bob's own line is still on his disk: %v", found)
	}
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	if restored, err := bob.device.Restore(ctx); err != nil || len(restored) != 0 {
		t.Errorf("after the finished leave, Restore answered %d group(s), %v", len(restored), err)
	}
}
