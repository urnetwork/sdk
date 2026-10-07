package sdk

// client_limit_status_test.go -- the provide intent a device declares on its
// platform connection and the client limit status it reports
// (client_limit_status.go, device_local_client_limit.go).
//
// The device-level tests run a real device against an in-process platform
// that accepts WebSocket and h1+ upgrades, records the provide intent each
// connection declares, and sends the platform's client limit close on demand
// (connect/transport_client_limit.go). connect's own tests pin the hold's
// timing on a manual clock; these pin what the device declares and reports.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
)

// testingClientLimitPlatformConnection is one platform-side connection.
type testingClientLimitPlatformConnection struct {
	provideIntent string
	conn          connect.H1MessageConn
}

// closeForClientLimit sends the platform's client limit close: the close
// control, the WebSocket close code on WebSocket only, then the socket close.
func (self testingClientLimitPlatformConnection) closeForClientLimit() {
	_ = self.conn.WriteMessage(
		websocket.BinaryMessage,
		[]byte{connect.TransportControlClose, 0, 0, 0, byte(connect.TransportCloseReasonClientLimitExceeded)},
	)
	if ws, ok := self.conn.(*websocket.Conn); ok {
		_ = ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(connect.ClientLimitCloseCode, connect.ClientLimitCloseText),
			time.Now().Add(time.Second),
		)
	}
	self.conn.Close()
}

// testingClientLimitPlatform is an in-process h1 platform on the v4 loopback.
type testingClientLimitPlatform struct {
	url         string
	connections chan testingClientLimitPlatformConnection
	// closed when the test ends, so a handler never waits on a full
	// connections queue that a failed test stopped reading
	closed chan struct{}
}

// newTestingClientLimitPlatform serves the platform on the v4 loopback until
// the test ends.
func newTestingClientLimitPlatform(t *testing.T) *testingClientLimitPlatform {
	t.Helper()
	platform := &testingClientLimitPlatform{
		connections: make(chan testingClientLimitPlatformConnection, 64),
		closed:      make(chan struct{}),
	}
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		connection := testingClientLimitPlatformConnection{
			provideIntent: r.Header.Get(connect.HeaderProvideIntent),
		}
		if r.Header.Get("Upgrade") == connect.H1FramerProtocol {
			raw, err := connect.AcceptFramedUpgrade(w, r, connect.H1FramerProtocol, time.Second)
			if err != nil {
				return
			}
			framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerProtocol, 65535, nil)
			if err != nil {
				raw.Close()
				return
			}
			connection.conn = framed
		} else {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			connection.conn = ws
		}
		defer connection.conn.Close()
		select {
		case platform.connections <- connection:
		case <-platform.closed:
			return
		}
		for {
			if _, _, err := connection.conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	platform.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() {
		close(platform.closed)
		server.CloseClientConnections()
		server.Close()
		handlers.Wait()
	})
	return platform
}

// nextConnectionDeclaring skips connections until one declares provideIntent
// ("" for none), or fails.
func (self *testingClientLimitPlatform) nextConnectionDeclaring(
	t *testing.T,
	provideIntent string,
) testingClientLimitPlatformConnection {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case connection := <-self.connections:
			if connection.provideIntent == provideIntent {
				return connection
			}
		case <-deadline:
			t.Fatalf("no platform connection declared provide intent %q", provideIntent)
			return testingClientLimitPlatformConnection{}
		}
	}
}

// drain discards every connection accepted so far, so a later wait reads only
// connections made after this point.
func (self *testingClientLimitPlatform) drain() {
	for {
		select {
		case <-self.connections:
		default:
			return
		}
	}
}

// testingClientLimitListener records every status a listener receives.
type testingClientLimitListener struct {
	statuses chan *ClientLimitStatus
}

// newTestingClientLimitListener buffers enough statuses for any one test.
func newTestingClientLimitListener() *testingClientLimitListener {
	return &testingClientLimitListener{
		statuses: make(chan *ClientLimitStatus, 64),
	}
}

// ClientLimitStatusChanged records the status.
func (self *testingClientLimitListener) ClientLimitStatusChanged(status *ClientLimitStatus) {
	self.statuses <- status
}

// next waits for the next received status, or fails.
func (self *testingClientLimitListener) next(t *testing.T, message string) *ClientLimitStatus {
	t.Helper()
	select {
	case status := <-self.statuses:
		return status
	case <-time.After(60 * time.Second):
		t.Fatal(message)
		return nil
	}
}

// A device whose provider dials the platform: a url-only space on the
// platform's ip literal, which derives no family urls, so the provider runs
// its single standby transport.
func newTestingClientLimitDevice(
	t *testing.T,
	platform *testingClientLimitPlatform,
	configure func(settings *DeviceLocalSettings),
) (*NetworkSpace, *DeviceLocal) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	networkSpace := NewNetworkSpaceWithUrls(ctx, "https://api.client-limit.example", platform.url, strategySettings)
	t.Cleanup(networkSpace.Close)
	if networkSpace.HasPlatformFamilyUrls() {
		t.Fatal("the ip literal space derived family urls")
	}

	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = true
	settings.ProvideExtenderEnabled = false
	// the dns pump is never dialed while h1 holds, and must never leave the
	// machine if it is
	settings.DnsPumpHost = "pump.client-limit.example"
	if configure != nil {
		configure(settings)
	}
	device, err := newDeviceLocalWithOverrides(networkSpace, "", "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(device.Close)
	return networkSpace, device
}

// waitProviderSettled waits until no migration is in flight and the provider's
// current transports are connected, so a close lands on the generation the
// test means.
func waitProviderSettled(t *testing.T, device *DeviceLocal) {
	t.Helper()
	provider := func() *deviceLocalProvider {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.provider
	}()
	if !waitProviderCondition(60*time.Second, func() bool {
		return !provider.migrating.Load() && provider.IsConnected()
	}) {
		t.Fatal("the provider never settled on a connected generation")
	}
}

// The app form of the hold: none without a hold, exceeded with the retry time
// in unix milliseconds while one is in force.
func TestNewClientLimitStatusMapsTheHold(t *testing.T) {
	connect.AssertEqual(t, newClientLimitStatus(connect.ClientLimitStatus{}), noneClientLimitStatus())
	retryTime := time.Date(2026, 10, 6, 12, 15, 0, 0, time.UTC)
	connect.AssertEqual(
		t,
		newClientLimitStatus(connect.ClientLimitStatus{Exceeded: true, RetryTime: retryTime}),
		&ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: retryTime.UnixMilli()},
	)
	connect.AssertEqual(t, noneClientLimitStatus().Status, ClientLimitStatusNone)
	connect.AssertEqual(t, noneClientLimitStatus().RetryTime, int64(0))
}

// The provider declares provide intent exactly while its mode includes public:
// every rebuilt generation, the generation it replaces, and a token refresh
// carry the declaration of the current mode, and every generation shares the
// provider's client limit hold.
func TestDeviceLocalProviderProvideIntentFollowsThePublicFlag(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModeNetwork)
	provider.clientLimitBackoff = connect.NewClientLimitBackoff()
	current := newFakeMigratablePlatformTransport(provider.auth, true)
	provider.platformTransport = current

	type generation struct {
		auth     *connect.ClientAuth
		settings *connect.PlatformTransportSettings
	}
	generations := make(chan generation, 4)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		generations <- generation{auth: auth, settings: settings}
		return newFakeMigratablePlatformTransport(auth, true)
	}
	nextGeneration := func(message string) generation {
		t.Helper()
		select {
		case built := <-generations:
			return built
		case <-time.After(15 * time.Second):
			t.Fatal(message)
			return generation{}
		}
	}
	waitInstalled := func(previous migratablePlatformTransport) migratablePlatformTransport {
		t.Helper()
		var installed migratablePlatformTransport
		if !waitProviderCondition(15*time.Second, func() bool {
			provider.stateLock.Lock()
			defer provider.stateLock.Unlock()
			installed = provider.platformTransport
			return installed != previous && !provider.migrating.Load()
		}) {
			t.Fatal("the rebuilt generation was not installed")
		}
		return installed
	}
	currentAuth := func(transport migratablePlatformTransport) *connect.ClientAuth {
		fake := transport.(*fakeMigratablePlatformTransport)
		fake.mutex.Lock()
		defer fake.mutex.Unlock()
		return fake.auth
	}

	if provider.auth.ProvideIntent {
		t.Fatal("a network provider declared provide intent")
	}

	provider.setProvideMode(ProvideModePublic)
	public := nextGeneration("the flip to public did not rebuild the transports")
	if !public.auth.ProvideIntent {
		t.Fatal("the public generation does not declare provide intent")
	}
	if public.settings.ClientLimitBackoff != provider.clientLimitBackoff {
		t.Fatal("the public generation does not share the provider's client limit hold")
	}
	if !currentAuth(current).ProvideIntent {
		t.Fatal("the replaced generation did not take the declaration for its redials")
	}
	publicTransport := waitInstalled(current)

	// a token refresh keeps the declaration of the mode
	provider.SetByJwt("refreshed")
	if auth := currentAuth(publicTransport); auth.ByJwt != "refreshed" || !auth.ProvideIntent {
		t.Fatalf("auth after a token refresh = %+v, want the refreshed token declaring intent", auth)
	}

	// public to stream keeps the flag: nothing is rebuilt or redeclared
	provider.setProvideMode(ProvideModeStream)
	select {
	case <-generations:
		t.Fatal("a mode change that keeps the public flag rebuilt the transports")
	default:
	}

	provider.setProvideMode(ProvideModeNetwork)
	network := nextGeneration("the flip back did not rebuild the transports")
	if network.auth.ProvideIntent {
		t.Fatal("the network generation still declares provide intent")
	}
	if network.settings.ClientLimitBackoff != provider.clientLimitBackoff {
		t.Fatal("the network generation does not share the provider's client limit hold")
	}
	if currentAuth(publicTransport).ProvideIntent {
		t.Fatal("the replaced public generation kept declaring provide intent")
	}
	waitInstalled(publicTransport)
}

// The device declares provide intent by itself once it provides publicly. The
// platform's client limit close of that connection holds the device off and
// reports ClientLimitStatusExceeded with a retry time at least 15 minutes out,
// to the getter, the listener and the transport status. Leaving public mode is
// a new declaration: the hold resets, the status returns to none, and the
// device reconnects declaring nothing.
func TestDeviceLocalClientLimitStatusFollowsTheProviderHold(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	_, device := newTestingClientLimitDevice(t, platform, nil)
	listener := newTestingClientLimitListener()
	sub := device.AddClientLimitStatusChangeListener(listener)
	defer sub.Close()

	connect.AssertEqual(t, device.GetClientLimitStatus(), noneClientLimitStatus())
	device.SetProvideMode(ProvideModePublic)
	declared := platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	waitProviderSettled(t, device)

	closeTime := time.Now()
	declared.closeForClientLimit()
	status := listener.next(t, "the client limit close reached no listener")
	if status.Status != ClientLimitStatusExceeded {
		t.Fatalf("listener status = %+v, want %s", status, ClientLimitStatusExceeded)
	}
	if earliest := closeTime.Add(connect.ClientLimitBackoffTimeout).UnixMilli(); status.RetryTime < earliest {
		t.Fatalf("retry time = %d, want at least %d (15 minutes after the close)", status.RetryTime, earliest)
	}
	connect.AssertEqual(t, device.GetClientLimitStatus(), status)
	if !waitProviderCondition(30*time.Second, func() bool {
		return device.GetProviderFamilyTransportStatus().StandbyState == connect.PlatformTransportStateClientLimit.String()
	}) {
		t.Fatalf("transport status = %+v, want the standby holding", device.GetProviderFamilyTransportStatus())
	}

	// held: what the platform accepted so far predates the reset
	platform.drain()
	device.SetProvideMode(ProvideModeNetwork)
	if status := listener.next(t, "the reset reached no listener"); status.Status != ClientLimitStatusNone || status.RetryTime != 0 {
		t.Fatalf("listener status after the reset = %+v, want none", status)
	}
	connect.AssertEqual(t, device.GetClientLimitStatus(), noneClientLimitStatus())
	platform.nextConnectionDeclaring(t, "")
}

// A device dials before it knows its provide mode, so its first connection
// declares nothing, and on a network over its client limit the platform closes
// it. Providing publicly is a new declaration the platform has not judged: the
// hold lifts at once and the device connects declaring intent, instead of
// waiting out a hold that judged the undeclared connection.
func TestDeviceLocalDeclaringProvideIntentLiftsAnUndeclaredHold(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	_, device := newTestingClientLimitDevice(t, platform, nil)
	listener := newTestingClientLimitListener()
	sub := device.AddClientLimitStatusChangeListener(listener)
	defer sub.Close()

	undeclared := platform.nextConnectionDeclaring(t, "")
	waitProviderSettled(t, device)
	undeclared.closeForClientLimit()
	if status := listener.next(t, "the client limit close reached no listener"); status.Status != ClientLimitStatusExceeded {
		t.Fatalf("listener status = %+v, want %s", status, ClientLimitStatusExceeded)
	}

	platform.drain()
	device.SetProvideMode(ProvideModePublic)
	if status := listener.next(t, "the new declaration did not lift the hold"); status.Status != ClientLimitStatusNone {
		t.Fatalf("listener status after the declaration = %+v, want none", status)
	}
	platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	connect.AssertEqual(t, device.GetClientLimitStatus(), noneClientLimitStatus())
}

// The status crosses the device rpc: a remote's listener and getter report
// what the device process reports, and the getter keeps the last value while
// the device process is gone.
func TestDeviceRemoteClientLimitStatus(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	networkSpace := NewNetworkSpaceWithUrls(ctx, "https://api.client-limit.example", platform.url, strategySettings)
	defer networkSpace.Close()
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		func(settings *DeviceLocalSettings) {
			settings.AllowProvider = true
			settings.ProvideExtenderEnabled = false
			settings.DnsPumpHost = "pump.client-limit.example"
		},
	)
	listener := newTestingClientLimitListener()
	sub := deviceRemote.AddClientLimitStatusChangeListener(listener)
	defer sub.Close()
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), noneClientLimitStatus())

	deviceLocal.SetProvideMode(ProvideModePublic)
	declared := platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	waitProviderSettled(t, deviceLocal)
	declared.closeForClientLimit()

	// the sync pushes the current status when the listener attaches, so skip
	// any none until the hold arrives
	var status *ClientLimitStatus
	for status == nil || status.Status == ClientLimitStatusNone {
		status = listener.next(t, "the client limit close reached no remote listener")
	}
	connect.AssertEqual(t, status, deviceLocal.GetClientLimitStatus())
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), status)

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), status)
}
