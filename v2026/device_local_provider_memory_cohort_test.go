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
	telemetry.record(start, first)
	telemetry.record(start.Add(20*time.Millisecond), peak)
	telemetry.record(start.Add(60*time.Millisecond), last)
	if telemetry.AtPeak != peak || telemetry.PeakElapsed != 20*time.Millisecond || telemetry.PeakHeapAlloc != 100 || telemetry.PeakHeapInuse != 120 || telemetry.PeakSys != 300 {
		t.Fatalf("total-runtime peak attribution was confused with component maxima: %+v", telemetry)
	}
	if telemetry.Samples != 3 || telemetry.Cadence != 20*time.Millisecond || telemetry.Duration != 60*time.Millisecond || telemetry.MaxGap != 40*time.Millisecond || telemetry.FirstGC != 5 || telemetry.LastGC != 8 {
		t.Fatalf("sampling jitter or GC interval was hidden: %+v", telemetry)
	}
	if allocations := testing.AllocsPerRun(1000, func() { telemetry.record(start.Add(time.Second), last) }); allocations != 0 {
		t.Fatalf("recording a sample added allocation pressure to the workload: %v", allocations)
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
}
