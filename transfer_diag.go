package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

// transferDiagLogSeconds enables the periodic transfer diagnostic log line.
// It is a build-time seam for the physical p2p rig (connect/FLIGHTGATEFIX.md
// §8, §10 Phase 4): set with
//
//	-ldflags "-X github.com/urnetwork/sdk.transferDiagLogSeconds=2"
//
// and every interval the device logs one `[flightgate] {...}` JSON line with
// the provider client's send-recovery and receive counters, every window
// client's counters, the shared p2p data-plane counters, and the connect
// state. Empty (the default, and every release build) logs nothing and
// allocates nothing: the data-plane stats object is only created when the
// seam is on.
var transferDiagLogSeconds string = ""

var transferDiagnosticSnapshotsEnabled atomic.Bool

// SetTransferDiagnosticSnapshotsEnabled opts subsequently constructed devices
// into acceptance diagnostics and returns the previous setting. It installs
// only the real P2P counters, not a ticker, logger, or snapshot collector. Call
// before creating the test device, then restore the previous value on teardown.
// Existing devices are deliberately unaffected. Ordinary applications leave
// this false; their data plane allocates and records no diagnostic counters.
func SetTransferDiagnosticSnapshotsEnabled(enabled bool) bool {
	return transferDiagnosticSnapshotsEnabled.Swap(enabled)
}

func prepareTransferDiag(clientSettings *connect.ClientSettings) *connect.P2pDataPlaneStats {
	if !transferDiagnosticSnapshotsEnabled.Load() && transferDiagInterval() <= 0 {
		return nil
	}
	stats := &connect.P2pDataPlaneStats{}
	attachTransferDiagStats(clientSettings, stats)
	return stats
}

func transferDiagInterval() time.Duration {
	if transferDiagLogSeconds == "" {
		return 0
	}
	seconds, err := strconv.Atoi(transferDiagLogSeconds)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// Android's logcat truncates a message near 4 KiB, so one sample is logged
// as several lines that share unix_millis: "state" (connect state and the
// shared p2p counters), "provider_send", "provider_receive", and one
// "window_send" and "window_receive" per window client. The rig tool
// (connect/tools/flightgate-devices) rejoins them by unix_millis.
type transferDiagState struct {
	Part                string                            `json:"part"`
	UnixMillis          int64                             `json:"unix_millis"`
	ConnectEnabled      bool                              `json:"connect_enabled"`
	ProvideMode         ProvideMode                       `json:"provide_mode"`
	LocationNetworkPeer bool                              `json:"location_network_peer"`
	LocationIsDevice    bool                              `json:"location_is_device"`
	LocationName        string                            `json:"location_name,omitempty"`
	ClientID            string                            `json:"client_id"`
	LocationClientID    string                            `json:"location_client_id,omitempty"`
	QualityClientCount  int                               `json:"quality_client_count"`
	SpeedClientCount    int                               `json:"speed_client_count"`
	WindowClientCount   int                               `json:"window_client_count"`
	P2p                 connect.P2pDataPlaneStatsSnapshot `json:"p2p"`
}

// transferDiagMemory is the Go runtime view the MEMSTEADY gate judges
// (goRuntimeBytes is go_total_bytes), read from the mobile sampler's reader
// at the diagnostic interval instead of the sampler's own 15 s.
type transferDiagMemory struct {
	Part                           string `json:"part"`
	UnixMillis                     int64  `json:"unix_millis"`
	GoTotalByteCount               int64  `json:"go_total_bytes"`
	GoLiveByteCount                int64  `json:"go_live_bytes"`
	GoGoalByteCount                int64  `json:"go_goal_bytes"`
	GoLimitByteCount               int64  `json:"go_limit_bytes"`
	DeviceMemoryTargetByteCount    int64  `json:"device_memory_target_bytes"`
	PhysicalByteCount              int64  `json:"physical_bytes"`
	GoroutineCount                 int64  `json:"goroutines"`
	PoolOutstandingCount           int64  `json:"pool_outstanding"`
	PacketPoolOutstandingByteCount int64  `json:"packet_pool_outstanding_bytes"`
	PoolRetainedByteCount          int64  `json:"pool_retained_bytes"`
	PacketPoolRetainedByteCount    int64  `json:"packet_pool_retained_bytes"`
	PoolCapacityByteCount          int64  `json:"pool_capacity_bytes"`
	transferDiagProcessCarrierBudget
	IdleReclaimCount  int64 `json:"idle_reclaim_count"`
	ForcedGCCount     int64 `json:"forced_gc_count"`
	GCCycleCount      int64 `json:"gc_cycles"`
	WindowClientCount int   `json:"window_client_count"`
}

type transferDiagProcessCarrierBudget struct {
	TransportBudgetTotalByteCount        int64  `json:"transport_budget_total_bytes"`
	TransportBudgetUsedByteCount         int64  `json:"transport_budget_used_bytes"`
	TransportBudgetMaxCount              int64  `json:"transport_budget_max_count"`
	TransportBudgetUsedCount             int64  `json:"transport_budget_used_count"`
	TransportBudgetPendingH1Count        int64  `json:"transport_budget_pending_h1"`
	TransportBudgetPendingH1Bytes        int64  `json:"transport_budget_pending_h1_bytes"`
	TransportBudgetReservedBytes         int64  `json:"transport_budget_reserved_bytes"`
	TransportBudgetReleasedBytes         int64  `json:"transport_budget_released_bytes"`
	TransportBudgetHandoffCount          int64  `json:"transport_budget_active_handoff_count"`
	TransportBudgetHandoffBytes          int64  `json:"transport_budget_active_handoff_bytes"`
	TransportBudgetHandoffSlots          int64  `json:"transport_budget_active_handoff_slots"`
	TransportBudgetHandoffID             uint64 `json:"transport_budget_active_handoff_id"`
	TransportBudgetHandoffFrom           string `json:"transport_budget_active_handoff_from"`
	TransportBudgetHandoffTo             string `json:"transport_budget_active_handoff_to"`
	TransportBudgetHandoffH1Bytes        int64  `json:"transport_budget_active_handoff_h1_bytes"`
	TransportBudgetPairID                uint64 `json:"transport_budget_pair_id"`
	TransportBudgetPairFrom              string `json:"transport_budget_pair_from"`
	TransportBudgetPairTo                string `json:"transport_budget_pair_to"`
	TransportBudgetPairH1Bytes           int64  `json:"transport_budget_pair_h1_bytes"`
	TransportBudgetPairBytes             int64  `json:"transport_budget_pair_bytes"`
	TransportBudgetPairSlots             int64  `json:"transport_budget_pair_slots"`
	TransportBudgetPairOwner             string `json:"transport_budget_pair_owner"`
	TransportBudgetAdditionalPairID      uint64 `json:"transport_budget_additional_pair_id"`
	TransportBudgetAdditionalPairFrom    string `json:"transport_budget_additional_pair_from"`
	TransportBudgetAdditionalPairTo      string `json:"transport_budget_additional_pair_to"`
	TransportBudgetAdditionalPairH1Bytes int64  `json:"transport_budget_additional_pair_h1_bytes"`
	TransportBudgetAdditionalPairBytes   int64  `json:"transport_budget_additional_pair_bytes"`
	TransportBudgetAdditionalPairSlots   int64  `json:"transport_budget_additional_pair_slots"`
	TransportBudgetAdditionalPairOwner   string `json:"transport_budget_additional_pair_owner"`
}

// Keep the device half in a second memory part below Android's logcat line
// limit. Both parts use one atomic budget snapshot and the same timestamp;
// the harness joins them before validating either limit.
type transferDiagDeviceCarrierBudget struct {
	Part                                 string `json:"part"`
	UnixMillis                           int64  `json:"unix_millis"`
	TransportBudgetTotalByteCount        int64  `json:"device_transport_budget_total_bytes"`
	TransportBudgetUsedByteCount         int64  `json:"device_transport_budget_used_bytes"`
	TransportBudgetMaxCount              int64  `json:"device_transport_budget_max_count"`
	TransportBudgetUsedCount             int64  `json:"device_transport_budget_used_count"`
	TransportBudgetPendingH1Count        int64  `json:"device_transport_budget_pending_h1"`
	TransportBudgetPendingH1Bytes        int64  `json:"device_transport_budget_pending_h1_bytes"`
	TransportBudgetReservedBytes         int64  `json:"device_transport_budget_reserved_bytes"`
	TransportBudgetReleasedBytes         int64  `json:"device_transport_budget_released_bytes"`
	TransportBudgetHandoffCount          int64  `json:"device_transport_budget_active_handoff_count"`
	TransportBudgetHandoffBytes          int64  `json:"device_transport_budget_active_handoff_bytes"`
	TransportBudgetHandoffSlots          int64  `json:"device_transport_budget_active_handoff_slots"`
	TransportBudgetHandoffID             uint64 `json:"device_transport_budget_active_handoff_id"`
	TransportBudgetHandoffFrom           string `json:"device_transport_budget_active_handoff_from"`
	TransportBudgetHandoffTo             string `json:"device_transport_budget_active_handoff_to"`
	TransportBudgetHandoffH1Bytes        int64  `json:"device_transport_budget_active_handoff_h1_bytes"`
	TransportBudgetPairID                uint64 `json:"device_transport_budget_pair_id"`
	TransportBudgetPairFrom              string `json:"device_transport_budget_pair_from"`
	TransportBudgetPairTo                string `json:"device_transport_budget_pair_to"`
	TransportBudgetPairH1Bytes           int64  `json:"device_transport_budget_pair_h1_bytes"`
	TransportBudgetPairBytes             int64  `json:"device_transport_budget_pair_bytes"`
	TransportBudgetPairSlots             int64  `json:"device_transport_budget_pair_slots"`
	TransportBudgetPairOwner             string `json:"device_transport_budget_pair_owner"`
	TransportBudgetAdditionalPairID      uint64 `json:"device_transport_budget_additional_pair_id"`
	TransportBudgetAdditionalPairFrom    string `json:"device_transport_budget_additional_pair_from"`
	TransportBudgetAdditionalPairTo      string `json:"device_transport_budget_additional_pair_to"`
	TransportBudgetAdditionalPairH1Bytes int64  `json:"device_transport_budget_additional_pair_h1_bytes"`
	TransportBudgetAdditionalPairBytes   int64  `json:"device_transport_budget_additional_pair_bytes"`
	TransportBudgetAdditionalPairSlots   int64  `json:"device_transport_budget_additional_pair_slots"`
	TransportBudgetAdditionalPairOwner   string `json:"device_transport_budget_additional_pair_owner"`
}

// Separate from carrier/runtime parts so logcat never truncates the shared
// transfer ownership proof. Root and role/NAT groups are one coherent budget
// snapshot; Pack is a diagnostic subset of client, not an additive owner.
type transferDiagDeviceTransferBudget struct {
	Part                   string `json:"part"`
	UnixMillis             int64  `json:"unix_millis"`
	RootTotalByteCount     int64  `json:"transfer_root_total_bytes"`
	RootUsedByteCount      int64  `json:"transfer_root_used_bytes"`
	RootReservedByteCount  int64  `json:"transfer_root_reserved_bytes"`
	RootReleasedByteCount  int64  `json:"transfer_root_released_bytes"`
	ClientTotalByteCount   int64  `json:"client_transfer_total_bytes"`
	ClientUsedByteCount    int64  `json:"client_transfer_used_bytes"`
	ProviderTotalByteCount int64  `json:"provider_transfer_total_bytes"`
	ProviderUsedByteCount  int64  `json:"provider_transfer_used_bytes"`
	NatTotalByteCount      int64  `json:"nat_budget_total_bytes"`
	NatUsedByteCount       int64  `json:"nat_budget_used_bytes"`
	NatReservedByteCount   int64  `json:"nat_budget_reserved_bytes"`
	NatReleasedByteCount   int64  `json:"nat_budget_released_bytes"`
	PackTotalByteCount     int64  `json:"pack_queue_total_bytes"`
	PackUsedByteCount      int64  `json:"pack_queue_used_bytes"`
	PinTotalByteCount      int64  `json:"peer_pin_total_bytes"`
	PinUsedByteCount       int64  `json:"peer_pin_used_bytes"`
	PinReservedByteCount   int64  `json:"peer_pin_reserved_bytes"`
	PinReleasedByteCount   int64  `json:"peer_pin_released_bytes"`
	PinCount               int    `json:"peer_pin_count"`
	PinCapacityRefusals    int64  `json:"peer_pin_capacity_refusals"`
	PinPersistenceFailures int64  `json:"peer_pin_persistence_failures"`
	PinRollbackRefusals    int64  `json:"peer_pin_rollback_refusals"`
	PinStateFailures       int64  `json:"peer_pin_state_failures"`
}

func transferDiagTransferBudget(usage *DeviceLocalMemoryUsage, millis int64) transferDiagDeviceTransferBudget {
	return transferDiagDeviceTransferBudget{
		Part: "memory_device_transfer", UnixMillis: millis,
		RootTotalByteCount:     usage.TransferRootBudgetByteCount,
		RootUsedByteCount:      usage.TransferRootUsedByteCount,
		RootReservedByteCount:  usage.TransferRootReservedByteCount,
		RootReleasedByteCount:  usage.TransferRootReleasedByteCount,
		ClientTotalByteCount:   usage.ClientTransferBudgetByteCount,
		ClientUsedByteCount:    usage.ClientTransferUsedByteCount,
		ProviderTotalByteCount: usage.ProviderTransferBudgetByteCount,
		ProviderUsedByteCount:  usage.ProviderTransferUsedByteCount,
		NatTotalByteCount:      usage.NatBudgetByteCount,
		NatUsedByteCount:       usage.NatUsedByteCount,
		NatReservedByteCount:   usage.NatReservedByteCount,
		NatReleasedByteCount:   usage.NatReleasedByteCount,
		PackTotalByteCount:     usage.PackQueueCapacityByteCount,
		PackUsedByteCount:      usage.PackQueueUsedByteCount,
		PinTotalByteCount:      usage.PeerKeyPinBudgetByteCount,
		PinUsedByteCount:       usage.PeerKeyPinUsedByteCount,
		PinReservedByteCount:   usage.PeerKeyPinReservedByteCount,
		PinReleasedByteCount:   usage.PeerKeyPinReleasedByteCount,
		PinCount:               usage.PeerKeyPinCount,
		PinCapacityRefusals:    usage.PeerKeyPinCapacityRefusals,
		PinPersistenceFailures: usage.PeerKeyPinPersistenceFailures,
		PinRollbackRefusals:    usage.PeerKeyPinRollbackRefusals,
		PinStateFailures:       usage.PeerKeyPinStateFailures,
	}
}

func transferDiagCarrierBudgets(
	snapshot connect.PlatformTransportBudgetHierarchyStats,
	millis int64,
) (transferDiagProcessCarrierBudget, transferDiagDeviceCarrierBudget) {
	root, child := snapshot.Root, snapshot.Budget
	rootPair, childPair := snapshot.RootHandoff, snapshot.BudgetHandoff
	rootAdditional, childAdditional := snapshot.RootAdditionalHandoff, snapshot.BudgetAdditionalHandoff
	return transferDiagProcessCarrierBudget{
			TransportBudgetTotalByteCount:        int64(root.TotalByteCount),
			TransportBudgetUsedByteCount:         int64(root.UsedByteCount),
			TransportBudgetMaxCount:              int64(root.MaxTransportCount),
			TransportBudgetUsedCount:             int64(root.UsedTransportCount),
			TransportBudgetPendingH1Count:        int64(root.PendingH1Count),
			TransportBudgetPendingH1Bytes:        int64(root.PendingH1ByteCount),
			TransportBudgetReservedBytes:         int64(root.ReservedByteCount),
			TransportBudgetReleasedBytes:         int64(root.ReleasedByteCount),
			TransportBudgetHandoffCount:          int64(root.ActiveHandoffCount),
			TransportBudgetHandoffBytes:          int64(root.ActiveHandoffByteCount),
			TransportBudgetHandoffSlots:          int64(root.ActiveHandoffTransportCount),
			TransportBudgetHandoffID:             root.ActiveHandoffID,
			TransportBudgetHandoffFrom:           root.ActiveHandoffFromClass,
			TransportBudgetHandoffTo:             root.ActiveHandoffToClass,
			TransportBudgetHandoffH1Bytes:        int64(root.ActiveHandoffH1ByteCount),
			TransportBudgetPairID:                rootPair.ID,
			TransportBudgetPairFrom:              rootPair.FromClass,
			TransportBudgetPairTo:                rootPair.ToClass,
			TransportBudgetPairH1Bytes:           int64(rootPair.H1ByteCount),
			TransportBudgetPairBytes:             int64(rootPair.ByteCount),
			TransportBudgetPairSlots:             int64(rootPair.TransportCount),
			TransportBudgetPairOwner:             rootPair.Owner,
			TransportBudgetAdditionalPairID:      rootAdditional.ID,
			TransportBudgetAdditionalPairFrom:    rootAdditional.FromClass,
			TransportBudgetAdditionalPairTo:      rootAdditional.ToClass,
			TransportBudgetAdditionalPairH1Bytes: int64(rootAdditional.H1ByteCount),
			TransportBudgetAdditionalPairBytes:   int64(rootAdditional.ByteCount),
			TransportBudgetAdditionalPairSlots:   int64(rootAdditional.TransportCount),
			TransportBudgetAdditionalPairOwner:   rootAdditional.Owner,
		}, transferDiagDeviceCarrierBudget{
			Part: "memory_device_transport", UnixMillis: millis,
			TransportBudgetTotalByteCount:        int64(child.TotalByteCount),
			TransportBudgetUsedByteCount:         int64(child.UsedByteCount),
			TransportBudgetMaxCount:              int64(child.MaxTransportCount),
			TransportBudgetUsedCount:             int64(child.UsedTransportCount),
			TransportBudgetPendingH1Count:        int64(child.PendingH1Count),
			TransportBudgetPendingH1Bytes:        int64(child.PendingH1ByteCount),
			TransportBudgetReservedBytes:         int64(child.ReservedByteCount),
			TransportBudgetReleasedBytes:         int64(child.ReleasedByteCount),
			TransportBudgetHandoffCount:          int64(child.ActiveHandoffCount),
			TransportBudgetHandoffBytes:          int64(child.ActiveHandoffByteCount),
			TransportBudgetHandoffSlots:          int64(child.ActiveHandoffTransportCount),
			TransportBudgetHandoffID:             child.ActiveHandoffID,
			TransportBudgetHandoffFrom:           child.ActiveHandoffFromClass,
			TransportBudgetHandoffTo:             child.ActiveHandoffToClass,
			TransportBudgetHandoffH1Bytes:        int64(child.ActiveHandoffH1ByteCount),
			TransportBudgetPairID:                childPair.ID,
			TransportBudgetPairFrom:              childPair.FromClass,
			TransportBudgetPairTo:                childPair.ToClass,
			TransportBudgetPairH1Bytes:           int64(childPair.H1ByteCount),
			TransportBudgetPairBytes:             int64(childPair.ByteCount),
			TransportBudgetPairSlots:             int64(childPair.TransportCount),
			TransportBudgetPairOwner:             childPair.Owner,
			TransportBudgetAdditionalPairID:      childAdditional.ID,
			TransportBudgetAdditionalPairFrom:    childAdditional.FromClass,
			TransportBudgetAdditionalPairTo:      childAdditional.ToClass,
			TransportBudgetAdditionalPairH1Bytes: int64(childAdditional.H1ByteCount),
			TransportBudgetAdditionalPairBytes:   int64(childAdditional.ByteCount),
			TransportBudgetAdditionalPairSlots:   int64(childAdditional.TransportCount),
			TransportBudgetAdditionalPairOwner:   childAdditional.Owner,
		}
}

type transferDiagSend struct {
	Part         string                                  `json:"part"`
	UnixMillis   int64                                   `json:"unix_millis"`
	Window       string                                  `json:"window,omitempty"`
	Destination  string                                  `json:"destination,omitempty"`
	SendRecovery connect.ClientSendRecoveryStatsSnapshot `json:"send_recovery"`
}

type transferDiagReceive struct {
	Part        string                             `json:"part"`
	UnixMillis  int64                              `json:"unix_millis"`
	Window      string                             `json:"window,omitempty"`
	Destination string                             `json:"destination,omitempty"`
	Receive     connect.ClientReceiveStatsSnapshot `json:"receive"`
}

// attachTransferDiag points a client's p2p transports at the device's shared
// data-plane counters. A no-op unless the seam is on.
func (self *DeviceLocal) attachTransferDiag(clientSettings *connect.ClientSettings) {
	attachTransferDiagStats(clientSettings, self.transferDiagStats)
}

func attachTransferDiagStats(clientSettings *connect.ClientSettings, stats *connect.P2pDataPlaneStats) {
	if stats == nil || clientSettings == nil {
		return
	}
	streamManagerSettings := clientSettings.StreamManagerSettings
	if streamManagerSettings == nil || streamManagerSettings.StreamBufferSettings == nil {
		return
	}
	p2pSettings := streamManagerSettings.StreamBufferSettings.P2pTransportSettings
	if p2pSettings == nil {
		return
	}
	p2pSettings.DataPlaneStats = stats
}

// startTransferDiag starts only the optional build-time logger. Counters are
// installed before provider construction, not into already running settings.
func (self *DeviceLocal) startTransferDiag() {
	interval := transferDiagInterval()
	if interval <= 0 {
		return
	}
	self.lifecycleWorkers.Add(1)
	go func() {
		defer self.lifecycleWorkers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-self.ctx.Done():
				return
			case <-ticker.C:
				self.logTransferDiag()
			}
		}
	}()
}

func (self *DeviceLocal) collectTransferDiag(millis int64) []any {
	state := transferDiagState{
		Part:           "state",
		UnixMillis:     millis,
		ConnectEnabled: self.GetConnectEnabled(),
		ProvideMode:    self.GetProvideMode(),
		P2p:            self.transferDiagStats.Snapshot(),
	}
	if clientID := self.GetClientId(); clientID != nil {
		state.ClientID = clientID.String()
	}
	if location := self.GetConnectLocation(); location != nil {
		state.LocationNetworkPeer = location.NetworkPeer
		state.LocationIsDevice = location.ConnectLocationId != nil && location.IsDevice()
		state.LocationName = location.Name
		if state.LocationIsDevice {
			state.LocationClientID = location.ConnectLocationId.ClientId.String()
		}
	}
	lines := []any{}
	if client := self.providerClientSnapshot(); client != nil {
		lines = append(lines,
			transferDiagSend{Part: "provider_send", UnixMillis: millis, SendRecovery: client.SendRecoveryStats()},
			transferDiagReceive{Part: "provider_receive", UnixMillis: millis, Receive: client.ReceiveStats()},
		)
	}
	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	self.stateLock.Unlock()
	if multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok && multi != nil {
		memory := multi.MemorySnapshot()
		state.QualityClientCount = memory.QualityClientCount
		state.SpeedClientCount = memory.SpeedClientCount
		for _, stats := range multi.ClientTransferStats() {
			window := "speed"
			if stats.WindowType == connect.WindowTypeQuality {
				window = "quality"
			}
			destination := stats.Destination.String()
			state.WindowClientCount += 1
			lines = append(lines,
				transferDiagSend{Part: "window_send", UnixMillis: millis, Window: window, Destination: destination, SendRecovery: stats.SendRecovery},
				transferDiagReceive{Part: "window_receive", UnixMillis: millis, Window: window, Destination: destination, Receive: stats.Receive},
			)
		}
	}
	if self.memorySampler != nil {
		var runtimeSnapshot mobileMemoryRuntimeSnapshot
		self.memorySampler.runtimeReader.read(&runtimeSnapshot, self.platformTransportBudget)
		rootBudget, deviceBudget := transferDiagCarrierBudgets(self.platformTransportBudget.StatsWithRoot(), millis)
		lines = append(lines, transferDiagMemory{
			Part:                             "memory",
			UnixMillis:                       millis,
			GoTotalByteCount:                 runtimeSnapshot.totalByteCount,
			GoLiveByteCount:                  runtimeSnapshot.liveByteCount,
			GoGoalByteCount:                  runtimeSnapshot.goalByteCount,
			GoLimitByteCount:                 runtimeSnapshot.limitByteCount,
			DeviceMemoryTargetByteCount:      self.settings.MemoryTargetByteCount,
			PhysicalByteCount:                runtimeSnapshot.physicalByteCount,
			GoroutineCount:                   runtimeSnapshot.goroutineCount,
			PoolOutstandingCount:             runtimeSnapshot.poolOutstandingCount,
			PacketPoolOutstandingByteCount:   runtimeSnapshot.packetPoolOutstandingByteCount,
			PoolRetainedByteCount:            runtimeSnapshot.poolRetainedByteCount,
			PacketPoolRetainedByteCount:      runtimeSnapshot.packetPoolRetainedByteCount,
			PoolCapacityByteCount:            runtimeSnapshot.poolCapacityByteCount,
			transferDiagProcessCarrierBudget: rootBudget,
			IdleReclaimCount:                 runtimeSnapshot.idleReclaimCount,
			ForcedGCCount:                    runtimeSnapshot.forcedGCCount,
			GCCycleCount:                     runtimeSnapshot.gcCycleCount,
			WindowClientCount:                state.WindowClientCount,
		}, deviceBudget, transferDiagTransferBudget(self.MemoryUsed(), millis))
	}
	lines = append([]any{state}, lines...)
	return lines
}

func (self *DeviceLocal) logTransferDiag() {
	lines := self.collectTransferDiag(time.Now().UnixMilli())
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			continue
		}
		glog.Infof("[flightgate] %s\n", string(encoded))
	}
}

const transferDiagnosticSnapshotMaxBytes = 64 * 1024

var errTransferDiagnosticUnavailable = errors.New("transfer diagnostic snapshot requires an opted-in mobile device")
var errTransferDiagnosticSnapshotTooLarge = errors.New("transfer diagnostic snapshot exceeds 64 KiB")

type boundedTransferDiagnosticBuffer struct {
	bytes.Buffer
}

func (b *boundedTransferDiagnosticBuffer) Write(p []byte) (int, error) {
	if len(p) > transferDiagnosticSnapshotMaxBytes-b.Len() {
		return 0, errTransferDiagnosticSnapshotTooLarge
	}
	return b.Buffer.Write(p)
}

// TransferDiagnosticSnapshotJson returns one on-demand private NDJSON batch.
// All parts share unix_millis; carrier root/child fields come from one atomic
// StatsWithRoot snapshot, and the transfer hierarchy uses the same collector
// as the existing diagnostic logger. It neither drains the primitive sampler
// nor changes budgets, connectivity, GC, or routes. No batch is retained.
// Opt-in is required before device construction so P2P counters measure real
// traffic from both the provider and every outbound window. Missing evidence
// is an error, not a plausible zero-valued snapshot.
func (self *DeviceLocal) TransferDiagnosticSnapshotJson() (string, error) {
	if self.transferDiagStats == nil || self.memorySampler == nil || self.platformTransportBudget == nil {
		return "", errTransferDiagnosticUnavailable
	}
	var buffer boundedTransferDiagnosticBuffer
	encoder := json.NewEncoder(&buffer)
	for _, part := range self.collectTransferDiag(time.Now().UnixMilli()) {
		if err := encoder.Encode(part); err != nil {
			return "", err
		}
	}
	return buffer.String(), nil
}

// SetTransferDiagDeferTimeoutResend turns FLIGHTGATEFIX §13.5's deferred
// whole-window timeout resend on or off for clients built after this call
// (the provider client at its next rotation, a window client at the next
// connect), so the rig can A/B the setting without a rebuild per arm.
func (self *DeviceLocal) SetTransferDiagDeferTimeoutResend(enabled bool) {
	self.stateLock.Lock()
	self.transferDiagDeferTimeoutResend = &enabled
	send := self.settings.ClientSettings.SendBufferSettings
	self.stateLock.Unlock()
	// the device's own settings feed the provider client
	if send != nil {
		send.DeferTimeoutResendWhileCumulativeProgress = enabled
	}
}

// applyTransferDiagSettings stamps the rig's setting overrides onto one
// client's settings. A no-op unless an override is set.
func (self *DeviceLocal) applyTransferDiagSettings(clientSettings *connect.ClientSettings) {
	self.stateLock.Lock()
	defer_ := self.transferDiagDeferTimeoutResend
	lane := self.transferDiagLaneRule
	self.stateLock.Unlock()
	if clientSettings == nil || clientSettings.SendBufferSettings == nil {
		return
	}
	if defer_ != nil {
		clientSettings.SendBufferSettings.DeferTimeoutResendWhileCumulativeProgress = *defer_
	}
	if lane != nil {
		clientSettings.SendBufferSettings.ReliableLaneProvenRecovery = *lane
	}
}

// TransferDiagDeferTimeoutResend reports the current override (false when
// unset), for the rig's status line.
func (self *DeviceLocal) TransferDiagDeferTimeoutResend() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transferDiagDeferTimeoutResend != nil && *self.transferDiagDeferTimeoutResend
}

// SetTransferDiagLaneRule turns FLIGHTGATEFIX's reliable-lane proven-recovery
// rule on or off for clients built after this call, so the rig can measure the
// rule as an arm of one build rather than a separate build.
func (self *DeviceLocal) SetTransferDiagLaneRule(enabled bool) {
	self.stateLock.Lock()
	self.transferDiagLaneRule = &enabled
	send := self.settings.ClientSettings.SendBufferSettings
	self.stateLock.Unlock()
	if send != nil {
		send.ReliableLaneProvenRecovery = enabled
	}
}

// TransferDiagLaneRule reports the current override (false when unset).
func (self *DeviceLocal) TransferDiagLaneRule() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transferDiagLaneRule != nil && *self.transferDiagLaneRule
}

// SetTransferDiagAllowDirect is the rig's relay-only control: while enabled,
// the next window (connect after a disconnect) is built with direct mode
// forced to allowDirect, superseding the performance profile and the
// same-network force; disabled restores the normal decision. Never used by
// an app; the debug receiver of the Android rig build calls it.
func (self *DeviceLocal) SetTransferDiagAllowDirect(enabled bool, allowDirect bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !enabled {
		self.transferDiagAllowDirect = nil
		return
	}
	value := allowDirect
	self.transferDiagAllowDirect = &value
}
