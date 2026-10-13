//go:build !ios && !android && !js

package sdk

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/extender"
)

// A sign-out keeps the lag.dev measurement key beside the extender identity
// (lag.dev/PLAN.md C4), on every removal of a space's local state, with the
// rest of the extender state (local_state_fresh_network_test.go). The
// certificate is named by the extender's id, which the kept identity keeps, so
// a key lost to a sign-out would only have the operator issue the same name
// again.

// A measurement key kept where the provider extender role keeps it, and what
// to find after a removal.
type testingMeasurementKey struct {
	keyFile   string
	keyBytes  []byte
	publicKey *ecdsa.PublicKey
}

func testingSeedMeasurementKey(t *testing.T, localState *LocalState) testingMeasurementKey {
	t.Helper()
	keyFile := extender.ExtenderMeasurementKeyPath(filepath.Join(localState.localStorageDir, extenderKeyFileName))
	measurementCert, err := extender.NewExtenderMeasurementCert(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return testingMeasurementKey{
		keyFile:   keyFile,
		keyBytes:  keyBytes,
		publicKey: measurementCert.PublicKey(),
	}
}

// The removal kept the key file as it was, and it loads as the same key.
func testingRequireMeasurementKeyKept(t *testing.T, seeded testingMeasurementKey) {
	t.Helper()
	keyBytes, err := os.ReadFile(seeded.keyFile)
	if err != nil {
		t.Fatalf("the sign-out removed the measurement key: %v", err)
	}
	if !bytes.Equal(keyBytes, seeded.keyBytes) {
		t.Fatal("the sign-out rewrote the measurement key")
	}
	measurementCert, err := extender.NewExtenderMeasurementCert(seeded.keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if !measurementCert.PublicKey().Equal(seeded.publicKey) {
		t.Fatal("the measurement key loaded after the sign-out is another key")
	}
}

// The kept name is the one the role writes: connect/extender's, beside the
// extender identity.
func TestExtenderMeasurementKeyFileNameIsTheRoles(t *testing.T) {
	if extenderMeasurementKeyFileName != extender.ExtenderMeasurementKeyFileName {
		t.Fatalf("kept name = %q, the role writes %q", extenderMeasurementKeyFileName, extender.ExtenderMeasurementKeyFileName)
	}
	localStorageDir := t.TempDir()
	keyFile := extender.ExtenderMeasurementKeyPath(filepath.Join(localStorageDir, extenderKeyFileName))
	if keyFile != filepath.Join(localStorageDir, extenderMeasurementKeyFileName) {
		t.Fatalf("the role keeps the key at %s", keyFile)
	}
	if !keptAcrossSignOut(filepath.Base(keyFile)) {
		t.Fatal("a sign-out does not keep the measurement key")
	}
}

// An explicit sign-out.
func TestLocalStateLogoutKeepsTheMeasurementKey(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	seeded := testingSeedExtenderState(t, localState)
	seededKey := testingSeedMeasurementKey(t, localState)
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	// the account's state went, so this was a removal
	testingRequireExtenderStateKept(t, localState, seeded)
	testingRequireMeasurementKeyKept(t, seededKey)
}

// The device's own logout after the server rejects the client.
func TestRejectedClientLogoutKeepsTheMeasurementKey(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	seeded := testingSeedExtenderState(t, fixture.localState)
	seededKey := testingSeedMeasurementKey(t, fixture.localState)
	if !fixture.api.rejectByJwt(fixture.initialJwt, "") {
		t.Fatal("the rejection did not reach the device's logout")
	}
	if after, err := fixture.localState.loadAuthState(); err != nil || after.ByClientJwt != "" {
		t.Fatal("the rejection did not log the space out")
	}
	testingRequireExtenderStateKept(t, fixture.localState, seeded)
	testingRequireMeasurementKeyKept(t, seededKey)
}

// The conditional reset of a stale owner, which removes entries by the same
// rule through another path.
func TestPairedResetKeepsTheMeasurementKey(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	seeded := testingSeedExtenderState(t, fixture.localState)
	seededKey := testingSeedMeasurementKey(t, fixture.localState)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("the stale owner was not reset")
	}
	testingRequireExtenderStateKept(t, fixture.localState, seeded)
	testingRequireMeasurementKeyKept(t, seededKey)
}
