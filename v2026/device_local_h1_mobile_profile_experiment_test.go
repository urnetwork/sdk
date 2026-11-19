//go:build !ios

package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type mobileH1ProfileArm struct {
	TargetMiB, ProcessMiB int64
}

func mobileH1ProfileExperimentArm(name string) (mobileH1ProfileArm, bool) {
	switch name {
	case "ios-v1":
		return mobileH1ProfileArm{20, 32}, true
	case "ios-v2":
		return mobileH1ProfileArm{32, 32}, true
	case "android-prior":
		return mobileH1ProfileArm{28, 64}, true
	case "android-full":
		return mobileH1ProfileArm{64, 64}, true
	default:
		return mobileH1ProfileArm{}, false
	}
}

// This screen isolates the admission-profile change at fixed GOGC and returned
// pools. All twelve loopback providers share the host process, so runtime is
// not a device footprint. Every profile uses the same provider cohort, and
// both configured windows must be healthy and stable before measured traffic.
// No physical qualification or HTTP TTFB is inferred.
func TestDeviceLocalH1MobileProfileExperiment(t *testing.T) {
	name := os.Getenv("URNETWORK_H1_MOBILE_PROFILE_ARM")
	if name == "" {
		t.Skip("opt-in fresh-process host screen: ios-v1,ios-v2,android-prior,android-full")
	}
	arm, ok := mobileH1ProfileExperimentArm(name)
	if !ok || !mobileRuntime() || runtime.GOMAXPROCS(0) != 2 {
		t.Fatal("declared profile, mobile-policy overlay and GOMAXPROCS=2 required")
	}
	if runIsolatedLoadTest(t) {
		return
	}
	SetMemoryLimit(arm.ProcessMiB << 20)
	debug.SetGCPercent(25)
	quietSeconds := 20
	if value := os.Getenv("URNETWORK_H1_MOBILE_PROFILE_QUIET_SECONDS"); value != "" {
		var err error
		quietSeconds, err = strconv.Atoi(value)
		if err != nil || quietSeconds != 60 {
			t.Fatal("only the predeclared 60-second quiet diagnostic override is allowed")
		}
	}
	threshold := currentMobileRuntimeReclaimTarget()
	if value := os.Getenv("URNETWORK_H1_MOBILE_PROFILE_THRESHOLD_MIB"); value != "" {
		expected, err := strconv.ParseInt(value, 10, 64)
		if err != nil || name != "android-full" || (expected != 24 && expected != 48) || threshold != expected<<20 {
			t.Fatal("the declared Android quiet-reclaim threshold does not match this binary")
		}
	}
	f := newH1OwnerFixtureWithProviders(t, 12, 170*time.Second)
	device, generator, multi := f.connectDeviceWithTarget(t, arm.TargetMiB<<20)
	h1OwnerTraffic(t, device)
	if device.transferMemory == nil {
		t.Fatal("mobile fixture has no shared transfer owner")
	}
	required := mobileH1ProfileRequiredWindows(arm.TargetMiB << 20)
	type observation struct {
		Runtime                        h1SoftLimitPoint
		Device                         *DeviceLocalMemoryUsage
		Pools                          connect.MessagePoolAggregateStats
		Readiness                      mobileH1ProfileReadiness
		TrimCount, TrimDeferred        int64
		TrimBelowTarget, TrimCooldowns int64
	}
	result := struct {
		Arm, Scope                 string
		Policy                     mobileH1ProfileArm
		QuietSeconds               int
		RuntimeReclaimTargetBytes  int64
		Cores, GOGC, Providers     int
		RequiredWindows            mobileH1ProfileWindows
		MeasuredReadiness          []mobileH1ProfileReadiness
		Points                     []observation
		Quality, Speed, Resume     h1SoftLimitTraffic
		NaturalResume              h1SoftLimitTraffic
		ClientDrops, ProviderDrops uint64
		Failure                    string `json:",omitempty"`
	}{Arm: name, Scope: "host-H1-ready-windows-twelve-provider-UDP-echo-screen-v2-not-device-gate", Policy: arm,
		Cores: 2, GOGC: 25, Providers: 12, RequiredWindows: required,
		QuietSeconds: quietSeconds, RuntimeReclaimTargetBytes: threshold, Points: make([]observation, 0, 1024)}
	started := time.Now()
	readiness := func() mobileH1ProfileReadiness {
		return mobileH1ProfileReadinessFromExits(multi.Exits(), multi.MemoryOwnerCensus().Transfer.Clients)
	}
	point := func(phase string) observation {
		return observation{
			Runtime: readH1SoftLimitPoint(phase, time.Since(started), multi),
			Device:  device.MemoryUsed(), Pools: connect.GetMessagePoolAggregateStats(), Readiness: readiness(),
			TrimCount: mobileIdleMemoryTrimCount.Load(), TrimDeferred: mobileIdleMemoryTrimDeferred.Load(),
			TrimBelowTarget: mobileIdleMemoryTrimBelow.Load(), TrimCooldowns: mobileIdleMemoryTrimCooldowns.Load(),
		}
	}
	result.Points = append(result.Points, point("forming"))
	measure := func(phase string, sourcePort, destinationPort int) (h1SoftLimitTraffic, error) {
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		var traffic h1SoftLimitTraffic
		_, err := runMobileH1ProfileWhenReady(ctx, required, time.Second, readiness, time.Now,
			func(ctx context.Context) error {
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				}
			}, func(ready mobileH1ProfileReadiness) error {
				result.MeasuredReadiness = append(result.MeasuredReadiness, ready)
				result.Points = append(result.Points, point("ready-"+phase))
				stop, done := make(chan struct{}), make(chan struct{})
				var lostReadiness error
				observe := func(label string) {
					p := point(label)
					result.Points = append(result.Points, p)
					if !mobileH1ProfileReady(required, p.Readiness) && lostReadiness == nil {
						lostReadiness = fmt.Errorf("%s lost configured profile windows: %+v", label, p.Readiness)
					}
				}
				go func() {
					defer close(done)
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							observe(phase + "-sample")
						}
					}
				}()
				var err error
				traffic, err = runH1ExperimentTraffic(device, 64, 1024, sourcePort, destinationPort)
				close(stop)
				<-done
				observe("post-" + phase)
				if err != nil {
					return err
				}
				return lostReadiness
			})
		return traffic, err
	}
	var err error
	result.Quality, err = measure("quality", 42000, 443)
	if err == nil {
		result.Speed, err = measure("speed", 43000, 123)
	}
	if err == nil {
		for i := range quietSeconds {
			time.Sleep(time.Second)
			result.Points = append(result.Points, point(fmt.Sprintf("quiet-%02d", i+1)))
		}
		if !mobileH1ProfileReady(required, readiness()) {
			err = fmt.Errorf("natural resume lost configured profile windows")
		} else {
			result.NaturalResume, err = runH1ExperimentTraffic(device, 1, 64, 44500, 443)
			result.Points = append(result.Points, point("natural-resume"))
		}
	}
	if err == nil {
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
	if err == nil && (result.ClientDrops != 0 || result.ProviderDrops != 0) {
		err = fmt.Errorf("reliable H1 handoff dropped")
	}
	if err != nil {
		result.Failure = err.Error()
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	t.Logf("MOBILE_H1_PROFILE_RESULT %s", encoded)
	if debug.SetMemoryLimit(-1) != arm.ProcessMiB<<20 || connect.MemoryBudget() != arm.ProcessMiB<<20 {
		t.Fatal("process policy drifted")
	}
	if result.Failure != "" {
		t.Fatal(result.Failure)
	}
}
