//go:build !ios

package sdk

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Seeds three real backend failures, the state earlier tests' unreachable
// platforms leave behind, witnesses that they hold this fixture's empty window
// at one exit, and then runs the original assertions in a process of their
// own, which does not inherit them. Before
// TestDeviceH3WindowPolicyTracksLiveModeAndColdReconnect ran isolated it failed
// this way in shuffled whole-package runs beside a busy package (2 of about 36,
// sdk-test-isolation): "resize watchdog: target=2 want=6" after the H1 mode
// change, with platform dials of earlier tests failing in the two minutes before
// it and none answering. The window loop lost no resize: an empty window holds
// at one exit while the backend reads as degraded (degradedWindowTarget), and
// the held discovery completes no round trip that would clear it.
func TestDeviceH3WindowPolicyIgnoresInheritedBackendFailures(t *testing.T) {
	if os.Getenv(isolatedLoadTestEnv) == t.Name() {
		TestDeviceH3WindowPolicyTracksLiveModeAndColdReconnect(t)
		return
	}

	runControls := func(control connect.OutOfBandControl, count int, wantFailures int64) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		observed := &reraceObservedOob{control: control}
		settings := connect.DefaultClientSettings()
		settings.Log = connect.NewNoopLogger()
		client := connect.NewClient(ctx, connect.NewId(), observed, settings)
		defer func() {
			cancel()
			join, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := client.CloseAndWait(join); err != nil {
				t.Errorf("backend witness client retirement: %v", err)
			}
			if got := observed.completed.Load(); got != int64(count) {
				t.Errorf("backend callbacks after joined retirement = %d, want %d", got, count)
			}
		}()
		for i := 0; i < count; i++ {
			key := connect.ContractKey{Destination: connect.DestinationId(connect.NewId())}
			client.ContractManager().CreateContract(key, uint64(i), connect.ByteCount(1024))
			if got := observed.completed.Load(); got != int64(i+1) {
				t.Fatalf("CreateContract did not complete synchronously: %d, want %d", got, i+1)
			}
		}
		if got := observed.failures.Load(); got != wantFailures {
			t.Fatalf("actual OOB failures = %d, want %d", got, wantFailures)
		}
	}
	// Run last, including after an expected Fatal: neither arm may leave the
	// intentionally seeded process state for an unrelated next test.
	defer runControls(reraceSuccessfulOob{}, 1, 0)
	runControls(connect.NewNoContractClientOob(), 3, 3)

	// The uncapped policy asks five quality exits and one speed exit of the
	// real destination. With the backend read as degraded the quality window
	// publishes one exit, so the first sum both windows publish is 2, which a
	// healthy process never publishes: 1 or 5 for one window, 6 for both.
	func() {
		target := ByteCount(32 * 1024 * 1024)
		device := newH3PolicyDevice(t, target)
		defer func() {
			join, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := device.CloseAndWait(join); err != nil {
				t.Errorf("witness device retirement: %v", err)
			}
		}()
		if err := device.SetConnectLocationChecked(&ConnectLocation{
			ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		}); err != nil {
			t.Fatal(err)
		}
		device.stateLock.Lock()
		multi, ok := device.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
		device.stateLock.Unlock()
		if !ok {
			t.Fatal("real SDK destination did not construct a multi-client")
		}
		changed := connect.NewMonitor()
		unsubscribe := multi.Monitor().AddMonitorEventCallback(func(*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
			changed.NotifyAll()
		})
		defer unsubscribe()
		watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
		defer stop()
		baseline := mobileTargetScaledCount(mobileQualityWindowSize, target) +
			mobileTargetScaledCount(mobileSpeedWindowSize, target)
		for {
			update := changed.NotifyChannel()
			status := device.GetWindowStatus()
			if 2 <= status.TargetSize {
				if status.TargetSize != 2 {
					t.Fatalf("seeded backend did not hold the empty window at one exit: target=%d, uncapped %d", status.TargetSize, baseline)
				}
				break
			}
			select {
			case <-update:
			case <-watchdog.Done():
				t.Fatalf("witness watchdog: target=%d", status.TargetSize)
			}
		}
		t.Log("H3_INHERITED_BACKEND_TARGET_TWO_CONFIRMED")
	}()
	if t.Failed() {
		return
	}
	TestDeviceH3WindowPolicyTracksLiveModeAndColdReconnect(t)
}
