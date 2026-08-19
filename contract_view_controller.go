//go:build !ios_extension

// view controller for the live throughput ui.
// Builds time series of throughput samples from the device's cumulative
// packet counters: one series for the client traffic and one for the
// provider traffic relayed for remote clients. Each point splits the
// deltas by route (remote, local, block). The series are poll-driven so
// they tick whether or not a connect client is up, and are densely
// sampled: missed ticks and ticks with no stats are zero-held so the
// series always has one point per sample interval. Alongside the route
// series, the controller partitions the remote traffic of the window by the
// physical transport that carried it, ready to render as a stacked bar (see
// `GetTransportDistribution`). All methods are safe for concurrent use.
package sdk

import (
	"cmp"
	"context"
	"math"
	"slices"
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
// see `PacketStats` for the route semantics.
//
// the provider series reuses these fields with a mirrored meaning: the raw
// provider counters only populate the remote and block routes (there is no
// split-tunnel local route when relaying), so its local route is synthesized
// as a mirror of the remote relay traffic — remote ingress (a client's egress
// relayed out to the internet) is presented as local egress, and remote egress
// (the return relayed back to the client) as local ingress. see
// `mirrorProviderThroughputPoint`
type ThroughputPoint struct {
	// sample end time, unix millis
	Time   int64
	Remote *ThroughputSample
	Local  *ThroughputSample
	Block  *ThroughputSample
	// the signed remote deltas of this point by transport type, in
	// `transportTypes` order. nil for a zero hold. kept off the exported
	// surface: a delta can be negative when a packet's provisional unknown
	// attribution moves to its physical carrier at a later sample than the one
	// that admitted it (see connect `transportPacketAttribution`), so the values
	// only reconcile summed over the window (see `GetTransportDistribution`)
	transportDeltas []transportThroughputDelta
}

// TransportShare is one transport type's slice of the window's remote traffic,
// ready to render as a segment of a stacked bar plus its legend entry. Local
// and blocked traffic never enter a carrier, so they are absent by
// construction (see `PacketStats.TransportStats`). Totals, not rates.
type TransportShare struct {
	TransportType      TransportType
	EgressByteCount    ByteCount
	IngressByteCount   ByteCount
	EgressPacketCount  int64
	IngressPacketCount int64
	// the transport's fraction of the window's remote bytes (both directions),
	// 0..1. 0 while idle. the shares of a distribution sum to 1 when there is
	// traffic
	Share float64
	// the cumulative share through this transport in the stable order: the
	// right edge of its segment as a fraction of the bar width, 0..1. Rendering
	// every segment from its neighbours' boundaries tiles exactly 100% of the
	// bar, also while a client tweens between two distributions. the last
	// transport's boundary is 1 when there is traffic and 0 when there is none
	Boundary float64
	// a whole percent for the legend: largest remainder over the used
	// transports, so the percents sum to exactly 100. 0 while idle
	Percent int
	// carried traffic in the window: draws a segment and a legend entry
	Used bool
	// enabled by the transport settings in force. an enabled transport that is
	// not used belongs in the unused footer; a used transport that is not
	// enabled (p2p, unknown, a just-disabled mode with traffic still in the
	// window) is still drawn, since the bar is the truthful proportion
	Enabled bool
}

type TransportShareList struct {
	exportedList[*TransportShare]
}

func NewTransportShareList() *TransportShareList {
	return &TransportShareList{
		exportedList: *newExportedList[*TransportShare](),
	}
}

// TransportDistribution is the window's remote traffic partitioned by the
// transport that carried it, in the stable order (h3, h1, dns, dnspump, p2p,
// unknown) with every transport present, zeros included.
type TransportDistribution struct {
	Shares *TransportShareList
	// the window's remote bytes, both directions
	ByteCount ByteCount
	// whether any transport carried traffic in the window
	Active bool
}

// the signed remote deltas of one transport type over one sample interval
type transportThroughputDelta struct {
	egressByteCount    int64
	ingressByteCount   int64
	egressPacketCount  int64
	ingressPacketCount int64
}

func (self *transportThroughputDelta) add(other transportThroughputDelta) {
	self.egressByteCount += other.egressByteCount
	self.ingressByteCount += other.ingressByteCount
	self.egressPacketCount += other.egressPacketCount
	self.ingressPacketCount += other.ingressPacketCount
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
	// whether the idle (all zero hold) snapshot has been delivered since the
	// series was last active. see `throughputSeriesNotifyWithLock`
	idleNotified bool
}

type ContractViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	packetStatsChangedSub         Sub
	providerPacketStatsChangedSub Sub

	transportSettingsChangedSub         Sub
	providerTransportSettingsChangedSub Sub

	// notifies the run loop when the sampling settings change
	settingsMonitor *connect.Monitor

	sampleInterval time.Duration
	windowDuration time.Duration

	clientSeries   *throughputSeries
	providerSeries *throughputSeries

	// the transport types the device's client / provider transport settings
	// enable, kept current by the settings change listeners. read for the
	// distribution's enabled flags
	enabledTransportTypes         map[TransportType]bool
	enabledProviderTransportTypes map[TransportType]bool

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

	// the transport policy drives the distribution's enabled flags. the
	// listeners deliver every change (and the device's truth again on each rpc
	// sync); the seed read covers the time before the first change. offline
	// the seed is the pending or last known policy
	vc.transportSettingsChangedSub = device.AddTransportSettingsChangeListener(
		&transportSettingsForwarder{vc: vc},
	)
	vc.providerTransportSettingsChangedSub = device.AddProviderTransportSettingsChangeListener(
		&providerTransportSettingsForwarder{vc: vc},
	)
	vc.transportSettingsChanged(device.GetTransportSettings(), false)
	vc.transportSettingsChanged(device.GetProviderTransportSettings(), true)

	go connect.HandleError(vc.run, cancel)

	return vc
}

// run polls the device packet stats every sample interval and appends
// throughput points for the deltas since the previous poll
func (self *ContractViewController) run() {
	defer self.cancel()

	// the absolute deadline of the next sample. a settings change re-reads the
	// interval but must NOT restart the clock: restarting it pushes the next
	// sample out by a full interval, and `sampleSeriesWithLock` treats a late
	// sample as a gap — zero-holding and backfilling a zero point. changing the
	// chart's window while a transfer runs would punch a visible dropout into
	// the series
	var nextSampleTime time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		nextSampleTime = time.Now().Add(self.sampleInterval)
	}()

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
			// the sampling settings changed. re-read them, but keep the pending
			// sample deadline. a shorter interval takes effect from the next
			// sample on; nothing may delay the pending one
			continue
		case <-time.After(time.Until(nextSampleTime)):
		}

		self.sample()
		nextSampleTime = time.Now().Add(sampleInterval)
	}
}

// sample polls the device and appends one throughput point per series
func (self *ContractViewController) sample() {
	// the device is an external object. call it outside the state lock.
	packetStats := self.device.GetPacketStats()
	providerPacketStats := self.device.GetProviderPacketStats()
	sampleTime := time.Now()

	appended := false
	notify := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.sampleSeriesWithLock(self.clientSeries, packetStats, sampleTime, false) {
			appended = true
		}
		if self.sampleSeriesWithLock(self.providerSeries, providerPacketStats, sampleTime, true) {
			appended = true
		}
		if appended {
			// evaluate both so each series records its idle delivery
			clientNotify := throughputSeriesNotifyWithLock(self.clientSeries)
			providerNotify := throughputSeriesNotifyWithLock(self.providerSeries)
			notify = clientNotify || providerNotify
		}
	}()

	if notify {
		self.throughputChanged()
	}
}

// throughputSeriesNeedsNotification returns true while any retained point still
// carries traffic. The window contents keep changing as active points age out
// toward the trim edge -- and the window transport totals with them -- so
// clients need the ticks until the last active point has left the retained
// series. That idle tail is bounded by the window (plus the off-screen buffer).
// Once every retained point is a zero hold, later snapshots are timestamp-only
// and semantically redundant: time-based charts scroll the retained points
// using their own clock, and sampling continues so the first new non-zero
// point wakes clients immediately.
func throughputSeriesNeedsNotification(series *throughputSeries) bool {
	for i := len(series.points) - 1; 0 <= i; i-- {
		if throughputPointActive(series.points[i]) {
			return true
		}
	}
	return false
}

// must be called with `stateLock`.
// whether this tick's snapshot needs a notification: every tick while the
// series is active (see `throughputSeriesNeedsNotification`), plus exactly one
// idle snapshot after it goes quiet -- or after it starts, so a fresh series
// delivers its first (zero) points and clients resolve the series' presence
// (e.g. whether the device has a provider) without waiting for traffic
func throughputSeriesNotifyWithLock(series *throughputSeries) bool {
	if throughputSeriesNeedsNotification(series) {
		series.idleNotified = false
		return true
	}
	if !series.idleNotified {
		series.idleNotified = true
		return true
	}
	return false
}

func throughputPointActive(point *ThroughputPoint) bool {
	if point == nil {
		return false
	}
	for _, sample := range []*ThroughputSample{point.Remote, point.Local, point.Block} {
		if sample != nil &&
			(0 < sample.EgressByteCount ||
				0 < sample.IngressByteCount ||
				0 < sample.EgressPacketCount ||
				0 < sample.IngressPacketCount) {
			return true
		}
	}
	return false
}

// must be called with `stateLock`.
// appends the points for one series at one tick: zero-hold backfill for
// missed ticks, then a delta point (or a zero-hold when there are no
// stats or the delta spans a gap). returns whether any point was appended
func (self *ContractViewController) sampleSeriesWithLock(series *throughputSeries, packetStats *PacketStats, sampleTime time.Time, provider bool) bool {
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
				point := newThroughputPoint(
					sampleTime,
					elapsed,
					series.prevPacketStats,
					packetStats,
				)
				if provider {
					// the provider has no split-tunnel local route; present its
					// relay (remote) traffic on the local route the provider ui reads
					point = mirrorProviderThroughputPoint(point)
				}
				series.points = append(series.points, point)
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
		transportDeltas: transportThroughputDeltas(prev, current),
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

// transportThroughputDeltas computes the signed per-transport remote deltas
// between two cumulative stats, in `transportTypes` order. Unlike the route
// deltas these are not clamped: the unknown bucket legitimately decreases when
// an admitted packet's attribution moves to its physical carrier, and the
// matching increase lands on that carrier in the same sample, so the deltas
// only reconcile summed over the window. A reset of the aggregate accumulators
// (the case the route deltas clamp for) invalidates the per-transport deltas
// of that direction as well, so a reset can never leave a phantom negative
// total in the window. Missing carrier breakdowns (an older peer across the
// rpc, or stats without a breakdown) contribute nothing.
func transportThroughputDeltas(prev *PacketStats, current *PacketStats) []transportThroughputDelta {
	if prev == nil || current == nil || prev.TransportStats == nil || current.TransportStats == nil {
		return nil
	}
	prevByType := transportPacketStatsByType(prev)
	currentByType := transportPacketStatsByType(current)
	egressReset := current.RemoteEgressByteCount < prev.RemoteEgressByteCount ||
		current.RemoteEgressPacketCount < prev.RemoteEgressPacketCount
	ingressReset := current.RemoteIngressByteCount < prev.RemoteIngressByteCount ||
		current.RemoteIngressPacketCount < prev.RemoteIngressPacketCount
	types := transportTypes()
	deltas := make([]transportThroughputDelta, len(types))
	for i, transportType := range types {
		prevStats := prevByType[transportType]
		currentStats := currentByType[transportType]
		if prevStats == nil {
			prevStats = &PacketStats{}
		}
		if currentStats == nil {
			currentStats = &PacketStats{}
		}
		if !egressReset {
			deltas[i].egressByteCount = int64(currentStats.RemoteEgressByteCount - prevStats.RemoteEgressByteCount)
			deltas[i].egressPacketCount = currentStats.RemoteEgressPacketCount - prevStats.RemoteEgressPacketCount
		}
		if !ingressReset {
			deltas[i].ingressByteCount = int64(currentStats.RemoteIngressByteCount - prevStats.RemoteIngressByteCount)
			deltas[i].ingressPacketCount = currentStats.RemoteIngressPacketCount - prevStats.RemoteIngressPacketCount
		}
	}
	return deltas
}

// the carrier breakdown of a stats snapshot keyed by transport type.
// duplicate rows for one type (not produced by the device, but tolerated) sum
func transportPacketStatsByType(packetStats *PacketStats) map[TransportType]*PacketStats {
	byType := map[TransportType]*PacketStats{}
	if packetStats == nil || packetStats.TransportStats == nil {
		return byType
	}
	for _, transportStats := range packetStats.TransportStats.getAll() {
		if transportStats == nil || transportStats.Stats == nil {
			continue
		}
		stats := byType[transportStats.TransportType]
		if stats == nil {
			stats = &PacketStats{}
			byType[transportStats.TransportType] = stats
		}
		stats.RemoteEgressByteCount += transportStats.Stats.RemoteEgressByteCount
		stats.RemoteEgressPacketCount += transportStats.Stats.RemoteEgressPacketCount
		stats.RemoteIngressByteCount += transportStats.Stats.RemoteIngressByteCount
		stats.RemoteIngressPacketCount += transportStats.Stats.RemoteIngressPacketCount
	}
	return byType
}

// mirrorProviderThroughputPoint synthesizes the provider's local route from its
// remote relay route. the provider counters only fill remote and block (there is
// no split-tunnel local route when relaying), so the provider stats ui — which
// reads the local route — would otherwise be flat. the mirror swaps direction so
// the labels read naturally from the provider's vantage: remote ingress (a
// client's egress relayed out to the internet) becomes local egress, and remote
// egress (the return relayed back to the client) becomes local ingress. remote
// and block are passed through unchanged
func mirrorProviderThroughputPoint(point *ThroughputPoint) *ThroughputPoint {
	return &ThroughputPoint{
		Time:   point.Time,
		Remote: point.Remote,
		// the carrier breakdown is of the remote route, which passes through
		transportDeltas: point.transportDeltas,
		Local: &ThroughputSample{
			EgressByteCount:    point.Remote.IngressByteCount,
			IngressByteCount:   point.Remote.EgressByteCount,
			EgressPacketCount:  point.Remote.IngressPacketCount,
			IngressPacketCount: point.Remote.EgressPacketCount,
			EgressBitRate:      point.Remote.IngressBitRate,
			IngressBitRate:     point.Remote.EgressBitRate,
		},
		Block: point.Block,
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

// adapts the client transport settings change listener
type transportSettingsForwarder struct {
	vc *ContractViewController
}

func (self *transportSettingsForwarder) TransportSettingsChanged(transportSettings *TransportSettings) {
	self.vc.transportSettingsChanged(transportSettings, false)
}

// adapts the provider transport settings change listener
type providerTransportSettingsForwarder struct {
	vc *ContractViewController
}

func (self *providerTransportSettingsForwarder) ProviderTransportSettingsChanged(transportSettings *TransportSettings) {
	self.vc.transportSettingsChanged(transportSettings, true)
}

// caches the transport types a policy enables. a nil policy (a device with no
// policy at all) enables nothing
func (self *ContractViewController) transportSettingsChanged(transportSettings *TransportSettings, provider bool) {
	enabled := map[TransportType]bool{}
	if transportSettings != nil {
		for _, transportType := range transportSettings.enabledTransportTypes() {
			enabled[transportType] = true
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if provider {
		self.enabledProviderTransportTypes = enabled
	} else {
		self.enabledTransportTypes = enabled
	}
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

// returns the client remote traffic of the window partitioned by the transport
// type that carried it, ready to render (see `TransportShare`). The window is
// the same one the throughput points span, evaluated as of now -- so as active
// points age out of the window the distribution follows, and a window with no
// remote traffic is inactive with every share zero. Snapshot; safe to call at
// any time. The enabled flags follow the device's client transport settings
// through its change listener.
func (self *ContractViewController) GetTransportDistribution() *TransportDistribution {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transportDistributionWithLock(self.clientSeries, time.Now(), self.enabledTransportTypes)
}

// the provider counterpart of `GetTransportDistribution`: the relayed traffic
// by carrier, enabled flags from the provider transport settings. Inactive
// with every share zero when the device has no provider
func (self *ContractViewController) GetProviderTransportDistribution() *TransportDistribution {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transportDistributionWithLock(self.providerSeries, time.Now(), self.enabledProviderTransportTypes)
}

// must be called with `stateLock`.
// sums the signed per-transport deltas of the points inside the window ending
// at `now`, clamps each total at zero, then derives the render values. The
// clamp only ever trims the unknown bucket (an attribution admitted before the
// window start and moved to its carrier inside it); the physical carriers are
// monotonic and exact
func (self *ContractViewController) transportDistributionWithLock(
	series *throughputSeries,
	now time.Time,
	enabled map[TransportType]bool,
) *TransportDistribution {
	windowStartTime := now.Add(-self.windowDuration).UnixMilli()
	types := transportTypes()
	totals := make([]transportThroughputDelta, len(types))
	for _, point := range series.points {
		if point.Time < windowStartTime {
			continue
		}
		for i := 0; i < len(point.transportDeltas) && i < len(totals); i += 1 {
			totals[i].add(point.transportDeltas[i])
		}
	}
	shares := make([]*TransportShare, len(types))
	for i, transportType := range types {
		shares[i] = &TransportShare{
			TransportType:      transportType,
			EgressByteCount:    ByteCount(max(totals[i].egressByteCount, 0)),
			IngressByteCount:   ByteCount(max(totals[i].ingressByteCount, 0)),
			EgressPacketCount:  max(totals[i].egressPacketCount, 0),
			IngressPacketCount: max(totals[i].ingressPacketCount, 0),
			Enabled:            enabled[transportType],
		}
	}
	return newTransportDistribution(shares)
}

// newTransportDistribution derives the render values (share, boundary, percent,
// used, active) from the per-transport byte totals. The shares are the byte
// fraction of the window total; the boundaries are the running sum, so every
// segment can be drawn from its neighbours' boundaries and the segments tile
// exactly 100% at any moment; the percents are whole numbers by largest
// remainder over the used transports, ties broken by stable order, so they sum
// to exactly 100 and never label a used transport 0 unless its remainder lost.
func newTransportDistribution(shares []*TransportShare) *TransportDistribution {
	var total ByteCount
	for _, share := range shares {
		total += share.EgressByteCount + share.IngressByteCount
	}
	distribution := &TransportDistribution{
		Shares:    NewTransportShareList(),
		ByteCount: total,
		Active:    0 < total,
	}
	running := float64(0)
	for _, share := range shares {
		if 0 < total {
			bytes := share.EgressByteCount + share.IngressByteCount
			share.Share = float64(bytes) / float64(total)
			share.Used = 0 < bytes
			running += share.Share
			share.Boundary = min(running, 1)
		}
	}
	if 0 < total {
		// the last boundary is exactly 1 regardless of float rounding
		shares[len(shares)-1].Boundary = 1
		assignLargestRemainderPercents(shares)
	}
	distribution.Shares.addAll(shares...)
	return distribution
}

// assignLargestRemainderPercents sets whole percents on the used shares that
// sum to exactly 100: floor each, then hand the missing points to the largest
// remainders, ties by stable order
func assignLargestRemainderPercents(shares []*TransportShare) {
	type remainder struct {
		index int
		value float64
	}
	remainders := []remainder{}
	allocated := 0
	for i, share := range shares {
		if !share.Used {
			share.Percent = 0
			continue
		}
		exact := share.Share * 100
		floor := int(math.Floor(exact))
		share.Percent = floor
		allocated += floor
		remainders = append(remainders, remainder{index: i, value: exact - float64(floor)})
	}
	slices.SortStableFunc(remainders, func(a remainder, b remainder) int {
		// descending by remainder; stable keeps the stable order among equals
		return cmp.Compare(b.value, a.value)
	})
	missing := 100 - allocated
	for _, r := range remainders {
		if missing <= 0 {
			break
		}
		shares[r.index].Percent += 1
		missing -= 1
	}
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
	self.transportSettingsChangedSub.Close()
	self.providerTransportSettingsChangedSub.Close()
}
