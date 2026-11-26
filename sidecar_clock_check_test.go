// The forwarding role's clock for the apps (sidecar_clock_check.go; sidecar §17.8's after sleep, owner decision 789):
// the waits are uncapped on iOS and Android and capped elsewhere, a low-memory desktop included; DeviceLocal.CheckClock
// reaches the bridge the device holds, which publishes at now, and does nothing in legacy; and a router over several
// roles asks each role's bridge. Devices run sidecar's posture vectors (device_sidecar_test.go).

package sdk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sidecardevice "github.com/urnetwork/sidecar/device"
)

// A bridge that counts its clock checks, over the real one, or alone where it has none.
type testingClockCheckedBridge struct {
	*sidecardevice.Bridge
	checks atomic.Int64
}

func (self *testingClockCheckedBridge) CheckClock() {
	self.checks.Add(1)
	if self.Bridge != nil {
		self.Bridge.CheckClock()
	}
}

// The cap on the bridge's waits per platform: none on iOS and Android, whose VPN component checks the clock at each
// wake, low memory or not; a desktop's on every other device, a low-memory one's included.
func TestBridgeClockCheckIntervalPerPlatform(t *testing.T) {
	desktop := sidecardevice.DefaultBridgeSettings().ClockCheckInterval
	if desktop <= 0 {
		t.Fatalf("CONTROL FAILED: a desktop's cap is %v", desktop)
	}
	cases := []struct {
		goos      string
		lowMemory bool
		interval  time.Duration
	}{
		{goos: "ios", interval: 0},
		{goos: "ios", lowMemory: true, interval: 0},
		{goos: "android", interval: 0},
		{goos: "android", lowMemory: true, interval: 0},
		{goos: "darwin", interval: desktop},
		{goos: "windows", interval: desktop},
		{goos: "linux", interval: desktop},
		{goos: "linux", lowMemory: true, interval: desktop},
		{goos: "windows", lowMemory: true, interval: desktop},
	}
	for _, testCase := range cases {
		settings := DefaultDeviceLocalSettings()
		settings.LowMemoryProfile = testCase.lowMemory
		if interval := deviceLocalBridgeSettingsFor(settings, testCase.goos).ClockCheckInterval; interval != testCase.interval {
			t.Errorf("%s low memory %t: a cap of %v, want %v", testCase.goos, testCase.lowMemory, interval, testCase.interval)
		}
	}
}

// In legacy the device holds no bridge and a check does nothing; past it, a check reaches the bridge the device holds,
// once, and the bridge publishes at now.
func TestDeviceLocalCheckClockReachesTheHeldBridge(t *testing.T) {
	role := newTestingDeviceRole(t)
	builtLock := sync.Mutex{}
	built := []*testingClockCheckedBridge{}
	device := role.device(t, func(settings *DeviceLocalSettings) {
		settings.testingNewDeviceBridge = func(ctx context.Context, store sidecardevice.PostureStore, bridgeSettings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
			bridge, err := sidecardevice.NewBridge(ctx, store, nil, bridgeSettings)
			if err != nil {
				return nil, err
			}
			checked := &testingClockCheckedBridge{Bridge: bridge}
			builtLock.Lock()
			built = append(built, checked)
			builtLock.Unlock()
			return checked, nil
		}
	})
	checkCount := func() int64 {
		builtLock.Lock()
		defer builtLock.Unlock()
		var count int64
		for _, bridge := range built {
			count += bridge.checks.Load()
		}
		return count
	}
	device.CheckClock()
	if count := checkCount(); count != 0 {
		t.Fatalf("a legacy device checked %d bridges", count)
	}

	role.enforce(t, device, testingBridgeRules())
	held := func() *testingClockCheckedBridge {
		builtLock.Lock()
		defer builtLock.Unlock()
		return built[len(built)-1]
	}()
	published := make(chan struct{}, 1)
	t.Cleanup(held.AddChangeCallback(func() {
		select {
		case published <- struct{}{}:
		default:
		}
	}))
	device.CheckClock()
	if count := checkCount(); count != 1 || held.checks.Load() != 1 {
		t.Fatalf("a check reached %d bridges, the held one %d times", count, held.checks.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	select {
	case <-published:
	case <-ctx.Done():
		t.Fatal("the held bridge did not publish at the check")
	}
}

// A router over several roles asks each role's bridge, the primary's among them, once.
func TestDeviceBridgeRouterCheckClockAsksEveryRole(t *testing.T) {
	first, primary, last := &testingClockCheckedBridge{}, &testingClockCheckedBridge{}, &testingClockCheckedBridge{}
	router := newDeviceBridgeRouter([]deviceBridgeRouterRole{
		{bridge: first},
		{bridge: primary, primary: true},
		{bridge: last},
	})
	router.CheckClock()
	for i, bridge := range []*testingClockCheckedBridge{first, primary, last} {
		if bridge.checks.Load() != 1 {
			t.Errorf("role %d's bridge was checked %d times", i, bridge.checks.Load())
		}
	}
}
