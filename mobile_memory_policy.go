package sdk

import (
	"runtime"
	"time"

	"github.com/urnetwork/connect"
)

// The 24-MiB plan is a mobile Go-runtime steady-state policy. It is not an
// iOS phys_footprint or jetsam ceiling; the extension must measure those
// separately. Keeping the threshold here makes every mobile construction use
// the same topology and queue profile instead of relying on app call order.
const mobileSteadyMemoryTargetByteCount ByteCount = 24 * 1024 * 1024

// Keep the complete 256-KiB mobile packet free-list warm. This is enough for
// 256 small-ACK roots plus 96 full-MTU roots under the pool's 1:3 split, while
// avoiding both a cold allocation wave and retention of a multi-MiB burst.
const mobilePacketPoolWarmByteCount ByteCount = 256 * 1024

// The process soft limit is an emergency GC boundary, not permission for
// returned buffers to consume the same fraction on a phone as they do in a
// server. At the 32-MiB mobile soft limit the generic ratios otherwise permit
// 13.2 MiB of free-list capacity; the sustained H1 trace filled 6.5 MiB and
// crossed the 28-MiB diagnostic ceiling after live traffic drained. These caps
// affect only returned buffers. Pool misses still allocate, and the separate
// packet/transfer budgets continue to bound live ownership.
const mobilePacketPoolCapacityByteCount ByteCount = 256 * 1024
const mobileLargeObjectPoolCapacityByteCount ByteCount = 512 * 1024

const (
	// Per-flow H1 handoff queues retain decoded packet roots before the shared
	// receive queue accounts them. Keep one device-wide bandwidth-delay window:
	// 1.5 MiB is the provider-on floor, while folding the idle provider share
	// into the client may raise it to 2 MiB for provider-off H1 performance.
	mobilePackQueueBudgetMinByteCount ByteCount = 1536 * 1024
	mobilePackQueueBudgetMaxByteCount ByteCount = 2 * 1024 * 1024
	// ReceiveSequence reorder queues hold decoded packet roots after the Pack
	// handoff releases its reservation. Their former 96-KiB per-sequence floor
	// was deliberately outside the shared budget; a browser fan-out of roughly
	// 80 flows therefore retained 6.51 MiB of packet roots while only 1.98 MiB
	// appeared in device accounting. Charge every queued byte to one aggregate
	// bandwidth-delay window. Provider-off can spend the same bounded 2-MiB
	// maximum as the Pack handoff; provider-on retains the 1.5-MiB floor.
	mobileReceiveQueueBudgetMinByteCount ByteCount = 1536 * 1024
	mobileReceiveQueueBudgetMaxByteCount ByteCount = 2 * 1024 * 1024
)

func mobilePackQueueBudgetByteCount(clientShareByteCount ByteCount) ByteCount {
	return min(
		mobilePackQueueBudgetMaxByteCount,
		max(mobilePackQueueBudgetMinByteCount, clientShareByteCount/10),
	)
}

func mobileReceiveQueueBudgetByteCount(clientShareByteCount ByteCount) ByteCount {
	return min(
		mobileReceiveQueueBudgetMaxByteCount,
		max(mobileReceiveQueueBudgetMinByteCount, clientShareByteCount/10),
	)
}

// mobileReceiveQueueBudgetForPlatform preserves the desktop/server share
// calculation and installs the exact aggregate mobile ceiling only for the
// <=24-MiB profile. Keeping this pure makes the provider on/off sizing policy
// directly testable on a non-mobile host.
func mobileReceiveQueueBudgetForPlatform(
	memoryTargetByteCount ByteCount,
	clientShareByteCount ByteCount,
	mobile bool,
) ByteCount {
	if mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return mobileReceiveQueueBudgetByteCount(clientShareByteCount)
	}
	return max(byteCountFraction(clientShareByteCount, 4, 7), 1536*1024)
}

func mobilePackQueueBudgetForPlatform(
	memoryTargetByteCount ByteCount,
	clientShareByteCount ByteCount,
	mobile bool,
) *connect.TransferMemoryBudget {
	if !mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return nil
	}
	return connect.NewTransferMemoryBudget(
		mobilePackQueueBudgetByteCount(clientShareByteCount),
	)
}

func defaultDeviceLocalMemoryTargetByteCountForPlatform(mobile bool) ByteCount {
	if mobile {
		return mobileSteadyMemoryTargetByteCount
	}
	return defaultDeviceLocalMemoryTargetByteCount
}

const (
	// Keep send, H3, forward, and control ownership at the measured
	// sixteen-message ceiling. ACKs use compact allocation-free values and get
	// a separate small burst budget: logical-lane division turns 64 into eight
	// entries per data lane instead of the former two, while retaining only a
	// few KiB per active peer. H1 gets a larger receive-pump burst window:
	// the adjacent ACK/coalescing trace recorded 2,280 Pack handoff drops while
	// ACK handoff drops remained zero and active runtime stayed below 24 MiB.
	// Connect enforces the H3 and H1 counts on one ordered channel, so mixed
	// carrier sequences cannot let H3 consume this reliable-carrier spend. The
	// iterative 64/128-KiB -> 128/256-KiB diagnostic stayed below 24 MiB, but it
	// did not improve public-provider bulk or fast.com throughput and amplified
	// timeout recovery. Keep the generic Connect mechanism available for a
	// controlled-provider A/B, while the production mobile policy stays fixed
	// at the measured 64/128-KiB knee.
	mobileClientSequenceBufferMaxCount        = 16
	mobileClientAckBufferMaxCount             = 64
	mobileH1ReceiveSequenceBufferMaxCount     = 64
	mobileReceiveSequenceBufferMaxByteCount   = 128 * 1024
	mobileH1ReceiveSequenceBufferMaxByteCount = 128 * 1024
	// The 1-ms arm removed nearly every H1 Pack handoff loss, but one remaining
	// timeout delayed four Wikipedia resources by 5.4--7.4 seconds. Ten
	// milliseconds is still bounded reader backpressure on a reliable carrier;
	// it is far below the recovery timer and does not enlarge either queue.
	mobileH1ReceivePackHandoffWaitTimeout = 10 * time.Millisecond
	mobileH1ReceiveAckHandoffWaitTimeout  = time.Millisecond
	// Eight compact Transfer ACKs need only channel-slot storage and cover far
	// more than one 10-ms ACK compression interval. They bypass a full ordinary
	// H1 route without increasing any data sequence or receive window.
	mobileH1AckPriorityBufferSize = 8
	mobileResendQueueMinByteCount = 64 * 1024
	mobileResendQueueMaxByteCount = 512 * 1024
	// An empty receive queue already admits one item even when the aggregate
	// budget is exhausted, so a per-sequence byte floor is unnecessary for
	// liveness. Zero makes all subsequent reorder ownership visible to and
	// bounded by the shared device budget instead of multiplying by flow count.
	mobileReceiveQueueMinByteCount                  = 0
	mobileReceiveQueueMaxByteCount                  = 768 * 1024
	mobileUnreliableFlightMaxByteCount              = 128 * 1024
	mobileUnreliableFlightMaxMessageCount           = 16
	mobileQualityWindowSize                         = 4
	mobileSpeedWindowSize                           = 1
	mobilePacketGroupMaxPacketCount                 = 16
	mobilePacketGroupMaxByteCount         ByteCount = 24 * 1024
	// Browser tabs leave many completed TCP flow objects behind for the
	// desktop-oriented ten-minute default. A three-minute mobile timeout keeps
	// active/keepalive traffic intact while retiring that stale graph inside a
	// five-minute post-burst steady-state measurement.
	mobileTcpSequenceIdleTimeout = 3 * time.Minute
)

func mobileRuntime() bool {
	return runtime.GOOS == "android" || runtime.GOOS == "ios"
}

func mobileLowMemoryPolicyEnabled(memoryTargetByteCount ByteCount) bool {
	return mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobileRuntime())
}

func mobileLowMemoryPolicyEnabledForPlatform(
	memoryTargetByteCount ByteCount,
	mobile bool,
) bool {
	return mobile && 0 < memoryTargetByteCount &&
		memoryTargetByteCount <= mobileSteadyMemoryTargetByteCount
}

func applyMobileLowMemoryPlatformTransportSettings(
	settings *connect.PlatformTransportSettings,
	memoryTargetByteCount ByteCount,
) {
	applyMobileLowMemoryPlatformTransportSettingsForPlatform(
		settings,
		memoryTargetByteCount,
		mobileRuntime(),
	)
}

func applyMobileLowMemoryPlatformTransportSettingsForPlatform(
	settings *connect.PlatformTransportSettings,
	memoryTargetByteCount ByteCount,
	mobile bool,
) {
	if settings == nil ||
		!mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	settings.H1AckPriorityBufferSize = mobileH1AckPriorityBufferSize
}

// applyMobileLowMemoryClientSettings bounds the number and bytes of packets
// one mobile exit can own before the shared per-device budgets take effect.
// The shared budgets remain the aggregate safety net; smaller per-sequence
// floors prevent many live flows from multiplying nominally "free" capacity.
func applyMobileLowMemoryClientSettings(
	settings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
) {
	applyMobileLowMemoryClientSettingsForPlatform(
		settings,
		memoryTargetByteCount,
		mobileRuntime(),
	)
}

func applyMobileLowMemoryClientSettingsForPlatform(
	settings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
	mobile bool,
) {
	if settings == nil ||
		!mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	settings.SendBufferSize = min(settings.SendBufferSize, mobileClientSequenceBufferMaxCount)
	settings.ForwardBufferSize = min(settings.ForwardBufferSize, mobileClientSequenceBufferMaxCount)
	if settings.SendBufferSettings != nil {
		send := settings.SendBufferSettings
		send.SequenceBufferSize = min(send.SequenceBufferSize, mobileClientSequenceBufferMaxCount)
		send.AckBufferSize = min(send.AckBufferSize, mobileClientAckBufferMaxCount)
		send.ResendQueueMinByteCount = min(
			send.ResendQueueMinByteCount,
			mobileResendQueueMinByteCount,
		)
		send.ResendQueueMaxByteCount = min(
			send.ResendQueueMaxByteCount,
			mobileResendQueueMaxByteCount,
		)
		send.UnreliableMaximumFlightByteCount = min(
			send.UnreliableMaximumFlightByteCount,
			mobileUnreliableFlightMaxByteCount,
		)
		send.UnreliableMaximumFlightMessageCount = min(
			send.UnreliableMaximumFlightMessageCount,
			mobileUnreliableFlightMaxMessageCount,
		)
	}
	if settings.ReceiveBufferSettings != nil {
		receive := settings.ReceiveBufferSettings
		receive.ReceiveQueueRetainedByteAccounting = true
		receive.SequenceBufferSize = min(
			receive.SequenceBufferSize,
			mobileClientSequenceBufferMaxCount,
		)
		receive.H1SequenceBufferSize = min(
			receive.H1SequenceBufferSize,
			mobileH1ReceiveSequenceBufferMaxCount,
		)
		receive.H1SequenceBufferAdaptiveMaxSize = 0
		receive.H1SequenceBufferAdaptiveStepSize = 0
		receive.H1SequenceBufferAdaptiveSaturationThreshold = 0
		receive.H1SequenceBufferAdaptiveSaturationWindow = 0
		receive.H1SequenceBufferAdaptiveMaxByteCount = 0
		receive.H1SequenceBufferAdaptiveStepByteCount = 0
		receive.SequenceBufferByteCount = min(
			receive.SequenceBufferByteCount,
			mobileReceiveSequenceBufferMaxByteCount,
		)
		receive.H1SequenceBufferByteCount = min(
			receive.H1SequenceBufferByteCount,
			mobileH1ReceiveSequenceBufferMaxByteCount,
		)
		receive.H1PackHandoffTimeout = mobileH1ReceivePackHandoffWaitTimeout
		receive.H1AckHandoffTimeout = mobileH1ReceiveAckHandoffWaitTimeout
		receive.ReceiveQueueMinByteCount = min(
			receive.ReceiveQueueMinByteCount,
			mobileReceiveQueueMinByteCount,
		)
		receive.ReceiveQueueMaxByteCount = min(
			receive.ReceiveQueueMaxByteCount,
			mobileReceiveQueueMaxByteCount,
		)
	}
	if settings.ForwardBufferSettings != nil {
		settings.ForwardBufferSettings.SequenceBufferSize = min(
			settings.ForwardBufferSettings.SequenceBufferSize,
			mobileClientSequenceBufferMaxCount,
		)
	}
	if settings.ContractManagerSettings != nil {
		settings.ContractManagerSettings.SequenceBufferSize = min(
			settings.ContractManagerSettings.SequenceBufferSize,
			mobileClientSequenceBufferMaxCount,
		)
	}
}

// applyMobileLowMemoryMultiClientSettings reduces the connected control/live
// set for a 24-MiB mobile DeviceLocal. Explicit fixed destinations are
// unaffected; this changes only Auto's quality and speed windows. Server and
// desktop defaults never pass the mobile platform gate.
func applyMobileLowMemoryMultiClientSettings(
	settings *connect.MultiClientSettings,
	memoryTargetByteCount ByteCount,
) {
	applyMobileLowMemoryMultiClientSettingsForPlatform(
		settings,
		memoryTargetByteCount,
		mobileRuntime(),
	)
}

func applyMobileLowMemoryMultiClientSettingsForPlatform(
	settings *connect.MultiClientSettings,
	memoryTargetByteCount ByteCount,
	mobile bool,
) {
	if settings == nil ||
		!mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	settings.SequenceBufferSize = min(
		settings.SequenceBufferSize,
		mobileClientSequenceBufferMaxCount,
	)
	settings.RemovalReceiveQueueSize = min(
		settings.RemovalReceiveQueueSize,
		mobileClientSequenceBufferMaxCount,
	)
	// Nonpositive packet-group limits mean "unbounded" in Connect. A partial
	// custom settings object must not accidentally bypass the mobile ownership
	// ceiling, so install the ceiling as well as lowering larger values.
	if settings.PacketGroupMaxPacketCount <= 0 ||
		mobilePacketGroupMaxPacketCount < settings.PacketGroupMaxPacketCount {
		settings.PacketGroupMaxPacketCount = mobilePacketGroupMaxPacketCount
	}
	if settings.PacketGroupMaxByteCount <= 0 ||
		mobilePacketGroupMaxByteCount < settings.PacketGroupMaxByteCount {
		settings.PacketGroupMaxByteCount = mobilePacketGroupMaxByteCount
	}
	settings.StandingReserve = false
	settings.StrictWindowSizeHardMax = true
	if settings.TcpSequenceIdleTimeout <= 0 ||
		mobileTcpSequenceIdleTimeout < settings.TcpSequenceIdleTimeout {
		settings.TcpSequenceIdleTimeout = mobileTcpSequenceIdleTimeout
	}
	if settings.WindowSizes == nil {
		settings.WindowSizes = make(map[connect.WindowType]connect.WindowSizeSettings, 2)
	}
	settings.WindowSizes[connect.WindowTypeQuality] = connect.WindowSizeSettings{
		WindowSizeMin:            mobileQualityWindowSize,
		WindowSizeMax:            mobileQualityWindowSize,
		WindowSizeHardMax:        mobileQualityWindowSize,
		WindowSizeReconnectScale: 1,
	}
	settings.WindowSizes[connect.WindowTypeSpeed] = connect.WindowSizeSettings{
		WindowSizeMin:            mobileSpeedWindowSize,
		WindowSizeMax:            mobileSpeedWindowSize,
		WindowSizeHardMax:        mobileSpeedWindowSize,
		FixedWindowSize:          mobileSpeedWindowSize,
		WindowSizeReconnectScale: 1,
	}
}
