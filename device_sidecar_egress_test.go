// The device role's bridge settings by platform and §17.9's egress spec on app devices (device_sidecar_egress.go;
// BRIDGE-M8-02): LinkCountMax 0 in the iOS tunnel, a phone's elsewhere on phones and the desktop's on desktops; the
// egress spec off by default, so a device under none or exits keeps its own route; and on, a device under none sends
// nothing to its own route, the packet answered with a reject into the tun, and one under exits sends nothing there
// either, since with no link it may build the exit rejects. Devices run sidecar's posture vectors (device_sidecar_test.go).

package sdk

import (
	"testing"

	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// LinkCountMax per platform (§17.5's memory row): none in the iOS tunnel until §22.9 measures a link there (owner
// decision 100), a phone's 8 on Android and on any low-memory device, the desktop's 64 elsewhere, each beside its pin
// table; and every platform's egress spec off until EnableBridgeEgress.
func TestBridgeLinkCountMaxPerPlatform(t *testing.T) {
	cases := []struct {
		goos         string
		lowMemory    bool
		linkCountMax int
		pinSlotCount int
	}{
		{goos: "ios", linkCountMax: 0, pinSlotCount: 1024},
		{goos: "ios", lowMemory: true, linkCountMax: 0, pinSlotCount: 1024},
		{goos: "android", linkCountMax: 8, pinSlotCount: 1024},
		{goos: "linux", lowMemory: true, linkCountMax: 8, pinSlotCount: 1024},
		{goos: "darwin", linkCountMax: 64, pinSlotCount: 8192},
		{goos: "windows", linkCountMax: 64, pinSlotCount: 8192},
		{goos: "linux", linkCountMax: 64, pinSlotCount: 8192},
	}
	for _, testCase := range cases {
		settings := DefaultDeviceLocalSettings()
		settings.LowMemoryProfile = testCase.lowMemory
		bridgeSettings := deviceLocalBridgeSettingsFor(settings, testCase.goos)
		if bridgeSettings.LinkCountMax != testCase.linkCountMax || bridgeSettings.PinSlotCount != testCase.pinSlotCount {
			t.Errorf("%s low memory %t: %d links, %d pin slots", testCase.goos, testCase.lowMemory, bridgeSettings.LinkCountMax,
				bridgeSettings.PinSlotCount)
		}
		if !bridgeSettings.EgressSpecOff || bridgeSettings.PolicyProfile != sidecardevice.BridgePolicyProfileSdkProvider {
			t.Errorf("%s: egress off %t, table %v", testCase.goos, bridgeSettings.EgressSpecOff, bridgeSettings.PolicyProfile)
		}
		settings.EnableBridgeEgress = true
		if deviceLocalBridgeSettingsFor(settings, testCase.goos).EgressSpecOff {
			t.Errorf("%s: EnableBridgeEgress leaves the spec off", testCase.goos)
		}
	}
	if DefaultDeviceLocalSettings().EnableBridgeEgress {
		t.Error("the egress spec is on by default")
	}
}

// What a packet to an address outside the block does on a device enforcing a state whose egress spec is mode, with
// app egress on or off: how many packets its own route took, and how many rejects the bridge answered with.
func testingEgressOutcome(t *testing.T, mode bridge.BridgeEgressMode, egress bool) (routeCount int, rejectCount uint64) {
	t.Helper()
	role := newTestingDeviceRole(t)
	device := role.device(t, func(settings *DeviceLocalSettings) {
		settings.EnableBridgeEgress = egress
	})
	rules := testingBridgeRules()
	rules.Egress = &bridge.BridgeRuleEgress{Mode: mode}
	if mode == bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_EXITS {
		rules.Egress.Exits = []*bridge.BridgeRuleLink{{
			TargetClientId:          []byte("exit-client-id16"),
			TargetSidecarId:         []byte("exit-sidecar-id1"),
			TargetIdentityPublicKey: make([]byte, 32),
			RuleId:                  make([]byte, 32),
			Weight:                  1,
		}}
	}
	role.enforce(t, device, rules)
	held := func() deviceBridge {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.bridge
	}()
	if held == nil {
		t.Fatal("CONTROL FAILED: the device holds no bridge")
	}
	before := held.Status()
	route := &testingCountingRoute{}
	sender := testingDeviceRoleSendDevice(route, held)
	for range 3 {
		sender.sendPacket(testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte("x")))
	}
	after := held.Status()
	route.stateLock.Lock()
	defer route.stateLock.Unlock()
	return route.packetCount, after.RejectSentCount + after.RejectUnsentCount - before.RejectSentCount - before.RejectUnsentCount
}

// Off, the default: under none and under exits a device's own route takes what no entry holds, as with no spec.
func TestBridgeEgressOffKeepsTheDevicesRoute(t *testing.T) {
	for _, mode := range []bridge.BridgeEgressMode{bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_NONE, bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_EXITS} {
		routeCount, rejectCount := testingEgressOutcome(t, mode, false)
		if routeCount != 3 || rejectCount != 0 {
			t.Errorf("%v off: own route took %d, %d rejects", mode, routeCount, rejectCount)
		}
	}
}

// On: under none nothing reaches the device's own route and each packet is a reject; under exits nothing reaches it
// either, and with no link this device role may build to the exit each is a reject too.
func TestBridgeEgressOnSendsOnlyToAnExitOrNowhere(t *testing.T) {
	for _, mode := range []bridge.BridgeEgressMode{bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_NONE, bridge.BridgeEgressMode_BRIDGE_EGRESS_MODE_EXITS} {
		routeCount, rejectCount := testingEgressOutcome(t, mode, true)
		if routeCount != 0 || rejectCount != 3 {
			t.Errorf("%v on: own route took %d, %d rejects", mode, routeCount, rejectCount)
		}
	}
	// the same device with no spec in its state keeps its own route, so the rejects above are the spec's
	role := newTestingDeviceRole(t)
	device := role.device(t, func(settings *DeviceLocalSettings) {
		settings.EnableBridgeEgress = true
	})
	role.enforce(t, device, testingBridgeRules())
	held := func() deviceBridge {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.bridge
	}()
	route := &testingCountingRoute{}
	if !testingDeviceRoleSendDevice(route, held).sendPacket(testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte("x"))) {
		t.Fatal("no spec: the device's own route refused the packet")
	}
	connect.AssertEqual(t, route.packetCount, 1)
}
