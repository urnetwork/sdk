//go:build !ios_extension

package sdk

// The user's exit location preferences, kept in the network space's local
// state, so a sign-out clears them with the rest of the account's state:
//
//   - Excluded exit locations: countries, regions (such as US states) and
//     cities a connection's picks avoid. Every connect carries the list as it
//     stands (ConnectViewController.Connect), and the device leaves the
//     providers located there out of the window (exit_location_exclusion.go).
//
//   - Favorite locations: places, collections and exits the user wants to
//     return to. A favorite exit is one member's device (a location with a
//     client id): it can go offline or leave the network at any time, so the
//     apps say so beside it (IsFavoriteExit), and connecting to it waits for
//     it while it is away, as a stayed-on exit does.
//
// The apps show the lists and call these to change them; after changing the
// excluded list they call ConnectViewController.ExcludedExitLocationsChanged,
// which applies the change to a connection in place.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

const (
	localExcludedExitLocationsFileName = ".excluded_exit_locations"
	localFavoriteLocationsFileName     = ".favorite_locations"
)

// exitLocationPreferencesSettings bounds the stored lists.
type exitLocationPreferencesSettings struct {
	// The most excluded exit locations kept; adding one past it fails. 64
	// covers every US state with room to spare. Non-positive allows none.
	ExcludedMaxCount int
	// The most favorites kept; adding one past it fails. 64 is more than a
	// location list shows comfortably. Non-positive allows none.
	FavoriteMaxCount int
}

func defaultExitLocationPreferencesSettings() *exitLocationPreferencesSettings {
	return &exitLocationPreferencesSettings{
		ExcludedMaxCount: 64,
		FavoriteMaxCount: 64,
	}
}

// CanExcludeExitLocation reports whether a location can be excluded: a
// country, region or city. A collection, a device, an exit and best available
// cannot.
func CanExcludeExitLocation(location *ConnectLocation) bool {
	return location != nil &&
		location.ConnectLocationId != nil &&
		location.ConnectLocationId.LocationId != nil &&
		location.ConnectLocationId.ClientId == nil &&
		location.ConnectLocationId.LocationGroupId == nil &&
		!location.ConnectLocationId.BestAvailable
}

// CanFavoriteLocation reports whether a location can be a favorite: a place,
// a collection or an exit (a device). Best available cannot.
func CanFavoriteLocation(location *ConnectLocation) bool {
	if location == nil || location.ConnectLocationId == nil {
		return false
	}
	id := location.ConnectLocationId
	return !id.BestAvailable && (id.ClientId != nil || id.LocationId != nil || id.LocationGroupId != nil)
}

// IsFavoriteExit reports whether a favorite is one member's device rather
// than a place: it can go offline or leave the network at any time, and the
// apps say so beside it.
func IsFavoriteExit(location *ConnectLocation) bool {
	return location != nil && location.ConnectLocationId != nil && location.ConnectLocationId.ClientId != nil
}

// GetExcludedExitLocations returns the excluded exit locations, the newest
// first. An unreadable record reads as none.
func (self *LocalState) GetExcludedExitLocations() *ConnectLocationList {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return exportConnectLocations(self.loadLocationListWithLock(localExcludedExitLocationsFileName))
}

// AddExcludedExitLocation excludes a country, region or city from every
// connection's picks from the next connect on (CanExcludeExitLocation). A
// location already excluded stays where it is.
func (self *LocalState) AddExcludedExitLocation(location *ConnectLocation) error {
	if !CanExcludeExitLocation(location) {
		return errors.New("only a country, region or city can be excluded")
	}
	return self.addToLocationList(localExcludedExitLocationsFileName, location, defaultExitLocationPreferencesSettings().ExcludedMaxCount)
}

// RemoveExcludedExitLocation stops excluding a location. A location that is
// not excluded changes nothing.
func (self *LocalState) RemoveExcludedExitLocation(location *ConnectLocation) error {
	return self.removeFromLocationList(localExcludedExitLocationsFileName, location)
}

// IsExcludedExitLocation reports whether the location is excluded.
func (self *LocalState) IsExcludedExitLocation(location *ConnectLocation) bool {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return locationListIndex(self.loadLocationListWithLock(localExcludedExitLocationsFileName), location) != -1
}

// The excluded exit locations' ids, as a connect carries them; empty for none.
func (self *LocalState) excludedExitLocationIds() *IdList {
	ids := NewIdList()
	for _, location := range self.GetExcludedExitLocations().getAll() {
		ids.Add(cloneId(location.ConnectLocationId.LocationId))
	}
	return ids
}

// GetFavoriteLocations returns the favorites, the newest first. An unreadable
// record reads as none.
func (self *LocalState) GetFavoriteLocations() *ConnectLocationList {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return exportConnectLocations(self.loadLocationListWithLock(localFavoriteLocationsFileName))
}

// AddFavoriteLocation keeps a place, collection or exit to return to
// (CanFavoriteLocation). A favorite already kept moves to the front.
func (self *LocalState) AddFavoriteLocation(location *ConnectLocation) error {
	if !CanFavoriteLocation(location) {
		return errors.New("best available cannot be a favorite")
	}
	return self.addToLocationList(localFavoriteLocationsFileName, location, defaultExitLocationPreferencesSettings().FavoriteMaxCount)
}

// RemoveFavoriteLocation forgets a favorite. A location that is not a
// favorite changes nothing.
func (self *LocalState) RemoveFavoriteLocation(location *ConnectLocation) error {
	return self.removeFromLocationList(localFavoriteLocationsFileName, location)
}

// IsFavoriteLocation reports whether the location is a favorite.
func (self *LocalState) IsFavoriteLocation(location *ConnectLocation) bool {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return locationListIndex(self.loadLocationListWithLock(localFavoriteLocationsFileName), location) != -1
}

// The stored list; a missing, unreadable or malformed record reads as empty.
// The caller holds authStateLock.
func (self *LocalState) loadLocationListWithLock(name string) []*ConnectLocation {
	data, err := os.ReadFile(filepath.Join(self.localStorageDir, name))
	if err != nil {
		return nil
	}
	var locations []*ConnectLocation
	if err := json.Unmarshal(data, &locations); err != nil {
		return nil
	}
	return slices.DeleteFunc(locations, func(location *ConnectLocation) bool {
		return location == nil || location.ConnectLocationId == nil
	})
}

// Puts the location at the front of the stored list, once. Dropped while the
// space is signed out, as the other account preferences are.
func (self *LocalState) addToLocationList(name string, location *ConnectLocation, maxCount int) error {
	return self.writeSignedIn(func() error {
		locations := self.loadLocationListWithLock(name)
		if index := locationListIndex(locations, location); index != -1 {
			locations = slices.Delete(locations, index, index+1)
		} else if maxCount <= len(locations) {
			return errors.New("too many locations")
		}
		locations = slices.Insert(locations, 0, storedLocation(location))
		return self.writeLocationListWithLock(name, locations)
	})
}

func (self *LocalState) removeFromLocationList(name string, location *ConnectLocation) error {
	return self.writeSignedIn(func() error {
		locations := self.loadLocationListWithLock(name)
		index := locationListIndex(locations, location)
		if index == -1 {
			return nil
		}
		return self.writeLocationListWithLock(name, slices.Delete(locations, index, index+1))
	})
}

// The caller holds authStateLock. An empty list removes the record.
func (self *LocalState) writeLocationListWithLock(name string, locations []*ConnectLocation) error {
	if len(locations) == 0 {
		return self.writePreferenceBytesWithLock(name, nil)
	}
	data, err := json.Marshal(locations)
	if err != nil {
		return err
	}
	return self.writePreferenceBytesWithLock(name, data)
}

// The location as a list keeps it: what identifies and describes it, without
// the connection's exclusions or the list's live counts.
func storedLocation(location *ConnectLocation) *ConnectLocation {
	stored := cloneConnectLocation(location)
	stored.ExcludeLocationIds = nil
	stored.ProviderCount = 0
	stored.MatchDistance = 0
	return stored
}

// The list's index of the location by its id, -1 when it is not in the list.
func locationListIndex(locations []*ConnectLocation, location *ConnectLocation) int {
	if location == nil || location.ConnectLocationId == nil {
		return -1
	}
	return slices.IndexFunc(locations, func(stored *ConnectLocation) bool {
		return stored.ConnectLocationId.Cmp(location.ConnectLocationId) == 0
	})
}

func exportConnectLocations(locations []*ConnectLocation) *ConnectLocationList {
	exported := NewConnectLocationList()
	exported.addAll(locations...)
	return exported
}
