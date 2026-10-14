package sdk

import "github.com/urnetwork/connect"

// Requires preferenceMutationLock, not stateLock. The same preference owner
// serializes client/provider setters, Load and destination construction through
// cap and carrier-policy publication. Only the owned snapshot needs stateLock;
// external graph/budget queries run after releasing it. Immutable capacities,
// never current free bytes, still account for retiring generations/Auto leases.
func (self *DeviceLocal) publicH3WindowSizeLimitWithLock() int {
	self.stateLock.Lock()
	settings := self.settings
	transportSettings := cloneTransportSettings(self.transportSettings)
	providerTransportSettings := cloneTransportSettings(self.providerTransportSettings)
	owner := self.platformTransportBudget
	networkSpace := self.networkSpace
	clientStrategy := self.clientStrategy
	provider := self.provider
	beforeComposition := self.testingBeforeH3WindowComposition
	self.stateLock.Unlock()
	if beforeComposition != nil {
		beforeComposition()
	}
	// Settings, strategies and provider family URLs are immutable after their
	// owner is constructed. Concurrent retirement cannot turn Used into free.
	if !mobileMemoryPolicyEnabledForPlatform(settings.MemoryTargetByteCount,
		settings.LowMemoryProfile) {
		return 0
	}
	mode, _ := toConnectTransportPolicy(transportSettings, false)
	switch mode {
	case connect.TransportModeH3, connect.TransportModeH3Dns, connect.TransportModeH3DnsPump:
	default:
		return 0
	}
	transport := newDeviceLocalPlatformTransportSettingsForProfile(
		settings.MemoryTargetByteCount, owner, nil,
		networkSpace.GetAltUrl(), settings.DnsPumpHost, true)
	claim := transport.H3MemoryReservationByteCount(mode, clientStrategy)
	if claim <= 0 {
		return 0
	}
	budget := owner.Stats()
	controlBytes := connect.ByteCount(0)
	controlSlots := 0
	if provider != nil {
		// Providing none folds transfer capacity, not the provider's control
		// carriers. Preserve its standby and every configured family path.
		controlSlots = 1
		if provider.platformUrlV4 != "" {
			controlSlots++
		}
		if provider.platformUrlV6 != "" {
			controlSlots++
		}
		providerMode, preferences := toConnectTransportPolicy(providerTransportSettings, true)
		providerTransport := newDeviceLocalPlatformTransportSettingsForProfile(
			settings.MemoryTargetByteCount, owner,
			settings.ProviderDialContextSettings, networkSpace.GetAltUrl(),
			settings.DnsPumpHost, true)
		providerTransport.ModePreferences = preferences
		if providerMode == connect.TransportModeH1 ||
			(providerMode == connect.TransportModeAuto && preferences[connect.TransportModeH1] > 0) {
			// Auto H3 is revocable; its required H1 control paths are not.
			controlBytes = connect.ByteCount(controlSlots) * providerTransport.H1BudgetByteCount
		} else {
			// Pinned family paths do not use an extender. The standby may,
			// and therefore retains its own complete configured graph.
			controlBytes = connect.ByteCount(controlSlots-1) *
				providerTransport.H3MemoryReservationByteCount(providerMode, nil)
			controlBytes += providerTransport.H3MemoryReservationByteCount(
				providerMode, provider.clientStrategy)
		}
	}
	limit := int(max(connect.ByteCount(0), budget.TotalByteCount-controlBytes) / claim)
	if budget.MaxTransportCount > 0 {
		limit = min(limit, max(0, budget.MaxTransportCount-controlSlots))
	}
	// No complete graph fitting is an admission failure, never permission to
	// publish a zero-provider minimum. The smallest positive target stays
	// unready until a real charged carrier and provider can be established.
	return max(1, limit)
}
