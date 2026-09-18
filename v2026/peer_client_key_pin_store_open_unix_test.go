//go:build darwin || linux || android || ios

package sdk

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBoundedPeerPinStoreRejectsFifoWithoutBlocking(t *testing.T) {
	state, budget := pinStoreTestOwner(t)
	if err := unix.Mkfifo(filepath.Join(state.localStorageDir, peerClientKeyPinsFileName), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
		store.Close()
		done <- err
	}()
	select {
	case err := <-done:
		if err != errPeerPinStoreIO || budget.UsedByteCount() != 0 {
			t.Fatalf("FIFO admission=%v used=%d", err, budget.UsedByteCount())
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked constructor before type validation")
	}
}
