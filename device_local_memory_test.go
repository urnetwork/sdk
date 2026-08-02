package sdk

import (
	"context"
	"runtime/debug"
	"testing"

	"github.com/urnetwork/connect"
)

// pinIosGcPacing pins the gc pacing to the ios setting for the duration of
// a memory measurement test: the sdk init sets gogc per-GOOS, and the
// (darwin) test host would otherwise measure desktop pacing, not the
// constrained-mobile configuration the regression ceilings guard. Keep the
// value in sync with the ios case of the sdk init.
func pinIosGcPacing(t *testing.T) {
	prev := debug.SetGCPercent(10)
	t.Cleanup(func() {
		debug.SetGCPercent(prev)
	})
}

// TestDeviceLocalSettingsMemoryTarget verifies the per-device memory target
// sizes the memory-dominant device defaults (see
// `DeviceLocalSettings.MemoryTargetByteCount`).
func TestDeviceLocalSettingsMemoryTarget(t *testing.T) {
	// the process budget scales the per-sequence caps; the target-derived
	// values are independent of it
	connect.SetMemoryBudget(24 * 1024 * 1024)
	defer connect.SetMemoryBudget(0)

	settings := DefaultDeviceLocalSettings()
	connect.AssertEqual(t, settings.MemoryTargetByteCount, connect.ByteCount(defaultDeviceLocalMemoryTargetByteCount))
	// one slot per 16 KiB of the client share (14/20 of the target),
	// capped at the unscaled default
	connect.AssertEqual(t, settings.SequenceBufferSize, 256)
	connect.AssertEqual(t, settings.ClientSettings.SendBufferSize, 256)
	connect.AssertEqual(t, settings.ClientSettings.SendBufferSettings.SequenceBufferSize, 256)
	// the per-sequence caps still scale from the process budget: 24/64 of 2 MiB
	connect.AssertEqual(t, settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount, connect.ByteCount(768*1024))

	// the shared transfer queue budget pair is 3:4 of the client share — at
	// the 20 MB reference the provide-on pair is the historically proven
	// 6 MiB send / 8 MiB receive
	sendBudget := settings.ClientSettings.SendBufferSettings.ResendQueueBudget
	receiveBudget := settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
	if sendBudget == nil || receiveBudget == nil {
		t.Fatalf("expected device transfer queue budgets to be set")
	}
	clientShare := connect.ByteCount(defaultDeviceLocalMemoryTargetByteCount) * deviceMemoryRatioClient / deviceMemoryRatioParts
	connect.AssertEqual(t, sendBudget.TotalByteCount(), clientShare*3/7)
	connect.AssertEqual(t, sendBudget.TotalByteCount(), connect.ByteCount(6*1024*1024))
	connect.AssertEqual(t, receiveBudget.TotalByteCount(), clientShare*4/7)
	connect.AssertEqual(t, receiveBudget.TotalByteCount(), connect.ByteCount(8*1024*1024))
	// p2p admission draws from the shared receive budget
	connect.AssertEqual(t, settings.ClientSettings.WebRtcSettings.MemoryBudget, receiveBudget)
	// floor below the borrow cap
	if settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount < settings.ClientSettings.SendBufferSettings.ResendQueueMinByteCount {
		t.Errorf("send floor above the borrow cap")
	}

	// two devices get independent pools
	other := DefaultDeviceLocalSettings()
	if other.ClientSettings.SendBufferSettings.ResendQueueBudget == sendBudget {
		t.Errorf("expected per-device budgets, got a shared pool across devices")
	}

	// a zero target falls back to the process-budget scaled sizing:
	// 24/64 of the unscaled 6 MiB send / 8 MiB receive pools and channel depth
	zeroSendBudget, zeroReceiveBudget := deviceLocalTransferBudgets(0)
	connect.AssertEqual(t, zeroSendBudget.TotalByteCount(), connect.ByteCount(2304*1024))
	connect.AssertEqual(t, zeroReceiveBudget.TotalByteCount(), connect.ByteCount(3*1024*1024))
	connect.AssertEqual(t, deviceLocalSequenceBufferSize(0), 96)
}

// TestProviderLocalUserNatSettings verifies the provide exit nat bounds the
// per source and aggregate flow counts (the local-traffic nats stay
// unlimited), sized from the provider share of the device memory target.
func TestProviderLocalUserNatSettings(t *testing.T) {
	// the provider share (4/20) of the default 20 MB device target: half the
	// share sizes the nat, 60% udp / 40% tcp by bytes over the per-flow cost
	// model
	providerTarget := connect.ByteCount(4 * 1024 * 1024)
	settings := providerLocalUserNatSettings(providerTarget, connect.NewNoopLogger())
	connect.AssertEqual(t, settings.UdpBufferSettings.GlobalLimit, 614)
	connect.AssertEqual(t, settings.UdpBufferSettings.UserLimit, 153)
	connect.AssertEqual(t, settings.TcpBufferSettings.GlobalLimit, 102)
	connect.AssertEqual(t, settings.TcpBufferSettings.UserLimit, 51)

	// a zero target with a process budget keeps the legacy scaled caps
	// (24/64 of the unscaled limits)
	connect.SetMemoryBudget(24 * 1024 * 1024)
	defer connect.SetMemoryBudget(0)
	settings = providerLocalUserNatSettings(0, connect.NewNoopLogger())
	connect.AssertEqual(t, settings.UdpBufferSettings.UserLimit, 192)
	connect.AssertEqual(t, settings.UdpBufferSettings.GlobalLimit, 768)
	connect.AssertEqual(t, settings.TcpBufferSettings.UserLimit, 96)
	connect.AssertEqual(t, settings.TcpBufferSettings.GlobalLimit, 192)
	// the scaled per flow depths flow through from the connect defaults
	connect.AssertEqual(t, settings.UdpBufferSettings.SequenceBufferSize, 96)
	connect.AssertEqual(t, settings.TcpBufferSettings.SequenceBufferSize, 384)

	// an unbudgeted zero target keeps unlimited flow counts (server/desktop)
	connect.SetMemoryBudget(0)
	settings = providerLocalUserNatSettings(0, connect.NewNoopLogger())
	connect.AssertEqual(t, settings.UdpBufferSettings.UserLimit, 0)
	connect.AssertEqual(t, settings.UdpBufferSettings.GlobalLimit, 0)
	connect.AssertEqual(t, settings.TcpBufferSettings.UserLimit, 0)
	connect.AssertEqual(t, settings.TcpBufferSettings.GlobalLimit, 0)
}

// TestDeviceLocalProvideMemoryRealloc verifies the provider share of the
// device memory target follows the provide state: while providing is off it
// backs the client pair; enabling provide moves it to the provider pair and
// the egress nat, live.
func TestDeviceLocalProvideMemoryRealloc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	target := connect.ByteCount(defaultDeviceLocalMemoryTargetByteCount)
	clientShare := target * deviceMemoryRatioClient / deviceMemoryRatioParts
	providerShare := target * deviceMemoryRatioProvider / deviceMemoryRatioParts
	clientResend := device.settings.ClientSettings.SendBufferSettings.ResendQueueBudget
	clientReceive := device.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
	providerResend, providerReceive := device.provider.transferBudgets()
	if providerResend == nil || providerReceive == nil {
		t.Fatal("expected the provider client to own its own budget pair")
	}

	// initial state: providing off, the provider share backs the client pair
	connect.AssertEqual(t, clientResend.TotalByteCount(), (clientShare+providerShare)*3/7)
	connect.AssertEqual(t, clientReceive.TotalByteCount(), (clientShare+providerShare)*4/7)
	connect.AssertEqual(t, providerResend.TotalByteCount(), connect.ByteCount(256*1024))
	connect.AssertEqual(t, providerReceive.TotalByteCount(), connect.ByteCount(384*1024))

	// provide on: the share moves to the provider pair (half; the other half
	// sizes the freshly built egress nat)
	device.SetProvideMode(ProvideModePublic)
	connect.AssertEqual(t, clientResend.TotalByteCount(), clientShare*3/7)
	connect.AssertEqual(t, clientReceive.TotalByteCount(), clientShare*4/7)
	connect.AssertEqual(t, providerResend.TotalByteCount(), providerShare/2*3/7)
	connect.AssertEqual(t, providerReceive.TotalByteCount(), providerShare/2*4/7)

	// provide off again: the share returns to the client pair
	device.SetProvideMode(ProvideModeNone)
	connect.AssertEqual(t, clientResend.TotalByteCount(), (clientShare+providerShare)*3/7)
	connect.AssertEqual(t, clientReceive.TotalByteCount(), (clientShare+providerShare)*4/7)
	connect.AssertEqual(t, providerResend.TotalByteCount(), connect.ByteCount(256*1024))
	connect.AssertEqual(t, providerReceive.TotalByteCount(), connect.ByteCount(384*1024))
}

// TestDeviceLocalMemoryCeiling drives the loopback echo load with the sdk
// configured for the ios packet tunnel budget (`SetMemoryLimit(32 MiB)`, the
// value the extension passes) and checks that the device keeps moving
// traffic under the soft limit and that the memory telemetry reads sanely. This
// guards the budget plumbing end to end: a regression that unhooks the
// budget from the settings or balloons the steady-state footprint shows up
// here.
func TestDeviceLocalMemoryCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal memory ceiling test in -short mode")
	}

	const budgetByteCount = 32 * 1024 * 1024

	pinIosGcPacing(t)
	prevLimit := debug.SetMemoryLimit(-1)
	SetMemoryLimit(budgetByteCount)
	t.Cleanup(func() {
		connect.SetMemoryBudget(0)
		SetMessagePoolMemoryTargets(connect.InitialMessagePoolByteCount/2, connect.InitialMessagePoolByteCount/2)
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

	// the device carries the default per-device memory target: the channel
	// depth derives from the client share (one slot per 16 KiB), capped at
	// the unscaled default
	device, tun, teardown := newLoopbackDeviceEnv(t, ctx, networkSpace, byJwt)
	defer teardown()
	connect.AssertEqual(t, device.settings.MemoryTargetByteCount, connect.ByteCount(defaultDeviceLocalMemoryTargetByteCount))
	connect.AssertEqual(t, device.settings.SequenceBufferSize, 256)

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
	connect.AssertEqual(t, stats.MemoryLimitByteCount, ByteCount(budgetByteCount))
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
