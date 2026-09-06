package sdk

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// TestMemoryAutotune is the measurement kernel for the memory settings
// auto-research loop (driven by an external script that sweeps the env
// knobs, records results, and hill-climbs). Env-gated so normal suites skip
// it; each invocation is one fresh-process measurement of one configuration.
//
// It runs the provider egress load (the same wiring as
// TestDeviceLocalProviderMemoryUnderLoad) under the configured gc pacing,
// process budget, pool bounds, and device memory target, and prints one
// machine-readable [autotune] line: echoed throughput, peak Go soft-limit
// usage, quiesced loaded heap, and pool efficiency.
//
// This measures the local surrogate (allocation/gc/pool efficiency and
// footprint at ~zero rtt); it cannot see bandwidth-delay effects of the
// client pair sizing or real device silicon.
func TestMemoryAutotune(t *testing.T) {
	if os.Getenv("URNET_AUTOTUNE") == "" {
		t.Skip("autotune kernel: set URNET_AUTOTUNE=1")
	}

	budgetMb := autotuneEnvInt("URNET_TUNE_BUDGET_MB", 34)
	// the advisory scaling budget (memoryScale consumers: per-sequence caps,
	// nat windows, read buffers) can be decoupled from the soft limit to
	// isolate its throughput effect. defaults to the budget (production
	// behavior).
	scaleBudgetMb := autotuneEnvInt("URNET_TUNE_SCALE_BUDGET_MB", budgetMb)
	gogc := autotuneEnvInt("URNET_TUNE_GOGC", 10)
	packetMb := autotuneEnvInt("URNET_TUNE_PACKET_MB", 12)
	largeMb := autotuneEnvInt("URNET_TUNE_LARGE_MB", 2)
	deviceMb := autotuneEnvInt("URNET_TUNE_DEVICE_MB", 20)
	seqBufferSize := autotuneEnvInt("URNET_TUNE_SEQ_BUFFER", 0) // 0 = derived
	peerCount := autotuneEnvInt("URNET_TUNE_PEERS", 6)
	rounds := autotuneEnvInt("URNET_TUNE_ROUNDS", 4)
	flowsPerPeer := autotuneEnvInt("URNET_TUNE_FLOWS", 2)
	bytesPerFlow := autotuneEnvInt("URNET_TUNE_FLOW_BYTES", 512<<10)

	prevGc := debug.SetGCPercent(gogc)
	prevLimit := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(int64(budgetMb) << 20)
	connect.SetMemoryBudget(int64(scaleBudgetMb) << 20)
	SetMessagePoolMemoryTargets(int64(packetMb)<<20, int64(largeMb)<<20)
	if autotuneEnvInt("URNET_TUNE_PREWARM", 0) == 1 {
		connect.WarmMessagePools()
	}
	t.Cleanup(func() {
		connect.SetMemoryBudget(0)
		SetMessagePoolMemoryTargets(connect.InitialMessagePoolByteCount/2, connect.InitialMessagePoolByteCount/2)
		debug.SetMemoryLimit(prevLimit)
		debug.SetGCPercent(prevGc)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.MemoryTargetByteCount = ByteCount(deviceMb) << 20
	if 0 < seqBufferSize {
		settings.SequenceBufferSize = seqBufferSize
		settings.ClientSettings.SendBufferSize = seqBufferSize
		settings.ClientSettings.SendBufferSettings.SequenceBufferSize = seqBufferSize
		settings.ClientSettings.ReceiveBufferSettings.SequenceBufferSize = seqBufferSize
	}
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

	device.SetProvideMode(ProvideModePublic)
	if !device.GetProvideEnabled() {
		t.Fatal("expected provide enabled")
	}
	providerClient := device.provider.Client()
	// pin the peer scaffolding at the unscaled reference so its transfer
	// settings do not vary with the configuration under test (the device
	// side sampled the tune scale above; settings sample at construction)
	connect.SetMemoryBudget(64 << 20)
	peers := make([]*providerLoadPeer, peerCount)
	for i := range peers {
		peers[i] = newProviderLoadPeer(t, ctx, providerClient)
	}
	connect.SetMemoryBudget(int64(scaleBudgetMb) << 20)
	defer func() {
		for _, peer := range peers {
			peer.close()
		}
	}()

	sampler := startPeakSampler()

	loadStart := time.Now()
	loadErrs := make(chan error, peerCount)
	for _, peer := range peers {
		go func() {
			loadErrs <- runLoadIteration(ctx, peer.tun, echoAddr, rounds, flowsPerPeer, bytesPerFlow)
		}()
	}
	for range peers {
		if err := <-loadErrs; err != nil {
			sampler.stop()
			// a wedged run is not a measurement; the driver retries
			t.Fatalf("[autotune] load error: %v", err)
		}
	}
	elapsed := time.Since(loadStart)

	peakTotal, peakHeap, _ := sampler.stop()
	_, loadedHeap := sampleStable()
	stats := GetMemoryStats()
	unpooledCount, unpooledByteCount := connect.MessagePoolUnpooledCounts()
	poolClasses := ""
	for i, classStats := range connect.GetMessagePoolClassStats() {
		if 0 < i {
			poolClasses += ","
		}
		poolClasses += fmt.Sprintf("%d:%d/%d/%d/%d",
			classStats.Size, classStats.Created, classStats.Taken,
			classStats.Retained, classStats.Capacity)
	}

	bytesMoved := int64(peerCount) * int64(rounds) * int64(flowsPerPeer) * int64(bytesPerFlow)
	throughputMibs := float64(bytesMoved) / (1 << 20) / elapsed.Seconds()

	fmt.Printf("[autotune] gogc=%d budget_mb=%d scale_mb=%d packet_mb=%d large_mb=%d device_mb=%d seq=%d peers=%d | throughput_mibs=%.2f peak_total_mib=%.2f peak_heap_mib=%.2f loaded_heap_mib=%.2f pool_created=%d pool_taken=%d pool_classes=%s unpooled_count=%d unpooled_mib=%.2f elapsed_ms=%d\n",
		gogc, budgetMb, scaleBudgetMb, packetMb, largeMb, deviceMb, seqBufferSize, peerCount,
		throughputMibs,
		float64(peakTotal)/(1<<20),
		float64(peakHeap)/(1<<20),
		float64(loadedHeap)/(1<<20),
		stats.PoolCreatedCount,
		stats.PoolTakenCount,
		poolClasses,
		unpooledCount,
		float64(unpooledByteCount)/(1<<20),
		elapsed.Milliseconds())
}

func autotuneEnvInt(name string, defaultValue int) int {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}
