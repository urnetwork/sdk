// The sign-in fence of a space's local state: every sign-out closes it, only a
// stored credential opens it again, and a write captured before a sign-out, or
// while signed out, stays dropped after the next sign-in.
package sdk

import (
	"path/filepath"
	"testing"
)

// Reports whether the captured sign-in's connect location write was saved.
func testingSignInFenceWrote(t *testing.T, signIn localStateSignIn, name string) bool {
	t.Helper()
	wrote, err := signIn.setConnectLocation(testingConnectStateLocation(name))
	if err != nil {
		t.Fatal(err)
	}
	return wrote
}

func TestLocalStateSignInFenceClosesOnEverySignOut(t *testing.T) {
	for _, signOut := range []struct {
		name    string
		signOut func(t *testing.T, space *testingAuthClientShape)
	}{
		{name: "logout", signOut: func(t *testing.T, space *testingAuthClientShape) {
			if err := space.localState.Logout(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "rejected client", signOut: func(t *testing.T, space *testingAuthClientShape) {
			space.startRemote(t)
			accepted, err := space.localState.logoutRejectedClient(
				space.initialJwt, space.instanceId, space.remoteDevice.authPublication,
			)
			if err != nil || !accepted {
				t.Fatalf("the rejected client's logout did not run: accepted=%t err=%v", accepted, err)
			}
		}},
		{name: "conditional cleanup", signOut: func(t *testing.T, space *testingAuthClientShape) {
			snapshot, err := space.networkSpace.GetAuthStateSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			result, err := space.networkSpace.ResetLocalStateIfCurrent(snapshot)
			if err != nil || result == nil || !result.GetReset() {
				t.Fatalf("the conditional cleanup did not reset: err=%v", err)
			}
		}},
	} {
		space := testingAuthClientShapeSpace(t)
		space.seedDistinctLogin(t)
		before := space.localState.currentSignIn()
		if !testingSignInFenceWrote(t, before, "before") {
			t.Fatalf("%s: a signed-in write was dropped", signOut.name)
		}

		signOut.signOut(t, space)
		signedOut := space.localState.currentSignIn()
		if testingSignInFenceWrote(t, before, "late") || testingSignInFenceWrote(t, signedOut, "signed out") {
			t.Fatalf("%s: a write from before the sign-out, or from while signed out, was saved", signOut.name)
		}
		// a failed sign-in's rollback stores no credential and opens nothing
		if err := space.localState.SetByJwt(""); err != nil {
			t.Fatal(err)
		}
		if err := space.localState.SetByClientJwt(""); err != nil {
			t.Fatal(err)
		}
		if testingSignInFenceWrote(t, space.localState.currentSignIn(), "rolled back") {
			t.Fatalf("%s: an empty credential opened the fence", signOut.name)
		}

		space.seedDistinctLogin(t)
		if testingSignInFenceWrote(t, before, "late") || testingSignInFenceWrote(t, signedOut, "signed out") {
			t.Fatalf("%s: a write captured before the next sign-in was saved after it", signOut.name)
		}
		if !testingSignInFenceWrote(t, space.localState.currentSignIn(), "next") {
			t.Fatalf("%s: the next sign-in's write was dropped", signOut.name)
		}
		location, err := space.localState.LoadConnectLocation()
		if err != nil || location == nil || location.Name != "next" {
			t.Fatalf("%s: the saved location is %v, err=%v", signOut.name, location, err)
		}
	}
}

// A new LocalState is signed in to whatever its store holds: a relaunch after
// a sign-out saves as before.
func TestLocalStateSignInFenceIsInProcess(t *testing.T) {
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	if err := space.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	relaunched := newLocalState(t.Context(), filepath.Dir(space.localState.localStorageDir))
	t.Cleanup(relaunched.Close)
	if !testingSignInFenceWrote(t, relaunched.currentSignIn(), "relaunched") {
		t.Fatal("a fresh local state dropped a write")
	}
	if err := relaunched.SetDefaultLocation(testingConnectStateLocation("relaunched")); err != nil {
		t.Fatal(err)
	}
	if location, err := relaunched.LoadDefaultLocation(); err != nil || location == nil {
		t.Fatal("a fresh local state dropped a plain write")
	}
}

// The previous account's late writes, as a device listener's mirror or an app
// save after a held device call makes them, of every record the fence covers.
func testingSignInFenceLateWrites(t *testing.T, localState *LocalState) {
	t.Helper()
	location := testingConnectStateLocation("previous account")
	for _, err := range []error{
		localState.SetConnectLocation(location),
		localState.SetDefaultLocation(location),
		localState.SetProvideMode(ProvideModePublic),
		localState.SetProvideControlMode(ProvideControlModeAlways),
		localState.SetProvideNetworkMode(ProvideNetworkModeAll),
		localState.SetRouteLocal(false),
		localState.SetBlockerEnabled(true),
		localState.SetVpnInterfaceWhileOffline(true),
		localState.SetPerformanceProfile(&PerformanceProfile{
			WindowType: WindowTypeSpeed,
			WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
		}),
		localState.SetDnsResolverSettings(&DnsResolverSettings{EnableRemoteDoh: true}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Lists each covered record that holds a written value instead of its default.
func testingSignInFenceWritten(t *testing.T, localState *LocalState) []string {
	t.Helper()
	written := []string{}
	if location, err := localState.LoadConnectLocation(); err != nil || location != nil {
		written = append(written, "connect location")
	}
	if location, err := localState.LoadDefaultLocation(); err != nil || location != nil {
		written = append(written, "default location")
	}
	if localState.GetProvideMode() != ProvideModeNone {
		written = append(written, "provide mode")
	}
	if localState.GetProvideControlMode() != ProvideControlModeNever {
		written = append(written, "provide control mode")
	}
	if localState.GetProvideNetworkMode() != ProvideNetworkModeWiFi {
		written = append(written, "provide network mode")
	}
	if !localState.GetRouteLocal() {
		written = append(written, "route local")
	}
	if localState.GetBlockerEnabled() {
		written = append(written, "blocker")
	}
	if localState.GetVpnInterfaceWhileOffline() {
		written = append(written, "vpn interface while offline")
	}
	if localState.GetPerformanceProfile() != nil {
		written = append(written, "performance profile")
	}
	if localState.GetDnsResolverSettings() != nil {
		written = append(written, "dns resolver settings")
	}
	return written
}

// A sign-in that stores its network token and is then rolled back (the apps
// store it first and clear it again when a later step fails: apple
// DeviceManager rollbackFailedNetworkClientAuthentication) leaves the space
// with no credential. It is signed out again: the previous account's late
// writes are dropped, and so is a write captured while the sign-in was open.
func TestLocalStateRolledBackSignInKeepsTheFenceClosed(t *testing.T) {
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	if err := space.localState.Logout(); err != nil {
		t.Fatal(err)
	}

	// the next sign-in stores its network token...
	if err := space.localState.SetByJwt(space.adminJwt); err != nil {
		t.Fatal(err)
	}
	rolledBack := space.localState.currentSignIn()
	// ...and a later step fails, so it is rolled back
	if err := space.localState.SetByJwt(""); err != nil {
		t.Fatal(err)
	}
	if err := space.localState.SetByClientJwt(""); err != nil {
		t.Fatal(err)
	}

	testingSignInFenceLateWrites(t, space.localState)
	if testingSignInFenceWrote(t, rolledBack, "rolled back") {
		t.Error("a write of the rolled-back sign-in was saved")
	}
	space.seedDistinctLogin(t)
	if written := testingSignInFenceWritten(t, space.localState); 0 < len(written) {
		t.Errorf("late writes after a rolled-back sign-in were saved for the next sign-in: %v", written)
	}
}

// The api's rejection of a network credential with no device (an account
// screen's sign-out, or the server's) removes the last stored credential
// through the same commit, so it closes the fence too. The next sign-in saves.
func TestLocalStateRejectedNetworkCredentialClosesTheFence(t *testing.T) {
	api := newApi(t.Context(), nil, "http://127.0.0.1:1")
	defer api.Close()
	networkJwt := taggedSessionTestJwt(t, "session", "")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, "")
	api.SetByJwt(networkJwt)
	api.SetNetworkCredentialStore(localState)
	signedIn := localState.currentSignIn()
	if !api.rejectNetworkCredential(api.captureNetworkTarget(), "") {
		t.Fatal("the network credential was not rejected")
	}
	if localState.GetByJwt() != "" {
		t.Fatal("the rejected network credential is still stored")
	}

	testingSignInFenceLateWrites(t, localState)
	if testingSignInFenceWrote(t, signedIn, "rejected") {
		t.Error("a write of the rejected sign-in was saved")
	}
	if written := testingSignInFenceWritten(t, localState); 0 < len(written) {
		t.Errorf("late writes after the rejected credential were saved: %v", written)
	}

	if err := localState.SetByJwt(taggedSessionTestJwt(t, "next session", "")); err != nil {
		t.Fatal(err)
	}
	testingSignInFenceLateWrites(t, localState)
	if written := testingSignInFenceWritten(t, localState); len(written) != 10 {
		t.Errorf("the next sign-in saved only %v", written)
	}
}

// Logout wipes every per-account setting (owner rule): a late write of any of
// them, such as android's device listener mirrors make, must not carry into
// the next sign-in, and the next sign-in's own writes all save.
func TestLocalStateAccountSettingsDroppedWhileSignedOut(t *testing.T) {
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	testingSignInFenceLateWrites(t, space.localState)
	if written := testingSignInFenceWritten(t, space.localState); len(written) != 10 {
		t.Fatalf("a signed-in write was dropped: saved only %v", written)
	}

	if err := space.localState.Logout(); err != nil {
		t.Fatal(err)
	}
	testingSignInFenceLateWrites(t, space.localState)
	space.seedDistinctLogin(t)
	if written := testingSignInFenceWritten(t, space.localState); 0 < len(written) {
		t.Errorf("late writes after the logout were saved for the next sign-in: %v", written)
	}

	testingSignInFenceLateWrites(t, space.localState)
	if written := testingSignInFenceWritten(t, space.localState); len(written) != 10 {
		t.Errorf("the next sign-in saved only %v", written)
	}
}
