package sdk

import (
	"bytes"

	"github.com/urnetwork/connect/v2026"
)

// DeviceLocalKeyMaterial carries the provider client's persisted identity
// material. Pass a value returned by DeviceLocal.GetKeyMaterial back to
// NewDeviceLocalWithKeyMaterial on the next process start to keep the
// provider ClientKey, TLS cert commitment and extender identity stable.
type DeviceLocalKeyMaterial struct {
	clientKeySeed            []byte
	provideTlsCertificatePem []byte
	provideTlsPrivateKeyPem  []byte
	// The extender identity seed of the device's space (EXTENDER.md B1, G2),
	// carried for an embedder whose space keeps no local state of its own: a
	// space with local state keeps `.extender_key` and that always wins, so
	// this is empty there.
	extenderKeySeed []byte
}

func NewDeviceLocalKeyMaterial(clientKeySeed []byte, provideTlsCertificatePem []byte, provideTlsPrivateKeyPem []byte) *DeviceLocalKeyMaterial {
	return &DeviceLocalKeyMaterial{
		clientKeySeed:            bytes.Clone(clientKeySeed),
		provideTlsCertificatePem: bytes.Clone(provideTlsCertificatePem),
		provideTlsPrivateKeyPem:  bytes.Clone(provideTlsPrivateKeyPem),
	}
}

func (self *DeviceLocalKeyMaterial) GetClientKeySeed() []byte {
	if self == nil {
		return nil
	}
	return bytes.Clone(self.clientKeySeed)
}

func (self *DeviceLocalKeyMaterial) GetProvideTlsCertificatePem() []byte {
	if self == nil {
		return nil
	}
	return bytes.Clone(self.provideTlsCertificatePem)
}

func (self *DeviceLocalKeyMaterial) GetProvideTlsPrivateKeyPem() []byte {
	if self == nil {
		return nil
	}
	return bytes.Clone(self.provideTlsPrivateKeyPem)
}

// The extender identity seed (B1). Empty when the device's space persists its
// own, which is every space with local state.
func (self *DeviceLocalKeyMaterial) GetExtenderKeySeed() []byte {
	if self == nil {
		return nil
	}
	return bytes.Clone(self.extenderKeySeed)
}

// Carries an extender identity seed an embedder kept from a previous run, so
// the device's space activates and peers under the identity its records
// already name (B1, G2). A seed of the wrong length is refused here rather
// than by the role, which would silently run on a generated one instead.
func (self *DeviceLocalKeyMaterial) SetExtenderKeySeed(extenderKeySeed []byte) {
	if self == nil {
		return
	}
	if 0 < len(extenderKeySeed) {
		if _, err := connect.ExtenderPublicKeyFromSeed(extenderKeySeed); err != nil {
			return
		}
	}
	self.extenderKeySeed = bytes.Clone(extenderKeySeed)
}

func (self *DeviceLocalKeyMaterial) IsEmpty() bool {
	return self == nil || (len(self.clientKeySeed) == 0 &&
		len(self.provideTlsCertificatePem) == 0 &&
		len(self.provideTlsPrivateKeyPem) == 0 &&
		len(self.extenderKeySeed) == 0)
}

func applyDeviceLocalKeyMaterial(settings *connect.ClientSettings, keyMaterial *DeviceLocalKeyMaterial) {
	if settings == nil || keyMaterial == nil {
		return
	}
	if 0 < len(keyMaterial.clientKeySeed) {
		settings.ClientKeySeed = bytes.Clone(keyMaterial.clientKeySeed)
	}
	if 0 < len(keyMaterial.provideTlsCertificatePem) || 0 < len(keyMaterial.provideTlsPrivateKeyPem) {
		if settings.EncryptionSettings == nil {
			settings.EncryptionSettings = connect.DefaultEncryptionSettings()
		} else {
			encryptionSettings := *settings.EncryptionSettings
			settings.EncryptionSettings = &encryptionSettings
		}
		settings.EncryptionSettings.ProvideTlsCertificatePem = bytes.Clone(keyMaterial.provideTlsCertificatePem)
		settings.EncryptionSettings.ProvideTlsPrivateKeyPem = bytes.Clone(keyMaterial.provideTlsPrivateKeyPem)
	}
}
