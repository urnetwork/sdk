package sdk

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A stay pinned to an extender (EXTENDER.md K10): the pin rides the location's
// one fixed spec as its transport hint, changes the installed transport,
// survives storage and the device rpc, and is offered only against a device
// process that keeps it.

func testPinnedStayLocation() (*ConnectLocation, connect.Id) {
	clientId := connect.NewId()
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{ClientId: newId(clientId)},
		Name:              "pinned",
		ExtenderIp:        "203.0.113.7",
	}, clientId
}

func TestConnectLocationPinRidesItsFixedSpec(t *testing.T) {
	location, clientId := testPinnedStayLocation()
	otherId := connect.NewId()
	specs := []*connect.ProviderSpec{{ClientId: &clientId}, {ClientId: &otherId}}
	setConnectLocationTransportHint(location, specs)
	if hint := specs[0].TransportHint; hint == nil || hint.ExtenderIp.String() != "203.0.113.7" {
		t.Fatalf("the stayed-on exit's hint = %+v", hint)
	}
	if hint := specs[1].TransportHint; hint != nil {
		t.Fatalf("another spec got the hint %+v", hint)
	}

	// no pin, a pin that does not parse, and a location without a client id
	// leave the specs as they are
	for name, unpin := range map[string]func(*ConnectLocation){
		"no pin":       func(location *ConnectLocation) { location.ExtenderIp = "" },
		"unparsable":   func(location *ConnectLocation) { location.ExtenderIp = "not an address" },
		"no client id": func(location *ConnectLocation) { location.ConnectLocationId = &ConnectLocationId{BestAvailable: true} },
	} {
		location, clientId := testPinnedStayLocation()
		unpin(location)
		specs := []*connect.ProviderSpec{{ClientId: &clientId}}
		setConnectLocationTransportHint(location, specs)
		if hint := specs[0].TransportHint; hint != nil {
			t.Fatalf("%s: the spec got the hint %+v", name, hint)
		}
	}
}

// A pin set or cleared on the same exit rebuilds the destination: the exit is
// dialed differently.
func TestConnectLocationPinChangesTheTransport(t *testing.T) {
	pinned, _ := testPinnedStayLocation()
	unpinned := cloneConnectLocation(pinned)
	unpinned.ExtenderIp = ""
	if connectLocationTransportEqual(pinned, unpinned) {
		t.Fatal("a pin change kept the installed transport")
	}
	if connectLocationValuesEqual(pinned, unpinned) {
		t.Fatal("a pin change kept the location's value")
	}
	if !connectLocationTransportEqual(pinned, cloneConnectLocation(pinned)) {
		t.Fatal("the same pin rebuilt the transport")
	}
}

// Saved with the location, and absent from an unpinned one, so the stored
// schema of every other location is unchanged.
func TestConnectLocationPinIsSaved(t *testing.T) {
	location, _ := testPinnedStayLocation()
	locationBytes, err := json.Marshal(location)
	if err != nil {
		t.Fatal(err)
	}
	var restored ConnectLocation
	if err := json.Unmarshal(locationBytes, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ExtenderIp != location.ExtenderIp {
		t.Fatalf("restored pin = %q, want %q", restored.ExtenderIp, location.ExtenderIp)
	}

	location.ExtenderIp = ""
	locationBytes, err = json.Marshal(location)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(locationBytes, []byte("extender_ip")) {
		t.Fatalf("an unpinned location wrote the pin: %s", locationBytes)
	}
}

func TestConnectLocationPinCrossesTheDeviceRpc(t *testing.T) {
	location, _ := testPinnedStayLocation()
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(newDeviceRemoteConnectLocationValue(location)); err != nil {
		t.Fatal(err)
	}
	var decoded DeviceRemoteConnectLocationValue
	if err := gob.NewDecoder(&buffer).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if pin := decoded.toConnectLocation().ExtenderIp; pin != location.ExtenderIp {
		t.Fatalf("the device rpc carried the pin as %q, want %q", pin, location.ExtenderIp)
	}
}

type testingExtenderPinRpc struct {
	supports bool
}

func (self *testingExtenderPinRpc) GetSupportsExtenderPin(_ RpcNoArg, supports *bool) error {
	*supports = self.supports
	return nil
}

func TestDeviceRemoteExtenderPinSupport(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	if deviceRemote.GetSupportsExtenderPin() {
		t.Fatal("a remote that never reached a device process offered the pin")
	}
	testExtenderProvideDeviceProcess(t, deviceRemote, &testingExtenderPinRpc{supports: true})
	if !deviceRemote.GetSupportsExtenderPin() {
		t.Fatal("a device process that keeps the pin was not offered it")
	}
}

// A device process downgraded under a running app answers the missing method:
// the remote stops offering the pin and keeps its session.
func TestDeviceRemoteExtenderPinMissingMethodKeepsTheSession(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	testExtenderProvideDeviceProcess(t, deviceRemote, &testingExtenderPinRpc{supports: true})
	if !deviceRemote.GetSupportsExtenderPin() {
		t.Fatal("the newer device process was not offered the pin")
	}

	testExtenderProvideDeviceProcess(t, deviceRemote, &testingOptionalMethodRpc{})
	if deviceRemote.GetSupportsExtenderPin() {
		t.Fatal("an older device process was offered the pin")
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the missing method tore the rpc session down")
	}
}

// A hosted device never dials an extender (K8), so it keeps no pin.
func TestHostedDeviceKeepsNoExtenderPin(t *testing.T) {
	device := &DeviceLocal{settings: &DeviceLocalSettings{HostedIncompatible: true}}
	if device.GetSupportsExtenderPin() {
		t.Fatal("a hosted device offered the pin")
	}
	device.settings.HostedIncompatible = false
	if !device.GetSupportsExtenderPin() {
		t.Fatal("a device that dials extenders refused the pin")
	}
}
