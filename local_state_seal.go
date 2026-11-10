// The credential rules for .auth_state (§18.7's network credential paragraph, D-SDK-11, I-6): what the file keeps,
// and the app's sealing of it. Same-user agents read the file, so a desktop release that opts in keeps no network
// credential there once a device client exists (NetworkSpaceValues.SessionOnlyNetworkCredential) and seals the
// file under a key in an app-only OS keystore (LocalStateSealer) where one exists. Both are off by default, so
// mobile apps and shipped desktops keep today's 0600 file until a release turns them on.
//
// The sealer is the app's, untrusted across the binding, in the shape of SidecarKeyStore (D-SDK-15): it never
// throws, each call answers a status, and the adapter here reads anything else as Failed: a nil key, a panic, a
// status it does not know, or a key that is not standard base64 of 32 bytes. Failed is never Absent, and Locked is
// never Absent either. A locked or failed sealer signs nobody out and deletes nothing: a read of the state fails,
// with ErrLocalStateSealerLocked or ErrLocalStateSealerFailed, a write fails before it touches the file, nothing
// falls back to the legacy files or to the plaintext envelope, and once the store answers again the state reads as
// it was.
//
// A sealed file:
//
//	"UAS1" || key id (16 random B) || nonce (12 random B) || AES-256-GCM(key, the envelope's json, AAD below)
//	AAD    "UAS1" || key id || ".auth_state"
//	key    32 random bytes, in the sealer as "auth_state." + hex(key id)
//
// The key is made the first time a sealer writes the file and kept for every later write of it, each under a fresh
// nonce; a file names its own key, so two spaces, or the app and its VPN component, never take each other's. A
// plaintext envelope found while a sealer is installed is sealed at its first read, and a sealed one read with no
// sealer installed is refused. Only the file's name is bound, not its directory: a space's directory is its own.
//
// F's token, the forwarding client's of the app's VPN component (DESIGN-DETAILS 4.2.2), lives only in
// SidecarForwardKeyStore, never in .auth_state. The file keeps its json keys either way, which CORE-M1-09's
// hostCredentialStores scans for a readable by_jwt.
package sdk

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// What PutSealKey and DeleteSealKey answer: SidecarKeyStore's numbers, so an app's keystore code serves both. Zero
// is Failed, so a sealer that sets nothing is never read as anything else.
const (
	// A fault, or a key the store cannot vouch for. Nothing is created, overwritten or deleted on it.
	LocalStateSealerFailed int32 = 0
	// The write took: kept, or deleted or absent.
	LocalStateSealerOk int32 = 1
	// The store cannot answer until it unlocks. Nothing is created, overwritten or deleted on it.
	LocalStateSealerLocked int32 = 4
)

// What a LocalStateSealKey's Status says, zero Failed.
const (
	// A fault, or a key the store cannot vouch for.
	LocalStateSealKeyFailed int32 = 0
	// The key is in KeyBase64.
	LocalStateSealKeyPresent int32 = 2
	// The store works and holds no such key, which a lock must never be reported as.
	LocalStateSealKeyAbsent int32 = 3
	// The store cannot answer until it unlocks.
	LocalStateSealKeyLocked int32 = 4
)

// The app's keystore for the key that seals .auth_state: an app-only OS keystore, such as the data protection
// keychain with an access group of the app's alone, so a same-user agent reads neither the key nor, through the
// file, a credential. Never throws across the binding: each call answers once, with a status. Called on Go
// threads, possibly several at once.
type LocalStateSealer interface {
	// Keeps the key under name, replacing any earlier one: LocalStateSealerOk, LocalStateSealerLocked or
	// LocalStateSealerFailed.
	PutSealKey(name string, keyBase64 string) int32
	// The key under name, with its status. Never nil.
	GetSealKey(name string) *LocalStateSealKey
	// LocalStateSealerOk whether it was deleted or absent, else LocalStateSealerLocked or LocalStateSealerFailed.
	DeleteSealKey(name string) int32
}

// GetSealKey's answer. json in the C ABI and js.
type LocalStateSealKey struct {
	// LocalStateSealKeyPresent, LocalStateSealKeyAbsent, LocalStateSealKeyLocked or LocalStateSealKeyFailed.
	Status int32 `json:"status"`
	// Standard base64 of the key, set only when Present.
	KeyBase64 string `json:"key_base64,omitempty"`
}

var (
	// The auth state is sealed under a key the app's keystore cannot give while it is locked. Nothing was changed:
	// read again once it unlocks.
	ErrLocalStateSealerLocked = errors.New("the auth state is sealed by an OS keystore that is locked")
	// The app's keystore failed, or answered what it cannot vouch for. Nothing was changed.
	ErrLocalStateSealerFailed = errors.New("the auth state's OS keystore failed")
	// The file is sealed and this local state has no sealer to open it.
	errLocalAuthStateSealedWithoutSealer = errors.New("the auth state is sealed and no sealer is installed")
	// The file's key is gone from the app's keystore, so the file opens no more.
	errLocalAuthStateSealKeyAbsent = errors.New("the auth state's sealing key is absent")
	// The file does not open under its key: a flipped byte, a truncation, or a file from elsewhere.
	errLocalAuthStateSealBroken = errors.New("the sealed auth state does not open")
)

// The sealed record's layout, a format of this file.
const (
	localAuthStateSealMagic        = "UAS1"
	localAuthStateSealKeyIdLength  = 16
	localAuthStateSealNonceLength  = 12
	localAuthStateSealKeyLength    = 32
	localAuthStateSealHeaderLength = len(localAuthStateSealMagic) + localAuthStateSealKeyIdLength + localAuthStateSealNonceLength
	localAuthStateSealKeyPrefix    = "auth_state."
)

// How a local state keeps .auth_state, fixed at its construction.
type localAuthSettings struct {
	// nil keeps the 0600 file in plaintext, as today
	sealer *localStateSealerAdapter
	// no by_jwt once a device client exists (NetworkSpaceValues.SessionOnlyNetworkCredential)
	sessionOnlyNetworkCredential bool
}

// The settings a space's values and its manager's sealer ask for.
func newLocalAuthSettings(sessionOnlyNetworkCredential bool, sealer LocalStateSealer) localAuthSettings {
	settings := localAuthSettings{sessionOnlyNetworkCredential: sessionOnlyNetworkCredential}
	if sealer != nil {
		settings.sealer = &localStateSealerAdapter{sealer: sealer}
	}
	return settings
}

// The app's sealer seen from here: bytes in and out, statuses one for one, and anything odd Failed.
type localStateSealerAdapter struct {
	sealer LocalStateSealer
}

// Keeps key under name. A panic or a status other than Ok or Locked is Failed.
func (self *localStateSealerAdapter) putKey(name string, key []byte) int32 {
	status := LocalStateSealerFailed
	if err := trace(func() {
		status = self.sealer.PutSealKey(name, base64.StdEncoding.EncodeToString(key))
	}); err != nil {
		return LocalStateSealerFailed
	}
	return localStateSealerWriteStatusOf(status)
}

// The key under name. A nil answer, a panic, a status this does not know, or a Present key that is not standard
// base64 of 32 bytes is Failed, never Absent.
func (self *localStateSealerAdapter) getKey(name string) ([]byte, int32) {
	var sealKey *LocalStateSealKey
	if err := trace(func() {
		sealKey = self.sealer.GetSealKey(name)
	}); err != nil || sealKey == nil {
		return nil, LocalStateSealKeyFailed
	}
	switch sealKey.Status {
	case LocalStateSealKeyPresent:
		key, err := base64.StdEncoding.Strict().DecodeString(sealKey.KeyBase64)
		if err != nil || len(key) != localAuthStateSealKeyLength {
			return nil, LocalStateSealKeyFailed
		}
		return key, LocalStateSealKeyPresent
	case LocalStateSealKeyAbsent:
		return nil, LocalStateSealKeyAbsent
	case LocalStateSealKeyLocked:
		return nil, LocalStateSealKeyLocked
	}
	return nil, LocalStateSealKeyFailed
}

// Deletes name. A panic or a status other than Ok or Locked is Failed.
func (self *localStateSealerAdapter) deleteKey(name string) int32 {
	status := LocalStateSealerFailed
	if err := trace(func() {
		status = self.sealer.DeleteSealKey(name)
	}); err != nil {
		return LocalStateSealerFailed
	}
	return localStateSealerWriteStatusOf(status)
}

// Ok and Locked as themselves, everything else Failed.
func localStateSealerWriteStatusOf(status int32) int32 {
	switch status {
	case LocalStateSealerOk, LocalStateSealerLocked:
		return status
	}
	return LocalStateSealerFailed
}

// The error a sealer's non-Ok answer reads as.
func localStateSealerError(status int32) error {
	if status == LocalStateSealerLocked || status == LocalStateSealKeyLocked {
		return ErrLocalStateSealerLocked
	}
	return ErrLocalStateSealerFailed
}

// The sealer's name for the key with this id.
func localAuthStateSealKeyName(keyId []byte) string {
	return localAuthStateSealKeyPrefix + hex.EncodeToString(keyId)
}

// What a sealed file's AEAD binds besides its plaintext.
func localAuthStateSealAad(keyId []byte) []byte {
	aad := make([]byte, 0, len(localAuthStateSealMagic)+len(keyId)+len(localAuthStateFileName))
	aad = append(aad, localAuthStateSealMagic...)
	aad = append(aad, keyId...)
	aad = append(aad, localAuthStateFileName...)
	return aad
}

// Whether data is a sealed record rather than the plaintext envelope, which is json and starts with {.
func isSealedLocalAuthState(data []byte) bool {
	return len(localAuthStateSealMagic) <= len(data) && string(data[:len(localAuthStateSealMagic)]) == localAuthStateSealMagic
}

func newLocalAuthStateAead(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// The plaintext envelope of a sealed file. Caller holds authStateLock.
func (self *LocalState) unsealAuthStateLocked(data []byte) ([]byte, error) {
	sealer := self.authSettings.sealer
	if sealer == nil {
		return nil, errLocalAuthStateSealedWithoutSealer
	}
	if len(data) < localAuthStateSealHeaderLength {
		return nil, errLocalAuthStateSealBroken
	}
	keyId := data[len(localAuthStateSealMagic) : len(localAuthStateSealMagic)+localAuthStateSealKeyIdLength]
	nonce := data[len(localAuthStateSealMagic)+localAuthStateSealKeyIdLength : localAuthStateSealHeaderLength]
	key, status := sealer.getKey(localAuthStateSealKeyName(keyId))
	switch status {
	case LocalStateSealKeyPresent:
	case LocalStateSealKeyAbsent:
		return nil, errLocalAuthStateSealKeyAbsent
	default:
		return nil, localStateSealerError(status)
	}
	aead, err := newLocalAuthStateAead(key)
	if err != nil {
		return nil, ErrLocalStateSealerFailed
	}
	plaintext, err := aead.Open(nil, nonce, data[localAuthStateSealHeaderLength:], localAuthStateSealAad(keyId))
	if err != nil {
		return nil, errLocalAuthStateSealBroken
	}
	return plaintext, nil
}

// The key id the file on disk names, nil when it is absent, plaintext or too short to name one. Caller holds
// authStateLock.
func (self *LocalState) sealedAuthStateKeyIdLocked() []byte {
	file, err := os.Open(self.authStatePath())
	if err != nil {
		return nil
	}
	defer file.Close()
	header := make([]byte, len(localAuthStateSealMagic)+localAuthStateSealKeyIdLength)
	if _, err := io.ReadFull(file, header); err != nil || !isSealedLocalAuthState(header) {
		return nil
	}
	return header[len(localAuthStateSealMagic):]
}

// The sealed record of a plaintext envelope, under the key of the file it replaces, or a new key when that file is
// not sealed or its key is gone. A locked or failed sealer fails here, before the file is touched. Caller holds
// authStateLock.
func (self *LocalState) sealAuthStateLocked(plaintext []byte) ([]byte, error) {
	sealer := self.authSettings.sealer
	var key []byte
	keyId := self.sealedAuthStateKeyIdLocked()
	if keyId != nil {
		var status int32
		key, status = sealer.getKey(localAuthStateSealKeyName(keyId))
		switch status {
		case LocalStateSealKeyPresent:
		case LocalStateSealKeyAbsent:
			// the file this replaces opens no more; the new one gets a key of its own
			keyId = nil
		default:
			return nil, localStateSealerError(status)
		}
	}
	if keyId == nil {
		keyId = make([]byte, localAuthStateSealKeyIdLength)
		key = make([]byte, localAuthStateSealKeyLength)
		if _, err := rand.Read(keyId); err != nil {
			return nil, err
		}
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if status := sealer.putKey(localAuthStateSealKeyName(keyId), key); status != LocalStateSealerOk {
			return nil, localStateSealerError(status)
		}
	}
	aead, err := newLocalAuthStateAead(key)
	if err != nil {
		return nil, ErrLocalStateSealerFailed
	}
	sealed := make([]byte, localAuthStateSealHeaderLength, localAuthStateSealHeaderLength+len(plaintext)+aead.Overhead())
	copy(sealed, localAuthStateSealMagic)
	copy(sealed[len(localAuthStateSealMagic):], keyId)
	nonce := sealed[len(localAuthStateSealMagic)+localAuthStateSealKeyIdLength : localAuthStateSealHeaderLength]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(sealed, nonce, plaintext, localAuthStateSealAad(keyId)), nil
}

// Deletes the key of a sealed file that a sign-out removed, best effort: a locked or failed sealer keeps it, which
// leaves an unused key and never an unreadable file. Called only once the file is gone. Caller holds authStateLock.
func (self *LocalState) deleteAuthStateSealKeyLocked(keyId []byte) {
	sealer := self.authSettings.sealer
	if sealer == nil || keyId == nil {
		return
	}
	if _, err := os.Lstat(self.authStatePath()); !errors.Is(err, os.ErrNotExist) {
		return
	}
	sealer.deleteKey(localAuthStateSealKeyName(keyId))
}

// What .auth_state keeps of state: with the network credential kept for the session only, no by_jwt once a device
// client exists, so the network credential lives in the Api's memory and dies with the session (§18.7); otherwise
// all of it. A by_jwt beside no client token stays: it is the sign-in that mints the device client, or a shipped
// Apple extension's client marker (selectStartupClientJwt).
func (self *LocalState) persistedAuthState(state persistedLocalAuthState) persistedLocalAuthState {
	if self.authSettings.sessionOnlyNetworkCredential && state.ByClientJwt != "" {
		state.ByJwt = ""
	}
	return state
}

// Whether this local state keeps the network credential for the session only, which an Api that adopts it renews
// in memory and never writes back (api_network_credential_renewal.go).
func (self *LocalState) sessionOnlyNetworkCredential() bool {
	return self.authSettings.sessionOnlyNetworkCredential
}
