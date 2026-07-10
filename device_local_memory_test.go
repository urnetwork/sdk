package sdk

import (
	"context"
	"runtime/debug"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/connect"
)

// TestDeviceLocalSettingsMemoryScaled verifies the memory budget scales the
// memory-dominant device defaults (see `SetMemoryLimit`).
func TestDeviceLocalSettingsMemoryScaled(t *testing.T) {
	// the ios packet tunnel budget
	connect.SetMemoryBudget(24 * 1024 * 1024)
	defer connect.SetMemoryBudget(0)

	settings := DefaultDeviceLocalSettings()
	// 24/64 of the unscaled 256
	assert.Equal(t, settings.SequenceBufferSize, 96)
	assert.Equal(t, settings.ClientSettings.SendBufferSize, 96)
	assert.Equal(t, settings.ClientSettings.SendBufferSettings.SequenceBufferSize, 96)
	// 24/64 of the unscaled 2 MiB resend queue cap
	assert.Equal(t, settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount, connect.ByteCount(768*1024))

	// the device creates a shared transfer queue budget pair, scaled:
	// 24/64 of the unscaled 6 MiB send / 8 MiB receive pools
	sendBudget := settings.ClientSettings.SendBufferSettings.ResendQueueBudget
	receiveBudget := settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
	if sendBudget == nil || receiveBudget == nil {
		t.Fatalf("expected device transfer queue budgets to be set")
	}
	assert.Equal(t, sendBudget.TotalByteCount(), connect.ByteCount(2304*1024))
	assert.Equal(t, receiveBudget.TotalByteCount(), connect.ByteCount(3*1024*1024))
	// floor below the borrow cap
	if settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount < settings.ClientSettings.SendBufferSettings.ResendQueueMinByteCount {
		t.Errorf("send floor above the borrow cap")
	}

	// two devices get independent pools
	other := DefaultDeviceLocalSettings()
	if other.ClientSettings.SendBufferSettings.ResendQueueBudget == sendBudget {
		t.Errorf("expected per-device budgets, got a shared pool across devices")
	}

	// no budget leaves the defaults unscaled
	connect.SetMemoryBudget(0)
	settings = DefaultDeviceLocalSettings()
	assert.Equal(t, settings.SequenceBufferSize, 256)
	assert.Equal(t, settings.ClientSettings.SendBufferSettings.ResendQueueBudget.TotalByteCount(), connect.ByteCount(6*1024*1024))
}

// TestDeviceLocalMemoryCeiling drives the loopback echo load with the sdk
// configured for the ios packet tunnel budget (`SetMemoryLimit(24 MiB)`) and
// checks that the device keeps moving traffic inside the budget and that the
// memory telemetry reads sanely. This guards the budget plumbing end to end:
// a regression that unhooks the budget from the settings or balloons the
// steady-state footprint shows up here.
func TestDeviceLocalMemoryCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal memory ceiling test in -short mode")
	}

	const budgetByteCount = 24 * 1024 * 1024

	prevLimit := debug.SetMemoryLimit(-1)
	SetMemoryLimit(budgetByteCount)
	t.Cleanup(func() {
		connect.SetMemoryBudget(0)
		connect.ResizeMessagePools(connect.InitialMessagePoolByteCount)
		debug.SetMemoryLimit(prevLimit)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()

	// the device is constructed after SetMemoryLimit, so it picks up the
	// scaled defaults
	device, tun, teardown := newLoopbackDeviceEnv(t, ctx, networkSpace, byJwt)
	defer teardown()
	assert.Equal(t, device.settings.SequenceBufferSize, 96)

	// move real traffic under the budget
	const (
		rounds       = 8
		flows        = 2
		bytesPerFlow = 128 << 10
	)
	if err := runLoadIteration(ctx, tun, echoAddr, rounds, flows, bytesPerFlow); err != nil {
		skipOnRaceGvisorWedge(t, "load", err)
		t.Fatalf("load: %v", err)
	}

	stats := GetMemoryStats()
	t.Logf("memory stats: live=%s goal=%s total=%s limit=%s goroutines=%d pool taken=%d returned=%d created=%d",
		humanBytes(uint64(stats.HeapLiveByteCount)),
		humanBytes(uint64(stats.HeapGoalByteCount)),
		humanBytes(uint64(stats.TotalRuntimeByteCount)),
		humanBytes(uint64(stats.MemoryLimitByteCount)),
		stats.GoroutineCount,
		stats.PoolTakenCount, stats.PoolReturnedCount, stats.PoolCreatedCount)

	// the telemetry reads sanely
	assert.Equal(t, stats.MemoryLimitByteCount, ByteCount(budgetByteCount))
	if stats.HeapLiveByteCount <= 0 {
		t.Errorf("heap live gauge not populated")
	}
	if stats.GoroutineCount <= 0 {
		t.Errorf("goroutine gauge not populated")
	}
	if stats.PoolTakenCount < stats.PoolReturnedCount {
		t.Errorf("pool returned (%d) exceeds taken (%d)", stats.PoolReturnedCount, stats.PoolTakenCount)
	}

	// the quiesced live set stays well inside the budget. the load moves
	// (rounds x flows x 128 KiB) through the full device stack, so a leak or
	// an unscaled queue ballooning past the budget fails here.
	_, quiescedHeap := sampleStable()
	t.Logf("quiesced heap: %s (budget %s)", humanBytes(quiescedHeap), humanBytes(uint64(budgetByteCount)))
	if uint64(budgetByteCount/2) < quiescedHeap {
		t.Errorf("quiesced heap %s exceeds half the %s budget", humanBytes(quiescedHeap), humanBytes(uint64(budgetByteCount)))
	}
}
