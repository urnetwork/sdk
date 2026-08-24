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

// A 512-KiB packet reuse set spends 256 KiB of the new headroom to avoid a cold
// allocation wave after reclaim. Together with the two fixed 256-KiB
// large-object floors it keeps 1 MiB of immediately reusable buffers, while
// the unchanged capacity can absorb a later burst.
const mobilePacketPoolWarmByteCount ByteCount = 512 * 1024

func defaultDeviceLocalMemoryTargetByteCountForPlatform(mobile bool) ByteCount {
	if mobile {
		return mobileSteadyMemoryTargetByteCount
	}
	return defaultDeviceLocalMemoryTargetByteCount
}

const (
	// Keep the measured H3-safe sixteen-message ceiling. A 32-message experiment
	// improved H1 bulk traffic but a stalled H3 page reached 29.30 MiB even with
	// the 512-root aggregate gate, because already-admitted work drains
	// asynchronously. The 24-MiB profile spends headroom outside this multiplier.
	mobileClientSequenceBufferMaxCount                = 16
	mobileReceiveSequenceBufferMaxByteCount           = 128 * 1024
	mobileResendQueueMinByteCount                     = 64 * 1024
	mobileResendQueueMaxByteCount                     = 512 * 1024
	mobileReceiveQueueMinByteCount                    = 96 * 1024
	mobileReceiveQueueMaxByteCount                    = 768 * 1024
	mobileUnreliableFlightMaxByteCount                = 128 * 1024
	mobileUnreliableFlightMaxMessageCount             = 16
	mobileQualityWindowSize                           = 4
	mobileSpeedWindowSize                             = 1
	mobilePacketGroupMaxPacketCount                   = 16
	mobilePacketGroupMaxByteCount           ByteCount = 24 * 1024
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
		send.AckBufferSize = min(send.AckBufferSize, mobileClientSequenceBufferMaxCount)
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
		receive.SequenceBufferSize = min(
			receive.SequenceBufferSize,
			mobileClientSequenceBufferMaxCount,
		)
		receive.SequenceBufferByteCount = min(
			receive.SequenceBufferByteCount,
			mobileReceiveSequenceBufferMaxByteCount,
		)
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
