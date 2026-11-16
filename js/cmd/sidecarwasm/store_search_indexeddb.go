//go:build js && wasm && sidecar_http

// The search cache's own IndexedDB object store (§18.8's storage row, D-SDK-24; SDK-M10-04): in each network's
// database, beside files and keys, a search store holds the index cache's files, the core's net/<network_id>/search/
// tree (state_search_cache.go's directory under state_dir.go's network tree), and nothing else. The core seals them
// as it seals every file, under the search class, so the medium holds only ciphertext; search.state, this device's
// policy overrides and notice answers, is §5.9's non-derived state and stays in files.
//
//	why apart   the cache is disposable by design: whatever it holds a sync fetches again, so it may be cleared,
//	            evicted or fail without touching the keychain, the MLS state or the token beside it
//	bound       Search.CacheByteCount, 128 MiB a network in the page Worker (sidecarworker's PageSearchSettings), which
//	            the core's index cache holds itself to (§5.9's bounds row)
//	writer      only the Worker holding the network's Web Lock opens its database (worker.js), so only it writes here,
//	            as for every other store (§18.8's instance row)
//	eviction    a browser clears an origin's storage bucket whole, so this store goes with the rest, search.state
//	            included, and the next Worker starts from the defaults
//
// The backend routes each path by its tree: Open, writes, Remove and appenders to the one store a path belongs to; a
// listing whose prefix can reach both reads both, merged in key order. The database's version 2 adds the store.

package main

import (
	"sort"
	"strings"
)

// The search cache's object store.
const indexedDbSearchStore = "search"

// The root's layout this routing follows: the network tree, and the search cache's directory under a network's
// (state_dir.go's networkTreeName, state_search_cache.go's sidecarSearchCacheDir).
const (
	indexedDbNetworkTreePrefix   = "net/"
	indexedDbSearchDirectoryName = "search/"
)

// Whether path lies in a network's search cache: net/<network_id>/search/....
func indexedDbSearchPath(path string) bool {
	rest, ok := strings.CutPrefix(path, indexedDbNetworkTreePrefix)
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, "/")
	return ok && strings.HasPrefix(after, indexedDbSearchDirectoryName)
}

// The object store path's record lives in.
func indexedDbStoreOf(path string) string {
	if indexedDbSearchPath(path) {
		return indexedDbSearchStore
	}
	return indexedDbFilesStore
}

// The object stores a listing of prefix reads: search alone for a prefix inside a search cache, both for one some
// search path starts with, files alone otherwise.
func indexedDbStoresListing(prefix string) []string {
	if indexedDbSearchPath(prefix) {
		return []string{indexedDbSearchStore}
	}
	if indexedDbPrefixReachesSearch(prefix) {
		return []string{indexedDbFilesStore, indexedDbSearchStore}
	}
	return []string{indexedDbFilesStore}
}

// Whether some search cache path starts with prefix: one shorter than a network's search directory that its every
// segment so far agrees with.
func indexedDbPrefixReachesSearch(prefix string) bool {
	if len(prefix) <= len(indexedDbNetworkTreePrefix) {
		return strings.HasPrefix(indexedDbNetworkTreePrefix, prefix)
	}
	rest, ok := strings.CutPrefix(prefix, indexedDbNetworkTreePrefix)
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, "/")
	if !ok {
		// still inside the network id
		return true
	}
	return strings.HasPrefix(indexedDbSearchDirectoryName, after)
}

// Every key starting with prefix in each store the prefix reaches, merged in key order.
func (self *indexedDb) listRouted(prefix string) ([]indexedDbListed, error) {
	stores := indexedDbStoresListing(prefix)
	if len(stores) == 1 {
		return self.list(stores[0], prefix)
	}
	merged := []indexedDbListed{}
	for _, store := range stores {
		listed, err := self.list(store, prefix)
		if err != nil {
			return nil, err
		}
		merged = append(merged, listed...)
	}
	sort.Slice(merged, func(i int, j int) bool {
		return merged[i].key < merged[j].key
	})
	return merged, nil
}
