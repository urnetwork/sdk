// Exercises the actual location RPC/codec and service-instance admission with
// explicit reply barriers. These fixtures do not run an app or change native
// startup policy; the full remote lifecycle has separate integration coverage.
package sdk

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Delegates healthy reads to the production server method. Optional barriers
// hold that exact RPC before its reply without depending on scheduler timing.
type testingCheckedLocationRpc struct {
	localRpc    *DeviceLocalRpc
	entered     chan struct{}
	release     <-chan struct{}
	enteredOnce sync.Once
	responseErr error
}

func (self *testingCheckedLocationRpc) GetConnectLocation(noArg RpcNoArg, reply **DeviceRemoteConnectLocation) error {
	if self.entered != nil {
		self.enteredOnce.Do(func() { close(self.entered) })
	}
	if self.release != nil {
		<-self.release
	}
	if self.responseErr != nil {
		return self.responseErr
	}
	return self.localRpc.GetConnectLocation(noArg, reply)
}

// Every socket and serving goroutine is closed/joined, including assertion
// failures. Deadlines below are failure guards, not the proof of an ordering.
func testingCheckedLocationService(t *testing.T, ctx context.Context, server *testingCheckedLocationRpc) *rpcClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("DeviceLocalRpc", server); err != nil {
		clientConn.Close()
		serverConn.Close()
		t.Fatal(err)
	}
	service := &rpcClient{
		ctx:         ctx,
		log:         connect.DefaultLogger(),
		timeout:     10 * time.Second,
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
		clientConn.Close()
		serverConn.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("location RPC server did not join")
		}
	})
	return service
}

// Only the state needed by the real observation method is constructed here.
func testingCheckedLocationRemote(service *rpcClient) *DeviceRemote {
	return &DeviceRemote{
		ctx:             context.Background(),
		settings:        defaultDeviceRpcSettings(),
		service:         service,
		remoteConnected: service != nil,
	}
}

// A particular provider, rather than BestAvailable, exposes lost selection.
func testingCheckedSpecificLocation() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{ClientId: NewId()},
		Name:              "specific selection",
	}
}

// Observes one asynchronously issued method after a separately controlled
// server barrier. Closing done publishes the result and joins the caller.
type testingCheckedLocationCall struct {
	done     chan struct{}
	location *ConnectLocation
	err      error
}

func testingStartCheckedLocationCall(t *testing.T, device *DeviceRemote) *testingCheckedLocationCall {
	t.Helper()
	call := &testingCheckedLocationCall{done: make(chan struct{})}
	go func() {
		defer close(call.done)
		call.location, call.err = device.GetConnectLocationChecked()
	}()
	t.Cleanup(func() {
		select {
		case <-call.done:
		case <-time.After(15 * time.Second):
			t.Error("checked location caller did not join")
		}
	})
	return call
}

// The channel carries a positive state transition; the timer only diagnoses
// a broken test/run instead of hanging the package indefinitely.
func testingAwaitCheckedLocation(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("checked location transition did not complete")
	}
}

// Real server/codec success preserves a specific selection and value ownership.
func TestDeviceRemoteCheckedLocationSpecific(t *testing.T) {
	location := testingCheckedSpecificLocation()
	local := &DeviceLocal{connectLocation: cloneConnectLocation(location)}
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: local},
	})
	device := testingCheckedLocationRemote(service)
	got, err := device.GetConnectLocationChecked()
	if err != nil || !connectLocationValuesEqual(got, location) {
		t.Fatalf("successful specific location was lost: location=%v err=%v", got, err)
	}
	got.Name = "caller mutation"
	if !connectLocationValuesEqual(local.GetConnectLocation(), location) {
		t.Fatal("caller-owned observation mutated the server location")
	}
}

// Successful inner nil is distinct from an RPC failure and from a pending
// local preference. It does not assert that the transport has no consumer.
func TestDeviceRemoteCheckedLocationSuccessfulNil(t *testing.T) {
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{}},
	})
	device := testingCheckedLocationRemote(service)
	pending := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	device.state.Location.Set(pending)
	got, err := device.GetConnectLocationChecked()
	if got != nil || err != nil {
		t.Fatalf("successful no-location response was not nil success: location=%v err=%v", got, err)
	}
	if device.state.Location.Value != pending {
		t.Fatal("successful observation changed a pending destination")
	}
}

// Absence of a service is unknown observation, not healthy no-location.
func TestDeviceRemoteCheckedLocationUnavailableHasNoFallback(t *testing.T) {
	device := testingCheckedLocationRemote(nil)
	pending := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	device.state.Location.Set(pending)
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationUnavailable) {
		t.Fatal("unavailable location observation became a successful fallback")
	}
	if device.state.Location.Value != pending || !device.state.Location.IsSet {
		t.Fatal("unavailable observation changed pending preferences")
	}
}

// Failed RPC returns a fixed stage error even when both compatibility caches
// contain values. The unchanged legacy getter still exposes its old fallback.
func TestDeviceRemoteCheckedLocationRpcErrorPreservesLegacyFallback(t *testing.T) {
	const serverPrivateText = "synthetic private peer diagnostic"
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		responseErr: errors.New(serverPrivateText),
	})
	device := testingCheckedLocationRemote(service)
	pending := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	known := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	device.state.Location.Set(pending)
	device.lastKnownState.Location.Set(known)
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationReadFailed) {
		t.Fatal("failed location RPC became a successful cached observation")
	}
	if strings.Contains(err.Error(), serverPrivateText) {
		t.Fatal("checked location exposed arbitrary peer error text")
	}
	if device.state.Location.Value != pending || device.lastKnownState.Location.Value != known {
		t.Fatal("failed observation changed cached or pending preferences")
	}
	if device.getService() != nil {
		t.Fatal("failed current service was not retired")
	}
	if !connectLocationValuesEqual(device.GetConnectLocation(), pending.toConnectLocation()) {
		t.Fatal("legacy pending-location fallback changed")
	}
	device.state.Location.Unset()
	if !connectLocationValuesEqual(device.GetConnectLocation(), known.toConnectLocation()) {
		t.Fatal("legacy last-known-location fallback changed")
	}
}

// Closed state cannot supply an admitted observation through a still
// referenced service. The live RPC is left available for its owning cleanup.
func TestDeviceRemoteCheckedLocationClosed(t *testing.T) {
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: testingCheckedSpecificLocation()}},
	})
	device := testingCheckedLocationRemote(service)
	device.closed = true
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationUnavailable) {
		t.Fatal("closed remote admitted a current location")
	}
	if device.getService() != service {
		t.Fatal("closed observation altered service ownership")
	}
}

// Browser synchronous getters must not reach the private browser RPC service.
func TestDeviceRemoteCheckedLocationBrowserDoesNotCallRpc(t *testing.T) {
	entered := make(chan struct{})
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{}},
		entered:  entered,
	})
	device := testingCheckedLocationRemote(nil)
	device.settings.BrowserStateOnly = true
	device.browserService = service
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationUnavailable) {
		t.Fatal("browser-only synchronous observation became healthy absence")
	}
	// The method has returned: if it had issued this synchronous RPC, the
	// handler's positive entry event would necessarily have preceded return.
	select {
	case <-entered:
		t.Fatal("synchronous browser getter issued an RPC")
	default:
	}
}

// A late successful reply cannot become the current observation of a newer
// published service, even though the connected flag is true again.
func TestDeviceRemoteCheckedLocationRejectsSupersededSuccess(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseReply := sync.OnceFunc(func() { close(release) })
	defer releaseReply()
	oldService := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: testingCheckedSpecificLocation()}},
		entered:  entered,
		release:  release,
	})
	currentLocation := testingCheckedSpecificLocation()
	newService := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: currentLocation}},
	})
	device := testingCheckedLocationRemote(oldService)
	pending := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	known := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	device.state.Location.Set(pending)
	device.lastKnownState.Location.Set(known)
	call := testingStartCheckedLocationCall(t, device)
	testingAwaitCheckedLocation(t, entered)
	device.stateLock.Lock()
	device.service = newService
	device.remoteConnected = true
	device.stateLock.Unlock()
	releaseReply()
	testingAwaitCheckedLocation(t, call.done)
	if call.location != nil || !errors.Is(call.err, errDeviceRemoteLocationSuperseded) {
		t.Fatal("old successful RPC was admitted after service replacement")
	}
	if device.state.Location.Value != pending || device.lastKnownState.Location.Value != known {
		t.Fatal("old observation overwrote newer cached or pending preferences")
	}
	got, err := device.GetConnectLocationChecked()
	if err != nil || !connectLocationValuesEqual(got, currentLocation) {
		t.Fatal("new service was not usable after rejecting the old reply")
	}
}

// A failed old request stays failed; its cleanup cannot close a newly
// published connection and a later connected flag does not repair that read.
func TestDeviceRemoteCheckedLocationFailedOldCallKeepsNewService(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseReply := sync.OnceFunc(func() { close(release) })
	defer releaseReply()
	oldService := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		entered:     entered,
		release:     release,
		responseErr: errors.New("synthetic old RPC failure"),
	})
	currentLocation := testingCheckedSpecificLocation()
	newService := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: currentLocation}},
	})
	device := testingCheckedLocationRemote(oldService)
	call := testingStartCheckedLocationCall(t, device)
	testingAwaitCheckedLocation(t, entered)
	device.stateLock.Lock()
	device.service = newService
	device.remoteConnected = true
	device.stateLock.Unlock()
	releaseReply()
	testingAwaitCheckedLocation(t, call.done)
	if call.location != nil || !errors.Is(call.err, errDeviceRemoteLocationReadFailed) {
		t.Fatal("old failed RPC was converted into a successful observation")
	}
	if device.getService() != newService || !device.remoteConnected {
		t.Fatal("failed old RPC retired the replacement service")
	}
	got, err := device.GetConnectLocationChecked()
	if err != nil || !connectLocationValuesEqual(got, currentLocation) {
		t.Fatal("failed old call made the replacement unusable")
	}
}

// Closing the remote while a response is held retires its admission without
// needing a new connection or a second user action.
func TestDeviceRemoteCheckedLocationRejectsCloseDuringReply(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseReply := sync.OnceFunc(func() { close(release) })
	defer releaseReply()
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: testingCheckedSpecificLocation()}},
		entered:  entered,
		release:  release,
	})
	device := testingCheckedLocationRemote(service)
	call := testingStartCheckedLocationCall(t, device)
	testingAwaitCheckedLocation(t, entered)
	device.stateLock.Lock()
	device.closed = true
	device.stateLock.Unlock()
	releaseReply()
	testingAwaitCheckedLocation(t, call.done)
	if call.location != nil || !errors.Is(call.err, errDeviceRemoteLocationSuperseded) {
		t.Fatal("closed remote admitted an already-issued response")
	}
}

// A cancelled remote has no current observation even before teardown clears
// its service pointer.
func TestDeviceRemoteCheckedLocationCancelledRemote(t *testing.T) {
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{}},
	})
	device := testingCheckedLocationRemote(service)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	device.ctx = ctx
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationUnavailable) {
		t.Fatal("cancelled remote reported a successful location observation")
	}
}

// The service lifecycle may end before the remote loop withdraws its pointer.
// Do not issue a new request on that already-cancelled generation.
func TestDeviceRemoteCheckedLocationCancelledService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := testingCheckedLocationService(t, ctx, &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{}},
	})
	device := testingCheckedLocationRemote(service)
	got, err := device.GetConnectLocationChecked()
	if got != nil || !errors.Is(err, errDeviceRemoteLocationUnavailable) {
		t.Fatal("cancelled service was treated as a usable current observation")
	}
}

// A read is not a preference mutation or replay. In particular, current
// pending/custom intent and a later cached event must not be overwritten.
func TestDeviceRemoteCheckedLocationLeavesAllPreferenceStateUntouched(t *testing.T) {
	location := testingCheckedSpecificLocation()
	service := testingCheckedLocationService(t, context.Background(), &testingCheckedLocationRpc{
		localRpc: &DeviceLocalRpc{deviceLocal: &DeviceLocal{connectLocation: location}},
	})
	device := testingCheckedLocationRemote(service)
	pending := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	known := newDeviceRemoteConnectLocation(testingCheckedSpecificLocation())
	destination := &DeviceRemoteDestination{Location: known}
	device.state.Location.Set(pending)
	device.lastKnownState.Location.Set(known)
	device.lastKnownState.Destination.Set(destination)
	device.lastKnownState.RemoveDestination.Set(true)
	got, err := device.GetConnectLocationChecked()
	if err != nil || !connectLocationValuesEqual(got, location) {
		t.Fatal("current location RPC did not return its actual value")
	}
	if device.state.Location.Value != pending || !device.state.Location.IsSet ||
		device.lastKnownState.Location.Value != known || !device.lastKnownState.Location.IsSet ||
		device.lastKnownState.Destination.Value != destination || !device.lastKnownState.Destination.IsSet ||
		!device.lastKnownState.RemoveDestination.IsSet || !device.lastKnownState.RemoveDestination.Value {
		t.Fatal("checked observation overwrote cached or pending preference state")
	}
}
