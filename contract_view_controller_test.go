package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

type testing_throughputListener struct {
	stateLock sync.Mutex
	count     int
}

func (self *testing_throughputListener) ThroughputChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
}

func (self *testing_throughputListener) getCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

func TestContractViewController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	vc := device.OpenContractViewController()
	defer device.CloseViewController(vc)

	listener := &testing_throughputListener{}
	sub := vc.AddThroughputListener(listener)
	defer sub.Close()

	// shrink the sampling settings so the test runs fast
	sampleInterval := 10 * time.Millisecond
	windowDuration := 100 * time.Millisecond
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.sampleInterval = sampleInterval
		vc.windowDuration = windowDuration
	}()
	// wake the run loop so it re-reads the sample interval
	vc.settingsMonitor.NotifyAll()

	// wait for the series to start
	timeout := time.Now().Add(5 * time.Second)
	for vc.GetThroughputPoints().Len() < 2 {
		if !time.Now().Before(timeout) {
			t.Fatalf("timeout waiting for throughput points")
		}
		time.Sleep(sampleInterval)
	}
	// let the series run well past the window so the window bound is observable
	time.Sleep(2 * windowDuration)

	points := vc.GetThroughputPoints()
	if n := points.Len(); n < 2 {
		t.Fatalf("expected at least 2 throughput points, got %d", n)
	}
	// the count must be bounded by the window
	if n, maxCount := points.Len(), int(windowDuration/sampleInterval)+3; maxCount < n {
		t.Fatalf("expected at most %d throughput points, got %d", maxCount, n)
	}
	// oldest first, all routes present, all deltas and rates non-negative
	for i := range points.Len() {
		point := points.Get(i)
		if 0 < i && point.Time < points.Get(i-1).Time {
			t.Fatalf("throughput points out of order at %d: %d < %d", i, point.Time, points.Get(i-1).Time)
		}
		for _, sample := range []*ThroughputSample{point.Remote, point.Local, point.Block} {
			if sample == nil {
				t.Fatalf("missing route sample at %d: %+v", i, point)
			}
			if sample.EgressByteCount < 0 || sample.IngressByteCount < 0 {
				t.Fatalf("negative byte count delta at %d: %+v", i, sample)
			}
			if sample.EgressPacketCount < 0 || sample.IngressPacketCount < 0 {
				t.Fatalf("negative packet count delta at %d: %+v", i, sample)
			}
			if sample.EgressBitRate < 0 || sample.IngressBitRate < 0 {
				t.Fatalf("negative bit rate at %d: %+v", i, sample)
			}
		}
	}

	if listener.getCount() < 1 {
		t.Fatalf("expected the throughput listener to fire")
	}
	if vc.GetPacketStats() == nil {
		t.Fatalf("expected packet stats after sampling")
	}

	// the test device allows a provider, so the provider series ticks in parallel
	providerPoints := vc.GetProviderThroughputPoints()
	if n := providerPoints.Len(); n < 2 {
		t.Fatalf("expected at least 2 provider throughput points, got %d", n)
	}
	if n, maxCount := providerPoints.Len(), int(windowDuration/sampleInterval)+3; maxCount < n {
		t.Fatalf("expected at most %d provider throughput points, got %d", maxCount, n)
	}
	for i := range providerPoints.Len() {
		point := providerPoints.Get(i)
		if point.Remote == nil || point.Local == nil || point.Block == nil {
			t.Fatalf("missing provider route sample at %d: %+v", i, point)
		}
	}
	if vc.GetProviderPacketStats() == nil {
		t.Fatalf("expected provider packet stats after sampling")
	}
}

func TestContractViewControllerDenseSampling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	vc := device.OpenContractViewController()
	defer device.CloseViewController(vc)

	// drive a standalone series directly with synthetic times
	series := &throughputSeries{}
	interval := defaultThroughputSampleInterval

	stats := func(remoteEgressByteCount ByteCount) *PacketStats {
		return &PacketStats{RemoteEgressByteCount: remoteEgressByteCount}
	}
	sample := func(packetStats *PacketStats, at time.Time) {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.sampleSeriesWithLock(series, packetStats, at, false)
	}
	remoteEgress := func(i int) ByteCount {
		return series.points[i].Remote.EgressByteCount
	}

	t0 := time.Now()

	// the first sample sets the base without a point
	sample(stats(1000), t0)
	if n := len(series.points); n != 0 {
		t.Fatalf("expected no points after the first sample, got %d", n)
	}

	// the second sample appends a delta point
	sample(stats(3000), t0.Add(interval))
	if n := len(series.points); n != 1 {
		t.Fatalf("expected 1 point, got %d", n)
	}
	if remoteEgress(0) != 2000 {
		t.Fatalf("expected delta 2000, got %d", remoteEgress(0))
	}

	// a gap of missed ticks backfills zero holds and rebases with a zero,
	// so the gap traffic never draws a spike
	sample(stats(9000), t0.Add(4*interval))
	if n := len(series.points); n != 4 {
		t.Fatalf("expected 4 points after the gap, got %d", n)
	}
	for i := 1; i < 4; i += 1 {
		if remoteEgress(i) != 0 {
			t.Fatalf("expected zero hold at %d, got %d", i, remoteEgress(i))
		}
	}

	// a tick with no stats zero-holds
	sample(nil, t0.Add(5*interval))
	if n := len(series.points); n != 5 {
		t.Fatalf("expected 5 points, got %d", n)
	}
	if remoteEgress(4) != 0 {
		t.Fatalf("expected zero hold for nil stats, got %d", remoteEgress(4))
	}

	// stats resuming after the nil tick span a gap, so rebase with a zero
	sample(stats(12000), t0.Add(6*interval))
	if n := len(series.points); n != 6 {
		t.Fatalf("expected 6 points, got %d", n)
	}
	if remoteEgress(5) != 0 {
		t.Fatalf("expected zero rebase after the gap, got %d", remoteEgress(5))
	}

	// the next regular tick resumes deltas
	sample(stats(12500), t0.Add(7*interval))
	if n := len(series.points); n != 7 {
		t.Fatalf("expected 7 points, got %d", n)
	}
	if remoteEgress(6) != 500 {
		t.Fatalf("expected delta 500, got %d", remoteEgress(6))
	}

	// the series is densely sampled: one point per interval
	for i := 1; i < len(series.points); i += 1 {
		dt := series.points[i].Time - series.points[i-1].Time
		if dt < 500 || 1500 < dt {
			t.Fatalf("expected dense sampling, got %dms between points %d and %d", dt, i-1, i)
		}
	}
}

// TestContractViewControllerProviderMirror asserts that provider-series points
// present the relay (remote) traffic mirrored onto the local route: remote
// ingress becomes local egress and remote egress becomes local ingress, since
// the provider counters never fill the local route themselves and the provider
// stats ui reads the local route
func TestContractViewControllerProviderMirror(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	vc := device.OpenContractViewController()
	defer device.CloseViewController(vc)

	series := &throughputSeries{}
	interval := defaultThroughputSampleInterval

	sample := func(packetStats *PacketStats, at time.Time) {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.sampleSeriesWithLock(series, packetStats, at, true)
	}

	t0 := time.Now()
	sample(&PacketStats{}, t0)
	sample(&PacketStats{
		// the forward relay: a remote client's traffic egressed to the internet
		RemoteIngressByteCount:   3000,
		RemoteIngressPacketCount: 30,
		// the return relay: internet traffic back to the remote client
		RemoteEgressByteCount:   1000,
		RemoteEgressPacketCount: 10,
		BlockIngressByteCount:   500,
		BlockIngressPacketCount: 5,
	}, t0.Add(interval))

	if n := len(series.points); n != 1 {
		t.Fatalf("expected 1 point, got %d", n)
	}
	point := series.points[0]
	// remote passes through unchanged
	if point.Remote.IngressByteCount != 3000 || point.Remote.EgressByteCount != 1000 {
		t.Fatalf("expected remote passthrough, got %+v", point.Remote)
	}
	if point.Remote.IngressPacketCount != 30 || point.Remote.EgressPacketCount != 10 {
		t.Fatalf("expected remote packet passthrough, got %+v", point.Remote)
	}
	// local mirrors remote with the direction swapped
	if point.Local.EgressByteCount != 3000 || point.Local.IngressByteCount != 1000 {
		t.Fatalf("expected mirrored local bytes, got %+v", point.Local)
	}
	if point.Local.EgressPacketCount != 30 || point.Local.IngressPacketCount != 10 {
		t.Fatalf("expected mirrored local packets, got %+v", point.Local)
	}
	if point.Local.EgressBitRate != point.Remote.IngressBitRate || point.Local.IngressBitRate != point.Remote.EgressBitRate {
		t.Fatalf("expected mirrored local bit rates, got local %+v remote %+v", point.Local, point.Remote)
	}
	// block passes through unchanged
	if point.Block.IngressByteCount != 500 || point.Block.IngressPacketCount != 5 {
		t.Fatalf("expected block passthrough, got %+v", point.Block)
	}
}
