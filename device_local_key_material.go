package sdk

import (
	"bytes"

	"github.com/urnetwork/connect"
)

// DeviceLocalKeyMaterial carries the provider client's persisted identity
// material. Pass a value returned by DeviceLocal.GetKeyMaterial back to
// NewDeviceLocalWithKeyMaterial on the next process start to keep the
// provider ClientKey and TLS cert commitment stable.
type DeviceLocalKeyMaterial struct {
	clientKeySeed            []byte
	provideTlsCertificatePem []byte
	provideTlsPrivateKeyPem  []byte
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

func (self *DeviceLocalKeyMaterial) IsEmpty() bool {
	return self == nil || (len(self.clientKeySeed) == 0 &&
		len(self.provideTlsCertificatePem) == 0 &&
		len(self.provideTlsPrivateKeyPem) == 0)
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
