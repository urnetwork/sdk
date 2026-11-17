// Windows can never flush a directory handle: FlushFileBuffers needs
// GENERIC_WRITE, which a directory handle cannot have, so os.(*File).Sync on a
// directory there always fails. The pin stores flushed the directory after
// every rename and failed closed when it was refused, so on Windows every
// commit failed, the store closed, and under post-quantum encryption every
// provider after the first was refused ("local identity pin store unavailable:
// peer identity pin store persistence failed", logged about 300 times in 18
// minutes in urnetwork/windows#26). A platform that cannot flush a directory
// now writes its rename through and never asks for the directory sync.
package sdk

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

// Both stores, under Windows' durable rename, admit and keep every pin while
// every directory flush is refused as Windows refuses it. Before the fix the
// first commit failed and closed the store.
func TestPeerPinStoresStayOpenWhereDirectoriesCannotBeFlushed(t *testing.T) {
	windowsRename := peerPinDurableRenameFor("windows")
	if windowsRename.flushDirectory {
		t.Fatal("windows cannot flush a directory, but its durable rename flushes one")
	}
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		f.setDurableRename(windowsRename)
		directoryFlushCount := 0
		f.commitStep(func(step string) error {
			if step == "directory sync" {
				directoryFlushCount += 1
				return errors.New("Access is denied.")
			}
			return nil
		})

		firstPeer, firstPin := testBoundedPin(11)
		if err := f.store.CommitPeerClientKeyPin(firstPeer, firstPin); err != nil {
			t.Fatalf("%s: the first provider's pin was refused: %v", kind, err)
		}
		secondPeer, secondPin := testBoundedPin(12)
		firstSnapshot, err := f.store.ReadPeerClientKeyPinSnapshot(firstPeer)
		if err != nil {
			t.Fatalf("%s: the store closed after the first commit: %v", kind, err)
		}
		secondSnapshot, err := f.store.ReadPeerClientKeyPinSnapshot(secondPeer)
		if err != nil {
			t.Fatal(err)
		}
		if admitted, err := f.store.CompareAndSwapPeerClientKeyPin(secondPeer, secondSnapshot, &secondPin); !admitted || err != nil {
			t.Fatalf("%s: the second provider's pin was refused: %t %v", kind, admitted, err)
		}
		if !firstSnapshot.Pinned || firstSnapshot.Pin != firstPin || !firstSnapshot.SignedHistorySeen {
			t.Fatalf("%s: the first pin was not kept: %+v", kind, firstSnapshot)
		}
		if directoryFlushCount != 0 {
			t.Fatalf("%s: asked for %d directory flushes on a platform that cannot flush one", kind, directoryFlushCount)
		}

		// the renamed file is the durable state: a new store reads both pins
		reloaded := newLocalStatePeerClientKeyPinStore(filepath.Dir(f.path))
		for _, want := range []struct {
			peer connect.Id
			pin  connect.ClientKeyPin
		}{
			{peer: firstPeer, pin: firstPin},
			{peer: secondPeer, pin: secondPin},
		} {
			got, err := reloaded.ReadPeerClientKeyPinSnapshot(want.peer)
			if err != nil || !got.Pinned || got.Pin != want.pin || !got.SignedHistorySeen {
				t.Fatalf("%s: the file lost a committed pin: %+v %v", kind, got, err)
			}
		}
	}
}

// Every platform that can flush a directory still requires the flush after the
// rename, and fails closed when it is refused (TestPeerPinStoresAtomicPersistenceFailure).
func TestPeerPinDurableRenameFlushesDirectoriesOffWindows(t *testing.T) {
	for _, goos := range []string{"darwin", "ios", "linux", "android", "freebsd"} {
		if !peerPinDurableRenameFor(goos).flushDirectory {
			t.Fatalf("%s can flush a directory, but its durable rename skips the flush", goos)
		}
	}
}
