package sdk

import (
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
)

func TestDeviceLocalNetworkChangedRebasesMultiAndKicksExactlyOnce(t *testing.T) {
	multi := &connect.RemoteUserNatMultiClient{}
	device := &DeviceLocal{remoteUserNatClient: multi}

	kicks := 0
	unregister := connect.AddNetworkChangeListener(func() { kicks++ })
	defer unregister()

	device.NetworkChanged()

	if kicks != 1 {
		t.Fatalf("NetworkChanged fired %d process kicks, want exactly 1", kicks)
	}
	// The timestamp is intentionally private to connect, but reflect.IsZero can
	// verify its state without bypassing package visibility or reading its value.
	freshSince := reflect.ValueOf(multi).Elem().FieldByName("uplinkFreshSince")
	if !freshSince.IsValid() || freshSince.IsZero() {
		t.Fatal("NetworkChanged did not route through the multi-client liveness rebase")
	}
}

func TestDeviceLocalNetworkChangedStillKicksWithoutMultiClient(t *testing.T) {
	device := &DeviceLocal{}
	kicks := 0
	unregister := connect.AddNetworkChangeListener(func() { kicks++ })
	defer unregister()

	device.NetworkChanged()

	if kicks != 1 {
		t.Fatalf("provider-only NetworkChanged fired %d kicks, want exactly 1", kicks)
	}
}

func TestNotifyNetworkChangeUsesCanonicalRecoverySeam(t *testing.T) {
	device := &DeviceLocal{}
	kicks := 0
	unregister := connect.AddNetworkChangeListener(func() { kicks++ })
	defer unregister()

	device.NotifyNetworkChange()

	if kicks != 1 {
		t.Fatalf("NotifyNetworkChange fired %d kicks, want canonical single kick", kicks)
	}
}
