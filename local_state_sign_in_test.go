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
