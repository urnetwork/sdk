// Real LocalState, provider construction and persistence controls. These
// fixtures do not exercise a native application or a real tunnel interface.
package sdk

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Uses the public key-material constructor with loopback-only network endpoints.
// Close and join stay outside SDK callbacks and complete before the next start.
func testingDeviceKeyLoadConstructor(t *testing.T, fixture *testingAuthClientShape, material *DeviceLocalKeyMaterial) *DeviceLocal {
	t.Helper()
	device, err := NewDeviceLocalWithKeyMaterial(
		fixture.networkSpace,
		fixture.initialJwt,
		"key-load-control",
		"test",
		"0.0.0",
		fixture.instanceId,
		false,
		material,
	)
	if err != nil || device == nil {
		t.Fatal("key-load control could not construct a real local device")
	}
	t.Cleanup(func() { testingDeviceKeyLoadJoin(t, device) })
	if device.GetKeyMaterial().IsEmpty() || len(device.GetPublicIdentityKey()) == 0 {
		t.Fatal("key-load control did not construct a provider identity")
	}
	return device
}

// Every constructed graph is joined; bounds diagnose deadlock, not absence.
func testingDeviceKeyLoadJoin(t *testing.T, device *DeviceLocal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := device.CloseAndWait(ctx); err != nil {
		t.Fatal("key-load control did not join its local device")
	}
}

// Checks optional bytes without putting their values in a failed assertion.
func testingDeviceKeyLoadEqual(first, second *DeviceLocalKeyMaterial) bool {
	return bytes.Equal(first.GetClientKeySeed(), second.GetClientKeySeed()) &&
		bytes.Equal(first.GetProvideTlsCertificatePem(), second.GetProvideTlsCertificatePem()) &&
		bytes.Equal(first.GetProvideTlsPrivateKeyPem(), second.GetProvideTlsPrivateKeyPem())
}

// The legacy loader's healthy path must keep the same provider identity through
// actual constructor/export/persist/restart, including an initial seed-only file.
func TestLegacyDeviceKeyLoadHealthyRestart(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	seed := make([]byte, 32)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	if err := fixture.localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil)); err != nil {
		t.Fatal("could not write the legacy healthy key fixture")
	}
	loaded := fixture.localState.GetDeviceLocalKeyMaterial()
	if loaded == nil || !bytes.Equal(loaded.GetClientKeySeed(), seed) {
		t.Fatal("legacy healthy key fixture was not loaded")
	}
	first := testingDeviceKeyLoadConstructor(t, fixture, loaded)
	identity := first.GetPublicIdentityKey()
	material := first.GetKeyMaterial()
	if !bytes.Equal(material.GetClientKeySeed(), seed) {
		t.Fatal("legacy healthy constructor changed the stored seed")
	}
	if err := fixture.localState.SetDeviceLocalKeyMaterial(material); err != nil {
		t.Fatal("could not persist the legacy healthy device identity")
	}
	testingDeviceKeyLoadJoin(t, first)
	reloaded := fixture.localState.GetDeviceLocalKeyMaterial()
	if !testingDeviceKeyLoadEqual(reloaded, material) {
		t.Fatal("legacy healthy persistence changed optional key fields")
	}
	second := testingDeviceKeyLoadConstructor(t, fixture, reloaded)
	if !bytes.Equal(second.GetPublicIdentityKey(), identity) ||
		!bytes.Equal(second.GetProvideTlsCertificatePem(), material.GetProvideTlsCertificatePem()) {
		t.Fatal("legacy healthy restart changed provider identity")
	}
	instance := fixture.localState.GetInstanceId()
	if fixture.localState.GetByJwt() != fixture.adminJwt ||
		fixture.localState.GetByClientJwt() != fixture.initialJwt ||
		instance == nil || instance.String() != fixture.instanceId.String() {
		t.Fatal("legacy healthy key restart changed the distinct auth roles")
	}
}
