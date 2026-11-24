package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	providerMemoryProcesses = 3
	providerMemoryCPUs      = 2
)

// All three fresh processes count. This is a predeclared cohort, not retries:
// a failed peak is never replaced by a later passing peak. A fixed CPU count
// makes per-P allocator/GC state comparable with the canonical two-core SDK
// lane, independent of the parent package's or host's scheduling configuration.
func runProviderMemoryCohort(t *testing.T) bool {
	t.Helper()
	if os.Getenv(isolatedLoadTestEnv) == t.Name() {
		if runtime.GOMAXPROCS(0) != providerMemoryCPUs {
			t.Fatalf("provider memory child GOMAXPROCS=%d, want %d", runtime.GOMAXPROCS(0), providerMemoryCPUs)
		}
		return false
	}
	err := providerMemoryCohort(func(index int) error {
		timeout := 2 * time.Minute
		if deadline, ok := t.Deadline(); ok {
			timeout = min(timeout, time.Until(deadline)-time.Second)
		}
		if timeout <= 0 {
			return errors.New("parent test deadline exhausted")
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], providerMemoryChildArgs(t.Name(), timeout)...)
		cmd.Env = providerMemoryChildEnv(os.Environ(), t.Name())
		output, err := cmd.CombinedOutput()
		t.Logf("provider memory process %d/%d (GOMAXPROCS=%d):\n%s", index, providerMemoryProcesses, providerMemoryCPUs, output)
		return err
	})
	if err != nil {
		t.Errorf("provider memory cohort failed: %v", err)
	}
	return true
}

func providerMemoryCohort(run func(int) error) error {
	var failures error
	for index := 1; index <= providerMemoryProcesses; index++ {
		if err := run(index); err != nil {
			failures = errors.Join(failures, fmt.Errorf("process %d/%d: %w", index, providerMemoryProcesses, err))
		}
	}
	return failures
}

func providerMemoryChildArgs(name string, timeout time.Duration) []string {
	return []string{"-test.run=^" + regexp.QuoteMeta(name) + "$", "-test.count=1", "-test.timeout=" + timeout.String(), "-test.v"}
}

func providerMemoryChildEnv(inherited []string, name string) []string {
	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if key != isolatedLoadTestEnv && key != "GOMAXPROCS" {
			env = append(env, entry)
		}
	}
	return append(env, isolatedLoadTestEnv+"="+name, "GOMAXPROCS="+strconv.Itoa(providerMemoryCPUs))
}

func TestProviderMemoryCohortKeepsEveryFailure(t *testing.T) {
	first, last := errors.New("first peak exceeded"), errors.New("last load failed")
	var calls []int
	err := providerMemoryCohort(func(index int) error {
		calls = append(calls, index)
		if index == 1 {
			return first
		}
		if index == 3 {
			return last
		}
		return nil
	})
	if !reflect.DeepEqual(calls, []int{1, 2, 3}) || !errors.Is(err, first) || !errors.Is(err, last) {
		t.Fatalf("cohort discarded a failure or changed its predetermined size: calls=%v err=%v", calls, err)
	}
	if err := providerMemoryCohort(func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestProviderMemoryChildContract(t *testing.T) {
	name := "TestFixture.With+Regex"
	args := providerMemoryChildArgs(name, 2*time.Minute)
	if !reflect.DeepEqual(args, []string{"-test.run=^TestFixture\\.With\\+Regex$", "-test.count=1", "-test.timeout=2m0s", "-test.v"}) {
		t.Fatalf("child did not select precisely one uncached bounded test: %v", args)
	}
	inherited := []string{"KEEP=this", "GOMAXPROCS=14", isolatedLoadTestEnv + "=stale", "GOMAXPROCS=8"}
	before := append([]string(nil), inherited...)
	env := providerMemoryChildEnv(inherited, name)
	if !reflect.DeepEqual(env, []string{"KEEP=this", isolatedLoadTestEnv + "=" + name, "GOMAXPROCS=2"}) || !reflect.DeepEqual(inherited, before) {
		t.Fatalf("child CPU/selection policy is inherited or mutated: env=%v inherited=%v", env, inherited)
	}
}

func TestProviderMemoryTelemetryKeepsPeakAttributionAndCadence(t *testing.T) {
	start := time.Unix(0, 0)
	var telemetry peakMemoryTelemetry
	first := providerMemorySnapshot{RuntimeBytes: 100, HeapAllocBytes: 60, HeapInuseBytes: 70, SysBytes: 150, NumGC: 5, Goroutines: 7}
	peak := providerMemorySnapshot{RuntimeBytes: 200, HeapAllocBytes: 80, HeapInuseBytes: 90, SysBytes: 250, NumGC: 6, Goroutines: 9}
	last := providerMemorySnapshot{RuntimeBytes: 180, HeapAllocBytes: 100, HeapInuseBytes: 120, SysBytes: 300, NumGC: 8, Goroutines: 8}
	telemetry.record(start, first, peakMemoryFootprint{Bytes: 90, CyclesBefore: 5, CyclesAfter: 5})
	telemetry.record(start.Add(20*time.Millisecond), peak, peakMemoryFootprint{Bytes: 190, CyclesBefore: 6, CyclesAfter: 6})
	lastFootprint := peakMemoryFootprint{Bytes: 170, CyclesBefore: 8, CyclesAfter: 8}
	telemetry.record(start.Add(60*time.Millisecond), last, lastFootprint)
	if telemetry.AtPeak != peak || telemetry.PeakElapsed != 20*time.Millisecond || telemetry.PeakHeapAlloc != 100 || telemetry.PeakHeapInuse != 120 || telemetry.PeakSys != 300 {
		t.Fatalf("total-runtime peak attribution was confused with component maxima: %+v", telemetry)
	}
	if telemetry.Samples != 3 || telemetry.Cadence != 20*time.Millisecond || telemetry.Duration != 60*time.Millisecond || telemetry.MaxGap != 40*time.Millisecond || telemetry.FirstGC != 5 || telemetry.LastGC != 8 {
		t.Fatalf("sampling jitter or GC interval was hidden: %+v", telemetry)
	}
	// counted under record's own frame, so another goroutine allocating in the
	// window, as an earlier test's teardown does in a whole-package run, cannot
	// count as the sample's
	recordFrame := runtime.FuncForPC(reflect.ValueOf((*peakMemoryTelemetry).record).Pointer()).Name()
	if allocations := profiledAllocationsUnder(t, recordFrame, false, func() {
		for range 1000 {
			telemetry.record(start.Add(time.Second), last, lastFootprint)
		}
	}); allocations != 0 {
		t.Fatalf("recording a sample added allocation pressure to the workload: %d objects in 1000 samples", allocations)
	}
}

// What a collection frees after a sample does not count toward the settled
// peak: the 34-MiB excursion is settled by the first reading three completed
// collections after it, at 25 MiB, and the readings the load ended before
// settling are settled by the end's forced collection.
func TestProviderMemorySettledPeakDropsWhatTheCollectionsFree(t *testing.T) {
	const mib = int64(1024 * 1024)
	start := time.Unix(0, 0)
	var telemetry peakMemoryTelemetry
	record := func(index int, footprintMiB int64, cyclesBefore int64, cyclesAfter int64) {
		telemetry.record(
			start.Add(time.Duration(index)*20*time.Millisecond),
			providerMemorySnapshot{RuntimeBytes: footprintMiB * mib, NumGC: cyclesAfter},
			peakMemoryFootprint{Bytes: footprintMiB * mib, CyclesBefore: cyclesBefore, CyclesAfter: cyclesAfter},
		)
	}
	record(0, 20, 10, 10)
	record(1, 34, 10, 11)
	record(2, 30, 12, 12)
	if telemetry.SettledPeak != 0 {
		t.Fatalf("a sample settled before three collections completed after it: %d", telemetry.SettledPeak)
	}
	record(3, 25, 14, 14)
	if telemetry.SettledPeak != 25*mib {
		t.Fatalf("settled peak %d after the excursion's collections, want 25 MiB", telemetry.SettledPeak)
	}
	telemetry.settleAll(peakMemoryFootprint{Bytes: 22 * mib, CyclesBefore: 15, CyclesAfter: 15})
	if telemetry.SettledPeak != 25*mib || telemetry.SettledAtEnd != 2 || telemetry.AtPeak.RuntimeBytes != 34*mib {
		t.Fatalf("settled peak %d with %d settled at the end and raw peak %d, want 25 MiB, 2 and 34 MiB", telemetry.SettledPeak, telemetry.SettledAtEnd, telemetry.AtPeak.RuntimeBytes)
	}
}

// A footprint the collections could not bring down stays in the settled peak:
// the provider holds it, not the runtime.
func TestProviderMemorySettledPeakKeepsWhatTheCollectionsLeave(t *testing.T) {
	const mib = int64(1024 * 1024)
	start := time.Unix(0, 0)
	var telemetry peakMemoryTelemetry
	for index, footprint := range []peakMemoryFootprint{
		{Bytes: 40 * mib, CyclesBefore: 20, CyclesAfter: 20},
		{Bytes: 41 * mib, CyclesBefore: 21, CyclesAfter: 22},
		{Bytes: 39 * mib, CyclesBefore: 23, CyclesAfter: 23},
	} {
		telemetry.record(start.Add(time.Duration(index)*20*time.Millisecond), providerMemorySnapshot{RuntimeBytes: footprint.Bytes}, footprint)
	}
	if telemetry.SettledPeak != 39*mib {
		t.Fatalf("settled peak %d, want the 39 MiB that three more collections left", telemetry.SettledPeak)
	}
}

// With every slot held, a reading joins the newest slot, which then settles
// only as far after that reading as the rule asks: the reading three
// collections past the older slots but not past the joined one leaves it held.
func TestProviderMemorySettlingWaitsForTheNewestHeldReading(t *testing.T) {
	start := time.Unix(0, 0)
	var telemetry peakMemoryTelemetry
	footprints := []peakMemoryFootprint{
		{Bytes: 10, CyclesBefore: 0, CyclesAfter: 5},
		{Bytes: 11, CyclesBefore: 1, CyclesAfter: 6},
		{Bytes: 12, CyclesBefore: 2, CyclesAfter: 7},
		{Bytes: 13, CyclesBefore: 3, CyclesAfter: 8},
		// every slot is held: this one joins the newest
		{Bytes: 14, CyclesBefore: 4, CyclesAfter: 9},
		// three past the slots at 5, 6 and 7, two past the joined one at 9
		{Bytes: 1, CyclesBefore: 11, CyclesAfter: 11},
	}
	if len(footprints)-2 != len(telemetry.settling) {
		t.Fatalf("the test fills %d slots, the telemetry holds %d", len(footprints)-2, len(telemetry.settling))
	}
	for index, footprint := range footprints {
		telemetry.record(start.Add(time.Duration(index)*20*time.Millisecond), providerMemorySnapshot{RuntimeBytes: footprint.Bytes}, footprint)
	}
	if telemetry.SettledPeak != 1 {
		t.Fatalf("settled peak %d after the older slots settled, want 1", telemetry.SettledPeak)
	}
	telemetry.settleAll(peakMemoryFootprint{Bytes: 100, CyclesBefore: 20, CyclesAfter: 20})
	if telemetry.SettledPeak != 14 || telemetry.SettledAtEnd != 3 {
		t.Fatalf("settled peak %d with %d settled at the end, want 14 and 3", telemetry.SettledPeak, telemetry.SettledAtEnd)
	}
}

func TestProviderMemorySamplerJoinsAndIncludesFinalSample(t *testing.T) {
	sampler := startPeakSampler()
	total, heap, goroutines := sampler.stop()
	// /gc/heap/live may still be zero before the fresh parent process's first
	// collection; current HeapAlloc, not last-GC live bytes, proves a read.
	if total <= 0 || heap < 0 || goroutines <= 0 || sampler.telemetry.AtPeak.HeapAllocBytes <= 0 || sampler.telemetry.Samples < 2 || sampler.telemetry.AtPeak.RuntimeBytes != total {
		t.Fatalf("missing initial/final measurement: total=%d heap=%d goroutines=%d telemetry=%+v", total, heap, goroutines, sampler.telemetry)
	}
	select {
	case <-sampler.doneCh:
	default:
		t.Fatal("stop returned with the sampler still running")
	}
	// the final sample cannot settle itself, so the end's collection settles
	// at least that one
	if settled := sampler.settle(); settled <= 0 || sampler.telemetry.SettledAtEnd < 1 {
		t.Fatalf("settle left the settled peak at %d with %d samples settled at the end", settled, sampler.telemetry.SettledAtEnd)
	}
}
