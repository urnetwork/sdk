package sdk

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Mobile packet-tunnel processes have a tight resident budget but also see
// short, allocation-heavy bursts. Keep the large bounded pool capacity for
// those bursts, then rebuild only its returned free lists after a full minute
// with no routed packets. A buffered signal coalesces each packet-stat epoch;
// there is no polling wakeup while the tunnel stays idle.
const mobileIdleMemoryTrimDelay = 60 * time.Second

// Packet statistics advance for small platform background flows even when the
// user-visible transfer is over. Treat an event epoch as burst activity only
// when at least 4 KiB moved. At the one-second production stats epoch this
// still protects traffic down to roughly 32 kbit/s, while allowing the measured
// sub-kilobyte keepalive/background trickle to become quiescent.
const mobileIdleMemoryActivityMinByteCount ByteCount = 4 * 1024

var (
	mobileIdleMemoryTrimmerOnce    sync.Once
	mobileIdleMemoryTrimmerStarted atomic.Bool
	mobileIdleMemoryActivity       = make(chan struct{}, 1)
	mobileIdleMemoryTrimCount      atomic.Int64
	mobileIdleMemoryTrimDropped    atomic.Int64
)

func startMobileIdleMemoryTrimmer() {
	if runtime.GOOS != "android" && runtime.GOOS != "ios" {
		return
	}
	mobileIdleMemoryTrimmerOnce.Do(func() {
		mobileIdleMemoryTrimmerStarted.Store(true)
		go connect.HandleError(func() {
			runIdleMemoryTrimmer(
				nil,
				mobileIdleMemoryActivity,
				mobileIdleMemoryTrimDelay,
				func() {
					if droppedByteCount := trimMemory(false); 0 < droppedByteCount {
						mobileIdleMemoryTrimDropped.Store(int64(droppedByteCount))
						mobileIdleMemoryTrimCount.Add(1)
					}
				},
			)
		})
	})
}

// Packet-stat callbacks are already coalesced on the transport event epoch,
// so this stays off the per-packet hot path. The nonblocking send also lets a
// busy epoch collapse into the one reset the timer actually needs.
func noteMobileMemoryActivity(byteCount ByteCount) {
	if byteCount < mobileIdleMemoryActivityMinByteCount {
		return
	}
	if !mobileIdleMemoryTrimmerStarted.Load() {
		return
	}
	select {
	case mobileIdleMemoryActivity <- struct{}{}:
	default:
	}
}

func packetStatsTrafficByteCount(stats *PacketStats) ByteCount {
	if stats == nil {
		return 0
	}
	return max(ByteCount(0), stats.RemoteEgressByteCount) +
		max(ByteCount(0), stats.RemoteIngressByteCount) +
		max(ByteCount(0), stats.LocalEgressByteCount) +
		max(ByteCount(0), stats.LocalIngressByteCount) +
		max(ByteCount(0), stats.BlockEgressByteCount) +
		max(ByteCount(0), stats.BlockIngressByteCount)
}

func packetStatsTrafficDelta(previous ByteCount, current ByteCount) ByteCount {
	if current <= previous {
		return 0
	}
	return current - previous
}

func runIdleMemoryTrimmer(
	stop <-chan struct{},
	activity <-chan struct{},
	quietDelay time.Duration,
	trim func(),
) {
	if quietDelay <= 0 {
		panic("idle memory trim delay must be positive")
	}
	timer := time.NewTimer(quietDelay)
	defer timer.Stop()
	timerChannel := timer.C
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(quietDelay)
		timerChannel = timer.C
	}

	for {
		select {
		case <-stop:
			return
		case <-activity:
			reset()
		case <-timerChannel:
			// If activity and the deadline became ready together, activity wins:
			// require one complete new quiet interval before touching free lists.
			select {
			case <-activity:
				reset()
				continue
			default:
			}
			trim()
			// One rebuild is sufficient for this quiet epoch. Leave the timer
			// disabled until a later packet-stat event starts another epoch.
			timerChannel = nil
		}
	}
}
