package sdk

import (
	"encoding/json"
	"strconv"
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
	PhysicalByteCount              int64  `json:"physical_bytes"`
	GoroutineCount                 int64  `json:"goroutines"`
	PoolOutstandingCount           int64  `json:"pool_outstanding"`
	PacketPoolOutstandingByteCount int64  `json:"packet_pool_outstanding_bytes"`
	PoolRetainedByteCount          int64  `json:"pool_retained_bytes"`
	PacketPoolRetainedByteCount    int64  `json:"packet_pool_retained_bytes"`
	PoolCapacityByteCount          int64  `json:"pool_capacity_bytes"`
	TransportBudgetUsedByteCount   int64  `json:"transport_budget_used_bytes"`
	IdleReclaimCount               int64  `json:"idle_reclaim_count"`
	ForcedGCCount                  int64  `json:"forced_gc_count"`
	GCCycleCount                   int64  `json:"gc_cycles"`
	WindowClientCount              int    `json:"window_client_count"`
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
	stats := self.transferDiagStats
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

// startTransferDiag creates the shared counters, attaches them to the device
// client settings (the provider client is built from these later) and starts
// the logger. Called once at device construction.
func (self *DeviceLocal) startTransferDiag() {
	interval := transferDiagInterval()
	if interval <= 0 {
		return
	}
	self.transferDiagStats = &connect.P2pDataPlaneStats{}
	self.attachTransferDiag(&self.settings.ClientSettings)
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

func (self *DeviceLocal) logTransferDiag() {
	millis := time.Now().UnixMilli()
	state := transferDiagState{
		Part:           "state",
		UnixMillis:     millis,
		ConnectEnabled: self.GetConnectEnabled(),
		ProvideMode:    self.GetProvideMode(),
		P2p:            self.transferDiagStats.Snapshot(),
	}
	if location := self.GetConnectLocation(); location != nil {
		state.LocationNetworkPeer = location.NetworkPeer
		state.LocationIsDevice = location.ConnectLocationId != nil && location.IsDevice()
		state.LocationName = location.Name
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
		self.memorySampler.runtimeReader.read(&runtimeSnapshot)
		lines = append(lines, transferDiagMemory{
			Part:                           "memory",
			UnixMillis:                     millis,
			GoTotalByteCount:               runtimeSnapshot.totalByteCount,
			GoLiveByteCount:                runtimeSnapshot.liveByteCount,
			GoGoalByteCount:                runtimeSnapshot.goalByteCount,
			GoLimitByteCount:               runtimeSnapshot.limitByteCount,
			PhysicalByteCount:              runtimeSnapshot.physicalByteCount,
			GoroutineCount:                 runtimeSnapshot.goroutineCount,
			PoolOutstandingCount:           runtimeSnapshot.poolOutstandingCount,
			PacketPoolOutstandingByteCount: runtimeSnapshot.packetPoolOutstandingByteCount,
			PoolRetainedByteCount:          runtimeSnapshot.poolRetainedByteCount,
			PacketPoolRetainedByteCount:    runtimeSnapshot.packetPoolRetainedByteCount,
			PoolCapacityByteCount:          runtimeSnapshot.poolCapacityByteCount,
			TransportBudgetUsedByteCount:   runtimeSnapshot.transportBudgetUsedByteCount,
			IdleReclaimCount:               runtimeSnapshot.idleReclaimCount,
			ForcedGCCount:                  runtimeSnapshot.forcedGCCount,
			GCCycleCount:                   runtimeSnapshot.gcCycleCount,
			WindowClientCount:              state.WindowClientCount,
		})
	}
	lines = append([]any{state}, lines...)
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			continue
		}
		glog.Infof("[flightgate] %s\n", string(encoded))
	}
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
