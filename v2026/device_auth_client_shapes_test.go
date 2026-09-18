package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type testingAuthClientShape struct {
	networkSpace    *NetworkSpace
	localState      *LocalState
	initialJwt      string
	adminJwt        string
	instanceId      *Id
	api             *Api
	deviceJwt       func() string
	addRefresh      func(JwtRefreshListener) Sub
	closeDevice     func()
	joinDevice      func(context.Context) error
	capturedRefresh func(string)
	localDevice     *DeviceLocal
	remoteDevice    *DeviceRemote
}

func testingAuthClientShapeSpace(t *testing.T) *testingAuthClientShape {
	t.Helper()
	networkSpace := newNetworkSpace(
		context.Background(),
		*NewNetworkSpaceKey("client-shapes.test", "test"),
		NetworkSpaceValues{
			ApiUrl:                   "http://127.0.0.1:1",
			PlatformUrl:              "ws://127.0.0.1:1",
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
		},
		t.TempDir(),
	)
	t.Cleanup(networkSpace.close)
	api := networkSpace.GetApi()
	// The real API commit and constructor-installed device listener below
	// run synchronously. The independent timer/network worker is not needed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := api.CloseAndWait(ctx); err != nil {
		t.Fatal("could not join the unused automatic API worker")
	}
	return &testingAuthClientShape{
		networkSpace: networkSpace,
		localState:   networkSpace.asyncLocalState.localState,
		initialJwt:   testingRefreshableJwtWithMarker(t, "client-shape-initial"),
		instanceId:   NewId(),
		api:          api,
	}
}

func (self *testingAuthClientShape) seedDistinctLogin(t *testing.T) {
	t.Helper()
	// The normal app login contract stores a user/network token first, then
	// the different client credential used to construct DeviceLocal/Remote.
	networkJwt := testingJwt(map[string]any{
		"user_id":      "00000000-0000-0000-0000-000000000003",
		"network_id":   "00000000-0000-0000-0000-000000000004",
		"network_name": "client-shapes-test",
	})
	if err := self.localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	self.adminJwt = networkJwt
	if err := self.localState.SetByClientJwt(self.initialJwt); err != nil {
		t.Fatal(err)
	}
	self.instanceId = self.localState.GetInstanceId()
	if self.instanceId == nil {
		t.Fatal("normal two-stage login did not establish an instance")
	}
}

func (self *testingAuthClientShape) startLocal(t *testing.T) {
	t.Helper()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		self.networkSpace, self.initialJwt, "client-shape-test", "test", "0.0.0",
		self.instanceId, settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	self.localDevice = device
	self.deviceJwt = func() string {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.byJwt
	}
	self.addRefresh = device.AddJwtRefreshListener
	self.closeDevice = device.Close
	self.joinDevice = device.CloseAndWait
	self.capturedRefresh = device.applyApiRefreshedByJwt
	self.installCleanup(t)
}

func (self *testingAuthClientShape) startRemote(t *testing.T) {
	t.Helper()
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	device, err := newDeviceRemoteWithOverrides(
		self.networkSpace, self.initialJwt, self.instanceId, settings,
		connect.NewId(), alwaysOfflineDeviceRpcDialer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	self.remoteDevice = device
	self.deviceJwt = func() string {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.byJwt
	}
	self.addRefresh = device.AddJwtRefreshListener
	self.closeDevice = device.Close
	self.joinDevice = device.CloseAndWait
	self.capturedRefresh = device.setByJwt
	self.installCleanup(t)
}

func (self *testingAuthClientShape) installCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := self.joinDevice(ctx); err != nil {
			t.Error("device auth-shape fixture did not join")
		}
	})
}

func (self *testingAuthClientShape) requireCurrentRefresh(t *testing.T) {
	t.Helper()
	refreshedJwt := testingRefreshableJwtWithMarker(t, "client-shape-refreshed")
	observed := []string{}
	sub := self.addRefresh(jwtRefreshListenerFunc(func(jwt string) { observed = append(observed, jwt) }))
	t.Cleanup(sub.Close)
	// Attach the actual window-client auth consumer before the API rotation,
	// without creating a provider transport or public network connection.
	var generator *connect.ApiMultiClientGenerator
	var windowAuth <-chan string
	if self.localDevice != nil {
		authorizations := make(chan string, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/network/auth-client" {
				http.Error(w, "unexpected path", http.StatusNotFound)
				return
			}
			authorizations <- r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"by_client_jwt":%q}`, refreshedJwt)
		}))
		t.Cleanup(server.Close)
		generator = connect.NewApiMultiClientGenerator(
			context.Background(), nil, self.networkSpace.clientStrategy, nil,
			server.URL, self.initialJwt, "ws://127.0.0.1:1", "client-shape-test",
			"test", "0.0.0", nil, connect.DefaultClientSettings,
			connect.DefaultApiMultiClientGeneratorSettings(),
		)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := generator.CloseAndWait(ctx); err != nil {
				t.Error("window auth consumer did not join")
			}
		})
		self.localDevice.stateLock.Lock()
		self.localDevice.apiMultiClientGenerator = generator
		self.localDevice.stateLock.Unlock()
		windowAuth = authorizations
	}
	if !self.api.setRefreshedByJwt(self.initialJwt, refreshedJwt) {
		t.Fatal("current API refresh was not accepted")
	}
	if self.api.GetByJwt() != refreshedJwt {
		t.Error("API did not retain its committed refresh")
	}
	if self.deviceJwt() != refreshedJwt {
		t.Error("accepted API refresh did not reach the actual device")
	}
	if len(observed) != 1 || observed[0] != refreshedJwt {
		t.Error("accepted API refresh did not reach the device observer exactly once")
	}
	state, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != self.adminJwt {
		t.Error("client refresh overwrote or synthesized the separate admin credential")
	}
	if state.ByClientJwt != refreshedJwt || state.InstanceId != self.instanceId.String() {
		t.Error("accepted API refresh did not persist the restartable credential and stable instance")
	}
	if generator != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := generator.NewClientArgsContext(ctx); err != nil {
			t.Fatal("window client auth request did not complete")
		}
		select {
		case got := <-windowAuth:
			if got != "Bearer "+refreshedJwt {
				t.Error("later window client mint used the stale constructor credential")
			}
		case <-ctx.Done():
			t.Fatal("window client auth request was not observed")
		}
	}
}

func TestDeviceLocalRefreshInitializesEmptyDaemonAuthStore(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.startLocal(t)
	fixture.requireCurrentRefresh(t)
}

func TestDeviceLocalRefreshAcceptsDistinctNetworkAndClientJwt(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	fixture.requireCurrentRefresh(t)
}

func TestDeviceRemoteRefreshAcceptsDistinctNetworkAndClientJwt(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	fixture.requireCurrentRefresh(t)
}

func TestDeviceLocalRefreshAcceptsExplicitAtomicAuthSeed(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	if err := fixture.localState.SetByClientJwtForInstance(fixture.initialJwt, fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.startLocal(t)
	fixture.requireCurrentRefresh(t)
}

func (self *testingAuthClientShape) requireLateRefreshRejected(t *testing.T) {
	t.Helper()
	before, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	beforeDevice := self.deviceJwt()
	observations := 0
	sub := self.addRefresh(jwtRefreshListenerFunc(func(string) { observations += 1 }))
	t.Cleanup(sub.Close)
	lateJwt := testingRefreshableJwtWithMarker(t, "superseded-client-shape-refresh")
	// The API owns the in-flight request, but a logged-out, replaced or
	// retired device must not republish it to persistence or device observers.
	self.api.setRefreshedByJwt(self.initialJwt, lateJwt)
	after, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("late refresh replaced protected auth storage")
	}
	if self.deviceJwt() != beforeDevice || observations != 0 {
		t.Error("late refresh published into a protected device generation")
	}
}

func TestDeviceLocalDistinctLoginRefreshCannotUndoLogout(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	if err := fixture.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	fixture.requireLateRefreshRejected(t)
}

func TestDeviceRemoteDistinctLoginRefreshCannotUndoLogout(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	if err := fixture.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	fixture.requireLateRefreshRejected(t)
}

func TestDeviceLocalDistinctLoginRefreshCannotReplaceNewerSession(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	if err := fixture.localState.SetByClientJwtForInstance(testingRefreshableJwtWithMarker(t, "newer-session"), fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.requireLateRefreshRejected(t)
}

func TestDeviceRemoteDistinctLoginRefreshCannotReplaceNewerSession(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	if err := fixture.localState.SetByClientJwtForInstance(testingRefreshableJwtWithMarker(t, "newer-session"), fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.requireLateRefreshRejected(t)
}

func TestDeviceLocalDistinctLoginRefreshCannotPublishAfterRetirement(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	fixture.requireCapturedRefreshRejectedAfterRetirement(t)
}

func TestDeviceRemoteDistinctLoginRefreshCannotPublishAfterRetirement(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	fixture.requireCapturedRefreshRejectedAfterRetirement(t)
}

func (self *testingAuthClientShape) requireCapturedRefreshRejectedAfterRetirement(t *testing.T) {
	t.Helper()
	before, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	beforeDevice := self.deviceJwt()
	observations := 0
	sub := self.addRefresh(jwtRefreshListenerFunc(func(string) { observations += 1 }))
	t.Cleanup(sub.Close)
	lateJwt := testingRefreshableJwtWithMarker(t, "captured-before-retirement")
	// Model a captured callback delivered after the old owner closes and a new
	// API owner installs its credential. The retired device must reject it even
	// though the API currently reports the event's credential.
	self.closeDevice()
	self.api.SetByJwt(lateJwt)
	if self.api.GetByJwt() != lateJwt {
		t.Fatal("retirement control lost the committed API credential")
	}
	self.capturedRefresh(lateJwt)
	after, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("captured refresh replaced auth after retirement")
	}
	if self.deviceJwt() != beforeDevice || observations != 0 {
		t.Error("captured refresh published after device retirement")
	}
}

func TestExistingInstanceClientSeedDoesNotCreateAdminCredential(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	if err := fixture.localState.SetByClientJwtForInstance(fixture.initialJwt, fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != "" {
		t.Error("client-only daemon seed synthesized an admin credential")
	}
	if state.ByClientJwt != fixture.initialJwt || state.InstanceId != fixture.instanceId.String() {
		t.Error("client-only seed lost the client credential or stable instance")
	}
}

func TestExistingInstanceClientSeedPreservesSeparateAdminCredential(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	rotatedJwt := testingRefreshableJwtWithMarker(t, "seeded-client-rotation")
	if err := fixture.localState.SetByClientJwtForInstance(rotatedJwt, fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != fixture.adminJwt {
		t.Error("client seed overwrote the separately stored admin credential")
	}
	if state.ByClientJwt != rotatedJwt || state.InstanceId != fixture.instanceId.String() {
		t.Error("client seed lost the client credential or stable instance")
	}
}
