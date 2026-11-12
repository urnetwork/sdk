//go:build js && wasm && sidecar_http

// The quick burner's key store (§18.8, Q-SDK-4): memory only, so it writes nothing, to IndexedDB or anywhere, and what
// it holds dies with the Worker; a burner takes no Web Lock and enrolls anew on each load. It is the core's own
// in-memory SidecarKeyStore, beside an instance on the core's memory backend (§5.6's ephemeral keychain).

package main

import (
	"context"

	"github.com/urnetwork/sidecar"
)

// A key store that holds secrets in memory alone.
func newMemoryKeyStore() sidecar.SidecarKeyStore {
	return sidecar.NewSidecarMemoryKeyStore()
}

// A quick burner's instance: the core's memory backend and a memory key store, with this leg's settings, as the page
// Worker's (store_indexeddb.go's newPageInstance), so it writes nothing anywhere.
func newBurnerInstance(ctx context.Context) (*sidecar.SidecarInstance, error) {
	settings := sidecar.DefaultSidecarSettings()
	return sidecar.NewSidecarInstance(ctx, &sidecar.SidecarInstanceArgs{
		Backend:  sidecar.NewSidecarMemoryBackend(),
		KeyStore: newMemoryKeyStore(),
		Settings: settings,
	})
}
