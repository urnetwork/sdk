package sdk

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The RPC's copy of the bridge status carries every field through gob both ways. The bound status holds StringLists
// and a bound list, which gob cannot encode, so a status sent as itself never crossed: a mirror that forgets a field
// drops it silently, which device_rpc_mirror_test.go exists for.
func TestRpcMirrorBridgeStatusComplete(t *testing.T) {
	seed := 0
	status := &BridgeStatus{}
	fillNonZero(t, reflect.ValueOf(status), &seed)
	// the bound lists' backing slices are unexported, so fillNonZero cannot fill them; the rows are filled here
	status.Addresses = stringListFromStrings([]string{"10.1.0.2", "fd00::2"})
	status.Networks = NewBridgeNetworkStatusList()
	for range 2 {
		network := &BridgeNetworkStatus{}
		fillNonZero(t, reflect.ValueOf(network), &seed)
		network.Alarms = stringListFromStrings([]string{"compile_deferred", "narrowing_overdue"})
		status.Networks.Add(network)
	}

	wired := gobRoundTrip(t, newDeviceRemoteBridgeStatus(status))
	connect.AssertEqual(t, wired.toBridgeStatus(), status)
}

// A remote's bridge status listener keeps hearing the VPN component's process across a reconnect: each connection's
// rpc starts with no listener, so the sync re-adds the ids the remote holds, and the reverse sync sends the current
// status. Over a real device RPC pair, each end on its own port.
func TestBridgeStatusMirrorFollowsAResync(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	address, err := parseDeviceRemoteAddress(testing_freeHostPort())
	if err != nil {
		t.Fatal(err)
	}
	settings.Address = address
	if err := device.SetRpcServer("", "", settings.Address.HostPort()); err != nil {
		t.Fatal(err)
	}
	remoteSpace, remoteState := testingLogoutCauseSpace(t, "http://127.0.0.1:1")
	_, remoteClientJwt := testingLogoutCauseLogin(t, remoteState, forward.role.instanceId)
	remote, err := newDeviceRemoteWithOverrides(remoteSpace, remoteClientJwt, forward.role.instanceId, settings, connect.NewId(), testing_deviceRpcDialer(settings))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	remote.Sync()
	if !remote.waitForSync(testSidecarTimeout) {
		t.Fatal("the remote did not sync with the device")
	}
	changed := make(chan *BridgeStatus, 64)
	sub := remote.AddBridgeStatusChangeListener(&testingBridgeStatusListener{changed: changed})
	defer sub.Close()

	testingForceDeviceRpcResync(remote)
	if !remote.waitForSync(testSidecarTimeout) {
		t.Fatal("the remote did not sync again")
	}
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	deadline := time.After(testSidecarTimeout)
	for {
		select {
		case status := <-changed:
			if status.Networks != nil && status.Networks.Len() == 1 && status.Networks.Get(0).Posture == "pending" {
				return
			}
		case <-deadline:
			t.Fatal("the remote's listener heard no status after the reconnect")
		}
	}
}

// Closing a connection's service drops that rpc's subscription on the device, so a reconnect leaves no rpc of a dead
// connection subscribed.
func TestBridgeStatusListenerLeavesWithItsConnection(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	rpc := &DeviceLocalRpc{deviceLocal: device}
	if err := rpc.AddBridgeStatusChangeListener(connect.NewId(), nil); err != nil {
		t.Fatal(err)
	}
	if count := len(device.bridgeStatusListeners().Get()); count != 1 {
		t.Fatalf("%d status listeners on the device, want 1", count)
	}
	func() {
		rpc.stateLock.Lock()
		defer rpc.stateLock.Unlock()
		rpc.closeService()
	}()
	if count := len(device.bridgeStatusListeners().Get()); count != 0 {
		t.Errorf("%d status listeners on the device after its connection closed", count)
	}
	if len(rpc.bridgeStatusChangeListenerIds) != 0 || rpc.bridgeStatusChangeListenerSub != nil {
		t.Error("the closed rpc still holds a listener")
	}
}

// Collects status changes.
type testingBridgeStatusListener struct {
	changed chan *BridgeStatus
}

func (self *testingBridgeStatusListener) BridgeStatusChanged(status *BridgeStatus) {
	select {
	case self.changed <- status:
	default:
	}
}
