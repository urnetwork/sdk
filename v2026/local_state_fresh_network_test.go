// A sign-out leaves the extender state alone (owner, 2026-10-05: local
// accounts on a device are trusted to a degree, and Account > Extenders has its
// own reset) while it removes the account's state: every removal of a space's
// local state keeps the extender directory, the gossip role, the extender
// identity and the provider extender setting, on disk and in memory.
package sdk

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// The extender state a test seeds, to find after a removal.
type testingExtenderState struct {
	directory []byte
	keySeed   []byte
}

// Seeds every piece of extender state, with the provider extender switched off
// (not the default), and one piece of account state, the saved destination.
func testingSeedExtenderState(t *testing.T, localState *LocalState) testingExtenderState {
	t.Helper()
	directory := []byte(`{"version":1,"records":[],"addresses":[{"ip":"192.0.2.10","source":"import"}]}`)
	if err := localState.setExtenders(directory); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetExtenderGossipMode(ExtenderGossipModeMember); err != nil {
		t.Fatal(err)
	}
	keySeed, err := localState.GetOrCreateExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := localState.SetProvideExtender(false); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetConnectLocation(testingStoredLocation("account-destination")); err != nil {
		t.Fatal(err)
	}
	return testingExtenderState{directory: directory, keySeed: keySeed}
}

// The removal kept every piece of extender state, in this object and on disk,
// and removed the saved destination.
func testingRequireExtenderStateKept(t *testing.T, localState *LocalState, seeded testingExtenderState) {
	t.Helper()
	// this object, then a cold read of the same files
	cold := newLocalState(context.Background(), filepath.Dir(localState.localStorageDir))
	defer cold.Close()
	for _, state := range []*LocalState{localState, cold} {
		if directory, err := state.getExtenders(); err != nil || !bytes.Equal(directory, seeded.directory) {
			t.Error("the sign-out removed the extender directory")
		}
		if state.GetExtenderGossipMode() != ExtenderGossipModeMember {
			t.Error("the sign-out reset the gossip role")
		}
		if keySeed, err := state.GetOrCreateExtenderKeySeed(); err != nil || !bytes.Equal(keySeed, seeded.keySeed) {
			t.Error("the sign-out replaced the extender identity")
		}
		if state.GetProvideExtender() {
			t.Error("the sign-out reset the provider extender setting")
		}
		if location, err := state.LoadConnectLocation(); err != nil || location != nil {
			t.Error("the sign-out kept the account's saved destination")
		}
	}
}

// An explicit sign-out.
func TestLocalStateLogoutLeavesTheExtenderState(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	seeded := testingSeedExtenderState(t, localState)
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	testingRequireExtenderStateKept(t, localState, seeded)
}

// The device's own logout after the server rejects the client.
func TestRejectedClientLogoutLeavesTheExtenderState(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	seeded := testingSeedExtenderState(t, fixture.localState)
	if !fixture.api.rejectByJwt(fixture.initialJwt, "") {
		t.Fatal("the rejection did not reach the device's logout")
	}
	if after, err := fixture.localState.loadAuthState(); err != nil || after.ByClientJwt != "" {
		t.Fatal("the rejection did not log the space out")
	}
	testingRequireExtenderStateKept(t, fixture.localState, seeded)
}

// The conditional reset of a stale owner.
func TestPairedResetLeavesTheExtenderState(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	seeded := testingSeedExtenderState(t, fixture.localState)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("the stale owner was not reset")
	}
	testingRequireExtenderStateKept(t, fixture.localState, seeded)
}
