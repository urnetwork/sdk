//go:build !ios_extension

// A connect, disconnect or provide-mode write already inside the sdk when its
// sign-in ends must not save into the next sign-in. These tests hold a real
// DeviceRemote's rpc at a barrier, sign out through the apps' paths or the
// server's, release it, and sign in again on the same space: the next sign-in
// must start disconnected, with nothing of the signed-out account saved, and
// its own writes must stay.
package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A fake live device over the device rpc. It answers the provide control mode,
// takes the connect controller's writes, and while armed holds the next call
// of one method at a barrier: the rpc a gesture is blocked in.
type testingSignInFenceDevice struct {
	provideControlMode ProvideControlMode
	entered            chan struct{}
	release            chan struct{}
	releaseOnce        sync.Once

	stateLock  sync.Mutex
	holdMethod string
	calls      []string
}

func testingNewSignInFenceDevice(provideControlMode ProvideControlMode) *testingSignInFenceDevice {
	return &testingSignInFenceDevice{
		provideControlMode: provideControlMode,
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
}

// Holds the next call of method until releaseHeld.
func (self *testingSignInFenceDevice) hold(method string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holdMethod = method
}

func (self *testingSignInFenceDevice) releaseHeld() {
	self.releaseOnce.Do(func() { close(self.release) })
}

func (self *testingSignInFenceDevice) call(method string) {
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.calls = append(self.calls, method)
		if method != self.holdMethod {
			return false
		}
		self.holdMethod = ""
		return true
	}()
	if held {
		close(self.entered)
		<-self.release
	}
}

func (self *testingSignInFenceDevice) getCalls() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.calls...)
}

func (self *testingSignInFenceDevice) GetProvideControlMode(_ RpcNoArg, provideControlMode *ProvideControlMode) error {
	self.call("GetProvideControlMode")
	*provideControlMode = self.provideControlMode
	return nil
}

func (self *testingSignInFenceDevice) SetProvideMode(provideMode ProvideMode, _ RpcVoid) error {
	self.call("SetProvideMode")
	return nil
}

func (self *testingSignInFenceDevice) Reconnect(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.call("Reconnect")
	return nil
}

func (self *testingSignInFenceDevice) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.call("SetConnectLocation")
	return nil
}

func (self *testingSignInFenceDevice) SetDefaultLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.call("SetDefaultLocation")
	return nil
}

// One sign-in's DeviceRemote over its fake live device, and the device's
// connect controller, as the apps hold them.
type testingSignInFenceSession struct {
	deviceRemote *DeviceRemote
	device       *testingSignInFenceDevice
	controller   *ConnectViewController
}

// A space signed in in the apps' order: the network token, then the client.
func testingNewSignInFenceSpace(t *testing.T) *testingAuthClientShape {
	t.Helper()
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	return space
}

// The current sign-in's device and controller. The remote's own lifecycle
// stays parked in its blocked dial; the fake device is its live service.
func testingStartSignInFenceSession(t *testing.T, space *testingAuthClientShape, provideControlMode ProvideControlMode) *testingSignInFenceSession {
	t.Helper()
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	dialer := &testingBlockedDeviceRpcDialer{
		entered: make(chan struct{}, 1024),
		release: make(chan struct{}),
	}
	deviceRemote, err := newDeviceRemoteWithOverrides(
		space.networkSpace,
		space.localState.GetByClientJwt(),
		space.localState.GetInstanceId(),
		settings,
		connect.NewId(),
		dialer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	controller := newConnectViewController(context.Background(), deviceRemote)
	controller.testingWindowMonitor = newTestingGridWindowMonitor()
	t.Cleanup(controller.Close)
	device := testingNewSignInFenceDevice(provideControlMode)
	testingInstallFakeDeviceService(t, deviceRemote, device)
	// before the fake server joins, so a held call cannot pin it
	t.Cleanup(device.releaseHeld)
	return &testingSignInFenceSession{
		deviceRemote: deviceRemote,
		device:       device,
		controller:   controller,
	}
}

// Runs a gesture on its own goroutine, as the apps' command queues do, and
// returns once the device holds it in the rpc it was armed for.
func testingHeldSignInFenceGesture(t *testing.T, session *testingSignInFenceSession, method string, gesture func()) chan struct{} {
	t.Helper()
	session.device.hold(method)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gesture()
	}()
	select {
	case <-session.device.entered:
	case <-done:
		t.Fatalf("the gesture returned without reaching %s", method)
	case <-time.After(15 * time.Second):
		t.Fatalf("the gesture never reached %s", method)
	}
	return done
}

// Releases the held rpc and joins the gesture.
func testingReleaseSignInFenceGesture(t *testing.T, session *testingSignInFenceSession, done chan struct{}) {
	t.Helper()
	session.device.releaseHeld()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the held gesture did not return")
	}
}

type testingSignInFenceCommit chan bool

func (self testingSignInFenceCommit) Complete(success bool) {
	self <- success
}

// The apps' sign-out: the async local state's logout (apple DeviceManager,
// windows and linux SdkHost::Logout).
func testingSignInFenceLogout(t *testing.T, space *testingAuthClientShape) {
	t.Helper()
	committed := make(testingSignInFenceCommit, 1)
	space.networkSpace.GetAsyncLocalState().Logout(committed)
	select {
	case success := <-committed:
		if !success {
			t.Fatal("the logout did not commit")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the logout did not complete")
	}
}

// The next sign-in on the same space, in the apps' order.
func testingSignInFenceSignInNext(t *testing.T, space *testingAuthClientShape) {
	t.Helper()
	if err := space.localState.SetByJwt(space.adminJwt); err != nil {
		t.Fatal(err)
	}
	if err := space.localState.SetByClientJwt(testingRefreshableJwtWithMarker(t, "sign-in-fence-next")); err != nil {
		t.Fatal(err)
	}
	if space.localState.GetInstanceId() == nil {
		t.Fatal("the next sign-in established no instance")
	}
}

// What a sign-in starts from, read the way the apps read it at sign-in.
func testingSignInFenceSaved(t *testing.T, localState *LocalState) (connectLocation *ConnectLocation, defaultLocation *ConnectLocation, provideModeSaved bool) {
	t.Helper()
	connectRead, err := localState.ReadConnectLocation()
	if err != nil {
		t.Fatal(err)
	}
	defaultRead, err := localState.ReadDefaultLocation()
	if err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(localState.localStorageDir, ".provide_mode"))
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal(statErr)
	}
	return connectRead.GetLocation(), defaultRead.GetLocation(), statErr == nil
}

// The next sign-in starts disconnected and saved nothing of the signed-out one.
func testingRequireSignInFenceFresh(t *testing.T, localState *LocalState) {
	t.Helper()
	connectLocation, defaultLocation, provideModeSaved := testingSignInFenceSaved(t, localState)
	if connectLocation != nil {
		t.Errorf("the next sign-in starts connected to the signed-out account's %q", connectLocation.Name)
	}
	if defaultLocation != nil {
		t.Errorf("the next sign-in starts with the signed-out account's default %q", defaultLocation.Name)
	}
	if provideModeSaved || localState.GetProvideMode() != ProvideModeNone {
		t.Errorf("the next sign-in starts with the signed-out account's provide mode %d", localState.GetProvideMode())
	}
}

// Never mode, held before any local-state write: the connect location and the
// default location are the signed-out account's late writes.
func TestConnectViewControllerConnectHeldThroughLogoutSavesNoLocation(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeNever)

	location := testingConnectStateLocation("signed out")
	done := testingHeldSignInFenceGesture(t, session, "GetProvideControlMode", func() {
		session.controller.Connect(location)
	})
	testingSignInFenceLogout(t, space)
	testingReleaseSignInFenceGesture(t, session, done)

	testingSignInFenceSignInNext(t, space)
	testingRequireSignInFenceFresh(t, space.localState)
	// only the local-state writes are fenced: the device still got the connect
	connect.AssertEqual(t, session.device.getCalls(), []string{
		"GetProvideControlMode", "Reconnect", "SetDefaultLocation",
	})
}

// Held in the reconnect itself, after the connect location was saved (the
// logout removes it): the default location is the late write.
func TestConnectViewControllerDefaultLocationHeldThroughLogoutIsNotSaved(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeNever)

	location := testingConnectStateLocation("signed out")
	done := testingHeldSignInFenceGesture(t, session, "Reconnect", func() {
		session.controller.Connect(location)
	})
	testingSignInFenceLogout(t, space)
	testingReleaseSignInFenceGesture(t, session, done)

	testingSignInFenceSignInNext(t, space)
	testingRequireSignInFenceFresh(t, space.localState)
}

// Auto mode, held before any local-state write: the public provide mode joins
// the locations as a late write.
func TestConnectViewControllerAutoProvideModeHeldThroughLogoutIsNotSaved(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)

	location := testingConnectStateLocation("signed out")
	done := testingHeldSignInFenceGesture(t, session, "GetProvideControlMode", func() {
		session.controller.Connect(location)
	})
	testingSignInFenceLogout(t, space)
	testingReleaseSignInFenceGesture(t, session, done)

	testingSignInFenceSignInNext(t, space)
	testingRequireSignInFenceFresh(t, space.localState)
	connect.AssertEqual(t, session.device.getCalls(), []string{
		"GetProvideControlMode", "SetProvideMode", "Reconnect", "SetDefaultLocation",
	})
}

// A server-initiated sign-out has no app-side settle. The device's
// rejected-client logout, which the api's auth logout listener runs, removes
// the account while the connect is held.
func TestConnectViewControllerConnectHeldThroughServerSignOutSavesNoLocation(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	clientJwt := space.localState.GetByClientJwt()
	instanceId := space.localState.GetInstanceId()

	location := testingConnectStateLocation("signed out")
	done := testingHeldSignInFenceGesture(t, session, "GetProvideControlMode", func() {
		session.controller.Connect(location)
	})
	accepted, err := space.localState.logoutRejectedClient(clientJwt, instanceId, session.deviceRemote.authPublication)
	if err != nil || !accepted {
		t.Fatalf("the rejected client's logout did not remove the account: accepted=%t err=%v", accepted, err)
	}
	testingReleaseSignInFenceGesture(t, session, done)

	testingSignInFenceSignInNext(t, space)
	testingRequireSignInFenceFresh(t, space.localState)
}

// The signed-out account's connect returns only after the next sign-in has
// connected: its late default location must not replace the next sign-in's,
// whose own writes all stay.
func TestConnectViewControllerHeldConnectCannotOverwriteNextSignIn(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)

	signedOutLocation := testingConnectStateLocation("signed out")
	done := testingHeldSignInFenceGesture(t, session, "Reconnect", func() {
		session.controller.Connect(signedOutLocation)
	})
	testingSignInFenceLogout(t, space)

	testingSignInFenceSignInNext(t, space)
	nextSession := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	nextLocation := testingConnectStateLocation("next sign-in")
	nextSession.controller.Connect(nextLocation)

	testingReleaseSignInFenceGesture(t, session, done)

	connectLocation, defaultLocation, provideModeSaved := testingSignInFenceSaved(t, space.localState)
	if connectLocation == nil || connectLocation.Name != "next sign-in" {
		t.Errorf("the next sign-in's connect location is %v", connectLocation)
	}
	if defaultLocation == nil || defaultLocation.Name != "next sign-in" {
		t.Errorf("the next sign-in's default location is %v", defaultLocation)
	}
	if !provideModeSaved || space.localState.GetProvideMode() != ProvideModePublic {
		t.Errorf("the next sign-in's Auto provide mode is %d", space.localState.GetProvideMode())
	}
}

// The disconnect counterpart: the signed-out account's held disconnect must
// not clear the next sign-in's connect location or set its provide mode.
func TestConnectViewControllerHeldDisconnectCannotClearNextSignIn(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	session.controller.Connect(testingConnectStateLocation("signed out"))

	done := testingHeldSignInFenceGesture(t, session, "GetProvideControlMode", func() {
		session.controller.Disconnect()
	})
	testingSignInFenceLogout(t, space)

	testingSignInFenceSignInNext(t, space)
	nextSession := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	nextSession.controller.Connect(testingConnectStateLocation("next sign-in"))

	testingReleaseSignInFenceGesture(t, session, done)

	connectLocation, _, _ := testingSignInFenceSaved(t, space.localState)
	if connectLocation == nil || connectLocation.Name != "next sign-in" {
		t.Errorf("the signed-out account's disconnect cleared the next sign-in's location: %v", connectLocation)
	}
	if provideMode := space.localState.GetProvideMode(); provideMode != ProvideModePublic {
		t.Errorf("the signed-out account's disconnect set the next sign-in's provide mode to %d", provideMode)
	}
}

// Credential renewal is not a sign-out: a connect held across both renewals
// saves everything it would have.
func TestConnectViewControllerConnectHeldThroughRenewalIsSaved(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	clientJwt := space.localState.GetByClientJwt()
	instanceId := space.localState.GetInstanceId()

	location := testingConnectStateLocation("renewed")
	done := testingHeldSignInFenceGesture(t, session, "GetProvideControlMode", func() {
		session.controller.Connect(location)
	})
	renewedAdminJwt := testingJwt(map[string]any{
		"user_id":      "00000000-0000-0000-0000-000000000003",
		"network_id":   "00000000-0000-0000-0000-000000000004",
		"network_name": "client-shapes-test",
		"renewed":      true,
	})
	if _, accepted, err := space.localState.replaceNetworkByJwt(space.adminJwt, renewedAdminJwt); err != nil || !accepted {
		t.Fatalf("the network token renewal was not stored: accepted=%t err=%v", accepted, err)
	}
	renewedClientJwt := testingRefreshableJwtWithMarker(t, "sign-in-fence-renewed")
	if accepted, err := space.localState.replaceRefreshedByJwt(clientJwt, renewedClientJwt, instanceId); err != nil || !accepted {
		t.Fatalf("the client token refresh was not stored: accepted=%t err=%v", accepted, err)
	}
	testingReleaseSignInFenceGesture(t, session, done)

	connectLocation, defaultLocation, provideModeSaved := testingSignInFenceSaved(t, space.localState)
	if connectLocation == nil || connectLocation.Name != "renewed" ||
		defaultLocation == nil || defaultLocation.Name != "renewed" {
		t.Fatalf("a renewal dropped the connect's locations: connect=%v default=%v", connectLocation, defaultLocation)
	}
	if !provideModeSaved || space.localState.GetProvideMode() != ProvideModePublic {
		t.Fatalf("a renewal dropped the connect's Auto provide mode: %d", space.localState.GetProvideMode())
	}
}

// With no sign-out a connect and a disconnect save as before, and the next
// sign-in after a logout saves its own connect normally.
func TestConnectViewControllerSavesWhileSignedIn(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	session := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)

	session.controller.Connect(testingConnectStateLocation("first"))
	connectLocation, defaultLocation, _ := testingSignInFenceSaved(t, space.localState)
	if connectLocation == nil || connectLocation.Name != "first" || defaultLocation == nil || defaultLocation.Name != "first" ||
		space.localState.GetProvideMode() != ProvideModePublic {
		t.Fatal("a signed-in connect did not save its location, default and Auto provide mode")
	}
	session.controller.Disconnect()
	connectLocation, defaultLocation, _ = testingSignInFenceSaved(t, space.localState)
	if connectLocation != nil || defaultLocation == nil || defaultLocation.Name != "first" ||
		space.localState.GetProvideMode() != ProvideModeNetwork {
		t.Fatal("a signed-in disconnect did not clear the location and keep the default")
	}

	testingSignInFenceLogout(t, space)
	testingSignInFenceSignInNext(t, space)
	nextSession := testingStartSignInFenceSession(t, space, ProvideControlModeAuto)
	nextSession.controller.Connect(testingConnectStateLocation("after sign-in"))
	connectLocation, defaultLocation, _ = testingSignInFenceSaved(t, space.localState)
	if connectLocation == nil || connectLocation.Name != "after sign-in" ||
		defaultLocation == nil || defaultLocation.Name != "after sign-in" ||
		space.localState.GetProvideMode() != ProvideModePublic {
		t.Fatal("the connect right after the next sign-in was not saved")
	}
}

// The apps' own writes of the same records (a provider list's save after its
// connect, listener mirrors, a provide control mode saved after the device
// call) carry no sign-in. They are dropped from a sign-out until the next
// sign-in stores its credential, and saved again after it.
func TestLocalStateAccountSettersDroppedWhileSignedOut(t *testing.T) {
	space := testingNewSignInFenceSpace(t)
	localState := space.localState
	location := testingConnectStateLocation("late app write")
	write := func() {
		for _, err := range []error{
			localState.SetConnectLocation(location),
			localState.SetDefaultLocation(location),
			localState.SetProvideMode(ProvideModePublic),
			localState.SetProvideControlMode(ProvideControlModeAlways),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	testingSignInFenceLogout(t, space)
	write()
	testingSignInFenceSignInNext(t, space)
	testingRequireSignInFenceFresh(t, localState)
	if localState.GetProvideControlMode() != ProvideControlModeNever {
		t.Fatalf("a late write set the next sign-in's provide control mode to %s", localState.GetProvideControlMode())
	}

	write()
	connectLocation, defaultLocation, _ := testingSignInFenceSaved(t, localState)
	if connectLocation == nil || defaultLocation == nil || localState.GetProvideMode() != ProvideModePublic ||
		localState.GetProvideControlMode() != ProvideControlModeAlways {
		t.Fatal("the next sign-in's own writes were dropped")
	}
}

// Android's in-process DeviceLocal saves through its owner, which the logout
// retires, and the app mirrors the device's connect location and provide mode
// into the same records from the device's listeners. A mirror running late,
// past the logout, must not save the signed-out account's location.
func TestConnectViewControllerAutoSaveMirrorHeldThroughLogoutSavesNoLocation(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	mirror := &testingSignInFenceMirror{
		localState: fixture.localState,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	releaseMirror := sync.OnceFunc(func() { close(mirror.release) })
	t.Cleanup(releaseMirror)
	mirrorSub := device.AddConnectLocationChangeListener(mirror)
	t.Cleanup(mirrorSub.Close)
	device.SetProvideControlMode(ProvideControlModeAuto)

	location := testingSpecificPreferenceLocation()
	mirror.armed.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Connect(location)
	}()
	select {
	case <-mirror.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the connect never reached the app's mirror")
	}
	// android's logout: the synchronous local state logout
	if err := fixture.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	releaseMirror()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the held connect did not return")
	}

	fixture.seedDistinctLogin(t)
	testingRequireSignInFenceFresh(t, fixture.localState)
}

// The app's connect location mirror (android DeviceManager): saves each
// change with the plain setter, and while armed holds its first call.
type testingSignInFenceMirror struct {
	localState *LocalState
	armed      atomic.Bool
	once       sync.Once
	entered    chan struct{}
	release    chan struct{}
}

func (self *testingSignInFenceMirror) ConnectLocationChanged(location *ConnectLocation) {
	first := false
	if self.armed.Load() {
		self.once.Do(func() { first = true })
	}
	if first {
		close(self.entered)
		<-self.release
	}
	_ = self.localState.SetConnectLocation(location)
	_ = self.localState.SetProvideMode(ProvideModePublic)
}
