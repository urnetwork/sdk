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

	gojwt "github.com/golang-jwt/jwt/v5"
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
	// the api and pump names resolve in the process; the platform is local
	strategySettings := testOfflineClientStrategySettings()
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

// The app form of the hold: none without a hold, whatever the plan; exceeded
// with the retry time in unix milliseconds while one is in force, and the
// prompt of the plan: the upgrade for a free network, a freed slot for a Pro
// network, whose plan is already the highest.
func TestNewClientLimitStatusMapsTheHold(t *testing.T) {
	connect.AssertEqual(t, newClientLimitStatus(connect.ClientLimitStatus{}, false), noneClientLimitStatus())
	connect.AssertEqual(t, newClientLimitStatus(connect.ClientLimitStatus{}, true), noneClientLimitStatus())
	retryTime := time.Date(2026, 10, 6, 12, 15, 0, 0, time.UTC)
	connect.AssertEqual(
		t,
		newClientLimitStatus(connect.ClientLimitStatus{Exceeded: true, RetryTime: retryTime}, false),
		&ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: retryTime.UnixMilli(), Prompt: ClientLimitPromptUpgrade},
	)
	connect.AssertEqual(
		t,
		newClientLimitStatus(connect.ClientLimitStatus{Exceeded: true, RetryTime: retryTime}, true),
		&ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: retryTime.UnixMilli(), Prompt: ClientLimitPromptRemoveClient},
	)
	connect.AssertEqual(t, noneClientLimitStatus().Status, ClientLimitStatusNone)
	connect.AssertEqual(t, noneClientLimitStatus().RetryTime, int64(0))
	connect.AssertEqual(t, noneClientLimitStatus().Prompt, ClientLimitPromptNone)
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
	// the api and pump names resolve in the process; the platform is local
	strategySettings := testOfflineClientStrategySettings()
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

// The prompt follows the plan of the device's client. A plan change with no
// hold in force emits nothing; a hold prompts for the plan the client has, and
// a plan change during the hold, as a refreshed client jwt brings, emits the
// same hold again with the new prompt.
func TestDeviceLocalClientLimitPromptFollowsThePlan(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	_, device := newTestingClientLimitDevice(t, platform, nil)
	listener := newTestingClientLimitListener()
	sub := device.AddClientLimitStatusChangeListener(listener)
	defer sub.Close()

	// a Pro client with no hold: nothing to emit, so the first status the
	// listener receives is the hold below
	device.clientLimitPro.Set(true)
	connect.AssertEqual(t, device.GetClientLimitStatus(), noneClientLimitStatus())

	undeclared := platform.nextConnectionDeclaring(t, "")
	waitProviderSettled(t, device)
	undeclared.closeForClientLimit()
	held := listener.next(t, "the client limit close reached no listener")
	if held.Status != ClientLimitStatusExceeded || held.Prompt != ClientLimitPromptRemoveClient {
		t.Fatalf("the Pro client's hold = %+v, want exceeded prompting to free a slot", held)
	}
	connect.AssertEqual(t, device.GetClientLimitStatus(), held)

	// the plan is free now: the same hold offers the upgrade
	device.clientLimitPro.Set(false)
	free := listener.next(t, "the plan change reached no listener")
	connect.AssertEqual(t, free, &ClientLimitStatus{
		Status:    ClientLimitStatusExceeded,
		RetryTime: held.RetryTime,
		Prompt:    ClientLimitPromptUpgrade,
	})
	connect.AssertEqual(t, device.GetClientLimitStatus(), free)
}

// The plan the prompt follows is the client jwt's pro claim; a jwt without
// one, or one that does not parse, is free.
func TestClientLimitProClaim(t *testing.T) {
	signed := func(claims gojwt.MapClaims) string {
		token, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims).SignedString([]byte("synthetic-client-limit-key"))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	connect.AssertEqual(t, clientLimitProClaim(signed(gojwt.MapClaims{"network_id": "synthetic", "pro": true})), true)
	connect.AssertEqual(t, clientLimitProClaim(signed(gojwt.MapClaims{"network_id": "synthetic", "pro": false})), false)
	connect.AssertEqual(t, clientLimitProClaim(signed(gojwt.MapClaims{"network_id": "synthetic"})), false)
	connect.AssertEqual(t, clientLimitProClaim(""), false)
	connect.AssertEqual(t, clientLimitProClaim("not.a.jwt"), false)
}

// RetryClientLimit ends a hold at once and the device dials again, never with
// no hold in force, and at most once per ClientLimitRetryMinInterval. Before,
// a user who upgraded or freed a slot waited out the rest of the 15 to 20
// minute hold, with no way for the app to reconnect sooner.
func TestDeviceLocalRetryClientLimitEndsTheHoldOncePerInterval(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	_, device := newTestingClientLimitDevice(t, platform, func(settings *DeviceLocalSettings) {
		// longer than the test, so the second retry is inside it
		settings.ClientLimitRetryMinInterval = time.Hour
	})
	listener := newTestingClientLimitListener()
	sub := device.AddClientLimitStatusChangeListener(listener)
	defer sub.Close()

	if device.RetryClientLimit() {
		t.Fatal("a retry with no hold in force ended one")
	}

	first := platform.nextConnectionDeclaring(t, "")
	waitProviderSettled(t, device)
	first.closeForClientLimit()
	if status := listener.next(t, "the client limit close reached no listener"); status.Status != ClientLimitStatusExceeded {
		t.Fatalf("listener status = %+v, want %s", status, ClientLimitStatusExceeded)
	}

	// held: what the platform accepted so far predates the retry
	platform.drain()
	if !device.RetryClientLimit() {
		t.Fatal("the retry did not end the hold")
	}
	if status := listener.next(t, "the retry reached no listener"); status.Status != ClientLimitStatusNone {
		t.Fatalf("listener status after the retry = %+v, want none", status)
	}
	// the device dials again at once, not at the hold's retry time
	second := platform.nextConnectionDeclaring(t, "")
	waitProviderSettled(t, device)

	// the platform refuses again: a new hold, and a retry inside the interval
	// ends nothing
	second.closeForClientLimit()
	if status := listener.next(t, "the second close reached no listener"); status.Status != ClientLimitStatusExceeded {
		t.Fatalf("listener status = %+v, want %s", status, ClientLimitStatusExceeded)
	}
	if device.RetryClientLimit() {
		t.Fatal("a second retry inside the interval ended the hold")
	}
	connect.AssertEqual(t, device.GetClientLimitStatus().Status, ClientLimitStatusExceeded)
}

// A remote device's retry crosses the device rpc to the device process, and
// the prompt crosses with the status. With no device process a retry ends
// nothing.
func TestDeviceRemoteRetryClientLimit(t *testing.T) {
	platform := newTestingClientLimitPlatform(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// the api and pump names resolve in the process; the platform is local
	strategySettings := testOfflineClientStrategySettings()
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

	undeclared := platform.nextConnectionDeclaring(t, "")
	waitProviderSettled(t, deviceLocal)
	undeclared.closeForClientLimit()
	// the sync pushes the current status when the listener attaches, so skip
	// any none until the hold arrives
	var status *ClientLimitStatus
	for status == nil || status.Status == ClientLimitStatusNone {
		status = listener.next(t, "the client limit close reached no remote listener")
	}
	connect.AssertEqual(t, status.Prompt, ClientLimitPromptUpgrade)
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), deviceLocal.GetClientLimitStatus())

	platform.drain()
	if !deviceRemote.RetryClientLimit() {
		t.Fatal("the remote's retry did not end the hold")
	}
	for status.Status != ClientLimitStatusNone {
		status = listener.next(t, "the retry reached no remote listener")
	}
	connect.AssertEqual(t, deviceLocal.GetClientLimitStatus(), noneClientLimitStatus())
	platform.nextConnectionDeclaring(t, "")
	if deviceRemote.RetryClientLimit() {
		t.Fatal("a remote retry with no hold in force ended one")
	}

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	if deviceRemote.RetryClientLimit() {
		t.Fatal("a remote retry with no device process ended a hold")
	}
}

// The notice of a hold, the same on every platform: it shows only while a
// connection is requested, alerts once per request, shows again quietly when
// the platform refuses the device again at its retry time, replaces itself in
// place when the prompt changes, and goes when the hold ends, when the request
// ends, and at a sign out.
func TestClientLimitNoticeTrackerAlertsOncePerConnectionRequest(t *testing.T) {
	upgrade := &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1, Prompt: ClientLimitPromptUpgrade}
	removeClient := &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1, Prompt: ClientLimitPromptRemoveClient}
	none := noneClientLimitStatus()

	tracker := NewClientLimitNoticeTracker()
	steps := []struct {
		status           *ClientLimitStatus
		connectRequested bool
		action           string
	}{
		// held with no connection requested: no notice
		{status: upgrade, connectRequested: false, action: ClientLimitNoticeNone},
		// the user connects: the notice alerts once
		{status: upgrade, connectRequested: true, action: ClientLimitNoticePost},
		{status: upgrade, connectRequested: true, action: ClientLimitNoticeNone},
		// the refreshed plan is Pro: replaced in place
		{status: removeClient, connectRequested: true, action: ClientLimitNoticeUpdate},
		// the hold ends and the device dials again
		{status: none, connectRequested: true, action: ClientLimitNoticeRemove},
		{status: none, connectRequested: true, action: ClientLimitNoticeNone},
		// refused again in the same request: shown without alerting
		{status: removeClient, connectRequested: true, action: ClientLimitNoticeUpdate},
		// the user disconnects
		{status: removeClient, connectRequested: false, action: ClientLimitNoticeRemove},
		// a new request alerts again
		{status: upgrade, connectRequested: true, action: ClientLimitNoticePost},
		// a status the app could not read is no hold
		{status: nil, connectRequested: true, action: ClientLimitNoticeRemove},
		// an older device process sends no prompt: still a hold to tell
		{status: &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1}, connectRequested: true, action: ClientLimitNoticeUpdate},
	}
	for i, step := range steps {
		if action := tracker.Update(step.status, step.connectRequested); action != step.action {
			t.Fatalf("step %d (%+v, requested %t): action = %q, want %q", i, step.status, step.connectRequested, action, step.action)
		}
	}

	// a sign out withdraws the notice, and the next account's request alerts
	connect.AssertEqual(t, tracker.Reset(), ClientLimitNoticeRemove)
	connect.AssertEqual(t, tracker.Reset(), ClientLimitNoticeNone)
	connect.AssertEqual(t, tracker.Update(upgrade, true), ClientLimitNoticePost)
}
