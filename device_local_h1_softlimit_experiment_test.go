//go:build !ios

package sdk

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Diagnostic experiment only. Run each arm in a fresh process. The soft limit
// is the sole varied policy: process sizing stays 32 MiB, device target 20 MiB,
// mobile GC pacing/pool bounds and workload stay identical. The explicit test
// overlay enables the mobile platform branch on a darwin host; it is not a
// production edit. Loopback providers share the measured process, so these
// absolute numbers are not an Android/iOS memory gate.
func TestDeviceLocalH1SoftLimitExperiment(t *testing.T) {
	limitMiB, err := strconv.Atoi(os.Getenv("URNETWORK_H1_SOFT_LIMIT_MIB"))
	if err != nil || (limitMiB != 24 && limitMiB != 32) {
		t.Skip("opt-in: URNETWORK_H1_SOFT_LIMIT_MIB=24 or 32, fresh process")
	}
	if !mobileRuntime() {
		t.Fatal("experiment requires the documented mobile-policy test overlay")
	}
	if runIsolatedLoadTest(t) {
		return
	}
	const processSizing = 32 * 1024 * 1024
	SetMemoryLimit(processSizing)
	debug.SetGCPercent(gcPercentForPlatform("ios"))
	debug.SetMemoryLimit(int64(limitMiB) * 1024 * 1024)
	if debug.SetMemoryLimit(-1) != int64(limitMiB)*1024*1024 || connect.MemoryBudget() != processSizing {
		t.Fatal("soft limit changed process sizing or was not applied")
	}
	f := newH1OwnerFixtureWithProviders(t, 5, 90*time.Second)
	device, _, multi := f.connectDeviceWithTarget(t, 20*1024*1024)
	h1OwnerTraffic(t, device)
	deadline := time.Now().Add(10 * time.Second)
	for multi.MemoryOwnerCensus().Transfer.Clients < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := multi.MemoryOwnerCensus().Transfer.Clients; got != 5 || device.transferMemory == nil {
		t.Fatalf("mobile five-exit fixture not formed: clients=%d mobile-budget=%t", got, device.transferMemory != nil)
	}
	result := h1SoftLimitResult{
		SoftLimit: debug.SetMemoryLimit(-1), ProcessSizing: connect.MemoryBudget(),
		DeviceTarget: device.settings.MemoryTargetByteCount, GcPercent: gcPercentForPlatform("ios"),
		Points: make([]h1SoftLimitPoint, 0, 512),
	}
	started := time.Now()
	point := func(phase string) h1SoftLimitPoint { return readH1SoftLimitPoint(phase, time.Since(started), multi) }
	result.Points = append(result.Points, point("connected"))
	stopSample, sampleDone := make(chan struct{}), make(chan struct{})
	var sampleLock sync.Mutex
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-ticker.C:
				sample := point("traffic-sample")
				sampleLock.Lock()
				result.Points = append(result.Points, sample)
				sampleLock.Unlock()
			}
		}
	}()
	result.Traffic, err = runH1SoftLimitTraffic(device, 64, 256)
	close(stopSample)
	<-sampleDone
	result.Points = append(result.Points, point("post-traffic"))
	if err != nil {
		result.Failure = err.Error()
	} else {
		for i := range 12 {
			time.Sleep(time.Second)
			result.Points = append(result.Points, point(fmt.Sprintf("quiet-%02d", i+1)))
		}
		// Natural quiet is reported first. The following explicit test-only
		// collections answer reachability/allocator questions, not acceptance.
		for i := range 3 {
			runtime.GC()
			time.Sleep(100 * time.Millisecond)
			result.Points = append(result.Points, point(fmt.Sprintf("forced-gc-%d", i+1)))
		}
		debug.FreeOSMemory()
		result.Points = append(result.Points, point("forced-scavenge"))
		result.Resume, err = runH1SoftLimitTraffic(device, 1, 64)
		if err != nil {
			result.Failure = err.Error()
		}
		result.Points = append(result.Points, point("resumed"))
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	t.Logf("H1_SOFTLIMIT_RESULT %s", encoded)
	if debug.SetMemoryLimit(-1) != result.SoftLimit || connect.MemoryBudget() != processSizing {
		t.Fatal("runtime policy changed during the controlled arm")
	}
	if result.Failure != "" {
		t.Fatal(result.Failure)
	}
}

type h1SoftLimitPoint struct {
	Phase                                                string `json:"phase"`
	ElapsedMs                                            int64  `json:"elapsed_ms"`
	Runtime, Heap, Inuse, Slack, Free, Stack, TotalAlloc uint64
	Goroutines                                           int
	Gc, ForcedGc                                         uint32
	PauseNs                                              uint64
	GcCpu, GcAssistCpu                                   float64
	HeapGoal, LimiterLastCycle                           uint64
	Clients, Flows, SendWorkers, ReceiveWorkers, Pacing  int64
}

func readH1SoftLimitPoint(phase string, elapsed time.Duration, multi *connect.RemoteUserNatMultiClient) h1SoftLimitPoint {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	samples := []metrics.Sample{
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/gc/mark/assist:cpu-seconds"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/gc/limiter/last-enabled:gc-cycle"},
	}
	metrics.Read(samples)
	owners := multi.MemoryOwnerCensus()
	return h1SoftLimitPoint{
		Phase: phase, ElapsedMs: elapsed.Milliseconds(), Runtime: m.Sys - m.HeapReleased,
		Heap: m.HeapAlloc, Inuse: m.HeapInuse, Slack: m.HeapInuse - m.HeapAlloc,
		Free: m.HeapIdle - m.HeapReleased, Stack: m.StackInuse, TotalAlloc: m.TotalAlloc,
		Goroutines: runtime.NumGoroutine(), Gc: m.NumGC, ForcedGc: m.NumForcedGC, PauseNs: m.PauseTotalNs,
		GcCpu: samples[0].Value.Float64(), GcAssistCpu: samples[1].Value.Float64(),
		HeapGoal: samples[2].Value.Uint64(), LimiterLastCycle: samples[3].Value.Uint64(),
		Clients: owners.Transfer.Clients, Flows: owners.Flows, SendWorkers: owners.Transfer.SendWorkers,
		ReceiveWorkers: owners.Transfer.ReceiveWorkers, Pacing: owners.Transfer.PacingServices,
	}
}

type h1SoftLimitTraffic struct {
	Packets                    int     `json:"packets"`
	UsefulBytes                int64   `json:"useful_bytes"`
	ElapsedNs                  int64   `json:"elapsed_ns"`
	Mbps                       float64 `json:"mbps"`
	P50Ns, P95Ns, P99Ns, MaxNs int64
}

type h1SoftLimitResult struct {
	SoftLimit                   int64 `json:"soft_limit"`
	ProcessSizing, DeviceTarget connect.ByteCount
	GcPercent                   int
	Points                      []h1SoftLimitPoint `json:"points"`
	Traffic                     h1SoftLimitTraffic `json:"traffic"`
	Resume                      h1SoftLimitTraffic `json:"resume"`
	Failure                     string             `json:"failure,omitempty"`
}

func runH1SoftLimitTraffic(device *DeviceLocal, flows, packets int) (h1SoftLimitTraffic, error) {
	return runH1ExperimentTraffic(device, flows, packets, 42000, 123)
}

func runH1ExperimentTraffic(device *DeviceLocal, flows, packets, sourcePort, destinationPort int) (h1SoftLimitTraffic, error) {
	const payloadSize = 1200
	channels := make([]chan uint64, flows)
	latencies := make([]int64, flows*packets)
	for i := range channels {
		channels[i] = make(chan uint64, 1)
	}
	unsub := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		_, payload, err := connect.ParseIpPathWithPayload(packet)
		if err != nil || len(payload) != payloadSize {
			return
		}
		id := binary.LittleEndian.Uint64(payload[:8])
		flow := int(id >> 32)
		if flow < len(channels) {
			select {
			case channels[flow] <- id:
			default:
			}
		}
	})
	defer unsub()
	errors := make(chan error, flows)
	var workers sync.WaitGroup
	started := time.Now()
	for flow := range flows {
		workers.Go(func() {
			payload := bytes.Repeat([]byte{byte(flow + 1)}, payloadSize)
			for sequence := range packets {
				id := uint64(flow)<<32 | uint64(sequence)
				binary.LittleEndian.PutUint64(payload[:8], id)
				packet := craftIpv4Packet(connect.IpProtocolUdp, net.IPv4(10, 0, 0, 5), sourcePort+flow,
					net.IPv4(203, 0, 113, byte(flow+1)), destinationPort, false, payload)
				sent := time.Now()
				if !device.SendPacket(packet, int32(len(packet))) {
					errors <- fmt.Errorf("packet admission refused: flow=%d sequence=%d", flow, sequence)
					return
				}
				select {
				case got := <-channels[flow]:
					if got != id {
						errors <- fmt.Errorf("packet identity mismatch: flow=%d sequence=%d got=%d", flow, sequence, got)
						return
					}
					latencies[flow*packets+sequence] = time.Since(sent).Nanoseconds()
				case <-time.After(5 * time.Second):
					errors <- fmt.Errorf("local H1 echo timeout: flow=%d sequence=%d", flow, sequence)
					return
				}
			}
		})
	}
	workers.Wait()
	elapsed := time.Since(started)
	select {
	case err := <-errors:
		return h1SoftLimitTraffic{}, err
	default:
	}
	slices.Sort(latencies)
	useful := int64(flows * packets * payloadSize)
	return h1SoftLimitTraffic{Packets: flows * packets, UsefulBytes: useful, ElapsedNs: elapsed.Nanoseconds(),
		Mbps: float64(useful) * 8 / elapsed.Seconds() / 1e6, P50Ns: latencies[len(latencies)/2],
		P95Ns: latencies[len(latencies)*95/100], P99Ns: latencies[len(latencies)*99/100], MaxNs: latencies[len(latencies)-1]}, nil
}
