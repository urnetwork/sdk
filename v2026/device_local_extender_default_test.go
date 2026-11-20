//go:build !ios && !android && !js

package sdk

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
)

// The provider extender setting where the space keeps no local state (F3): a
// headless embedder builds such a space, and the device holds the setting for
// its own life. The role follows it exactly as it follows a stored setting.

// Turning the setting off on a space with no local state stops the role and
// reports off, and turning it back on starts the role again.
func TestDeviceLocalProviderExtenderOptOutWithoutLocalState(t *testing.T) {
	fixture := newTestProvideExtenderFixtureWithSpace(t, newTestProvideExtenderUrlSpace, nil, nil)
	if fixture.networkSpace.asyncLocalState != nil {
		t.Fatal("the url-only space kept local state")
	}
	fixture.waitPass()
	if fixture.extender() == nil {
		t.Fatal("the role did not run with the setting on")
	}

	fixture.device.SetProvideExtender(false)
	if fixture.device.GetProvideExtender() {
		t.Fatal("the setting stayed on on a space with no local state")
	}
	if fixture.extender() != nil {
		t.Fatal("the opt-out left the role running")
	}
	if status := fixture.device.GetExtenderProvideStatus(); status.State != ExtenderProvideStateOff ||
		status.Enabled || status.Listening {
		t.Fatalf("status after the opt-out = %+v, expected off", status)
	}
	// the listener reports it as well
	fixture.waitStatus("off", func(status *ExtenderProvideStatus) bool {
		return status.State == ExtenderProvideStateOff
	})
	// the carriers are released, so the port binds again
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.tcpPort)))
	if err != nil {
		t.Fatalf("the opt-out did not release the tcp carrier: %v", err)
	}
	listener.Close()

	fixture.device.SetProvideExtender(true)
	if !fixture.device.GetProvideExtender() {
		t.Fatal("the setting did not go back on")
	}
	if fixture.extender() == nil {
		t.Fatal("turning the setting back on did not start the role")
	}
	fixture.waitStatus("listening again", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})
}

// With the device default off the role waits for the user: a providing device
// reports off and runs no role until the setting is turned on (F3, G2). Each
// case is its own test because the fixture turns the suite's role switches on,
// which a second fixture in one test would race with the first one's role.
func TestDeviceLocalProviderExtenderDeviceDefaultOffOnAStoredSpace(t *testing.T) {
	testProvideExtenderDeviceDefaultOff(t, nil)
}

// The same on a space that keeps no local state, where the device holds the
// setting it is then given.
func TestDeviceLocalProviderExtenderDeviceDefaultOffWithoutLocalState(t *testing.T) {
	testProvideExtenderDeviceDefaultOff(t, newTestProvideExtenderUrlSpace)
}

// The device-default-off case on the space newSpace builds; a nil newSpace
// takes the fixture's own space, which stores the setting.
func testProvideExtenderDeviceDefaultOff(
	t *testing.T,
	newSpace func(ctx context.Context) *NetworkSpace,
) {
	t.Helper()
	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newSpace,
		func(settings *DeviceLocalSettings) {
			settings.DefaultProvideExtender = false
		},
		nil,
	)
	if !fixture.device.GetProvideEnabled() {
		t.Fatal("the device is not providing")
	}
	if fixture.device.GetProvideExtender() {
		t.Fatal("the setting read on with the device default off")
	}
	if fixture.extender() != nil {
		t.Fatal("the role ran with the device default off")
	}
	fixture.waitStatus("off", func(status *ExtenderProvideStatus) bool {
		return status.State == ExtenderProvideStateOff
	})

	fixture.device.SetProvideExtender(true)
	if fixture.extender() == nil {
		t.Fatal("turning the setting on did not start the role")
	}
	fixture.waitPass()
	fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})
	// a space with local state stores the choice, where it outlives the device
	if asyncLocalState := fixture.networkSpace.asyncLocalState; asyncLocalState != nil {
		provideExtender, stored := asyncLocalState.GetLocalState().getStoredProvideExtender()
		if !stored || !provideExtender {
			t.Fatalf("stored = %t, %t, expected the choice stored", provideExtender, stored)
		}
	}
}

// A setting the space stores wins over the device default: stored on runs the
// role under a default of off (F3).
func TestDeviceLocalProviderExtenderStoredOnWinsOverTheDeviceDefault(t *testing.T) {
	testProvideExtenderStoredSettingWins(t, true, false)
}

// And stored off keeps the role off under a default of on.
func TestDeviceLocalProviderExtenderStoredOffWinsOverTheDeviceDefault(t *testing.T) {
	testProvideExtenderStoredSettingWins(t, false, true)
}

// A stored setting against a device default, on a stored space whose
// setting is written before the device exists.
func testProvideExtenderStoredSettingWins(
	t *testing.T,
	storedProvideExtender bool,
	defaultProvideExtender bool,
) {
	t.Helper()
	// the space the fixture builds when it builds its own, with the setting
	// stored before the device exists
	newSpace := func(ctx context.Context) *NetworkSpace {
		networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
		t.Cleanup(networkSpaceManager.Close)
		networkSpace := networkSpaceManager.updateNetworkSpace(
			NewNetworkSpaceKey(testProvideExtenderHost, "main"),
			func(values *NetworkSpaceValues) {},
		)
		localState := networkSpace.asyncLocalState.GetLocalState()
		if err := localState.SetProvideExtender(storedProvideExtender); err != nil {
			t.Fatal(err)
		}
		return networkSpace
	}
	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newSpace,
		func(settings *DeviceLocalSettings) {
			settings.DefaultProvideExtender = defaultProvideExtender
		},
		nil,
	)
	if !fixture.device.GetProvideEnabled() {
		t.Fatal("the device is not providing")
	}
	if provideExtender := fixture.device.GetProvideExtender(); provideExtender != storedProvideExtender {
		t.Fatalf("setting = %t, expected the stored value", provideExtender)
	}
	if running := fixture.extender() != nil; running != storedProvideExtender {
		t.Fatalf("role running = %t, expected the stored value", running)
	}
	if storedProvideExtender {
		fixture.waitPass()
	}
}

// The embedder's hard switch wins over everything: with it off the role never
// runs, though the device default is on and the user turned the setting on,
// which the space then stores (G1).
func TestDeviceLocalProviderExtenderHardSwitchWinsOnAStoredSpace(t *testing.T) {
	testProvideExtenderHardSwitchWins(t, nil)
}

// The same on a space that keeps no local state.
func TestDeviceLocalProviderExtenderHardSwitchWinsWithoutLocalState(t *testing.T) {
	testProvideExtenderHardSwitchWins(t, newTestProvideExtenderUrlSpace)
}

// The hard switch case on the space newSpace builds; a nil newSpace takes
// the fixture's own space, which stores the setting.
func testProvideExtenderHardSwitchWins(
	t *testing.T,
	newSpace func(ctx context.Context) *NetworkSpace,
) {
	t.Helper()
	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newSpace,
		func(settings *DeviceLocalSettings) {
			settings.ProvideExtenderEnabled = false
			settings.DefaultProvideExtender = true
		},
		nil,
	)
	if !fixture.device.GetProvideEnabled() {
		t.Fatal("the device is not providing")
	}
	fixture.device.SetProvideExtender(true)
	// the user's setting reads on: only the embedder's switch is off
	if !fixture.device.GetProvideExtender() {
		t.Fatal("the setting did not take")
	}
	if fixture.extender() != nil {
		t.Fatal("the role ran with the embedder's switch off")
	}
	status := fixture.device.GetExtenderProvideStatus()
	if status.State != ExtenderProvideStateNotProviding || status.Enabled || status.Listening {
		t.Fatalf("status = %+v, expected not providing", status)
	}
}

// A newer change of the setting is never undone by an older one that read the
// setting first and applied it last (G2): turning the setting off reads off,
// the newer change turns it back on and runs to the end before the off is
// applied, and the role must be running at the end.
func TestDeviceLocalProviderExtenderNewerOnIsAppliedLast(t *testing.T) {
	testProvideExtenderNewerChangeIsAppliedLast(t, true)
}

// And turning the setting on must not start the role over a newer off, which
// would leave the carriers open after the user turned the role off.
func TestDeviceLocalProviderExtenderNewerOffIsAppliedLast(t *testing.T) {
	testProvideExtenderNewerChangeIsAppliedLast(t, false)
}

// The older change sets the opposite of newerProvideExtender. The apply hook
// lands the newer change between the older change's read and its apply, in the
// older change's own goroutine, so the order is forced and nothing waits.
func testProvideExtenderNewerChangeIsAppliedLast(t *testing.T, newerProvideExtender bool) {
	t.Helper()
	var fixture *testProvideExtenderFixture
	// armed by the test right before the older change; the hook fires once
	var armed atomic.Bool
	var landed atomic.Bool
	fixture = newTestProvideExtenderFixtureWithSpace(
		t,
		newTestProvideExtenderUrlSpace,
		func(settings *DeviceLocalSettings) {
			// the older change starts from the newer change's state
			settings.DefaultProvideExtender = newerProvideExtender
			settings.testingBeforeExtenderProvideApply = func(enabled bool) {
				if enabled == newerProvideExtender || !armed.CompareAndSwap(true, false) {
					return
				}
				fixture.device.SetProvideExtender(newerProvideExtender)
				landed.Store(true)
			}
		},
		nil,
	)
	if !fixture.device.GetProvideEnabled() {
		t.Fatal("the device is not providing")
	}
	if running := fixture.extender() != nil; running != newerProvideExtender {
		t.Fatalf("role running = %t before the changes, expected %t", running, newerProvideExtender)
	}

	armed.Store(true)
	fixture.device.SetProvideExtender(!newerProvideExtender)
	if !landed.Load() {
		t.Fatal("the newer change did not land between the older change's read and its apply")
	}

	if provideExtender := fixture.device.GetProvideExtender(); provideExtender != newerProvideExtender {
		t.Fatalf("setting = %t, expected the newer %t", provideExtender, newerProvideExtender)
	}
	if running := fixture.extender() != nil; running != newerProvideExtender {
		t.Fatalf("role running = %t after the older change applied, expected %t from the newer setting",
			running, newerProvideExtender)
	}
	status := fixture.device.GetExtenderProvideStatus()
	if newerProvideExtender {
		if !status.Enabled {
			t.Fatalf("status = %+v, expected the role running", status)
		}
		fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
			return status.Enabled && status.Listening
		})
	} else {
		if status.Enabled || status.State != ExtenderProvideStateOff {
			t.Fatalf("status = %+v, expected off", status)
		}
		// the carriers are released, so the port binds again
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.tcpPort)))
		if err != nil {
			t.Fatalf("the role kept the tcp carrier: %v", err)
		}
		listener.Close()
	}
}
