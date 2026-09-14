package sdk

import (
	"context"
	"slices"
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

func TestThroughputSeriesNotificationStopsWhenRetainedPointsIdle(t *testing.T) {
	active := zeroThroughputPoint(time.Now())
	active.Remote.EgressByteCount = 1
	series := &throughputSeries{points: []*ThroughputPoint{active}}

	if !throughputSeriesNeedsNotification(series) {
		t.Fatal("active series did not request a notification")
	}

	// zero holds after the activity keep notifying while the active point is
	// still retained: the window contents (and the window transport totals)
	// change as it ages toward the trim edge
	for i := 1; i <= 8; i++ {
		series.points = append(series.points, zeroThroughputPoint(time.Now().Add(time.Duration(i)*time.Second)))
		if !throughputSeriesNeedsNotification(series) {
			t.Fatalf("quiet sample %d stopped notifications while an active point was retained", i)
		}
	}

	// once the trim drops the last active point, every retained point is a zero
	// hold and the snapshots are timestamp-only
	series.points = series.points[1:]
	if throughputSeriesNeedsNotification(series) {
		t.Fatal("idle series continued notifying after the last active point was trimmed")
	}

	resumed := zeroThroughputPoint(time.Now().Add(10 * time.Second))
	resumed.Local.IngressPacketCount = 1
	series.points = append(series.points, resumed)
	if !throughputSeriesNeedsNotification(series) {
		t.Fatal("new activity did not resume notifications")
	}
}

// TestThroughputSeriesNotifyDeliversOneIdleSnapshot asserts the per-tick
// notification decision: every active tick, one idle snapshot when the series
// starts or goes quiet, then silence until new activity
func TestThroughputSeriesNotifyDeliversOneIdleSnapshot(t *testing.T) {
	series := &throughputSeries{}
	now := time.Now()

	// a fresh series delivers its first zero point once
	series.points = append(series.points, zeroThroughputPoint(now))
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("fresh series did not deliver its first snapshot")
	}
	series.points = append(series.points, zeroThroughputPoint(now.Add(time.Second)))
	if throughputSeriesNotifyWithLock(series) {
		t.Fatal("idle series notified twice")
	}

	// activity notifies every tick
	active := zeroThroughputPoint(now.Add(2 * time.Second))
	active.Remote.IngressByteCount = 10
	series.points = append(series.points, active)
	for i := 0; i < 3; i++ {
		if !throughputSeriesNotifyWithLock(series) {
			t.Fatalf("active series stopped notifying at %d", i)
		}
		series.points = append(series.points, zeroThroughputPoint(now.Add(time.Duration(3+i)*time.Second)))
	}

	// the trim drops the active point: one final idle snapshot, then quiet
	series.points = series.points[3:]
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("series going idle did not deliver its final snapshot")
	}
	if throughputSeriesNotifyWithLock(series) {
		t.Fatal("idle series notified after its final snapshot")
	}
}

// TestContractViewControllerTransportDistribution asserts the per-transport
// window totals: exact for the physical carriers, reconciled for the unknown
// bucket, invalidated by a counter reset, and bounded by the window
func TestContractViewControllerTransportDistribution(t *testing.T) {
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

	// cumulative stats with a carrier breakdown. the aggregate remote is the sum
	// of the carriers, as the device reports it
	type carrier struct {
		transportType TransportType
		egress        ByteCount
		ingress       ByteCount
	}
	stats := func(carriers ...carrier) *PacketStats {
		packetStats := &PacketStats{TransportStats: NewTransportPacketStatsList()}
		for _, c := range carriers {
			packetStats.RemoteEgressByteCount += c.egress
			packetStats.RemoteEgressPacketCount += int64(c.egress / 100)
			packetStats.RemoteIngressByteCount += c.ingress
			packetStats.RemoteIngressPacketCount += int64(c.ingress / 100)
			packetStats.TransportStats.Add(&TransportPacketStats{
				TransportType: c.transportType,
				Stats: &PacketStats{
					RemoteEgressByteCount:    c.egress,
					RemoteEgressPacketCount:  int64(c.egress / 100),
					RemoteIngressByteCount:   c.ingress,
					RemoteIngressPacketCount: int64(c.ingress / 100),
				},
			})
		}
		return packetStats
	}
	sample := func(packetStats *PacketStats, at time.Time, provider bool) {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.sampleSeriesWithLock(series, packetStats, at, provider)
	}
	enabled := map[TransportType]bool{TransportTypeH3: true, TransportTypeH1: true}
	windowStats := func(now time.Time) map[TransportType]*TransportShare {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		distribution := vc.transportDistributionWithLock(series, now, enabled)
		list := distribution.Shares
		byType := map[TransportType]*TransportShare{}
		for _, item := range list.getAll() {
			byType[item.TransportType] = item
		}
		// every type is present in the stable order
		if list.Len() != len(transportTypes()) {
			t.Fatalf("expected %d transport rows, got %d", len(transportTypes()), list.Len())
		}
		for i, transportType := range transportTypes() {
			if list.Get(i).TransportType != transportType {
				t.Fatalf("expected transport %s at %d, got %s", transportType, i, list.Get(i).TransportType)
			}
		}
		// the enabled flags follow the settings, not the traffic
		if !byType[TransportTypeH3].Enabled || !byType[TransportTypeH1].Enabled || byType[TransportTypeDns].Enabled || byType[TransportTypeP2p].Enabled {
			t.Fatalf("expected enabled flags from the settings, got %+v", byType)
		}
		return byType
	}

	t0 := time.Now()

	// base sample: 1000 bytes on h3, 500 on h1 already carried
	sample(stats(
		carrier{TransportTypeH3, 1000, 4000},
		carrier{TransportTypeH1, 500, 0},
	), t0, false)

	// t1: h3 carries 2000 more egress and 1000 ingress; a 300-byte packet is
	// admitted but not yet written to a route (unknown)
	sample(stats(
		carrier{TransportTypeH3, 3000, 5000},
		carrier{TransportTypeH1, 500, 0},
		carrier{TransportTypeUnknown, 300, 0},
	), t0.Add(interval), false)

	byType := windowStats(t0.Add(interval))
	if got := byType[TransportTypeH3]; got.EgressByteCount != 2000 || got.IngressByteCount != 1000 {
		t.Fatalf("expected h3 2000/1000, got %+v", got)
	}
	if got := byType[TransportTypeH1]; got.EgressByteCount != 0 || got.IngressByteCount != 0 {
		t.Fatalf("expected idle h1, got %+v", got)
	}
	// the in-flight packet is provisionally unknown
	if got := byType[TransportTypeUnknown]; got.EgressByteCount != 300 || got.EgressPacketCount != 3 {
		t.Fatalf("expected unknown 300, got %+v", got)
	}

	// t2: the in-flight packet is attributed to h1 (moved out of unknown), and
	// h1 carries 700 more. the aggregate only grows by the 700
	sample(stats(
		carrier{TransportTypeH3, 3000, 5000},
		carrier{TransportTypeH1, 1500, 0},
		carrier{TransportTypeUnknown, 0, 0},
	), t0.Add(2*interval), false)

	byType = windowStats(t0.Add(2 * interval))
	if got := byType[TransportTypeH3]; got.EgressByteCount != 2000 || got.IngressByteCount != 1000 {
		t.Fatalf("expected h3 unchanged 2000/1000, got %+v", got)
	}
	// h1 owns the moved packet plus its own new traffic
	if got := byType[TransportTypeH1]; got.EgressByteCount != 1000 || got.EgressPacketCount != 10 {
		t.Fatalf("expected h1 1000, got %+v", got)
	}
	// and the unknown bucket reconciles to zero over the window: the provisional
	// +300 at t1 and the -300 move at t2 cancel instead of double counting
	if got := byType[TransportTypeUnknown]; got.EgressByteCount != 0 || got.EgressPacketCount != 0 {
		t.Fatalf("expected unknown reconciled to 0, got %+v", got)
	}
	// the carriers total to the window's aggregate remote traffic
	var remoteEgress, carrierEgress int64
	for _, point := range series.points {
		remoteEgress += int64(point.Remote.EgressByteCount)
	}
	for _, item := range byType {
		carrierEgress += int64(item.EgressByteCount)
	}
	if remoteEgress != carrierEgress {
		t.Fatalf("expected carriers to total the aggregate %d, got %d", remoteEgress, carrierEgress)
	}
	// the render values: h3 carried 3000 of 4000 bytes, h1 1000
	if got := byType[TransportTypeH3]; !got.Used || got.Percent != 75 || got.Share != 0.75 || got.Boundary != 0.75 {
		t.Fatalf("expected h3 75%% used, got %+v", got)
	}
	if got := byType[TransportTypeH1]; !got.Used || got.Percent != 25 || got.Share != 0.25 || got.Boundary != 1 {
		t.Fatalf("expected h1 25%% used, got %+v", got)
	}
	// idle transports draw nothing: zero-width segments at the previous edge
	if got := byType[TransportTypeDns]; got.Used || got.Percent != 0 || got.Share != 0 || got.Boundary != 1 {
		t.Fatalf("expected idle dns, got %+v", got)
	}
	if got := byType[TransportTypeUnknown]; got.Used || got.Boundary != 1 {
		t.Fatalf("expected idle unknown with the last boundary at 1, got %+v", got)
	}

	// the window bounds the totals: a window ending past the points sees them
	// age out
	byType = windowStats(t0.Add(3*interval + defaultThroughputWindowDuration))
	for transportType, item := range byType {
		if item.EgressByteCount != 0 || item.IngressByteCount != 0 {
			t.Fatalf("expected %s to age out of the window, got %+v", transportType, item)
		}
	}
	// a window starting exactly at the last point still includes it, and only
	// it: h1 keeps the move plus its own delta, and the clamp trims the unknown
	// bucket's -300 (the admit aged out of the window before the move) to zero
	// rather than surfacing a negative total
	byType = windowStats(t0.Add(2*interval + defaultThroughputWindowDuration))
	if got := byType[TransportTypeH1]; got.EgressByteCount != 1000 {
		t.Fatalf("expected only the t2 h1 delta in the window, got %+v", got)
	}
	if got := byType[TransportTypeH3]; got.EgressByteCount != 0 || got.IngressByteCount != 0 {
		t.Fatalf("expected the t1 h3 delta to age out, got %+v", got)
	}
	if got := byType[TransportTypeUnknown]; got.EgressByteCount != 0 {
		t.Fatalf("expected the unknown bucket to clamp at zero, got %+v", got)
	}

	// t3: the accumulators reset (a new client) -- the aggregate delta clamps
	// to zero and the carrier deltas are invalidated for the same direction
	sample(stats(
		carrier{TransportTypeH3, 100, 5000},
	), t0.Add(3*interval), false)
	last := series.points[len(series.points)-1]
	if last.Remote.EgressByteCount != 0 {
		t.Fatalf("expected the reset egress delta to clamp to 0, got %+v", last.Remote)
	}
	if last.transportDeltas == nil {
		t.Fatalf("expected transport deltas on the reset point")
	}
	for i, delta := range last.transportDeltas {
		if delta.egressByteCount != 0 || delta.egressPacketCount != 0 {
			t.Fatalf("expected reset egress deltas to be zeroed at %d, got %+v", i, delta)
		}
	}
	// ingress did not reset (5000 -> 5000): no phantom negative from the
	// disappeared h1 row either, since h1 ingress was 0
	byType = windowStats(t0.Add(3 * interval))
	if got := byType[TransportTypeH3]; got.EgressByteCount != 2000 || got.IngressByteCount != 1000 {
		t.Fatalf("expected h3 window totals to survive the reset, got %+v", got)
	}

	// stats without a carrier breakdown contribute nothing to the totals
	sample(&PacketStats{RemoteEgressByteCount: 100, RemoteEgressPacketCount: 1}, t0.Add(4*interval), false)
	last = series.points[len(series.points)-1]
	if last.transportDeltas != nil {
		t.Fatalf("expected no transport deltas without a breakdown, got %+v", last.transportDeltas)
	}

	// the provider series carries the breakdown through the mirror
	providerSeries := &throughputSeries{}
	series = providerSeries
	sample(stats(carrier{TransportTypeDns, 0, 0}), t0, true)
	sample(stats(carrier{TransportTypeDns, 800, 200}), t0.Add(interval), true)
	byType = windowStats(t0.Add(interval))
	if got := byType[TransportTypeDns]; got.EgressByteCount != 800 || got.IngressByteCount != 200 {
		t.Fatalf("expected the provider mirror to carry the dns breakdown, got %+v", got)
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

// TestNewTransportDistribution asserts the render math: shares by byte
// fraction, boundaries as a running sum ending exactly at 1, whole percents
// that sum to exactly 100 by largest remainder with stable-order ties, used
// flags, and the inactive empty window
func TestNewTransportDistribution(t *testing.T) {
	shares := func(bytes map[TransportType]ByteCount) []*TransportShare {
		out := []*TransportShare{}
		for _, transportType := range transportTypes() {
			b := bytes[transportType]
			out = append(out, &TransportShare{
				TransportType:    transportType,
				EgressByteCount:  b / 2,
				IngressByteCount: b - b/2,
			})
		}
		return out
	}
	byType := func(distribution *TransportDistribution) map[TransportType]*TransportShare {
		m := map[TransportType]*TransportShare{}
		for _, share := range distribution.Shares.getAll() {
			m[share.TransportType] = share
		}
		return m
	}

	// empty window: inactive, everything zero
	empty := newTransportDistribution(shares(nil))
	if empty.Active || empty.ByteCount != 0 {
		t.Fatalf("expected an inactive empty distribution, got %+v", empty)
	}
	for _, share := range empty.Shares.getAll() {
		if share.Used || share.Share != 0 || share.Boundary != 0 || share.Percent != 0 {
			t.Fatalf("expected zero share for %s, got %+v", share.TransportType, share)
		}
	}

	// thirds: 33+33+33 would be 99; the largest remainder hands the missing
	// point to the first in stable order among the equal remainders
	thirds := newTransportDistribution(shares(map[TransportType]ByteCount{
		TransportTypeH3:  1000,
		TransportTypeH1:  1000,
		TransportTypeDns: 1000,
	}))
	if !thirds.Active || thirds.ByteCount != 3000 {
		t.Fatalf("expected an active 3000 byte distribution, got %+v", thirds)
	}
	m := byType(thirds)
	if m[TransportTypeH3].Percent != 34 || m[TransportTypeH1].Percent != 33 || m[TransportTypeDns].Percent != 33 {
		t.Fatalf("expected 34/33/33, got %d/%d/%d", m[TransportTypeH3].Percent, m[TransportTypeH1].Percent, m[TransportTypeDns].Percent)
	}
	sum := 0
	for _, share := range thirds.Shares.getAll() {
		sum += share.Percent
	}
	if sum != 100 {
		t.Fatalf("expected percents to sum to 100, got %d", sum)
	}
	// boundaries: a running sum in stable order, idle transports hold the
	// previous edge, the last is exactly 1
	b := []float64{}
	for _, share := range thirds.Shares.getAll() {
		b = append(b, share.Boundary)
	}
	if !(0.33 < b[0] && b[0] < 0.34) || !(0.66 < b[1] && b[1] < 0.67) || b[2] != 1 || b[3] != 1 || b[4] != 1 || b[5] != 1 {
		t.Fatalf("expected running boundaries ending at 1, got %v", b)
	}
	if m[TransportTypeDnsPump].Used || m[TransportTypeP2p].Used || m[TransportTypeUnknown].Used {
		t.Fatalf("expected idle transports unused")
	}

	// a tiny share can round to 0 percent while still used (drawn): 999/1
	// floors to 99/0 and the missing point goes to the larger remainder (0.9 vs
	// 0.1). apps label a used 0 as "<1%"; the sum is still exactly 100
	tiny := byType(newTransportDistribution(shares(map[TransportType]ByteCount{
		TransportTypeH3:      999,
		TransportTypeUnknown: 1,
	})))
	if tiny[TransportTypeH3].Percent+tiny[TransportTypeUnknown].Percent != 100 {
		t.Fatalf("expected the tiny split to sum to 100, got %+v %+v", tiny[TransportTypeH3], tiny[TransportTypeUnknown])
	}
	if !tiny[TransportTypeUnknown].Used || tiny[TransportTypeUnknown].Percent != 0 || tiny[TransportTypeH3].Percent != 100 {
		t.Fatalf("expected 100/0 with the sliver still used, got %+v %+v", tiny[TransportTypeH3], tiny[TransportTypeUnknown])
	}
	if tiny[TransportTypeUnknown].Boundary != 1 || !(0.998 < tiny[TransportTypeH3].Boundary && tiny[TransportTypeH3].Boundary < 1) {
		t.Fatalf("expected the sliver to own the last 0.1%% of the bar, got %+v %+v", tiny[TransportTypeH3], tiny[TransportTypeUnknown])
	}

	// enabled flags pass through untouched by the math
	flagged := shares(map[TransportType]ByteCount{TransportTypeH1: 10})
	flagged[0].Enabled = true
	f := byType(newTransportDistribution(flagged))
	if !f[TransportTypeH3].Enabled || f[TransportTypeH3].Used || f[TransportTypeH1].Enabled || !f[TransportTypeH1].Used {
		t.Fatalf("expected enabled independent of used, got %+v %+v", f[TransportTypeH3], f[TransportTypeH1])
	}
}

// TestContractViewControllerEnabledFlagsFollowTransportSettings asserts the
// distribution's enabled flags track the device's client and provider policies
// through the transport settings change listeners: seeded at open, updated on
// every change, independent per policy
func TestContractViewControllerEnabledFlagsFollowTransportSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	// a non-default client policy set before the controller opens is seeded
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})

	vc := device.OpenContractViewController()
	defer device.CloseViewController(vc)

	enabledOf := func(distribution *TransportDistribution) []TransportType {
		enabled := []TransportType{}
		for _, share := range distribution.Shares.getAll() {
			if share.Enabled {
				enabled = append(enabled, share.TransportType)
			}
		}
		return enabled
	}

	if got := enabledOf(vc.GetTransportDistribution()); !slices.Equal(got, []TransportType{TransportTypeH1}) {
		t.Fatalf("expected the seeded client policy h1, got %v", got)
	}
	// the provider default enables every selectable carrier
	if got := enabledOf(vc.GetProviderTransportDistribution()); !slices.Equal(got, []TransportType{TransportTypeH3, TransportTypeH1, TransportTypeDns, TransportTypeDnsPump}) {
		t.Fatalf("expected the default provider policy, got %v", got)
	}

	// a change while open is delivered by the listener (the device fires
	// synchronously)
	auto := DefaultTransportSettings()
	auto.SetAutoModeEnabled(TransportModeH3, false)
	auto.SetAutoModeEnabled(TransportModeDnsPump, false)
	device.SetTransportSettings(auto)
	if got := enabledOf(vc.GetTransportDistribution()); !slices.Equal(got, []TransportType{TransportTypeH1, TransportTypeDns}) {
		t.Fatalf("expected the changed client policy h1, dns, got %v", got)
	}
	// the client change leaves the provider flags alone, and vice versa
	if got := enabledOf(vc.GetProviderTransportDistribution()); len(got) != 4 {
		t.Fatalf("expected the provider policy untouched, got %v", got)
	}
	device.SetProviderTransportSettings(&TransportSettings{Mode: TransportModeDnsPump})
	if got := enabledOf(vc.GetProviderTransportDistribution()); !slices.Equal(got, []TransportType{TransportTypeDnsPump}) {
		t.Fatalf("expected the changed provider policy dnspump, got %v", got)
	}
	if got := enabledOf(vc.GetTransportDistribution()); !slices.Equal(got, []TransportType{TransportTypeH1, TransportTypeDns}) {
		t.Fatalf("expected the client policy untouched, got %v", got)
	}
	// p2p and unknown are never enabled
	for _, share := range vc.GetTransportDistribution().Shares.getAll() {
		if (share.TransportType == TransportTypeP2p || share.TransportType == TransportTypeUnknown) && share.Enabled {
			t.Fatalf("expected %s never enabled", share.TransportType)
		}
	}
}
