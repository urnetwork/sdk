// The shipped Apple extension stored only a client marker in ByJwt plus its
// stable instance. Exact independent client input can repair that shape, never
// manufacture an admin credential or infer a rotated marker's ownership.
package sdk

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Seeds the historical extension marker without a derived-client field.
func testingLegacyExtensionMarker(t *testing.T) *testingAuthClientShape {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "legacy-extension-marker", nil)
	if err := fixture.localState.SetByJwt(fixture.initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetInstanceId(fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// The constructor may seed only its supplied client while keeping the old
// marker opaque and preserving every routing/instance value.
func testingLegacyExtensionConstructor(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingLegacyExtensionMarker(t)
	if remote {
		fixture.startRemote(t)
	} else {
		fixture.startLocal(t)
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != fixture.initialJwt || state.ByClientJwt != fixture.initialJwt ||
		state.InstanceId != fixture.instanceId.String() ||
		fixture.deviceJwt() != fixture.initialJwt || !fixture.localState.GetBlockerEnabled() {
		t.Error("exact legacy marker repair lost provider identity or routing state")
	}
	nextJwt := testingStartupClientJwt(t, "after-legacy-repair", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt) {
		t.Fatal("repaired provider did not accept its client refresh")
	}
	next, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if next.ByJwt != fixture.initialJwt || next.ByClientJwt != nextJwt ||
		next.InstanceId != fixture.instanceId.String() || fixture.deviceJwt() != nextJwt {
		t.Error("legacy repair relabeled or clobbered its retained opaque marker")
	}
}

// A real Local constructor accepts the exact shipped extension shape.
func TestLocalConstructorRepairsExactLegacyExtensionMarker(t *testing.T) {
	testingLegacyExtensionConstructor(t, false)
}

// The shared native Remote path must not apply a different credential policy.
func TestRemoteConstructorRepairsExactLegacyExtensionMarker(t *testing.T) {
	testingLegacyExtensionConstructor(t, true)
}

// Ambiguous legacy state is never guessed from an admin field or token dates.
func testingLegacyExtensionAmbiguity(t *testing.T, mutation string) {
	t.Helper()
	fixture := testingLegacyExtensionMarker(t)
	supplied := fixture.initialJwt
	instanceId := fixture.instanceId
	switch mutation {
	case "different-instance":
		instanceId = NewId()
	case "different-marker":
		if err := fixture.localState.SetByJwt(testingStartupClientJwt(t, "different-marker", nil)); err != nil {
			t.Fatal(err)
		}
	case "rotated-client":
		supplied = testingStartupClientJwt(t, "ambiguous-rotation", map[string]any{
			"iat": testingClientStartupNow - 10,
		})
	case "admin-only":
		supplied = testingJwt(map[string]any{
			"network_id": "00000000-0000-0000-0000-000000000004",
		})
		if err := fixture.localState.SetByJwt(supplied); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unsupported legacy ambiguity")
	}
	// SetByJwt intentionally clears pairing on a changed marker. Recreate
	// the historical nonempty-instance/missing-client shape being tested;
	// an empty instance would instead describe an allowed fresh login.
	if err := fixture.localState.SetInstanceId(fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if before.InstanceId != fixture.instanceId.String() || before.ByClientJwt != "" {
		t.Fatal("legacy ambiguity fixture lost its required partial-store shape")
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, supplied, "legacy-ambiguity", "test", "0.0.0",
		instanceId, settings, connect.NewId(),
	)
	if device != nil {
		_ = device.CloseAndWait(context.Background())
		t.Error("ambiguous legacy constructor returned a provider")
	}
	after, readErr := fixture.localState.loadAuthState()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || after != before || !fixture.localState.GetBlockerEnabled() ||
		fixture.api.GetByJwt() != "" {
		t.Error("ambiguous legacy startup changed auth or routing state")
	}
}

// An established instance cannot be reassigned through a legacy-marker repair.
func TestLegacyExtensionMarkerRejectsDifferentInstance(t *testing.T) {
	testingLegacyExtensionAmbiguity(t, "different-instance")
}

// Equality of logical identity is not equality of the historical marker.
func TestLegacyExtensionMarkerRejectsDifferentStoredMarker(t *testing.T) {
	testingLegacyExtensionAmbiguity(t, "different-marker")
}

// Fresh dates alone cannot authorize recovering a missing client field.
func TestLegacyExtensionMarkerRejectsAmbiguousRotatedClient(t *testing.T) {
	testingLegacyExtensionAmbiguity(t, "rotated-client")
}

// Even exact bytes do not make an admin-only JWT a provider credential.
func TestLegacyExtensionMarkerRejectsAdminOnlyCredential(t *testing.T) {
	testingLegacyExtensionAmbiguity(t, "admin-only")
}

// Compatibility preparation must not seed a legacy field before allocation
// succeeds. No OS interface, pool exhaustion or privileged mutation is used.
func TestFailedLocalConstructorPreservesExactLegacyMarker(t *testing.T) {
	fixture := testingLegacyExtensionMarker(t)
	fixture.api.SetByJwt("previous-legacy-api")
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) { return netip.Addr{}, false }
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "legacy-failure", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if device != nil {
		_ = device.CloseAndWait(context.Background())
		t.Error("failed legacy constructor exposed a provider")
	}
	after, readErr := fixture.localState.loadAuthState()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || after != before || !fixture.localState.GetBlockerEnabled() ||
		fixture.api.GetByJwt() != "previous-legacy-api" {
		t.Error("failed legacy constructor committed speculative compatibility state")
	}
}
