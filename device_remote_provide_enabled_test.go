// Provide enabled on a DeviceRemote (its provide change listeners) is told as
// the other settings are (device_remote_setting_state_test.go): a pending write
// the provide state follows tells it at once, a report the device made before
// it had the write does not undo it, the getter answers the write first, and
// the latest value is told last. Unlike the other settings, a value equal to
// the last told is told again. The fixtures are the setting tests': the
// remote's lifecycle parks in a blocked dial, and the tests drive the reverse
// rpc handlers and a fake device service directly, so every ordering below is
// a barrier, never a timing. Timers only bound a broken run.
package sdk

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Delivers a device report of provide enabled through the production reverse
// rpc handler and returns once the remote has handled it.
func testingReportProvideEnabled(t *testing.T, deviceRemote *DeviceRemote, provideEnabled bool) {
	t.Helper()
	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()

	if err := deviceRemoteRpc.ProvideChanged(provideEnabled, nil); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() {
		close(handled)
	})
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("the reverse report was not handled")
	}
}

// With no rpc a control mode write tells provide enabled before it returns. A
// report the device made before it had the write (the replay of a sync that
// started before the write) must not flip the listeners back, and while the
// sync that takes the write is in flight the getter still answers with it. The
// device's report of the write it applied is told again although unchanged,
// and so is a later change of its own.
func TestDeviceRemoteProvideEnabledWriteSurvivesStaleDeviceReport(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingSettingRecorder{}
	sub := deviceRemote.AddProvideChangeListener(recorder)
	defer sub.Close()
	controlMode := testingRemoteSettingNamed(t, "ProvideControlMode")

	// the device's state before the write: always providing
	testingReportSetting(t, deviceRemote, controlMode, ProvideControlModeAlways)
	testingReportProvideEnabled(t, deviceRemote, true)
	connect.AssertEqual(t, recorder.get(), []string{"provide:true"})

	deviceRemote.SetProvideControlMode(ProvideControlModeNever)
	connect.AssertEqual(t, recorder.get(), []string{"provide:true", "provide:false"})
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)

	// the replay of a sync that started before the write
	testingReportProvideEnabled(t, deviceRemote, true)
	if last := recorder.last(); last != "provide:false" {
		t.Fatalf("a stale report flipped the provide listeners to %q of %v; the pending write is off", last, recorder.get())
	}
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)

	// the sync delivers the write; while it is in flight the getter still
	// answers with it
	testingTakeSettingSync(t, deviceRemote)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)

	// the device reports what it applied
	testingReportProvideEnabled(t, deviceRemote, false)
	connect.AssertEqual(t, recorder.get(), []string{"provide:true", "provide:false", "provide:false"})

	// later, another controller turns providing back on
	testingReportSetting(t, deviceRemote, controlMode, ProvideControlModeAlways)
	testingReportProvideEnabled(t, deviceRemote, true)
	connect.AssertEqual(t, recorder.get(), []string{"provide:true", "provide:false", "provide:false", "provide:true"})
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), true)
}

// A write made while a sync is in flight stays pending after the sync
// publishes the service, until the follow-up sync takes it. Meanwhile the
// getter must answer with the write, as the listeners were told, not with the
// device's answer, which predates it.
func TestDeviceRemoteProvideEnabledGetterAnswersPendingWriteWhileLive(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingSettingRecorder{}
	sub := deviceRemote.AddProvideChangeListener(recorder)
	defer sub.Close()
	testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideControlMode"), ProvideControlModeAlways)
	testingReportProvideEnabled(t, deviceRemote, true)

	// the write, while the rpc is down
	deviceRemote.SetProvideControlMode(ProvideControlModeNever)
	connect.AssertEqual(t, recorder.last(), "provide:false")

	// a sync publishes the service without the write
	device := testingNewSettingFakeDevice()
	device.set("ProvideControlMode", ProvideControlModeAlways)
	device.set("ProvideEnabled", "true")
	testingInstallSettingFakeDevice(t, deviceRemote, device)

	if deviceRemote.GetProvideEnabled() {
		t.Fatal("provide enabled reads the device's answer from before the pending write; the write is off")
	}
	connect.AssertEqual(t, deviceRemote.GetProvideControlMode(), ProvideControlModeNever)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModeNone)
	connect.AssertEqual(t, recorder.last(), "provide:false")
}

// A provide listener that, while armed, holds its first call until released:
// the delivering goroutine descheduled in the middle of its delivery.
type testingHeldProvideListener struct {
	recorder *testingSettingRecorder
	armed    atomic.Bool
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
}

func (self *testingHeldProvideListener) ProvideChanged(provideEnabled bool) {
	first := false
	if self.armed.Load() {
		self.once.Do(func() { first = true })
	}
	if first {
		close(self.entered)
		<-self.release
	}
	self.recorder.ProvideChanged(provideEnabled)
}

// The device's report is being delivered when a write, pending with the rpc
// down, tells the opposite value on another goroutine. However the two
// deliveries interleave, the listeners hear the write's value last.
func TestDeviceRemoteProvideEnabledReportAndWriteTellTheLatestLast(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideControlMode"), ProvideControlModeAlways)
	recorder := &testingSettingRecorder{}
	held := &testingHeldProvideListener{
		recorder: recorder,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	sub := deviceRemote.AddProvideChangeListener(held)
	defer sub.Close()
	releaseHeld := sync.OnceFunc(func() { close(held.release) })
	defer releaseHeld()

	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()
	held.armed.Store(true)
	if err := deviceRemoteRpc.ProvideChanged(true, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the device's report never reached the listener")
	}
	held.armed.Store(false)

	// the write, on this goroutine, while the report's delivery is held
	deviceRemote.SetProvideControlMode(ProvideControlModeNever)

	releaseHeld()
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() { close(handled) })
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("the device's report was not handled")
	}

	if last := recorder.last(); last != "provide:false" {
		t.Fatalf("the provide listeners last heard %q of %v; the pending write is off", last, recorder.get())
	}
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)
}

// End to end over the device rpc, under the manual control mode. A provide
// mode write lands while the first sync is in flight, so it is pending, and
// the first sync's replay of the device's older provide enabled is dropped.
// The first sync's reply must not replace the pending state as last known: the
// follow-up sync takes the write, and a view opened while it is in flight
// seeds itself from the getter. The listeners must never hear the device's
// older value once the write was told, and the view must end providing.
func TestDeviceRemoteProvideEnabledViewOpenedDuringFollowUpSync(t *testing.T) {
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
	sub := deviceRemote.AddProvideChangeListener(recorder)
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
	connect.AssertEqual(t, deviceLocal.GetProvideControlMode(), ProvideControlModeManual)
	deviceLocal.SetProvideMode(ProvideModeNone)
	connect.AssertEqual(t, deviceLocal.GetProvideEnabled(), false)
	deviceRemote.Sync()

	select {
	case <-firstSync.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first sync never started")
	}
	// the write, while the first sync is in flight
	deviceRemote.SetProvideMode(ProvideModePublic)
	connect.AssertEqual(t, recorder.get(), []string{"provide:true"})
	releaseFirstSync()

	select {
	case <-followUpSync.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the follow-up sync never started")
	}
	// the follow-up sync took the write and is in flight; a view opens and
	// seeds itself from the getter
	view := &testingSettingRecorder{}
	view.ProvideChanged(deviceRemote.GetProvideEnabled())
	viewSub := deviceRemote.AddProvideChangeListener(view)
	defer viewSub.Close()
	if events := view.get(); !slices.Equal(events, []string{"provide:true"}) {
		t.Fatalf("the view seeded %v while the follow-up sync takes the write", events)
	}
	releaseFollowUpSync()

	applied := testing_waitFor(30*time.Second, func() bool {
		return deviceRemote.GetRemoteConnected() && deviceLocal.GetProvideEnabled()
	})
	if !applied {
		t.Fatal("the device never applied the write")
	}
	// the device's pushes keep their first-queued order and the default
	// location is replayed after the provide state, so once the remote hears
	// this later default location the replay has been handled
	deviceLocal.SetDefaultLocation(testingConnectStateLocation("barrier"))
	select {
	case <-barrier.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote never heard the device's later default location")
	}

	if slices.Contains(recorder.get(), "provide:false") {
		t.Fatalf("the provide listeners heard the device's older value after the write: %v", recorder.get())
	}
	if last := view.last(); last != "provide:true" {
		t.Fatalf("the view last heard %q of %v; the device provides", last, view.get())
	}
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), true)
}

// Refreshes the provide mode from the provide enabled listener, as the status
// of the Linux and Windows apps does (they hold no provide mode listener), and
// records provide enabled with the provide mode the getter answers then.
type testingProvideModeRefreshListener struct {
	deviceRemote *DeviceRemote
	recorder     *testingSettingRecorder
}

func (self *testingProvideModeRefreshListener) ProvideChanged(provideEnabled bool) {
	self.recorder.add(fmt.Sprintf(
		"provide:%t provideMode:%s",
		provideEnabled,
		testingProvideModeName(self.deviceRemote.GetProvideMode()),
	))
}

// A connect write under the auto control mode with the rpc down moves the
// provide mode the getters derive between network and public. Each write must
// tell provide enabled before it returns, with the state the getters derive,
// so a status that refreshes the provide mode from the provide enabled
// listener shows the mode the write derives. Without the tell the desktop apps
// show the previous mode until the device reports. The view controller's
// connect and disconnect taps write Reconnect and SetConnectLocation(nil).
func TestDeviceRemoteProvideEnabledToldByPendingAutoConnect(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// the device's connect state before the write
		connected bool
		write     func(deviceRemote *DeviceRemote)
		// the provide mode the getters derive from the write
		provideMode string
	}{
		{
			name:      "SetConnectLocation",
			connected: false,
			write: func(deviceRemote *DeviceRemote) {
				deviceRemote.SetConnectLocation(testingConnectStateLocation("tapped"))
			},
			provideMode: "public",
		},
		{
			name:      "Reconnect",
			connected: false,
			write: func(deviceRemote *DeviceRemote) {
				deviceRemote.Reconnect(testingConnectStateLocation("tapped"))
			},
			provideMode: "public",
		},
		{
			name:      "SetConnectLocation(nil)",
			connected: true,
			write: func(deviceRemote *DeviceRemote) {
				deviceRemote.SetConnectLocation(nil)
			},
			provideMode: "network",
		},
		{
			name:      "SetDestination",
			connected: false,
			write: func(deviceRemote *DeviceRemote) {
				specs := NewProviderSpecList()
				specs.Add(&ProviderSpec{BestAvailable: true})
				deviceRemote.SetDestination(testingConnectStateLocation("tapped"), specs)
			},
			provideMode: "public",
		},
		{
			name:      "RemoveDestination",
			connected: true,
			write: func(deviceRemote *DeviceRemote) {
				deviceRemote.RemoveDestination()
			},
			provideMode: "network",
		},
	} {
		func() {
			deviceRemote := testingConnectStateRemoteWithoutListeners(t)
			recorder := &testingSettingRecorder{}
			sub := deviceRemote.AddProvideChangeListener(&testingProvideModeRefreshListener{
				deviceRemote: deviceRemote,
				recorder:     recorder,
			})
			defer sub.Close()

			// the device's state before the write: auto, providing to network
			// peers while idle and publicly while connected
			provideMode := "network"
			var location *ConnectLocation
			if testCase.connected {
				provideMode = "public"
				location = testingConnectStateLocation("connected")
			}
			testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideControlMode"), ProvideControlModeAuto)
			testingReportSetting(t, deviceRemote, testingRemoteSettingNamed(t, "ProvideMode"), provideMode)
			testingReportConnectState(t, deviceRemote, location, testCase.connected)
			testingReportProvideEnabled(t, deviceRemote, true)
			want := []string{"provide:true provideMode:" + provideMode}
			if events := recorder.get(); !slices.Equal(events, want) {
				t.Errorf("%s: before the write the provide listeners heard %v, want %v", testCase.name, events, want)
				return
			}

			testCase.write(deviceRemote)

			// told before the write returns, with the state the getters derive
			want = append(want, "provide:true provideMode:"+testCase.provideMode)
			if events := recorder.get(); !slices.Equal(events, want) {
				t.Errorf("%s: after the write the provide listeners heard %v, want %v", testCase.name, events, want)
				return
			}
			if !deviceRemote.GetProvideEnabled() {
				t.Errorf("%s: provide enabled reads false; auto provides", testCase.name)
			}
			if got := testingProvideModeName(deviceRemote.GetProvideMode()); got != testCase.provideMode {
				t.Errorf("%s: the provide mode reads %s, want %s", testCase.name, got, testCase.provideMode)
			}
		}()
	}
}
