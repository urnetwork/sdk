//go:build js && wasm && sidecar_http

// The search cache's own IndexedDB object store and the page Worker's search on its own leg (SDK-M10-04), in Node under
// test/sidecarwasm_harness.mjs with the fake IndexedDB: the index cache's files land in the search store and every other
// file in files, a listing that reaches both reads both in key order, a version-1 database gains the store at its next
// open, the page Worker and the quick burner run their core's search as §18.4's browsers row says, and the search
// operations answer through JSON on this leg, unavailable while no engine runs. test/sidecarwasm_search.test.ts runs
// them.

package main

import (
	"slices"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// The keys of databaseName's store.
func testStoreKeys(control js.Value, databaseName string, store string) []string {
	keys := control.Call("keys", databaseName, store)
	names := []string{}
	for index := 0; index < keys.Length(); index += 1 {
		names = append(names, keys.Index(index).String())
	}
	slices.Sort(names)
	return names
}

// The index cache's files go to the search store, search.state and every other file to files; reads, listings, removals
// and appenders route alike, and a listing that reaches both stores merges them in key order.
func TestSearchCacheHasItsOwnStore(t *testing.T) {
	control := testFakeIndexedDb(t)
	database := testIndexedDb(t)
	databaseName := "test-" + t.Name()
	backend := newIndexedDbStoreBackend(database)
	networkId := sidecar.NewId()
	base := "net/" + networkId.String() + "/"
	for _, path := range []string{base + "search/cache/a.idx", base + "search/cache/b.idx", base + "search.state", base + "keychain.ur", "sidecar.json"} {
		if err := testBackendWrite(backend, path, "sealed "+path); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	if search := testStoreKeys(control, databaseName, indexedDbSearchStore); !slices.Equal(search, []string{base + "search/cache/a.idx", base + "search/cache/b.idx"}) {
		t.Errorf("the search store holds %v", search)
	}
	if files := testStoreKeys(control, databaseName, indexedDbFilesStore); !slices.Equal(files, []string{base + "keychain.ur", base + "search.state", "sidecar.json"}) {
		t.Errorf("the files store holds %v", files)
	}
	for _, path := range []string{base + "search/cache/a.idx", base + "search.state"} {
		if content, err := testBackendRead(backend, path); err != nil || content != "sealed "+path {
			t.Errorf("reading %s: %q %v", path, content, err)
		}
	}
	listingNames := func(prefix string) []string {
		entries, err := backend.List(prefix)
		if err != nil {
			t.Fatalf("listing %s: %v", prefix, err)
		}
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name)
		}
		return names
	}
	if all := listingNames(base); !slices.Equal(all, []string{base + "keychain.ur", base + "search.state", base + "search/cache/a.idx", base + "search/cache/b.idx"}) {
		t.Errorf("the network's listing: %v", all)
	}
	if search := listingNames(base + "search/"); !slices.Equal(search, []string{base + "search/cache/a.idx", base + "search/cache/b.idx"}) {
		t.Errorf("the search directory's listing: %v", search)
	}
	if partial := listingNames(base + "se"); len(partial) != 3 {
		t.Errorf("a listing ending mid-segment: %v", partial)
	}
	if err := backend.Remove(base + "search/cache/a.idx"); err != nil {
		t.Fatal(err)
	}
	if search := testStoreKeys(control, databaseName, indexedDbSearchStore); !slices.Equal(search, []string{base + "search/cache/b.idx"}) {
		t.Errorf("after a removal the search store holds %v", search)
	}
	appender, made, err := backend.OpenAppend(base + "search/journal")
	if err != nil || !made {
		t.Fatalf("an appender: %v %v", made, err)
	}
	appender.Write([]byte("entry"))
	if err := appender.Sync(); err != nil {
		t.Fatal(err)
	}
	appender.Close()
	if search := testStoreKeys(control, databaseName, indexedDbSearchStore); !slices.Contains(search, base+"search/journal") {
		t.Errorf("an appender's record is not in the search store: %v", search)
	}
	for path, want := range map[string]bool{
		base + "search/x": true, base + "search": false, base + "search.state": false, "net/search/x": false, "search/x": false, base + "searches/x": false,
	} {
		if got := indexedDbSearchPath(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// A database a Worker opened at version 1, with files and keys alone, gains the search store at its next open and keeps
// what it held.
func TestSearchStoreJoinsAVersionOneDatabase(t *testing.T) {
	control := testFakeIndexedDb(t)
	databaseName := "test-" + t.Name()
	request := js.Global().Get("indexedDB").Call("open", databaseName, 1)
	upgrade := js.FuncOf(func(this js.Value, args []js.Value) any {
		database := request.Get("result")
		database.Call("createObjectStore", indexedDbFilesStore)
		database.Call("createObjectStore", indexedDbKeysStore)
		return nil
	})
	request.Set("onupgradeneeded", upgrade)
	old, err := awaitRequest(request)
	upgrade.Release()
	if err != nil {
		t.Fatal(err)
	}
	first := &indexedDb{database: old}
	if err := first.put(indexedDbFilesStore, "sidecar.json", bytesToJs([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	// the fake keeps one connection a database, so the version-1 one is left open: a browser's upgrade waits for it to
	// close, which the Web Lock's hand-over does (worker.js)
	reopened, err := openIndexedDb(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	backend := newIndexedDbStoreBackend(reopened)
	if content, err := testBackendRead(backend, "sidecar.json"); err != nil || content != "{}" {
		t.Errorf("what version 1 held: %q %v", content, err)
	}
	path := "net/" + sidecar.NewId().String() + "/search/cache/a.idx"
	if err := testBackendWrite(backend, path, "x"); err != nil {
		t.Fatalf("a write to the new store: %v", err)
	}
	if search := testStoreKeys(control, databaseName, indexedDbSearchStore); !slices.Equal(search, []string{path}) {
		t.Errorf("the new search store holds %v", search)
	}
}

// The page Worker's core search runs as a browser's and the quick burner's as a burner's, and the search operations
// answer through JSON on this leg: unavailable while no host installs an engine, invalid when malformed, and a
// conditions report reaches the search with nothing open or open.
func TestPageWorkerSearchOnThisLeg(t *testing.T) {
	worker := testPageWorker(t)
	worker.conditions(&sidecarworker.WorkerConditions{Unmetered: true})
	if reply := testPageReply(t, worker, `{"id":"s","op":"search","query":"plan"}`); reply.Ok || reply.Code != sidecarworker.CodeUnavailable {
		t.Errorf("a search before open: %+v", reply)
	}
	testOpenNetwork(t, worker, sidecar.NewId())
	network := worker.current()
	search := network.settings.Search
	if search.CacheByteCount != 128*1024*1024 || search.AllPolicy != sidecar.SearchPolicyWifiCharging || search.RemoteAsk || search.MemoryByteCount != 8*1024*1024+256*1024 {
		t.Errorf("the page Worker's search settings: %+v", search)
	}
	worker.conditions(&sidecarworker.WorkerConditions{Unmetered: true, Charging: true, Foreground: true})
	if conditions, _ := network.local.SearchConditions(); !conditions.Reported || !conditions.Unmetered || !conditions.Charging {
		t.Errorf("the frame's conditions did not reach the core: %+v", conditions)
	}
	for _, request := range []string{
		`{"id":"s","op":"search","query":"plan","scope":"all"}`,
		`{"id":"st","op":"search_status"}`,
		`{"id":"p","op":"search_policy","policy":"never"}`,
	} {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeUnavailable {
			t.Errorf("%s on a network with no engine: %+v", request, reply)
		}
	}
	for _, request := range []string{
		`{"id":"m","op":"search_more"}`,
		`{"id":"x","op":"search","scope":"everything"}`,
		`{"id":"y","op":"search_policy","policy":"sometimes"}`,
	} {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeInvalid {
			t.Errorf("%s: %+v", request, reply)
		}
	}
	if reply := testPageReply(t, worker, `{"id":"c","op":"search_cancel","search_id":"0123456789abcdef0123456789abcdef"}`); !reply.Ok {
		t.Errorf("a cancel of a search no one holds: %+v", reply)
	}

	burner := testPageWorker(t)
	if _, err := burner.open(&pageOpenConfig{NetworkId: sidecar.NewId().String(), Urls: testPageServiceUrls(), Burner: true}); err != nil {
		t.Fatal(err)
	}
	if burnerSearch := burner.current().settings.Search; burnerSearch.CacheByteCount != 0 || burnerSearch.AllPolicy != sidecar.SearchPolicyNever {
		t.Errorf("the burner's search settings: %+v", burnerSearch)
	}
	if reply := testPageReply(t, burner, `{"id":"b","op":"search","query":"code","scope":"all"}`); reply.Ok || reply.Code != sidecarworker.CodeUnsupported {
		t.Errorf("a burner's search of all: %+v", reply)
	}
	burner.conditions(&sidecarworker.WorkerConditions{Charging: true})
}
