package sdk

// The provider display order: the plottable providers west to east about
// their spherical centroid, then the ones with no coordinates. Untagged so
// the ios_extension build (the packet tunnel extension's SDK slice, which
// excludes the view controllers) can order the widget snapshot exactly as
// ProviderLocationsViewController orders the app's provider details view.

import (
	"cmp"
	"math"
	"slices"
)

// OrderConnectedProviderLocations returns a new list in display order:
// plottable providers west to east about their centroid, then unplottable
// ones, each provider once. Exported for the packet tunnel extension.
func OrderConnectedProviderLocations(locations *ConnectedProviderLocationList) *ConnectedProviderLocationList {
	order, _, _ := orderConnectedProviderLocations(locations)
	ordered := NewConnectedProviderLocationList()
	for _, location := range order {
		ordered.Add(location)
	}
	return ordered
}

// orderConnectedProviderLocations returns the display order, how many of
// its leading entries are plottable, and the oldest-connected provider's
// client id (the sdk sorts the window oldest-connected first).
func orderConnectedProviderLocations(locations *ConnectedProviderLocationList) ([]*ConnectedProviderLocation, int, string) {
	plottable := []*ConnectedProviderLocation{}
	unplottable := []*ConnectedProviderLocation{}
	lonLats := [][2]float64{}
	longest := ""
	seen := map[string]bool{}
	// both device implementations return an empty list rather than nil, but a
	// nil here would be a crash rather than an empty globe
	if locations != nil {
		for i := 0; i < locations.Len(); i += 1 {
			location := locations.Get(i)
			if location == nil || location.ClientId == nil {
				continue
			}
			clientId := location.ClientId.String()
			if seen[clientId] {
				// the same provider twice would give the selection two homes
				continue
			}
			seen[clientId] = true
			if longest == "" {
				longest = clientId
			}
			if lat, lon, ok := plotCoordinates(location); ok {
				plottable = append(plottable, location)
				lonLats = append(lonLats, [2]float64{lon, lat})
			} else {
				unplottable = append(unplottable, location)
			}
		}
	}

	centroidLon := centroidLongitude(lonLats)
	slices.SortStableFunc(plottable, func(a, b *ConnectedProviderLocation) int {
		_, aLon, _ := plotCoordinates(a)
		_, bLon, _ := plotCoordinates(b)
		return cmp.Compare(
			signedLonDelta(aLon, centroidLon),
			signedLonDelta(bLon, centroidLon),
		)
	})

	order := make([]*ConnectedProviderLocation, 0, len(plottable)+len(unplottable))
	order = append(order, plottable...)
	order = append(order, unplottable...)
	return order, len(plottable), longest
}

// plotCoordinates is the provider's dot position on the globe: the city
// centroid when known, else the region centroid. ok is false when the
// provider has neither — it is listed but never plotted.
func plotCoordinates(location *ConnectedProviderLocation) (lat float64, lon float64, ok bool) {
	if location.HasCityCoordinates {
		return location.CityLat, location.CityLon, true
	}
	if location.HasRegionCoordinates {
		return location.RegionLat, location.RegionLon, true
	}
	return 0, 0, false
}

// centroidLongitude is the longitude, in degrees [-180, 180], of the
// spherical centroid of the given {lon, lat} points: unit vectors summed,
// the horizontal direction of the sum taken. Latitude only weights — a point
// near a pole pulls the centroid's longitude less than one near the equator,
// matching how little its own longitude means there. An empty or perfectly
// balanced set (a zero sum, where no direction is meaningful) resolves to 0
// deterministically (atan2(0, 0) == 0).
func centroidLongitude(lonLats [][2]float64) float64 {
	x := 0.0
	y := 0.0
	for _, lonLat := range lonLats {
		lambda := lonLat[0] * math.Pi / 180
		phi := lonLat[1] * math.Pi / 180
		x += math.Cos(phi) * math.Cos(lambda)
		y += math.Cos(phi) * math.Sin(lambda)
	}
	return math.Atan2(y, x) * 180 / math.Pi
}

// signedLonDelta is the signed east-west offset of `lonDeg` from
// `referenceDeg`, normalized to [-180, 180): negative is west of the
// reference, positive east. Sorting providers by this offset from their
// `centroidLongitude` is the wheel order — west to east as seen from the
// providers' center, with the cut (the wheel's two ends) on the meridian
// opposite the centroid, the farthest place from the data. A point exactly
// opposite the reference lands at -180, the far-west end.
func signedLonDelta(lonDeg float64, referenceDeg float64) float64 {
	delta := math.Mod(lonDeg-referenceDeg, 360)
	if delta < -180 {
		delta += 360
	} else if delta >= 180 {
		delta -= 360
	}
	return delta
}
