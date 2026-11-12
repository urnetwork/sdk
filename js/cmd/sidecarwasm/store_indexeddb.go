//go:build js && wasm && sidecar_http

// The page Worker's host (§18.10): its SidecarHost, whose Store is the core's sealed store over IndexedDB, whose
// KeyStore is the non-extractable WebCrypto root key (keystore_webcrypto.go), and whose TokenSource is nil, so §5.6's
// refresher runs. Sealing stays the core's: this file brings only the medium, so the core's sealed files, keychain.ur,
// keychain.wrap and unlock.wrap run here unchanged (§5.9, CORE-M1-02), and the medium only ever holds ciphertext,
// sidecar.json aside.
//
// One database per network, urnetwork-sidecar-v1-<network_id>, each its own sidecar instance with its own instance
// document, sidecar.json, and its own WebCrypto root key (§18.8's storage row, owner decision 217), so two networks'
// Workers share nothing, and the network's Web Lock, which worker.js holds whenever the Worker has the network open
// (§18.8's instance row), covers everything a Worker writes. The name is the canonical text of the id the Worker
// parsed, never the string it was given, so one network has one database however a caller spelled its id. Nothing
// shipped on the unsuffixed urnetwork-sidecar-v1, so nothing is migrated from it and nothing opens it.
//
// Each database holds two object stores: files, keyed by the instance's paths, and keys, the key store's. Each
// backend write is one readwrite transaction with durability strict, which commits whole or not at all, so a killed
// tab leaves the old record or the new one (§22.9's no torn writes), and commits only once the browser has the bytes
// on its disk. The search cache gets its own object store with SDK-M10-04 (D-SDK-24), never this one.
//
// A browser clears an origin's storage bucket whole (§18.8's storage row), so an eviction takes every network's
// database at once, each one's files and key store together, and each reads as Absent with nothing recorded: the
// network's next Worker enrolls anew. A quota error, an abort or a database that will not open is Failed, never
// Absent.
//
// Every call here waits on the browser, so none may run inside a js.FuncOf callback (async_js.go). Safe for
// concurrent use from goroutines.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"syscall/js"

	"github.com/urnetwork/sidecar"
)

// The databases and their object stores.
const (
	// a network's database is this and the network id's canonical text
	indexedDbNamePrefix  = "urnetwork-sidecar-v1-"
	indexedDbVersion     = 1
	indexedDbFilesStore  = "files"
	indexedDbKeysStore   = "keys"
	indexedDbKeyRangeEnd = "￿"
)

// networkId's database under prefix, indexedDbNamePrefix or a test's own: the parsed id's canonical text, so no
// caller's spelling of the id ever names a database.
func indexedDbNetworkName(prefix string, networkId sidecar.Id) string {
	return prefix + networkId.String()
}

// An open IndexedDB database. Safe for concurrent use.
type indexedDb struct {
	database js.Value
}

// Opens the database, making its object stores the first time.
func openIndexedDb(name string) (*indexedDb, error) {
	factory := js.Global().Get("indexedDB")
	if factory.IsUndefined() || factory.IsNull() {
		return nil, fmt.Errorf("%w: no IndexedDB", errBrowserCall)
	}
	request := factory.Call("open", name, indexedDbVersion)
	upgrade := js.FuncOf(func(this js.Value, args []js.Value) any {
		// a versionchange transaction is open only during this event, so the stores are made here, without waiting
		database := request.Get("result")
		names := database.Get("objectStoreNames")
		for _, store := range []string{indexedDbFilesStore, indexedDbKeysStore} {
			if !names.Call("contains", store).Bool() {
				database.Call("createObjectStore", store)
			}
		}
		return nil
	})
	defer upgrade.Release()
	request.Set("onupgradeneeded", upgrade)
	database, err := awaitRequest(request)
	request.Set("onupgradeneeded", js.Null())
	if err != nil {
		return nil, err
	}
	return &indexedDb{database: database}, nil
}

// A transaction over one object store. readwrite ones ask for durability strict, so they commit once on the disk.
func (self *indexedDb) transaction(store string, readwrite bool) (js.Value, error) {
	mode := "readonly"
	options := js.Undefined()
	if readwrite {
		mode = "readwrite"
		options = js.ValueOf(map[string]any{"durability": "strict"})
	}
	var transaction js.Value
	var thrown error
	func() {
		defer func() {
			// a closed database or an unknown store throws, which syscall/js raises as a panic
			if recovered := recover(); recovered != nil {
				thrown = fmt.Errorf("%w: the transaction did not open", errBrowserCall)
			}
		}()
		transaction = self.database.Call("transaction", store, mode, options)
	}()
	return transaction, thrown
}

// The value under key, or undefined.
func (self *indexedDb) get(store string, key string) (js.Value, error) {
	transaction, err := self.transaction(store, false)
	if err != nil {
		return js.Undefined(), err
	}
	return awaitRequest(transaction.Call("objectStore", store).Call("get", key))
}

// Puts value under key in one transaction, returning once it committed.
func (self *indexedDb) put(store string, key string, value js.Value) error {
	transaction, err := self.transaction(store, true)
	if err != nil {
		return err
	}
	transaction.Call("objectStore", store).Call("put", value, key)
	return awaitTransaction(transaction)
}

// Adds value under key only if nothing is there, in one transaction: false when something was.
func (self *indexedDb) add(store string, key string, value js.Value) (bool, error) {
	transaction, err := self.transaction(store, true)
	if err != nil {
		return false, err
	}
	objectStore := transaction.Call("objectStore", store)
	request := objectStore.Call("count", key)
	answers := make(chan bool, 1)
	counted := js.FuncOf(func(this js.Value, args []js.Value) any {
		// still inside the transaction's event: add only where nothing is
		empty := request.Get("result").Int() == 0
		if empty {
			objectStore.Call("add", value, key)
		}
		select {
		case answers <- empty:
		default:
		}
		return nil
	})
	defer counted.Release()
	request.Set("onsuccess", counted)
	if err := awaitTransaction(transaction); err != nil {
		return false, err
	}
	select {
	case added := <-answers:
		return added, nil
	default:
		return false, fmt.Errorf("%w: the count never answered", errBrowserCall)
	}
}

// Deletes key in one transaction. Nothing there is not an error.
func (self *indexedDb) delete(store string, key string) error {
	transaction, err := self.transaction(store, true)
	if err != nil {
		return err
	}
	transaction.Call("objectStore", store).Call("delete", key)
	return awaitTransaction(transaction)
}

// One key a listing found, with its value's byte length.
type indexedDbListed struct {
	key       string
	byteCount int64
}

// Every key starting with prefix, in IndexedDB's key order, which for these ASCII paths is byte order.
func (self *indexedDb) list(store string, prefix string) ([]indexedDbListed, error) {
	transaction, err := self.transaction(store, false)
	if err != nil {
		return nil, err
	}
	keyRange := js.Global().Get("IDBKeyRange").Call("bound", prefix, prefix+indexedDbKeyRangeEnd)
	request := transaction.Call("objectStore", store).Call("openCursor", keyRange)
	steps := make(chan browserAnswer, 1)
	stepped := js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case steps <- browserAnswer{value: request.Get("result")}:
		default:
		}
		return nil
	})
	failed := js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case steps <- browserAnswer{err: browserError(request.Get("error"))}:
		default:
		}
		return nil
	})
	defer stepped.Release()
	defer failed.Release()
	request.Set("onsuccess", stepped)
	request.Set("onerror", failed)
	listed := []indexedDbListed{}
	for {
		// the goroutine this wakes runs before the event returns, so continue lands while the transaction is active
		step := <-steps
		if step.err != nil {
			return nil, step.err
		}
		cursor := step.value
		if cursor.IsNull() || cursor.IsUndefined() {
			return listed, nil
		}
		byteCount := int64(-1)
		if value := cursor.Get("value"); value.Type() == js.TypeObject {
			byteCount = int64(value.Get("byteLength").Int())
		}
		listed = append(listed, indexedDbListed{key: cursor.Get("key").String(), byteCount: byteCount})
		cursor.Call("continue")
	}
}

// The core's SidecarStoreBackend over the files object store. Temp files live in Go memory until their Rename, which
// is the one transaction that writes them, so nothing torn or temporary ever reaches IndexedDB.
type indexedDbStoreBackend struct {
	database *indexedDb
}

// A backend over database's files.
func newIndexedDbStoreBackend(database *indexedDb) *indexedDbStoreBackend {
	return &indexedDbStoreBackend{database: database}
}

// A browser failure as the core's Failed. Only the browser's error name survives, never a path (I-13).
func indexedDbFault(err error) error {
	return fmt.Errorf("%w: %v", sidecar.ErrSidecarStoreFailed, err)
}

// The bytes at path: Absent only when the database answers that nothing is there.
func (self *indexedDbStoreBackend) Open(path string) (io.ReadCloser, error) {
	value, err := self.database.get(indexedDbFilesStore, path)
	if err != nil {
		return nil, indexedDbFault(err)
	}
	if value.IsUndefined() {
		return nil, sidecar.ErrSidecarStoreAbsent
	}
	content, ok := bytesFromJs(value)
	if !ok {
		return nil, fmt.Errorf("%w: a record that is not bytes", sidecar.ErrSidecarStoreFailed)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// A buffer, which reaches IndexedDB at Rename.
func (self *indexedDbStoreBackend) CreateTemp() (sidecar.SidecarStoreBackendFile, error) {
	return &indexedDbStoreFile{backend: self}, nil
}

// A transaction is already whole and durable when it commits.
func (self *indexedDbStoreBackend) SyncDirectory(path string) error {
	return nil
}

// Deletes path.
func (self *indexedDbStoreBackend) Remove(path string) error {
	if err := self.database.delete(indexedDbFilesStore, path); err != nil {
		return indexedDbFault(err)
	}
	return nil
}

// Each path starting with prefix, sorted, with its byte count.
func (self *indexedDbStoreBackend) List(prefix string) ([]sidecar.SidecarStoreEntry, error) {
	listed, err := self.database.list(indexedDbFilesStore, prefix)
	if err != nil {
		return nil, indexedDbFault(err)
	}
	entries := []sidecar.SidecarStoreEntry{}
	for _, item := range listed {
		if !strings.HasPrefix(item.key, prefix) || item.byteCount < 0 {
			return nil, fmt.Errorf("%w: a record that is not bytes", sidecar.ErrSidecarStoreFailed)
		}
		entries = append(entries, sidecar.SidecarStoreEntry{Name: item.key, ByteCount: item.byteCount})
	}
	return entries, nil
}

// Temp files never reach IndexedDB, so a kill leaves none.
func (self *indexedDbStoreBackend) RemoveTemp() error {
	return nil
}

// An appender over path's record: each Sync writes the whole record in one transaction, so a kill leaves the record as
// of the last Sync, which state_log.go reads as a log with no torn tail.
func (self *indexedDbStoreBackend) OpenAppend(path string) (sidecar.SidecarStoreBackendAppendFile, bool, error) {
	value, err := self.database.get(indexedDbFilesStore, path)
	if err != nil {
		return nil, false, indexedDbFault(err)
	}
	made := value.IsUndefined()
	content := []byte{}
	if !made {
		var ok bool
		content, ok = bytesFromJs(value)
		if !ok {
			return nil, false, fmt.Errorf("%w: a record that is not bytes", sidecar.ErrSidecarStoreFailed)
		}
	} else if err := self.database.put(indexedDbFilesStore, path, bytesToJs(content)); err != nil {
		return nil, false, indexedDbFault(err)
	}
	return &indexedDbAppendFile{backend: self, path: path, content: content}, made, nil
}

// A file not yet renamed in.
type indexedDbStoreFile struct {
	backend *indexedDbStoreBackend
	buffer  bytes.Buffer
	done    bool
}

// Buffers p.
func (self *indexedDbStoreFile) Write(p []byte) (int, error) {
	if self.done {
		return 0, sidecar.ErrSidecarStoreFailed
	}
	return self.buffer.Write(p)
}

// The bytes become durable with the transaction at Rename.
func (self *indexedDbStoreFile) Sync() error {
	return nil
}

// Writes the whole file at path in one transaction.
func (self *indexedDbStoreFile) Rename(path string) error {
	if self.done {
		return sidecar.ErrSidecarStoreFailed
	}
	self.done = true
	if err := self.backend.database.put(indexedDbFilesStore, path, bytesToJs(self.buffer.Bytes())); err != nil {
		return indexedDbFault(err)
	}
	return nil
}

// Drops the buffer.
func (self *indexedDbStoreFile) Discard() error {
	self.done = true
	self.buffer.Reset()
	return nil
}

// An appender holding the record as last read or synced, plus what was written since.
type indexedDbAppendFile struct {
	backend *indexedDbStoreBackend
	path    string
	content []byte
	closed  bool
}

// Appends p in memory, durable at the next Sync.
func (self *indexedDbAppendFile) Write(p []byte) (int, error) {
	if self.closed {
		return 0, sidecar.ErrSidecarStoreFailed
	}
	self.content = append(self.content, p...)
	return len(p), nil
}

// The record's length with what was written since.
func (self *indexedDbAppendFile) Size() (int64, error) {
	return int64(len(self.content)), nil
}

// Cuts to byteCount, durable at the next Sync.
func (self *indexedDbAppendFile) Truncate(byteCount int64) error {
	if self.closed || byteCount < 0 || int64(len(self.content)) < byteCount {
		return sidecar.ErrSidecarStoreFailed
	}
	self.content = self.content[:byteCount]
	return nil
}

// Writes the whole record in one transaction.
func (self *indexedDbAppendFile) Sync() error {
	if self.closed {
		return sidecar.ErrSidecarStoreFailed
	}
	if err := self.backend.database.put(indexedDbFilesStore, self.path, bytesToJs(self.content)); err != nil {
		return indexedDbFault(err)
	}
	return nil
}

// Ends the appender.
func (self *indexedDbAppendFile) Close() error {
	self.closed = true
	return nil
}

// The page Worker's SidecarHost for one network: its instance's sealed store, the WebCrypto key store and no
// TokenSource, so §5.6's sidecarTokenManager refreshes.
type pageHost struct {
	instance  *sidecar.SidecarInstance
	networkId *sidecar.Id
	keyStore  sidecar.SidecarKeyStore
}

// The instance's store for this network, sealed by the core over IndexedDB.
func (self *pageHost) Store() sidecar.SidecarStore {
	return self.instance.Store(self.networkId)
}

// The WebCrypto key store, or the quick burner's memory one.
func (self *pageHost) KeyStore() sidecar.SidecarKeyStore {
	return self.keyStore
}

// None: the core's refresher runs (§18.10).
func (self *pageHost) TokenSource() sidecar.SidecarTokenSource {
	return nil
}

// An instance on database, one network's, and keyStore, over the same database, with this leg's settings, whose
// defaults carry §5.9's wasm cost for Argon2id, 19 MiB, t=2, p=1, since js/wasm runs one thread and a Worker has a
// phone's memory, not a desktop's (the root's settings_http.go, its one source). The caller opens database once, with
// openIndexedDb, and shares it with the key store; a database that will not open is an error there, never an empty
// instance, so a Worker never enrolls anew over a store it merely failed to read.
func newPageInstance(ctx context.Context, database *indexedDb, keyStore sidecar.SidecarKeyStore) (*sidecar.SidecarInstance, error) {
	settings := sidecar.DefaultSidecarSettings()
	return sidecar.NewSidecarInstance(ctx, &sidecar.SidecarInstanceArgs{
		Backend:  newIndexedDbStoreBackend(database),
		KeyStore: keyStore,
		Settings: settings,
	})
}
