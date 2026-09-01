// Device RPC reconnect tests guard retry pacing and explicit low-latency wakes.
package sdk

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type testingFailingDeviceRpcDialer struct {
	attemptTimes chan time.Time
}

type testingBlockedDeviceRpcDialer struct {
	entered chan struct{}
	release chan struct{}
}

func (self *testingBlockedDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	select {
	case self.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	select {
	case <-self.release:
		return nil, nil, errors.New("released blocked dial")
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (self *testingFailingDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case self.attemptTimes <- time.Now():
		return nil, nil, errors.New("expected dial failure")
	}
}

type testingFailingDeviceRpcListener struct {
	attemptTimes chan time.Time
	closeCount   atomic.Int64
}

func (self *testingFailingDeviceRpcListener) Accept(ctx context.Context) (net.Conn, net.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case self.attemptTimes <- time.Now():
		return nil, nil, errors.New("expected accept failure")
	}
}

func (self *testingFailingDeviceRpcListener) Close() error {
	self.closeCount.Add(1)
	return nil
}

type testingCountingDeviceRpcLogger struct {
	infoCount    atomic.Int64
	warningCount atomic.Int64
	errorCount   atomic.Int64
}

func (self *testingCountingDeviceRpcLogger) Info(args ...any) {
	self.infoCount.Add(1)
}

func (self *testingCountingDeviceRpcLogger) Infof(format string, args ...any) {
	self.infoCount.Add(1)
}

func (self *testingCountingDeviceRpcLogger) Warningf(format string, args ...any) {
	self.warningCount.Add(1)
}

func (self *testingCountingDeviceRpcLogger) Errorf(format string, args ...any) {
	self.errorCount.Add(1)
}

func (self *testingCountingDeviceRpcLogger) V(level int32) connect.Verbose {
	return testingDisabledDeviceRpcVerbose{}
}

type testingDisabledDeviceRpcVerbose struct {
}

func (self testingDisabledDeviceRpcVerbose) Enabled() bool {
	return false
}

func (self testingDisabledDeviceRpcVerbose) Info(args ...any) {
}

func (self testingDisabledDeviceRpcVerbose) Infof(format string, args ...any) {
}

func testingNewFailingDeviceRemote(
	reconnectTimeout time.Duration,
) (*DeviceRemote, *testingFailingDeviceRpcDialer, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	settings.RpcReconnectTimeout = reconnectTimeout
	dialer := &testingFailingDeviceRpcDialer{
		attemptTimes: make(chan time.Time, 16),
	}
	deviceRemote := &DeviceRemote{
		ctx:              ctx,
		cancel:           cancel,
		log:              settings.logger(),
		settings:         settings,
		reconnectMonitor: connect.NewMonitor(),
		syncMonitor:      connect.NewMonitor(),
		dialer:           dialer,
		dialerChanged:    make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		deviceRemote.run()
	}()
	return deviceRemote, dialer, done
}

// A transport replacement must publish its fresh notification channel in the
// same generation as its dialer. Otherwise a connection made with the new
// dialer can consume the old closed edge and immediately tear itself down.
func TestDeviceRemoteDialerReplacementPublishesOneGeneration(t *testing.T) {
	settings := defaultDeviceRpcSettings()
	deviceRemote := &DeviceRemote{
		settings:      settings,
		log:           settings.logger(),
		dialer:        &testingFailingDeviceRpcDialer{attemptTimes: make(chan time.Time, 1)},
		dialerChanged: make(chan struct{}),
	}
	oldDialerChanged := deviceRemote.dialerChanged

	err := deviceRemote.SetRpcServer("", "", "127.0.0.1:12042")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-oldDialerChanged:
	default:
		t.Fatal("replaced dialer generation was not notified")
	}
	dialer, dialerChanged := func() (deviceRpcDialer, <-chan struct{}) {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return deviceRemote.dialer, deviceRemote.dialerChanged
	}()
	if dialer == nil {
		t.Fatal("replacement dialer is nil")
	}
	select {
	case <-dialerChanged:
		t.Fatal("replacement dialer was paired with the stale closed edge")
	default:
	}
}

func testingReceiveDeviceRpcAttempt(
	t *testing.T,
	attemptTimes <-chan time.Time,
	timeout time.Duration,
) time.Time {
	t.Helper()
	select {
	case attemptTime := <-attemptTimes:
		return attemptTime
	case <-time.After(timeout):
		t.Fatal("timed out waiting for device rpc attempt")
		return time.Time{}
	}
}

// Browser WebSocket completion is delivered by the same JavaScript event loop
// that invokes synchronous getters. Holding stateLock across Dial makes the
// getter wait for an event that cannot run until the getter returns, freezing
// the page and hot-spinning the wasm runtime.
func TestDeviceRemoteGetterDoesNotWaitBehindBlockedDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	dialer := &testingBlockedDeviceRpcDialer{
		entered: make(chan struct{}, 1),
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
	defer deviceRemote.Close()

	select {
	case <-dialer.entered:
	case <-time.After(time.Second):
		t.Fatal("device rpc dial did not reach the deterministic barrier")
	}
	getterResult := make(chan bool, 1)
	go func() {
		getterResult <- deviceRemote.GetRemoteConnected()
	}()
	select {
	case connected := <-getterResult:
		if connected {
			t.Fatal("blocked dial reported a connected remote")
		}
	case <-time.After(time.Second):
		t.Fatal("state getter waited behind external device rpc dial")
	}
	close(dialer.release)
}

func TestDeviceRemoteFailedSyncRestoreKeepsNewerPendingState(t *testing.T) {
	deviceRemote := &DeviceRemote{
		settings: defaultDeviceRpcSettings(),
		state: DeviceRemoteState{
			BlockerEnabled: deviceRemoteValue[bool]{Value: false, IsSet: true},
		},
	}

	request := deviceRemote.takeSyncRequest()
	deviceRemote.state.BlockerEnabled.Set(true)
	deviceRemote.restoreSyncState(request.State)
	if !deviceRemote.state.BlockerEnabled.IsSet || !deviceRemote.state.BlockerEnabled.Value {
		t.Fatal("failed sync restoration overwrote a newer pending blocker value")
	}
}

// TestDeviceRemotePacesFailedDialAttempts verifies a missing local extension
// cannot cause randomly clustered reconnect work.
func TestDeviceRemotePacesFailedDialAttempts(t *testing.T) {
	reconnectTimeout := 40 * time.Millisecond
	deviceRemote, dialer, done := testingNewFailingDeviceRemote(reconnectTimeout)
	defer func() {
		deviceRemote.cancel()
		<-done
	}()

	previousTime := testingReceiveDeviceRpcAttempt(t, dialer.attemptTimes, time.Second)
	for range 3 {
		attemptTime := testingReceiveDeviceRpcAttempt(t, dialer.attemptTimes, time.Second)
		if elapsed := attemptTime.Sub(previousTime); elapsed < reconnectTimeout {
			t.Fatalf("failed dials were %s apart, want at least %s", elapsed, reconnectTimeout)
		}
		previousTime = attemptTime
	}
}

// TestDeviceRemoteSyncBypassesFailedDialPacing verifies an explicit lifecycle
// wake still attempts immediately instead of adding page-load latency.
func TestDeviceRemoteSyncBypassesFailedDialPacing(t *testing.T) {
	reconnectTimeout := 500 * time.Millisecond
	deviceRemote, dialer, done := testingNewFailingDeviceRemote(reconnectTimeout)
	defer func() {
		deviceRemote.cancel()
		<-done
	}()

	firstTime := testingReceiveDeviceRpcAttempt(t, dialer.attemptTimes, time.Second)
	deviceRemote.Sync()
	secondTime := testingReceiveDeviceRpcAttempt(t, dialer.attemptTimes, time.Second)
	if elapsed := secondTime.Sub(firstTime); elapsed >= reconnectTimeout {
		t.Fatalf("explicit sync waited %s for reconnect pace %s", elapsed, reconnectTimeout)
	}
}

// TestDeviceLocalRpcManagerPacesRepeatedAcceptErrors verifies a persistent
// listener error cannot reuse an expired deadline and hot-spin.
func TestDeviceLocalRpcManagerPacesRepeatedAcceptErrors(t *testing.T) {
	reconnectTimeout := 40 * time.Millisecond
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	settings.RpcReconnectTimeout = reconnectTimeout
	listener := &testingFailingDeviceRpcListener{
		attemptTimes: make(chan time.Time, 16),
	}
	deviceLocal := &DeviceLocal{
		log: settings.logger(),
	}
	manager := newDeviceLocalRpcManager(
		context.Background(),
		deviceLocal,
		settings,
		listener,
	)
	defer manager.Close()

	previousTime := testingReceiveDeviceRpcAttempt(t, listener.attemptTimes, time.Second)
	for range 3 {
		attemptTime := testingReceiveDeviceRpcAttempt(t, listener.attemptTimes, time.Second)
		if elapsed := attemptTime.Sub(previousTime); elapsed < reconnectTimeout {
			t.Fatalf("accept errors were %s apart, want at least %s", elapsed, reconnectTimeout)
		}
		previousTime = attemptTime
	}
}

// TestDeviceLocalRpcManagerLogsRepeatedAcceptErrorOnce verifies a persistent
// listener failure does not turn its retry pace into repeated log work.
func TestDeviceLocalRpcManagerLogsRepeatedAcceptErrorOnce(t *testing.T) {
	logger := &testingCountingDeviceRpcLogger{}
	settings := defaultDeviceRpcSettings()
	settings.ClientSettings.Log = logger
	settings.RpcReconnectTimeout = 10 * time.Millisecond
	listener := &testingFailingDeviceRpcListener{
		attemptTimes: make(chan time.Time, 16),
	}
	deviceLocal := &DeviceLocal{
		log: settings.logger(),
	}
	manager := newDeviceLocalRpcManager(
		context.Background(),
		deviceLocal,
		settings,
		listener,
	)

	for range 4 {
		testingReceiveDeviceRpcAttempt(t, listener.attemptTimes, time.Second)
	}
	manager.Close()
	time.Sleep(20 * time.Millisecond)
	if infoCount := logger.infoCount.Load(); infoCount != 1 {
		t.Fatalf("repeated accept failure logged %d times, want one", infoCount)
	}
}

func TestDeviceRemoteDefaultLocationFallsBackToLastKnownValue(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "persisted",
	}
	device.lastKnownState.DefaultLocation.Set(newDeviceRemoteDefaultLocation(location))

	got := device.GetDefaultLocation()

	if !connectLocationValuesEqual(got, location) {
		t.Fatalf("default location fallback = %+v, want %+v", got, location)
	}
}

func TestDeviceRemoteEquivalentDefaultLocationIsNoOp(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "persisted",
	}
	device.lastKnownState.DefaultLocation.Set(newDeviceRemoteDefaultLocation(location))

	device.SetDefaultLocation(cloneConnectLocation(location))

	if device.state.DefaultLocation.IsSet {
		t.Fatalf("equivalent default location was queued for rpc")
	}
}

func TestDeviceRemoteChangedDefaultLocationIsQueued(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "persisted",
	}
	device.lastKnownState.DefaultLocation.Set(newDeviceRemoteDefaultLocation(location))
	changed := cloneConnectLocation(location)
	changed.Name = "updated"

	device.SetDefaultLocation(changed)

	if !device.state.DefaultLocation.IsSet {
		t.Fatalf("changed default location was not queued for rpc")
	}
}

func TestDeviceRemoteConnectLocationFallsBackToLastKnownDestination(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "destination",
	}
	device.lastKnownState.Destination.Set(&DeviceRemoteDestination{
		Location: newDeviceRemoteConnectLocation(location),
	})

	got := device.GetConnectLocation()

	if !connectLocationValuesEqual(got, location) {
		t.Fatalf("connect location fallback = %+v, want %+v", got, location)
	}
}

func TestDeviceRemotePendingDestinationRemovalOverridesLastKnownLocation(t *testing.T) {
	device := &DeviceRemote{}
	device.lastKnownState.Location.Set(newDeviceRemoteConnectLocation(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}))
	device.state.RemoveDestination.Set(true)

	if got := device.GetConnectLocation(); got != nil {
		t.Fatalf("pending removal returned stale location: %+v", got)
	}
}

func TestDeviceRemoteEquivalentConnectLocationIsNoOp(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "persisted",
	}
	pending := newDeviceRemoteConnectLocation(location)
	device.state.Location.Set(pending)

	device.SetConnectLocation(cloneConnectLocation(location))

	if !device.state.Location.IsSet {
		t.Fatalf("equivalent pending connect location was lost")
	}
	if device.state.Location.Value != pending {
		t.Fatalf("equivalent pending connect location was re-queued")
	}
	if got := device.state.Location.Value.toConnectLocation(); !connectLocationValuesEqual(got, location) {
		t.Fatalf("equivalent pending connect location changed: %+v", got)
	}
}

func TestDeviceRemoteConnectLocationDoesNotSuppressCustomDestinationChange(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	device.lastKnownState.Destination.Set(&DeviceRemoteDestination{
		Location: newDeviceRemoteConnectLocation(location),
		Specs: []*ProviderSpec{{
			ClientId: NewId(),
		}},
	})

	device.SetConnectLocation(cloneConnectLocation(location))

	if !device.state.Location.IsSet {
		t.Fatalf("connect-location command was suppressed by custom destination metadata")
	}
	if device.state.Destination.IsSet {
		t.Fatalf("custom destination remained pending after connect-location command")
	}
}

func TestDeviceRemoteEquivalentDestinationRemovalIsNoOp(t *testing.T) {
	device := &DeviceRemote{}
	device.lastKnownState.RemoveDestination.Set(true)

	device.RemoveDestination()

	if device.state.RemoveDestination.IsSet {
		t.Fatalf("equivalent destination removal was queued for rpc")
	}
}

func TestDeviceRemoteDestinationOwnsQueuedProviderSpecs(t *testing.T) {
	device := &DeviceRemote{settings: defaultDeviceRpcSettings()}
	clientId := NewId()
	spec := &ProviderSpec{ClientId: clientId}
	specs := NewProviderSpecList()
	specs.Add(spec)

	device.SetDestination(&ConnectLocation{}, specs)
	replacementId := NewId()
	spec.ClientId = replacementId

	if !device.state.Destination.IsSet {
		t.Fatalf("destination was not queued")
	}
	queued := device.state.Destination.Value
	if len(queued.Specs) != 1 || queued.Specs[0] == nil || queued.Specs[0].ClientId == nil {
		t.Fatalf("queued provider specs missing: %+v", queued.Specs)
	}
	if queued.Specs[0].ClientId.Cmp(clientId) != 0 {
		t.Fatalf("queued provider spec followed caller mutation")
	}
}

func TestDeviceRemoteEquivalentKnownDestinationIsNoOp(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	clientId := NewId()
	known := &DeviceRemoteDestination{
		Location: newDeviceRemoteConnectLocation(location),
		Specs:    []*ProviderSpec{{ClientId: cloneId(clientId)}},
	}
	device.lastKnownState.Destination.Set(known)
	specs := NewProviderSpecList()
	specs.Add(&ProviderSpec{ClientId: cloneId(clientId)})

	device.SetDestination(cloneConnectLocation(location), specs)

	if device.state.Destination.IsSet {
		t.Fatalf("equivalent known destination was queued for rpc")
	}
}

func TestDeviceRemoteKnownDestinationDoesNotSuppressPendingLocationOverride(t *testing.T) {
	device := &DeviceRemote{settings: defaultDeviceRpcSettings()}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	clientId := NewId()
	device.lastKnownState.Destination.Set(&DeviceRemoteDestination{
		Location: newDeviceRemoteConnectLocation(location),
		Specs:    []*ProviderSpec{{ClientId: cloneId(clientId)}},
	})
	device.state.Location.Set(newDeviceRemoteConnectLocation(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{ClientId: NewId()},
	}))
	specs := NewProviderSpecList()
	specs.Add(&ProviderSpec{ClientId: cloneId(clientId)})

	device.SetDestination(cloneConnectLocation(location), specs)

	if !device.state.Destination.IsSet {
		t.Fatalf("destination did not override pending connect location")
	}
	if device.state.Location.IsSet {
		t.Fatalf("pending connect location survived destination override")
	}
}

func TestDeviceRemoteKnownRemovalDoesNotSuppressPendingLocationOverride(t *testing.T) {
	device := &DeviceRemote{settings: defaultDeviceRpcSettings()}
	device.lastKnownState.RemoveDestination.Set(true)
	device.state.Location.Set(newDeviceRemoteConnectLocation(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}))

	device.RemoveDestination()

	if !device.state.RemoveDestination.IsSet {
		t.Fatalf("removal did not override pending connect location")
	}
	if device.state.Location.IsSet {
		t.Fatalf("pending connect location survived removal override")
	}
}

func TestDeviceRemoteCustomDestinationWithoutLocationIsConnectEnabled(t *testing.T) {
	device := &DeviceRemote{settings: defaultDeviceRpcSettings()}
	specs := NewProviderSpecList()
	specs.Add(&ProviderSpec{ClientId: NewId()})

	device.SetDestination(nil, specs)

	if !device.GetConnectEnabled() {
		t.Fatalf("spec-only custom destination reported disconnected")
	}
	if got := device.GetConnectLocation(); got != nil {
		t.Fatalf("spec-only custom destination invented a display location: %+v", got)
	}
}

func TestDeviceRemotePendingDefaultLocationOverridesLastKnownNoOp(t *testing.T) {
	device := &DeviceRemote{}
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	device.lastKnownState.DefaultLocation.Set(newDeviceRemoteDefaultLocation(location))
	pending := newDeviceRemoteDefaultLocation(nil)
	device.state.DefaultLocation.Set(pending)

	device.SetDefaultLocation(cloneConnectLocation(location))

	if device.state.DefaultLocation.Value == pending {
		t.Fatalf("last-known no-op suppressed pending default-location override")
	}
	if got := device.state.DefaultLocation.Value.toConnectLocation(); !connectLocationValuesEqual(got, location) {
		t.Fatalf("default-location override was not queued: %+v", got)
	}
}
