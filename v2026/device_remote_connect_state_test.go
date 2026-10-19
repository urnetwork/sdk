// A connect or disconnect tap on a DeviceRemote shows its connect state to the
// listeners without waiting for the device to report it back, and a report the
// device made before it had the tap does not undo it (owner report
// 2026-10-16). The fixtures park the
// remote's lifecycle in a blocked dial and drive the reverse rpc handlers and a
// fake device service directly, so every ordering below is a barrier, never a
// timing. Timers only bound a broken run.
package sdk

import (
	"context"
	"fmt"
	"net"
	"net/rpc"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Records the connect state events in arrival order.
type testingConnectStateRecorder struct {
	stateLock sync.Mutex
	events    []string
}

func (self *testingConnectStateRecorder) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *testingConnectStateRecorder) get() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

func (self *testingConnectStateRecorder) ConnectLocationChanged(location *ConnectLocation) {
	if location == nil {
		self.add("location:nil")
	} else {
		self.add("location:" + location.Name)
	}
}

func (self *testingConnectStateRecorder) ConnectChanged(connectEnabled bool) {
	self.add(fmt.Sprintf("connect:%t", connectEnabled))
}

// A remote whose lifecycle never reaches a device: every write is pending.
func testingConnectStateRemote(t *testing.T) (*DeviceRemote, *testingConnectStateRecorder) {
	t.Helper()
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingConnectStateRecorder{}
	connectLocationSub := deviceRemote.AddConnectLocationChangeListener(recorder)
	t.Cleanup(connectLocationSub.Close)
	connectSub := deviceRemote.AddConnectChangeListener(recorder)
	t.Cleanup(connectSub.Close)
	return deviceRemote, recorder
}

func testingConnectStateRemoteWithoutListeners(t *testing.T) *DeviceRemote {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	dialer := &testingBlockedDeviceRpcDialer{
		entered: make(chan struct{}, 1024),
		release: make(chan struct{}),
	}
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		connect.NewId(),
		dialer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	return deviceRemote
}

// Delivers device reports through the production reverse rpc handlers and
// returns once the remote has handled them: callbacks run serially, so a
// later barrier callback runs after every report.
func testingReportConnectState(t *testing.T, deviceRemote *DeviceRemote, location *ConnectLocation, connectEnabled bool) {
	t.Helper()
	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()

	err := deviceRemoteRpc.ConnectLocationChanged(&DeviceRemoteConnectLocationChangeEvent{
		Location: newDeviceRemoteConnectLocation(location),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := deviceRemoteRpc.ConnectChanged(connectEnabled, nil); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() {
		close(handled)
	})
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("reverse reports were not handled")
	}
}

func testingConnectStateLocation(name string) *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{ClientId: NewId()},
		Name:              name,
	}
}

// With no rpc, the tap reaches the listeners and the view before it returns.
func TestDeviceRemoteConnectTapNotifiesWithoutRpc(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)

	location := testingConnectStateLocation("tapped")
	controller.Connect(location)

	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
	if status := controller.GetConnectionStatus(); status != DestinationSet && status != Connecting {
		t.Fatalf("connection status after the tap = %s, want the connecting state", status)
	}
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), true)
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "tapped")

	controller.Disconnect()

	connect.AssertEqual(t, recorder.get(), []string{
		"location:tapped", "connect:true",
		"location:nil", "connect:false",
	})
	connect.AssertEqual(t, controller.GetConnectionStatus(), Disconnected)
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
}

// A report the device made before it had the pending tap (the replay of a
// sync that started before the tap) must not flip the view back. The tap
// stays shown until the device reports it applied it, and that report is not
// announced a second time.
func TestDeviceRemoteConnectTapSurvivesStaleDeviceReport(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)

	location := testingConnectStateLocation("tapped")
	controller.Connect(location)
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})

	// the device's state before the tap
	testingReportConnectState(t, deviceRemote, nil, false)

	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
	if status := controller.GetConnectionStatus(); status != DestinationSet && status != Connecting {
		t.Fatalf("a stale device report flipped the connection status to %s", status)
	}
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), true)
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "tapped")

	// the sync delivers the tap; the device reports what it applied
	testingTakeConnectSync(t, deviceRemote)
	// while that sync is in flight the getters still answer with the tap
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), true)
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "tapped")
	testingReportConnectState(t, deviceRemote, location, true)

	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), true)
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "tapped")
}

// The disconnect counterpart: a stale connected report must not undo the tap.
func TestDeviceRemoteDisconnectTapSurvivesStaleDeviceReport(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)

	location := testingConnectStateLocation("connected")
	controller.Connect(location)
	controller.Disconnect()
	connect.AssertEqual(t, recorder.get(), []string{
		"location:connected", "connect:true",
		"location:nil", "connect:false",
	})

	// the device's state before the disconnect tap
	testingReportConnectState(t, deviceRemote, location, true)

	connect.AssertEqual(t, recorder.get(), []string{
		"location:connected", "connect:true",
		"location:nil", "connect:false",
	})
	connect.AssertEqual(t, controller.GetConnectionStatus(), Disconnected)
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
	if location := deviceRemote.GetConnectLocation(); location != nil {
		t.Fatalf("a stale device report restored the location %q", location.Name)
	}
}

// A fake device whose connect writes block on a barrier.
type testingConnectTapDeviceRpc struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	stateLock   sync.Mutex
	writes      []string
}

func (self *testingConnectTapDeviceRpc) releaseWrites() {
	self.releaseOnce.Do(func() { close(self.release) })
}

func (self *testingConnectTapDeviceRpc) write(name string) error {
	self.stateLock.Lock()
	self.writes = append(self.writes, name)
	self.stateLock.Unlock()
	self.enteredOnce.Do(func() { close(self.entered) })
	<-self.release
	return nil
}

func (self *testingConnectTapDeviceRpc) Reconnect(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	return self.write("Reconnect")
}

func (self *testingConnectTapDeviceRpc) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	return self.write("SetConnectLocation")
}

func (self *testingConnectTapDeviceRpc) SetDestination(destination *DeviceRemoteDestination, _ RpcVoid) error {
	return self.write("SetDestination")
}

func (self *testingConnectTapDeviceRpc) RemoveDestination(_ RpcNoArg, _ RpcVoid) error {
	return self.write("RemoveDestination")
}

// Publishes the fake device as the remote's live service, as a completed sync
// does. The remote's own lifecycle stays parked in its blocked dial.
func testingInstallConnectTapService(t *testing.T, deviceRemote *DeviceRemote, server *testingConnectTapDeviceRpc) {
	t.Helper()
	testingInstallFakeDeviceService(t, deviceRemote, server)
	// before the server joins, so a held write cannot pin it
	t.Cleanup(server.releaseWrites)
}

// Serves the receiver as DeviceLocalRpc and publishes it as the remote's live
// service.
func testingInstallFakeDeviceService(t *testing.T, deviceRemote *DeviceRemote, receiver any) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("DeviceLocalRpc", receiver); err != nil {
		t.Fatal(err)
	}
	service := &rpcClient{
		ctx:         context.Background(),
		log:         connect.NewNoopLogger(),
		timeout:     time.Minute,
		closeClient: clientConn.Close,
		client:      rpc.NewClient(clientConn),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		rpcServer.ServeConn(serverConn)
	}()
	t.Cleanup(func() {
		service.Close()
		serverConn.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the fake device rpc server did not join")
		}
	})

	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	deviceRemote.service = service
	deviceRemote.remoteConnected = true
}

// With a live rpc, the listeners hear the tap as soon as the device applied
// it, from the write itself, and not only when the device's report comes back
// over the reverse rpc. Nothing is announced while the device still holds the
// write: a view grid built then would bind to the outgoing session's window.
// The device's echo of the applied write is not announced again.
func TestDeviceRemoteConnectTapNotifiesOnceDeviceApplies(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		write     func(*DeviceRemote, *ConnectLocation)
		rpcMethod string
	}{
		{"Reconnect", (*DeviceRemote).Reconnect, "Reconnect"},
		{"SetConnectLocation", (*DeviceRemote).SetConnectLocation, "SetConnectLocation"},
		{"SetDestination", func(deviceRemote *DeviceRemote, location *ConnectLocation) {
			specs := NewProviderSpecList()
			specs.Add(&ProviderSpec{BestAvailable: true})
			deviceRemote.SetDestination(location, specs)
		}, "SetDestination"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			deviceRemote, recorder := testingConnectStateRemote(t)
			server := &testingConnectTapDeviceRpc{
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			testingInstallConnectTapService(t, deviceRemote, server)

			location := testingConnectStateLocation("tapped")
			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				testCase.write(deviceRemote, location)
			}()

			// the device holds the write: its old window is still current
			select {
			case <-server.entered:
			case <-time.After(15 * time.Second):
				t.Fatal("the write never reached the device")
			}
			connect.AssertEqual(t, len(recorder.get()), 0)

			server.releaseWrites()
			select {
			case <-writeDone:
			case <-time.After(15 * time.Second):
				t.Fatal("the write did not return after the device applied it")
			}
			// announced by the write, before any report from the device
			connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
			server.stateLock.Lock()
			connect.AssertEqual(t, server.writes, []string{testCase.rpcMethod})
			server.stateLock.Unlock()

			// the device's echo of the applied write
			testingReportConnectState(t, deviceRemote, location, true)
			connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})

			// a later change of the device's own is announced
			testingReportConnectState(t, deviceRemote, nil, false)
			connect.AssertEqual(t, recorder.get(), []string{
				"location:tapped", "connect:true",
				"location:nil", "connect:false",
			})
		})
	}
}

// The disconnect counterpart with a live rpc.
func TestDeviceRemoteDisconnectTapNotifiesOnceDeviceApplies(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		write     func(*DeviceRemote)
		rpcMethod string
	}{
		{"SetConnectLocationNil", func(deviceRemote *DeviceRemote) { deviceRemote.SetConnectLocation(nil) }, "SetConnectLocation"},
		{"RemoveDestination", (*DeviceRemote).RemoveDestination, "RemoveDestination"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			deviceRemote, recorder := testingConnectStateRemote(t)
			location := testingConnectStateLocation("connected")
			testingReportConnectState(t, deviceRemote, location, true)
			connect.AssertEqual(t, recorder.get(), []string{"location:connected", "connect:true"})

			server := &testingConnectTapDeviceRpc{
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			testingInstallConnectTapService(t, deviceRemote, server)

			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				testCase.write(deviceRemote)
			}()
			select {
			case <-server.entered:
			case <-time.After(15 * time.Second):
				t.Fatal("the write never reached the device")
			}
			connect.AssertEqual(t, recorder.get(), []string{"location:connected", "connect:true"})

			server.releaseWrites()
			select {
			case <-writeDone:
			case <-time.After(15 * time.Second):
				t.Fatal("the write did not return after the device applied it")
			}
			// announced by the write, before any report from the device
			connect.AssertEqual(t, recorder.get(), []string{
				"location:connected", "connect:true",
				"location:nil", "connect:false",
			})
			testingReportConnectState(t, deviceRemote, nil, false)
			connect.AssertEqual(t, recorder.get(), []string{
				"location:connected", "connect:true",
				"location:nil", "connect:false",
			})
		})
	}
}

// Holds the first write of a dialed forward stream, which is the Sync request:
// the remote has taken its pending state into the sync, and the service is not
// published yet.
type testingHeldSyncConn struct {
	net.Conn
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (self *testingHeldSyncConn) Write(b []byte) (int, error) {
	self.once.Do(func() {
		close(self.entered)
		<-self.release
	})
	return self.Conn.Write(b)
}

func testingNewHeldSyncConn() *testingHeldSyncConn {
	return &testingHeldSyncConn{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Holds the sync of each of the first dials, one held stream per dial in turn.
type testingHeldSyncDialer struct {
	dialer deviceRpcDialer

	stateLock sync.Mutex
	helds     []*testingHeldSyncConn
}

func (self *testingHeldSyncDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	forwardConn, reverseConn, err := self.dialer.Dial(ctx)
	if err != nil {
		return forwardConn, reverseConn, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < len(self.helds) {
		held := self.helds[0]
		self.helds = self.helds[1:]
		held.Conn = forwardConn
		return held, reverseConn, nil
	}
	return forwardConn, reverseConn, nil
}

// Closes once the remote hears the device's default location named "barrier".
type testingDefaultLocationBarrier struct {
	once    sync.Once
	reached chan struct{}
}

func (self *testingDefaultLocationBarrier) DefaultLocationChanged(location *ConnectLocation) {
	if location != nil && location.Name == "barrier" {
		self.once.Do(func() { close(self.reached) })
	}
}

// End to end over the device rpc: a tap while the first sync is in flight
// shows at once, the device applies it on the next sync, and the listeners
// hear the tap once, with no revert to the device's older state in between.
func TestDeviceRemoteConnectTapDuringSyncReachesDevice(t *testing.T) {
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

	held := testingNewHeldSyncConn()
	releaseSync := sync.OnceFunc(func() { close(held.release) })
	defer releaseSync()
	dialer := &testingHeldSyncDialer{
		dialer: testing_deviceRpcDialer(settings),
		helds:  []*testingHeldSyncConn{held},
	}
	deviceRemote, err := newDeviceRemoteWithOverrides(networkSpace, byJwt, instanceId, settings, clientId, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()

	recorder := &testingConnectStateRecorder{}
	connectLocationSub := deviceRemote.AddConnectLocationChangeListener(recorder)
	defer connectLocationSub.Close()
	connectSub := deviceRemote.AddConnectChangeListener(recorder)
	defer connectSub.Close()
	barrier := &testingDefaultLocationBarrier{reached: make(chan struct{})}
	barrierSub := deviceRemote.AddDefaultLocationChangeListener(barrier)
	defer barrierSub.Close()
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)

	deviceLocalSettings := testDeviceLocalSettingsRpc()
	deviceLocalSettings.DisableLogging = true
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", instanceId, deviceLocalSettings, clientId)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	deviceRemote.Sync()

	select {
	case <-held.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first sync never started")
	}
	location := testingConnectStateLocation("tapped")
	controller.Connect(location)
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
	releaseSync()

	applied := testing_waitFor(30*time.Second, func() bool {
		deviceLocation := deviceLocal.GetConnectLocation()
		return deviceRemote.GetRemoteConnected() &&
			deviceLocation != nil && deviceLocation.Name == "tapped"
	})
	if !applied {
		t.Fatal("the device never applied the tap")
	}
	// The device replays its state to the remote's listeners as each sync
	// finishes, before the remote publishes the service. Its pushes keep their
	// first-queued order and the default location is replayed after the
	// connect state, so once the remote hears this later default location the
	// replay has been handled.
	deviceLocal.SetDefaultLocation(testingConnectStateLocation("barrier"))
	select {
	case <-barrier.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote never heard the device's later default location")
	}
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), true)
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "tapped")
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})
	if status := controller.GetConnectionStatus(); status == Disconnected {
		t.Fatalf("the connection status reverted to %s", status)
	}
}

// Takes the remote's pending state, as takeSyncRequest does when a sync starts.
func testingTakeConnectSync(t *testing.T, deviceRemote *DeviceRemote) {
	t.Helper()
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	syncState := deviceRemote.state
	deviceRemote.state = DeviceRemoteState{}
	if !syncState.Location.IsSet {
		t.Fatal("the sync took no connect write")
	}
}

// Fails unless the last location and connect events the listeners heard are
// the given state.
func testingAssertConnectStateLastHeard(t *testing.T, deviceRemote *DeviceRemote, recorder *testingConnectStateRecorder, location string, connectEnabled bool) {
	t.Helper()
	events := recorder.get()
	lastLocation := ""
	lastConnect := ""
	for _, event := range events {
		switch {
		case strings.HasPrefix(event, "location:"):
			lastLocation = event
		case strings.HasPrefix(event, "connect:"):
			lastConnect = event
		}
	}
	if lastLocation != "location:"+location || lastConnect != fmt.Sprintf("connect:%t", connectEnabled) {
		t.Fatalf(
			"listeners last heard %s %s of %v; the device is at location:%s connect:%t (GetConnectEnabled=%t)",
			lastLocation, lastConnect, events, location, connectEnabled, deviceRemote.GetConnectEnabled(),
		)
	}
}

// Taps queued while the rpc is down (connect, disconnect, connect) reach the
// device in one sync, so it replays only the final state. A change the device
// later makes on its own (the extension's own disconnect, another controller,
// a recreated extension's replay) must still reach the listeners.
func TestDeviceRemoteDeviceDisconnectAfterQueuedTapsIsTold(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	location := testingConnectStateLocation("tapped")

	deviceRemote.Reconnect(location)
	deviceRemote.SetConnectLocation(nil)
	deviceRemote.Reconnect(location)

	testingTakeConnectSync(t, deviceRemote)
	// the sync's replay of the state the device applied
	testingReportConnectState(t, deviceRemote, location, true)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "tapped", true)

	// later, the device disconnects on its own
	testingReportConnectState(t, deviceRemote, nil, false)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "nil", false)
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
}

// The same with the rpc live: the device coalesces its pushes per key, last
// value wins (DeviceLocalRpc.enqueueReverse), so three quick taps can come
// back as one report.
func TestDeviceRemoteDeviceDisconnectAfterCoalescedEchoIsTold(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	server := &testingConnectTapDeviceRpc{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	testingInstallConnectTapService(t, deviceRemote, server)
	server.releaseWrites()

	location := testingConnectStateLocation("tapped")
	deviceRemote.Reconnect(location)
	deviceRemote.SetConnectLocation(nil)
	deviceRemote.Reconnect(location)

	// the coalesced push of the final state
	testingReportConnectState(t, deviceRemote, location, true)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "tapped", true)

	// later, the device disconnects on its own
	testingReportConnectState(t, deviceRemote, nil, false)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "nil", false)
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
}

// A connect listener that, while armed, holds its first call until released:
// the first writer's goroutine descheduled in the middle of its delivery.
type testingHeldConnectListener struct {
	recorder *testingConnectStateRecorder
	armed    atomic.Bool
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
}

func (self *testingHeldConnectListener) ConnectChanged(connectEnabled bool) {
	first := false
	if self.armed.Load() {
		self.once.Do(func() { first = true })
	}
	if first {
		close(self.entered)
		<-self.release
	}
	self.recorder.add(fmt.Sprintf("connect:%t", connectEnabled))
}

// Two connect writes on different goroutines with the rpc live, and the
// device's in-order reports of both. However the deliveries interleave, the
// listeners hear the latest state last.
func TestDeviceRemoteConcurrentConnectWritesTellTheLatestLast(t *testing.T) {
	deviceRemote, _ := testingConnectStateRemote(t)
	recorder := &testingConnectStateRecorder{}
	held := &testingHeldConnectListener{
		recorder: recorder,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	sub := deviceRemote.AddConnectChangeListener(held)
	defer sub.Close()
	releaseHeld := sync.OnceFunc(func() { close(held.release) })
	defer releaseHeld()

	server := &testingConnectTapDeviceRpc{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	testingInstallConnectTapService(t, deviceRemote, server)
	server.releaseWrites()

	location := testingConnectStateLocation("tapped")
	held.armed.Store(true)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		deviceRemote.Reconnect(location)
	}()
	select {
	case <-held.entered:
	case <-firstDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the first write is stuck")
	}
	held.armed.Store(false)

	// the second write, from another goroutine, completes meanwhile
	deviceRemote.SetConnectLocation(nil)
	// the device reports both writes, in order
	testingReportConnectState(t, deviceRemote, location, true)
	testingReportConnectState(t, deviceRemote, nil, false)

	releaseHeld()
	select {
	case <-firstDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the first write did not return")
	}

	events := recorder.get()
	if len(events) == 0 || events[len(events)-1] != "connect:false" {
		t.Fatalf("the connect listener last heard %v; the device is disconnected", events)
	}
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
}

// A fake live device that holds a connect location, answers the getters from
// it, applies connect writes to it, and registers listeners without replaying
// the current state, as DeviceLocalRpc does while the rpc is live.
type testingConnectStateFakeDevice struct {
	stateLock sync.Mutex
	location  *ConnectLocation
}

func (self *testingConnectStateFakeDevice) set(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.location = location
}

func (self *testingConnectStateFakeDevice) get() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.location
}

func (self *testingConnectStateFakeDevice) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.set(location.toConnectLocation())
	return nil
}

func (self *testingConnectStateFakeDevice) Reconnect(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.set(location.toConnectLocation())
	return nil
}

func (self *testingConnectStateFakeDevice) RemoveDestination(_ RpcNoArg, _ RpcVoid) error {
	self.set(nil)
	return nil
}

func (self *testingConnectStateFakeDevice) GetConnectLocation(_ RpcNoArg, location **DeviceRemoteConnectLocation) error {
	*location = newDeviceRemoteConnectLocation(self.get())
	return nil
}

func (self *testingConnectStateFakeDevice) GetConnectEnabled(_ RpcNoArg, connectEnabled *bool) error {
	*connectEnabled = self.get() != nil
	return nil
}

func (self *testingConnectStateFakeDevice) AddConnectLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingConnectStateFakeDevice) RemoveConnectLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingConnectStateFakeDevice) AddConnectChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

func (self *testingConnectStateFakeDevice) RemoveConnectChangeListener(listenerId connect.Id, _ RpcVoid) error {
	return nil
}

// While the remote has no connect listener the device pushes nothing, so what
// the remote last told can go stale. A consumer that registers later and seeds
// itself from the getters, as the ConnectViewController and the apps' view
// models do, must still hear the device's next change.
func TestDeviceRemoteConnectListenerGapThenDeviceDisconnectIsTold(t *testing.T) {
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	device := &testingConnectStateFakeDevice{}
	testingInstallFakeDeviceService(t, deviceRemote, device)

	location := testingConnectStateLocation("tapped")

	// a first consumer; the user connects, then disconnects, from the app
	first := &testingConnectStateRecorder{}
	firstLocationSub := deviceRemote.AddConnectLocationChangeListener(first)
	firstConnectSub := deviceRemote.AddConnectChangeListener(first)
	deviceRemote.Reconnect(location)
	deviceRemote.SetConnectLocation(nil)
	// the device's report of the disconnect
	testingReportConnectState(t, deviceRemote, nil, false)
	testingAssertConnectStateLastHeard(t, deviceRemote, first, "nil", false)

	// the consumer goes away
	firstLocationSub.Close()
	firstConnectSub.Close()

	// with no remote listener the device connects on its own (a widget, a
	// shortcut, on demand) and pushes nothing
	device.set(location)

	// a new consumer registers and seeds itself from the getters
	second := &testingConnectStateRecorder{}
	secondLocationSub := deviceRemote.AddConnectLocationChangeListener(second)
	defer secondLocationSub.Close()
	secondConnectSub := deviceRemote.AddConnectChangeListener(second)
	defer secondConnectSub.Close()
	second.ConnectLocationChanged(deviceRemote.GetConnectLocation())
	second.ConnectChanged(deviceRemote.GetConnectEnabled())
	testingAssertConnectStateLastHeard(t, deviceRemote, second, "tapped", true)

	// the device disconnects on its own and reports it
	device.set(nil)
	testingReportConnectState(t, deviceRemote, nil, false)
	testingAssertConnectStateLastHeard(t, deviceRemote, second, "nil", false)
}

// End to end over the device rpc. The device is connected. A disconnect tap
// lands while the first sync is in flight, so it is pending and the first
// sync's replay is dropped. The first sync's reply (still connected) must not
// replace the pending tap as last known: the next sync takes the tap, and while
// it is in flight a view opened then seeds itself from the getters. The
// device's replay of the applied disconnect equals what the listeners were
// told and is not told again, so the view must already seed disconnected.
func TestDeviceRemoteViewOpenedDuringFollowUpSyncEndsDisconnected(t *testing.T) {
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

	recorder := &testingConnectStateRecorder{}
	connectLocationSub := deviceRemote.AddConnectLocationChangeListener(recorder)
	defer connectLocationSub.Close()
	connectSub := deviceRemote.AddConnectChangeListener(recorder)
	defer connectSub.Close()
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
	deviceLocal.SetConnectLocation(testingConnectStateLocation("connected"))
	deviceRemote.Sync()

	select {
	case <-firstSync.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first sync never started")
	}
	// the disconnect tap, while the first sync is in flight
	deviceRemote.SetConnectLocation(nil)
	releaseFirstSync()

	select {
	case <-followUpSync.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the follow-up sync never started")
	}
	// the follow-up sync took the tap and is in flight
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
	if location := deviceRemote.GetConnectLocation(); location != nil {
		t.Fatalf("during the follow-up sync the remote answers the location %q", location.Name)
	}
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)
	releaseFollowUpSync()

	applied := testing_waitFor(30*time.Second, func() bool {
		return deviceRemote.GetRemoteConnected() && deviceLocal.GetConnectLocation() == nil
	})
	if !applied {
		t.Fatal("the device never applied the disconnect")
	}
	// the device's pushes keep their first-queued order and the default
	// location is replayed after the connect state, so once the remote hears
	// this later default location the replay has been handled
	deviceLocal.SetDefaultLocation(testingConnectStateLocation("barrier"))
	select {
	case <-barrier.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote never heard the device's later default location")
	}
	if controller.GetConnected() {
		t.Fatalf("the view shows connected (status %s) while the device is disconnected", controller.GetConnectionStatus())
	}
	connect.AssertEqual(t, controller.GetConnectionStatus(), Disconnected)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "nil", false)
}

// Records remote connection changes and resets of one window, in order.
type testingRpcReconnectLog struct {
	stateLock sync.Mutex
	events    []string
}

func (self *testingRpcReconnectLog) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *testingRpcReconnectLog) RemoteChanged(remoteConnected bool) {
	self.add(fmt.Sprintf("remote:%t", remoteConnected))
}

func (self *testingRpcReconnectLog) get() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// whether a window reset followed the remote going down
func (self *testingRpcReconnectLog) resetAfterReconnect() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	down := slices.Index(self.events, "remote:false")
	return 0 <= down &&
		slices.Contains(self.events[down:], "remote:true") &&
		slices.Contains(self.events[down:], "window:reset")
}

// End to end over the device rpc: across an rpc reconnect, the device's replay
// of the connect location it already told is not told again, so the view keeps
// its grid, and the device's reset snapshot at the end of the sync reaches the
// kept grid's window.
func TestDeviceRemoteConnectGridKeptAcrossRpcReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	recorder := &testingConnectStateRecorder{}
	connectLocationSub := deviceRemote.AddConnectLocationChangeListener(recorder)
	defer connectLocationSub.Close()
	barrier := &testingDefaultLocationBarrier{reached: make(chan struct{})}
	barrierSub := deviceRemote.AddDefaultLocationChangeListener(barrier)
	defer barrierSub.Close()
	controller := deviceRemote.OpenConnectViewController()
	defer deviceRemote.CloseViewController(controller)

	location := testingConnectStateLocation("connected")
	controller.Connect(location)
	grid := controller.GetGrid()
	if grid == nil {
		t.Fatal("the connect built no grid")
	}
	// the device's echo of the connect is handled before its later default
	// location (see TestDeviceRemoteConnectTapDuringSyncReachesDevice)
	deviceLocal.SetDefaultLocation(testingConnectStateLocation("barrier"))
	select {
	case <-barrier.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the remote never heard the device's later default location")
	}
	connect.AssertEqual(t, recorder.get(), []string{"location:connected"})
	if controller.GetGrid() != grid {
		t.Fatal("the device's echo of the connect replaced the grid")
	}

	log := &testingRpcReconnectLog{}
	remoteSub := deviceRemote.AddRemoteChangeListener(log)
	defer remoteSub.Close()
	unsubscribeWindow := grid.windowMonitor.AddMonitorEventCallback(func(
		windowExpandEvent *connect.WindowExpandEvent,
		providerEvents map[connect.Id]*connect.ProviderEvent,
		reset bool,
	) {
		if reset {
			log.add("window:reset")
		}
	})
	defer unsubscribeWindow()

	// a resync request tears the live rpc down and syncs again
	deviceRemote.Sync()
	if !testing_waitFor(30*time.Second, log.resetAfterReconnect) {
		t.Fatalf("no window reset after the rpc reconnect: %v", log.get())
	}

	if controller.GetGrid() != grid {
		t.Fatal("the view replaced its grid across the rpc reconnect")
	}
	connect.AssertEqual(t, recorder.get(), []string{"location:connected"})
	connect.AssertEqual(t, deviceRemote.GetConnectLocation().Name, "connected")
}
