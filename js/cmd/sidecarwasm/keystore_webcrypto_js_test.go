//go:build js && wasm && sidecar_http

// The WebCrypto key store, row by row through §5.9's status table by D-SDK-15, the instance's answers to it, and each
// network's database keeping its own root key, in Node under test/sidecarwasm_harness.mjs.
// test/sidecarwasm_keystore.test.ts builds and runs them.

package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The name the fake keeps a test's database under.
func testIndexedDbName(t *testing.T) string {
	return "test-" + t.Name()
}

// Each key store answer, as the table has it.
func TestWebCryptoKeyStoreStatusTable(t *testing.T) {
	database := testIndexedDb(t)
	keyStore := newWebCryptoKeyStore(database)
	if value, status := keyStore.GetSecret("w"); status != sidecar.SidecarKeyStoreAbsent || value != nil {
		t.Errorf("an empty store: %v, %v", value, status)
	}
	if status := keyStore.PutSecret("w", []byte("the store wrap's key")); status != sidecar.SidecarKeyStoreOk {
		t.Fatalf("put: %v", status)
	}
	if value, status := keyStore.GetSecret("w"); status != sidecar.SidecarKeyStorePresent || string(value) != "the store wrap's key" {
		t.Errorf("get: %q, %v", value, status)
	}
	if value, status := keyStore.GetSecret("another"); status != sidecar.SidecarKeyStoreAbsent || value != nil {
		t.Errorf("another name: %v, %v", value, status)
	}
	if status := keyStore.PutSecret("w", []byte("replaced")); status != sidecar.SidecarKeyStoreOk {
		t.Errorf("replace: %v", status)
	}
	if value, _ := keyStore.GetSecret("w"); string(value) != "replaced" {
		t.Errorf("replaced reads %q", value)
	}
	if status := keyStore.DeleteSecret("w"); status != sidecar.SidecarKeyStoreOk {
		t.Errorf("delete: %v", status)
	}
	if status := keyStore.DeleteSecret("w"); status != sidecar.SidecarKeyStoreOk {
		t.Errorf("delete again: %v", status)
	}
	if _, status := keyStore.GetSecret("w"); status != sidecar.SidecarKeyStoreAbsent {
		t.Errorf("after delete: %v", status)
	}
}

// What the store holds is ciphertext under a key no script can read: the record carries no secret's bytes, and
// exportKey on the stored root key rejects.
func TestWebCryptoRootKeyNeverLeaves(t *testing.T) {
	control := testFakeIndexedDb(t)
	database := testIndexedDb(t)
	keyStore := newWebCryptoKeyStore(database)
	secret := []byte("a secret that must stay inside the Worker")
	if status := keyStore.PutSecret("w", secret); status != sidecar.SidecarKeyStoreOk {
		t.Fatal(status)
	}
	record := control.Call("raw", testIndexedDbName(t), indexedDbKeysStore, webCryptoSecretPrefix+"w")
	ciphertext, ok := bytesFromJs(record.Get("ciphertext"))
	if !ok || bytes.Contains(ciphertext, secret) || len(ciphertext) != len(secret)+webCryptoTagByteCount {
		t.Errorf("the stored record: %d bytes, plaintext inside %v", len(ciphertext), bytes.Contains(ciphertext, secret))
	}
	root := control.Call("raw", testIndexedDbName(t), indexedDbKeysStore, webCryptoRootKeyName)
	if !webCryptoIsRootKey(root) || root.Get("extractable").Bool() {
		t.Fatal("the stored root is not a non-extractable AES-GCM key")
	}
	subtle, _ := webCryptoSubtle()
	if _, err := awaitPromise(subtle.Call("exportKey", "raw", root)); !errors.Is(err, errBrowserCall) {
		t.Errorf("exportKey answered %v", err)
	}
}

// Anything the store cannot vouch for is Failed, never Absent, and creates nothing.
func TestWebCryptoKeyStoreFailures(t *testing.T) {
	control := testFakeIndexedDb(t)
	name := testIndexedDbName(t)
	cases := []struct {
		name    string
		corrupt func()
	}{
		{name: "a ciphertext with a byte flipped", corrupt: func() {
			record := control.Call("raw", name, indexedDbKeysStore, webCryptoSecretPrefix+"w")
			ciphertext := record.Get("ciphertext")
			ciphertext.SetIndex(0, ciphertext.Index(0).Int()^0x01)
		}},
		{name: "a record moved to another name", corrupt: func() {
			record := control.Call("raw", name, indexedDbKeysStore, webCryptoSecretPrefix+"other")
			control.Call("setRaw", name, indexedDbKeysStore, webCryptoSecretPrefix+"w", record)
		}},
		{name: "a record that is not one", corrupt: func() {
			control.Call("setRaw", name, indexedDbKeysStore, webCryptoSecretPrefix+"w", "a string")
		}},
		{name: "a secret whose root key is gone", corrupt: func() {
			control.Call("setRaw", name, indexedDbKeysStore, webCryptoRootKeyName, js.Undefined())
		}},
		{name: "a root that is no CryptoKey", corrupt: func() {
			control.Call("setRaw", name, indexedDbKeysStore, webCryptoRootKeyName, bytesToJs([]byte("raw key bytes")))
		}},
		{name: "a read that fails", corrupt: func() {
			control.Set("readFault", "UnknownError")
		}},
	}
	for _, c := range cases {
		database, err := openIndexedDb(name)
		if err != nil {
			t.Fatal(err)
		}
		keyStore := newWebCryptoKeyStore(database)
		keyStore.PutSecret("w", []byte("w's bytes"))
		keyStore.PutSecret("other", []byte("other's bytes"))
		keysBefore := control.Call("keys", name, indexedDbKeysStore).Length()
		c.corrupt()
		if value, status := keyStore.GetSecret("w"); status != sidecar.SidecarKeyStoreFailed || value != nil {
			t.Errorf("%s: %q, %v", c.name, value, status)
		}
		if keysAfter := control.Call("keys", name, indexedDbKeysStore).Length(); keysAfter > keysBefore {
			t.Errorf("%s: a failed read created %d records", c.name, keysAfter-keysBefore)
		}
		control.Set("readFault", js.Null())
		control.Call("setRaw", name, indexedDbKeysStore, webCryptoRootKeyName, js.Undefined())
		control.Call("setRaw", name, indexedDbKeysStore, webCryptoSecretPrefix+"w", js.Undefined())
		control.Call("setRaw", name, indexedDbKeysStore, webCryptoSecretPrefix+"other", js.Undefined())
	}
	// a write the database refuses for quota is Failed
	database := testIndexedDb(t)
	keyStore := newWebCryptoKeyStore(database)
	control.Set("writeFault", "QuotaExceededError")
	if status := keyStore.PutSecret("w", []byte("w")); status != sidecar.SidecarKeyStoreFailed {
		t.Errorf("a quota error: %v", status)
	}
}

// Two first puts at once, as two calls of one Worker or two Workers racing, make one root key, which wraps both.
func TestWebCryptoFirstPutsMakeOneRootKey(t *testing.T) {
	control := testFakeIndexedDb(t)
	database := testIndexedDb(t)
	first, second := newWebCryptoKeyStore(database), newWebCryptoKeyStore(database)
	wait := sync.WaitGroup{}
	statuses := make([]sidecar.SidecarKeyStoreStatus, 2)
	for index, keyStore := range []*webCryptoKeyStore{first, second} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses[index] = keyStore.PutSecret("w"+string(rune('a'+index)), []byte{byte(index)})
		}()
	}
	wait.Wait()
	if statuses[0] != sidecar.SidecarKeyStoreOk || statuses[1] != sidecar.SidecarKeyStoreOk {
		t.Fatalf("puts: %v", statuses)
	}
	for index, keyStore := range []*webCryptoKeyStore{second, first} {
		for _, secretName := range []string{"wa", "wb"} {
			if _, status := keyStore.GetSecret(secretName); status != sidecar.SidecarKeyStorePresent {
				t.Errorf("store %d reads %s: %v", index, secretName, status)
			}
		}
	}
	keys := control.Call("keys", testIndexedDbName(t), indexedDbKeysStore)
	if keys.Length() != 3 {
		t.Errorf("%d records, want one root and two secrets", keys.Length())
	}
}

// Each network's database makes and keeps its own root key: a secret sealed in one network's database, moved into
// another's under the same name, opens there as Failed, never as the other's secret, and each opens its own.
func TestEachNetworkHasItsOwnRootKey(t *testing.T) {
	control := testFakeIndexedDb(t)
	prefix := testDatabaseNamePrefix(t)
	names := []string{indexedDbNetworkName(prefix, sidecar.NewId()), indexedDbNetworkName(prefix, sidecar.NewId())}
	keyStores := []*webCryptoKeyStore{}
	for index, name := range names {
		database, err := openIndexedDb(name)
		if err != nil {
			t.Fatal(err)
		}
		keyStore := newWebCryptoKeyStore(database)
		if status := keyStore.PutSecret("w", []byte{byte(index)}); status != sidecar.SidecarKeyStoreOk {
			t.Fatalf("network %d's put: %v", index, status)
		}
		if !webCryptoIsRootKey(control.Call("raw", name, indexedDbKeysStore, webCryptoRootKeyName)) {
			t.Fatalf("network %d's database holds no root key", index)
		}
		keyStores = append(keyStores, keyStore)
	}
	own := control.Call("raw", names[1], indexedDbKeysStore, webCryptoSecretPrefix+"w")
	control.Call("setRaw", names[1], indexedDbKeysStore, webCryptoSecretPrefix+"w", control.Call("raw", names[0], indexedDbKeysStore, webCryptoSecretPrefix+"w"))
	if value, status := keyStores[1].GetSecret("w"); status != sidecar.SidecarKeyStoreFailed || value != nil {
		t.Errorf("the other network's secret opened: %v, %v", value, status)
	}
	control.Call("setRaw", names[1], indexedDbKeysStore, webCryptoSecretPrefix+"w", own)
	for index, keyStore := range keyStores {
		if value, status := keyStore.GetSecret("w"); status != sidecar.SidecarKeyStorePresent || !bytes.Equal(value, []byte{byte(index)}) {
			t.Errorf("network %d reads its own secret as %v, %v", index, value, status)
		}
	}
}

// The instance's answers to the key store: a first run creates U; W absent with nothing recorded makes a fresh one,
// but with an identity recorded it is lost, never a new root; a store that fails is Failed and writes nothing.
func TestKeychainFollowsTheKeyStoreTable(t *testing.T) {
	control := testFakeIndexedDb(t)
	open := func(database *indexedDb, keyStore sidecar.SidecarKeyStore) (*sidecar.SidecarInstance, sidecar.SidecarInstanceState) {
		instance, err := newPageInstance(context.Background(), database, keyStore)
		if err != nil {
			t.Fatal(err)
		}
		instance.Unlock(nil)
		state, _ := instance.State()
		return instance, state
	}
	database := testIndexedDb(t)
	keyStore := newWebCryptoKeyStore(database)
	instance, state := open(database, keyStore)
	instance.Close()
	if state != sidecar.SidecarInstanceOpen {
		t.Fatalf("first run: %v", state)
	}
	// a network's keychain.wrap records an identity; W then goes missing
	backend := newIndexedDbStoreBackend(database)
	if err := testBackendWrite(backend, "net/00000000000000000000000000000001/keychain.wrap", "a recorded keychain's wrap"); err != nil {
		t.Fatal(err)
	}
	for _, key := range keyNames(control, testIndexedDbName(t)) {
		if key != webCryptoRootKeyName {
			control.Call("setRaw", testIndexedDbName(t), indexedDbKeysStore, key, js.Undefined())
		}
	}
	instance, state = open(database, keyStore)
	instance.Close()
	if state != sidecar.SidecarInstanceLost {
		t.Errorf("W absent with an identity recorded: %v", state)
	}
	// a key store read that fails: Failed, and nothing new on the medium
	filesBefore := len(keyNames(control, testIndexedDbName(t))) + control.Call("keys", testIndexedDbName(t), indexedDbFilesStore).Length()
	control.Set("readFault", "UnknownError")
	instance, state = open(database, keyStore)
	instance.Close()
	control.Set("readFault", js.Null())
	filesAfter := len(keyNames(control, testIndexedDbName(t))) + control.Call("keys", testIndexedDbName(t), indexedDbFilesStore).Length()
	if state != sidecar.SidecarInstanceFailed || filesAfter != filesBefore {
		t.Errorf("a failed key store: %v, %d records before and %d after", state, filesBefore, filesAfter)
	}
}

// The keys object store's record names.
func keyNames(control js.Value, databaseName string) []string {
	keys := control.Call("keys", databaseName, indexedDbKeysStore)
	names := []string{}
	for index := 0; index < keys.Length(); index += 1 {
		names = append(names, keys.Index(index).String())
	}
	return names
}

// The quick burner's store writes nothing: its instance opens with no IndexedDB at all.
func TestBurnerWritesNothing(t *testing.T) {
	control := testFakeIndexedDb(t)
	databaseCountBefore := control.Get("databases").Get("size").Int()
	instance, err := newBurnerInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.Unlock(nil); err != nil {
		t.Fatal(err)
	}
	if state, _ := instance.State(); state != sidecar.SidecarInstanceOpen {
		t.Errorf("the burner: %v", state)
	}
	if control.Get("databases").Get("size").Int() != databaseCountBefore {
		t.Error("the burner opened a database")
	}
}
