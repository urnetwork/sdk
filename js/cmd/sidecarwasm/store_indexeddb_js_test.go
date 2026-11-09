//go:build js && wasm && sidecar_http

// The page Worker's IndexedDB backend and host, in Node under test/sidecarwasm_harness.mjs, which installs the fake
// IndexedDB (test/fake_indexeddb.mjs) these tests arm faults on. test/sidecarwasm_store.test.ts builds and runs them.

package main

import (
	"bytes"
	"context"
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

// A browser clears the bucket whole: the next Worker's database is empty, reads Absent with nothing recorded, and its
// instance starts fresh.
func TestEvictedStoreStartsFresh(t *testing.T) {
	control := testFakeIndexedDb(t)
	database := testIndexedDb(t)
	instance, err := newPageInstance(context.Background(), database, newWebCryptoKeyStore(database))
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Unlock(nil); err != nil {
		t.Fatal(err)
	}
	instance.Close()
	control.Call("evict")
	// the old connection can do nothing, so a read through it fails rather than reading as nothing there
	if _, err := testBackendRead(newIndexedDbStoreBackend(database), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreFailed) {
		t.Errorf("the evicted connection read %v", err)
	}
	fresh := testIndexedDb(t)
	if _, err := testBackendRead(newIndexedDbStoreBackend(fresh), "unlock.wrap"); !errors.Is(err, sidecar.ErrSidecarStoreAbsent) {
		t.Errorf("after the eviction: %v", err)
	}
	next, err := newPageInstance(context.Background(), fresh, newWebCryptoKeyStore(fresh))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Unlock(nil); err != nil {
		t.Fatal(err)
	}
	if state, _ := next.State(); state != sidecar.SidecarInstanceOpen {
		t.Errorf("the fresh instance: %v", state)
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
