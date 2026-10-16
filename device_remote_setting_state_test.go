// A setting write on a DeviceRemote (the provide mode, the provide control
// mode, provide paused, offline, the vpn interface while offline, route local,
// the blocker) shows to its listeners without waiting for the device to report
// it back, and a report the device made before it had the write does not undo
// it, as a connect tap does (device_remote_connect_state_test.go, owner report
// 2026-10-16). The fixtures are the connect tests': the remote's lifecycle
// parks in a blocked dial, and the tests drive the reverse rpc handlers and a
// fake device service directly, so every ordering below is a barrier, never a
// timing. Timers only bound a broken run.
//
// Every setting runs through the same orderings, so each test loops over
// testingRemoteSettings and names the setting in its failures.
package sdk

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Records the setting events in arrival order.
type testingSettingRecorder struct {
	stateLock sync.Mutex
	events    []string
}

func (self *testingSettingRecorder) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *testingSettingRecorder) get() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

func (self *testingSettingRecorder) last() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.events) == 0 {
		return ""
	}
	return self.events[len(self.events)-1]
}

func (self *testingSettingRecorder) ProvideModeChanged(provideMode ProvideMode) {
	self.add("provideMode:" + testingProvideModeName(provideMode))
}

func (self *testingSettingRecorder) ProvideControlModeChanged(provideControlMode ProvideControlMode) {
	self.add("provideControlMode:" + provideControlMode)
}

func (self *testingSettingRecorder) ProvidePausedChanged(providePaused bool) {
	self.add(fmt.Sprintf("providePaused:%t", providePaused))
}

func (self *testingSettingRecorder) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.add(fmt.Sprintf("offline:%t/%t", offline, vpnInterfaceWhileOffline))
}

func (self *testingSettingRecorder) VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool) {
	self.add(fmt.Sprintf("vpnInterfaceWhileOffline:%t", vpnInterfaceWhileOffline))
}

func (self *testingSettingRecorder) RouteLocalChanged(routeLocal bool) {
	self.add(fmt.Sprintf("routeLocal:%t", routeLocal))
}

func (self *testingSettingRecorder) BlockerEnabledChanged(blockerEnabled bool) {
	self.add(fmt.Sprintf("blockerEnabled:%t", blockerEnabled))
}

// The name a provide mode is spelled with in the tests.
func testingProvideModeName(provideMode ProvideMode) string {
	switch provideMode {
	case ProvideModeNone:
		return "none"
	case ProvideModeNetwork:
		return "network"
	case ProvideModeFriendsAndFamily:
		return "friendsAndFamily"
	case ProvideModePublic:
		return "public"
	default:
		return fmt.Sprintf("%d", provideMode)
	}
}

// The provide mode a test name spells.
func testingProvideMode(name string) ProvideMode {
	switch name {
	case "network":
		return ProvideModeNetwork
	case "friendsAndFamily":
		return ProvideModeFriendsAndFamily
	case "public":
		return ProvideModePublic
	default:
		return ProvideModeNone
	}
}

// One setting of a DeviceRemote, for the orderings every setting runs through.
// A value is spelled as a string: true or false, a provide mode name, or a
// provide control mode.
type testingRemoteSetting struct {
	name string
	// the device's value at the start, and the value the tests write
	initial string
	tapped  string
	// the event its listeners record for a value
	event       func(value string) string
	write       func(deviceRemote *DeviceRemote, value string)
	get         func(deviceRemote *DeviceRemote) string
	addListener func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub
	// the device's report, through the production reverse rpc handler
	report func(deviceRemoteRpc *DeviceRemoteRpc, value string) error
	// the device process's own setter and getter
	deviceSet func(deviceLocal *DeviceLocal, value string)
	deviceGet func(deviceLocal *DeviceLocal) string
}

// A setting whose value is a bool, moving from false to true.
func testingBoolSetting(
	name string,
	listenerName string,
	write func(*DeviceRemote, bool),
	get func(*DeviceRemote) bool,
	addListener func(*DeviceRemote, *testingSettingRecorder) Sub,
	report func(*DeviceRemoteRpc, bool) error,
	deviceSet func(*DeviceLocal, bool),
	deviceGet func(*DeviceLocal) bool,
) *testingRemoteSetting {
	return &testingRemoteSetting{
		name:    name,
		initial: "false",
		tapped:  "true",
		event: func(value string) string {
			return listenerName + ":" + value
		},
		write: func(deviceRemote *DeviceRemote, value string) {
			write(deviceRemote, value == "true")
		},
		get: func(deviceRemote *DeviceRemote) string {
			return fmt.Sprintf("%t", get(deviceRemote))
		},
		addListener: addListener,
		report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
			return report(deviceRemoteRpc, value == "true")
		},
		deviceSet: func(deviceLocal *DeviceLocal, value string) {
			deviceSet(deviceLocal, value == "true")
		},
		deviceGet: func(deviceLocal *DeviceLocal) string {
			return fmt.Sprintf("%t", deviceGet(deviceLocal))
		},
	}
}

// Every setting the remote tells as it tells its connect state.
func testingRemoteSettings() []*testingRemoteSetting {
	return []*testingRemoteSetting{
		{
			// the remote's default control mode is manual, so the provide mode
			// the getters derive is the mode written
			name:    "ProvideMode",
			initial: "network",
			tapped:  "public",
			event: func(value string) string {
				return "provideMode:" + value
			},
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetProvideMode(testingProvideMode(value))
			},
			get: func(deviceRemote *DeviceRemote) string {
				return testingProvideModeName(deviceRemote.GetProvideMode())
			},
			addListener: func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddProvideModeChangeListener(recorder)
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.ProvideModeChanged(testingProvideMode(value), nil)
			},
			deviceSet: func(deviceLocal *DeviceLocal, value string) {
				deviceLocal.SetProvideMode(testingProvideMode(value))
			},
			deviceGet: func(deviceLocal *DeviceLocal) string {
				return testingProvideModeName(deviceLocal.GetProvideMode())
			},
		},
		{
			name:    "ProvideControlMode",
			initial: ProvideControlModeNever,
			tapped:  ProvideControlModeAlways,
			event: func(value string) string {
				return "provideControlMode:" + value
			},
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetProvideControlMode(value)
			},
			get: func(deviceRemote *DeviceRemote) string {
				return deviceRemote.GetProvideControlMode()
			},
			addListener: func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddProvideControlModeChangeListener(recorder)
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.ProvideControlModeChanged(value, nil)
			},
			deviceSet: func(deviceLocal *DeviceLocal, value string) {
				deviceLocal.SetProvideControlMode(value)
			},
			deviceGet: func(deviceLocal *DeviceLocal) string {
				return deviceLocal.GetProvideControlMode()
			},
		},
		testingBoolSetting(
			"ProvidePaused",
			"providePaused",
			(*DeviceRemote).SetProvidePaused,
			(*DeviceRemote).GetProvidePaused,
			func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddProvidePausedChangeListener(recorder)
			},
			func(deviceRemoteRpc *DeviceRemoteRpc, providePaused bool) error {
				return deviceRemoteRpc.ProvidePausedChanged(providePaused, nil)
			},
			(*DeviceLocal).SetProvidePaused,
			(*DeviceLocal).GetProvidePaused,
		),
		{
			// the offline listeners also hear the vpn interface while offline,
			// which stays off
			name:    "Offline",
			initial: "false",
			tapped:  "true",
			event: func(value string) string {
				return "offline:" + value + "/false"
			},
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetOffline(value == "true")
			},
			get: func(deviceRemote *DeviceRemote) string {
				return fmt.Sprintf("%t", deviceRemote.GetOffline())
			},
			addListener: func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddOfflineChangeListener(recorder)
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.OfflineChanged(&DeviceRemoteOfflineChangeEvent{
					Offline:                  value == "true",
					VpnInterfaceWhileOffline: false,
				}, nil)
			},
			deviceSet: func(deviceLocal *DeviceLocal, value string) {
				deviceLocal.SetOffline(value == "true")
			},
			deviceGet: func(deviceLocal *DeviceLocal) string {
				return fmt.Sprintf("%t", deviceLocal.GetOffline())
			},
		},
		testingBoolSetting(
			"VpnInterfaceWhileOffline",
			"vpnInterfaceWhileOffline",
			(*DeviceRemote).SetVpnInterfaceWhileOffline,
			(*DeviceRemote).GetVpnInterfaceWhileOffline,
			func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddVpnInterfaceWhileOfflineChangeListener(recorder)
			},
			func(deviceRemoteRpc *DeviceRemoteRpc, vpnInterfaceWhileOffline bool) error {
				return deviceRemoteRpc.VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline, nil)
			},
			(*DeviceLocal).SetVpnInterfaceWhileOffline,
			(*DeviceLocal).GetVpnInterfaceWhileOffline,
		),
		testingBoolSetting(
			"RouteLocal",
			"routeLocal",
			(*DeviceRemote).SetRouteLocal,
			(*DeviceRemote).GetRouteLocal,
			func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddRouteLocalChangeListener(recorder)
			},
			func(deviceRemoteRpc *DeviceRemoteRpc, routeLocal bool) error {
				return deviceRemoteRpc.RouteLocalChanged(routeLocal, nil)
			},
			(*DeviceLocal).SetRouteLocal,
			(*DeviceLocal).GetRouteLocal,
		),
		testingBoolSetting(
			"BlockerEnabled",
			"blockerEnabled",
			(*DeviceRemote).SetBlockerEnabled,
			(*DeviceRemote).GetBlockerEnabled,
			func(deviceRemote *DeviceRemote, recorder *testingSettingRecorder) Sub {
				return deviceRemote.AddBlockerEnabledChangeListener(recorder)
			},
			func(deviceRemoteRpc *DeviceRemoteRpc, blockerEnabled bool) error {
				return deviceRemoteRpc.BlockerEnabledChanged(blockerEnabled, nil)
			},
			(*DeviceLocal).SetBlockerEnabled,
			(*DeviceLocal).GetBlockerEnabled,
		),
	}
}

// The setting of testingRemoteSettings with the name.
func testingRemoteSettingNamed(t *testing.T, name string) *testingRemoteSetting {
	t.Helper()
	for _, setting := range testingRemoteSettings() {
		if setting.name == name {
			return setting
		}
	}
	t.Fatalf("no setting %s", name)
	return nil
}

// Delivers a device report of the setting through the production reverse rpc
// handler and returns once the remote has handled it: callbacks run serially,
// so a later barrier callback runs after the report.
func testingReportSetting(t *testing.T, deviceRemote *DeviceRemote, setting *testingRemoteSetting, value string) {
	t.Helper()
	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()

	if err := setting.report(deviceRemoteRpc, value); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() {
		close(handled)
	})
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: the reverse report was not handled", setting.name)
	}
}

// Takes the remote's pending state, as takeSyncRequest does when a sync starts.
func testingTakeSettingSync(t *testing.T, deviceRemote *DeviceRemote) {
	t.Helper()
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	if !deviceRemote.state.hasPendingSyncState() {
		t.Fatal("the sync took no write")
	}
	deviceRemote.state = DeviceRemoteState{}
}

// Reports whether the listeners heard exactly the events of the values, and
// fails the test with the setting's name otherwise.
func testingSettingHeard(t *testing.T, setting *testingRemoteSetting, recorder *testingSettingRecorder, values ...string) bool {
	t.Helper()
	want := []string{}
	for _, value := range values {
		want = append(want, setting.event(value))
	}
	if events := recorder.get(); !slices.Equal(events, want) {
		t.Errorf("%s: the listeners heard %v, want %v", setting.name, events, want)
		return false
	}
	return true
}

// Reports whether the getter answers the value, and fails the test with the
// setting's name otherwise.
func testingSettingGets(t *testing.T, setting *testingRemoteSetting, deviceRemote *DeviceRemote, value string, when string) bool {
	t.Helper()
	if got := setting.get(deviceRemote); got != value {
		t.Errorf("%s: %s the getter answers %s, want %s", setting.name, when, got, value)
		return false
	}
	return true
}

// A remote whose lifecycle never reaches a device, with one listener of the
// setting.
func testingSettingRemote(t *testing.T, setting *testingRemoteSetting) (*DeviceRemote, *testingSettingRecorder) {
	t.Helper()
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingSettingRecorder{}
	sub := setting.addListener(deviceRemote, recorder)
	t.Cleanup(sub.Close)
	return deviceRemote, recorder
}

// A fake live device that holds each setting by name, answers the getters
// from it, applies writes to it, and registers listeners without replaying the
// current state, as DeviceLocalRpc does while the rpc is live. While armed, a
// write blocks on a barrier before the device applies it.
type testingSettingFakeDevice struct {
	stateLock   sync.Mutex
	values      map[string]string
	writes      []string
	armed       bool
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

// A fake device that holds no write.
func testingNewSettingFakeDevice() *testingSettingFakeDevice {
	return &testingSettingFakeDevice{
		values:  map[string]string{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Holds the next write on the barrier.
func (self *testingSettingFakeDevice) arm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = true
}

func (self *testingSettingFakeDevice) releaseWrites() {
	self.releaseOnce.Do(func() { close(self.release) })
}

func (self *testingSettingFakeDevice) set(name string, value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.values[name] = value
}

func (self *testingSettingFakeDevice) value(name string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.values[name]
}

func (self *testingSettingFakeDevice) getWrites() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writes)
}

// Takes a write of the setting, held on the barrier while armed.
func (self *testingSettingFakeDevice) write(name string, value string) error {
	armed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writes = append(self.writes, name+":"+value)
		return self.armed
	}()
	if armed {
		self.enteredOnce.Do(func() { close(self.entered) })
		<-self.release
	}
	self.set(name, value)
	return nil
}

func (self *testingSettingFakeDevice) SetProvideMode(provideMode ProvideMode, _ RpcVoid) error {
	return self.write("ProvideMode", testingProvideModeName(provideMode))
}

func (self *testingSettingFakeDevice) GetProvideMode(_ RpcNoArg, provideMode *ProvideMode) error {
	*provideMode = testingProvideMode(self.value("ProvideMode"))
	return nil
}

func (self *testingSettingFakeDevice) SetProvideControlMode(provideControlMode ProvideControlMode, _ RpcVoid) error {
	return self.write("ProvideControlMode", provideControlMode)
}

func (self *testingSettingFakeDevice) GetProvideControlMode(_ RpcNoArg, provideControlMode *ProvideControlMode) error {
	*provideControlMode = self.value("ProvideControlMode")
	return nil
}

func (self *testingSettingFakeDevice) SetProvidePaused(providePaused bool, _ RpcVoid) error {
	return self.write("ProvidePaused", fmt.Sprintf("%t", providePaused))
}

func (self *testingSettingFakeDevice) GetProvidePaused(_ RpcNoArg, providePaused *bool) error {
	*providePaused = self.value("ProvidePaused") == "true"
	return nil
}

func (self *testingSettingFakeDevice) SetOffline(offline bool, _ RpcVoid) error {
	return self.write("Offline", fmt.Sprintf("%t", offline))
}

func (self *testingSettingFakeDevice) GetOffline(_ RpcNoArg, offline *bool) error {
	*offline = self.value("Offline") == "true"
	return nil
}

func (self *testingSettingFakeDevice) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool, _ RpcVoid) error {
	return self.write("VpnInterfaceWhileOffline", fmt.Sprintf("%t", vpnInterfaceWhileOffline))
}

func (self *testingSettingFakeDevice) GetVpnInterfaceWhileOffline(_ RpcNoArg, vpnInterfaceWhileOffline *bool) error {
	*vpnInterfaceWhileOffline = self.value("VpnInterfaceWhileOffline") == "true"
	return nil
}

func (self *testingSettingFakeDevice) SetRouteLocal(routeLocal bool, _ RpcVoid) error {
	return self.write("RouteLocal", fmt.Sprintf("%t", routeLocal))
}

func (self *testingSettingFakeDevice) GetRouteLocal(_ RpcNoArg, routeLocal *bool) error {
	*routeLocal = self.value("RouteLocal") == "true"
	return nil
}

func (self *testingSettingFakeDevice) SetBlockerEnabled(blockerEnabled bool, _ RpcVoid) error {
	return self.write("BlockerEnabled", fmt.Sprintf("%t", blockerEnabled))
}

func (self *testingSettingFakeDevice) GetBlockerEnabled(_ RpcNoArg, blockerEnabled *bool) error {
	*blockerEnabled = self.value("BlockerEnabled") == "true"
	return nil
}

func (self *testingSettingFakeDevice) AddProvideModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveProvideModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddProvideControlModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveProvideControlModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddProvidePausedChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveProvidePausedChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddRouteLocalChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveRouteLocalChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) AddBlockerEnabledChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingSettingFakeDevice) RemoveBlockerEnabledChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

// Publishes the fake device as the remote's live service, as a completed sync
// does. The remote's own lifecycle stays parked in its blocked dial.
func testingInstallSettingFakeDevice(t *testing.T, deviceRemote *DeviceRemote, device *testingSettingFakeDevice) {
	t.Helper()
	testingInstallFakeDeviceService(t, deviceRemote, device)
	// before the server joins, so a held write cannot pin it
	t.Cleanup(device.releaseWrites)
}

// With no rpc the write reaches the listeners before it returns. A report the
// device made before it had the pending write (the replay of a sync that
// started before the write) must not flip the listeners back, and the
// device's report of the write it applied is not told a second time.
func TestDeviceRemoteSettingWriteSurvivesStaleDeviceReport(t *testing.T) {
	for _, setting := range testingRemoteSettings() {
		func() {
			deviceRemote, recorder := testingSettingRemote(t, setting)
			// the device's state before the write
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			if !testingSettingHeard(t, setting, recorder, setting.initial) {
				return
			}

			setting.write(deviceRemote, setting.tapped)
			if !testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped) ||
				!testingSettingGets(t, setting, deviceRemote, setting.tapped, "after the write") {
				return
			}

			// the replay of a sync that started before the write
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			if !testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped) ||
				!testingSettingGets(t, setting, deviceRemote, setting.tapped, "after a stale report") {
				return
			}

			// the sync delivers the write; while it is in flight the getter
			// still answers with it
			testingTakeSettingSync(t, deviceRemote)
			if !testingSettingGets(t, setting, deviceRemote, setting.tapped, "while the sync is in flight") {
				return
			}
			// the device reports what it applied
			testingReportSetting(t, deviceRemote, setting, setting.tapped)
			testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped)
			testingSettingGets(t, setting, deviceRemote, setting.tapped, "after the device applied the write")
		}()
	}
}

// With a live rpc the listeners hear the write as soon as the device applied
// it, from the write itself, and not only when the device's report comes back
// over the reverse rpc. Nothing is told while the device still holds the
// write. The device's echo of the applied write is not told again, and a later
// change of the device's own is.
func TestDeviceRemoteSettingWriteNotifiesOnceDeviceApplies(t *testing.T) {
	for _, setting := range testingRemoteSettings() {
		func() {
			deviceRemote, recorder := testingSettingRemote(t, setting)
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			device := testingNewSettingFakeDevice()
			device.set(setting.name, setting.initial)
			device.arm()
			testingInstallSettingFakeDevice(t, deviceRemote, device)

			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				setting.write(deviceRemote, setting.tapped)
			}()
			select {
			case <-device.entered:
			case <-time.After(15 * time.Second):
				t.Fatalf("%s: the write never reached the device", setting.name)
			}
			if !testingSettingHeard(t, setting, recorder, setting.initial) {
				device.releaseWrites()
				<-writeDone
				return
			}

			device.releaseWrites()
			select {
			case <-writeDone:
			case <-time.After(15 * time.Second):
				t.Fatalf("%s: the write did not return after the device applied it", setting.name)
			}
			// told by the write, before any report from the device
			if !testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped) {
				return
			}
			if writes := device.getWrites(); !slices.Equal(writes, []string{setting.name + ":" + setting.tapped}) {
				t.Errorf("%s: the device took the writes %v", setting.name, writes)
				return
			}

			// the device's echo of the applied write
			testingReportSetting(t, deviceRemote, setting, setting.tapped)
			if !testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped) {
				return
			}

			// a later change of the device's own
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			testingSettingHeard(t, setting, recorder, setting.initial, setting.tapped, setting.initial)
		}()
	}
}

// Writes queued while the rpc is down reach the device in one sync, so it
// replays only the final value. A change the device later makes on its own
// (another controller, the extension's own change) must still reach the
// listeners.
func TestDeviceRemoteSettingDeviceChangeAfterQueuedWritesIsTold(t *testing.T) {
	for _, setting := range testingRemoteSettings() {
		func() {
			deviceRemote, recorder := testingSettingRemote(t, setting)
			testingReportSetting(t, deviceRemote, setting, setting.initial)

			setting.write(deviceRemote, setting.tapped)
			setting.write(deviceRemote, setting.initial)
			setting.write(deviceRemote, setting.tapped)

			testingTakeSettingSync(t, deviceRemote)
			// the sync's replay of the value the device applied
			testingReportSetting(t, deviceRemote, setting, setting.tapped)
			if last := recorder.last(); last != setting.event(setting.tapped) {
				t.Errorf("%s: the listeners last heard %q of %v after the replay", setting.name, last, recorder.get())
				return
			}

			// later, the device changes it on its own
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			if last := recorder.last(); last != setting.event(setting.initial) {
				t.Errorf("%s: the listeners last heard %q of %v after the device's change", setting.name, last, recorder.get())
				return
			}
			testingSettingGets(t, setting, deviceRemote, setting.initial, "after the device's change")
		}()
	}
}

// While the remote has no listener of a setting the device pushes nothing, so
// what the remote last told can go stale. A consumer that registers later and
// seeds itself from the getter, as the apps' view models do, must still hear
// the device's next change.
func TestDeviceRemoteSettingListenerGapThenDeviceChangeIsTold(t *testing.T) {
	for _, setting := range testingRemoteSettings() {
		func() {
			deviceRemote := testingConnectStateRemoteWithoutListeners(t)
			device := testingNewSettingFakeDevice()
			device.set(setting.name, setting.initial)
			testingInstallSettingFakeDevice(t, deviceRemote, device)

			// a first consumer; the user changes the setting and back
			first := &testingSettingRecorder{}
			firstSub := setting.addListener(deviceRemote, first)
			setting.write(deviceRemote, setting.tapped)
			setting.write(deviceRemote, setting.initial)
			// the device's echo
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			if last := first.last(); last != setting.event(setting.initial) {
				t.Errorf("%s: the first consumer last heard %q of %v", setting.name, last, first.get())
				firstSub.Close()
				return
			}

			// the consumer goes away
			firstSub.Close()

			// with no remote listener the device changes it on its own and
			// pushes nothing
			device.set(setting.name, setting.tapped)

			// a new consumer registers and seeds itself from the getter
			second := &testingSettingRecorder{}
			secondSub := setting.addListener(deviceRemote, second)
			defer secondSub.Close()
			second.add(setting.event(setting.get(deviceRemote)))
			if last := second.last(); last != setting.event(setting.tapped) {
				t.Errorf("%s: the new consumer seeded %q", setting.name, last)
				return
			}

			// the device changes it back on its own and reports it
			device.set(setting.name, setting.initial)
			testingReportSetting(t, deviceRemote, setting, setting.initial)
			if last := second.last(); last != setting.event(setting.initial) {
				t.Errorf("%s: the new consumer last heard %q of %v; the device is at %s", setting.name, last, second.get(), setting.initial)
			}
		}()
	}
}

// End to end over the device rpc. A write lands while the first sync is in
// flight, so it is pending, and the first sync's replay of the device's older
// value is dropped. The first sync's reply must not replace the pending write
// as last known: the follow-up sync takes the write, and a view opened while
// it is in flight seeds itself from the getter. The listeners must never hear
// the device's older value once the write was told, and the view must end at
// the written value.
func TestDeviceRemoteSettingViewOpenedDuringFollowUpSync(t *testing.T) {
	for _, setting := range testingRemoteSettings() {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
			if err != nil {
				t.Fatal(err)
			}
			clientId := connect.NewId()
			instanceId := NewId()
			settings := defaultDeviceRpcSettings()
			settings.DisableLogging = true

			firstSync := testingNewHeldSyncConn()
			followUpSync := testingNewHeldSyncConn()
			releaseFirstSync := sync.OnceFunc(func() { close(firstSync.release) })
			defer releaseFirstSync()
			releaseFollowUpSync := sync.OnceFunc(func() { close(followUpSync.release) })
			defer releaseFollowUpSync()
			dialer := &testingHeldSyncDialer{
				dialer: testing_deviceRpcDialer(settings),
				helds:  []*testingHeldSyncConn{firstSync, followUpSync},
			}
			deviceRemote, err := newDeviceRemoteWithOverrides(networkSpace, byJwt, instanceId, settings, clientId, dialer)
			if err != nil {
				t.Fatal(err)
			}
			defer deviceRemote.Close()

			recorder := &testingSettingRecorder{}
			sub := setting.addListener(deviceRemote, recorder)
			defer sub.Close()
			barrier := &testingDefaultLocationBarrier{reached: make(chan struct{})}
			barrierSub := deviceRemote.AddDefaultLocationChangeListener(barrier)
			defer barrierSub.Close()

			deviceLocalSettings := testDeviceLocalSettingsRpc()
			deviceLocalSettings.DisableLogging = true
			deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", instanceId, deviceLocalSettings, clientId)
			if err != nil {
				t.Fatal(err)
			}
			defer deviceLocal.Close()
			setting.deviceSet(deviceLocal, setting.initial)
			if value := setting.deviceGet(deviceLocal); value != setting.initial {
				t.Fatalf("%s: the device holds %s, want %s", setting.name, value, setting.initial)
			}
			deviceRemote.Sync()

			select {
			case <-firstSync.entered:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: the first sync never started", setting.name)
			}
			// the write, while the first sync is in flight
			setting.write(deviceRemote, setting.tapped)
			if !testingSettingHeard(t, setting, recorder, setting.tapped) {
				return
			}
			releaseFirstSync()

			select {
			case <-followUpSync.entered:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: the follow-up sync never started", setting.name)
			}
			// the follow-up sync took the write and is in flight; a view opens
			// and seeds itself from the getter
			view := &testingSettingRecorder{}
			view.add(setting.event(setting.get(deviceRemote)))
			viewSub := setting.addListener(deviceRemote, view)
			defer viewSub.Close()
			if !testingSettingHeard(t, setting, view, setting.tapped) {
				return
			}
			releaseFollowUpSync()

			applied := testing_waitFor(30*time.Second, func() bool {
				return deviceRemote.GetRemoteConnected() && setting.deviceGet(deviceLocal) == setting.tapped
			})
			if !applied {
				t.Fatalf("%s: the device never applied the write", setting.name)
			}
			// the device's pushes keep their first-queued order and the
			// default location is replayed after the settings, so once the
			// remote hears this later default location the replay has been
			// handled
			deviceLocal.SetDefaultLocation(testingConnectStateLocation("barrier"))
			select {
			case <-barrier.reached:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: the remote never heard the device's later default location", setting.name)
			}

			if slices.Contains(recorder.get(), setting.event(setting.initial)) {
				t.Errorf("%s: the listeners heard the device's older value after the write: %v", setting.name, recorder.get())
				return
			}
			if last := view.last(); last != setting.event(setting.tapped) {
				t.Errorf("%s: the view last heard %q of %v; the device is at %s", setting.name, last, view.get(), setting.tapped)
				return
			}
			testingSettingGets(t, setting, deviceRemote, setting.tapped, "after the device applied the write")
		}()
	}
}

// The view controller's connect under the auto control mode, with the rpc
// down (owner report 2026-10-16). Connect sets the provide mode to public
// before it reconnects; with no connect write pending yet the getters derive
// network for auto. The connect write then moves auto to public, and the
// listeners must hear it, not sit at network while GetProvideMode answers
// public until the sync's replay.
func TestDeviceRemoteAutoConnectTapTellsPublicProvideMode(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	provideRecorder := &testingSettingRecorder{}
	provideSub := deviceRemote.AddProvideModeChangeListener(provideRecorder)
	defer provideSub.Close()
	connectRecorder := &testingConnectStateRecorder{}
	connectSub := deviceRemote.AddConnectChangeListener(connectRecorder)
	defer connectSub.Close()

	// the device's state before the tap: auto, idle, providing to network
	// peers
	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()
	reports := []func() error{
		func() error { return deviceRemoteRpc.ProvideControlModeChanged(ProvideControlModeAuto, nil) },
		func() error { return deviceRemoteRpc.ProvideModeChanged(ProvideModeNetwork, nil) },
		func() error { return deviceRemoteRpc.ConnectChanged(false, nil) },
	}
	for _, report := range reports {
		if err := report(); err != nil {
			t.Fatal(err)
		}
	}
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() { close(handled) })
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("the reverse reports were not handled")
	}
	connect.AssertEqual(t, provideRecorder.get(), []string{"provideMode:network"})
	connect.AssertEqual(t, deviceRemote.GetProvideControlMode(), ProvideControlModeAuto)

	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)
	controller.Connect(testingConnectStateLocation("tapped"))

	connect.AssertEqual(t, connectRecorder.get(), []string{"connect:false", "connect:true"})
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModePublic)
	if last := provideRecorder.last(); last != "provideMode:public" {
		t.Fatalf(
			"the provide mode listeners last heard %q of %v while GetProvideMode answers %s",
			last, provideRecorder.get(), testingProvideModeName(deviceRemote.GetProvideMode()),
		)
	}

	// a stale replay of the idle provide mode does not flip it back
	testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideMode"), "network")
	connect.AssertEqual(t, provideRecorder.last(), "provideMode:public")

	// while the sync that takes the tap is in flight the getter still
	// answers public, and the device's replay of public is not told again
	testingTakeSettingSync(t, deviceRemote)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModePublic)
	before := provideRecorder.get()
	testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideMode"), "public")
	connect.AssertEqual(t, provideRecorder.get(), before)

	// the control mode is the device's throughout
	connect.AssertEqual(t, deviceRemote.GetProvideControlMode(), ProvideControlModeAuto)
}

// A destination written under the auto control mode with the rpc down. The
// device connects a destination by its spec list and provides publicly while
// connected (DeviceLocal.applyDestination), so the provide mode the getters
// derive and the listeners hear must follow what GetConnectEnabled answers: a
// custom destination given by its specs alone, with no display location, is
// public, and a display location without specs is idle.
func TestDeviceRemoteAutoDestinationProvideModeFollowsTheSpecs(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		location       *ConnectLocation
		specs          []*ProviderSpec
		connectEnabled bool
		provideMode    string
	}{
		{
			name:           "SpecsWithoutLocation",
			location:       nil,
			specs:          []*ProviderSpec{{BestAvailable: true}},
			connectEnabled: true,
			provideMode:    "public",
		},
		{
			name:           "LocationWithoutSpecs",
			location:       testingConnectStateLocation("display"),
			specs:          nil,
			connectEnabled: false,
			provideMode:    "network",
		},
		{
			name:           "SpecsAndLocation",
			location:       testingConnectStateLocation("display"),
			specs:          []*ProviderSpec{{BestAvailable: true}},
			connectEnabled: true,
			provideMode:    "public",
		},
	} {
		func() {
			deviceRemote := testingConnectStateRemoteWithoutListeners(t)
			provideRecorder := &testingSettingRecorder{}
			provideSub := deviceRemote.AddProvideModeChangeListener(provideRecorder)
			defer provideSub.Close()

			// the device's state before the write: auto, idle, providing to
			// network peers
			testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideControlMode"), ProvideControlModeAuto)
			testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideMode"), "network")
			testingReportConnectState(t, deviceRemote, nil, false)
			if events := provideRecorder.get(); !slices.Equal(events, []string{"provideMode:network"}) {
				t.Errorf("%s: before the write the provide mode listeners heard %v", testCase.name, events)
				return
			}

			specs := NewProviderSpecList()
			for _, spec := range testCase.specs {
				specs.Add(spec)
			}
			deviceRemote.SetDestination(testCase.location, specs)

			if connectEnabled := deviceRemote.GetConnectEnabled(); connectEnabled != testCase.connectEnabled {
				t.Errorf("%s: connect enabled reads %t, want %t", testCase.name, connectEnabled, testCase.connectEnabled)
			}
			if provideMode := testingProvideModeName(deviceRemote.GetProvideMode()); provideMode != testCase.provideMode {
				t.Errorf("%s: the provide mode reads %s, want %s", testCase.name, provideMode, testCase.provideMode)
			}
			if last := provideRecorder.last(); last != "provideMode:"+testCase.provideMode {
				t.Errorf("%s: the provide mode listeners last heard %q of %v, want %s", testCase.name, last, provideRecorder.get(), testCase.provideMode)
			}
		}()
	}
}

// Upgrading to Pro must never change the provide control mode (owner rule
// 2026-10-02). The provide mode moves (an app's provide toggle or an upgrade
// flow writes it, the auto mapping derives it from a connect, the device
// reports it), and the control mode the listeners hear, the getter answers and
// the device is sent stays the one the user chose, with the rpc down and live.
func TestDeviceRemoteProvideModeChangesNeverChangeTheControlMode(t *testing.T) {
	for _, provideControlMode := range []ProvideControlMode{
		ProvideControlModeAuto,
		ProvideControlModeManual,
		ProvideControlModeAlways,
		ProvideControlModeNetwork,
	} {
		func() {
			deviceRemote := testingConnectStateRemoteWithoutListeners(t)
			controlRecorder := &testingSettingRecorder{}
			controlSub := deviceRemote.AddProvideControlModeChangeListener(controlRecorder)
			defer controlSub.Close()
			provideRecorder := &testingSettingRecorder{}
			provideSub := deviceRemote.AddProvideModeChangeListener(provideRecorder)
			defer provideSub.Close()

			deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
			defer deviceRemoteRpc.Close()
			report := func(f func() error) {
				if err := f(); err != nil {
					t.Fatal(err)
				}
				handled := make(chan struct{})
				deviceRemoteRpc.dispatch(func() { close(handled) })
				select {
				case <-handled:
				case <-time.After(15 * time.Second):
					t.Fatal("the reverse report was not handled")
				}
			}
			report(func() error { return deviceRemoteRpc.ProvideControlModeChanged(provideControlMode, nil) })
			want := []string{"provideControlMode:" + provideControlMode}

			assertControlMode := func(when string) bool {
				if events := controlRecorder.get(); !slices.Equal(events, want) {
					t.Errorf("%s: %s the control mode listeners heard %v", provideControlMode, when, events)
					return false
				}
				if got := deviceRemote.GetProvideControlMode(); got != provideControlMode {
					t.Errorf("%s: %s the control mode reads %s", provideControlMode, when, got)
					return false
				}
				deviceRemote.stateLock.Lock()
				queued := deviceRemote.state.ProvideControlMode.IsSet
				deviceRemote.stateLock.Unlock()
				if queued {
					t.Errorf("%s: %s a control mode write is queued for the device", provideControlMode, when)
					return false
				}
				return true
			}

			// with the rpc down: the provide mode the apps write around an
			// upgrade, a connect, a disconnect, and the device's reports
			location := testingConnectStateLocation("tapped")
			deviceRemote.SetProvideMode(ProvideModePublic)
			deviceRemote.Reconnect(location)
			deviceRemote.SetProvideMode(ProvideModeNone)
			deviceRemote.SetConnectLocation(nil)
			deviceRemote.SetProvideMode(ProvideModePublic)
			if !assertControlMode("with the writes pending") {
				return
			}
			testingTakeSettingSync(t, deviceRemote)
			report(func() error { return deviceRemoteRpc.ProvideModeChanged(ProvideModePublic, nil) })
			report(func() error { return deviceRemoteRpc.ProvideModeChanged(ProvideModeNone, nil) })
			if !assertControlMode("after the device's provide mode reports") {
				return
			}

			// with the rpc live
			device := testingNewSettingFakeDevice()
			device.set("ProvideControlMode", provideControlMode)
			testingInstallSettingFakeDevice(t, deviceRemote, device)
			deviceRemote.SetProvideMode(ProvideModePublic)
			deviceRemote.SetProvideMode(ProvideModeNetwork)
			report(func() error { return deviceRemoteRpc.ProvideModeChanged(ProvideModePublic, nil) })
			if !assertControlMode("with the rpc live") {
				return
			}
			for _, write := range device.getWrites() {
				if write != "ProvideMode:public" && write != "ProvideMode:network" {
					t.Errorf("%s: the device was sent %s", provideControlMode, write)
				}
			}
			if len(provideRecorder.get()) == 0 {
				t.Errorf("%s: the provide mode listeners heard nothing", provideControlMode)
			}
		}()
	}
}

// Writes again from inside its route local callback, the first time it hears
// route local on.
type testingRewritingRouteLocalListener struct {
	deviceRemote *DeviceRemote
	recorder     *testingSettingRecorder
	rewritten    bool
}

func (self *testingRewritingRouteLocalListener) RouteLocalChanged(routeLocal bool) {
	self.recorder.RouteLocalChanged(routeLocal)
	if routeLocal && !self.rewritten {
		self.rewritten = true
		self.deviceRemote.SetRouteLocal(false)
	}
}

// A listener that writes a setting from its callback neither deadlocks the
// delivery nor hears the two writes out of order.
func TestDeviceRemoteSettingListenerWritesFromCallback(t *testing.T) {
	setting := testingRemoteSettingNamed(t, "RouteLocal")
	deviceRemote, recorder := testingSettingRemote(t, setting)
	rewriting := &testingRewritingRouteLocalListener{
		deviceRemote: deviceRemote,
		recorder:     &testingSettingRecorder{},
	}
	sub := deviceRemote.AddRouteLocalChangeListener(rewriting)
	defer sub.Close()
	testingReportSetting(t, deviceRemote, setting, "false")

	written := make(chan struct{})
	go func() {
		defer close(written)
		deviceRemote.SetRouteLocal(true)
	}()
	select {
	case <-written:
	case <-time.After(15 * time.Second):
		t.Fatal("a listener that writes from its callback deadlocked the delivery")
	}

	connect.AssertEqual(t, rewriting.recorder.get(), []string{"routeLocal:false", "routeLocal:true", "routeLocal:false"})
	connect.AssertEqual(t, recorder.get(), []string{"routeLocal:false", "routeLocal:true", "routeLocal:false"})
	connect.AssertEqual(t, deviceRemote.GetRouteLocal(), false)
}

// Exits its goroutine (runtime.Goexit) the first time it hears the blocker on,
// as t.FailNow does in a test listener.
type testingGoexitBlockerListener struct {
	once sync.Once
}

func (self *testingGoexitBlockerListener) BlockerEnabledChanged(blockerEnabled bool) {
	exit := false
	if blockerEnabled {
		self.once.Do(func() { exit = true })
	}
	if exit {
		runtime.Goexit()
	}
}

// A listener that exits the delivering goroutine ends that delivery, not the
// later ones: panic containment does not catch runtime.Goexit.
func TestDeviceRemoteSettingDeliveryResumesAfterListenerGoexit(t *testing.T) {
	setting := testingRemoteSettingNamed(t, "BlockerEnabled")
	deviceRemote, recorder := testingSettingRemote(t, setting)
	sub := deviceRemote.AddBlockerEnabledChangeListener(&testingGoexitBlockerListener{})
	defer sub.Close()

	written := make(chan struct{})
	go func() {
		defer close(written)
		deviceRemote.SetBlockerEnabled(true)
	}()
	select {
	case <-written:
	case <-time.After(15 * time.Second):
		t.Fatal("the write never returned")
	}

	deviceRemote.SetBlockerEnabled(false)
	connect.AssertEqual(t, recorder.last(), "blockerEnabled:false")
}

// A provide paused listener that, while armed, holds its first call until
// released: the first writer's goroutine descheduled in the middle of its
// delivery.
type testingHeldProvidePausedListener struct {
	recorder *testingSettingRecorder
	armed    atomic.Bool
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
}

func (self *testingHeldProvidePausedListener) ProvidePausedChanged(providePaused bool) {
	first := false
	if self.armed.Load() {
		self.once.Do(func() { first = true })
	}
	if first {
		close(self.entered)
		<-self.release
	}
	self.recorder.ProvidePausedChanged(providePaused)
}

// Two writes of a setting on different goroutines. However the deliveries
// interleave, the listeners hear the latest value last.
func TestDeviceRemoteConcurrentSettingWritesTellTheLatestLast(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingSettingRecorder{}
	held := &testingHeldProvidePausedListener{
		recorder: recorder,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	sub := deviceRemote.AddProvidePausedChangeListener(held)
	defer sub.Close()
	releaseHeld := sync.OnceFunc(func() { close(held.release) })
	defer releaseHeld()

	held.armed.Store(true)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		deviceRemote.SetProvidePaused(true)
	}()
	select {
	case <-held.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the first write is stuck")
	}
	held.armed.Store(false)

	// the second write, from another goroutine, completes meanwhile
	deviceRemote.SetProvidePaused(false)

	releaseHeld()
	select {
	case <-firstDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the first write did not return")
	}

	if last := recorder.last(); last != "providePaused:false" {
		t.Fatalf("the provide paused listener last heard %q of %v; provide paused is off", last, recorder.get())
	}
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), false)
}
