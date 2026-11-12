//go:build js && wasm && sidecar_http

// The page Worker's IndexedDB backend and host, in Node under test/sidecarwasm_harness.mjs, which installs the fake
// IndexedDB (test/fake_indexeddb.mjs) these tests arm faults on, and each network's database being its own instance
// (owner decision 217). test/sidecarwasm_store.test.ts builds and runs them.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The fake's control, failing a test run anywhere but the harness: a leg that did not run has not passed.
func testFakeIndexedDb(t *testing.T) js.Value {
	t.Helper()
	control := js.Global().Get("fakeIndexedDb")
	if control.Type() != js.TypeObject {
		t.Fatal("no fake IndexedDB: run under test/sidecarwasm_harness.mjs")
	}
	return control
}

// A database of the test's own, so tests share nothing.
func testIndexedDb(t *testing.T) *indexedDb {
	t.Helper()
	testFakeIndexedDb(t)
	database, err := openIndexedDb("test-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	return database
}

// The prefix a test's networks' databases are named under, so tests share nothing.
func testDatabaseNamePrefix(t *testing.T) string {
	return "test-" + t.Name() + "-"
}

// The instance id databaseName's instance document holds, as stored.
func testInstanceId(t *testing.T, control js.Value, databaseName string) string {
	t.Helper()
	encoded, ok := bytesFromJs(control.Call("raw", databaseName, indexedDbFilesStore, "sidecar.json"))
	if !ok {
		t.Fatalf("%s holds no instance document", databaseName)
	}
	document := struct {
		InstanceId string `json:"instance_id"`
	}{}
	if err := json.Unmarshal(encoded, &document); err != nil || document.InstanceId == "" {
		t.Fatalf("%s's instance document: %v", databaseName, err)
	}
	return document.InstanceId
}

// Every record of databaseName's object stores, by store and key, as the fake keeps them. A write replaces its
// record's object, so the same objects under the same keys mean nothing was written.
func testDatabaseRecords(control js.Value, databaseName string) map[string]js.Value {
	records := map[string]js.Value{}
	for _, store := range []string{indexedDbFilesStore, indexedDbKeysStore} {
		keys := control.Call("keys", databaseName, store)
		for index := 0; index < keys.Length(); index += 1 {
			key := keys.Index(index).String()
			records[store+"/"+key] = control.Call("raw", databaseName, store, key)
		}
	}
	return records
}

// Whether two snapshots hold the same records, object for object.
func testRecordsEqual(before map[string]js.Value, after map[string]js.Value) bool {
	if len(before) != len(after) {
		return false
	}
	for key, value := range before {
		if other, ok := after[key]; !ok || !value.Equal(other) {
			return false
		}
	}
	return true
}

// Loses databaseName alone, as indexedDB.deleteDatabase would: it leaves the bucket, and a connection still open on
// it makes no more transactions, as the fake's evict has it for every database.
func testLoseDatabase(control js.Value, databaseName string) {
	databases := control.Get("databases")
	control.Get("evicted").Call("add", databases.Call("get", databaseName))
	databases.Call("delete", databaseName)
}

// A network's record, as an enrollment's last step writes it.
func testNetworkRecord(networkId sidecar.Id) sidecar.SidecarNetworkRecord {
	return sidecar.SidecarNetworkRecord{
		NetworkId:         networkId,
		ClientId:          sidecar.NewId(),
		SidecarId:         sidecar.NewId(),
		ConnectInstanceId: sidecar.NewId(),
	}
}

// name's instance on a connection of its own, unlocked by its key store's wrap, with that connection.
func testOpenPageInstance(t *testing.T, name string) (*indexedDb, *sidecar.SidecarInstance) {
	t.Helper()
	database, err := openIndexedDb(name)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := newPageInstance(context.Background(), database, newWebCryptoKeyStore(database))
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Unlock(nil); err != nil {
		instance.Close()
		t.Fatal(err)
	}
	if state, _ := instance.State(); state != sidecar.SidecarInstanceOpen {
		instance.Close()
		t.Fatalf("%s's instance: %v", name, state)
	}
	return database, instance
}

// Writes content at path by the backend's steps.
func testBackendWrite(backend sidecar.SidecarStoreBackend, path string, content string) error {
	file, err := backend.CreateTemp()
	if err != nil {
		return err
	}
	defer file.Discard()
	if _, err := file.Write([]byte(content)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Rename(path); err != nil {
		return err
	}
	return backend.SyncDirectory(path)
}

// The bytes at path, or the error.
func testBackendRead(backend sidecar.SidecarStoreBackend, path string) (string, error) {
	reader, err := backend.Open(path)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	return string(content), err
}

func TestIndexedDbBackendWritesReadsListsAndRemoves(t *testing.T) {
	backend := newIndexedDbStoreBackend(testIndexedDb(t))
	for path, content := range map[string]string{
		"sidecar.json":          "{}",
		"net/a/keychain.ur":     "sealed a",
		"net/a/keychain.wrap":   "wrap a",
		"net/b/keychain.ur":     "sealed b",
		"net/a/search/shard-00": "kept out of this prefix's sort only by its name",
	} {
		if err := testBackendWrite(backend, path, content); err != nil {
			t.Fatal(err)
		}
	}
	if content, err := testBackendRead(backend, "net/a/keychain.ur"); err != nil || content != "sealed a" {
		t.Errorf("read %q, %v", content, err)
	}
	entries, err := backend.List("net/a/")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name)
		if entry.Name == "net/a/keychain.wrap" && entry.ByteCount != int64(len("wrap a")) {
			t.Errorf("%s counts %d bytes", entry.Name, entry.ByteCount)
		}
	}
	if !slices.Equal(names, []string{"net/a/keychain.ur", "net/a/keychain.wrap", "net/a/search/shard-00"}) {
		t.Errorf("listed %v", names)
	}
	if err := backend.Remove("net/a/keychain.ur"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Remove("net/a/keychain.ur"); err != nil {
		t.Errorf("removing what is not there: %v", err)
	}
	if _, err := testBackendRead(backend, "net/a/keychain.ur"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("a removed path reads %v", err)
	}
	if err := backend.RemoveTemp(); err != nil {
		t.Error(err)
	}
}

// Absent only when the database answers that nothing is there: a read that fails is Failed.
func TestIndexedDbAbsentIsNeverAFault(t *testing.T) {
	control := testFakeIndexedDb(t)
	backend := newIndexedDbStoreBackend(testIndexedDb(t))
	if _, err := testBackendRead(backend, "sidecar.json"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("nothing there: %v", err)
	}
	control.Set("readFault", "UnknownError")
	_, err := testBackendRead(backend, "sidecar.json")
	if !errors.Is(err, sidecar.ErrSidecarStoreFailed) || errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("a failed read: %v", err)
	}
}

// A write whose transaction dies before it commits, as a killed tab's does, leaves the old record whole.
func TestIndexedDbKilledWriteLeavesTheOldRecord(t *testing.T) {
	control := testFakeIndexedDb(t)
	backend := newIndexedDbStoreBackend(testIndexedDb(t))
	if err := testBackendWrite(backend, "sidecar.json", "old"); err != nil {
		t.Fatal(err)
	}
	control.Set("abortNextCommit", true)
	err := testBackendWrite(backend, "sidecar.json", "new, never committed")
	if !errors.Is(err, sidecar.ErrSidecarStoreFailed) {
		t.Errorf("the killed write answered %v", err)
	}
	if content, err := testBackendRead(backend, "sidecar.json"); err != nil || content != "old" {
		t.Errorf("after the kill: %q, %v", content, err)
	}
}

// A quota error is Failed, never Absent, and changes nothing.
func TestIndexedDbQuotaIsFailed(t *testing.T) {
	control := testFakeIndexedDb(t)
	backend := newIndexedDbStoreBackend(testIndexedDb(t))
	control.Set("writeFault", "QuotaExceededError")
	err := testBackendWrite(backend, "net/a/keychain.ur", "sealed")
	if !errors.Is(err, sidecar.ErrSidecarStoreFailed) || errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("a quota error answered %v", err)
	}
	if _, err := testBackendRead(backend, "net/a/keychain.ur"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("the refused write left %v", err)
	}
}

// An appender's writes are durable whole at each Sync, a truncate included, and nothing between.
func TestIndexedDbAppendSyncsWhole(t *testing.T) {
	control := testFakeIndexedDb(t)
	backend := newIndexedDbStoreBackend(testIndexedDb(t))
	appender, made, err := backend.OpenAppend("net/a/log")
	if err != nil || !made {
		t.Fatalf("%v, made %v", err, made)
	}
	appender.Write([]byte("one "))
	appender.Write([]byte("two "))
	if err := appender.Sync(); err != nil {
		t.Fatal(err)
	}
	appender.Write([]byte("three"))
	// the tab dies at this Sync: the record stays as of the last
	control.Set("abortNextCommit", true)
	if err := appender.Sync(); err == nil {
		t.Error("the killed sync answered nil")
	}
	if content, _ := testBackendRead(backend, "net/a/log"); content != "one two " {
		t.Errorf("after the killed sync: %q", content)
	}
	if err := appender.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if err := appender.Sync(); err != nil {
		t.Fatal(err)
	}
	appender.Close()
	reopened, made, err := backend.OpenAppend("net/a/log")
	if err != nil || made {
		t.Fatalf("%v, made %v", err, made)
	}
	if size, _ := reopened.Size(); size != 4 {
		t.Errorf("reopened at %d bytes", size)
	}
	if content, _ := testBackendRead(backend, "net/a/log"); content != "one " {
		t.Errorf("read %q", content)
	}
}

// A keychain opens on the IndexedDB backend and the WebCrypto key store, and a second instance on the same database,
// as the next load of the page, opens it again with no pass.
func TestKeychainOpensAndReopens(t *testing.T) {
	database := testIndexedDb(t)
	instance, err := newPageInstance(context.Background(), database, newWebCryptoKeyStore(database))
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Unlock(nil); err != nil {
		t.Fatal(err)
	}
	if state, _ := instance.State(); state != sidecar.SidecarInstanceOpen {
		t.Fatalf("first open: %v", state)
	}
	instance.Close()
	reopened, err := newPageInstance(context.Background(), database, newWebCryptoKeyStore(database))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Unlock(nil); err != nil {
		t.Fatal(err)
	}
	if state, _ := reopened.State(); state != sidecar.SidecarInstanceOpen {
		t.Errorf("reopen: %v", state)
	}
	host := &pageHost{instance: reopened, keyStore: newWebCryptoKeyStore(database)}
	if host.Store() == nil || host.KeyStore() == nil || host.TokenSource() != nil {
		t.Errorf("the host: %v, %v, %v", host.Store(), host.KeyStore(), host.TokenSource())
	}
}

// One network's database lost alone starts that network fresh and leaves the other network's instance as it was; the
// browser then clears the bucket whole, every network's database at once, and each network's next instance starts
// fresh. A database gone reads Absent with nothing recorded through a new connection, and Failed through the old
// one, never as nothing there.
func TestEvictedStoreStartsFresh(t *testing.T) {
	control := testFakeIndexedDb(t)
	prefix := testDatabaseNamePrefix(t)
	names := []string{indexedDbNetworkName(prefix, sidecar.NewId()), indexedDbNetworkName(prefix, sidecar.NewId())}
	connections := []*indexedDb{}
	instanceIds := []string{}
	for _, name := range names {
		database, instance := testOpenPageInstance(t, name)
		instance.Close()
		connections = append(connections, database)
		instanceIds = append(instanceIds, testInstanceId(t, control, name))
	}
	// the first network's database goes alone
	left := testDatabaseRecords(control, names[1])
	testLoseDatabase(control, names[0])
	if _, err := testBackendRead(newIndexedDbStoreBackend(connections[0]), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreFailed) {
		t.Errorf("the lost database's connection read %v", err)
	}
	fresh, err := openIndexedDb(names[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testBackendRead(newIndexedDbStoreBackend(fresh), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("the lost network's new database: %v", err)
	}
	_, instance := testOpenPageInstance(t, names[0])
	instance.Close()
	if instanceId := testInstanceId(t, control, names[0]); instanceId == instanceIds[0] {
		t.Error("the lost network's instance came back")
	} else {
		instanceIds[0] = instanceId
	}
	// the other network's is left: its connection still reads, and its instance reopens as it was by its own root key
	if !testRecordsEqual(left, testDatabaseRecords(control, names[1])) {
		t.Error("losing one network's database changed the other's")
	}
	if _, err := testBackendRead(newIndexedDbStoreBackend(connections[1]), "unlock.wrap"); err != nil {
		t.Errorf("the other network's connection read %v", err)
	}
	_, instance = testOpenPageInstance(t, names[1])
	instance.Close()
	if instanceId := testInstanceId(t, control, names[1]); instanceId != instanceIds[1] {
		t.Errorf("the other network's instance is %s, was %s", instanceId, instanceIds[1])
	}
	// the browser's eviction takes the bucket whole: both networks start fresh
	control.Call("evict")
	if _, err := testBackendRead(newIndexedDbStoreBackend(connections[1]), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreFailed) {
		t.Errorf("an evicted connection read %v", err)
	}
	for index, name := range names {
		next, err := openIndexedDb(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testBackendRead(newIndexedDbStoreBackend(next), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
			t.Errorf("network %d after the eviction: %v", index, err)
		}
		_, instance := testOpenPageInstance(t, name)
		instance.Close()
		if testInstanceId(t, control, name) == instanceIds[index] {
			t.Errorf("network %d's instance survived the eviction", index)
		}
	}
}

// Two networks' instances, each loaded before either writes, each record their own network, as an enrollment in one
// tab beside an enrollment in another does: both records stand, since each network's database holds its own instance
// document. Over one document shared by both, the second write would have dropped the first one's network
// (sdk-m4-04-instance-document-across-workers, owner decision 217).
func TestNetworkDatabasesKeepTheirOwnInstances(t *testing.T) {
	control := testFakeIndexedDb(t)
	prefix := testDatabaseNamePrefix(t)
	networkIds := []sidecar.Id{sidecar.NewId(), sidecar.NewId()}
	instances := []*sidecar.SidecarInstance{}
	for _, networkId := range networkIds {
		_, instance := testOpenPageInstance(t, indexedDbNetworkName(prefix, networkId))
		instances = append(instances, instance)
	}
	if testInstanceId(t, control, indexedDbNetworkName(prefix, networkIds[0])) == testInstanceId(t, control, indexedDbNetworkName(prefix, networkIds[1])) {
		t.Fatal("two networks' databases hold one instance")
	}
	for index, instance := range instances {
		if err := instance.RecordNetwork(testNetworkRecord(networkIds[index])); err != nil {
			t.Fatal(err)
		}
	}
	for _, instance := range instances {
		instance.Close()
	}
	for index, networkId := range networkIds {
		_, reopened := testOpenPageInstance(t, indexedDbNetworkName(prefix, networkId))
		networks := reopened.Networks()
		reopened.Close()
		if len(networks) != 1 || networks[0].NetworkId != networkId {
			t.Errorf("network %d's database records %+v", index, networks)
		}
	}
}

// The page Worker's instance takes this leg's settings, whose Argon2id cost is §5.9's wasm cost, 19 MiB, t=2, p=1, set
// by the root's settings_http.go alone.
func TestPageWorkerSettingsCarryTheWasmPassCost(t *testing.T) {
	keychain := sidecar.DefaultSidecarSettings().KeychainSettings
	if keychain.PassMemoryByteCount != 19*1024*1024 || keychain.PassIterationCount != 2 || keychain.PassThreadCount != 1 {
		t.Errorf("Argon2id at %d B, t=%d, p=%d", keychain.PassMemoryByteCount, keychain.PassIterationCount, keychain.PassThreadCount)
	}
}

// A Go call waiting on a promise hands the thread back: two timers the event loop runs while it waits both fire
// before the timer that resolves it.
func TestAwaitPromiseLeavesTheEventLoopRunning(t *testing.T) {
	ticks := 0
	tick := js.FuncOf(func(this js.Value, args []js.Value) any {
		ticks += 1
		return nil
	})
	defer tick.Release()
	var resolve js.Value
	executor := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve = args[0]
		return nil
	})
	defer executor.Release()
	promise := js.Global().Get("Promise").New(executor)
	settle := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve.Invoke(ticks)
		return nil
	})
	defer settle.Release()
	js.Global().Call("setTimeout", tick, 0)
	js.Global().Call("setTimeout", tick, 0)
	js.Global().Call("setTimeout", settle, 0)
	value, err := awaitPromise(promise)
	if err != nil || value.Int() != 2 {
		t.Errorf("resolved with %v ticks, %v", value, err)
	}
	// and a rejection answers as a failure with the error's name
	rejected := js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New("a value that must not surface"))
	if _, err := awaitPromise(rejected); !errors.Is(err, errBrowserCall) || bytes.Contains([]byte(err.Error()), []byte("must not surface")) {
		t.Errorf("a rejection answered %v", err)
	}
}
