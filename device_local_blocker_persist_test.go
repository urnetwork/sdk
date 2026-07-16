package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestDeviceLocalBlockerEnabledPersistRestore: the toggle persists to local
// state on set, and a new device on the same network space restores it.
func TestDeviceLocalBlockerEnabledPersistRestore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	connect.AssertEqual(t, false, device.GetBlockerEnabled())

	// the set persists asynchronously to local state
	device.SetBlockerEnabled(true)
	connect.AssertEqual(t, true, device.GetBlockerEnabled())
	persisted := false
	for i := 0; i < 100; i += 1 {
		if localState.GetBlockerEnabled() {
			persisted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	connect.AssertEqual(t, true, persisted)
	device.Close()

	// a new device on the same network space restores the persisted toggle
	restored := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer restored.Close()
	connect.AssertEqual(t, true, restored.GetBlockerEnabled())
}
