package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestDeviceLocalRoutingTierPersistRestore: the tier persists to local state
// on set, and a new device on the same network space restores it. Mirrors
// TestDeviceLocalBlockerEnabledPersistRestore (device_local_blocker_persist_test.go)
// in shape -- this is the device-level path (SetRoutingTier -> persistRoutingTier
// -> LocalState -> a fresh DeviceLocal's constructor restore block), not just
// the LocalState primitive TestRoutingTierPersistsAcrossLocalStateReload
// already covers.
func TestDeviceLocalRoutingTierPersistRestore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	connect.AssertEqual(t, int(RoutingTierOff), localState.GetRoutingTier())

	// the set persists asynchronously to local state
	device.SetRoutingTier(int(RoutingTierFull))
	persisted := false
	for i := 0; i < 100; i += 1 {
		if localState.GetRoutingTier() == int(RoutingTierFull) {
			persisted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	connect.AssertEqual(t, true, persisted)
	device.Close()

	// a new device on the same network space restores the persisted tier
	restored := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer restored.Close()
	connect.AssertEqual(t, int(RoutingTierFull), restored.routingTier)
}
