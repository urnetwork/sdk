package sdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func contractStatsFlushTestDevice(t *testing.T, epoch time.Duration) *DeviceLocal {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	settings.ContractStatsEpoch = epoch
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	t.Cleanup(device.Close)
	return device
}

func contractStatsFlushTestEvent(contractId connect.Id, path connect.TransferPath, usedByteCount connect.ByteCount, usedByteCountDelta connect.ByteCount, sequence uint64) *connect.ContractStatsEvent {
	return &connect.ContractStatsEvent{
		ContractId:         contractId,
		Path:               path,
		TransferByteCount:  1 << 30,
		UsedByteCount:      usedByteCount,
		UsedByteCountDelta: usedByteCountDelta,
		Open:               true,
		Sequence:           sequence,
	}
}

func waitForContractStats(t *testing.T, listener *testing_contractStatsListener, timeout time.Duration, message string, done func(*ContractStats) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		listener.stateLock.Lock()
		contractStats := listener.contractStats
		listener.stateLock.Unlock()
		if contractStats != nil && done(contractStats) {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

// The flush loop arms one timer for its life, not one per epoch.
func TestContractStatsFlushReusesOneTimer(t *testing.T) {
	device := contractStatsFlushTestDevice(t, 2*time.Millisecond)
	path := connect.TransferPath{SourceId: connect.NewId(), DestinationId: connect.NewId()}
	contractId := connect.NewId()

	timers := timerAllocationsUnder(t, ").runContractStatsFlush", func() {
		// a moving transfer keeps every epoch busy
		for i := 1; i <= 100; i += 1 {
			device.updateContractStatsEvents([]*connect.ContractStatsEvent{
				contractStatsFlushTestEvent(contractId, path, connect.ByteCount(i*100), 100, uint64(i)),
			})
			time.Sleep(time.Millisecond)
		}
	})
	if 4 < timers {
		t.Fatalf("the flush loop allocated %d timer objects over ~50 epochs, want one timer for its life", timers)
	}
}

// Once the last batch of a transfer is carried out and its bit rate decays,
// the loop parks instead of waking every epoch. A later update wakes it, and
// the trailing edge and the decay still happen.
func TestContractStatsFlushParksWhenSettled(t *testing.T) {
	device := contractStatsFlushTestDevice(t, 2*time.Millisecond)
	passes := atomic.Int64{}
	countPass := func() {
		passes.Add(1)
	}
	device.contractStatsFlushPassForTest.Store(&countPass)

	listener := &testing_contractStatsListener{}
	sub := device.AddEgressContractStatsChangeListener(listener)
	defer sub.Close()

	path := connect.TransferPath{SourceId: connect.NewId(), DestinationId: connect.NewId()}
	contractId := connect.NewId()
	// the first batch emits; the second lands in the gate and only the flush
	// loop carries it out
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{
		contractStatsFlushTestEvent(contractId, path, 100, 100, 1),
	})
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{
		contractStatsFlushTestEvent(contractId, path, 5000, 4900, 2),
	})
	waitForContractStats(t, listener, 5*time.Second, "the gated batch was not carried out", func(contractStats *ContractStats) bool {
		return contractStats.ContractUsedByteCount == 5000 && contractStats.ContractBitRate == 0
	})
	time.Sleep(20 * time.Millisecond)

	before := passes.Load()
	time.Sleep(100 * time.Millisecond)
	if idlePasses := passes.Load() - before; 2 < idlePasses {
		t.Fatalf("the flush loop made %d passes over 100ms with nothing to flush at a 2ms epoch, want it parked", idlePasses)
	}

	// an update after the park emits at once with a fresh bit rate; the woken
	// loop decays the rate when the transfer stops
	time.Sleep(10 * time.Millisecond)
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{
		contractStatsFlushTestEvent(contractId, path, 9000, 4000, 3),
	})
	waitForContractStats(t, listener, 5*time.Second, "the update after the park was not emitted", func(contractStats *ContractStats) bool {
		return contractStats.ContractUsedByteCount == 9000
	})
	waitForContractStats(t, listener, 5*time.Second, "the bit rate did not decay after the park", func(contractStats *ContractStats) bool {
		return contractStats.ContractUsedByteCount == 9000 && contractStats.ContractBitRate == 0
	})

	// a client teardown clears the tracker while the loop is parked; the
	// cleared stats are still pushed
	time.Sleep(20 * time.Millisecond)
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		device.contracts.clear()
	}()
	waitForContractStats(t, listener, 5*time.Second, "the cleared stats were not pushed after the park", func(contractStats *ContractStats) bool {
		return contractStats.ContractUsedByteCount == 0
	})
}
