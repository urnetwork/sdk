//go:build !ios_extension

package sdk

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

type FilterLocationsState = string

const (
	LocationsLoading FilterLocationsState = "LOCATIONS_LOADING"
	LocationsLoaded  FilterLocationsState = "LOCATIONS_LOADED"
	LocationsError   FilterLocationsState = "LOCATIONS_ERROR"
)

type FilteredLocations struct {
	BestMatches *ConnectLocationList
	Promoted    *ConnectLocationList
	Countries   *ConnectLocationList
	Cities      *ConnectLocationList
	Regions     *ConnectLocationList
	Devices     *ConnectLocationList
}

// type FilteredLocationsStateListener interface {
// 	Update(state FilterLocationsState)
// }

type FilteredLocationsListener interface {
	FilteredLocationsChanged(locations *FilteredLocations, state FilterLocationsState)
}

type LocationsViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device
	// an api-only controller (NewLocationsViewControllerWithApi) has no device:
	// the browse needs nothing but the network space api, so a host without a
	// device plane (a browser tab before the extension attaches) runs the same
	// grouping and ordering as every app. Exactly one of device / api is set.
	api *Api

	stateLock sync.Mutex

	nextFilterSequenceNumber     int64
	previousFilterSequenceNumber int64

	filteredLocations     *FilteredLocations
	filteredLocationState FilterLocationsState

	filteredLocationListeners *connect.CallbackList[FilteredLocationsListener]
	// filteredLocationsStateListeners *connect.CallbackList[FilteredLocationsStateListener]
}

func newLocationsViewController(ctx context.Context, device Device) *LocationsViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &LocationsViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		nextFilterSequenceNumber:     0,
		previousFilterSequenceNumber: 0,
		filteredLocations:            nil,
		filteredLocationState:        LocationsError,

		filteredLocationListeners: connect.NewCallbackList[FilteredLocationsListener](),
		// filteredLocationsStateListeners: connect.NewCallbackList[FilteredLocationsStateListener](),
	}
	return vc
}

// NewLocationsViewControllerWithApi opens the location browse over an api with
// no device. It is the controller every app's location chooser renders, for
// hosts that have a network member jwt but no device yet; the caller owns it
// and must Close it.
func NewLocationsViewControllerWithApi(ctx context.Context, api *Api) *LocationsViewController {
	vc := newLocationsViewController(ctx, nil)
	vc.api = api
	return vc
}

func (self *LocationsViewController) getApi() *Api {
	if self.api != nil {
		return self.api
	}
	return self.device.GetApi()
}

func (self *LocationsViewController) Start() {
	go connect.HandleError(func() {
		self.FilterLocations("")
	})
}

func (self *LocationsViewController) Stop() {}

func (self *LocationsViewController) Close() {
	if self.device != nil {
		deviceLog(self.device).Info("[lcvc]close")
	}

	self.cancel()
}

// func (self *LocationsViewController) GetLocations() *ConnectLocationList {
// 	return self.locations
// }

func (self *LocationsViewController) GetFilteredLocations() *FilteredLocations {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.filteredLocations
}

func (self *LocationsViewController) GetFilteredLocationState() FilterLocationsState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.filteredLocationState
}

func (self *LocationsViewController) filteredLocationsChanged(locations *FilteredLocations, state FilterLocationsState) {
	for _, listener := range self.filteredLocationListeners.Get() {
		connect.HandleError(func() {
			listener.FilteredLocationsChanged(locations, state)
		})
	}
}

func (self *LocationsViewController) AddFilteredLocationsListener(listener FilteredLocationsListener) Sub {
	callbackId := self.filteredLocationListeners.Add(listener)
	return newSub(func() {
		self.filteredLocationListeners.Remove(callbackId)
	})
}

// func (self *LocationsViewController) filterLocationsStateChanged(state FilterLocationsState) {
// 	for _, listener := range self.filteredLocationsStateListeners.Get() {
// 		connect.HandleError(func() {
// 			listener.Update(state)
// 		})
// 	}
// }

// func (self *LocationsViewController) AddFilteredLocationsStateListener(listener FilteredLocationsStateListener) Sub {
// 	callbackId := self.filteredLocationsStateListeners.Add(listener)
// 	return newSub(func() {
// 		self.filteredLocationsStateListeners.Remove(callbackId)
// 	})
// }

func (self *LocationsViewController) FilterLocations(filter string) {
	// api call, call callback
	filter = strings.TrimSpace(filter)

	// locationsVcLog("FILTER LOCATIONS %s", filter)
	// self.filterLocationsStateChanged(LocationsLoading)

	var filterSequenceNumber int64
	var snapshotLocations *FilteredLocations
	var snapshotState FilterLocationsState
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.nextFilterSequenceNumber += 1
		filterSequenceNumber = self.nextFilterSequenceNumber

		self.filteredLocationState = LocationsLoading
		snapshotLocations = self.filteredLocations
		snapshotState = self.filteredLocationState
	}()

	self.filteredLocationsChanged(snapshotLocations, snapshotState)

	// locationsVcLog("POST FILTER LOCATIONS %s", filter)

	callback := FindLocationsCallback(connect.NewApiCallback[*FindLocationsResult](
		func(result *FindLocationsResult, err error) {
			// locationsVcLog("FIND LOCATIONS RESULT %s %s", result, err)

			update := false
			var notifyLocations *FilteredLocations
			var notifyState FilterLocationsState
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.previousFilterSequenceNumber < filterSequenceNumber {
					self.previousFilterSequenceNumber = filterSequenceNumber
					update = true
					if err == nil {
						self.setFilteredLocationsFromResult(result, filter)
					} else {
						self.filteredLocationState = LocationsError
						self.filteredLocations = nil
					}
					notifyLocations = self.filteredLocations
					notifyState = self.filteredLocationState
				}
			}()
			if update {
				self.filteredLocationsChanged(notifyLocations, notifyState)
			}
		},
	))

	if filter == "" {
		self.getApi().GetProviderLocations(callback)
	} else {
		findLocations := &FindLocationsArgs{
			Query: filter,
		}
		self.getApi().FindProviderLocations(findLocations, callback)
	}
}

// must be called with the state lock
// func (self *LocationsViewController) setFilteredLocationState(state FilterLocationsState) {
// 	self.filteredLocationState = state
// }

func GetFilteredLocationsFromResult(result *FindLocationsResult, filter string) *FilteredLocations {
	var bestMatch []*ConnectLocation
	var promoted []*ConnectLocation
	var countries []*ConnectLocation
	var cities []*ConnectLocation
	var regions []*ConnectLocation
	var devices []*ConnectLocation

	for i := 0; i < result.Groups.Len(); i += 1 {
		groupResult := result.Groups.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				LocationGroupId: groupResult.LocationGroupId,
			},
			Name:          groupResult.Name,
			ProviderCount: int32(groupResult.ProviderCount),
			Promoted:      groupResult.Promoted,
			MatchDistance: int32(groupResult.MatchDistance),
		}

		if groupResult.MatchDistance == 0 && filter != "" {
			bestMatch = append(bestMatch, location)
		} else if groupResult.Promoted {
			promoted = append(promoted, location)
		}
	}

	for i := 0; i < result.Locations.Len(); i += 1 {
		locationResult := result.Locations.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				LocationId: locationResult.LocationId,
			},
			LocationType:      locationResult.LocationType,
			Name:              locationResult.Name,
			City:              locationResult.City,
			Region:            locationResult.Region,
			Country:           locationResult.Country,
			CountryCode:       locationResult.CountryCode,
			CityLocationId:    locationResult.CityLocationId,
			RegionLocationId:  locationResult.RegionLocationId,
			CountryLocationId: locationResult.CountryLocationId,
			ProviderCount:     int32(locationResult.ProviderCount),
			MatchDistance:     int32(locationResult.MatchDistance),
			Stable:            locationResult.Stable,
			StrongPrivacy:     locationResult.StrongPrivacy,
		}

		if location.MatchDistance == 0 && filter != "" {
			bestMatch = append(bestMatch, location)
		} else {

			if location.LocationType == LocationTypeCountry {
				countries = append(countries, location)
			}

			// only show cities when searching
			if location.LocationType == LocationTypeCity && filter != "" {
				cities = append(cities, location)
			}

			// only show regions when searching
			if location.LocationType == LocationTypeRegion && filter != "" {
				regions = append(regions, location)
			}

		}

	}

	for i := 0; i < result.Devices.Len(); i += 1 {
		locationDeviceResult := result.Devices.Get(i)

		location := &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{
				ClientId: locationDeviceResult.ClientId,
			},
			Name: locationDeviceResult.DeviceName,
		}
		devices = append(devices, location)
	}

	slices.SortStableFunc(bestMatch, cmpConnectLocations)
	slices.SortStableFunc(promoted, cmpConnectLocations)
	slices.SortStableFunc(countries, cmpConnectLocations)
	slices.SortStableFunc(cities, cmpConnectLocations)
	slices.SortStableFunc(regions, cmpConnectLocations)

	exportedBestMatches := NewConnectLocationList()
	exportedBestMatches.addAll(bestMatch...)

	exportedPromoted := NewConnectLocationList()
	exportedPromoted.addAll(promoted...)

	exportedCountries := NewConnectLocationList()
	exportedCountries.addAll(countries...)

	exportedCities := NewConnectLocationList()
	exportedCities.addAll(cities...)

	exportedRegions := NewConnectLocationList()
	exportedRegions.addAll(regions...)

	exportedDevices := NewConnectLocationList()
	exportedDevices.addAll(devices...)

	filteredLocations := &FilteredLocations{
		BestMatches: exportedBestMatches,
		Promoted:    exportedPromoted,
		Countries:   exportedCountries,
		Cities:      exportedCities,
		Regions:     exportedRegions,
		Devices:     exportedDevices,
	}

	return filteredLocations
}

// must be called with the state lock
func (self *LocationsViewController) setFilteredLocationsFromResult(result *FindLocationsResult, filter string) {
	filteredLocations := GetFilteredLocationsFromResult(result, filter)

	self.filteredLocations = filteredLocations
	self.filteredLocationState = LocationsLoaded

}

func cmpConnectLocations(a *ConnectLocation, b *ConnectLocation) int {
	// sort locations
	// - provider count descending
	// - name

	if a == b {
		return 0
	}

	if (a.MatchDistance <= 1) != (b.MatchDistance <= 1) {
		if a.MatchDistance <= 1 {
			return -1
		} else {
			return 1
		}
	}

	// provider count descending
	if a.ProviderCount != b.ProviderCount {
		if a.ProviderCount < b.ProviderCount {
			return 1
		} else {
			return -1
		}
	}

	if a.Name != b.Name {
		if a.Name < b.Name {
			return -1
		} else {
			return 1
		}
	}

	return a.ConnectLocationId.Cmp(b.ConnectLocationId)

}

/**
 * Code is usually the country code which maps to a color hex.
 * If the location is not a country, we just need a unique string that represents the location
 * ie locationId.toString()
 */
func (vc *LocationsViewController) GetColorHex(code string) string {
	return GetColorHex(code)
}
