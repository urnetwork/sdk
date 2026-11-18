//go:build windows

package sdk

import (
	"path/filepath"
	"runtime"
	"testing"
)

// On Windows itself, with nothing injected: both stores commit through the
// write-through rename, stay open, and a new store reads every pin back.
func TestPeerPinStoresCommitOnWindows(t *testing.T) {
	if peerPinDurableRenameFor(runtime.GOOS).flushDirectory {
		t.Fatal("windows cannot flush a directory, but its durable rename flushes one")
	}
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		firstPeer, firstPin := testBoundedPin(21)
		if err := f.store.CommitPeerClientKeyPin(firstPeer, firstPin); err != nil {
			t.Fatalf("%s: the first pin was refused: %v", kind, err)
		}
		secondPeer, secondPin := testBoundedPin(22)
		if err := f.store.CommitPeerClientKeyPin(secondPeer, secondPin); err != nil {
			t.Fatalf("%s: the second pin was refused: %v", kind, err)
		}
		reloaded := newLocalStatePeerClientKeyPinStore(filepath.Dir(f.path))
		if got, err := reloaded.ReadPeerClientKeyPinSnapshot(firstPeer); err != nil || got.Pin != firstPin {
			t.Fatalf("%s: the file lost the first pin: %+v %v", kind, got, err)
		}
		if got, err := reloaded.ReadPeerClientKeyPinSnapshot(secondPeer); err != nil || got.Pin != secondPin {
			t.Fatalf("%s: the file lost the second pin: %+v %v", kind, got, err)
		}
	}
}
