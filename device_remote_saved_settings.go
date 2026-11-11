// The settings an app sets through its DeviceRemote that the device does not
// keep: the kill switch (route local), the split rules, the dns resolver
// settings and the ad blocker. The remote keeps them in the app's own local
// state and queues them for every device it syncs with.
//
// The windows service and the linux daemon build a new DeviceLocal for each
// tunnel session, and again after a restart, and neither loads or auto saves
// that device's preferences: since the preference catalog (c46108dd) a device
// does that only when its host calls DeviceLocal.Load and SetAutoSave. So what
// such a device was told over the rpc ended with it. The next session came up
// with no split rules, the default dns settings (no adblock preset or
// personal id, the default servers, Fast DNS on connect off) and the blocker
// off, and on windows with route local on, which is the kill switch off for
// every packet the tunnel cannot carry, while the apps still showed the user's
// settings (support inbox 8227, and 8200 for the kill switch). The ios
// extension keeps its own copy, but in a container the app cannot read, so the
// ios app keeps a copy and queues it for each new remote itself (apple
// BlockActionsStore, DnsSettingsStore and DeviceManager.initDevice), as the
// linux app does for route local and, with its own rules, for the dns
// settings.
//
// The app is the one author of these settings: a device changes them only
// through the rpc setters the app drives. So the remote keeps the app's record
// and replays it, for every app:
//
//   - At construction it queues the saved values for the device, as writes
//     made while the rpc is down: the first sync applies them over whatever the
//     device came up with, and the getters answer with them until it does.
//     Nothing saved queues nothing, and the device's own values stand. The dns
//     settings are queued as SetDnsResolverSettings queues them. A sync whose
//     dns settings are the device's own leaves the device's resolvers alone
//     (DeviceLocalRpc.Sync, sameDnsResolverSettings): applying them again would
//     restart a running device's resolvers on every attach to a tunnel that is
//     up.
//   - It saves a value whenever it learns the whole of it: the device's value,
//     reported or read over the rpc, a whole value the app set, and an edit of
//     a split rule list the remote knew, applied or queued for the next sync. A
//     device's value that arrives while a write of it is queued predates the
//     write, which the next sync applies over it, and is not saved. An edit of
//     a list the remote never saw is not the whole list, and is not saved
//     either: saving it would replace the device's rules with it at the next
//     remote's first sync. Route local is not saved here: every app saves its
//     kill switch to its local state when the user sets it
//     (LocalState.SetRouteLocal).
//
// The saves go to this process's local state only, in the order the remote
// learned the values, off the caller's goroutine (AsyncLocalState), and one
// made after a sign-out is dropped (local_state_sign_in.go), so the next
// account never comes up with the previous one's settings. A failed save logs
// the setting's name and nothing else: the dns settings carry a personal id. A
// remote to a hosted device (DisableHostedIncompatible) neither queues nor
// saves: its settings are the hosted device's, not this app's.
package sdk

import (
	"slices"
	"sync"
)

// Where a value the remote learned came from.
type deviceRemoteSettingSource int

const (
	// the device's value, reported or read over the rpc
	deviceRemoteSettingReported deviceRemoteSettingSource = iota
	// a whole value the app set
	deviceRemoteSettingSet
	// the app's edit of one split rule, applied to the list the remote last
	// knew
	deviceRemoteSettingEdited
)

// The saves of one setting.
type deviceRemoteSavedSetting struct {
	// the remote has the whole value: one it queued at construction, the
	// device's, or one the app set. Guarded by DeviceRemote.stateLock
	known bool
	// the revision of the latest value learned. Guarded by
	// DeviceRemote.stateLock
	learned uint64

	// the revision of the latest value saved, so a save that runs after a
	// newer one never replaces it
	saveLock sync.Mutex
	saved    uint64
}

// The settings the remote saves for the next one.
type deviceRemoteSavedSettings struct {
	blockActionOverrides deviceRemoteSavedSetting
	dnsResolverSettings  deviceRemoteSavedSetting
	blockerEnabled       deviceRemoteSavedSetting
}

// The app's local state, when this remote keeps the app's settings in it.
func (self *DeviceRemote) savedSettingsLocalState() *AsyncLocalState {
	if self.settings == nil || self.settings.DisableHostedIncompatible || self.networkSpace == nil {
		return nil
	}
	return self.networkSpace.GetAsyncLocalState()
}

// Queues the saved settings for the device. The constructor calls this before
// the run loop starts, so it sets the state without stateLock, as the log
// verbosity restore does.
func (self *DeviceRemote) queueSavedSettings() {
	asyncLocalState := self.savedSettingsLocalState()
	if asyncLocalState == nil {
		return
	}
	localState := asyncLocalState.GetLocalState()
	if routeLocal, ok := localState.routeLocalIfSet(); ok {
		self.state.RouteLocal.Set(routeLocal)
		self.lastKnownState.RouteLocal.Set(routeLocal)
	}
	if blockerEnabled, ok := localState.blockerEnabledIfSet(); ok {
		self.state.BlockerEnabled.Set(blockerEnabled)
		self.lastKnownState.BlockerEnabled.Set(blockerEnabled)
		self.savedSettings.blockerEnabled.known = true
	}
	// nil is never saved or unreadable, which leaves the device's rules alone.
	// An empty list is a real value, the last rule removed, and clears them
	if overrides := localState.GetBlockActionOverrides(); overrides != nil {
		overridesRpc := newBlockActionOverridesRpc(overrides)
		self.state.BlockActionOverrides.Set(overridesRpc)
		self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		self.savedSettings.blockActionOverrides.known = true
	}
	// as SetDnsResolverSettings queues them: a selection no device can apply
	// is left out, and lists edited under a selection apply as custom
	dnsResolverSettings := localState.GetDnsResolverSettings()
	if dnsResolverSettings != nil && validateDnsResolverSettingsSelection(dnsResolverSettings) == nil {
		settingsRpc := newDnsResolverSettingsRpc(dnsResolverSettingsAsEdited(dnsResolverSettings))
		self.state.DnsResolverSettings.Set(settingsRpc)
		self.lastKnownState.DnsResolverSettings.Set(settingsRpc)
		self.savedSettings.dnsResolverSettings.known = true
	}
}

// Records that the remote learned a value of `setting`, and returns the local
// state and revision its save takes, or false when the value is not saved (see
// the file header). `pending` says whether a write of the setting is queued
// for the next sync. Must be called with stateLock.
func (self *DeviceRemote) learnSavedSettingWithLock(
	setting *deviceRemoteSavedSetting,
	source deviceRemoteSettingSource,
	pending bool,
) (*AsyncLocalState, uint64, bool) {
	switch source {
	case deviceRemoteSettingReported:
		if pending {
			// made before the device had the queued write
			return nil, 0, false
		}
		setting.known = true
	case deviceRemoteSettingSet:
		setting.known = true
	case deviceRemoteSettingEdited:
		if !setting.known {
			return nil, 0, false
		}
	}
	asyncLocalState := self.savedSettingsLocalState()
	if asyncLocalState == nil {
		return nil, 0, false
	}
	setting.learned += 1
	return asyncLocalState, setting.learned, true
}

// Saves a value the next remote queues, after every save of the setting
// queued before it. `name` is fixed, and is all a failure logs. Never called
// with stateLock.
func (self *DeviceRemote) saveSavedSetting(
	asyncLocalState *AsyncLocalState,
	setting *deviceRemoteSavedSetting,
	name string,
	revision uint64,
	write func(*LocalState) error,
) {
	// the sign-in the value belongs to: a sign-out before the save runs drops it
	signIn := asyncLocalState.GetLocalState().currentSignIn()
	asyncLocalState.serialAsync(func() error {
		setting.saveLock.Lock()
		defer setting.saveLock.Unlock()
		if revision <= setting.saved {
			// a newer value was saved first
			return nil
		}
		saved, err := signIn.write(write)
		if err != nil {
			self.log.Warningf("[dr]save %s failed", name)
			return err
		}
		if saved {
			setting.saved = revision
		}
		return nil
	})
}

// Keeps a split rule list the remote learned as last known, and returns its
// save, to run once stateLock is released, or nil. Must be called with
// stateLock.
func (self *DeviceRemote) learnBlockActionOverridesWithLock(
	overridesRpc []*BlockActionOverrideRpc,
	source deviceRemoteSettingSource,
) func() {
	self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
	setting := &self.savedSettings.blockActionOverrides
	asyncLocalState, revision, save := self.learnSavedSettingWithLock(setting, source, self.state.BlockActionOverrides.IsSet)
	if !save {
		return nil
	}
	// the save's own copy, made now: the rpc values stay with the state
	overrides := toBlockActionOverrideList(overridesRpc)
	return func() {
		self.saveSavedSetting(asyncLocalState, setting, "split rules", revision, func(localState *LocalState) error {
			return localState.saveCatalogPreferenceWithLock("block-action-overrides", overrides)
		})
	}
}

// Keeps dns resolver settings the remote learned as last known, and returns
// their save, to run once stateLock is released, or nil. Must be called with
// stateLock.
func (self *DeviceRemote) learnDnsResolverSettingsWithLock(
	settingsRpc *DnsResolverSettingsRpc,
	source deviceRemoteSettingSource,
) func() {
	self.lastKnownState.DnsResolverSettings.Set(settingsRpc)
	if settingsRpc == nil {
		// a device without a resolver: nothing to save, and nil is never saved
		return nil
	}
	setting := &self.savedSettings.dnsResolverSettings
	asyncLocalState, revision, save := self.learnSavedSettingWithLock(setting, source, self.state.DnsResolverSettings.IsSet)
	if !save {
		return nil
	}
	dnsResolverSettings := settingsRpc.toDnsResolverSettings()
	return func() {
		self.saveSavedSetting(asyncLocalState, setting, "dns settings", revision, func(localState *LocalState) error {
			// as LocalState.SetDnsResolverSettings saves an app's copy
			saved := dnsResolverSettingsAsEdited(dnsResolverSettings)
			if saved.PresetId != "" {
				saved = ownDnsResolverSettings(saved)
			}
			return localState.saveCatalogPreferenceWithLock("dns-resolver-settings", saved)
		})
	}
}

// Returns the save of a blocker setting the remote learned, to run once
// stateLock is released, or nil. The caller keeps the value as last known, as
// it always has. Must be called with stateLock.
func (self *DeviceRemote) learnBlockerEnabledWithLock(
	blockerEnabled bool,
	source deviceRemoteSettingSource,
) func() {
	setting := &self.savedSettings.blockerEnabled
	asyncLocalState, revision, save := self.learnSavedSettingWithLock(setting, source, self.state.BlockerEnabled.IsSet)
	if !save {
		return nil
	}
	return func() {
		self.saveSavedSetting(asyncLocalState, setting, "blocker", revision, func(localState *LocalState) error {
			return localState.saveCatalogPreferenceWithLock("blocker-enabled", blockerEnabled)
		})
	}
}

// Whether two settings are the same in every field, a missing list and an
// empty one alike. A sync leaves queued settings that are the device's own
// alone (DeviceLocalRpc.Sync), so a field this does not read would leave a
// real change unapplied: it reads every field, which
// TestSameDnsResolverSettingsReadsEveryField holds it to.
func sameDnsResolverSettings(a *DnsResolverSettings, b *DnsResolverSettings) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aRpc := newDnsResolverSettingsRpc(a)
	bRpc := newDnsResolverSettingsRpc(b)
	return aRpc.PresetId == bRpc.PresetId &&
		aRpc.PresetPersonalId == bRpc.PresetPersonalId &&
		aRpc.EnableRemoteDoh == bRpc.EnableRemoteDoh &&
		aRpc.EnableLocalDoh == bRpc.EnableLocalDoh &&
		aRpc.EnableRemoteDns == bRpc.EnableRemoteDns &&
		aRpc.EnableLocalDns == bRpc.EnableLocalDns &&
		aRpc.EnableFallback == bRpc.EnableFallback &&
		aRpc.DnsUpgradeMaskAddress == bRpc.DnsUpgradeMaskAddress &&
		slices.Equal(aRpc.RemoteDohUrlsIpv4, bRpc.RemoteDohUrlsIpv4) &&
		slices.Equal(aRpc.RemoteDohUrlsIpv6, bRpc.RemoteDohUrlsIpv6) &&
		slices.Equal(aRpc.LocalDohUrlsIpv4, bRpc.LocalDohUrlsIpv4) &&
		slices.Equal(aRpc.LocalDohUrlsIpv6, bRpc.LocalDohUrlsIpv6) &&
		slices.Equal(aRpc.RemoteDnsIpv4, bRpc.RemoteDnsIpv4) &&
		slices.Equal(aRpc.RemoteDnsIpv6, bRpc.RemoteDnsIpv6) &&
		slices.Equal(aRpc.LocalDnsIpv4, bRpc.LocalDnsIpv4) &&
		slices.Equal(aRpc.LocalDnsIpv6, bRpc.LocalDnsIpv6)
}
