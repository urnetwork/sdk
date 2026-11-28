//go:build !ios_extension

package sdk

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// Synthetic places: a country, a region in it, a city in that region, and
// another country.
type testingExitPlaces struct {
	country      *ConnectLocation
	region       *ConnectLocation
	city         *ConnectLocation
	otherCountry *ConnectLocation
}

func testingNewExitPlaces() *testingExitPlaces {
	countryId := NewId()
	regionId := NewId()
	cityId := NewId()
	otherCountryId := NewId()
	return &testingExitPlaces{
		country: &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{LocationId: countryId},
			Name:              "Country A",
			LocationType:      LocationTypeCountry,
			CountryLocationId: countryId,
			ProviderCount:     40,
		},
		region: &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{LocationId: regionId},
			Name:              "Region B",
			LocationType:      LocationTypeRegion,
			RegionLocationId:  regionId,
			CountryLocationId: countryId,
			ProviderCount:     7,
		},
		city: &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{LocationId: cityId},
			Name:              "City C",
			LocationType:      LocationTypeCity,
			CityLocationId:    cityId,
			RegionLocationId:  regionId,
			CountryLocationId: countryId,
		},
		otherCountry: &ConnectLocation{
			ConnectLocationId: &ConnectLocationId{LocationId: otherCountryId},
			Name:              "Country D",
			LocationType:      LocationTypeCountry,
			CountryLocationId: otherCountryId,
		},
	}
}

func testingIdList(ids ...*Id) *IdList {
	list := NewIdList()
	for _, id := range ids {
		list.Add(id)
	}
	return list
}

func testingConnectIdSet(ids ...*Id) map[connect.Id]bool {
	if len(ids) == 0 {
		return nil
	}
	set := map[connect.Id]bool{}
	for _, id := range ids {
		set[id.toConnectId()] = true
	}
	return set
}

// The exclusions apply to best available, a collection and a pick that
// contains the excluded place; a pick that is the place or lies in it wins,
// and so does a pinned exit.
func TestDestinationExcludedLocationIdsKeepExplicitPicks(t *testing.T) {
	places := testingNewExitPlaces()
	excluded := testingIdList(places.region.ConnectLocationId.LocationId, places.otherCountry.ConnectLocationId.LocationId)
	regionId := places.region.ConnectLocationId.LocationId
	otherCountryId := places.otherCountry.ConnectLocationId.LocationId

	with := func(location *ConnectLocation) *ConnectLocation {
		stamped := cloneConnectLocation(location)
		stamped.ExcludeLocationIds = excluded
		return stamped
	}
	cases := []struct {
		name     string
		location *ConnectLocation
		expected map[connect.Id]bool
	}{
		{
			name:     "best available",
			location: with(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}),
			expected: testingConnectIdSet(regionId, otherCountryId),
		},
		{
			name:     "a collection",
			location: with(&ConnectLocation{ConnectLocationId: &ConnectLocationId{LocationGroupId: NewId()}}),
			expected: testingConnectIdSet(regionId, otherCountryId),
		},
		{
			name:     "the country that contains the excluded region",
			location: with(places.country),
			expected: testingConnectIdSet(regionId, otherCountryId),
		},
		{
			name:     "the excluded region itself",
			location: with(places.region),
			expected: testingConnectIdSet(otherCountryId),
		},
		{
			name:     "a city in the excluded region",
			location: with(places.city),
			expected: testingConnectIdSet(otherCountryId),
		},
		{
			name:     "the excluded country itself",
			location: with(places.otherCountry),
			expected: testingConnectIdSet(regionId),
		},
		{
			name:     "a pinned exit",
			location: with(&ConnectLocation{ConnectLocationId: &ConnectLocationId{ClientId: NewId()}}),
			expected: nil,
		},
		{
			name:     "no exclusions",
			location: places.country,
			expected: nil,
		},
	}
	for _, c := range cases {
		if got := destinationExcludedLocationIds(c.location); !maps.Equal(got, c.expected) {
			t.Errorf("%s: excluded %v, expected %v", c.name, got, c.expected)
		}
	}
}

// A discovery answering fixed providers and recording each request.
type testingExitDiscovery struct {
	stateLock sync.Mutex
	providers []*connect.FindProvidersProvider
	err       error
	requests  []connect.FindProviders2Args
}

func (self *testingExitDiscovery) FindProviders2(
	ctx context.Context,
	byJwt string,
	findProviders2 *connect.FindProviders2Args,
) (*connect.FindProviders2Result, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, *findProviders2)
	if self.err != nil {
		return nil, self.err
	}
	return &connect.FindProviders2Result{Providers: slices.Clone(self.providers)}, nil
}

func testingExitProvider(clientId connect.Id, location *connect.ProviderLocation) *connect.FindProvidersProvider {
	return &connect.FindProvidersProvider{ClientId: clientId, Location: location}
}

func testingProviderClientIds(providers []*connect.FindProvidersProvider) []connect.Id {
	clientIds := []connect.Id{}
	for _, provider := range providers {
		clientIds = append(clientIds, provider.ClientId)
	}
	return clientIds
}

// Providers in an excluded country, region or city are left out of the
// answer and out of later requests; providers elsewhere, with no location, or
// named by the request stay.
func TestExcludeLocationProviderDiscoveryLeavesOutExcludedProviders(t *testing.T) {
	excludedRegionId := connect.NewId()
	excludedCityId := connect.NewId()
	countryId := connect.NewId()
	otherRegionId := connect.NewId()

	inRegion := connect.NewId()
	inCity := connect.NewId()
	elsewhere := connect.NewId()
	unknown := connect.NewId()
	pinned := connect.NewId()
	inner := &testingExitDiscovery{
		providers: []*connect.FindProvidersProvider{
			testingExitProvider(inRegion, &connect.ProviderLocation{CountryLocationId: &countryId, RegionLocationId: &excludedRegionId}),
			testingExitProvider(inCity, &connect.ProviderLocation{CountryLocationId: &countryId, RegionLocationId: &otherRegionId, CityLocationId: &excludedCityId}),
			testingExitProvider(elsewhere, &connect.ProviderLocation{CountryLocationId: &countryId, RegionLocationId: &otherRegionId}),
			testingExitProvider(unknown, nil),
			testingExitProvider(pinned, &connect.ProviderLocation{CountryLocationId: &countryId, RegionLocationId: &excludedRegionId}),
		},
	}
	discovery := newExcludeLocationProviderDiscovery(
		inner,
		map[connect.Id]bool{excludedRegionId: true, excludedCityId: true},
		defaultExcludeLocationDiscoverySettings(),
	)

	callerExcluded := []connect.Id{connect.NewId()}
	args := &connect.FindProviders2Args{
		Specs:            []*connect.ProviderSpec{{BestAvailable: true}, {ClientId: &pinned}},
		Count:            8,
		ExcludeClientIds: callerExcluded,
	}
	result, err := discovery.FindProviders2(context.Background(), "", args)
	if err != nil {
		t.Fatal(err)
	}
	if got, expected := testingProviderClientIds(result.Providers), []connect.Id{elsewhere, unknown, pinned}; !slices.Equal(got, expected) {
		t.Fatalf("answered %v, expected %v", got, expected)
	}

	// the next request leaves out the excluded providers already answered,
	// and the caller's own exclusions are its own
	if _, err := discovery.FindProviders2(context.Background(), "", args); err != nil {
		t.Fatal(err)
	}
	if got, expected := inner.requests[1].ExcludeClientIds, []connect.Id{callerExcluded[0], inRegion, inCity}; !slices.Equal(got, expected) {
		t.Fatalf("the next request excluded %v, expected %v", got, expected)
	}
	if !slices.Equal(args.ExcludeClientIds, callerExcluded) {
		t.Fatalf("the caller's request changed to %v", args.ExcludeClientIds)
	}
	if !slices.Equal(inner.requests[0].ExcludeClientIds, callerExcluded) {
		t.Fatalf("the first request excluded %v, expected only the caller's", inner.requests[0].ExcludeClientIds)
	}
}

// The remembered providers are bounded, the newest kept.
func TestExcludeLocationProviderDiscoveryRemembersABoundedSet(t *testing.T) {
	excludedRegionId := connect.NewId()
	providers := []*connect.FindProvidersProvider{}
	clientIds := []connect.Id{}
	for range 5 {
		clientId := connect.NewId()
		clientIds = append(clientIds, clientId)
		providers = append(providers, testingExitProvider(clientId, &connect.ProviderLocation{RegionLocationId: &excludedRegionId}))
	}
	inner := &testingExitDiscovery{providers: providers}
	settings := defaultExcludeLocationDiscoverySettings()
	settings.RememberedClientIdMaxCount = 3
	discovery := newExcludeLocationProviderDiscovery(inner, map[connect.Id]bool{excludedRegionId: true}, settings)

	args := &connect.FindProviders2Args{Specs: []*connect.ProviderSpec{{BestAvailable: true}}}
	for range 2 {
		result, err := discovery.FindProviders2(context.Background(), "", args)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Providers) != 0 {
			t.Fatalf("answered %v, expected none", testingProviderClientIds(result.Providers))
		}
	}
	if got, expected := inner.requests[1].ExcludeClientIds, clientIds[2:]; !slices.Equal(got, expected) {
		t.Fatalf("remembered %v, expected the newest %v", got, expected)
	}
}

// A failed discovery is passed on as it is.
func TestExcludeLocationProviderDiscoveryPassesOnFailures(t *testing.T) {
	failure := errors.New("unreachable")
	discovery := newExcludeLocationProviderDiscovery(
		&testingExitDiscovery{err: failure},
		map[connect.Id]bool{connect.NewId(): true},
		defaultExcludeLocationDiscoverySettings(),
	)
	if _, err := discovery.FindProviders2(context.Background(), "", &connect.FindProviders2Args{}); !errors.Is(err, failure) {
		t.Fatalf("error %v, expected %v", err, failure)
	}
}

// Without exclusions a destination keeps its discovery (nil is the api's);
// with exclusions the filter goes over it, or over the api's.
func TestDestinationProviderDiscoveryWrapsOnlyWithExclusions(t *testing.T) {
	places := testingNewExitPlaces()
	inner := &testingExitDiscovery{}
	if discovery := destinationProviderDiscovery(places.country, nil, nil, "https://api.example"); discovery != nil {
		t.Fatalf("no exclusions: discovery %T, expected the api's (nil)", discovery)
	}
	if discovery := destinationProviderDiscovery(places.country, inner, nil, "https://api.example"); discovery != inner {
		t.Fatalf("no exclusions: discovery %T, expected the device's", discovery)
	}

	excluding := cloneConnectLocation(places.country)
	excluding.ExcludeLocationIds = testingIdList(places.region.ConnectLocationId.LocationId)
	wrapped, ok := destinationProviderDiscovery(excluding, inner, nil, "https://api.example").(*excludeLocationProviderDiscovery)
	if !ok || wrapped.inner != inner {
		t.Fatal("exclusions: the filter must wrap the device's discovery")
	}
	overApi, ok := destinationProviderDiscovery(excluding, nil, nil, "https://api.example").(*excludeLocationProviderDiscovery)
	if !ok {
		t.Fatal("exclusions: the filter must wrap the api's discovery")
	}
	if api, ok := overApi.inner.(*apiProviderDiscovery); !ok || api.apiUrl != "https://api.example" {
		t.Fatalf("exclusions over the api: inner %T", overApi.inner)
	}
}

// A change of the exclusions is a change of the installed transport, and
// cloning keeps them apart.
func TestConnectLocationExclusionsAreTransportState(t *testing.T) {
	places := testingNewExitPlaces()
	a := cloneConnectLocation(places.country)
	a.ExcludeLocationIds = testingIdList(places.region.ConnectLocationId.LocationId)
	b := cloneConnectLocation(a)
	if !connectLocationTransportEqual(a, b) || !connectLocationValuesEqual(a, b) {
		t.Fatal("a clone must equal its location")
	}
	b.ExcludeLocationIds.Add(places.otherCountry.ConnectLocationId.LocationId)
	if a.ExcludeLocationIds.Len() != 1 {
		t.Fatal("the clone shares its exclusions with the location")
	}
	if connectLocationTransportEqual(a, b) || connectLocationValuesEqual(a, b) {
		t.Fatal("other exclusions must be another transport")
	}
	// nil and empty exclude the same nothing
	c := cloneConnectLocation(places.country)
	d := cloneConnectLocation(places.country)
	d.ExcludeLocationIds = NewIdList()
	if !connectLocationTransportEqual(c, d) {
		t.Fatal("nil and empty exclusions must be the same transport")
	}
}

// The exclusions cross the device rpc with the location, gob and all.
func TestDeviceRemoteConnectLocationCarriesExclusions(t *testing.T) {
	places := testingNewExitPlaces()
	location := cloneConnectLocation(places.country)
	location.ExcludeLocationIds = testingIdList(places.region.ConnectLocationId.LocationId, places.city.ConnectLocationId.LocationId)

	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(newDeviceRemoteConnectLocation(location)); err != nil {
		t.Fatal(err)
	}
	var decoded DeviceRemoteConnectLocation
	if err := gob.NewDecoder(&buffer).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if received := decoded.toConnectLocation(); !connectLocationTransportEqual(received, location) {
		t.Fatalf("received exclusions %v, expected %v", received.ExcludeLocationIds, location.ExcludeLocationIds)
	}
}

func testingExitLocalState(t *testing.T) *LocalState {
	t.Helper()
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	return localState
}

func testingLocationNames(locations *ConnectLocationList) []string {
	names := []string{}
	for _, location := range locations.getAll() {
		names = append(names, location.Name)
	}
	return names
}

// Excluded exit locations are countries, regions and cities, kept newest
// first, once each, and removable; they survive a restart and go with a
// sign-out.
func TestExcludedExitLocationsAreKeptInTheLocalState(t *testing.T) {
	home := t.TempDir()
	places := testingNewExitPlaces()
	localState := newLocalState(context.Background(), home)

	for _, location := range []*ConnectLocation{places.region, places.otherCountry, places.region} {
		if err := localState.AddExcludedExitLocation(location); err != nil {
			t.Fatal(err)
		}
	}
	if got, expected := testingLocationNames(localState.GetExcludedExitLocations()), []string{"Region B", "Country D"}; !slices.Equal(got, expected) {
		t.Fatalf("excluded %v, expected %v", got, expected)
	}
	for _, location := range []*ConnectLocation{
		{ConnectLocationId: &ConnectLocationId{BestAvailable: true}},
		{ConnectLocationId: &ConnectLocationId{LocationGroupId: NewId()}},
		{ConnectLocationId: &ConnectLocationId{ClientId: NewId()}},
		nil,
	} {
		if err := localState.AddExcludedExitLocation(location); err == nil {
			t.Fatalf("%v could be excluded", location)
		}
	}
	if !localState.IsExcludedExitLocation(places.region) || localState.IsExcludedExitLocation(places.country) {
		t.Fatal("IsExcludedExitLocation disagrees with the list")
	}
	if got := localState.excludedExitLocationIds(); !idListsEqual(got, testingIdList(places.region.ConnectLocationId.LocationId, places.otherCountry.ConnectLocationId.LocationId)) {
		t.Fatalf("the connect carries %v", got)
	}
	localState.Close()

	// a restart reads them back
	restarted := newLocalState(context.Background(), home)
	defer restarted.Close()
	if err := restarted.RemoveExcludedExitLocation(places.otherCountry); err != nil {
		t.Fatal(err)
	}
	if got, expected := testingLocationNames(restarted.GetExcludedExitLocations()), []string{"Region B"}; !slices.Equal(got, expected) {
		t.Fatalf("after a restart and a removal: %v, expected %v", got, expected)
	}

	// the stored record keeps what describes the place, not live counts
	stored := restarted.GetExcludedExitLocations().Get(0)
	if stored.ProviderCount != 0 || stored.RegionLocationId.Cmp(places.region.ConnectLocationId.LocationId) != 0 {
		t.Fatalf("stored %+v", stored)
	}

	if err := restarted.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(restarted.localStorageDir, localExcludedExitLocationsFileName)); !os.IsNotExist(err) {
		t.Fatalf("the excluded exit locations outlived the sign-out: %v", err)
	}
}

// Favorites are places, collections and exits, the newest first; adding one
// again moves it to the front; best available cannot be one; an exit is a
// member's device.
func TestFavoriteLocationsAreKeptInTheLocalState(t *testing.T) {
	places := testingNewExitPlaces()
	localState := testingExitLocalState(t)
	collection := &ConnectLocation{ConnectLocationId: &ConnectLocationId{LocationGroupId: NewId()}, Name: "Collection E"}
	exit := &ConnectLocation{ConnectLocationId: &ConnectLocationId{ClientId: NewId()}, Name: "Exit F", ExtenderIp: "192.0.2.7"}

	for _, location := range []*ConnectLocation{places.city, collection, exit, places.city} {
		if err := localState.AddFavoriteLocation(location); err != nil {
			t.Fatal(err)
		}
	}
	if got, expected := testingLocationNames(localState.GetFavoriteLocations()), []string{"City C", "Exit F", "Collection E"}; !slices.Equal(got, expected) {
		t.Fatalf("favorites %v, expected %v", got, expected)
	}
	if err := localState.AddFavoriteLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}); err == nil {
		t.Fatal("best available became a favorite")
	}
	favoriteExit := localState.GetFavoriteLocations().Get(1)
	if !IsFavoriteExit(favoriteExit) || IsFavoriteExit(places.city) {
		t.Fatal("IsFavoriteExit must tell an exit from a place")
	}
	if favoriteExit.ExtenderIp != "192.0.2.7" {
		t.Fatal("a favorite exit must keep its extender pin")
	}
	if err := localState.RemoveFavoriteLocation(exit); err != nil {
		t.Fatal(err)
	}
	if localState.IsFavoriteLocation(exit) || !localState.IsFavoriteLocation(collection) {
		t.Fatal("IsFavoriteLocation disagrees with the list")
	}
	// removing what is not there changes nothing
	if err := localState.RemoveFavoriteLocation(places.otherCountry); err != nil {
		t.Fatal(err)
	}
	if got := localState.GetFavoriteLocations().Len(); got != 2 {
		t.Fatalf("%d favorites, expected 2", got)
	}
}

// The lists are bounded.
func TestFavoriteLocationsAreBounded(t *testing.T) {
	localState := testingExitLocalState(t)
	maxCount := defaultExitLocationPreferencesSettings().FavoriteMaxCount
	for i := 0; i < maxCount; i += 1 {
		if err := localState.AddFavoriteLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{LocationId: NewId()}}); err != nil {
			t.Fatalf("favorite %d: %v", i, err)
		}
	}
	if err := localState.AddFavoriteLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{LocationId: NewId()}}); err == nil {
		t.Fatal("a favorite past the bound was kept")
	}
	// one already kept still moves to the front
	first := localState.GetFavoriteLocations().Get(maxCount - 1)
	if err := localState.AddFavoriteLocation(first); err != nil {
		t.Fatal(err)
	}
}

// A fake live device that records the locations the connect controller
// installs.
type testingExitDevice struct {
	stateLock  sync.Mutex
	reconnects []*ConnectLocation
	current    *ConnectLocation
}

func (self *testingExitDevice) GetProvideControlMode(_ RpcNoArg, provideControlMode *ProvideControlMode) error {
	*provideControlMode = ProvideControlModeManual
	return nil
}

func (self *testingExitDevice) Reconnect(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reconnects = append(self.reconnects, location.toConnectLocation())
	self.current = location.toConnectLocation()
	return nil
}

func (self *testingExitDevice) SetDefaultLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	return nil
}

func (self *testingExitDevice) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.current = location.toConnectLocation()
	return nil
}

func (self *testingExitDevice) GetConnectLocation(_ RpcNoArg, location **DeviceRemoteConnectLocation) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	*location = newDeviceRemoteConnectLocation(self.current)
	return nil
}

func (self *testingExitDevice) getReconnects() []*ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reconnects)
}

// A connect carries the excluded exit locations as they stand, through the
// device rpc; a change of them reconnects a connection it affects, and not
// one it does not.
func TestConnectCarriesTheExcludedExitLocations(t *testing.T) {
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	dialer := &testingBlockedDeviceRpcDialer{
		entered: make(chan struct{}, 1024),
		release: make(chan struct{}),
	}
	deviceRemote, err := newDeviceRemoteWithOverrides(
		space.networkSpace,
		space.localState.GetByClientJwt(),
		space.localState.GetInstanceId(),
		settings,
		connect.NewId(),
		dialer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	controller := newConnectViewController(context.Background(), deviceRemote)
	controller.testingWindowMonitor = newTestingGridWindowMonitor()
	t.Cleanup(controller.Close)
	device := &testingExitDevice{}
	testingInstallFakeDeviceService(t, deviceRemote, device)

	places := testingNewExitPlaces()
	if err := space.localState.AddExcludedExitLocation(places.region); err != nil {
		t.Fatal(err)
	}

	controller.Connect(places.country)
	reconnects := device.getReconnects()
	if len(reconnects) != 1 {
		t.Fatalf("%d reconnects, expected 1", len(reconnects))
	}
	if got, expected := reconnects[0].ExcludeLocationIds, testingIdList(places.region.ConnectLocationId.LocationId); !idListsEqual(got, expected) {
		t.Fatalf("the connect carried %v, expected %v", got, expected)
	}

	// a change that affects the connection reconnects it with the new list
	if err := space.localState.AddExcludedExitLocation(places.otherCountry); err != nil {
		t.Fatal(err)
	}
	controller.ExcludedExitLocationsChanged()
	reconnects = device.getReconnects()
	if len(reconnects) != 2 {
		t.Fatalf("%d reconnects after the change, expected 2", len(reconnects))
	}
	expected := testingIdList(places.otherCountry.ConnectLocationId.LocationId, places.region.ConnectLocationId.LocationId)
	if got := reconnects[1].ExcludeLocationIds; !idListsEqual(got, expected) {
		t.Fatalf("the reconnect carried %v, expected %v", got, expected)
	}

	// a pick inside the changed place is not affected by it
	controller.Connect(places.city)
	if err := space.localState.RemoveExcludedExitLocation(places.region); err != nil {
		t.Fatal(err)
	}
	controller.ExcludedExitLocationsChanged()
	if got := len(device.getReconnects()); got != 3 {
		t.Fatalf("%d reconnects, expected 3: the change does not affect a pick inside the place", got)
	}
}
