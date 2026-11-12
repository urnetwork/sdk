//go:build js && wasm && sidecar_http

// The page Worker's SidecarKeyStore (§18.8, D-SDK-15): a non-extractable WebCrypto AES-GCM root key, kept by
// structured clone in the keys object store of one network's database, which wraps each secret the core puts here, W
// above all, which wraps U (keys_unlock.go). Each network's database is its own instance (store_indexeddb.go, owner
// decision 217), so each has its own root key, made at its first put, and a secret sealed in one network's opens in no
// other's. The key's bytes never exist in Go or in script: generateKey makes it with extractable false, so exportKey
// refuses it, and IndexedDB keeps the CryptoKey object itself. A secret comes back to Go in the Worker and goes
// nowhere else: GetSecret's bytes never leave the Worker (§18.8), and the core reports W to the privacy harness as it
// takes it (testHookSecret("W")), so nothing here repeats that.
//
// A secret is stored as {iv, ciphertext}, AES-GCM with a fresh 12-byte iv and the item's name as additional data,
// under "secret/<name>", so a record moved to another name fails to open.
//
// Statuses follow §5.9's table through D-SDK-15. Absent only when the database answers and holds no such item, which
// an eviction gives with nothing recorded, so the core enrolls anew (only Absent with no recorded sidecar_id creates a
// root). A secret with no root key, a record that is not one, a failed decrypt, a quota error, an abort or a database
// that will not answer is Failed, which creates, overwrites and deletes nothing. WebCrypto has no lock, so nothing here
// answers Locked. No call panics across the host boundary.
//
// Every call waits on the browser, so none may run inside a js.FuncOf callback (async_js.go). Calls run one at a time,
// so two first puts make one root key. Safe for concurrent use from goroutines.

package main

import (
	"crypto/rand"
	"sync"
	"syscall/js"

	"github.com/urnetwork/sidecar"
)

// Where the root key and the secrets are, in the keys object store, and the AEAD's sizes.
const (
	webCryptoRootKeyName    = "root"
	webCryptoSecretPrefix   = "secret/"
	webCryptoIvByteCount    = 12
	webCryptoTagByteCount   = 16
	webCryptoAdditionalData = "urnetwork-sidecar-v1/keystore\x00"
)

// A key store over database's keys object store.
type webCryptoKeyStore struct {
	database *indexedDb
	// one call at a time, so the root key is made once
	operationLock sync.Mutex
}

// The key store over database.
func newWebCryptoKeyStore(database *indexedDb) *webCryptoKeyStore {
	return &webCryptoKeyStore{database: database}
}

// The Worker's crypto.subtle.
func webCryptoSubtle() (js.Value, bool) {
	cryptoObject := js.Global().Get("crypto")
	if cryptoObject.Type() != js.TypeObject {
		return js.Undefined(), false
	}
	subtle := cryptoObject.Get("subtle")
	return subtle, subtle.Type() == js.TypeObject
}

// Whether value is a non-extractable AES-GCM CryptoKey that can encrypt and decrypt: anything else under the root's
// name is no root this store made.
func webCryptoIsRootKey(value js.Value) bool {
	cryptoKey := js.Global().Get("CryptoKey")
	if value.Type() != js.TypeObject || cryptoKey.Type() != js.TypeFunction || !value.InstanceOf(cryptoKey) {
		return false
	}
	if value.Get("extractable").Bool() || value.Get("algorithm").Get("name").String() != "AES-GCM" {
		return false
	}
	usages := value.Get("usages")
	return usages.Call("includes", "encrypt").Bool() && usages.Call("includes", "decrypt").Bool()
}

// The root key: Present with it, Absent when the store holds none, Failed otherwise. With create, an absent one is
// made, non-extractable, and added only where none is, so two Workers racing keep one.
func (self *webCryptoKeyStore) rootKey(create bool) (js.Value, sidecar.SidecarKeyStoreStatus) {
	value, err := self.database.get(indexedDbKeysStore, webCryptoRootKeyName)
	if err != nil {
		return js.Undefined(), sidecar.SidecarKeyStoreFailed
	}
	if !value.IsUndefined() {
		if !webCryptoIsRootKey(value) {
			return js.Undefined(), sidecar.SidecarKeyStoreFailed
		}
		return value, sidecar.SidecarKeyStorePresent
	}
	if !create {
		return js.Undefined(), sidecar.SidecarKeyStoreAbsent
	}
	subtle, ok := webCryptoSubtle()
	if !ok {
		return js.Undefined(), sidecar.SidecarKeyStoreFailed
	}
	algorithm := js.ValueOf(map[string]any{"name": "AES-GCM", "length": 256})
	usages := js.ValueOf([]any{"encrypt", "decrypt"})
	key, err := awaitPromise(subtle.Call("generateKey", algorithm, false, usages))
	if err != nil || !webCryptoIsRootKey(key) {
		return js.Undefined(), sidecar.SidecarKeyStoreFailed
	}
	added, err := self.database.add(indexedDbKeysStore, webCryptoRootKeyName, key)
	if err != nil {
		return js.Undefined(), sidecar.SidecarKeyStoreFailed
	}
	if !added {
		// another Worker's came first: that one wraps everything
		return self.rootKey(false)
	}
	return key, sidecar.SidecarKeyStorePresent
}

// The AEAD's parameters for name and iv.
func webCryptoAesGcm(name string, iv []byte) js.Value {
	return js.ValueOf(map[string]any{
		"name":           "AES-GCM",
		"iv":             bytesToJs(iv),
		"additionalData": bytesToJs([]byte(webCryptoAdditionalData + name)),
		"tagLength":      webCryptoTagByteCount * 8,
	})
}

// Runs a call, turning a panic, which syscall/js raises for a thrown exception, into Failed.
func webCryptoGuard(status *sidecar.SidecarKeyStoreStatus) {
	if recovered := recover(); recovered != nil {
		*status = sidecar.SidecarKeyStoreFailed
	}
}

// Wraps value under the root key, making the key the first time: Ok or Failed.
func (self *webCryptoKeyStore) PutSecret(name string, value []byte) (status sidecar.SidecarKeyStoreStatus) {
	defer webCryptoGuard(&status)
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	key, keyStatus := self.rootKey(true)
	if keyStatus != sidecar.SidecarKeyStorePresent {
		return sidecar.SidecarKeyStoreFailed
	}
	subtle, ok := webCryptoSubtle()
	if !ok {
		return sidecar.SidecarKeyStoreFailed
	}
	iv := make([]byte, webCryptoIvByteCount)
	if _, err := rand.Read(iv); err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	ciphertext, err := awaitPromise(subtle.Call("encrypt", webCryptoAesGcm(name, iv), key, bytesToJs(value)))
	if err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	sealed, ok := bytesFromJs(ciphertext)
	if !ok {
		return sidecar.SidecarKeyStoreFailed
	}
	record := js.ValueOf(map[string]any{"iv": bytesToJs(iv), "ciphertext": bytesToJs(sealed)})
	if err := self.database.put(indexedDbKeysStore, webCryptoSecretPrefix+name, record); err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	return sidecar.SidecarKeyStoreOk
}

// The secret under name with Present, or nil with Absent or Failed.
func (self *webCryptoKeyStore) GetSecret(name string) (secret []byte, status sidecar.SidecarKeyStoreStatus) {
	defer webCryptoGuard(&status)
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	record, err := self.database.get(indexedDbKeysStore, webCryptoSecretPrefix+name)
	if err != nil {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	if record.IsUndefined() {
		return nil, sidecar.SidecarKeyStoreAbsent
	}
	// a secret whose root key is gone cannot be vouched for: Failed, never Absent
	key, keyStatus := self.rootKey(false)
	if keyStatus != sidecar.SidecarKeyStorePresent || record.Type() != js.TypeObject {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	iv, ivOk := bytesFromJs(record.Get("iv"))
	sealed, sealedOk := bytesFromJs(record.Get("ciphertext"))
	if !ivOk || !sealedOk || len(iv) != webCryptoIvByteCount || len(sealed) < webCryptoTagByteCount {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	subtle, ok := webCryptoSubtle()
	if !ok {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	plaintext, err := awaitPromise(subtle.Call("decrypt", webCryptoAesGcm(name, iv), key, bytesToJs(sealed)))
	if err != nil {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	opened, ok := bytesFromJs(plaintext)
	if !ok {
		return nil, sidecar.SidecarKeyStoreFailed
	}
	return opened, sidecar.SidecarKeyStorePresent
}

// Ok whether the secret was deleted or absent, else Failed. The root key stays: other secrets need it.
func (self *webCryptoKeyStore) DeleteSecret(name string) (status sidecar.SidecarKeyStoreStatus) {
	defer webCryptoGuard(&status)
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if err := self.database.delete(indexedDbKeysStore, webCryptoSecretPrefix+name); err != nil {
		return sidecar.SidecarKeyStoreFailed
	}
	return sidecar.SidecarKeyStoreOk
}
