package sdk

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// Mobile packet-tunnel processes have a tight resident budget but also see
// short, allocation-heavy bursts. A 15-second payload-quiet debounce reclaims
// a measured high-water before iOS remains near jetsam for a full minute. The
// one-minute cooldown bounds forced-GC battery/latency cost across repeated
// quiet epochs.
const mobileIdleMemoryTrimDelay = 15 * time.Second
const mobileIdleMemoryTrimCooldown = 60 * time.Second

// One or two control-frame buffers can remain borrowed at idle. Allow a small
// fixed control working set, but never collect under the thousands of owners a
// real transfer burst can create.
const mobileIdleMemoryMaxOutstandingPoolCount int64 = 16

// The extension's current documented baseline is roughly 16 MiB below a
// roughly 50-MiB jetsam boundary. Start a quiet reclaim at 40 MiB by default,
// leaving headroom for one last live burst. The iOS host may tune this to a
// measured device-family boundary through SetExtensionMemoryPressureByteCount.
const mobilePhysicalFootprintReclaimByteCount int64 = 40 * 1024 * 1024

// Packet statistics advance for small platform background flows even when the
// user-visible transfer is over. Treat an event epoch as burst activity only
// when at least 4 KiB moved. At the one-second production stats epoch this
// still protects traffic down to roughly 32 kbit/s, while allowing the measured
// sub-kilobyte keepalive/background trickle to become quiescent.
const mobileIdleMemoryActivityMinByteCount ByteCount = 4 * 1024

var (
	mobileIdleMemoryTrimmerOnce     sync.Once
	mobileIdleMemoryTrimmerStarted  atomic.Bool
	mobileIdleMemoryActivity        = make(chan struct{}, 1)
	mobileIdleMemoryTrimCount       atomic.Int64
	mobileIdleMemoryTrimDropped     atomic.Int64
	mobileIdleMemoryTrimDeferred    atomic.Int64
	mobileIdleMemoryTrimBelow       atomic.Int64
	mobileIdleMemoryTrimCooldowns   atomic.Int64
	mobileIdleMemoryTrimBefore      atomic.Int64
	mobileIdleMemoryTrimAfter       atomic.Int64
	mobilePhysicalPressureByteCount atomic.Int64
	mobilePhysicalPressureArmed     atomic.Bool
	mobilePhysicalPressureCount     atomic.Int64
	mobileRuntimePressureArmed      atomic.Bool
)

func init() {
	mobilePhysicalPressureByteCount.Store(mobilePhysicalFootprintReclaimByteCount)
}

type mobileMemoryReclaimSnapshot struct {
	runtimeByteCount  int64
	poolOutstanding   int64
	physicalByteCount int64
}

type mobileMemoryReclaimOutcome uint8

const (
	mobileMemoryReclaimBelowTarget mobileMemoryReclaimOutcome = iota + 1
	mobileMemoryReclaimInFlight
	mobileMemoryReclaimCooldown
	mobileMemoryReclaimed
)

type mobileMemoryReclaimResult struct {
	outcome    mobileMemoryReclaimOutcome
	retryAfter time.Duration
}

func mobileMemoryPressureShouldRearm(before, after, target int64) bool {
	return target < after &&
		int64(automaticIdleMemoryRebuildMinDroppedByteCount) <= max(int64(0), before-after)
}

// mobileMemoryReclaimer is the allocation-free decision core. Production
// injects runtime/pool counters and the reclaim operation; tests inject exact
// time and state so no scheduler sleeps are needed for policy coverage.
type mobileMemoryReclaimer struct {
	targetByteCount         int64
	physicalTargetByteCount func() int64
	maxPoolOutstanding      int64
	quietRetry              time.Duration
	cooldown                time.Duration
	now                     func() time.Time
	sample                  func() mobileMemoryReclaimSnapshot
	reclaim                 func()
	lastReclaimTime         time.Time
}

func (self *mobileMemoryReclaimer) attempt() mobileMemoryReclaimResult {
	snapshot := self.sample()
	physicalTargetByteCount := int64(0)
	if self.physicalTargetByteCount != nil {
		physicalTargetByteCount = self.physicalTargetByteCount()
	}
	if snapshot.runtimeByteCount <= self.targetByteCount &&
		(physicalTargetByteCount <= 0 ||
			snapshot.physicalByteCount <= physicalTargetByteCount) {
		return mobileMemoryReclaimResult{outcome: mobileMemoryReclaimBelowTarget}
	}
	if self.maxPoolOutstanding < snapshot.poolOutstanding {
		return mobileMemoryReclaimResult{
			outcome:    mobileMemoryReclaimInFlight,
			retryAfter: self.quietRetry,
		}
	}
	now := self.now()
	if !self.lastReclaimTime.IsZero() {
		remaining := self.cooldown - now.Sub(self.lastReclaimTime)
		if 0 < remaining {
			return mobileMemoryReclaimResult{
				outcome:    mobileMemoryReclaimCooldown,
				retryAfter: remaining,
			}
		}
	}
	self.reclaim()
	self.lastReclaimTime = now
	return mobileMemoryReclaimResult{outcome: mobileMemoryReclaimed}
}

func mobileMemorySnapshot() mobileMemoryReclaimSnapshot {
	poolStats := connect.GetMessagePoolAggregateStats()
	return mobileMemoryReclaimSnapshot{
		runtimeByteCount: runtimeTotalByteCount(),
		poolOutstanding: max(
			int64(0),
			int64(poolStats.Taken)-int64(poolStats.Returned),
		),
		physicalByteCount: mobilePhysicalFootprintCurrent.Load(),
	}
}

func startMobileIdleMemoryTrimmer() {
	if runtime.GOOS != "android" && runtime.GOOS != "ios" {
		return
	}
	mobileIdleMemoryTrimmerOnce.Do(func() {
		mobileIdleMemoryTrimmerStarted.Store(true)
		go connect.HandleError(func() {
			reclaimer := &mobileMemoryReclaimer{
				targetByteCount:         int64(mobileSteadyMemoryTargetByteCount),
				physicalTargetByteCount: mobilePhysicalPressureByteCount.Load,
				maxPoolOutstanding:      mobileIdleMemoryMaxOutstandingPoolCount,
				quietRetry:              mobileIdleMemoryTrimDelay,
				cooldown:                mobileIdleMemoryTrimCooldown,
				now:                     time.Now,
				sample:                  mobileMemorySnapshot,
				reclaim: func() {
					before := runtimeTotalByteCount()
					connect.ShedMemory()
					droppedByteCount := rebuildMessagePools(
						true,
						mobilePacketPoolWarmByteCount,
					)
					after := runtimeTotalByteCount()
					mobileIdleMemoryTrimBefore.Store(before)
					mobileIdleMemoryTrimAfter.Store(after)
					mobileIdleMemoryTrimDropped.Store(int64(droppedByteCount))
					mobileIdleMemoryTrimCount.Add(1)
					// A burst can contain more than one generation of garbage. If
					// this pass made a material drop but still ended above a Go or
					// physical ceiling, allow one later sampler/host crossing to arm
					// another pass. The reclaimer cooldown keeps those attempts at
					// least a minute apart; once a pass is immaterial the latches stay
					// armed and an irreducible floor cannot create a GC loop.
					if mobileMemoryPressureShouldRearm(
						before,
						after,
						int64(mobileSteadyMemoryTargetByteCount),
					) {
						mobileRuntimePressureArmed.Store(false)
					}
					physicalTarget := mobilePhysicalPressureByteCount.Load()
					if 0 < physicalTarget &&
						physicalTarget < mobilePhysicalFootprintCurrent.Load() &&
						int64(automaticIdleMemoryRebuildMinDroppedByteCount) <=
							max(int64(0), before-after) {
						mobilePhysicalPressureArmed.Store(false)
					}
				},
			}
			runIdleMemoryTrimmerWithRetry(
				nil,
				mobileIdleMemoryActivity,
				mobileIdleMemoryTrimDelay,
				func() time.Duration {
					result := reclaimer.attempt()
					switch result.outcome {
					case mobileMemoryReclaimBelowTarget:
						mobileIdleMemoryTrimBelow.Add(1)
					case mobileMemoryReclaimInFlight:
						mobileIdleMemoryTrimDeferred.Add(1)
					case mobileMemoryReclaimCooldown:
						mobileIdleMemoryTrimCooldowns.Add(1)
					}
					return result.retryAfter
				},
			)
		})
	})
}

func mobilePhysicalPressureTransition(
	byteCount int64,
	threshold int64,
	armed bool,
) (nextArmed bool, signal bool) {
	if threshold <= 0 {
		return false, false
	}
	if threshold <= byteCount {
		return true, !armed
	}
	// Ten percent of hysteresis prevents a noisy kernel gauge from repeatedly
	// creating quiet epochs at the boundary.
	if byteCount <= threshold-threshold/10 {
		return false, false
	}
	return armed, false
}

func noteMobilePhysicalFootprint(byteCount int64) {
	// The extension can publish TASK_VM_INFO before SetMemoryLimit starts the
	// reclaimer. Do not consume the crossing in that interval; a later sample
	// must still be able to arm the first quiet epoch.
	if !mobileIdleMemoryTrimmerStarted.Load() {
		return
	}
	threshold := mobilePhysicalPressureByteCount.Load()
	for {
		armed := mobilePhysicalPressureArmed.Load()
		nextArmed, signal := mobilePhysicalPressureTransition(
			byteCount,
			threshold,
			armed,
		)
		if nextArmed == armed || mobilePhysicalPressureArmed.CompareAndSwap(armed, nextArmed) {
			if signal {
				mobilePhysicalPressureCount.Add(1)
				noteMobileMemoryActivity(mobileIdleMemoryActivityMinByteCount)
			}
			return
		}
	}
}

func mobileRuntimePressureTransition(
	byteCount int64,
	target int64,
	armed bool,
) (nextArmed bool, signal bool) {
	if target <= 0 || byteCount <= target {
		return false, false
	}
	return true, !armed
}

// noteMobileRuntimeFootprint closes the quiet-timer blind spot where runtime
// metadata/control churn crosses the steady target after a successful reclaim
// but no later TUN payload arrives to arm another epoch. The sampler invokes
// this only once per 15 seconds, and the transition latch plus the reclaimer's
// one-minute cooldown prevents periodic signal/GC loops while a high-water is
// continuously above target.
func noteMobileRuntimeFootprint(byteCount int64) {
	if !mobileIdleMemoryTrimmerStarted.Load() {
		return
	}
	for {
		armed := mobileRuntimePressureArmed.Load()
		nextArmed, signal := mobileRuntimePressureTransition(
			byteCount,
			int64(mobileSteadyMemoryTargetByteCount),
			armed,
		)
		if nextArmed == armed || mobileRuntimePressureArmed.CompareAndSwap(armed, nextArmed) {
			if signal {
				noteMobileMemoryActivity(mobileIdleMemoryActivityMinByteCount)
			}
			return
		}
	}
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
	runIdleMemoryTrimmerWithRetry(stop, activity, quietDelay, func() time.Duration {
		trim()
		return 0
	})
}

func runIdleMemoryTrimmerWithRetry(
	stop <-chan struct{},
	activity <-chan struct{},
	quietDelay time.Duration,
	maintenance func() time.Duration,
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
			retryAfter := maintenance()
			if 0 < retryAfter {
				timer.Reset(retryAfter)
				timerChannel = timer.C
			} else {
				// One rebuild (or a below-target decision) is sufficient for this
				// quiet epoch. Leave the timer disabled until later payload
				// activity starts another epoch.
				timerChannel = nil
			}
		}
	}
}
