// Network omission is a server-recoverable legacy shape, not a missing
// provider identity. All auth values here are synthetic and remain unlogged.
package sdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A read-only selection must preserve the supplied bytes without borrowing
// a network from separate admin auth or committing a speculative instance.
func testingClientStartupUnknownNetwork(t *testing.T, networkClaim any) {
	t.Helper()
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	adminJwt := testingJwt(map[string]any{
		"user_id":    "00000000-0000-0000-0000-000000000003",
		"network_id": "00000000-0000-0000-0000-000000000009",
	})
	if err := localState.SetByJwt(adminJwt); err != nil {
		t.Fatal(err)
	}
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	supplied := testingStartupClientJwt(t, "recoverable-network", map[string]any{
		"network_id": networkClaim,
	})
	selected, err := localState.SelectClientJwtForInstance(supplied, NewId())
	if err != nil {
		t.Fatal("startup rejected a server-recoverable network identity")
	}
	identity, err := parseStartupClientJwt(selected)
	if err != nil {
		t.Fatal("selected client was not parseable")
	}
	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if selected != supplied || identity.networkId != (connect.Id{}) || after != before {
		t.Error("startup rewrote unknown network auth or borrowed the separate admin identity")
	}
}

// Missing fields decode to zero in the server's signed ByJwt struct.
func TestClientStartupAcceptsOmittedNetworkWithoutRewriting(t *testing.T) {
	testingClientStartupUnknownNetwork(t, nil)
}

// The explicit zero UUID reaches the same server recovery branch.
func TestClientStartupAcceptsZeroNetworkWithoutRewriting(t *testing.T) {
	testingClientStartupUnknownNetwork(t, "00000000-0000-0000-0000-000000000000")
}

// JSON null and malformed present values do not have the server's omission
// semantics. The startup check must not collapse those cases into unknown.
func TestClientStartupRejectsMalformedPresentNetwork(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty", value: ""},
		{name: "malformed", value: "not-a-uuid"},
		{name: "compact", value: "00000000000000000000000000000004"},
		{name: "number", value: 1},
		{name: "array", value: []string{}},
	}
	for _, test := range cases {
		localState := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(localState.Close)
		supplied := testingJwt(map[string]any{
			"client_id":  "00000000-0000-0000-0000-000000000001",
			"device_id":  "00000000-0000-0000-0000-000000000002",
			"network_id": test.value,
		})
		if _, err := localState.SelectClientJwtForInstance(supplied, NewId()); err == nil {
			t.Errorf("startup accepted a malformed present network claim: %s", test.name)
		}
		state, err := localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if state.ByJwt != "" || state.ByClientJwt != "" || state.InstanceId != "" {
			t.Error("rejected malformed network claim changed auth")
		}
	}
}

// A current fully specified provider identity remains admitted.
func TestClientStartupCompleteNetworkHealthyControl(t *testing.T) {
	stored := testingStartupClientJwt(t, "known-stored", nil)
	supplied := testingStartupClientJwt(t, "known-newer", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	testingSelectClientStartup(t, stored, supplied, supplied)
}

// A refreshed credential supplies the network omitted by its predecessor.
func TestClientStartupSelectsRecoveredNetworkOverOlderOmission(t *testing.T) {
	stored := testingStartupClientJwt(t, "omitted-stored", map[string]any{"network_id": nil})
	supplied := testingStartupClientJwt(t, "known-newer", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	testingSelectClientStartup(t, stored, supplied, supplied)
}

// A stale profile may still carry the omitted-network predecessor after disk
// has the refreshed client. Startup must select the durable complete token.
func TestClientStartupKeepsRecoveredNetworkAgainstOldOmission(t *testing.T) {
	stored := testingStartupClientJwt(t, "known-stored", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	supplied := testingStartupClientJwt(t, "omitted-older", map[string]any{"network_id": nil})
	testingSelectClientStartup(t, stored, supplied, stored)
}

// Network recovery does not relax the current client/device binding.
func TestClientStartupUnknownNetworkCannotChangeClientOrDevice(t *testing.T) {
	for _, key := range []string{"client_id", "device_id"} {
		localState := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(localState.Close)
		stored := testingStartupClientJwt(t, "stored", nil)
		instanceId := NewId()
		if err := localState.SetByClientJwtForInstance(stored, instanceId); err != nil {
			t.Fatal(err)
		}
		before, err := localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		supplied := testingStartupClientJwt(t, "different-provider", map[string]any{
			"network_id": nil,
			key:          "00000000-0000-0000-0000-000000000009",
			"iat":        testingClientStartupNow - 10,
		})
		if _, err := localState.SelectClientJwtForInstance(supplied, instanceId); err == nil {
			t.Errorf("unknown network allowed a different %s", key)
		}
		after, err := localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Error("failed unknown-network identity comparison changed auth")
		}
	}
}

// The original key-material fixture had only client_id. Its exact shape
// remains inadmissible under today's provider refresh contract, before any
// constructor can disturb a serving owner or write key/auth state.
func TestDeviceLocalRejectsClientOnlyKeyFixtureWithoutAuthMutation(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	fixture.initialJwt = testingJwt(map[string]any{
		"client_id": "00000000-0000-0000-0000-000000000001",
	})
	built := testingBuildReservedDevice(fixture, false)
	testingCleanupNetworkCompatibilityDevice(t, built)
	if built.local != nil || built.err == nil ||
		!strings.Contains(built.err.Error(), "startup client JWT is missing device_id") {
		t.Fatal("client-only fixture did not fail at the current provider identity boundary")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || !fixture.api.deviceOwnsByJwt(fixture.localDevice.authPublication, before.ByClientJwt) {
		t.Error("invalid client-only fixture changed a serving auth owner")
	}
}

// Captures each concrete device for cleanup; replacing a fixture's callbacks
// must not accidentally join only its last device twice.
func testingCleanupNetworkCompatibilityDevice(t *testing.T, built testingAuthConstructorResult) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if built.local != nil {
			if err := built.local.CloseAndWait(ctx); err != nil {
				t.Error("network compatibility local device did not join")
			}
		}
		if built.remote != nil {
			if err := built.remote.CloseAndWait(ctx); err != nil {
				t.Error("network compatibility remote device did not join")
			}
		}
	})
}

// Reads the successfully published provider-owned credential, never the
// shared API field that may subsequently contain an explicit admin login.
func testingNetworkCompatibilityClient(t *testing.T, built testingAuthConstructorResult) string {
	t.Helper()
	if built.local != nil {
		return built.local.GetClientJwt()
	}
	if built.remote != nil {
		return built.remote.GetClientJwt()
	}
	t.Fatal("network compatibility constructor returned no device")
	return ""
}

// Real constructors, installed refresh callbacks and a stale-profile restart
// must converge on the exact server-supplied recovered JWT without rewriting it.
func testingDeviceNetworkRecoveryAndRestart(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "legacy-network-omitted", map[string]any{
		"network_id": nil, "iat": nil, "exp": nil,
	})
	fixture.seedDistinctLogin(t)
	if remote {
		fixture.startRemote(t)
	} else {
		fixture.startLocal(t)
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByClientJwt != fixture.initialJwt || fixture.deviceJwt() != fixture.initialJwt ||
		fixture.api.GetByJwt() != fixture.initialJwt {
		t.Fatal("initial unknown-network constructor changed the client credential")
	}
	recovered := testingStartupClientJwt(t, "network-recovered", nil)
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, recovered) {
		t.Fatal("actual recovery refresh was not accepted")
	}
	afterRefresh, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterRefresh.ByClientJwt != recovered || fixture.deviceJwt() != recovered ||
		afterRefresh.ByJwt != fixture.adminJwt || afterRefresh.InstanceId != fixture.instanceId.String() {
		t.Fatal("installed recovery callback lost client/admin/instance separation")
	}
	built := testingBuildReservedDevice(fixture, remote)
	testingCleanupNetworkCompatibilityDevice(t, built)
	if built.err != nil {
		t.Fatal("stale-profile restart rejected the recovered durable client")
	}
	if testingNetworkCompatibilityClient(t, built) != recovered || fixture.api.GetByJwt() != recovered {
		t.Error("stale-profile restart did not publish the recovered credential")
	}
	next := testingStartupClientJwt(t, "post-recovery-restart", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(recovered, next) {
		t.Fatal("restarted owner could not accept its next refresh")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByClientJwt != next || testingNetworkCompatibilityClient(t, built) != next ||
		after.ByJwt != fixture.adminJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("recovery restart stranded refresh ownership or changed the admin/instance")
	}
}

// Local providers preserve the server-recoverable shape through real callbacks.
func TestDeviceLocalNetworkRecoveryAndStaleProfileRestart(t *testing.T) {
	testingDeviceNetworkRecoveryAndRestart(t, false)
}

// Native remotes use the same recovery and provider publication contract.
func TestDeviceRemoteNetworkRecoveryAndStaleProfileRestart(t *testing.T) {
	testingDeviceNetworkRecoveryAndRestart(t, true)
}

// A real API refresh is held after API commit and before device/storage
// publication. In the conflict case storage says N1, supplied is unknown, and
// the API says N2: comparing only the selected unknown to N2 is insufficient.
func testingDeviceNetworkRefreshOverlap(t *testing.T, remote bool, conflict bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	networkClaim := any(nil)
	if conflict {
		networkClaim = "00000000-0000-0000-0000-000000000004"
	}
	fixture.initialJwt = testingStartupClientJwt(t, "before-network-refresh", map[string]any{
		"network_id": networkClaim,
	})
	fixture.seedDistinctLogin(t)
	var owner *deviceAuthPublicationGate
	if remote {
		fixture.startRemote(t)
		owner = fixture.remoteDevice.authPublication
	} else {
		fixture.startLocal(t)
		owner = fixture.localDevice.authPublication
	}
	previousJwt := fixture.initialJwt
	currentNetwork := "00000000-0000-0000-0000-000000000004"
	if conflict {
		currentNetwork = "00000000-0000-0000-0000-000000000009"
		fixture.initialJwt = testingStartupClientJwt(t, "unknown-newer-proposal", map[string]any{
			"network_id": nil, "iat": testingClientStartupNow - 50,
		})
	}
	refreshed := testingStartupClientJwt(t, "held-network-refresh", map[string]any{
		"network_id": currentNetwork, "iat": testingClientStartupNow - 10,
	})
	entered, resume := testingHoldAuthPublication(t, owner)
	refreshDone := make(chan struct{})
	accepted := false
	go func() {
		defer close(refreshDone)
		accepted = fixture.api.setRefreshedByJwt(previousJwt, refreshed)
	}()
	t.Cleanup(func() {
		resume()
		testingAwaitAuthBoundary(t, refreshDone)
	})
	testingAwaitAuthBoundary(t, entered)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if before.ByClientJwt != previousJwt || fixture.api.GetByJwt() != refreshed {
		t.Fatal("held refresh did not establish the API/storage ordering")
	}
	done := make(chan testingAuthConstructorResult, 1)
	go func() { done <- testingBuildReservedDevice(fixture, remote) }()
	var built testingAuthConstructorResult
	select {
	case built = <-done:
	case <-time.After(5 * time.Second):
		resume()
		t.Error("startup joined the held prior device callback")
		select {
		case built = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("startup did not finish after releasing its prior callback")
		}
	}
	testingCleanupNetworkCompatibilityDevice(t, built)
	if conflict {
		if built.err == nil || built.local != nil || built.remote != nil {
			resume()
			testingAwaitAuthBoundary(t, refreshDone)
			t.Fatal("startup used an unknown network to bridge conflicting known identities")
		}
		if !strings.Contains(built.err.Error(), "serving API client differs from the established startup identity") {
			t.Fatal("conflicting known network did not reach the three-candidate identity check")
		}
		after, err := fixture.localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if after != before || !fixture.api.deviceOwnsByJwt(owner, refreshed) {
			t.Error("rejected network conflict changed storage or stole the serving API owner")
		}
	} else if built.err != nil {
		t.Fatal("startup rejected a held server recovery refresh")
	} else if testingNetworkCompatibilityClient(t, built) != refreshed {
		t.Error("startup failed to adopt the exact API-owned recovered credential")
	}
	resume()
	testingAwaitAuthBoundary(t, refreshDone)
	if !accepted {
		t.Fatal("held current refresh did not finish")
	}
	next := testingStartupClientJwt(t, "after-held-network-refresh", map[string]any{
		"network_id": currentNetwork, "iat": testingClientStartupNow - 1,
	})
	if !fixture.api.setRefreshedByJwt(refreshed, next) {
		t.Fatal("surviving owner could not accept its next refresh")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	currentJwt := fixture.deviceJwt()
	if !conflict {
		currentJwt = testingNetworkCompatibilityClient(t, built)
	}
	if after.ByClientJwt != next || currentJwt != next || after.ByJwt != fixture.adminJwt ||
		after.InstanceId != fixture.instanceId.String() {
		t.Error("network selection lost the surviving owner's auth publication")
	}
}

// Omitted-network recovery may already be committed at the API, ahead of disk.
func TestDeviceLocalAdoptsHeldNetworkRecovery(t *testing.T) {
	testingDeviceNetworkRefreshOverlap(t, false, false)
}

// Native remotes must make the same coherent API/storage choice.
func TestDeviceRemoteAdoptsHeldNetworkRecovery(t *testing.T) {
	testingDeviceNetworkRefreshOverlap(t, true, false)
}

// Unknown is not a transitive bridge between two known conflicting networks.
func TestDeviceLocalRejectsUnknownNetworkBridgeDuringRefresh(t *testing.T) {
	testingDeviceNetworkRefreshOverlap(t, false, true)
}

// The real remote constructor must reject the same three-candidate conflict.
func TestDeviceRemoteRejectsUnknownNetworkBridgeDuringRefresh(t *testing.T) {
	testingDeviceNetworkRefreshOverlap(t, true, true)
}
