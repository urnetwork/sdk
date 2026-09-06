// Explicit local preference persistence is separate from authentication,
// device construction, and transport teardown. It never owns native VPN intent.
package sdk

import (
	"errors"

	"github.com/urnetwork/connect"
)

const localPreferencesUnsupportedMessage = "device preferences require an owned local store"
const localPreferencesClosedMessage = "device preference owner is closed"

// A completed checked load can carry an optional default-selection failure.
// That failure is not absence and must not authorize a later default choice.
// Required failures return nil plus error before any routing adoption.
type DeviceLocalLoadResult struct {
	hasConnectLocation bool
	hasDefaultLocation bool
	defaultError       string
	preferencePresent  map[string]bool
	preferenceErrors   map[string]string
}

// A nonnil result represents completed required preference adoption.
func (self *DeviceLocalLoadResult) GetLoaded() bool { return self != nil }

// Reports the saved current destination, not upstream provider readiness.
func (self *DeviceLocalLoadResult) GetHasConnectLocation() bool { return self.hasConnectLocation }

// False with a nonempty default error means unobserved, not absent.
func (self *DeviceLocalLoadResult) GetHasDefaultLocation() bool { return self.hasDefaultLocation }

// A fixed stage only; no path, stored value, or credential is exposed.
func (self *DeviceLocalLoadResult) GetDefaultError() string { return self.defaultError }

// One immutable local mutation result. Saved means the durable file commit
// completed, not that a provider is available. Off-mode mutations have both
// autoSaveEnabled and saved false. Sequence orders this device's operations.
type DeviceLocalSaveResult struct {
	sequence        int64
	preference      string
	autoSaveEnabled bool
	saved           bool
	errorMessage    string
}

// Returns the local operation sequence, not a persistent generation.
func (self *DeviceLocalSaveResult) GetSequence() int64 { return self.sequence }

// Returns only the fixed catalog name.
func (self *DeviceLocalSaveResult) GetPreference() string { return self.preference }

// Distinguishes disabled persistence from a failed enabled save.
func (self *DeviceLocalSaveResult) GetAutoSaveEnabled() bool { return self.autoSaveEnabled }

// Reports the actual file commit, including an equal-value explicit mutation.
func (self *DeviceLocalSaveResult) GetSaved() bool { return self.saved }

// Empty means no error; nonempty values are fixed stages, never raw I/O text.
func (self *DeviceLocalSaveResult) GetError() string { return self.errorMessage }

// Observes completed mutations outside all SDK state/storage locks. A listener
// may request Close or make another preference change without self-deadlock.
type LocalStateSaveListener interface {
	LocalStateSaved(result *DeviceLocalSaveResult)
}

// Subscribes without an implicit replay. The getter supplies the current level.
func (self *DeviceLocal) AddLocalStateSaveListener(listener LocalStateSaveListener) Sub {
	self.preferenceMutationLock.Lock()
	if self.localStateSaveListeners == nil {
		self.localStateSaveListeners = connect.NewCallbackList[LocalStateSaveListener]()
	}
	listeners := self.localStateSaveListeners
	callbackId := listeners.Add(listener)
	self.preferenceMutationLock.Unlock()
	return newSub(func() { listeners.Remove(callbackId) })
}

// The result is immutable; callers cannot change later diagnostics.
func (self *DeviceLocal) GetLastLocalStateSaveResult() *DeviceLocalSaveResult {
	self.preferenceMutationLock.Lock()
	defer self.preferenceMutationLock.Unlock()
	return self.lastLocalStateSave
}

// Enabling neither loads nor saves a snapshot. Call before exposing RPC or
// user input. Disabling is always safe, including during owner retirement.
func (self *DeviceLocal) SetAutoSave(enabled bool) error {
	self.preferenceMutationLock.Lock()
	defer self.preferenceMutationLock.Unlock()
	if enabled {
		if err := self.withOwnedPreferenceStore(func(*LocalState) error { return nil }); err != nil {
			return err
		}
	}
	self.autoSave = enabled
	return nil
}

// False is the constructor default; Load does not change it.
func (self *DeviceLocal) GetAutoSave() bool {
	self.preferenceMutationLock.Lock()
	defer self.preferenceMutationLock.Unlock()
	return self.autoSave
}

// Checks the accepted device's existing owner pair without replacing auth.
// Credential renewal may change bytes while retaining this owner; an API
// refresh ahead of its storage callback is not a settled observation.
// The preference lock, when needed, is acquired before this storage boundary.
// The accepted read/write linearizes here. A later owner transition may retire
// this device; native owners must still fence their own publication. No auth
// lock spans transport construction, external work, or notifications.
func (self *DeviceLocal) withOwnedPreferenceStore(operation func(*LocalState) error) error {
	if self.settings == nil || self.settings.HostedIncompatible || self.ownsApi ||
		self.networkSpace == nil || self.api == nil || self.api != self.networkSpace.api ||
		self.networkSpace.asyncLocalState == nil || self.authPublication == nil {
		return errors.New(localPreferencesUnsupportedMessage)
	}
	localState := self.networkSpace.asyncLocalState.localState
	self.api.authMutationLock.Lock()
	defer self.api.authMutationLock.Unlock()
	localState.authStateLock.Lock()
	defer localState.authStateLock.Unlock()
	if self.ctx.Err() != nil || self.networkSpace.ctx.Err() != nil ||
		self.api.ctx.Err() != nil || localState.ctx.Err() != nil {
		return errors.New(localPreferencesClosedMessage)
	}
	apiState := func() localAuthApiSnapshot {
		self.api.mutex.Lock()
		defer self.api.mutex.Unlock()
		return self.api.authStateSnapshotWithLock()
	}()
	if apiState.owner != self.authPublication || localState.deviceAuthOwner != self.authPublication ||
		apiState.byJwt == "" || apiState.rejectedByJwt != "" {
		return errors.New(localAuthSnapshotSupersededMessage)
	}
	authState, err := localState.loadAuthStateLocked()
	if err != nil {
		return localStorageStageError("read preference auth owner", err)
	}
	if authState.ByClientJwt != apiState.byJwt || authState.InstanceId != self.instanceId.String() {
		return errors.New(localAuthSnapshotSupersededMessage)
	}
	return operation(localState)
}

// Reads without rewriting and applies to an already accepted local device.
// Native explicit-disconnect intent must be admitted before calling Load.
// Missing current means no restored consumer; the default never substitutes
// for current intent. Load does not enable autosave or autosave its replay.
// Required observation failures precede adoption. A later lifecycle failure
// can leave partial in-memory application; it never rewrites stored values.
func (self *DeviceLocal) Load() (*DeviceLocalLoadResult, error) {
	var notifications []func()
	result, err := func() (*DeviceLocalLoadResult, error) {
		self.preferenceMutationLock.Lock()
		defer self.preferenceMutationLock.Unlock()
		if self.authPublication != nil {
			release := self.authPublication.Begin()
			if release == nil {
				return nil, errors.New(localPreferencesClosedMessage)
			}
			defer release()
		}
		var location, defaultLocation *ConnectLocation
		var defaultErr error
		result := &DeviceLocalLoadResult{
			preferencePresent: map[string]bool{},
			preferenceErrors:  map[string]string{},
		}
		var preferences []localPreferenceObservation
		err := self.withOwnedPreferenceStore(func(localState *LocalState) error {
			var err error
			location, err = localState.loadLocationWithLock(localConnectLocationFileName)
			if err != nil {
				return localStorageStageError("load connect location", err)
			}
			defaultLocation, defaultErr = localState.loadLocationWithLock(localDefaultLocationFileName)
			preferences, err = localState.loadPreferenceCatalogWithLock(result)
			return err
		})
		if err != nil {
			return nil, err
		}
		if self.testingBeforePreferenceApply != nil {
			self.testingBeforePreferenceApply()
		}
		if err := self.withOwnedPreferenceStore(func(*LocalState) error { return nil }); err != nil {
			return nil, err
		}
		result.hasConnectLocation = location != nil
		result.hasDefaultLocation = defaultLocation != nil
		provideControlMode := self.GetProvideControlMode()
		for _, preference := range preferences {
			if preference.name == "provide-control-mode" {
				provideControlMode = preference.value.(string)
			}
		}
		for _, preference := range preferences {
			// Non-manual control owns its derived mode. Replaying stale raw
			// Public first could briefly enable providing under saved Never.
			if preference.name == "provide-mode" && provideControlMode != ProvideControlModeManual {
				continue
			}
			var notify func()
			var err error
			if preference.name == "control-ip-family-policy" || preference.name == "log-verbosity" {
				notify, err = self.applyOwnedGlobalPreferenceWithLock(preference.name, preference.value.(int))
			} else {
				notify, err = self.applyLocalCatalogPreferenceWithLock(preference.name, preference.value)
			}
			if err != nil {
				if err.Error() == localAuthSnapshotSupersededMessage || err.Error() == localPreferencesClosedMessage {
					return nil, err
				}
				return nil, localStorageStageError("apply "+preference.name, err)
			}
			if notify != nil {
				notifications = append(notifications, notify)
			}
		}
		if defaultErr == nil {
			notify, err := self.applyDefaultLocation(defaultLocation)
			if err != nil {
				return nil, err
			}
			notifications = append(notifications, notify)
		} else {
			result.defaultError = "load default location"
		}
		notify, err := self.applyDestination(location, locationProviderSpecs(location), false)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, notify)
		return result, nil
	}()
	self.logLocalPreferenceLoad(result, err)
	for _, notify := range notifications {
		notify()
	}
	return result, err
}

// Preserves Set's no-rebuild behavior while returning its actual save error.
func (self *DeviceLocal) SetConnectLocationChecked(location *ConnectLocation) error {
	return self.setConnectLocationChecked(location, false)
}

// A deliberate reconnect still rebuilds, but only after an enabled save
// succeeds. Native recovery must independently establish that it is needed.
func (self *DeviceLocal) ReconnectChecked(location *ConnectLocation) error {
	return self.setConnectLocationChecked(location, true)
}

// Validates the destination before its identity is dereferenced.
func (self *DeviceLocal) setConnectLocationChecked(location *ConnectLocation, rebuild bool) error {
	location = cloneConnectLocation(location)
	return self.setDestinationChecked(location, locationProviderSpecs(location), rebuild)
}

// Sync defers this operation's notification until its service lock is released.
func (self *DeviceLocal) setConnectLocationCheckedDeferred(location *ConnectLocation, rebuild bool) (func(), error) {
	location = cloneConnectLocation(location)
	return self.setDestinationCheckedDeferred(location, locationProviderSpecs(location), rebuild)
}

// The existing location record stores one selector, never a list of custom
// provider specs. Autosave may not claim it can restore a different selector.
func locationProviderSpecs(location *ConnectLocation) *ProviderSpecList {
	if location == nil || location.ConnectLocationId == nil {
		return nil
	}
	specs := NewProviderSpecList()
	specs.Add(&ProviderSpec{
		LocationId: location.ConnectLocationId.LocationId, LocationGroupId: location.ConnectLocationId.LocationGroupId,
		ClientId: location.ConnectLocationId.ClientId, BestAvailable: location.ConnectLocationId.BestAvailable,
	})
	return specs
}

// All public destination producers, including RPC SetDestination, pass here.
// Close uses its internal transport teardown and never calls this mutation.
func (self *DeviceLocal) setDestinationChecked(location *ConnectLocation, specs *ProviderSpecList, rebuild bool) error {
	notify, err := self.setDestinationCheckedDeferred(location, specs, rebuild)
	if notify != nil {
		notify()
	}
	return err
}

// Notification ownership is transferred to the caller, including on error.
func (self *DeviceLocal) setDestinationCheckedDeferred(location *ConnectLocation, specs *ProviderSpecList, rebuild bool) (func(), error) {
	location = cloneConnectLocation(location)
	return self.mutateLocalPreferenceDeferred("connect-location", func(localState *LocalState) error {
		if location != nil && location.ConnectLocationId == nil {
			return errors.New("invalid connect location")
		}
		toSpecs := func(list *ProviderSpecList) []*connect.ProviderSpec {
			values := []*connect.ProviderSpec{}
			if list != nil {
				for i := 0; i < list.Len(); i += 1 {
					if spec := list.Get(i); spec != nil {
						values = append(values, spec.toConnectProviderSpec())
					}
				}
			}
			return values
		}
		if providerSpecsFingerprint(toSpecs(specs)) != providerSpecsFingerprint(toSpecs(locationProviderSpecs(location))) {
			return errors.New("custom destination cannot be saved as a location")
		}
		return localState.setLocationWithLock(localConnectLocationFileName, location)
	}, func() (func(), error) {
		if location != nil && location.ConnectLocationId == nil {
			return nil, errors.New("invalid connect location")
		}
		return self.applyDestination(location, specs, rebuild)
	})
}

// The default is separately persisted and never changes current routing.
func (self *DeviceLocal) SetDefaultLocationChecked(location *ConnectLocation) error {
	notify, err := self.setDefaultLocationCheckedDeferred(location)
	if notify != nil {
		notify()
	}
	return err
}

// The Sync caller publishes this result after releasing its service lock.
func (self *DeviceLocal) setDefaultLocationCheckedDeferred(location *ConnectLocation) (func(), error) {
	location = cloneConnectLocation(location)
	return self.mutateLocalPreferenceDeferred("default-location", func(localState *LocalState) error {
		if location != nil && location.ConnectLocationId == nil {
			return errors.New("invalid default location")
		}
		return localState.setLocationWithLock(localDefaultLocationFileName, location)
	}, func() (func(), error) {
		if location != nil && location.ConnectLocationId == nil {
			return nil, errors.New("invalid default location")
		}
		return self.applyDefaultLocation(location)
	})
}

// Serializes explicit choices through file commit and live application, then
// publishes both callbacks and the immutable result without any lock held.
// This is an in-process boundary; native owners still join before replacing
// NetworkSpace/LocalState objects that point at the same directory.
func (self *DeviceLocal) mutateLocalPreference(
	preference string,
	save func(*LocalState) error,
	apply func() (func(), error),
) error {
	notify, err := self.mutateLocalPreferenceDeferred(preference, save, apply)
	if notify != nil {
		notify()
	}
	return err
}

// The caller owns exactly one notification closure, not a global drain queue.
// RPC Sync collects it locally and publishes after its outer lock is released.
func (self *DeviceLocal) mutateLocalPreferenceDeferred(
	preference string,
	save func(*LocalState) error,
	apply func() (func(), error),
) (func(), error) {
	var notify func()
	var listeners *connect.CallbackList[LocalStateSaveListener]
	var result *DeviceLocalSaveResult
	err := func() error {
		self.preferenceMutationLock.Lock()
		defer self.preferenceMutationLock.Unlock()
		self.preferenceSaveSequence += 1
		result = &DeviceLocalSaveResult{
			sequence: self.preferenceSaveSequence, preference: preference, autoSaveEnabled: self.autoSave,
		}
		defer func() {
			self.lastLocalStateSave = result
			listeners = self.localStateSaveListeners
		}()
		if self.authPublication != nil {
			release := self.authPublication.Begin()
			if release == nil {
				result.errorMessage = localPreferencesClosedMessage
				return errors.New(result.errorMessage)
			}
			defer release()
		}
		if self.ctx != nil && self.ctx.Err() != nil {
			result.errorMessage = localPreferencesClosedMessage
			return errors.New(result.errorMessage)
		}
		if self.autoSave {
			if err := self.withOwnedPreferenceStore(save); err != nil {
				result.errorMessage = "save " + preference
				if err.Error() == localAuthSnapshotSupersededMessage || err.Error() == localPreferencesClosedMessage {
					result.errorMessage = err.Error()
				}
				return localStorageStageError(result.errorMessage, err)
			}
			result.saved = true
		}
		var err error
		notify, err = apply()
		if err != nil {
			result.errorMessage = "apply " + preference
			return localStorageStageError(result.errorMessage, err)
		}
		return nil
	}()
	return func() {
		if err != nil && self.log != nil {
			self.log.Errorf("[local-state] save sequence=%d preference=%s auto_save=%t saved=%t error=%s",
				result.sequence, result.preference, result.autoSaveEnabled, result.saved, result.errorMessage)
		}
		if notify != nil {
			notify()
		}
		if listeners != nil {
			for _, listener := range listeners.Get() {
				connect.HandleError(func() { listener.LocalStateSaved(result) })
			}
		}
	}, err
}
