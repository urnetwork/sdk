package sdk

import (
	"testing"
	"time"
)

func TestIdleMemoryTrimmerWaitsForCompleteQuietEpoch(t *testing.T) {
	stop := make(chan struct{})
	activity := make(chan struct{}, 1)
	trimmed := make(chan time.Time, 2)
	done := make(chan struct{})
	const quietDelay = 50 * time.Millisecond
	go func() {
		defer close(done)
		runIdleMemoryTrimmer(stop, activity, quietDelay, func() {
			trimmed <- time.Now()
		})
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle memory trimmer did not stop")
		}
	}()

	time.Sleep(30 * time.Millisecond)
	activityAt := time.Now()
	activity <- struct{}{}
	select {
	case at := <-trimmed:
		t.Fatalf("trim ran only %s after activity", at.Sub(activityAt))
	case <-time.After(30 * time.Millisecond):
	}

	select {
	case at := <-trimmed:
		if elapsed := at.Sub(activityAt); elapsed < quietDelay {
			t.Fatalf("trim ran only %s after activity, want at least %s", elapsed, quietDelay)
		}
	case <-time.After(4 * quietDelay):
		t.Fatal("trim did not run after a complete quiet epoch")
	}

	select {
	case <-trimmed:
		t.Fatal("trim repeated without new activity")
	case <-time.After(2 * quietDelay):
	}

	activity <- struct{}{}
	select {
	case <-trimmed:
	case <-time.After(4 * quietDelay):
		t.Fatal("new activity did not arm another quiet epoch")
	}
}

func TestPacketStatsTrafficActivityThreshold(t *testing.T) {
	stats := &PacketStats{
		RemoteEgressByteCount:  1024,
		RemoteIngressByteCount: 2048,
		LocalEgressByteCount:   512,
		LocalIngressByteCount:  256,
		BlockEgressByteCount:   128,
		BlockIngressByteCount:  64,
	}
	if got, want := packetStatsTrafficByteCount(stats), ByteCount(4032); got != want {
		t.Fatalf("packetStatsTrafficByteCount = %d, want %d", got, want)
	}
	if got := packetStatsTrafficDelta(4000, 4032); got != 32 {
		t.Fatalf("packetStatsTrafficDelta = %d, want 32", got)
	}
	if got := packetStatsTrafficDelta(4032, 4000); got != 0 {
		t.Fatalf("counter reset delta = %d, want 0", got)
	}
	if mobileIdleMemoryActivityMinByteCount != 4*1024 {
		t.Fatalf(
			"mobile activity threshold = %d, want 4096",
			mobileIdleMemoryActivityMinByteCount,
		)
	}
}

func TestMobileMemoryReclaimerGatesTargetFlightAndCooldown(t *testing.T) {
	now := time.Unix(100, 0)
	snapshot := mobileMemoryReclaimSnapshot{}
	reclaimCount := 0
	reclaimer := &mobileMemoryReclaimer{
		targetByteCount:         20,
		physicalTargetByteCount: func() int64 { return 40 },
		maxPoolOutstanding:      2,
		quietRetry:              15 * time.Second,
		cooldown:                time.Minute,
		now:                     func() time.Time { return now },
		sample:                  func() mobileMemoryReclaimSnapshot { return snapshot },
		reclaim:                 func() { reclaimCount += 1 },
	}

	snapshot = mobileMemoryReclaimSnapshot{runtimeByteCount: 20}
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimBelowTarget || got.retryAfter != 0 {
		t.Fatalf("below-target result = %+v", got)
	}
	snapshot = mobileMemoryReclaimSnapshot{runtimeByteCount: 21, poolOutstanding: 3}
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimInFlight || got.retryAfter != 15*time.Second {
		t.Fatalf("in-flight result = %+v", got)
	}
	snapshot.poolOutstanding = 2
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimed || reclaimCount != 1 {
		t.Fatalf("first reclaim result = %+v count=%d", got, reclaimCount)
	}
	now = now.Add(10 * time.Second)
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimCooldown || got.retryAfter != 50*time.Second {
		t.Fatalf("cooldown result = %+v", got)
	}
	now = now.Add(50 * time.Second)
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimed || reclaimCount != 2 {
		t.Fatalf("post-cooldown result = %+v count=%d", got, reclaimCount)
	}
	now = now.Add(time.Minute)
	snapshot = mobileMemoryReclaimSnapshot{runtimeByteCount: 19, physicalByteCount: 41}
	if got := reclaimer.attempt(); got.outcome != mobileMemoryReclaimed || reclaimCount != 3 {
		t.Fatalf("physical-pressure result = %+v count=%d", got, reclaimCount)
	}
}

func TestMobileMemoryPressureRearmsOnlyAfterMaterialAboveTargetDrop(t *testing.T) {
	const mib = int64(1024 * 1024)
	for _, testCase := range []struct {
		name   string
		before int64
		after  int64
		target int64
		want   bool
	}{
		{name: "material still high", before: 30 * mib, after: 21 * mib, target: 20 * mib, want: true},
		{name: "reached target", before: 30 * mib, after: 20 * mib, target: 20 * mib},
		{name: "immaterial high floor", before: 21*mib + mib/2, after: 21 * mib, target: 20 * mib},
		{name: "counter anomaly", before: 20 * mib, after: 21 * mib, target: 20 * mib},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mobileMemoryPressureShouldRearm(
				testCase.before,
				testCase.after,
				testCase.target,
			); got != testCase.want {
				t.Fatalf("rearm = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestMobilePhysicalPressureTransitionHasHysteresis(t *testing.T) {
	const threshold = int64(40)
	if armed, signal := mobilePhysicalPressureTransition(39, threshold, false); armed || signal {
		t.Fatalf("below threshold transition = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobilePhysicalPressureTransition(40, threshold, false); !armed || !signal {
		t.Fatalf("threshold transition = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobilePhysicalPressureTransition(41, threshold, true); !armed || signal {
		t.Fatalf("armed transition = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobilePhysicalPressureTransition(37, threshold, true); !armed || signal {
		t.Fatalf("hysteresis transition = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobilePhysicalPressureTransition(36, threshold, true); armed || signal {
		t.Fatalf("reset transition = armed=%v signal=%v", armed, signal)
	}
}

func TestMobileRuntimePressureTransitionSignalsEachCeilingCrossing(t *testing.T) {
	const target = int64(20)
	if armed, signal := mobileRuntimePressureTransition(20, target, false); armed || signal {
		t.Fatalf("at-target transition = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobileRuntimePressureTransition(21, target, false); !armed || !signal {
		t.Fatalf("first crossing = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobileRuntimePressureTransition(22, target, true); !armed || signal {
		t.Fatalf("continuous high-water = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobileRuntimePressureTransition(19, target, true); armed || signal {
		t.Fatalf("below-target reset = armed=%v signal=%v", armed, signal)
	}
	if armed, signal := mobileRuntimePressureTransition(21, target, false); !armed || !signal {
		t.Fatalf("second crossing = armed=%v signal=%v", armed, signal)
	}
}

func TestMobileRuntimePressureCrossingArmsOneQuietEpoch(t *testing.T) {
	previousStarted := mobileIdleMemoryTrimmerStarted.Load()
	previousArmed := mobileRuntimePressureArmed.Load()
	t.Cleanup(func() {
		mobileIdleMemoryTrimmerStarted.Store(previousStarted)
		mobileRuntimePressureArmed.Store(previousArmed)
		select {
		case <-mobileIdleMemoryActivity:
		default:
		}
	})
	select {
	case <-mobileIdleMemoryActivity:
	default:
	}
	mobileRuntimePressureArmed.Store(false)
	mobileIdleMemoryTrimmerStarted.Store(false)
	noteMobileRuntimeFootprint(int64(mobileSteadyMemoryTargetByteCount) + 1)
	if mobileRuntimePressureArmed.Load() {
		t.Fatal("runtime crossing was consumed before the trimmer started")
	}
	mobileIdleMemoryTrimmerStarted.Store(true)

	noteMobileRuntimeFootprint(int64(mobileSteadyMemoryTargetByteCount) + 1)
	select {
	case <-mobileIdleMemoryActivity:
	default:
		t.Fatal("runtime ceiling crossing did not arm a quiet epoch")
	}
	noteMobileRuntimeFootprint(int64(mobileSteadyMemoryTargetByteCount) + 2)
	select {
	case <-mobileIdleMemoryActivity:
		t.Fatal("continuous runtime high-water armed a second epoch")
	default:
	}
	noteMobileRuntimeFootprint(int64(mobileSteadyMemoryTargetByteCount))
	noteMobileRuntimeFootprint(int64(mobileSteadyMemoryTargetByteCount) + 1)
	select {
	case <-mobileIdleMemoryActivity:
	default:
		t.Fatal("second runtime ceiling crossing did not re-arm")
	}
}

func TestMobilePhysicalPressureWaitsForTrimmerStartup(t *testing.T) {
	previousStarted := mobileIdleMemoryTrimmerStarted.Load()
	previousArmed := mobilePhysicalPressureArmed.Load()
	previousCount := mobilePhysicalPressureCount.Load()
	previousThreshold := mobilePhysicalPressureByteCount.Load()
	t.Cleanup(func() {
		mobileIdleMemoryTrimmerStarted.Store(previousStarted)
		mobilePhysicalPressureArmed.Store(previousArmed)
		mobilePhysicalPressureCount.Store(previousCount)
		mobilePhysicalPressureByteCount.Store(previousThreshold)
		select {
		case <-mobileIdleMemoryActivity:
		default:
		}
	})
	select {
	case <-mobileIdleMemoryActivity:
	default:
	}
	mobileIdleMemoryTrimmerStarted.Store(false)
	mobilePhysicalPressureArmed.Store(false)
	mobilePhysicalPressureCount.Store(0)
	mobilePhysicalPressureByteCount.Store(40)

	noteMobilePhysicalFootprint(41)
	if mobilePhysicalPressureArmed.Load() || mobilePhysicalPressureCount.Load() != 0 {
		t.Fatal("physical crossing was consumed before the trimmer started")
	}
	mobileIdleMemoryTrimmerStarted.Store(true)
	noteMobilePhysicalFootprint(41)
	if !mobilePhysicalPressureArmed.Load() || mobilePhysicalPressureCount.Load() != 1 {
		t.Fatal("physical crossing did not arm after the trimmer started")
	}
	select {
	case <-mobileIdleMemoryActivity:
	default:
		t.Fatal("physical crossing did not arm a quiet epoch")
	}
}

func TestIdleMemoryTrimmerRetriesDeferredMaintenance(t *testing.T) {
	stop := make(chan struct{})
	activity := make(chan struct{}, 1)
	attempts := make(chan int, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		runIdleMemoryTrimmerWithRetry(
			stop,
			activity,
			10*time.Millisecond,
			func() time.Duration {
				count += 1
				attempts <- count
				if count == 1 {
					return 10 * time.Millisecond
				}
				return 0
			},
		)
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("retry trimmer did not stop")
		}
	}()
	for want := 1; want <= 2; want += 1 {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("attempt = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("maintenance attempt %d did not run", want)
		}
	}
	select {
	case got := <-attempts:
		t.Fatalf("maintenance repeated after success: %d", got)
	case <-time.After(30 * time.Millisecond):
	}
}
