package sdk

// Excluded exit locations: places, such as a US state, that a user does not
// want a connection's traffic to exit from. The user keeps the list in the
// network space's local state (exit_location_preferences.go), and every connect
// carries it on the location (ConnectLocation.ExcludeLocationIds), so the
// device that makes the connection applies the list as it stood then, whether
// it runs in the app's process or behind the device rpc.
//
// A device applies the list to the providers its discovery answers: a
// provider located in an excluded country, region or city is left out of the
// window and asked to be left out of later answers. The list applies to best
// available, to collections and to a pick that contains an excluded place (the
// United States without Utah). A pick that is an excluded place, or lies
// inside one, is the user's explicit choice and wins, and so does a pinned
// exit. A provider whose location discovery does not say (an older server) is
// kept: exclusion is only as good as the provider's geolocation.

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

// excludeLocationDiscoverySettings holds the tunables of the discovery filter.
type excludeLocationDiscoverySettings struct {
	// RememberedClientIdMaxCount bounds the excluded providers one destination
	// remembers and asks discovery to leave out of its later answers, so the
	// same providers are not offered on every expansion pass. 256 covers many
	// passes of a large window at a few kilobytes per request. Non-positive
	// remembers none.
	RememberedClientIdMaxCount int
}

func defaultExcludeLocationDiscoverySettings() *excludeLocationDiscoverySettings {
	return &excludeLocationDiscoverySettings{
		RememberedClientIdMaxCount: 256,
	}
}

// The excluded location ids that apply to a destination, or nil for none: the
// location's list less the pick itself and the places it lies in. A pinned
// exit (a client id) applies none.
func destinationExcludedLocationIds(location *ConnectLocation) map[connect.Id]bool {
	if location == nil || location.ConnectLocationId == nil || location.ExcludeLocationIds == nil {
		return nil
	}
	if location.ConnectLocationId.ClientId != nil {
		return nil
	}
	picked := map[connect.Id]bool{}
	if location.ConnectLocationId.LocationId != nil {
		picked[location.ConnectLocationId.LocationId.toConnectId()] = true
		for _, id := range []*Id{location.CityLocationId, location.RegionLocationId, location.CountryLocationId} {
			if id != nil {
				picked[id.toConnectId()] = true
			}
		}
	}
	excludedLocationIds := map[connect.Id]bool{}
	for _, id := range location.ExcludeLocationIds.getAll() {
		if id == nil {
			continue
		}
		if locationId := id.toConnectId(); !picked[locationId] {
			excludedLocationIds[locationId] = true
		}
	}
	if len(excludedLocationIds) == 0 {
		return nil
	}
	return excludedLocationIds
}

// Whether discovery located the provider in one of the places. A provider
// with no location is not.
func providerLocatedIn(provider *connect.FindProvidersProvider, locationIds map[connect.Id]bool) bool {
	if provider == nil || provider.Location == nil {
		return false
	}
	for _, id := range []*connect.Id{
		provider.Location.CityLocationId,
		provider.Location.RegionLocationId,
		provider.Location.CountryLocationId,
	} {
		if id != nil && locationIds[*id] {
			return true
		}
	}
	return false
}

// A discovery that leaves out the providers in excluded locations, over the
// discovery the destination would use otherwise. One per destination: what it
// remembers is that destination's. Safe for concurrent use.
type excludeLocationProviderDiscovery struct {
	inner               connect.NetworkProviderDiscovery
	excludedLocationIds map[connect.Id]bool
	settings            *excludeLocationDiscoverySettings

	stateLock sync.Mutex
	// the excluded providers answered so far, oldest first, without repeats
	rememberedClientIds []connect.Id
}

func newExcludeLocationProviderDiscovery(
	inner connect.NetworkProviderDiscovery,
	excludedLocationIds map[connect.Id]bool,
	settings *excludeLocationDiscoverySettings,
) *excludeLocationProviderDiscovery {
	return &excludeLocationProviderDiscovery{
		inner:               inner,
		excludedLocationIds: excludedLocationIds,
		settings:            settings,
	}
}

// Implements connect.NetworkProviderDiscovery. The request goes out as its own
// copy, with the excluded providers already answered added to its exclusions;
// the answer comes back without the providers in excluded locations. A
// provider the request names by client id is never left out.
func (self *excludeLocationProviderDiscovery) FindProviders2(
	ctx context.Context,
	byJwt string,
	findProviders2 *connect.FindProviders2Args,
) (*connect.FindProviders2Result, error) {
	request := *findProviders2
	pinnedClientIds := map[connect.Id]bool{}
	for _, spec := range findProviders2.Specs {
		if spec != nil && spec.ClientId != nil {
			pinnedClientIds[*spec.ClientId] = true
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if 0 < len(self.rememberedClientIds) {
			request.ExcludeClientIds = append(slices.Clone(findProviders2.ExcludeClientIds), self.rememberedClientIds...)
		}
	}()

	result, err := self.inner.FindProviders2(ctx, byJwt, &request)
	if err != nil || result == nil {
		return result, err
	}

	filtered := *result
	filtered.Providers = make([]*connect.FindProvidersProvider, 0, len(result.Providers))
	excludedClientIds := []connect.Id{}
	for _, provider := range result.Providers {
		if provider != nil && !pinnedClientIds[provider.ClientId] && providerLocatedIn(provider, self.excludedLocationIds) {
			excludedClientIds = append(excludedClientIds, provider.ClientId)
			continue
		}
		filtered.Providers = append(filtered.Providers, provider)
	}
	if 0 < len(excludedClientIds) {
		self.remember(excludedClientIds)
	}
	return &filtered, nil
}

// Keeps the newest excluded providers up to the bound.
func (self *excludeLocationProviderDiscovery) remember(clientIds []connect.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	maxCount := self.settings.RememberedClientIdMaxCount
	if maxCount <= 0 {
		return
	}
	for _, clientId := range clientIds {
		if slices.Contains(self.rememberedClientIds, clientId) {
			continue
		}
		self.rememberedClientIds = append(self.rememberedClientIds, clientId)
	}
	if maxCount < len(self.rememberedClientIds) {
		self.rememberedClientIds = slices.Clone(self.rememberedClientIds[len(self.rememberedClientIds)-maxCount:])
	}
}

// The api's discovery, as the multi client generator calls it when it has no
// discovery of its own: find-providers2 over the device's client strategy.
type apiProviderDiscovery struct {
	clientStrategy *connect.ClientStrategy
	apiUrl         string
}

// Implements connect.NetworkProviderDiscovery.
func (self *apiProviderDiscovery) FindProviders2(
	ctx context.Context,
	byJwt string,
	findProviders2 *connect.FindProviders2Args,
) (*connect.FindProviders2Result, error) {
	return connect.HttpPostWithStrategy(
		ctx,
		self.clientStrategy,
		fmt.Sprintf("%s/network/find-providers2", self.apiUrl),
		findProviders2,
		byJwt,
		&connect.FindProviders2Result{},
		connect.NewNoopApiCallback[*connect.FindProviders2Result](),
	)
}

// The discovery a destination's generator uses: `inner` (nil for the api's)
// behind the exclusion filter when the destination excludes any location,
// else `inner` as it is.
func destinationProviderDiscovery(
	location *ConnectLocation,
	inner connect.NetworkProviderDiscovery,
	clientStrategy *connect.ClientStrategy,
	apiUrl string,
) connect.NetworkProviderDiscovery {
	excludedLocationIds := destinationExcludedLocationIds(location)
	if len(excludedLocationIds) == 0 {
		return inner
	}
	if inner == nil {
		inner = &apiProviderDiscovery{
			clientStrategy: clientStrategy,
			apiUrl:         apiUrl,
		}
	}
	return newExcludeLocationProviderDiscovery(inner, excludedLocationIds, defaultExcludeLocationDiscoverySettings())
}

// Two exclusion lists hold the same ids in the same order.
func idListsEqual(a *IdList, b *IdList) bool {
	var aIds []*Id
	if a != nil {
		aIds = a.getAll()
	}
	var bIds []*Id
	if b != nil {
		bIds = b.getAll()
	}
	return slices.EqualFunc(aIds, bIds, idsEqual)
}

func cloneIdList(ids *IdList) *IdList {
	if ids == nil {
		return nil
	}
	cloned := NewIdList()
	for _, id := range ids.getAll() {
		cloned.Add(cloneId(id))
	}
	return cloned
}
