package sdk

import (
	"bytes"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// DeviceLocalKeyMaterial carries the provider client's persisted identity
// material. Pass a value returned by DeviceLocal.GetKeyMaterial back to
// NewDeviceLocalWithKeyMaterial on the next process start to keep the
// provider ClientKey, TLS cert commitment and extender identity stable.
//
// An identity belongs to the network it was made for (owner decision
// 2026-10-05: logout must not cross contaminate other networks; each network
// starts fresh). Material that came from a device or from local state records
// that network, and a device for another network's client never takes it: it
// starts on a new identity instead, so peers never see one key under two
// networks' clients. Material an embedder builds itself records none and is
// taken as before; such embedders delete their stored identity at sign-out.
type DeviceLocalKeyMaterial struct {
	clientKeySeed            []byte
	provideTlsCertificatePem []byte
	provideTlsPrivateKeyPem  []byte
	// The extender identity seed of the device's space (EXTENDER.md B1, G2),
	// carried for an embedder whose space keeps no local state of its own: a
	// space with local state keeps `.extender_key` and that always wins, so
	// this is empty there.
	extenderKeySeed []byte
	// The network the identity was made for, empty when unknown.
	networkId string
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

// An independent copy, including the network the identity belongs to.
func (self *DeviceLocalKeyMaterial) clone() *DeviceLocalKeyMaterial {
	if self == nil {
		return nil
	}
	keyMaterial := NewDeviceLocalKeyMaterial(
		self.clientKeySeed,
		self.provideTlsCertificatePem,
		self.provideTlsPrivateKeyPem,
	)
	keyMaterial.extenderKeySeed = bytes.Clone(self.extenderKeySeed)
	keyMaterial.networkId = self.networkId
	return keyMaterial
}

// Whether a device for the client `byJwt` may take this identity: false only
// when both the identity and the credential name a network and they differ.
func (self *DeviceLocalKeyMaterial) belongsToNetworkOf(byJwt string) bool {
	if self == nil || self.networkId == "" {
		return true
	}
	networkId := byJwtNetworkId(byJwt)
	return networkId == "" || networkId == self.networkId
}

// The network a credential (a client or an admin jwt) names, in canonical
// form, or empty when it names none or cannot be read. Unverified: it only
// decides which stored identity a device may take, never any admission.
func byJwtNetworkId(byJwt string) string {
	if byJwt == "" {
		return ""
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return ""
	}
	text, ok := claims["network_id"].(string)
	if !ok {
		return ""
	}
	networkId, err := ParseId(text)
	if err != nil {
		return ""
	}
	return networkId.String()
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
