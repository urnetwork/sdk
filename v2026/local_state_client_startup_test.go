// Startup freshness and partial-store cases use fixed JWT dates and real
// LocalState transactions. Identity comparisons never inspect an admin token.
package sdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const testingClientStartupNow int64 = 2000000000

// Supplies a complete client identity; nil overrides remove a claim.
func testingStartupClientJwt(t *testing.T, marker string, overrides map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"client_id":  "00000000-0000-0000-0000-000000000001",
		"device_id":  "00000000-0000-0000-0000-000000000002",
		"network_id": "00000000-0000-0000-0000-000000000004",
		"iat":        testingClientStartupNow - 100,
		"exp":        testingClientStartupNow + 1000,
		"marker":     marker,
	}
	for key, value := range overrides {
		if value == nil {
			delete(claims, key)
		} else {
			claims[key] = value
		}
	}
	return testingJwt(claims)
}

// Exercises read-only preparation, with distinct admin/client
// values and a saved routing preference as the non-destructive control.
func testingSelectClientStartup(t *testing.T, stored string, supplied string, want string) {
	t.Helper()
	localState := newLocalState(context.Background(), t.TempDir())
	adminJwt := testingJwt(map[string]any{"network_id": "00000000-0000-0000-0000-000000000004"})
	if err := localState.SetByJwt(adminJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := NewId()
	if err := localState.SetByClientJwtForInstance(stored, instanceId); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	selected, err := localState.selectClientJwtForInstance(
		supplied, instanceId, time.Unix(testingClientStartupNow, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if selected != want || state.ByClientJwt != stored {
		t.Error("startup did not select the expected client without changing the store")
	}
	if state.ByJwt != adminJwt || state.InstanceId != instanceId.String() || !localState.GetBlockerEnabled() {
		t.Error("client selection changed the separate admin, stable instance or routing state")
	}
}

// A later durable issue date must not be replaced by an older profile token.
func TestClientStartupKeepsNewerDurableIssueDate(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "older", map[string]any{"iat": testingClientStartupNow - 200})
	testingSelectClientStartup(t, stored, supplied, stored)
}

// A genuinely newer supplied token must not be trapped behind an old store.
func TestClientStartupAcceptsNewerSuppliedIssueDate(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "newer", map[string]any{"iat": testingClientStartupNow - 50})
	testingSelectClientStartup(t, stored, supplied, supplied)
}

// No total ordering is invented for different tokens minted in the same second.
func TestClientStartupEqualDatesKeepDurableToken(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "same-dates", nil)
	testingSelectClientStartup(t, stored, supplied, stored)
}

// A token without issue/expiry dates cannot displace a dated durable token.
func TestClientStartupUndatedSuppliedTokenKeepsDurableToken(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "undated", map[string]any{"iat": nil, "exp": nil})
	testingSelectClientStartup(t, stored, supplied, stored)
}

// Two undated credentials are tied; the existing durable token wins.
func TestClientStartupTwoUndatedTokensKeepDurableToken(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", map[string]any{"iat": nil, "exp": nil})
	supplied := testingStartupClientJwt(t, "supplied", map[string]any{"iat": nil, "exp": nil})
	testingSelectClientStartup(t, stored, supplied, stored)
}

// Expiry protection has precedence over issue date, as on existing Apple startup.
func TestClientStartupRejectsExpiredSuppliedToken(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "expired", map[string]any{
		"iat": testingClientStartupNow - 50, "exp": testingClientStartupNow,
	})
	testingSelectClientStartup(t, stored, supplied, stored)
}

// A usable supplied credential can recover an expired persisted copy.
func TestClientStartupReplacesExpiredDurableToken(t *testing.T) {
	stored := testingStartupClientJwt(t, "expired", map[string]any{"exp": testingClientStartupNow})
	supplied := testingStartupClientJwt(t, "usable", map[string]any{"iat": testingClientStartupNow - 200})
	testingSelectClientStartup(t, stored, supplied, supplied)
}

// Equal issue dates use expiry as the existing secondary freshness signal.
func TestClientStartupEqualIssueDateUsesLaterExpiry(t *testing.T) {
	stored := testingStartupClientJwt(t, "stored", nil)
	supplied := testingStartupClientJwt(t, "longer", map[string]any{"exp": testingClientStartupNow + 2000})
	testingSelectClientStartup(t, stored, supplied, supplied)
}

// A different identity cannot reuse an established provider instance.
func testingClientStartupRejectsIdentityChange(t *testing.T, key string) {
	t.Helper()
	localState := newLocalState(context.Background(), t.TempDir())
	stored := testingStartupClientJwt(t, "stored", nil)
	instanceId := NewId()
	if err := localState.SetByClientJwtForInstance(stored, instanceId); err != nil {
		t.Fatal(err)
	}
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	supplied := testingStartupClientJwt(t, "changed", map[string]any{
		key:   "00000000-0000-0000-0000-000000000009",
		"iat": testingClientStartupNow - 10,
	})
	if _, err := localState.SelectClientJwtForInstance(supplied, instanceId); err == nil {
		t.Error("same-instance startup accepted another logical identity")
	}
	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("failed identity comparison changed durable auth")
	}
}

// Client identity is part of the established provider owner.
func TestClientStartupRejectsSameInstanceDifferentClient(t *testing.T) {
	testingClientStartupRejectsIdentityChange(t, "client_id")
}

// Device identity is not interchangeable even when the client id matches.
func TestClientStartupRejectsSameInstanceDifferentDevice(t *testing.T) {
	testingClientStartupRejectsIdentityChange(t, "device_id")
}

// Network identity participates in the comparison independently of other ids.
func TestClientStartupRejectsSameInstanceDifferentNetwork(t *testing.T) {
	testingClientStartupRejectsIdentityChange(t, "network_id")
}

// A new-login instance may establish a different client under the same admin.
func TestClientStartupAcceptsNewIdentityWithNewInstance(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	stored := testingStartupClientJwt(t, "stored", nil)
	if err := localState.SetByClientJwtForInstance(stored, NewId()); err != nil {
		t.Fatal(err)
	}
	supplied := testingStartupClientJwt(t, "new-login", map[string]any{
		"client_id": "00000000-0000-0000-0000-000000000009",
	})
	instanceId := NewId()
	selected, err := localState.SelectClientJwtForInstance(supplied, instanceId)
	if err != nil {
		t.Fatal(err)
	}
	if selected != supplied || localState.GetInstanceId().Cmp(instanceId) == 0 ||
		localState.GetByClientJwt() != stored || localState.GetByJwt() != "" {
		t.Error("new-login preparation committed speculative client or instance state")
	}
}

// The supplied stable instance repairs its exactly matching unpaired client.
func TestClientStartupPreparesMatchingClientMissingInstanceWithoutMutation(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	clientJwt := testingStartupClientJwt(t, "matching", nil)
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if err := localState.SetInstanceId(nil); err != nil {
		t.Fatal(err)
	}
	selected, err := localState.SelectClientJwtForInstance(clientJwt, instanceId)
	if err != nil {
		t.Fatal(err)
	}
	if selected != clientJwt || localState.GetInstanceId() != nil {
		t.Error("matching partial-state preparation changed its durable instance")
	}
}

// A missing instance cannot authorize choosing between different credentials.
func TestClientStartupRejectsDifferentClientMissingInstance(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	if err := localState.SetByClientJwt(testingStartupClientJwt(t, "stored", nil)); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetInstanceId(nil); err != nil {
		t.Fatal(err)
	}
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := localState.SelectClientJwtForInstance(testingStartupClientJwt(t, "supplied", nil), NewId()); err == nil {
		t.Error("startup guessed ownership from an unpaired different client")
	}
	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("ambiguous partial-state failure changed auth")
	}
}

// An instance alone is not a restartable provider credential.
func TestClientStartupRejectsInstanceWithoutClient(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	instanceId := NewId()
	if err := localState.SetInstanceId(instanceId); err != nil {
		t.Fatal(err)
	}
	if _, err := localState.SelectClientJwtForInstance(testingStartupClientJwt(t, "supplied", nil), instanceId); err == nil {
		t.Error("startup silently repaired an instance-only auth envelope")
	}
	if localState.GetByClientJwt() != "" {
		t.Error("failed partial-state check synthesized client auth")
	}
}

// Admin-only or device-less tokens cannot use the current provider refresh
// contract. Network omission has separate server-recovery coverage.
func TestClientStartupRejectsMissingIdentityClaims(t *testing.T) {
	for _, key := range []string{"client_id", "device_id"} {
		localState := newLocalState(context.Background(), t.TempDir())
		supplied := testingStartupClientJwt(t, "missing-claim", map[string]any{key: nil})
		if _, err := localState.SelectClientJwtForInstance(supplied, NewId()); err == nil {
			t.Errorf("startup accepted a token missing %s", key)
		}
		state, err := localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if state.ByJwt != "" || state.ByClientJwt != "" || state.InstanceId != "" {
			t.Error("rejected non-client token changed empty auth")
		}
	}
}

// Deterministic envelope-size failure occurs after owner selection but before
// durable commit. It must preserve the prior live API and LocalState owner.
func TestClientStartupCommitFailurePreservesPreviousDeviceOwner(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "initial", nil)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	supplied := testingStartupClientJwt(t, "oversize", map[string]any{
		"iat":     testingClientStartupNow - 10,
		"padding": strings.Repeat("x", localAuthStateMaxBytes),
	})
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, supplied, "failed-seed", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if device != nil {
		device.Close()
		t.Fatal("oversize auth started a replacement device")
	}
	if err == nil {
		t.Fatal("oversize auth commit unexpectedly succeeded")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	fixture.localState.authStateLock.Lock()
	owner := fixture.localState.deviceAuthOwner
	fixture.localState.authStateLock.Unlock()
	if after != before || owner != fixture.localDevice.authPublication ||
		!fixture.api.deviceOwnsByJwt(fixture.localDevice.authPublication, fixture.initialJwt) {
		t.Error("failed durable startup commit stole the previous live auth owner")
	}
}
