//go:build !ios

package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type androidH1MemoryArm struct {
	SizingMiB, SoftMiB int64
}

func androidH1MemoryExperimentArm(name string) (androidH1MemoryArm, bool) {
	switch name {
	case "40-40":
		return androidH1MemoryArm{40, 40}, true
	case "40-64":
		return androidH1MemoryArm{40, 64}, true
	case "64-64":
		return androidH1MemoryArm{64, 64}, true
	default:
		return androidH1MemoryArm{}, false
	}
}

func TestAndroidH1MemoryExperimentPolicy(t *testing.T) {
	previous := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previous) })
	for _, name := range []string{"40-40", "40-64", "64-64"} {
		t.Run(name, func(t *testing.T) {
			arm, ok := androidH1MemoryExperimentArm(name)
			if !ok || arm.SizingMiB > arm.SoftMiB {
				t.Fatal("invalid declared arm")
			}
			connect.SetMemoryBudget(arm.SizingMiB << 20)
			packet, large := messagePoolMemoryTargetsForPlatform(arm.SizingMiB<<20, true)
			if packet != 256<<10 || large != 512<<10 || gcPercentForPlatform("android") != 25 {
				t.Fatal("experiment changed the fixed mobile returned-pool or GC policy")
			}
			device, _ := transferMemoryTestDevice(t, 28<<20)
			receive := device.settings.ReceiveBufferSettings
			if receive.ReceiveQueueBudget.TotalByteCount() != 1908408 ||
				min(receive.ReceiveQueueMaxByteCount, receive.ReceiveQueueBudget.TotalByteCount()) != 1908408 {
				t.Fatal("process setting changed explicit Android receive admission")
			}
			if got := connect.NewPlatformTransportBudgetForMemoryTarget(28 << 20).Stats().TotalByteCount; got != 7<<20 {
				t.Fatal("process setting changed the explicit Android carrier budget")
			}
		})
	}
	for _, invalid := range []string{"", "64-40", "64", "40-64 ", "32-64"} {
		if _, ok := androidH1MemoryExperimentArm(invalid); ok {
			t.Fatalf("undeclared arm %q accepted", invalid)
		}
	}
}

// Opt-in host proxy, not a device memory/performance gate. The real H1 carrier
// connects to five loopback providers in this same process. The private Go
// overlay enables only mobileRuntime on darwin; every arm uses that same
// overlay, target 28 MiB, GOGC 25 and two cores. 40-64 varies only the runtime
// soft limit; 64-64 also varies process-default constructor sizing. This does
// not model Android TLS, kernel accounting, physical footprint, or HTTP TTFB.
func TestDeviceLocalH1AndroidMemoryExperiment(t *testing.T) {
	name := os.Getenv("URNETWORK_H1_ANDROID_MEMORY_ARM")
	if name == "" {
		t.Skip("opt-in fresh process: URNETWORK_H1_ANDROID_MEMORY_ARM=40-40,40-64,64-64")
	}
	arm, ok := androidH1MemoryExperimentArm(name)
	if !ok || !mobileRuntime() || runtime.GOMAXPROCS(0) != 2 {
		t.Fatal("experiment requires a declared arm, mobile-policy overlay, and GOMAXPROCS=2")
	}
	if runIsolatedLoadTest(t) {
		return
	}
	SetMemoryLimit(arm.SizingMiB << 20)
	debug.SetGCPercent(25)
	debug.SetMemoryLimit(arm.SoftMiB << 20)
	f := newH1OwnerFixtureWithProviders(t, 5, 90*time.Second)
	device, generator, multi := f.connectDeviceWithTarget(t, 28<<20)
	h1OwnerTraffic(t, device)
	deadline := time.Now().Add(10 * time.Second)
	for multi.MemoryOwnerCensus().Transfer.Clients != 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if multi.MemoryOwnerCensus().Transfer.Clients != 5 || device.transferMemory == nil {
		t.Fatal("mobile five-exit fixture did not form")
	}
	result := struct {
		Arm, Scope                    string
		SizingMiB, SoftMiB, TargetMiB int64
		GOGC, Cores                   int
		Points                        []h1SoftLimitPoint
		Quality, Speed, Resume        h1SoftLimitTraffic
		ClientDrops, ProviderDrops    uint64
		Failure                       string `json:",omitempty"`
	}{Arm: name, Scope: "host-H1-loopback-UDP-echo-not-device-gate", SizingMiB: arm.SizingMiB,
		SoftMiB: arm.SoftMiB, TargetMiB: 28, GOGC: 25, Cores: 2, Points: make([]h1SoftLimitPoint, 0, 1024)}
	started := time.Now()
	point := func(phase string) h1SoftLimitPoint { return readH1SoftLimitPoint(phase, time.Since(started), multi) }
	result.Points = append(result.Points, point("connected"))
	measure := func(phase string, sourcePort, destinationPort int) (h1SoftLimitTraffic, error) {
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					result.Points = append(result.Points, point(phase+"-sample"))
				}
			}
		}()
		traffic, err := runH1ExperimentTraffic(device, 64, 256, sourcePort, destinationPort)
		close(stop)
		<-done
		result.Points = append(result.Points, point("post-"+phase))
		return traffic, err
	}
	var err error
	result.Quality, err = measure("quality", 42000, 443)
	if err == nil {
		result.Speed, err = measure("speed", 43000, 123)
	}
	if err == nil {
		// Natural quiet is measured before any test-only forced collection.
		for i := range 12 {
			time.Sleep(time.Second)
			result.Points = append(result.Points, point(fmt.Sprintf("quiet-%02d", i+1)))
		}
		runtime.GC()
		result.Points = append(result.Points, point("forced-gc"))
		debug.FreeOSMemory()
		result.Points = append(result.Points, point("forced-scavenge"))
		result.Resume, err = runH1ExperimentTraffic(device, 1, 64, 44000, 443)
		result.Points = append(result.Points, point("resumed"))
	}
	for _, provider := range f.providers {
		result.ProviderDrops += provider.ReceiveStats().PackHandoffDropCount
	}
	generator.mu.Lock()
	for _, client := range generator.clients {
		result.ClientDrops += client.ReceiveStats().PackHandoffDropCount
	}
	generator.mu.Unlock()
	if err == nil && (result.ProviderDrops != 0 || result.ClientDrops != 0) {
		err = fmt.Errorf("reliable fixture dropped a receive handoff")
	}
	if err != nil {
		result.Failure = err.Error()
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ANDROID_H1_MEMORY_RESULT %s", encoded)
	if debug.SetMemoryLimit(-1) != arm.SoftMiB<<20 || connect.MemoryBudget() != arm.SizingMiB<<20 {
		t.Fatal("process policy drifted during the arm")
	}
	if result.Failure != "" {
		t.Fatal(result.Failure)
	}
}
