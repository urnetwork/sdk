package sdk

import "testing"

// Read one effective configuration: rebuilding the multi client may make the
// next getter nil, so two reads cannot establish one predicate's authority.
func testingReliabilitySettingsMatch(getSettings func() *ReliabilitySettings, heartbeatMillis int64, affinityMillis int64) bool {
	settings := getSettings()
	return settings != nil && settings.HeartbeatIntervalMillis == heartbeatMillis &&
		settings.AffinityMaxTimeMillis == affinityMillis
}

// A returned owned snapshot stays valid when the next getter is unavailable.
// Advance the getter on every call to make a second read fail deterministically,
// without a scheduling race, worker or wall-clock wait.
func TestReliabilityWaitPredicateUsesOneSettingsSnapshot(t *testing.T) {
	for _, affinity := range []int64{-1, 0, 45_000, 60_000} {
		settings := []*ReliabilitySettings{
			{HeartbeatIntervalMillis: 123456, AffinityMaxTimeMillis: affinity},
			nil,
			{HeartbeatIntervalMillis: 1, AffinityMaxTimeMillis: affinity},
			{HeartbeatIntervalMillis: 123456, AffinityMaxTimeMillis: affinity + 1},
			{HeartbeatIntervalMillis: 123456, AffinityMaxTimeMillis: affinity},
		}
		want := []bool{true, false, false, false, true}
		calls := 0
		getSettings := func() *ReliabilitySettings {
			if calls >= len(settings) {
				t.Fatal("predicate read beyond the bounded transition fixture")
			}
			current := settings[calls]
			calls++
			return current
		}
		for index, expected := range want {
			if got := testingReliabilitySettingsMatch(getSettings, 123456, affinity); got != expected {
				t.Fatalf("affinity %d transition %d: matched=%t, want %t", affinity, index, got, expected)
			}
			if calls != index+1 {
				t.Fatalf("affinity %d transition %d: %d getter calls, want %d", affinity, index, calls, index+1)
			}
		}
	}
}
