//go:build !ios

package sdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Test-only mobile policy overlays change the quality count, never production
// files. Five provider fixtures are present in every arm. The H1 carrier and
// routing/selection code are real; origins are in-process UDP echo providers.
// These results cannot substitute for physical TCP/TLS/fast.com acceptance.
func TestDeviceLocalH1TopologyExperiment(t *testing.T) {
	quality, err := strconv.Atoi(os.Getenv("URNETWORK_H1_TOPOLOGY_QUALITY"))
	if err != nil || quality < 2 || 4 < quality {
		t.Skip("opt-in: URNETWORK_H1_TOPOLOGY_QUALITY=2, 3 or 4 with matching test overlay")
	}
	if !mobileRuntime() || mobileQualityWindowSize != quality || mobileSpeedWindowSize != 1 {
		t.Fatal("mobile topology overlay does not match the requested arm")
	}
	if runIsolatedLoadTest(t) {
		return
	}
	const processSizing = 32 * 1024 * 1024
	SetMemoryLimit(processSizing)
	debug.SetGCPercent(gcPercentForPlatform("ios"))
	debug.SetMemoryLimit(processSizing)
	f := newH1OwnerFixtureWithProviders(t, 5, 120*time.Second)
	device, generator, multi := f.connectDeviceWithTarget(t, 20*1024*1024)
	h1OwnerTraffic(t, device)
	deadline := time.Now().Add(10 * time.Second)
	for (multi.MemoryOwnerCensus().Transfer.Clients != int64(quality+1) || h1TopologyAddedCount(multi) != quality+1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := multi.MemoryOwnerCensus().Transfer.Clients; got != int64(quality+1) || h1TopologyAddedCount(multi) != quality+1 || device.transferMemory == nil {
		t.Fatalf("topology not formed: clients=%d added=%d expected=%d mobile-budget=%t", got, h1TopologyAddedCount(multi), quality+1, device.transferMemory != nil)
	}
	result := h1TopologyResult{
		Quality: quality, Speed: 1, ProviderFixtures: len(f.providers),
		SoftLimit: debug.SetMemoryLimit(-1), ProcessSizing: connect.MemoryBudget(),
		DeviceTarget: device.settings.MemoryTargetByteCount, GcPercent: gcPercentForPlatform("ios"),
		Points: make([]h1SoftLimitPoint, 0, 512),
	}
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
		traffic, err := runH1ExperimentTraffic(device, 64, 1024, sourcePort, destinationPort)
		close(stop)
		<-done
		result.Points = append(result.Points, point("post-"+phase))
		return traffic, err
	}
	result.QualityTraffic, err = measure("quality", 42000, 443)
	if err == nil {
		result.SpeedTraffic, err = measure("speed", 43000, 123)
	}
	if err == nil {
		for i := range 12 {
			time.Sleep(time.Second)
			result.Points = append(result.Points, point(fmt.Sprintf("quiet-%02d", i+1)))
		}
		for i := range 3 {
			runtime.GC()
			time.Sleep(100 * time.Millisecond)
			result.Points = append(result.Points, point(fmt.Sprintf("forced-gc-%d", i+1)))
		}
		debug.FreeOSMemory()
		result.Points = append(result.Points, point("forced-scavenge"))
		result.Resume, err = runH1ExperimentTraffic(device, 1, 64, 44000, 443)
		result.Points = append(result.Points, point("resumed"))
	}
	{
		// A faster result may not omit packets at an improperly modeled
		// provider handoff. Count before injecting the deliberate failure.
		for _, provider := range f.providers {
			result.ProviderHandoffDrops += provider.ReceiveStats().PackHandoffDropCount
			result.ProviderTransfer.add(provider.ReceiveStats())
		}
		generator.mu.Lock()
		for _, client := range generator.clients {
			result.ClientHandoffDrops += client.ReceiveStats().PackHandoffDropCount
			result.ClientTransfer.add(client.ReceiveStats())
		}
		generator.mu.Unlock()
		result.PacketPressureDrops = device.mobilePacketPressureDropCount.Load()
		if err == nil && (result.ProviderHandoffDrops != 0 || result.ClientHandoffDrops != 0) {
			err = fmt.Errorf("reliable H1 fixture dropped packets at a receive handoff")
		}
	}
	if err == nil {
		// Inject only after all normal memory/performance observations. A
		// blackhole includes Transfer ACK/control traffic, not just IP data.
		result.Failover, err = runH1TopologyFailover(f, device, multi)
		result.Points = append(result.Points, point("post-failover"))
	}
	if err != nil {
		result.Failure = err.Error()
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	t.Logf("H1_TOPOLOGY_RESULT %s", encoded)
	if debug.SetMemoryLimit(-1) != processSizing || connect.MemoryBudget() != processSizing {
		t.Fatal("runtime policy changed during topology arm")
	}
	if result.Failure != "" {
		t.Fatal(result.Failure)
	}
}

func h1TopologyAddedCount(multi *connect.RemoteUserNatMultiClient) int {
	count := 0
	for _, event := range multi.Monitor().ProviderEvents() {
		if event.State.IsActive() {
			count++
		}
	}
	return count
}

type h1TopologyResult struct {
	Quality, Speed, ProviderFixtures         int
	SoftLimit                                int64
	ProcessSizing, DeviceTarget              connect.ByteCount
	GcPercent                                int
	Points                                   []h1SoftLimitPoint `json:"points"`
	QualityTraffic, SpeedTraffic             h1SoftLimitTraffic
	Resume                                   h1SoftLimitTraffic
	ProviderHandoffDrops, ClientHandoffDrops uint64
	PacketPressureDrops                      int64
	ProviderTransfer, ClientTransfer         h1TopologyTransferCounts
	Failover                                 h1TopologyFailover
	Failure                                  string `json:"failure,omitempty"`
}

type h1TopologyTransferCounts struct {
	NoAckOffered, NoAckWritten, NoAckRefused, NoAckDiscard, PrewireExpired, ReceiveQueueDrops, AckHandoffDrops uint64
}

func (c *h1TopologyTransferCounts) add(s connect.ClientReceiveStatsSnapshot) {
	c.NoAckOffered += s.SendNoAckOfferedCount
	c.NoAckWritten += s.SendNoAckWriteCount
	c.NoAckRefused += s.SendNoAckRefusedCount
	c.NoAckDiscard += s.SendNoAckDiscardCount
	c.PrewireExpired += s.SendPackDeadlineDropCount
	c.ReceiveQueueDrops += s.ReceiveQueueDropCount
	c.AckHandoffDrops += s.AckHandoffDropCount
}

type h1TopologyFailover struct {
	RemovalNs, DroppedFrames    int64
	HealthyBefore, HealthyAfter int
	FreshFlowTraffic            h1SoftLimitTraffic
}

func runH1TopologyFailover(f *h1OwnerFixture, device *DeviceLocal, multi *connect.RemoteUserNatMultiClient) (h1TopologyFailover, error) {
	var result h1TopologyFailover
	payload := []byte("local-h1-topology-victim-probe")
	providers := make(chan connect.Id, 1)
	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		_, received, err := connect.ParseIpPathWithPayload(packet)
		if err == nil && bytes.Equal(received, payload) {
			select {
			case providers <- source.SourceId:
			default:
			}
		}
	})
	defer unsub()
	packet := craftIpv4Packet(connect.IpProtocolUdp, net.IPv4(10, 0, 0, 5), 45000,
		net.IPv4(203, 0, 113, 201), 443, false, payload)
	if !device.SendPacket(packet, int32(len(packet))) {
		return result, fmt.Errorf("failover victim probe admission refused")
	}
	var victim connect.Id
	select {
	case victim = <-providers:
	case <-time.After(5 * time.Second):
		return result, fmt.Errorf("failover victim probe timeout")
	}
	active := func() (int, bool) {
		count, victimActive := 0, false
		for _, event := range multi.Monitor().ProviderEvents() {
			if event.State.IsActive() {
				count++
				victimActive = victimActive || event.EgressClientId == victim
			}
		}
		return count, victimActive
	}
	var victimActive bool
	result.HealthyBefore, victimActive = active()
	if !victimActive || result.HealthyBefore < 3 {
		return result, fmt.Errorf("failover victim was not an active multi-exit provider")
	}
	started := time.Now()
	f.blackhole.Store(&victim)
	defer f.blackhole.Store(nil)
	// Keep the existing proven flow active so ordinary production cping and
	// Transfer lifetime machinery see a real unavailable exit. Do not alter
	// those timers, force quarantine, or invoke a private removal hook.
	for time.Since(started) < 65*time.Second {
		result.HealthyAfter, victimActive = active()
		if !victimActive {
			result.RemovalNs = time.Since(started).Nanoseconds()
			result.DroppedFrames = f.dropped.Load()
			var err error
			result.FreshFlowTraffic, err = runH1ExperimentTraffic(device, 16, 64, 46000, 443)
			return result, err
		}
		device.SendPacket(packet, int32(len(packet)))
		time.Sleep(250 * time.Millisecond)
	}
	result.DroppedFrames = f.dropped.Load()
	return result, fmt.Errorf("blackholed provider not removed within unchanged 65s observation bound")
}
