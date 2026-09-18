package sdk

import "github.com/urnetwork/connect/v2026"

// One lifetime owner spans every client/provider generation and both egress
// NATs. Child capacities are overlapping permission ceilings, not standing
// reservations: idle provider capacity can be borrowed without forgetting
// owners still draining after a role change.
type deviceLocalTransferMemory struct {
	root        *connect.TransferMemoryBudget
	client      *connect.TransferMemoryBudget
	provider    *connect.TransferMemoryBudget
	nat         *connect.TransferMemoryBudget
	peerKeyPins *connect.TransferMemoryBudget

	clientShare   ByteCount
	providerShare ByteCount
}

func newDeviceLocalTransferMemoryForPlatform(settings *DeviceLocalSettings, mobile bool) *deviceLocalTransferMemory {
	if !mobileMemoryPolicyEnabledForPlatform(settings.MemoryTargetByteCount, mobile) {
		return nil
	}
	_, clientShare, _, providerShare := deviceMemoryShares(settings)
	root := connect.NewTransferMemoryBudget(clientShare + providerShare)
	memory := &deviceLocalTransferMemory{
		root:          root,
		client:        connect.NewTransferMemoryBudgetWithParent(clientShare+providerShare, root),
		provider:      connect.NewTransferMemoryBudgetWithParent(deviceLocalProviderIdleTransferByteCount, root),
		nat:           connect.NewTransferMemoryBudgetWithParent(providerShare/2, root),
		clientShare:   clientShare,
		providerShare: providerShare,
	}
	memory.peerKeyPins = connect.NewTransferMemoryBudgetWithParent(peerPinStoreMemoryByteCount, memory.client)
	return memory
}

const deviceLocalProviderIdleTransferByteCount ByteCount = (256 + 384) * 1024

func configureDeviceLocalClientMemoryForPlatform(settings *DeviceLocalSettings, memory *deviceLocalTransferMemory, mobile bool) {
	_, clientShare, _, _ := deviceMemoryShares(settings)
	var parent *connect.TransferMemoryBudget
	if memory != nil {
		parent = memory.client
	}
	client := &settings.ClientSettings
	client.SendBufferSettings.ResendQueueBudget, client.ReceiveBufferSettings.ReceiveQueueBudget =
		deviceLocalTransferBudgets(clientShare, parent)
	// The provider settings deliberately copy this same Pack pointer. The
	// shared handoff is one client-group owner, never a second provider pool.
	client.ReceiveBufferSettings.PackQueueBudget = parentDeviceLocalTransferBudget(
		mobilePackQueueBudgetForPlatform(settings.MemoryTargetByteCount, clientShare, mobile), parent,
	)
	applyMobileLowMemoryClientSettingsForPlatform(client, settings.MemoryTargetByteCount, mobile)
	if clientShare > 0 {
		client.WebRtcSettings.ReceiveBufferSize = deviceLocalP2pReceiveBufferByteCount
		client.WebRtcSettings.MemoryBudget = deviceLocalWebRtcBudget(clientShare, parent)
		client.WebRtcSettings.NetworkPeerMemoryBudget = parentDeviceLocalTransferBudget(
			client.WebRtcSettings.NetworkPeerMemoryBudget, parent,
		)
	}
}

func (self *deviceLocalTransferMemory) setProvideActive(active bool) {
	if self == nil {
		return
	}
	clientTotal := self.clientShare + self.providerShare
	providerTotal := deviceLocalProviderIdleTransferByteCount
	if active {
		clientTotal = self.clientShare
		providerTotal = max(providerTotal, self.providerShare/2)
	}
	// The order cannot overdraw the unchanged root. A shrunk child may keep
	// admitted owners until teardown; the enlarged sibling sees those exact
	// same bytes at the root and must wait for their release.
	self.client.SetTotalByteCount(clientTotal)
	self.provider.SetTotalByteCount(providerTotal)
	// Local-fallback egress is useful even when providing is off. Its child
	// stays live in both roles, and actual NAT ownership consumes the root.
}

func deviceLocalTransferBudgetWithParent(total ByteCount, parents ...*connect.TransferMemoryBudget) *connect.TransferMemoryBudget {
	var parent *connect.TransferMemoryBudget
	if len(parents) != 0 {
		parent = parents[0]
	}
	return connect.NewTransferMemoryBudgetWithParent(total, parent)
}

// Used only during single-threaded construction, before settings are handed
// to any client. Existing live budgets must never be reparented/replaced.
func parentDeviceLocalTransferBudget(budget, parent *connect.TransferMemoryBudget) *connect.TransferMemoryBudget {
	if budget == nil || parent == nil {
		return budget
	}
	return connect.NewTransferMemoryBudgetWithParent(budget.TotalByteCount(), parent)
}

func applyDeviceLocalTransferMemoryUsage(usage *DeviceLocalMemoryUsage, memory *deviceLocalTransferMemory, packQueue *connect.TransferMemoryBudget) {
	if memory == nil {
		return
	}
	// Pack is a subset of the client group. An earlier separate Pack read
	// can outlive its actual charge during a concurrent drain, falsely placing
	// it above the later root usage. Sample it and the pin child under the root's lock;
	// StatsWithDescendants also verifies that Pack belongs to that tree.
	stats := memory.root.StatsWithDescendants(memory.client, memory.provider, memory.nat, packQueue, memory.peerKeyPins)
	root, client, provider, nat, pack := stats[0], stats[1], stats[2], stats[3], stats[4]
	pins := stats[5]
	usage.PeerKeyPinBudgetByteCount = pins.TotalByteCount
	usage.PeerKeyPinUsedByteCount = pins.UsedByteCount
	usage.PeerKeyPinReservedByteCount = pins.ReservedByteCount
	usage.PeerKeyPinReleasedByteCount = pins.ReleasedByteCount
	usage.TransferRootBudgetByteCount = root.TotalByteCount
	usage.TransferRootUsedByteCount = root.UsedByteCount
	usage.TransferRootReservedByteCount = root.ReservedByteCount
	usage.TransferRootReleasedByteCount = root.ReleasedByteCount
	usage.ClientTransferBudgetByteCount = client.TotalByteCount
	usage.ClientTransferUsedByteCount = client.UsedByteCount
	usage.ProviderTransferBudgetByteCount = provider.TotalByteCount
	usage.ProviderTransferUsedByteCount = provider.UsedByteCount
	usage.NatBudgetByteCount = nat.TotalByteCount
	usage.NatUsedByteCount = nat.UsedByteCount
	usage.NatReservedByteCount = nat.ReservedByteCount
	usage.NatReleasedByteCount = nat.ReleasedByteCount
	usage.ClientReceiveByteCount += pack.UsedByteCount - usage.PackQueueUsedByteCount
	usage.PackQueueCapacityByteCount = pack.TotalByteCount
	usage.PackQueueUsedByteCount = pack.UsedByteCount
}
