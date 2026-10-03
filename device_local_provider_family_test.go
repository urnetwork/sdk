package sdk

// The provider's family-pinned transport group (connect/IPV6.md A1, A4):
// construction with and without family urls, make-before-break across
// transport kinds, migration through the group, and the status readout on
// the local device, the remote device and its degraded paths.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// newTestProviderClient builds a connect client whose route manager a
// transport group can register on. The oob endpoint answers nothing useful;
// the transports under test dial unreachable urls and never connect.
func newTestProviderClient(t *testing.T) (*connect.Client, *connect.ClientStrategy) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	endpoint := httptest.NewServer(http.NotFoundHandler())
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	control := connect.NewApiOutOfBandControl(ctx, strategy, "synthetic-provider-token", endpoint.URL)
	settings := connect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	client := connect.NewClient(ctx, connect.NewId(), control, settings)
	t.Cleanup(func() {
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		_ = client.CloseAndWait(joinCtx)
		_ = control.CloseAndWait(joinCtx)
		strategy.Close()
		endpoint.Close()
	})
	return client, strategy
}

func newTestFamilyProvider(t *testing.T, platformUrlV4 string, platformUrlV6 string) *deviceLocalProvider {
	t.Helper()
	client, strategy := newTestProviderClient(t)
	provider := &deviceLocalProvider{
		ctx:                       client.Ctx(),
		client:                    client,
		clientStrategy:            strategy,
		clientStrategySettings:    connect.DefaultClientStrategySettings(),
		platformUrl:               "wss://127.0.0.1:1",
		platformUrlV4:             platformUrlV4,
		platformUrlV6:             platformUrlV6,
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		targetMode:                connect.TransportModeH1,
		modePreferences:           connect.DefaultTransportModePreferences(),
		transportPolicyVersion:    1,
		migrateConnectTimeout:     platformTransportMigrateConnectTimeout,
		migrateMaxScheduleDelay:   platformTransportMigrateMaxScheduleDelay,
		auth: &connect.ClientAuth{
			ByJwt:      "test",
			InstanceId: connect.NewId(),
			AppVersion: "0.0.0",
		},
	}
	return provider
}

func waitTransportDone(t *testing.T, transport migratablePlatformTransport) {
	t.Helper()
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()
	if joining, ok := transport.(interface {
		CloseAndWait(context.Context) error
	}); ok {
		if err := joining.CloseAndWait(joinCtx); err != nil {
			t.Fatal(err)
		}
		return
	}
	transport.Close()
}

// With family urls the provider runs the v4-pinned, v6-pinned and standby
// transports as one group, and the standby is held while the pins dial.
func TestDeviceLocalProviderTransportGroupWithFamilyUrls(t *testing.T) {
	provider := newTestFamilyProvider(t, "wss://connect-v4.test.invalid", "wss://connect-v6.test.invalid")
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)
	group, ok := transport.(*connect.FamilyPlatformTransportGroup)
	if !ok {
		t.Fatalf("provider transport is %T, want a family transport group", transport)
	}
	if group.Ipv4Transport() == nil || group.Ipv6Transport() == nil || group.StandbyTransport() == nil {
		t.Fatal("group is missing a pinned or standby transport")
	}
	if n := len(group.Transports()); n != 3 {
		t.Fatalf("group transports = %d, want 3", n)
	}
	if group.Ipv4Transport().IpFamily() != 4 || group.Ipv6Transport().IpFamily() != 6 || group.StandbyTransport().IpFamily() != 0 {
		t.Fatal("group transports do not carry their families")
	}

	provider.stateLock.Lock()
	provider.platformTransport = transport
	provider.stateLock.Unlock()
	status := provider.familyTransportStatus()
	if !status.HasIpv4 || !status.HasIpv6 {
		t.Fatalf("status = %+v, want both pins present", *status)
	}
	for _, state := range []string{status.Ipv4State, status.Ipv6State, status.StandbyState} {
		if state == "" || state == ProviderFamilyTransportStateUnknown {
			t.Fatalf("status = %+v, want every state known", *status)
		}
	}
	if status.StandbyActive {
		t.Fatalf("status = %+v, want the standby held while the pins dial", *status)
	}
	if status.StandbyState != connect.PlatformTransportStateDisabled.String() {
		t.Fatalf("standby state = %q, want %q", status.StandbyState, connect.PlatformTransportStateDisabled)
	}
}

// Without family urls (an ip literal, a custom space) the group is the
// legacy single transport: no pins, the standby dials at once.
func TestDeviceLocalProviderTransportGroupWithoutFamilyUrls(t *testing.T) {
	provider := newTestFamilyProvider(t, "", "")
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)
	group, ok := transport.(*connect.FamilyPlatformTransportGroup)
	if !ok {
		t.Fatalf("provider transport is %T, want a family transport group", transport)
	}
	if group.Ipv4Transport() != nil || group.Ipv6Transport() != nil {
		t.Fatal("legacy group built a pinned transport")
	}
	if n := len(group.Transports()); n != 1 {
		t.Fatalf("group transports = %d, want 1", n)
	}
	provider.stateLock.Lock()
	provider.platformTransport = transport
	provider.stateLock.Unlock()
	status := provider.familyTransportStatus()
	if status.HasIpv4 || status.HasIpv6 {
		t.Fatalf("status = %+v, want no pins", *status)
	}
	if status.Ipv4State != ProviderFamilyTransportStateUnknown || status.Ipv6State != ProviderFamilyTransportStateUnknown {
		t.Fatalf("status = %+v, want unknown pin states", *status)
	}
	if !status.StandbyActive || status.StandbyState != connect.PlatformTransportStateConnecting.String() {
		t.Fatalf("status = %+v, want the standby dialing", *status)
	}
}

// A nil settings source falls back to the connect defaults rather than
// dereferencing nil when building the pinned strategies.
func TestDeviceLocalProviderTransportGroupNilStrategySettings(t *testing.T) {
	provider := newTestFamilyProvider(t, "wss://connect-v4.test.invalid", "wss://connect-v6.test.invalid")
	provider.clientStrategySettings = nil
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)
	if group, ok := transport.(*connect.FamilyPlatformTransportGroup); !ok || group.Ipv4Transport() == nil {
		t.Fatalf("provider transport = %T without pins", transport)
	}
}

// Make-before-break is decided per transport kind: group against group,
// group against a single transport through the standby, and any unknown
// (test) transport never forces break-before-make.
func TestCanMakeBeforeBreakAcrossTransportKinds(t *testing.T) {
	client, strategy := newTestProviderClient(t)
	auth := &connect.ClientAuth{ByJwt: "test", InstanceId: connect.NewId(), AppVersion: "0.0.0"}
	settings := connect.DefaultPlatformTransportSettings()
	newGroup := func() *connect.FamilyPlatformTransportGroup {
		return connect.NewFamilyPlatformTransportGroup(
			client.Ctx(),
			connect.DefaultClientStrategySettings(),
			strategy,
			client.RouteManager(),
			"wss://127.0.0.1:1",
			"wss://connect-v4.test.invalid",
			"wss://connect-v6.test.invalid",
			auth,
			connect.TransportModeH1,
			settings,
			nil,
		)
	}
	newSingle := func() *connect.PlatformTransport {
		return connect.NewPlatformTransportWithTargetMode(
			client.Ctx(),
			strategy,
			client.RouteManager(),
			"wss://127.0.0.1:1",
			auth,
			connect.TransportModeH1,
			settings,
		)
	}
	groupA, groupB := newGroup(), newGroup()
	singleA, singleB := newSingle(), newSingle()
	defer waitTransportDone(t, groupA)
	defer waitTransportDone(t, groupB)
	defer waitTransportDone(t, singleA)
	defer waitTransportDone(t, singleB)
	fake := newFakeMigratablePlatformTransport(auth, true)

	cases := []struct {
		name     string
		next     migratablePlatformTransport
		previous migratablePlatformTransport
	}{
		{"group-group", groupB, groupA},
		{"group-single", groupA, singleA},
		{"single-group", singleA, groupA},
		{"single-single", singleB, singleA},
		{"fake-group", fake, groupA},
		{"group-fake", groupA, fake},
		{"fake-fake", fake, fake},
	}
	for _, c := range cases {
		// H1 transitions always keep the old route (connect's bounded handoff)
		if !canMakeBeforeBreak(c.next, c.previous) {
			t.Fatalf("%s: h1 transition must make before break", c.name)
		}
	}
}

// A resident migration builds a full replacement group; when it does not
// connect in time the old carrier stays current and the group is joined.
func TestDeviceLocalProviderMigrationBuildsGroupAndKeepsOldOnTimeout(t *testing.T) {
	provider := newTestFamilyProvider(t, "wss://connect-v4.test.invalid", "wss://connect-v6.test.invalid")
	oldTransport := newFakeMigratablePlatformTransport(provider.auth, true)
	provider.platformTransport = oldTransport
	provider.migrateConnectTimeout = 500 * time.Millisecond
	built := make(chan *connect.FamilyPlatformTransportGroup, 1)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		next := provider.newProviderPlatformTransport(auth, targetMode, settings)
		built <- next.(*connect.FamilyPlatformTransportGroup)
		return next
	}
	provider.requestPlatformTransportMigration(time.Now())
	var group *connect.FamilyPlatformTransportGroup
	select {
	case group = <-built:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not build a replacement group")
	}
	if group.Ipv4Transport() == nil || group.Ipv6Transport() == nil {
		t.Fatal("replacement group is missing its pinned transports")
	}
	for deadline := time.Now().Add(10 * time.Second); provider.migrating.Load() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if provider.migrating.Load() {
		t.Fatal("migration did not finish")
	}
	provider.stateLock.Lock()
	current := provider.platformTransport
	provider.stateLock.Unlock()
	if current != oldTransport {
		t.Fatal("an unconnected replacement group displaced the live carrier")
	}
	oldTransport.mutex.Lock()
	oldClosed := oldTransport.closed
	oldTransport.mutex.Unlock()
	if oldClosed {
		t.Fatal("the live carrier was closed while its replacement never connected")
	}
	select {
	case <-group.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned replacement group was not joined")
	}
}

// SetByJwt reaches every transport of the group (the auth generation is
// installed on the pins and the standby alike), and Close joins the group.
func TestDeviceLocalProviderSetByJwtAndCloseReachGroup(t *testing.T) {
	provider := newTestFamilyProvider(t, "wss://connect-v4.test.invalid", "wss://connect-v6.test.invalid")
	provider.platformTransport = provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	provider.SetByJwt("rotated")
	provider.stateLock.Lock()
	authVersion := provider.authVersion
	byJwt := provider.auth.ByJwt
	provider.stateLock.Unlock()
	if authVersion != 1 || byJwt != "rotated" {
		t.Fatalf("auth version %d jwt %q after SetByJwt", authVersion, byJwt)
	}
	group := provider.platformTransport.(*connect.FamilyPlatformTransportGroup)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	if err := provider.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
	// Done mints a fresh channel that closes once the joined group is
	// observed, so give it a moment rather than reading it synchronously
	select {
	case <-group.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("provider close did not join its transport group")
	}
	if status := provider.familyTransportStatus(); status.StandbyState != ProviderFamilyTransportStateUnknown {
		t.Fatalf("closed provider status = %+v, want unknown", *status)
	}
}

// The local device reads the provider's readout, and reports unknown with no
// provider or after close; a legacy single transport reports through its
// connected bit.
func TestDeviceLocalGetProviderFamilyTransportStatus(t *testing.T) {
	device := &DeviceLocal{}
	if status := device.GetProviderFamilyTransportStatus(); status.StandbyState != ProviderFamilyTransportStateUnknown ||
		status.HasIpv4 || status.HasIpv6 || status.StandbyActive {
		t.Fatalf("no-provider status = %+v, want unknown", *status)
	}

	auth := &connect.ClientAuth{ByJwt: "test", InstanceId: connect.NewId(), AppVersion: "0.0.0"}
	transport := newFakeMigratablePlatformTransport(auth, false)
	provider := &deviceLocalProvider{platformTransport: transport}
	device = &DeviceLocal{provider: provider}
	status := device.GetProviderFamilyTransportStatus()
	if status.HasIpv4 || status.HasIpv6 || !status.StandbyActive ||
		status.StandbyState != connect.PlatformTransportStateConnecting.String() {
		t.Fatalf("legacy disconnected status = %+v", *status)
	}
	transport.connect()
	if status := device.GetProviderFamilyTransportStatus(); status.StandbyState != connect.PlatformTransportStateConnected.String() {
		t.Fatalf("legacy connected status = %+v", *status)
	}

	device.stateLock.Lock()
	device.closed = true
	device.stateLock.Unlock()
	if status := device.GetProviderFamilyTransportStatus(); status.StandbyState != ProviderFamilyTransportStateUnknown {
		t.Fatalf("closed device status = %+v, want unknown", *status)
	}
}

// The readout crosses the device rpc: the remote reads through to the local
// provider (the test space derives family urls, so both pins exist), and a
// remote with no service degrades to the last known readout, else unknown.
func TestDeviceRemoteProviderFamilyTransportStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !networkSpace.HasPlatformFamilyUrls() {
		t.Fatalf("test network space derives no family urls from %q", networkSpace.platformUrl)
	}

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
		clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(10 * time.Second) {
		t.Fatal("device remote did not complete its initial sync")
	}

	localStatus := deviceLocal.GetProviderFamilyTransportStatus()
	if !localStatus.HasIpv4 || !localStatus.HasIpv6 {
		t.Fatalf("local status = %+v, want both pins", *localStatus)
	}
	remoteStatus := deviceRemote.GetProviderFamilyTransportStatus()
	if remoteStatus.HasIpv4 != localStatus.HasIpv4 || remoteStatus.HasIpv6 != localStatus.HasIpv6 {
		t.Fatalf("remote status = %+v, local = %+v", *remoteStatus, *localStatus)
	}
	known := map[string]bool{
		connect.PlatformTransportStateConnecting.String(): true,
		connect.PlatformTransportStateConnected.String():  true,
		connect.PlatformTransportStateDisabled.String():   true,
		connect.PlatformTransportStateSleeping.String():   true,
		connect.PlatformTransportStateIdlePolicy.String(): true,
	}
	for _, state := range []string{remoteStatus.Ipv4State, remoteStatus.Ipv6State, remoteStatus.StandbyState} {
		if !known[state] {
			t.Fatalf("remote status = %+v carries an unknown state", *remoteStatus)
		}
	}
	deviceRemote.stateLock.Lock()
	retained := deviceRemote.lastProviderFamilyTransportStatus
	deviceRemote.stateLock.Unlock()
	if retained == nil {
		t.Fatal("remote did not retain the last known readout")
	}

	// degraded: no service, last known retained
	detached := &DeviceRemote{lastProviderFamilyTransportStatus: retained}
	if status := detached.GetProviderFamilyTransportStatus(); *status != *retained {
		t.Fatalf("detached status = %+v, want the retained %+v", *status, *retained)
	}
	// degraded: never observed
	if status := (&DeviceRemote{}).GetProviderFamilyTransportStatus(); status.StandbyState != ProviderFamilyTransportStateUnknown {
		t.Fatalf("never-observed status = %+v, want unknown", *status)
	}
}
