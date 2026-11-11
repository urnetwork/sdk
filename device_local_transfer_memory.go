package sdk

import (
	"math"

	"github.com/urnetwork/connect"
)

// The measured 32/64 MiB profiles back the same encoded credit with retained
// decode ownership. Smaller/default Android and targetless profiles keep their
// existing calibration. These remain overlapping ceilings under the same root.
func deviceLocalReceiveBackingEnabled(target ByteCount, mobile bool) bool {
	return mobileMemoryPolicyEnabledForPlatform(target, mobile) && 32*1024*1024 <= target
}

func deviceLocalPhysicalReceiveBytes(logical ByteCount, target ByteCount, mobile bool) ByteCount {
	if !deviceLocalReceiveBackingEnabled(target, mobile) || logical <= 0 {
		return logical
	}
	if logical > math.MaxInt64/7*2 {
		return math.MaxInt64
	}
	// Ceil(7*logical/2), so the wire's floor(2*physical/7) preserves odd
	// logical capacities as well. No physical ownership is reserved here.
	return logical/2*7 + (logical%2*7+1)/2
}

func deviceLocalBackedH1ReceiveCount(target ByteCount) int {
	// 256 at32 MiB and512 at64 MiB. Keep a fixed512-slot ceiling for larger
	// targets rather than allowing a host-supplied target to size a huge channel.
	return int(min(target, ByteCount(64*1024*1024)) / (128 * 1024))
}

// Window construction only copies immutable policy. It never retunes the
// shared RX/Head/Pack budgets that older generations may still be using.
func copyDeviceLocalReceiveSizingToWindow(receive *connect.ReceiveBufferSettings, settings *DeviceLocalSettings) {
	if receive == nil || settings == nil || settings.ReceiveBufferSettings == nil ||
		!deviceLocalReceiveBackingEnabled(settings.MemoryTargetByteCount, settings.LowMemoryProfile) {
		return
	}
	source := settings.ReceiveBufferSettings
	receive.H1SequenceBufferSize = source.H1SequenceBufferSize
	receive.AdvertisedReceiveWindowBudgetScaleNumerator = source.AdvertisedReceiveWindowBudgetScaleNumerator
	receive.AdvertisedReceiveWindowBudgetScaleDenominator = source.AdvertisedReceiveWindowBudgetScaleDenominator
}

// One lifetime owner spans every client/provider generation and both egress
// NATs. Child capacities are overlapping permission ceilings, not standing
// reservations: idle provider capacity can be borrowed without forgetting
// owners still draining after a role change.
type deviceLocalTransferMemory struct {
	root                 *connect.TransferMemoryBudget
	client               *connect.TransferMemoryBudget
	provider             *connect.TransferMemoryBudget
	nat                  *connect.TransferMemoryBudget
	peerKeyPins          *connect.TransferMemoryBudget
	headRecovery         *connect.TransferMemoryBudget
	providerHeadRecovery *connect.TransferMemoryBudget

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
	// Construction exposes a stable empty child before clients receive their
	// effective RX ceiling. Configuration grants permission without reserving.
	memory.headRecovery = connect.NewTransferMemoryBudgetWithParent(0, memory.client)
	memory.providerHeadRecovery = connect.NewTransferMemoryBudgetWithParent(0, memory.provider)
	return memory
}

// The actual provider generation shares one device-owned recovery partition.
// It is a sibling of provider RX under the unchanged provider parent/root;
// permission is granted without reserving bytes or changing an ancestor cap.
func (self *deviceLocalTransferMemory) configureProviderHeadRecovery(settings *connect.ClientSettings) {
	if self == nil || settings == nil || settings.ReceiveBufferSettings == nil {
		return
	}
	receive := settings.ReceiveBufferSettings
	rx := receive.ReceiveQueueBudget
	if rx == nil || rx.Parent() != self.provider {
		return
	}
	if self.providerHeadRecovery == nil {
		self.providerHeadRecovery = connect.NewTransferMemoryBudgetWithParent(0, self.provider)
	}
	self.providerHeadRecovery.SetTotalByteCount(rx.TotalByteCount())
	receive.ReceiveHeadRecoveryBudget = self.providerHeadRecovery
}

const deviceLocalProviderIdleTransferByteCount ByteCount = (256 + 384) * 1024

func configureDeviceLocalClientMemoryForPlatform(settings *DeviceLocalSettings, memory *deviceLocalTransferMemory, mobile bool) {
	_, clientShare, _, _ := deviceMemoryShares(settings)
	var parent *connect.TransferMemoryBudget
	if memory != nil {
		parent = memory.client
	}
	client := &settings.ClientSettings
	configuredH1Count := client.ReceiveBufferSettings.H1SequenceBufferSize
	client.SendBufferSettings.ResendQueueBudget, client.ReceiveBufferSettings.ReceiveQueueBudget =
		deviceLocalTransferBudgets(clientShare, parent)
	// The provider settings deliberately copy this same Pack pointer. The
	// shared handoff is one client-group owner, never a second provider pool.
	client.ReceiveBufferSettings.PackQueueBudget = parentDeviceLocalTransferBudget(
		mobilePackQueueBudgetForPlatform(settings.MemoryTargetByteCount, clientShare, mobile), parent,
	)
	applyMobileLowMemoryClientSettingsForPlatform(client, settings.MemoryTargetByteCount, mobile)
	if memory != nil {
		receive := client.ReceiveBufferSettings
		if rx := receive.ReceiveQueueBudget; rx != nil && rx.Parent() == memory.client {
			headBytes := rx.TotalByteCount()
			if deviceLocalReceiveBackingEnabled(settings.MemoryTargetByteCount, mobile) {
				// Initial providing is off. Use the actual final client parent,
				// including its folded provider share, before publishing clients.
				headBytes = mobileReceiveQueueBudgetForPlatform(settings.MemoryTargetByteCount, memory.client.TotalByteCount(), mobile)
				rx.SetTotalByteCount(deviceLocalPhysicalReceiveBytes(headBytes, settings.MemoryTargetByteCount, mobile))
				receive.AdvertisedReceiveWindowBudgetScaleNumerator = 2
				receive.AdvertisedReceiveWindowBudgetScaleDenominator = 7
				count := deviceLocalBackedH1ReceiveCount(settings.MemoryTargetByteCount)
				if !settings.hasDefaultH1ReceiveCount || configuredH1Count != settings.defaultH1ReceiveCount {
					count = min(count, configuredH1Count)
				}
				receive.H1SequenceBufferSize = count
			}
			// One shared partition survives client generations and role changes.
			// Its ceiling is permission; only an admitted head charges the root.
			if memory.headRecovery == nil {
				memory.headRecovery = connect.NewTransferMemoryBudgetWithParent(headBytes, memory.client)
			} else {
				memory.headRecovery.SetTotalByteCount(headBytes)
			}
			receive.ReceiveHeadRecoveryBudget = memory.headRecovery
		}
	}
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
	stats := memory.root.StatsWithDescendants(memory.client, memory.provider, memory.nat, packQueue, memory.peerKeyPins, memory.headRecovery, memory.providerHeadRecovery)
	root, client, provider, nat, pack := stats[0], stats[1], stats[2], stats[3], stats[4]
	pins := stats[5]
	head := stats[6]
	providerHead := stats[7]
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
	usage.ClientReceiveByteCount += head.UsedByteCount - usage.ClientReceiveHeadRecoveryUsedByteCount
	usage.ClientReceiveHeadRecoveryBudgetByteCount = head.TotalByteCount
	usage.ClientReceiveHeadRecoveryUsedByteCount = head.UsedByteCount
	usage.ClientReceiveHeadRecoveryReservedByteCount = head.ReservedByteCount
	usage.ClientReceiveHeadRecoveryReleasedByteCount = head.ReleasedByteCount
	usage.ProviderReceiveByteCount += providerHead.UsedByteCount - usage.ProviderReceiveHeadRecoveryUsedByteCount
	usage.ProviderReceiveHeadRecoveryBudgetByteCount = providerHead.TotalByteCount
	usage.ProviderReceiveHeadRecoveryUsedByteCount = providerHead.UsedByteCount
	usage.ProviderReceiveHeadRecoveryReservedByteCount = providerHead.ReservedByteCount
	usage.ProviderReceiveHeadRecoveryReleasedByteCount = providerHead.ReleasedByteCount
	usage.PackQueueCapacityByteCount = pack.TotalByteCount
	usage.PackQueueUsedByteCount = pack.UsedByteCount
}
