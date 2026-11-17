// Explicit provider identity persistence uses the accepted local device owner.
// It is independent of preference autosave and never changes auth or live keys.
package sdk

import "errors"

const providerKeyStoreUnsupportedMessage = "provider key persistence requires an owned local store"
const providerKeyOwnerClosedMessage = "provider key owner is closed"
const providerKeyClientMissingMessage = "provider key client is unavailable"

// Saves this device's current material only. Native owners still fence their
// own callback admission; the final SDK ownership check prevents late writes
// after reset, replacement or close. No provider-secret list is written here.
// The existing file writer is not transactional: a write error can have partial
// effects. Callers must stop, not construct a replacement identity on error.
func (self *DeviceLocal) SaveKeyMaterial() error {
	return self.saveProviderKeyState("key-material")
}

// Saves only the current provider-secret list, after its explicit load/init.
// Callers that also save key material must check both operation errors; two
// successful calls are not one atomic two-file transaction. Autosave is unused.
func (self *DeviceLocal) SaveProvideSecretKeys() error {
	return self.saveProviderKeyState("provide-secret-keys")
}

// Only the two fixed public operations enter here. Serialization begins before
// capture, so delayed callbacks persist the current level, not an old payload.
// No callback/notification/client getter executes inside the auth lock boundary.
func (self *DeviceLocal) saveProviderKeyState(part string) error {
	self.providerKeySaveLock.Lock()
	defer self.providerKeySaveLock.Unlock()
	if self.settings == nil || self.settings.HostedIncompatible || self.ownsApi ||
		self.networkSpace == nil || self.api == nil || self.api != self.networkSpace.api ||
		self.networkSpace.asyncLocalState == nil || self.authPublication == nil {
		return errors.New(providerKeyStoreUnsupportedMessage)
	}
	release := self.authPublication.Begin()
	if release == nil {
		return errors.New(providerKeyOwnerClosedMessage)
	}
	defer release()
	if self.providerClientSnapshot() == nil {
		return errors.New(providerKeyClientMissingMessage)
	}
	var material *DeviceLocalKeyMaterial
	var secrets *ProvideSecretKeyList
	if part == "key-material" {
		material = self.GetKeyMaterial()
		if material == nil || material.IsEmpty() {
			return errors.New("device key material is unavailable")
		}
	} else {
		secrets = self.GetProvideSecretKeys()
		if secrets == nil {
			return errors.New("provide secret keys are unavailable")
		}
	}
	if self.testingBeforeProviderKeySaveAdmission != nil {
		self.testingBeforeProviderKeySaveAdmission(part)
	}
	// This existing paired admission does not depend on the autosave mode:
	// API authMutationLock -> LocalState authStateLock -> short API mutex.
	return self.withOwnedPreferenceStore(func(localState *LocalState) error {
		var err error
		if part == "key-material" {
			err = localState.setDeviceLocalKeyMaterialWithLock(material)
		} else {
			err = localState.SetProvideSecretKeys(secrets)
		}
		if err != nil {
			if part == "key-material" {
				return errors.New("save device key material")
			}
			return errors.New("save provide secret keys")
		}
		if self.testingAfterProviderKeySaveCommit != nil {
			self.testingAfterProviderKeySaveCommit(part)
		}
		return nil
	})
}
