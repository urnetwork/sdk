//go:build !ios_extension && !js

// The app's secret stores across the binding (§18.5, D-SDK-15): SidecarKeyStore, the app's wrap of the sidecar's
// unlock key U (the data protection keychain, AndroidKeyStore), and SidecarPresenceSigner, its presence key.
//
// An app's implementation is untrusted across gobind. gobind carries no error type, and a panic that crosses it exits
// the app, so the store answers status codes and a SidecarSecret, and the adapter here reads anything else as Failed:
// a nil secret, a panic, a status it does not know, or a value that does not decode. Failed is never Absent, and only
// Absent, for a network with no recorded sidecar_id, creates a root key (§5.9's table, D-CORE-15), which the core
// decides from the statuses this adapter maps one for one. Values cross as standard base64.

package sdk

import (
	"encoding/base64"

	"github.com/urnetwork/sidecar"
)

// What PutSecret and DeleteSecret answer: the core's SidecarKeyStoreStatus numbers, one for one. Zero is Failed, so a
// store that sets nothing is never read as anything else.
const (
	// A fault, or a value the store cannot vouch for. Nothing is created, overwritten or deleted on it.
	SidecarKeyStoreFailed int32 = 0
	// The write took: kept, or deleted or absent.
	SidecarKeyStoreOk int32 = 1
	// The store cannot answer until it unlocks, after which the app calls Unlock(""). Nothing is created,
	// overwritten or deleted on it.
	SidecarKeyStoreLocked int32 = 4
)

// What a SidecarSecret's Status says: the core's numbers, one for one, zero Failed.
const (
	// A fault, or a value the store cannot vouch for.
	SidecarSecretFailed int32 = 0
	// The value is in ValueBase64.
	SidecarSecretPresent int32 = 2
	// The store works and holds no such item, which a lock must never be reported as.
	SidecarSecretAbsent int32 = 3
	// The store cannot answer until it unlocks.
	SidecarSecretLocked int32 = 4
)

// The app's store of the sidecar's secrets, which never throws across the binding: each call answers once, with a
// status. Called on Go threads, possibly several at once.
type SidecarKeyStore interface {
	// Keeps the value under name, replacing any earlier one: SidecarKeyStoreOk, SidecarKeyStoreLocked or
	// SidecarKeyStoreFailed.
	PutSecret(name string, valueBase64 string) int32
	// The value under name, with its status. Never nil.
	GetSecret(name string) *SidecarSecret
	// SidecarKeyStoreOk whether it was deleted or absent, else SidecarKeyStoreLocked or SidecarKeyStoreFailed.
	DeleteSecret(name string) int32
}

// GetSecret's answer. json in the C ABI and js.
type SidecarSecret struct {
	// SidecarSecretPresent, SidecarSecretAbsent, SidecarSecretLocked or SidecarSecretFailed.
	Status int32 `json:"status"`
	// Standard base64 of the value, set only when Present.
	ValueBase64 string `json:"value_base64,omitempty"`
}

// The app's presence key: a Secure Enclave or platform FIDO key, per §16.6's profiles. Nil: another device approves.
// The calls that use it are SDK-M8-05's and later; the shell keeps it for them.
type SidecarPresenceSigner interface {
	// The public key and its attestation, registered per §16.6.
	GetAttestation() string
	// After a presence check with user verification over the message (webauthn-es256: its SHA-256 as the
	// challenge), base64 of §6.9's WebAuthnEvidence; "" if refused.
	SignWithPresence(messageBase64 string) string
}

// The core's SidecarKeyStore over an app's: base64 strings to bytes, statuses one for one, and anything odd Failed.
type sidecarKeyStoreAdapter struct {
	keyStore SidecarKeyStore
}

// The core's view of keyStore.
func newSidecarKeyStoreAdapter(keyStore SidecarKeyStore) *sidecarKeyStoreAdapter {
	return &sidecarKeyStoreAdapter{keyStore: keyStore}
}

// Keeps value under name. A panic or a status other than Ok or Locked is Failed.
func (self *sidecarKeyStoreAdapter) PutSecret(name string, value []byte) sidecar.SidecarKeyStoreStatus {
	status := SidecarKeyStoreFailed
	if err := trace(func() {
		status = self.keyStore.PutSecret(name, base64.StdEncoding.EncodeToString(value))
	}); err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	return sidecarWriteStatusOf(status)
}

// The value under name. A nil secret, a panic, a status the core does not know, or a Present value that is not
// standard base64 is Failed, never Absent.
func (self *sidecarKeyStoreAdapter) GetSecret(name string) ([]byte, sidecar.SidecarKeyStoreStatus) {
	var secret *SidecarSecret
	if err := trace(func() {
		secret = self.keyStore.GetSecret(name)
	}); err != nil || secret == nil {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	switch secret.Status {
	case SidecarSecretPresent:
		value, err := base64.StdEncoding.Strict().DecodeString(secret.ValueBase64)
		if err != nil {
			return nil, sidecar.SidecarKeyStoreFailed
		}
		return value, sidecar.SidecarKeyStorePresent
	case SidecarSecretAbsent:
		return nil, sidecar.SidecarKeyStoreAbsent
	case SidecarSecretLocked:
		return nil, sidecar.SidecarKeyStoreLocked
	}
	return nil, sidecar.SidecarKeyStoreFailed
}

// Deletes name. A panic or a status other than Ok or Locked is Failed.
func (self *sidecarKeyStoreAdapter) DeleteSecret(name string) sidecar.SidecarKeyStoreStatus {
	status := SidecarKeyStoreFailed
	if err := trace(func() {
		status = self.keyStore.DeleteSecret(name)
	}); err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	return sidecarWriteStatusOf(status)
}

// The core's status of a write's answer: Ok and Locked as themselves, everything else Failed.
func sidecarWriteStatusOf(status int32) sidecar.SidecarKeyStoreStatus {
	switch status {
	case SidecarKeyStoreOk:
		return sidecar.SidecarKeyStoreOk
	case SidecarKeyStoreLocked:
		return sidecar.SidecarKeyStoreLocked
	}
	return sidecar.SidecarKeyStoreFailed
}
