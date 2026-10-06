//go:build !ios_extension

package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestGridConnectionStatus(t *testing.T) {
	cases := []struct {
		name                  string
		minSatisfied          bool
		failed                bool
		activeProviderCount   int
		locationProviderCount int32
		expected              ConnectionStatus
	}{
		{"min satisfied", true, false, 4, 0, Connected},
		{"forming, no location count", false, false, 2, 0, Connecting},
		{"forming, more providers in the location", false, false, 2, 10, Connecting},
		{"every provider of a one provider location active", false, false, 1, 1, Connected},
		{"every provider of a three provider location active", false, false, 3, 3, Connected},
		{"more active than the location count", false, false, 2, 1, Connected},
		{"no active provider", false, false, 0, 1, Connecting},
		{"failed", false, true, 0, 1, ConnectFailed},
		{"satisfied wins over failed", true, true, 1, 0, Connected},
	}
	for _, c := range cases {
		status := gridConnectionStatus(c.minSatisfied, c.failed, c.activeProviderCount, c.locationProviderCount)
		if status != c.expected {
			t.Fatalf("%s: status %s, expected %s", c.name, status, c.expected)
		}
	}
}

// A location with fewer providers than the window minimum reads Connected once
// all of its providers are active, even though the window minimum is not met.
func TestConnectViewControllerLowProviderCountLocationConnected(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	controller := testingPreferenceController(t, device)
	location := testingSpecificPreferenceLocation()
	location.ProviderCount = 2
	controller.Connect(location)

	monitor := controller.testingWindowMonitor.(*testing_gridWindowMonitor)
	monitor.mu.Lock()
	monitor.windowExpandEvent = connect.WindowExpandEvent{TargetSize: 4, MinSatisfied: false}
	monitor.mu.Unlock()

	providerIdA := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		providerIdA: {ClientId: providerIdA, State: connect.ProviderStateAdded},
	})
	if status := controller.GetConnectionStatus(); status != Connecting {
		t.Fatalf("one of two providers active: status %s, expected %s", status, Connecting)
	}

	providerIdB := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		providerIdA: {ClientId: providerIdA, State: connect.ProviderStateAdded},
		providerIdB: {ClientId: providerIdB, State: connect.ProviderStateAdded},
	})
	if status := controller.GetConnectionStatus(); status != Connected {
		t.Fatalf("both providers of the location active: status %s, expected %s", status, Connected)
	}
}
