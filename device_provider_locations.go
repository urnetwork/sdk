package sdk

import (
	"bytes"
	"cmp"
	"math"
	"slices"

	"github.com/urnetwork/connect"
)

// ConnectedProviderLocation is one currently connected (routing-eligible)
// window provider with its location and connected-since time.
//
// The connected-since time is stamped by the device hosting the window when
// the provider becomes routing-eligible. It survives network changes, resets
// on a destination change or same-provider reconnect, and providers rotate
// out at the window's max client lifetime (~60 min), so durations top out
// around there. A viewer on another device computes duration against its own
// clock, so cross-device clock skew shifts displayed durations.
type ConnectedProviderLocation struct {
	// note gomobile does not support struct composition

	// ClientId is the egress provider's client id (the destination tail) —
	// the id that identifies the provider to the user.
	ClientId    *Id
	Country     string
	CountryCode string
	Region      string
	City        string
	// region centroid
	RegionLat float64
	RegionLon float64
	// city centroid
	CityLat float64
	CityLon float64
	// HasLocation is false when the provider's location is unknown: the
	// user's own fixed peers, restored window identities, and older servers.
	HasLocation          bool
	HasRegionCoordinates bool
	HasCityCoordinates   bool
	// ConnectedSinceMillis is the unix millis when the provider became
	// routing-eligible in the window. 0 when unknown (an older device peer).
	// The ui derives the connected duration from this and ticks it locally.
	ConnectedSinceMillis int64
}

type ConnectedProviderLocationList struct {
	exportedList[*ConnectedProviderLocation]
}

func NewConnectedProviderLocationList() *ConnectedProviderLocationList {
	return &ConnectedProviderLocationList{
		exportedList: *newExportedList[*ConnectedProviderLocation](),
	}
}

// fired whenever the connected provider set or its locations may have
// changed. Consumers re-read `Device.GetConnectedProviderLocations`.
type ConnectedProviderLocationChangeListener interface {
	ConnectedProviderLocationsChanged()
}

// deriveConnectedProviderLocations maps the monitor's retained events — the
// currently tracked window providers — to the exported list: active
// (routing-eligible) providers only, sorted oldest-connected first, unknown
// connected-since last, egress client id as the tiebreak.
func deriveConnectedProviderLocations(providerEvents map[connect.Id]*connect.ProviderEvent) *ConnectedProviderLocationList {
	providerId := func(event *connect.ProviderEvent) connect.Id {
		if event.EgressClientId != (connect.Id{}) {
			return event.EgressClientId
		}
		// events from an older device peer lack the egress id
		return event.ClientId
	}
	connectedSinceMillis := func(event *connect.ProviderEvent) int64 {
		if event.EventTime.IsZero() {
			return 0
		}
		return event.EventTime.UnixMilli()
	}

	active := []*connect.ProviderEvent{}
	for _, providerEvent := range providerEvents {
		if providerEvent != nil && providerEvent.State.IsActive() {
			active = append(active, providerEvent)
		}
	}
	slices.SortFunc(active, func(a, b *connect.ProviderEvent) int {
		aSince := connectedSinceMillis(a)
		bSince := connectedSinceMillis(b)
		if aSince == 0 {
			aSince = math.MaxInt64
		}
		if bSince == 0 {
			bSince = math.MaxInt64
		}
		if c := cmp.Compare(aSince, bSince); c != 0 {
			return c
		}
		aId := providerId(a)
		bId := providerId(b)
		return bytes.Compare(aId[:], bId[:])
	})

	locations := NewConnectedProviderLocationList()
	for _, providerEvent := range active {
		location := &ConnectedProviderLocation{
			ClientId:             newId(providerId(providerEvent)),
			ConnectedSinceMillis: connectedSinceMillis(providerEvent),
		}
		if l := providerEvent.Location; l != nil {
			location.HasLocation = true
			location.Country = l.Country
			location.CountryCode = l.CountryCode
			location.Region = l.Region
			location.City = l.City
			if c := l.RegionCoordinates; c != nil {
				location.HasRegionCoordinates = true
				location.RegionLat = c.Lat
				location.RegionLon = c.Lon
			}
			if c := l.CityCoordinates; c != nil {
				location.HasCityCoordinates = true
				location.CityLat = c.Lat
				location.CityLon = c.Lon
			}
		}
		locations.Add(location)
	}
	return locations
}
