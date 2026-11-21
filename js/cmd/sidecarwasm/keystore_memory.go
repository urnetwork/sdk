//go:build js && wasm && sidecar_http

// The quick burner's key store (§18.8, Q-SDK-4): memory only, so it writes nothing, to IndexedDB or anywhere, and what
// it holds dies with the Worker; a burner takes no Web Lock and enrolls anew on each load. It is the core's own
// in-memory SidecarKeyStore, beside an instance on the core's memory backend (§5.6's ephemeral keychain).

package main

import (
	"context"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/sdk/js/sidecarworker"
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

// Opens a fresh memory instance for an ephemeral client. Its key-origin document keeps
// the close capability and sends it on pagehide.
func (self *pageWorker) openBurner(networkId sidecar.Id, settings *sidecar.SidecarSettings) (*pageNetwork, error) {
	instance, err := newBurnerInstance(self.ctx)
	if err != nil {
		return nil, err
	}
	local, err := sidecar.NewSidecar(self.ctx, &pageHost{instance: instance}, settings)
	if err != nil {
		instance.Close()
		return nil, err
	}
	network := &pageNetwork{networkId: networkId, settings: settings, instance: instance, local: local, ephemeral: true}
	burner, err := sidecarworker.NewQuickBurner(&sidecarworker.WorkerArgs{
		Sidecar: local,
		Enroll:  network.enroll,
		Repo:    network.repo,
		Records: network.records,
		Search:  network.search,
		Yield:   yieldToEventLoop,
	}, &sidecarworker.QuickBurnerArgs{})
	if err != nil {
		local.Close()
		instance.Close()
		return nil, err
	}
	local.Unlock(nil)
	network.burner = burner
	return network, nil
}
