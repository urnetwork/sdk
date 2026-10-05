//go:build !js

// The device configuration changed event, at the remote the browser runs: a
// DeviceLocal served through the hosted rpc listener the proxy host uses, and
// a remote built the way the platform remote is built for a browser. Serving
// the device under a new generation stands in for the host recreating it.
package sdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect"
)

// Serves device-rpc websockets to the hosted listener installed last, as the
// proxy host's device-rpc handler serves its hosted device.
type testingHostedRpcDevice struct {
	ctx         context.Context
	deviceLocal *DeviceLocal
	instanceId  *Id
	server      *httptest.Server

	stateLock sync.Mutex
	listener  *HostedDeviceRpcListener
}

// A DeviceLocal without its localhost rpc, behind a websocket server that has
// no hosted listener to serve until serveGeneration installs one.
func testing_newHostedRpcDevice(t *testing.T, ctx context.Context) *testingHostedRpcDevice {
	t.Helper()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)
	settings := testDeviceLocalSettingsRpc()
	// the hosted listener replaces the localhost one
	settings.EnableRpc = false
	instanceId := NewId()
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, byJwt, "", "", "", instanceId, settings, connect.NewId(),
	)
	connect.AssertEqual(t, err, nil)
	t.Cleanup(deviceLocal.Close)

	hosted := &testingHostedRpcDevice{
		ctx:         ctx,
		deviceLocal: deviceLocal,
		instanceId:  instanceId,
	}
	upgrader := websocket.Upgrader{}
	hosted.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		listener := func() *HostedDeviceRpcListener {
			hosted.stateLock.Lock()
			defer hosted.stateLock.Unlock()
			return hosted.listener
		}()
		if listener != nil {
			_ = listener.ServeWs(ws)
		}
	}))
	t.Cleanup(hosted.server.Close)
	return hosted
}

// Installs a fresh hosted listener under deviceGeneration. The device closes
// the previous listener and its sessions, as when the host recreates the
// device. The new listener is published first, so a remote that reconnects in
// between waits for it instead of reaching the closed one.
func (self *testingHostedRpcDevice) serveGeneration(deviceGeneration string) {
	listener := NewHostedDeviceRpcListener(self.ctx)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.listener = listener
	}()
	self.deviceLocal.StartHostedRpc(listener, deviceGeneration)
}

// A remote paired with the hosted device, set up as NewPlatformDeviceRemote
// sets up the browser's: hosted-incompatible setters off, and reads served
// from the sync snapshot while writes resync.
func (self *testingHostedRpcDevice) newRemote(t *testing.T) *DeviceRemote {
	t.Helper()

	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.BrowserStateOnly = true
	dialer := NewPlatformDeviceRpcDialer(
		"ws"+self.server.URL[len("http"):],
		"synthetic-signed-proxy-id",
		settings,
	)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		self.deviceLocal.networkSpace,
		self.deviceLocal.byJwt,
		self.instanceId,
		settings,
		self.deviceLocal.clientId,
		dialer,
	)
	connect.AssertEqual(t, err, nil)
	t.Cleanup(deviceRemote.Close)
	return deviceRemote
}

// What the listeners saw for one successful sync.
type testingConfigurationSync struct {
	recreated            bool
	configurationChanged bool
	// read through the remote's getter while the event was being delivered
	performanceProfile *PerformanceProfile
}

// Records one testingConfigurationSync per successful sync, in order. The
// remote calls its listeners one at a time from its run loop, a sync's
// remote change first, so each event lands on the record of the sync that
// caused it. The signal never blocks the run loop: a waiter re-reads the
// records on every wake, so coalesced signals lose nothing.
type testingConfigurationListener struct {
	deviceRemote *DeviceRemote

	stateLock sync.Mutex
	syncs     []*testingConfigurationSync

	recorded chan struct{}
}

// Reads the performance profile through deviceRemote when an event arrives.
func newTestingConfigurationListener(deviceRemote *DeviceRemote) *testingConfigurationListener {
	return &testingConfigurationListener{
		deviceRemote: deviceRemote,
		recorded:     make(chan struct{}, 1),
	}
}

// Wakes a waiter without blocking; one pending wake is enough.
func (self *testingConfigurationListener) signal() {
	select {
	case self.recorded <- struct{}{}:
	default:
	}
}

// A connected remote completed a sync: a new record.
func (self *testingConfigurationListener) RemoteChanged(remoteConnected bool) {
	if !remoteConnected {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.syncs = append(self.syncs, &testingConfigurationSync{})
	}()
	self.signal()
}

// Marks the current sync.
func (self *testingConfigurationListener) DeviceRecreated() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.syncs[len(self.syncs)-1].recreated = true
	}()
	self.signal()
}

// Marks the current sync, with what the getter reads while it is delivered.
func (self *testingConfigurationListener) DeviceConfigurationChanged() {
	performanceProfile := self.deviceRemote.GetPerformanceProfile()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		syncRecord := self.syncs[len(self.syncs)-1]
		syncRecord.configurationChanged = true
		syncRecord.performanceProfile = performanceProfile
	}()
	self.signal()
}

// A copy of the records, for assertions.
func (self *testingConfigurationListener) syncRecords() []testingConfigurationSync {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	syncRecords := []testingConfigurationSync{}
	for _, syncRecord := range self.syncs {
		syncRecords = append(syncRecords, *syncRecord)
	}
	return syncRecords
}

// Waits until the records satisfy condition. The timeout only bounds a
// failure.
func (self *testingConfigurationListener) awaitRecords(
	t *testing.T,
	description string,
	condition func(syncs []testingConfigurationSync) bool,
) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for !condition(self.syncRecords()) {
		select {
		case <-self.recorded:
		case <-timeout:
			t.Fatalf("timed out waiting for %s: %+v", description, self.syncRecords())
		}
	}
}

// The browser re-applies its own settings when this fires, so it fires after
// the first sync and after a sync with a recreated device, and stays quiet on
// a reconnect to the same device, which every browser setter causes (a setter
// queues its state and resyncs). When it fires the getter reads the device
// that was just synced, not the one before it.
func TestDeviceRemoteConfigurationChangedOnFirstSyncAndRecreate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	hosted := testing_newHostedRpcDevice(t, ctx)
	hosted.serveGeneration(NewId().String())
	deviceRemote := hosted.newRemote(t)
	listener := newTestingConfigurationListener(deviceRemote)
	defer deviceRemote.AddRemoteChangeListener(listener).Close()
	defer deviceRemote.AddDeviceRecreatedListener(listener).Close()
	defer deviceRemote.AddDeviceConfigurationChangedListener(listener).Close()

	deviceRemote.Sync()
	listener.awaitRecords(t, "the first sync's configuration change", func(syncs []testingConfigurationSync) bool {
		return 1 <= len(syncs) && syncs[0].configurationChanged
	})

	// a reconnect to the same device
	testingForceDeviceRpcResync(deviceRemote)
	listener.awaitRecords(t, "the reconnect to the same device", func(syncs []testingConfigurationSync) bool {
		return 2 <= len(syncs)
	})

	// the host recreates the device, which starts from other settings than the
	// remote saw last
	fixedIpProfile := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
	}
	hosted.deviceLocal.SetPerformanceProfile(fixedIpProfile)
	hosted.serveGeneration(NewId().String())
	deviceRemote.Sync()
	listener.awaitRecords(t, "the recreated device's configuration change", func(syncs []testingConfigurationSync) bool {
		for _, syncRecord := range syncs {
			if syncRecord.recreated && syncRecord.configurationChanged {
				return true
			}
		}
		return false
	})

	syncs := listener.syncRecords()
	if len(syncs) < 3 {
		t.Fatalf("expected at least three syncs, got %+v", syncs)
	}
	if !syncs[0].configurationChanged || syncs[0].recreated {
		t.Fatalf("the first sync must report a configuration change and no recreation: %+v", syncs[0])
	}
	recreatedIndex := -1
	for i, syncRecord := range syncs[1:] {
		if syncRecord.recreated {
			recreatedIndex = i + 1
			break
		}
		if syncRecord.configurationChanged {
			t.Fatalf("sync %d reconnected to the same device and reported a configuration change", i+1)
		}
	}
	if recreatedIndex < 2 {
		t.Fatalf("expected a reconnect to the same device before the recreated one: %+v", syncs)
	}
	recreated := syncs[recreatedIndex]
	if !recreated.configurationChanged {
		t.Fatal("the sync that reached the recreated device did not report a configuration change")
	}
	if !reflect.DeepEqual(recreated.performanceProfile, fixedIpProfile) {
		t.Fatalf("at the event the getter read %+v, want the recreated device's %+v", recreated.performanceProfile, fixedIpProfile)
	}
	for _, syncRecord := range syncs[recreatedIndex+1:] {
		if syncRecord.configurationChanged {
			t.Fatalf("a reconnect to the recreated device reported a configuration change: %+v", syncs)
		}
	}
}

// A device that reports no generation cannot show a recreation, so every sync
// with it reports a configuration change, and none reports a recreation.
func TestDeviceRemoteConfigurationChangedOnEverySyncWithoutGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	hosted := testing_newHostedRpcDevice(t, ctx)
	hosted.serveGeneration("")
	deviceRemote := hosted.newRemote(t)
	listener := newTestingConfigurationListener(deviceRemote)
	defer deviceRemote.AddRemoteChangeListener(listener).Close()
	defer deviceRemote.AddDeviceRecreatedListener(listener).Close()
	defer deviceRemote.AddDeviceConfigurationChangedListener(listener).Close()

	deviceRemote.Sync()
	listener.awaitRecords(t, "the first sync's configuration change", func(syncs []testingConfigurationSync) bool {
		return 1 <= len(syncs) && syncs[0].configurationChanged
	})
	testingForceDeviceRpcResync(deviceRemote)
	listener.awaitRecords(t, "the reconnect's configuration change", func(syncs []testingConfigurationSync) bool {
		return 2 <= len(syncs) && syncs[1].configurationChanged
	})

	// the two syncs awaited above; a later one may still be delivering
	syncs := listener.syncRecords()
	if len(syncs) < 2 {
		t.Fatalf("expected at least two syncs, got %+v", syncs)
	}
	for i, syncRecord := range syncs[:2] {
		if !syncRecord.configurationChanged || syncRecord.recreated {
			t.Fatalf("sync %d without a generation: %+v", i, syncRecord)
		}
	}
}
