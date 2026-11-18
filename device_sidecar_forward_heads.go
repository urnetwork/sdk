//go:build !js

// The forwarding role's config head reader (DESIGN-DETAILS 4.2.5's Compile, step 3; QD-814; SDK-M8-16): the root's
// reader over F (sidecar.ForwardingClient.ConfigHeads), its keys the generations the role's Forwarder keeps, opened by
// its forwarding reader key, and its pin in the role's sealed store. Every base names a head, base 1's included, so
// it is what lets the VPN component compile at all, with no sidecar role running. Every VPN component builds the same
// one: Android's VPN service, the desktop app services and, since the reader went on the ios_extension leg (QD-839,
// QD-973; owner decision 491), the iOS and macOS network extensions.

package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/sidecar"
)

// The head reader of role's Forwarder over client, its keys through running, the Forwarder once it is built: until
// then a key is not held, which withholds a read the Forwarder retries.
func deviceForwardConfigHeads(role *deviceLocalForwarding, client *sidecar.ForwardingClient, sidecarId sidecar.Id, configRepoId sidecar.Id, forwarderSettings *sidecar.ForwarderSettings, running *atomic.Pointer[sidecar.Forwarder]) (sidecar.ConfigHeadReader, error) {
	role.stateLock.Lock()
	store := role.forwarderStore
	role.stateLock.Unlock()
	heads, err := client.ConfigHeads(&sidecar.ForwardingConfigHeadsArgs{
		NetworkId:    sidecar.Id(role.networkId),
		SidecarId:    sidecarId,
		ConfigRepoId: configRepoId,
		RepoKeyAt: func(generation uint64) *sidecar.EvsRepoKey {
			if forwarder := running.Load(); forwarder != nil {
				return forwarder.RepoKeyAt(generation)
			}
			return nil
		},
		Store:    store,
		Settings: sidecar.DefaultForwardingConfigHeadsSettings(forwarderSettings),
	})
	if err != nil {
		return nil, err
	}
	return heads, nil
}
