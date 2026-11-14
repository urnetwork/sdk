// Reopen after a real constructor-installed renewal, with no active UI or
// native repair callback. The newly accepted device consumes checked storage.
package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestSameOwnerRenewalColdReopenRestoresIntendedDestinationWithoutUi(t *testing.T) {
	home := t.TempDir()
	fixture := testingPairedAuthSpaceAt(t, home)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	if err := fixture.localState.SetConnectLocation(testingStoredLocation("cold-intended-destination")); err != nil {
		t.Fatal("could not persist destination")
	}
	if err := fixture.localState.SetDefaultLocation(testingStoredLocation("cold-default")); err != nil {
		t.Fatal("could not persist default")
	}
	renewed := testingRefreshableJwtWithMarker(t, "cold-renewal")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, renewed) {
		t.Fatal("actual same-owner renewal did not commit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fixture.localDevice.CloseAndWait(ctx); err != nil {
		t.Fatal("old device owner did not join")
	}
	fixture.networkSpace.close()
	fresh := testingPairedAuthSpaceAt(t, home)
	before := testingPairedAuthSnapshot(t, fresh)
	if before.GetByJwt() != fixture.adminJwt || before.GetByClientJwt() != renewed || before.GetInstanceId().Cmp(fixture.instanceId) != 0 {
		t.Fatal("cold process lost separate auth roles or stable instance")
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} }
	device, err := newDeviceLocalWithOverrides(fresh.networkSpace, renewed, "cold-restore", "test", "0",
		before.GetInstanceId(), settings, connect.NewId())
	if err != nil {
		t.Fatal("cold authenticated device could not be constructed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("cold device did not join")
		}
	})
	accepted := testingPairedAuthSnapshot(t, fresh)
	location, err := accepted.LoadConnectLocation()
	if err != nil || location == nil || location.Name != "cold-intended-destination" {
		t.Fatal("cold checked restore lost intended destination")
	}
	defaultLocation, err := accepted.LoadDefaultLocation()
	if err != nil || defaultLocation == nil || defaultLocation.Name != "cold-default" {
		t.Fatal("cold checked restore lost default")
	}
	device.SetUpgradeMuxSettings(nil)
	device.SetConnectLocation(location)
	device.SetDefaultLocation(defaultLocation)
	if !device.GetConnectEnabled() || !connectLocationValuesEqual(device.GetConnectLocation(), location) || !connectLocationValuesEqual(device.GetDefaultLocation(), defaultLocation) {
		t.Fatal("cold restore did not build the actual intended consumer")
	}
}
