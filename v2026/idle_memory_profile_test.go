package sdk

import (
	"math"
	"runtime/debug"
	"testing"
	"time"
)

func TestMobileRuntimeReclaimProfileThresholds(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct{ limit, want int64 }{
		{-1, 24 * mib}, {0, 24 * mib}, {24 * mib, 24 * mib},
		{32 * mib, 24 * mib}, {40 * mib, 30 * mib}, {64 * mib, 48 * mib},
		{64*mib + 3, 48*mib + 2}, {math.MaxInt64, math.MaxInt64/4*3 + 2},
	} {
		if got := mobileRuntimeReclaimTargetForLimit(tc.limit); got != tc.want {
			t.Errorf("soft limit=%d threshold=%d want=%d", tc.limit, got, tc.want)
		}
	}
	if mobilePhysicalFootprintReclaimByteCount != 40*mib {
		t.Fatal("Go profile scaling altered the independent physical pressure trigger")
	}
}

func TestMobileRuntimeReclaimDoesNotTrimExpandedAllowanceEarly(t *testing.T) {
	const mib = int64(1 << 20)
	limit := int64(64 * mib)
	state := mobileMemoryReclaimSnapshot{runtimeByteCount: 32 * mib}
	now := time.Unix(100, 0)
	calls := 0
	r := &mobileMemoryReclaimer{
		targetByteCount:         func() int64 { return mobileRuntimeReclaimTargetForLimit(limit) },
		physicalTargetByteCount: func() int64 { return 40 * mib },
		maxPoolOutstanding:      16, quietRetry: 2 * time.Second, cooldown: time.Minute,
		now: func() time.Time { return now }, sample: func() mobileMemoryReclaimSnapshot { return state },
		reclaim: func() { calls++ },
	}
	for _, usage := range []int64{24*mib + 1, 32 * mib, 48 * mib} {
		state.runtimeByteCount = usage
		if got := r.attempt(); got.outcome != mobileMemoryReclaimBelowTarget || calls != 0 {
			t.Fatalf("expanded allowance trimmed early at %d: %+v calls=%d", usage, got, calls)
		}
	}
	state.runtimeByteCount = 48*mib + 1
	if got := r.attempt(); got.outcome != mobileMemoryReclaimed || calls != 1 {
		t.Fatalf("crossing expanded pressure boundary did not reclaim: %+v calls=%d", got, calls)
	}
	if got := r.attempt(); got.outcome != mobileMemoryReclaimCooldown || calls != 1 {
		t.Fatal("expanded threshold bypassed the existing cooldown")
	}
	now = now.Add(time.Minute)
	state.runtimeByteCount, state.physicalByteCount = 32*mib, 40*mib+1
	if got := r.attempt(); got.outcome != mobileMemoryReclaimed || calls != 2 {
		t.Fatal("expanded Go allowance masked independent physical pressure")
	}
	now = now.Add(time.Minute)
	state.physicalByteCount = 0
	limit = 32 * mib
	if got := r.attempt(); got.outcome != mobileMemoryReclaimed || calls != 3 {
		t.Fatal("lowering the effective soft limit left a stale higher reclaim threshold")
	}
}

func TestMobileRuntimeProfilePressureLatchUsesCurrentLimit(t *testing.T) {
	previous := debug.SetMemoryLimit(64 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
	target := currentMobileRuntimeReclaimTarget()
	if target != 48<<20 {
		t.Fatalf("runtime threshold does not use actual Go soft limit: %d", target)
	}
	if armed, signal := mobileRuntimePressureTransition(32<<20, target, true); armed || signal {
		t.Fatal("raising the limit did not clear an old pressure latch below the new threshold")
	}
	if armed, signal := mobileRuntimePressureTransition(48<<20+1, target, false); !armed || !signal {
		t.Fatal("new high-water did not arm one quiet epoch")
	}
	if armed, signal := mobileRuntimePressureTransition(49<<20, target, true); !armed || signal {
		t.Fatal("continuous new high-water armed repeatedly")
	}
	debug.SetMemoryLimit(32 << 20)
	if currentMobileRuntimeReclaimTarget() != 24<<20 {
		t.Fatal("iOS-profile runtime pressure boundary changed")
	}
}

func TestMobileRuntimeProfileSamplerDoesNotArmBelowExpandedLimit(t *testing.T) {
	limit := debug.SetMemoryLimit(64 << 20)
	started, armed := mobileIdleMemoryTrimmerStarted.Load(), mobileRuntimePressureArmed.Load()
	t.Cleanup(func() {
		debug.SetMemoryLimit(limit)
		mobileIdleMemoryTrimmerStarted.Store(started)
		mobileRuntimePressureArmed.Store(armed)
		select {
		case <-mobileIdleMemoryActivity:
		default:
		}
	})
	mobileIdleMemoryTrimmerStarted.Store(true)
	mobileRuntimePressureArmed.Store(false)
	select {
	case <-mobileIdleMemoryActivity:
	default:
	}
	for _, value := range []int64{24<<20 + 1, 32 << 20, 48 << 20} {
		noteMobileRuntimeFootprint(value)
		if mobileRuntimePressureArmed.Load() {
			t.Fatal("sampler armed below the expanded pressure threshold")
		}
		select {
		case <-mobileIdleMemoryActivity:
			t.Fatal("sampler signaled an early quiet maintenance epoch")
		default:
		}
	}
	noteMobileRuntimeFootprint(48<<20 + 1)
	select {
	case <-mobileIdleMemoryActivity:
	default:
		t.Fatal("sampler failed to arm at the expanded pressure boundary")
	}
	noteMobileRuntimeFootprint(49 << 20)
	select {
	case <-mobileIdleMemoryActivity:
		t.Fatal("continuous pressure repeated a maintenance signal")
	default:
	}
}
