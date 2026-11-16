//go:build !js

// NewDeviceLocalWithForwardKeyStore (QD-844; sidecar §18.6's Setup, owner decision 197, SDK-M8-16): the constructor a
// host on the c abi gives its VPN component's forwarding key store through. A device built through it sets forwarding
// up as one built on settings that carry the store by SetForwardKeyStore does, where NewDeviceLocal on the same
// settings with no store sets none up; the argument is the device's store whatever the settings held; the caller's
// settings are left as they were; and behind the device role's release gate the store takes no call.
package sdk

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
)

// A forwarding key store that counts every call it takes before it hands the call on.
type testingCountingForwardKeyStore struct {
	store     SidecarForwardKeyStore
	callCount atomic.Int64
}

func (self *testingCountingForwardKeyStore) PutSecret(name string, valueBase64 string) int32 {
	self.callCount.Add(1)
	return self.store.PutSecret(name, valueBase64)
}

func (self *testingCountingForwardKeyStore) GetSecret(name string) *SidecarForwardSecret {
	self.callCount.Add(1)
	return self.store.GetSecret(name)
}

func (self *testingCountingForwardKeyStore) DeleteSecret(name string) int32 {
	self.callCount.Add(1)
	return self.store.DeleteSecret(name)
}

// The settings a forwarding test's device is built on, with the VPN component's directory and no key store: what a
// host on the c abi can say in json, beside the test's own transport and the device role's Go-only gate.
func (self *testingForwardDevice) settingsWithNoKeyStore() *DeviceLocalSettings {
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.EnableBridge = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return &testingDnsOwnerGenerator{}
	}
	settings.ForwardStateDir = self.stateDir
	settings.testingForwardingDeps = func(deps *deviceForwardingDeps) {
		deps.apiUrl = "https://api.forward.test"
		deps.platformUrl = "wss://platform.forward.test"
		deps.transport = self.transport
		deps.clock = self.clock
	}
	return settings
}

// A device on the role's space through NewDeviceLocalWithForwardKeyStore, joined at the test's end.
func (self *testingForwardDevice) deviceWithKeyStore(t *testing.T, settings *DeviceLocalSettings, keyStore SidecarForwardKeyStore) *DeviceLocal {
	t.Helper()
	device, err := NewDeviceLocalWithForwardKeyStore(
		self.role.space, self.role.clientJwt, "device-role", "test", "0", self.role.instanceId, settings, keyStore,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testingJoinDeviceRole(t, device)
	})
	return device
}

// A device on the role's space through NewDeviceLocal, joined at the test's end.
func (self *testingForwardDevice) deviceOnSettings(t *testing.T, settings *DeviceLocalSettings) *DeviceLocal {
	t.Helper()
	device, err := NewDeviceLocal(
		self.role.space, self.role.clientJwt, "device-role", "test", "0", self.role.instanceId, settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testingJoinDeviceRole(t, device)
	})
	return device
}

// A device built through the constructor with a key store sets forwarding up: its keys are made into the store, a
// hand-off is taken and anchors the posture, and the sealed store goes under the component's directory. The settings
// it was built on still carry no store, so NewDeviceLocal on them sets no forwarding up: it has no forwarding host,
// refuses a setup, and its posture is not the one the hand-off anchored.
func TestNewDeviceLocalWithForwardKeyStoreSetsForwardingUp(t *testing.T) {
	forward := newTestingForwardDevice(t)
	networkId := forward.role.vectors.networkId()
	settings := forward.settingsWithNoKeyStore()

	device := forward.deviceWithKeyStore(t, settings, forward.store)
	if device.forwardingHost() == nil {
		t.Fatal("the device built with a key store has no forwarding host")
	}
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatalf("the setup: %v", err)
	}
	testingForwardStarted(t, device, networkId)
	if state := device.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Errorf("after the hand-off the posture is %v", state)
	}
	if names := forward.store.names(); len(names) == 0 {
		t.Error("the setup kept nothing in the key store the constructor was given")
	}
	role, _ := device.forwardingHost().role(networkId, false)
	if role == nil {
		t.Fatal("the network has no forwarding role")
	}
	if token, _ := role.keys.token(); token == "" {
		t.Error("F's token was not kept")
	}
	if matches, _ := filepath.Glob(filepath.Join(forward.stateDir, deviceForwardDirectoryName, "*", "setup*")); len(matches) == 0 {
		t.Error("no setup record under the component's directory")
	}
	testingJoinDeviceRole(t, device)

	if settings.forwardKeyStore != nil {
		t.Fatal("the constructor left its key store on the caller's settings")
	}
	writes := forward.store.writes
	plain := forward.deviceOnSettings(t, settings)
	if plain.forwardingHost() != nil {
		t.Fatal("a device built with no key store has a forwarding host")
	}
	if _, err := plain.forwardingKeys(networkId); !errors.Is(err, errDeviceForwardingNone) {
		t.Errorf("a device built with no key store answered a setup: %v", err)
	}
	if state := plain.bridgeOwner.roleDecision(networkId).State; state == sidecardevice.PosturePending {
		t.Error("a device built with no key store restored the forwarding role's posture")
	}
	if forward.store.writes != writes {
		t.Error("a device built with no key store wrote to the store")
	}
}

// A device built through the constructor restores what an earlier one's hand-off anchored, before its first packet,
// as one built on settings that carry the store does.
func TestNewDeviceLocalWithForwardKeyStoreRestoresThePosture(t *testing.T) {
	forward := newTestingForwardDevice(t)
	networkId := forward.role.vectors.networkId()
	device := forward.device(t, nil)
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	testingJoinDeviceRole(t, device)

	restarted := forward.deviceWithKeyStore(t, forward.settingsWithNoKeyStore(), forward.store)
	if state := restarted.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Errorf("after a restart through the constructor the posture is %v", state)
	}
	if restarted.sendRoute.Load().bridge == nil {
		t.Error("the restored posture is not in the first send route")
	}
}

// The argument is the device's store whatever the settings held: nil over a store the settings carry is no store, so
// the device sets no forwarding up and the settings' store takes no call; and a store over the settings' own is the
// one the keys go to. The caller's settings keep the store they had.
func TestNewDeviceLocalWithForwardKeyStoreArgumentIsTheStore(t *testing.T) {
	forward := newTestingForwardDevice(t)
	networkId := forward.role.vectors.networkId()
	settingsStore := &testingCountingForwardKeyStore{store: newTestingForwardKeyStore()}
	settings := forward.settingsWithNoKeyStore()
	settings.SetForwardKeyStore(settingsStore)

	none := forward.deviceWithKeyStore(t, settings, nil)
	if none.forwardingHost() != nil {
		t.Fatal("a device built with a nil key store has a forwarding host")
	}
	if _, err := none.forwardingKeys(networkId); !errors.Is(err, errDeviceForwardingNone) {
		t.Errorf("a device built with a nil key store answered a setup: %v", err)
	}
	testingJoinDeviceRole(t, none)

	device := forward.deviceWithKeyStore(t, settings, forward.store)
	if _, err := device.forwardingKeys(networkId); err != nil {
		t.Fatalf("the keys: %v", err)
	}
	if names := forward.store.names(); len(names) == 0 {
		t.Error("the keys did not go to the store the constructor was given")
	}
	if count := settingsStore.callCount.Load(); count != 0 {
		t.Errorf("the settings' store took %d calls", count)
	}
	if settings.forwardKeyStore != SidecarForwardKeyStore(settingsStore) {
		t.Error("the constructor changed the store on the caller's settings")
	}
}

// Behind the device role's release gate, which is EnableBridge and off by default, a device given a key store and a
// directory runs no forwarding role: every forwarding call refuses, and the store takes no call. So does one given a
// store and no absolute directory with the gate open.
func TestNewDeviceLocalWithForwardKeyStoreStaysOffBehindTheGate(t *testing.T) {
	forward := newTestingForwardDevice(t)
	networkId := forward.role.vectors.networkId()
	store := &testingCountingForwardKeyStore{store: forward.store}

	gated := forward.settingsWithNoKeyStore()
	gated.EnableBridge = false
	off := forward.deviceWithKeyStore(t, gated, store)
	if off.forwardingHost() != nil {
		t.Fatal("a device behind the gate has a forwarding host")
	}
	if _, err := off.forwardingKeys(networkId); !errors.Is(err, errDeviceBridgeOff) {
		t.Errorf("a device behind the gate answered a setup: %v", err)
	}
	testingJoinDeviceRole(t, off)

	for _, directory := range []string{"", "relative/forward"} {
		settings := forward.settingsWithNoKeyStore()
		settings.ForwardStateDir = directory
		device := forward.deviceWithKeyStore(t, settings, store)
		if device.forwardingHost() != nil {
			t.Fatalf("a device with the directory %q has a forwarding host", directory)
		}
		if _, err := device.forwardingKeys(networkId); !errors.Is(err, errDeviceForwardingNone) {
			t.Errorf("a device with the directory %q answered a setup: %v", directory, err)
		}
		testingJoinDeviceRole(t, device)
	}
	if count := store.callCount.Load(); count != 0 {
		t.Errorf("the store took %d calls from devices that run no forwarding role", count)
	}
}

// Settings that are nil are an error, as NewDeviceLocal answers them, never a panic through a binding.
func TestNewDeviceLocalWithForwardKeyStoreNilSettings(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device, err := NewDeviceLocalWithForwardKeyStore(
		forward.role.space, forward.role.clientJwt, "device-role", "test", "0", forward.role.instanceId, nil, forward.store,
	)
	if err == nil || device != nil {
		t.Fatalf("nil settings built %v, %v", device, err)
	}
}
