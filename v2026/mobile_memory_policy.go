package sdk

import (
	"runtime"
	"time"

	"github.com/urnetwork/connect/v2026"
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
	return mobilePackQueueBudgetByteCountForTarget(
		clientShareByteCount,
		mobileSteadyMemoryTargetByteCount,
	)
}

func mobilePackQueueBudgetByteCountForTarget(
	clientShareByteCount ByteCount,
	memoryTargetByteCount ByteCount,
) ByteCount {
	return min(
		mobileTargetScaledByteCount(mobilePackQueueBudgetMaxByteCount, memoryTargetByteCount),
		max(
			mobileTargetScaledByteCount(mobilePackQueueBudgetMinByteCount, memoryTargetByteCount),
			clientShareByteCount/10,
		),
	)
}

func mobileReceiveQueueBudgetByteCount(clientShareByteCount ByteCount) ByteCount {
	return mobileReceiveQueueBudgetByteCountForTarget(
		clientShareByteCount,
		mobileSteadyMemoryTargetByteCount,
	)
}

func mobileReceiveQueueBudgetByteCountForTarget(
	clientShareByteCount ByteCount,
	memoryTargetByteCount ByteCount,
) ByteCount {
	return min(
		mobileTargetScaledByteCount(mobileReceiveQueueBudgetMaxByteCount, memoryTargetByteCount),
		max(
			mobileTargetScaledByteCount(
				mobileReceiveQueueBudgetMinByteCount,
				memoryTargetByteCount,
			),
			clientShareByteCount/10,
		),
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
	if mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return mobileReceiveQueueBudgetByteCountForTarget(
			clientShareByteCount,
			memoryTargetByteCount,
		)
	}
	return max(byteCountFraction(clientShareByteCount, 4, 7), 1536*1024)
}

// mobileReceiveQueueMaxByteCountForPool is the mobile ceiling on the
// per-sequence receive hold, ReceiveQueueMaxByteCount.
//
// Connect advertises the smaller of that field and the attached receive
// pool's live total (receiveWindowAdvertisement in connect/transfer.go), and
// the sender clamps its window to the advertisement. The pool is what
// admission charges -- bytes above it are never taken, and on mobile
// ReceiveQueueMinByteCount is zero so nothing is held outside it -- so the
// pool is the bound on what the phone will hold, and it is memory the device
// has already reserved. A field below the pool therefore costs window and
// saves nothing: the 768 KiB constant, calibrated for the 24 MiB profile
// before the pools were attached, was undercutting a 1.5 MiB pool by half.
//
// On a pooled client the field's one job is to stay at or above every total
// the pool can take, so the advertisement reads the pool itself, live through
// the provide-mode resizes of applyProvideMemorySharesWithLock. That total is
// at most mobileReceiveQueueBudgetMaxByteCount scaled by the target, the
// ceiling of mobileReceiveQueueBudgetByteCountForTarget, and the provider
// pair (4/7 of half the provider share) sits under the same line. Taking the
// ceiling rather than the pool's total at call time keeps this independent of
// when the policy runs relative to the pool being attached or resized.
//
// One unit mismatch to know about. The mobile pool charges retained bytes
// (ReceiveQueueRetainedByteAccounting: the carrier root, the frame roots and
// the owner per held Pack) while the field and the advertisement count
// payload, so an out-of-order run fills the pool before the advertised
// payload figure is reached and the receive hold policy evicts or refuses the
// rest, which the eviction notice turns into resends. That is bandwidth after
// a loss, never occupancy: admission still stops at the pool. The 768 KiB
// constant was already above what the pool holds in payload for MTU-sized
// Packs, so it protected nothing the pool does not.
//
// A client with no pool keeps the calibrated constant as its bound: there the
// field is the only ceiling on the hold.
func mobileReceiveQueueMaxByteCountForPool(
	pooled bool,
	memoryTargetByteCount ByteCount,
) ByteCount {
	maxByteCount := mobileTargetScaledByteCount(
		mobileReceiveQueueMaxByteCount,
		memoryTargetByteCount,
	)
	if pooled {
		maxByteCount = max(
			maxByteCount,
			mobileTargetScaledByteCount(
				mobileReceiveQueueBudgetMaxByteCount,
				memoryTargetByteCount,
			),
		)
	}
	return maxByteCount
}

func mobilePackQueueBudgetForPlatform(
	memoryTargetByteCount ByteCount,
	clientShareByteCount ByteCount,
	mobile bool,
) *connect.TransferMemoryBudget {
	if !mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return nil
	}
	return connect.NewTransferMemoryBudget(
		mobilePackQueueBudgetByteCountForTarget(clientShareByteCount, memoryTargetByteCount),
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
	mobileClientSequenceBufferMaxCount = 16
	mobileClientAckBufferMaxCount      = 64
	// Fast.com and modern pages open independent TCP flows. Explicit H1 may
	// hash their request/ACK direction across the maximum negotiated lane set so
	// one missing Transfer Pack does not head-of-line block every flow. Connect
	// divides the existing send/ACK slot budgets across lanes and shares the
	// exact resend/receive byte budgets, so this spends only lazy sequence
	// metadata rather than eight independent bandwidth-delay windows. Download
	// data needs the same setting on the provider sender; enabling only this side
	// is a responsiveness optimization, not a bulk-throughput claim.
	mobileH1LogicalDataLaneCount              = 8
	mobileH1ReceiveSequenceBufferMaxCount     = 64
	mobileReceiveSequenceBufferMaxByteCount   = 128 * 1024
	mobileH1ReceiveSequenceBufferMaxByteCount = 128 * 1024
	// A controlled H1 provider proved that a finite handoff timeout merely moves
	// synthetic loss downstream: after the carrier route became lossless, the
	// 10-ms boundary dropped 24 messages and pinned the 2-MiB reorder budget.
	// Negative means wait for capacity or cancellation. The fixed per-sequence
	// count/byte gates and shared exact Pack budget still bound ownership.
	mobileH1ReceivePackHandoffWaitTimeout = -1 * time.Nanosecond
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
	mobileReceiveQueueMinByteCount = 0
	// The per-sequence receive hold of a client with NO attached pool. On a
	// pooled client the pool bounds the hold instead; see
	// mobileReceiveQueueMaxByteCountForPool.
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

func mobileMemoryPolicyEnabled(memoryTargetByteCount ByteCount) bool {
	return mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobileRuntime())
}

// mobileMemoryPolicyEnabledForPlatform reports whether the mobile memory
// policy applies. It is deliberately independent of how large the target is:
// the invariants this policy installs (retained-byte accounting, no standing
// reserve, the hard admission ceiling, the mobile TCP idle timeout, the
// packet-group ownership caps) are properties of a phone, not of a particular
// budget, and the byte/count caps are sized from the target by
// mobileTargetScaled* rather than gated on it.
//
// The predicate it replaces also required memoryTargetByteCount <= 24 MiB, so
// one byte above the steady target silently restored desktop sequence depths,
// queue budgets, window sizes and packet-group limits -- the whole policy
// disappeared exactly when a device was given more memory to work with.
func mobileMemoryPolicyEnabledForPlatform(
	memoryTargetByteCount ByteCount,
	mobile bool,
) bool {
	return mobile && 0 < memoryTargetByteCount
}

// The mobile caps are calibrated at the 24-MiB steady target. At or below that
// target the calibrated value stands, which is exactly what the gate these
// replace did for every target it admitted, so no profile that ships today
// changes. Above it, a cap grows in proportion to the target.
//
// Both are monotone non-decreasing in the target, so a larger budget can never
// produce a tighter cap.
func mobileTargetScaledByteCount(
	calibratedByteCount ByteCount,
	memoryTargetByteCount ByteCount,
) ByteCount {
	if memoryTargetByteCount <= mobileSteadyMemoryTargetByteCount {
		return calibratedByteCount
	}
	return calibratedByteCount * memoryTargetByteCount / mobileSteadyMemoryTargetByteCount
}

func mobileTargetScaledCount(calibratedCount int, memoryTargetByteCount ByteCount) int {
	if memoryTargetByteCount <= mobileSteadyMemoryTargetByteCount {
		return calibratedCount
	}
	return int(
		ByteCount(calibratedCount) * memoryTargetByteCount / mobileSteadyMemoryTargetByteCount,
	)
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
		!mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	settings.H1AckPriorityBufferSize = mobileTargetScaledCount(
		mobileH1AckPriorityBufferSize,
		memoryTargetByteCount,
	)
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
		!mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	sequenceBufferMaxCount := mobileTargetScaledCount(
		mobileClientSequenceBufferMaxCount,
		memoryTargetByteCount,
	)
	settings.SendBufferSize = min(settings.SendBufferSize, sequenceBufferMaxCount)
	settings.ForwardBufferSize = min(settings.ForwardBufferSize, sequenceBufferMaxCount)
	if settings.SendBufferSettings != nil {
		send := settings.SendBufferSettings
		send.SequenceBufferSize = min(send.SequenceBufferSize, sequenceBufferMaxCount)
		send.AckBufferSize = min(
			send.AckBufferSize,
			mobileTargetScaledCount(mobileClientAckBufferMaxCount, memoryTargetByteCount),
		)
		send.ResendQueueMinByteCount = min(
			send.ResendQueueMinByteCount,
			mobileTargetScaledByteCount(mobileResendQueueMinByteCount, memoryTargetByteCount),
		)
		send.ResendQueueMaxByteCount = min(
			send.ResendQueueMaxByteCount,
			mobileTargetScaledByteCount(mobileResendQueueMaxByteCount, memoryTargetByteCount),
		)
		send.UnreliableMaximumFlightByteCount = min(
			send.UnreliableMaximumFlightByteCount,
			mobileTargetScaledByteCount(
				mobileUnreliableFlightMaxByteCount,
				memoryTargetByteCount,
			),
		)
		send.UnreliableMaximumFlightMessageCount = min(
			send.UnreliableMaximumFlightMessageCount,
			mobileTargetScaledCount(
				mobileUnreliableFlightMaxMessageCount,
				memoryTargetByteCount,
			),
		)
	}
	if settings.ReceiveBufferSettings != nil {
		receive := settings.ReceiveBufferSettings
		// invariant: charge every retained reorder byte to the shared budget
		receive.ReceiveQueueRetainedByteAccounting = true
		receive.SequenceBufferSize = min(
			receive.SequenceBufferSize,
			sequenceBufferMaxCount,
		)
		receive.H1SequenceBufferSize = min(
			receive.H1SequenceBufferSize,
			mobileTargetScaledCount(
				mobileH1ReceiveSequenceBufferMaxCount,
				memoryTargetByteCount,
			),
		)
		receive.H1SequenceBufferAdaptiveMaxSize = 0
		receive.H1SequenceBufferAdaptiveStepSize = 0
		receive.H1SequenceBufferAdaptiveSaturationThreshold = 0
		receive.H1SequenceBufferAdaptiveSaturationWindow = 0
		receive.H1SequenceBufferAdaptiveMaxByteCount = 0
		receive.H1SequenceBufferAdaptiveStepByteCount = 0
		receive.SequenceBufferByteCount = min(
			receive.SequenceBufferByteCount,
			mobileTargetScaledByteCount(
				mobileReceiveSequenceBufferMaxByteCount,
				memoryTargetByteCount,
			),
		)
		receive.H1SequenceBufferByteCount = min(
			receive.H1SequenceBufferByteCount,
			mobileTargetScaledByteCount(
				mobileH1ReceiveSequenceBufferMaxByteCount,
				memoryTargetByteCount,
			),
		)
		receive.H1PackHandoffTimeout = mobileH1ReceivePackHandoffWaitTimeout
		receive.ReliablePackHandoffTimeout = mobileH1ReceivePackHandoffWaitTimeout
		receive.H1AckHandoffTimeout = mobileH1ReceiveAckHandoffWaitTimeout
		receive.ReceiveQueueMinByteCount = min(
			receive.ReceiveQueueMinByteCount,
			mobileReceiveQueueMinByteCount,
		)
		receive.ReceiveQueueMaxByteCount = min(
			receive.ReceiveQueueMaxByteCount,
			mobileReceiveQueueMaxByteCountForPool(
				receive.ReceiveQueueBudget != nil,
				memoryTargetByteCount,
			),
		)
	}
	if settings.ForwardBufferSettings != nil {
		settings.ForwardBufferSettings.SequenceBufferSize = min(
			settings.ForwardBufferSettings.SequenceBufferSize,
			sequenceBufferMaxCount,
		)
	}
	if settings.ContractManagerSettings != nil {
		settings.ContractManagerSettings.SequenceBufferSize = min(
			settings.ContractManagerSettings.SequenceBufferSize,
			sequenceBufferMaxCount,
		)
	}
}

// applyMobileH1PerformanceClientSettings enables bounded flow isolation only
// for an explicit H1 destination policy. H3 and Auto remain unchanged until
// their own performance iteration; non-mobile and larger-memory profiles keep
// caller-selected lane settings.
func applyMobileH1PerformanceClientSettings(
	settings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
	explicitH1 bool,
) {
	applyMobileH1PerformanceClientSettingsForPlatform(
		settings,
		memoryTargetByteCount,
		mobileRuntime(),
		explicitH1,
	)
}

func applyMobileH1PerformanceClientSettingsForPlatform(
	settings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
	mobile bool,
	explicitH1 bool,
) {
	if settings == nil || settings.SendBufferSettings == nil || !explicitH1 ||
		!mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	settings.SendBufferSettings.LogicalDataLaneCount = mobileTargetScaledCount(
		mobileH1LogicalDataLaneCount,
		memoryTargetByteCount,
	)
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
		!mobileMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return
	}
	sequenceBufferMaxCount := mobileTargetScaledCount(
		mobileClientSequenceBufferMaxCount,
		memoryTargetByteCount,
	)
	settings.SequenceBufferSize = min(
		settings.SequenceBufferSize,
		sequenceBufferMaxCount,
	)
	settings.RemovalReceiveQueueSize = min(
		settings.RemovalReceiveQueueSize,
		sequenceBufferMaxCount,
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
	qualityWindowSize := mobileTargetScaledCount(mobileQualityWindowSize, memoryTargetByteCount)
	speedWindowSize := mobileTargetScaledCount(mobileSpeedWindowSize, memoryTargetByteCount)
	settings.WindowSizes[connect.WindowTypeQuality] = connect.WindowSizeSettings{
		WindowSizeMin:            qualityWindowSize,
		WindowSizeMax:            qualityWindowSize,
		WindowSizeHardMax:        qualityWindowSize,
		WindowSizeReconnectScale: 1,
	}
	settings.WindowSizes[connect.WindowTypeSpeed] = connect.WindowSizeSettings{
		WindowSizeMin:            speedWindowSize,
		WindowSizeMax:            speedWindowSize,
		WindowSizeHardMax:        speedWindowSize,
		FixedWindowSize:          speedWindowSize,
		WindowSizeReconnectScale: 1,
	}
}
