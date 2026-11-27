//go:build !js

// The device role's status notifications (QD-1437): a read of a role's posture holds no bridge again, so a status,
// which reads each forwarding role's posture, asks for no further status, and a hold that changes nothing tells nobody.
// Before, each read settled the owner, each settle held the bridges again and each hold told the listeners, so with one
// listener attached each status asked for the next: 8,663 statuses in 500 ms with nothing changing.

package sdk

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A forwarding device whose status notifications and owner's holds the test counts, its role set up, started and then
// stopped, so nothing of the role calls the posture meanwhile.
func testingQuietForwardDevice(t *testing.T) (device *DeviceLocal, networkId connect.Id, notified *atomic.Int64, holds *atomic.Int64) {
	t.Helper()
	notified = &atomic.Int64{}
	forward := newTestingForwardDevice(t)
	device = forward.device(t, func(settings *DeviceLocalSettings) {
		settings.testingBridgeStatusNotify = func() {
			notified.Add(1)
		}
	})
	networkId = forward.role.vectors.networkId()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	role.stop()
	holds = &atomic.Int64{}
	func() {
		owner := device.bridgeOwner
		owner.operationLock.Lock()
		defer owner.operationLock.Unlock()
		hold := owner.hold
		owner.hold = func(bridge deviceBridge) {
			holds.Add(1)
			hold(bridge)
		}
	}()
	return device, networkId, notified, holds
}

// With a listener attached, the one status triggered is the one told: its read of the role's posture holds no bridge
// again and tells nothing more, and no other read of a role's posture does either.
func TestBridgeStatusReadAsksForNoFurtherStatus(t *testing.T) {
	device, networkId, notified, holds := testingQuietForwardDevice(t)
	changed := make(chan *BridgeStatus, 64)
	sub := device.AddBridgeStatusChangeListener(&testingBridgeStatusListener{changed: changed})
	defer sub.Close()
	notifiedBefore, holdsBefore := notified.Load(), holds.Load()

	device.notifyBridgeStatus()
	select {
	case status := <-changed:
		if status.Networks == nil || status.Networks.Len() != 1 {
			t.Fatalf("the status told carries no forwarding role: %+v", status)
		}
	case <-time.After(testSidecarTimeout):
		t.Fatal("the status triggered was never told")
	}
	// the status told was built, its read of the role's posture done before it was told
	if count := holds.Load() - holdsBefore; count != 0 {
		t.Errorf("the status's read held the bridges again %d times", count)
	}
	if count := notified.Load() - notifiedBefore; count != 1 {
		t.Errorf("the status triggered made %d notifications", count)
	}

	device.bridgeOwner.roleDecision(networkId)
	device.bridgeOwner.roleDecisionNow(networkId)
	device.bridgeOwner.roleStatus(networkId)
	device.GetBridgeStatus()
	if count := holds.Load() - holdsBefore; count != 0 {
		t.Errorf("the reads of the role's posture held the bridges again %d times", count)
	}
	if count := notified.Load() - notifiedBefore; count != 1 {
		t.Errorf("the reads of the role's posture made %d notifications beside the one triggered", count-1)
	}
	select {
	case status := <-changed:
		t.Errorf("a status was told with nothing changed: %+v", status)
	default:
	}
}

// A hold that changes nothing, as each settle after an owner's call repeats, tells nobody: holding the bridge held
// again, its routes as they were, makes no notification, nor does a settle after a call that moves nothing; a hold that
// changes the held bridge makes one, and so does the hold back.
func TestDeviceLocalHoldThatChangesNothingTellsNobody(t *testing.T) {
	device, networkId, notified, holds := testingQuietForwardDevice(t)
	held := func() deviceBridge {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.bridge
	}()
	if held == nil {
		t.Fatal("the device holds no bridge past legacy")
	}
	before := notified.Load()

	device.holdBridge(held)
	device.holdBridge(held)
	if count := notified.Load() - before; count != 0 {
		t.Errorf("two holds that changed nothing made %d notifications", count)
	}
	holdsBefore := holds.Load()
	if err := device.bridgeOwner.onRole(networkId, false, func(bridge deviceBridgeOwned) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if holds.Load() == holdsBefore {
		t.Fatal("the owner's call settled with no hold")
	}
	if count := notified.Load() - before; count != 0 {
		t.Errorf("a settle after a call that moved nothing made %d notifications", count)
	}

	device.holdBridge(nil)
	device.holdBridge(held)
	if count := notified.Load() - before; count != 2 {
		t.Errorf("two holds that changed the held bridge made %d notifications", count)
	}
}
