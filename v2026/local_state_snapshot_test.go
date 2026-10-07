// All claim parsing begins with the real stored envelope or its captured
// snapshot; malformed types must not panic or return partial identity.
package sdk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalAuthSnapshotRetainsSeparateImmutableAdminAndClient(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	if snapshot.GetByJwt() != fixture.adminJwt || snapshot.GetByClientJwt() != fixture.initialJwt || snapshot.GetEmpty() {
		t.Fatal("snapshot merged separate admin/client credentials")
	}
	instance := snapshot.GetInstanceId()
	if instance == nil || instance.Cmp(fixture.instanceId) != 0 {
		t.Fatal("snapshot lost stable instance")
	}
	instance.id[0] ^= 1
	if snapshot.GetInstanceId().Cmp(fixture.instanceId) != 0 {
		t.Fatal("caller mutated captured instance")
	}
	if err := fixture.localState.SetByJwt(testingJwt(map[string]any{"network_name": "replacement"})); err != nil {
		t.Fatal("could not install later admin state")
	}
	claims, err := snapshot.ParseByJwt()
	if err != nil || claims == nil || claims.NetworkName != "client-shapes-test" ||
		snapshot.GetByJwt() != fixture.adminJwt || snapshot.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("snapshot accessor reread later storage")
	}
	current, err := fixture.localState.ParseByJwt()
	if err != nil || current == nil || current.NetworkName != "replacement" {
		t.Fatal("legacy parser failed healthy current read")
	}
}

func TestLocalAuthSnapshotMalformedClaimTypesReturnControlledErrors(t *testing.T) {
	for _, field := range []string{"user_id", "network_id", "network_name", "guest_mode", "pro"} {
		for _, value := range []any{nil, []any{"private-marker"}, map[string]any{"private-marker": 1}, 42} {
			state := newLocalState(context.Background(), t.TempDir())
			t.Cleanup(state.Close)
			if err := state.SetByJwt(testingJwt(map[string]any{field: value})); err != nil {
				t.Fatal("could not persist malformed-claim fixture")
			}
			snapshot, err := state.GetAuthStateSnapshot()
			if err != nil {
				t.Fatal("could not capture malformed-claim envelope")
			}
			for _, parse := range []func() (*ByJwt, error){state.ParseByJwt, snapshot.ParseByJwt} {
				claims, err := parse()
				if err == nil || claims != nil || strings.Contains(err.Error(), "private-marker") {
					t.Fatal("wrong claim type was partial, accepted, or exposed stored values")
				}
			}
		}
	}
}

func TestLocalAuthSnapshotHealthyOptionalClaimsAndClientOnlyCompatibility(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	if err := state.SetByJwt(testingJwt(map[string]any{
		"user_id": "not-an-id", "network_id": "", "network_name": "legacy", "guest_mode": true, "pro": false,
	})); err != nil {
		t.Fatal("could not save optional claims")
	}
	claims, err := state.ParseByJwt()
	if err != nil || claims == nil || claims.UserId != nil || claims.NetworkId != nil || claims.NetworkName != "legacy" || !claims.GuestMode || claims.Pro {
		t.Fatal("healthy legacy optional claim semantics changed")
	}
	if err := state.SetByJwt(""); err != nil {
		t.Fatal("could not clear admin fixture")
	}
	client := testingRefreshableJwtWithMarker(t, "client-only")
	if err := state.SetByClientJwt(client); err != nil {
		t.Fatal("could not save client-only fixture")
	}
	snapshot, err := state.GetAuthStateSnapshot()
	if err != nil || snapshot.GetByJwt() != "" || snapshot.GetByClientJwt() != client {
		t.Fatal("client-only capture synthesized admin")
	}
	if claims, err := snapshot.ParseByJwt(); err == nil || claims != nil {
		t.Fatal("admin parser fell back to provider claims")
	}
}

func TestPairedSnapshotCheckedLocationErrorNeverMeansAbsent(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	if location, err := snapshot.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("genuine paired absence was not retained")
	}
	if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, localConnectLocationFileName), []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not save corrupt location")
	}
	if location, err := snapshot.LoadConnectLocation(); err == nil || location != nil || err.Error() == localAuthSnapshotSupersededMessage {
		t.Fatal("corrupt location was absent or classified as retryable supersession")
	}
	localOnly, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal("could not capture local-only state")
	}
	if location, err := localOnly.LoadDefaultLocation(); err == nil || location != nil {
		t.Fatal("disk-only snapshot authorized a paired read")
	}
	if err := localOnly.SetDefaultLocation(testingStoredLocation("unowned")); err == nil {
		t.Fatal("disk-only snapshot authorized a paired write")
	}
}
