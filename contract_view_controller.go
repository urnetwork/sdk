// view controller for the live throughput ui.
// Builds time series of throughput samples from the device's cumulative
// packet counters: one series for the client traffic and one for the
// provider traffic relayed for remote clients. Each point splits the
// deltas by route (remote, local, block). The series are poll-driven so
// they tick whether or not a connect client is up, and are densely
// sampled: missed ticks and ticks with no stats are zero-held so the
// series always has one point per sample interval. All methods are safe
// for concurrent use.
package sdk

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	defaultThroughputSampleInterval = 1 * time.Second
	defaultThroughputWindowDuration = 60 * time.Second
	// hard cap on retained points, independent of the window settings
	throughputPointMaxCount = 1024
	// points kept just before the window start, so the spline that renders the
	// left edge of the window has its control points and doesn't reorient as
	// points slide off. these are off-screen and not shown
	throughputPointBufferCount = 2
)

type ThroughputListener interface {
	ThroughputChanged()
}

// throughput for one route over one sample interval.
// the byte and packet counts are deltas over the interval,
// and the bit rates are normalized to bits per second
type ThroughputSample struct {
	EgressByteCount    ByteCount
	IngressByteCount   ByteCount
	EgressPacketCount  int64
	IngressPacketCount int64
	EgressBitRate      int
	IngressBitRate     int
}

// a throughput sample over one sample interval, split by route.
// remote is traffic egressed to providers. local is traffic routed
// to the local user nat. block is traffic dropped by the security rules.
// see `PacketStats` for the route semantics
type ThroughputPoint struct {
	// sample end time, unix millis
	Time   int64
	Remote *ThroughputSample
	Local  *ThroughputSample
	Block  *ThroughputSample
}

// the sampling state for one series
type throughputSeries struct {
	// latest stats, from either the poll or the device push
	latestPacketStats *PacketStats
	// previous poll sample, the base for the next deltas
	prevPacketStats *PacketStats
	prevSampleTime  time.Time
	// time of the last appended point, the base for zero-hold backfill
	lastPointTime time.Time
	// oldest first
	points []*ThroughputPoint
}

type ContractViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	packetStatsChangedSub         Sub
	providerPacketStatsChangedSub Sub

	// notifies the run loop when the sampling settings change
	settingsMonitor *connect.Monitor

	sampleInterval time.Duration
	windowDuration time.Duration

	clientSeries   *throughputSeries
	providerSeries *throughputSeries

	throughputListeners *connect.CallbackList[ThroughputListener]
}

func newContractViewController(ctx context.Context, device Device) *ContractViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ContractViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		settingsMonitor: connect.NewMonitor(),

		sampleInterval: defaultThroughputSampleInterval,
		windowDuration: defaultThroughputWindowDuration,

		clientSeries:   &throughputSeries{},
		providerSeries: &throughputSeries{},

		throughputListeners: connect.NewCallbackList[ThroughputListener](),
	}
	// the push listeners keep the latest stats fresh between polls.
	// the poll in the run loop is the source of truth for the series.
	vc.packetStatsChangedSub = device.AddPacketStatsChangeListener(vc)
	vc.providerPacketStatsChangedSub = device.AddProviderPacketStatsChangeListener(
		&providerPacketStatsForwarder{vc: vc},
	)

	go connect.HandleError(vc.run, cancel)

	return vc
}

// run polls the device packet stats every sample interval and appends
// throughput points for the deltas since the previous poll
func (self *ContractViewController) run() {
	defer self.cancel()

	for {
		var notify chan struct{}
		var sampleInterval time.Duration
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// subscribe before reading the settings so a change can't be missed
			notify = self.settingsMonitor.NotifyChannel()
			sampleInterval = self.sampleInterval
		}()

		select {
		case <-self.ctx.Done():
			return
		case <-notify:
			// the sampling settings changed. restart the wait.
			continue
		case <-time.After(sampleInterval):
		}

		self.sample()
	}
}

// sample polls the device and appends one throughput point per series
func (self *ContractViewController) sample() {
	// the device is an external object. call it outside the state lock.
	packetStats := self.device.GetPacketStats()
	providerPacketStats := self.device.GetProviderPacketStats()
	sampleTime := time.Now()

	appended := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.sampleSeriesWithLock(self.clientSeries, packetStats, sampleTime) {
			appended = true
		}
		if self.sampleSeriesWithLock(self.providerSeries, providerPacketStats, sampleTime) {
			appended = true
		}
	}()

	if appended {
		self.throughputChanged()
	}
}

// must be called with `stateLock`.
// appends the points for one series at one tick: zero-hold backfill for
// missed ticks, then a delta point (or a zero-hold when there are no
// stats or the delta spans a gap). returns whether any point was appended
func (self *ContractViewController) sampleSeriesWithLock(series *throughputSeries, packetStats *PacketStats, sampleTime time.Time) bool {
	appended := false

	// backfill zero holds for missed ticks so the series stays
	// densely sampled at one point per interval
	if !series.lastPointTime.IsZero() {
		t := series.lastPointTime.Add(self.sampleInterval)
		// no point in backfilling outside the window
		if windowStartTime := sampleTime.Add(-self.windowDuration); t.Before(windowStartTime) {
			t = windowStartTime
		}
		// fill up to just before the current tick
		for !t.Add(self.sampleInterval/2).After(sampleTime) && len(series.points) < throughputPointMaxCount {
			series.points = append(series.points, zeroThroughputPoint(t))
			series.lastPointTime = t
			appended = true
			t = t.Add(self.sampleInterval)
		}
	}

	series.latestPacketStats = packetStats
	if packetStats == nil {
		// no stats this tick. zero-hold while the series is live.
		if series.prevPacketStats != nil {
			series.points = append(series.points, zeroThroughputPoint(sampleTime))
			series.lastPointTime = sampleTime
			appended = true
		}
	} else {
		if series.prevPacketStats != nil {
			elapsed := sampleTime.Sub(series.prevSampleTime)
			if 3*self.sampleInterval/2 < elapsed {
				// the delta spans a gap and can't be attributed per
				// interval. rebase and zero-hold this tick.
				series.points = append(series.points, zeroThroughputPoint(sampleTime))
			} else {
				series.points = append(series.points, newThroughputPoint(
					sampleTime,
					elapsed,
					series.prevPacketStats,
					packetStats,
				))
			}
			series.lastPointTime = sampleTime
			appended = true
		}
		series.prevPacketStats = packetStats
		series.prevSampleTime = sampleTime
	}

	series.points = trimThroughputPoints(series.points, sampleTime, self.windowDuration)
	return appended
}

// newThroughputPoint computes the route deltas between two cumulative stats.
// negative deltas are clamped to zero since the accumulators can reset
func newThroughputPoint(sampleTime time.Time, elapsed time.Duration, prev *PacketStats, current *PacketStats) *ThroughputPoint {
	delta := func(current int64, prev int64) int64 {
		if d := current - prev; 0 < d {
			return d
		}
		return 0
	}
	bitRate := func(deltaByteCount ByteCount) int {
		if elapsed <= 0 {
			return 0
		}
		return int(float64(8*deltaByteCount) / elapsed.Seconds())
	}
	sample := func(
		egressByteCount ByteCount,
		ingressByteCount ByteCount,
		egressPacketCount int64,
		ingressPacketCount int64,
	) *ThroughputSample {
		return &ThroughputSample{
			EgressByteCount:    egressByteCount,
			IngressByteCount:   ingressByteCount,
			EgressPacketCount:  egressPacketCount,
			IngressPacketCount: ingressPacketCount,
			EgressBitRate:      bitRate(egressByteCount),
			IngressBitRate:     bitRate(ingressByteCount),
		}
	}
	return &ThroughputPoint{
		Time: sampleTime.UnixMilli(),
		Remote: sample(
			delta(current.RemoteEgressByteCount, prev.RemoteEgressByteCount),
			delta(current.RemoteIngressByteCount, prev.RemoteIngressByteCount),
			delta(current.RemoteEgressPacketCount, prev.RemoteEgressPacketCount),
			delta(current.RemoteIngressPacketCount, prev.RemoteIngressPacketCount),
		),
		Local: sample(
			delta(current.LocalEgressByteCount, prev.LocalEgressByteCount),
			delta(current.LocalIngressByteCount, prev.LocalIngressByteCount),
			delta(current.LocalEgressPacketCount, prev.LocalEgressPacketCount),
			delta(current.LocalIngressPacketCount, prev.LocalIngressPacketCount),
		),
		Block: sample(
			delta(current.BlockEgressByteCount, prev.BlockEgressByteCount),
			delta(current.BlockIngressByteCount, prev.BlockIngressByteCount),
			delta(current.BlockEgressPacketCount, prev.BlockEgressPacketCount),
			delta(current.BlockIngressPacketCount, prev.BlockIngressPacketCount),
		),
	}
}

// a zero-hold point
func zeroThroughputPoint(sampleTime time.Time) *ThroughputPoint {
	return &ThroughputPoint{
		Time:   sampleTime.UnixMilli(),
		Remote: &ThroughputSample{},
		Local:  &ThroughputSample{},
		Block:  &ThroughputSample{},
	}
}

// drops points older than the window (keeping a small off-screen buffer before
// the window start so the spline retains its shape at the left edge), and caps
// the total count
func trimThroughputPoints(points []*ThroughputPoint, now time.Time, windowDuration time.Duration) []*ThroughputPoint {
	windowStartTime := now.Add(-windowDuration).UnixMilli()
	i := 0
	for i < len(points) && points[i].Time < windowStartTime {
		i += 1
	}
	// keep the buffer points just before the window start
	if i -= throughputPointBufferCount; i < 0 {
		i = 0
	}
	if d := len(points) - i - throughputPointMaxCount; 0 < d {
		i += d
	}
	if 0 < i {
		return append([]*ThroughputPoint{}, points[i:]...)
	}
	return points
}

// PacketStatsChangeListener
func (self *ContractViewController) PacketStatsChanged(packetStats *PacketStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientSeries.latestPacketStats = packetStats
}

// the provider packet stats push, forwarded from `providerPacketStatsForwarder`
func (self *ContractViewController) providerPacketStatsPushed(packetStats *PacketStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.providerSeries.latestPacketStats = packetStats
}

// adapts the provider packet stats push to a separate method,
// since the client push uses the same listener interface
type providerPacketStatsForwarder struct {
	vc *ContractViewController
}

func (self *providerPacketStatsForwarder) PacketStatsChanged(packetStats *PacketStats) {
	self.vc.providerPacketStatsPushed(packetStats)
}

// returns a snapshot of the client throughput points, oldest first
func (self *ContractViewController) GetThroughputPoints() *ThroughputPointList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	throughputPoints := NewThroughputPointList()
	throughputPoints.addAll(self.clientSeries.points...)
	return throughputPoints
}

// returns a snapshot of the provider throughput points, oldest first.
// empty when the device has no provider
func (self *ContractViewController) GetProviderThroughputPoints() *ThroughputPointList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	throughputPoints := NewThroughputPointList()
	throughputPoints.addAll(self.providerSeries.points...)
	return throughputPoints
}

// returns the latest client packet stats. May be nil before the first sample.
func (self *ContractViewController) GetPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clientSeries.latestPacketStats
}

// returns the latest provider packet stats.
// Nil when the device has no provider
func (self *ContractViewController) GetProviderPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.providerSeries.latestPacketStats
}

func (self *ContractViewController) SetWindowDurationSeconds(seconds int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.windowDuration = time.Duration(seconds) * time.Second
	}()
	self.settingsMonitor.NotifyAll()
}

func (self *ContractViewController) GetWindowDurationSeconds() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return int(self.windowDuration / time.Second)
}

func (self *ContractViewController) throughputChanged() {
	for _, listener := range self.throughputListeners.Get() {
		connect.HandleError(func() {
			listener.ThroughputChanged()
		})
	}
}

func (self *ContractViewController) AddThroughputListener(listener ThroughputListener) Sub {
	callbackId := self.throughputListeners.Add(listener)
	return newSub(func() {
		self.throughputListeners.Remove(callbackId)
	})
}

func (self *ContractViewController) Start() {}

func (self *ContractViewController) Stop() {}

func (self *ContractViewController) Close() {
	deviceLog(self.device).Info("[ctvc]close")
	self.cancel()
	self.packetStatsChangedSub.Close()
	self.providerPacketStatsChangedSub.Close()
}
