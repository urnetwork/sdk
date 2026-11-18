//go:build !ios_extension

package sdk

import (
	"testing"
)

func TestFilteredLocationsRegionGroups(t *testing.T) {
	regionA := NewId()
	regionB := NewId()
	regionMissing := NewId()
	result := &FindLocationsResult{
		Groups:  NewLocationGroupResultList(),
		Devices: NewLocationDeviceResultList(),
		Locations: newTestLocationResultList(
			&LocationResult{LocationId: regionA, LocationType: LocationTypeRegion, Name: "Alpha", ProviderCount: 10, MatchDistance: 2},
			&LocationResult{LocationId: regionB, LocationType: LocationTypeRegion, Name: "Beta", ProviderCount: 30, MatchDistance: 2},
			&LocationResult{LocationId: NewId(), LocationType: LocationTypeCity, Name: "A1", RegionLocationId: regionA, ProviderCount: 5, MatchDistance: 2},
			&LocationResult{LocationId: NewId(), LocationType: LocationTypeCity, Name: "B1", RegionLocationId: regionB, ProviderCount: 7, MatchDistance: 2},
			// no region id: matched by name
			&LocationResult{LocationId: NewId(), LocationType: LocationTypeCity, Name: "B2", Region: "Beta", ProviderCount: 3, MatchDistance: 2},
			// region not in the result
			&LocationResult{LocationId: NewId(), LocationType: LocationTypeCity, Name: "X1", RegionLocationId: regionMissing, ProviderCount: 1, MatchDistance: 2},
		),
	}

	// not searching: no groups, like Regions and Cities
	if n := GetFilteredLocationsFromResult(result, "").RegionGroups.Len(); n != 0 {
		t.Fatalf("groups without a filter: %d", n)
	}

	groups := GetFilteredLocationsFromResult(result, "zz").RegionGroups
	if groups.Len() != 3 {
		t.Fatalf("groups: %d, want 3", groups.Len())
	}
	names := func(list *ConnectLocationList) []string {
		var out []string
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, list.Get(i).Name)
		}
		return out
	}
	// regions in Regions order (provider count descending), cities sorted within
	if g := groups.Get(0); g.Region.Name != "Beta" || !equalStrings(names(g.Cities), []string{"B1", "B2"}) {
		t.Errorf("group 0: %v %v", g.Region.Name, names(g.Cities))
	}
	if g := groups.Get(1); g.Region.Name != "Alpha" || !equalStrings(names(g.Cities), []string{"A1"}) {
		t.Errorf("group 1: %v %v", g.Region.Name, names(g.Cities))
	}
	if g := groups.Get(2); g.Region != nil || !equalStrings(names(g.Cities), []string{"X1"}) {
		t.Errorf("group 2 (other): %v %v", g.Region, names(g.Cities))
	}
}

func equalStrings(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newTestLocationResultList(locations ...*LocationResult) *LocationResultList {
	list := NewLocationResultList()
	list.addAll(locations...)
	return list
}
