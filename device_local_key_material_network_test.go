// A device identity belongs to the network it was made for (owner decision
// 2026-10-05: logout must not cross contaminate other networks). The paths
// that keep an identity across a sign-out without deleting it, the Android
// app's stale-state logout and the Apple tunnel's conditional reset, must not
// hand it to the next network's device; the same network keeps it.
package sdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
)

const (
	testingIdentityNetworkA = "00000000-0000-0000-0000-00000000000a"
	testingIdentityNetworkB = "00000000-0000-0000-0000-00000000000b"
)

// A synthetic unsigned client credential of the server's client shape for one
// network: these offline tests exercise identity selection, not admission.
func testingNetworkClientJwt(clientId connect.Id, networkId string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"client_id":"%s","device_id":"00000000-0000-0000-0000-000000000002","network_id":"%s","user_id":"00000000-0000-0000-0000-000000000003"}`,
		clientId,
		networkId,
	)))
	return fmt.Sprintf("%s.%s.", header, payload)
}

// The admin credential of one network, as an app stores it at sign-in.
func testingNetworkAdminJwt(networkId string) string {
	return testingJwt(map[string]any{
		"user_id":      "00000000-0000-0000-0000-000000000003",
		"network_id":   networkId,
		"network_name": "identity-network-test",
	})
}

// A 32-byte seed no device would generate.
func testingIdentitySeed(first byte) []byte {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = first + byte(i)
	}
	return seed
}

// Builds a device for `clientJwt` from `keyMaterial` and returns the client key
// seed it runs on.
func testingDeviceSeed(t *testing.T, networkSpace *NetworkSpace, clientJwt string, keyMaterial *DeviceLocalKeyMaterial) []byte {
	t.Helper()
	deviceLocal, err := NewDeviceLocalWithKeyMaterial(networkSpace, clientJwt, "", "", "", NewId(), false, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	seed := deviceLocal.GetClientKeySeed()
	if len(seed) == 0 {
		t.Fatal("the device runs on no client key")
	}
	return seed
}

// Material that names a network is taken only by that network's devices.
func TestDeviceIdentityIsNotTakenByAnotherNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}

	seed := testingIdentitySeed(1)
	keyMaterial := NewDeviceLocalKeyMaterial(seed, nil, nil)
	keyMaterial.networkId = testingIdentityNetworkA

	if got := testingDeviceSeed(t, networkSpace, testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkB), keyMaterial); bytes.Equal(got, seed) {
		t.Fatal("network B's device took network A's identity")
	}
	if got := testingDeviceSeed(t, networkSpace, testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA), keyMaterial); !bytes.Equal(got, seed) {
		t.Fatal("network A's device did not keep its own identity")
	}
}

// What a device hands out to persist names its network, and a later device
// for another network does not take it back.
func TestDeviceKeyMaterialNamesItsNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}

	deviceA, err := NewDeviceLocalWithKeyMaterial(networkSpace, testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA), "", "", "", NewId(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyMaterial := deviceA.GetKeyMaterial()
	deviceA.Close()
	if keyMaterial.networkId != testingIdentityNetworkA {
		t.Fatal("the device's key material does not name its network")
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	if err := localState.SetDeviceLocalKeyMaterial(keyMaterial); err != nil {
		t.Fatal(err)
	}
	stored, err := localState.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || stored.networkId != testingIdentityNetworkA {
		t.Fatal("the stored identity lost its network")
	}
	if got := testingDeviceSeed(t, networkSpace, testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkB), stored); bytes.Equal(got, keyMaterial.GetClientKeySeed()) {
		t.Fatal("network B's device took the identity network A's device made")
	}
}

// The Android app's stale-state logout re-saves the identity across the wipe;
// the next sign-in, to another network, starts on a new identity. The record
// was written before the network was kept, as an older sdk wrote it.
func TestStaleLogoutIdentityDoesNotFollowTheNextNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	seed := testingIdentitySeed(41)
	if err := localState.SetByJwt(testingNetworkAdminJwt(testingIdentityNetworkA)); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA)); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil)); err != nil {
		t.Fatal(err)
	}

	// MainApplication.logoutStaleLocalState
	kept := localState.GetDeviceLocalKeyMaterial()
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetDeviceLocalKeyMaterial(kept); err != nil {
		t.Fatal(err)
	}

	// the next sign-in, to network B, then DeviceManager.initDevice
	if err := localState.SetByJwt(testingNetworkAdminJwt(testingIdentityNetworkB)); err != nil {
		t.Fatal(err)
	}
	clientJwtB := testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkB)
	if err := localState.SetByClientJwt(clientJwtB); err != nil {
		t.Fatal(err)
	}
	if got := testingDeviceSeed(t, networkSpace, clientJwtB, localState.GetDeviceLocalKeyMaterial()); bytes.Equal(got, seed) {
		t.Fatal("network B's device took the identity network A's stale logout kept")
	}
}

// The same stale logout followed by the same network's sign-in keeps the
// identity, as it always did.
func TestStaleLogoutIdentityStaysWithItsNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	seed := testingIdentitySeed(73)
	if err := localState.SetByJwt(testingNetworkAdminJwt(testingIdentityNetworkA)); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA)); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil)); err != nil {
		t.Fatal(err)
	}
	kept := localState.GetDeviceLocalKeyMaterial()
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetDeviceLocalKeyMaterial(kept); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByJwt(testingNetworkAdminJwt(testingIdentityNetworkA)); err != nil {
		t.Fatal(err)
	}
	clientJwt := testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA)
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if got := testingDeviceSeed(t, networkSpace, clientJwt, localState.GetDeviceLocalKeyMaterial()); !bytes.Equal(got, seed) {
		t.Fatal("the same network's sign-in did not keep its identity")
	}
}

// The Apple tunnel's conditional reset of a stale owner preserves the identity
// file and returns it; a device for the next owner's client, on another
// network, does not run on it. The record predates the network field.
func TestPairedResetIdentityDoesNotFollowTheNextNetwork(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	seed := testingIdentitySeed(11)
	if err := fixture.localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil)); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("the stale owner was not reset")
	}
	preserved := result.GetDeviceLocalKeyMaterial()
	if preserved == nil || !bytes.Equal(preserved.GetClientKeySeed(), seed) {
		t.Fatal("the reset did not return the identity it preserved")
	}
	if got := testingDeviceSeed(t, fixture.networkSpace, testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkB), preserved); bytes.Equal(got, seed) {
		t.Fatal("the next owner's device, on another network, took the reset owner's identity")
	}
}
