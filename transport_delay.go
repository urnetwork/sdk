package sdk

// transport_delay.go -- the transport quality of the device's connection
// (connect/DELAY.md §10, as calibrated by the transport delay quality
// thresholds report): the TransportDelay apps read, its listener, and the
// window that turns the delay stats of the window transports into it.
//
// Every transport of the multi client's window clients measures its connection
// and publishes one connect.TransportDelayStats per active interval (DELAY.md
// §8, §9). Those connections share one access link, so the window reads the
// link one second at a time. It buckets the live samples by the second their
// interval ended, counts each connection once per second, and reads each
// direction of each second as:
//   - throughput: the network-limited rates of the connections the link held
//     back, plus the byte rates of the other active connections. A second
//     with no limited connection has no throughput reading. The sum reads the
//     link rather than one connection's share of it, and absorbs one stalled
//     server edge or slow peer.
//   - standing queue: the second-highest reading when two or more
//     connections report one, else the one reading. A queue on the user's
//     link is common to every connection crossing it; a server stall hits
//     one.
//   - fallback: DNS-carried transports carried most of the second's bytes
//     (D10).
//
// Each direction scores each signal on the second-worst of its newest N
// sampled seconds no older than M, so one stray second does not set a level
// and two do; fewer than two sampled seconds score nothing. The direction's
// quality is the worse of the two scores (D1), from 0 (normal operation) to 1
// (worst), quantized to a step (§10.3). A worse level is reported at once, and
// a better one only once the quality is the hysteresis below the level's band,
// so an app hears a change only when a level moves (D7). A direction with no
// live sample is 0 (D4).
//
// The window takes the samples of one multi client generation at a time. A
// rebuild drops the retired generation's samples and its levels, and a sample
// a retired multi client's transports publish late is dropped.
//
// The window is plain state behind its own lock and reads no clock of its own,
// so its tests construct the times (transport_delay_test.go). DeviceLocal feeds
// it from the transport goroutines and evaluates it on its own worker
// (device_local_transport_delay.go).

import (
	"cmp"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// The defaults of the DeviceLocalSettings transport delay fields, from the
// thresholds report. They are starting points until the shadow trial sets
// them (DELAY.md §13).
const (
	defaultTransportDelayWindowCount           = 10
	defaultTransportDelayWindowTimeout         = 20 * time.Second
	defaultTransportDelayGoodUpBitsPerSecond   = 750_000
	defaultTransportDelayBadUpBitsPerSecond    = 175_000
	defaultTransportDelayGoodDownBitsPerSecond = 1_500_000
	defaultTransportDelayBadDownBitsPerSecond  = 350_000
	defaultTransportDelayGoodUpQueueDelay      = 500 * time.Millisecond
	defaultTransportDelayGoodDownQueueDelay    = 500 * time.Millisecond
	defaultTransportDelayBadUpQueueDelay       = 2500 * time.Millisecond
	defaultTransportDelayBadDownQueueDelay     = 2500 * time.Millisecond
	defaultTransportDelayQualityStep           = 0.25
	defaultTransportDelayQualityHysteresis     = 0.10
)

const (
	// the re-evaluation while any sample is live, so a direction returns to 0
	// without new traffic (D4)
	transportDelayEvaluateInterval = 1 * time.Second
	// the window holds every sample of the last M, about one per connection
	// per second; a transport that redials over and over adds a connection
	// per dial, so past this many the oldest are dropped
	transportDelayWindowMaxSampleCount = 2048
)

// TransportDelay is the device's current transport quality. Plain float64,
// int64 and bool fields so gomobile, cgo (JSON with Go field names as keys)
// and gob carry it as is.
type TransportDelay struct {
	// 0 (normal) to 1 (worst), quantized to TransportDelayQualityStep
	UpQuality   float64
	DownQuality float64
	// the scored throughput of the link: the second-lowest per-second
	// throughput in the window. 0: none
	UpBitsPerSecond   int64
	DownBitsPerSecond int64
	// the scored standing queue: the second-highest per-second queue in the
	// window. 0: none
	UpQueueDelayMillis   int64
	DownQueueDelayMillis int64
	// the second that set the reported level was carried mostly by a
	// DNS-carried fallback transport: the apps say URnetwork is using a
	// slower fallback connection, not that the user's internet is slow (D10)
	Fallback bool
}

// TransportDelayChangeListener receives each change of either direction's
// quality level and of Fallback (see Device.GetTransportDelay). The throughput
// and standing queue ride along, but a change of only those does not fire it.
type TransportDelayChangeListener interface {
	TransportDelayChanged(delay *TransportDelay)
}

// noneTransportDelay is normal operation, which is also the readout of a
// device with no multi client to measure.
func noneTransportDelay() *TransportDelay {
	return &TransportDelay{}
}

func cloneTransportDelay(delay *TransportDelay) *TransportDelay {
	if delay == nil {
		return nil
	}
	copied := *delay
	return &copied
}

// Reports a change the listeners hear: a level of either direction, or
// Fallback.
func transportDelayLevelChanged(a *TransportDelay, b *TransportDelay) bool {
	return a.UpQuality != b.UpQuality ||
		a.DownQuality != b.DownQuality ||
		a.Fallback != b.Fallback
}

// q_throughput (DELAY.md §10.2): 0 without a reading and at or above good, 1
// at or below bad, and between them on a ratio scale.
func transportDelayThroughputQuality(bitsPerSecond int64, goodBitsPerSecond int64, badBitsPerSecond int64) float64 {
	if bitsPerSecond <= 0 || goodBitsPerSecond <= bitsPerSecond {
		return 0
	}
	if bitsPerSecond <= badBitsPerSecond {
		return 1
	}
	// bad < bitsPerSecond < good; a bad of 0 or less reads as 1 bit/s, the
	// lowest rate a sample reports
	badBitsPerSecond = max(badBitsPerSecond, 1)
	return math.Log(float64(goodBitsPerSecond)/float64(bitsPerSecond)) /
		math.Log(float64(goodBitsPerSecond)/float64(badBitsPerSecond))
}

// q_delay (DELAY.md §10.2): 0 at or below good, 1 at or above bad, and linear
// between them.
func transportDelayQueueQuality(queueDelay time.Duration, goodQueueDelay time.Duration, badQueueDelay time.Duration) float64 {
	if queueDelay <= goodQueueDelay {
		return 0
	}
	if badQueueDelay <= queueDelay {
		return 1
	}
	return float64(queueDelay-goodQueueDelay) / float64(badQueueDelay-goodQueueDelay)
}

// Quantizes a direction's quality from its current level (DELAY.md §10.3).
// The level is 0 exactly when the quality is 0, and any other quality rounds
// up to ceil(quality / step) · step, at most 1. A worse level is taken at
// once, at every level. A better one waits for the quality to fall the
// hysteresis below the current level's band (level − step, level], so a path
// to a level does not move its boundary; returning to 0 needs a quality of 0.
// step is in (0, 1].
func transportDelayLevel(quality float64, level float64, step float64, hysteresis float64) float64 {
	if quality <= 0 {
		return 0
	}
	// the epsilon keeps float noise in a quality on a step boundary, such as
	// 0.30000000000000004 for a step of 0.1, from rounding up a whole step
	const epsilon = 1e-9
	quantized := min(max(math.Ceil(quality/step-epsilon), 1)*step, 1)
	if level <= quantized {
		return quantized
	}
	// the band's lower edge is the level below, also for a top level clamped
	// to 1 by a step that does not divide 1
	lowerLevel := (math.Ceil(level/step-epsilon) - 1) * step
	if quality <= lowerLevel-hysteresis {
		return quantized
	}
	return level
}

// The thresholds of one direction (DELAY.md §10.2).
type transportDelayThresholds struct {
	goodBitsPerSecond int64
	badBitsPerSecond  int64
	goodQueueDelay    time.Duration
	badQueueDelay     time.Duration
}

// One direction of one sample.
type transportDelayDirectionSample struct {
	bitsPerSecond   int64
	byteCount       ByteCount
	queueDelay      time.Duration
	queueDelayValid bool
}

// The part of one connect.TransportDelayStats the window reads. Up is what the
// device sends, down what it receives.
type transportDelaySample struct {
	connectionId connect.Id
	endTime      time.Time
	// what the byte counts were counted over: the interval, which is at least
	// the measurement interval long
	duration time.Duration
	// from a DNS-carried transport (D10)
	fallback bool
	up       transportDelayDirectionSample
	down     transportDelayDirectionSample
}

// One direction of one second of the link, over the connections active in it.
type transportDelaySecond struct {
	// a connection was network-limited, and the throughput then read
	limited       bool
	bitsPerSecond int64
	// a connection reported a standing queue, and the queue then read
	queueDelayValid bool
	queueDelay      time.Duration
	// DNS-carried transports carried most of the second's bytes
	fallback bool
}

// Sums one direction of one second over its connections.
type transportDelaySecondAccumulator struct {
	limited              bool
	limitedBitsPerSecond int64
	otherBitsPerSecond   int64

	queueDelayCount         int
	highestQueueDelay       time.Duration
	secondHighestQueueDelay time.Duration

	byteCount         ByteCount
	fallbackByteCount ByteCount
}

func (self *transportDelaySecondAccumulator) add(sample transportDelayDirectionSample, duration time.Duration, fallback bool) {
	if 0 < sample.bitsPerSecond {
		self.limited = true
		self.limitedBitsPerSecond += sample.bitsPerSecond
	} else if 0 < sample.byteCount {
		self.otherBitsPerSecond += int64(float64(8*sample.byteCount) / duration.Seconds())
	}
	if sample.queueDelayValid {
		self.queueDelayCount += 1
		if self.highestQueueDelay < sample.queueDelay {
			self.secondHighestQueueDelay = self.highestQueueDelay
			self.highestQueueDelay = sample.queueDelay
		} else if self.secondHighestQueueDelay < sample.queueDelay {
			self.secondHighestQueueDelay = sample.queueDelay
		}
	}
	self.byteCount += sample.byteCount
	if fallback {
		self.fallbackByteCount += sample.byteCount
	}
}

func (self *transportDelaySecondAccumulator) reading() transportDelaySecond {
	second := transportDelaySecond{
		limited:  self.limited,
		fallback: 0 < self.byteCount && self.byteCount < 2*self.fallbackByteCount,
	}
	if self.limited {
		second.bitsPerSecond = self.limitedBitsPerSecond + self.otherBitsPerSecond
	}
	switch {
	case 2 <= self.queueDelayCount:
		second.queueDelayValid = true
		second.queueDelay = self.secondHighestQueueDelay
	case self.queueDelayCount == 1:
		second.queueDelayValid = true
		second.queueDelay = self.highestQueueDelay
	}
	return second
}

// One direction's scores and the quality they set.
type transportDelayScore struct {
	// the scored throughput and standing queue, 0 with no score
	bitsPerSecond int64
	queueDelay    time.Duration
	// the worse of the two scores' qualities (D1), and whether a second the
	// fallback carried set it. false while the quality is 0
	quality  float64
	fallback bool
}

// transportDelayWindow is the window of delay samples, its per-second
// evaluation and the levels it last reported. Safe for concurrent use: the
// transport goroutines add samples while the device's worker evaluates.
type transportDelayWindow struct {
	windowCount       int
	windowTimeout     time.Duration
	interval          time.Duration
	up                transportDelayThresholds
	down              transportDelayThresholds
	qualityStep       float64
	qualityHysteresis float64

	stateLock sync.Mutex
	// the multi client generation the window takes samples from: the device's
	// connection generation and its multi client. With no multi client the
	// window takes nothing
	generation int64
	multi      *connect.RemoteUserNatMultiClient
	// every live sample, in the order they arrived
	samples []transportDelaySample
	// the last evaluation, whose qualities are the levels the next one moves
	// from
	delay TransportDelay

	// reused by each evaluation
	sortedSamples []transportDelaySample
	upSeconds     []transportDelaySecond
	downSeconds   []transportDelaySecond
	scoreSeconds  []transportDelaySecond
}

// Builds the window from the device settings. A window count, timeout or step
// of zero or less takes its default, a step above 1 is 1 and a negative
// hysteresis is 0, so a settings json that zeroes a field (the c abi decodes
// one over the defaults) cannot empty the window or leave the quality without
// levels; EnableTransportDelay is the switch. The thresholds are taken as set.
func newTransportDelayWindow(settings *DeviceLocalSettings) *transportDelayWindow {
	window := &transportDelayWindow{
		windowCount:   settings.TransportDelayWindowCount,
		windowTimeout: settings.TransportDelayWindowTimeout,
		// the window transports measure on connect's default interval
		// (DeviceLocal.applyDestination)
		interval: connect.DefaultTransportDelaySettings().Interval,
		up: transportDelayThresholds{
			goodBitsPerSecond: settings.TransportDelayGoodUpBitsPerSecond,
			badBitsPerSecond:  settings.TransportDelayBadUpBitsPerSecond,
			goodQueueDelay:    settings.TransportDelayGoodUpQueueDelay,
			badQueueDelay:     settings.TransportDelayBadUpQueueDelay,
		},
		down: transportDelayThresholds{
			goodBitsPerSecond: settings.TransportDelayGoodDownBitsPerSecond,
			badBitsPerSecond:  settings.TransportDelayBadDownBitsPerSecond,
			goodQueueDelay:    settings.TransportDelayGoodDownQueueDelay,
			badQueueDelay:     settings.TransportDelayBadDownQueueDelay,
		},
		qualityStep:       settings.TransportDelayQualityStep,
		qualityHysteresis: max(settings.TransportDelayQualityHysteresis, 0),
	}
	if window.windowCount <= 0 {
		window.windowCount = defaultTransportDelayWindowCount
	}
	if window.windowTimeout <= 0 {
		window.windowTimeout = defaultTransportDelayWindowTimeout
	}
	if window.qualityStep <= 0 {
		window.qualityStep = defaultTransportDelayQualityStep
	}
	window.qualityStep = min(window.qualityStep, 1)
	return window
}

// Starts the window over for a multi client generation: every sample and both
// levels are dropped, so the next evaluation starts from normal operation.
// multi nil takes no sample until the next reset.
func (self *transportDelayWindow) reset(generation int64, multi *connect.RemoteUserNatMultiClient) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.generation = generation
	self.multi = multi
	self.delay = TransportDelay{}
	// the buffers grow with the connections of a generation; let them go
	self.samples = nil
	self.sortedSamples = nil
	self.upSeconds = nil
	self.downSeconds = nil
	self.scoreSeconds = nil
}

// Takes one sample of a generation and reports whether the window kept it. A
// sample of any other generation is dropped: a transport of a retired multi
// client can publish after the rebuild that retired it. So is a sample that
// is already too old to count.
func (self *transportDelayWindow) add(
	generation int64,
	multi *connect.RemoteUserNatMultiClient,
	stats *connect.TransportDelayStats,
	now time.Time,
) bool {
	if stats == nil {
		return false
	}
	sample := transportDelaySample{
		connectionId: stats.ConnectionId,
		endTime:      stats.EndTime,
		duration:     max(stats.EndTime.Sub(stats.StartTime), self.interval),
		fallback:     stats.TransportType == connect.TransportTypeH3Dns || stats.TransportType == connect.TransportTypeH3DnsPump,
		up: transportDelayDirectionSample{
			bitsPerSecond:   stats.SendBitsPerSecond,
			byteCount:       stats.SendByteCount,
			queueDelay:      stats.SendQueueDelay,
			queueDelayValid: stats.SendQueueDelayValid,
		},
		down: transportDelayDirectionSample{
			bitsPerSecond:   stats.ReceiveBitsPerSecond,
			byteCount:       stats.ReceiveByteCount,
			queueDelay:      stats.ReceiveQueueDelay,
			queueDelayValid: stats.ReceiveQueueDelayValid,
		},
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if multi == nil || multi != self.multi || generation != self.generation {
		return false
	}
	if !self.live(sample, now) {
		return false
	}
	self.samples = append(self.samples, sample)
	if excess := len(self.samples) - transportDelayWindowMaxSampleCount; 0 < excess {
		self.samples = slices.Delete(self.samples, 0, excess)
	}
	return true
}

// Ages out the samples older than the timeout, reads the rest as seconds,
// scores each direction and moves the levels. Returns the readout and whether
// any sample is still live, which keeps the caller evaluating once a second so
// a direction returns to 0 without new traffic (D4).
func (self *transportDelayWindow) evaluate(now time.Time) (*TransportDelay, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.samples = slices.DeleteFunc(self.samples, func(sample transportDelaySample) bool {
		return !self.live(sample, now)
	})
	self.secondsWithLock()
	up := self.scoreWithLock(self.upSeconds, self.up)
	down := self.scoreWithLock(self.downSeconds, self.down)
	upLevel := transportDelayLevel(up.quality, self.delay.UpQuality, self.qualityStep, self.qualityHysteresis)
	downLevel := transportDelayLevel(down.quality, self.delay.DownQuality, self.qualityStep, self.qualityHysteresis)
	// the second that set the reported level says whose slowness it is
	// (D10): the worse direction's, and between equal levels the worse
	// quality's. Directions that read equally badly are the user's network
	// unless the fallback sets both
	fallback := false
	switch {
	case upLevel == 0 && downLevel == 0:
	case downLevel < upLevel:
		fallback = up.fallback
	case upLevel < downLevel:
		fallback = down.fallback
	case down.quality < up.quality:
		fallback = up.fallback
	case up.quality < down.quality:
		fallback = down.fallback
	default:
		fallback = up.fallback && down.fallback
	}
	self.delay = TransportDelay{
		UpQuality:            upLevel,
		DownQuality:          downLevel,
		UpBitsPerSecond:      up.bitsPerSecond,
		DownBitsPerSecond:    down.bitsPerSecond,
		UpQueueDelayMillis:   up.queueDelay.Milliseconds(),
		DownQueueDelayMillis: down.queueDelay.Milliseconds(),
		Fallback:             fallback,
	}
	delay := self.delay
	return &delay, 0 < len(self.samples)
}

// The last evaluation, normal operation before the first and after a reset.
func (self *transportDelayWindow) get() *TransportDelay {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delay := self.delay
	return &delay
}

// Reads the live samples into upSeconds and downSeconds, newest second first.
// A connection counts once in a second: an interval that closes early can put
// two of a connection's samples in one second, and the later one stands for
// it. Must be called with stateLock.
func (self *transportDelayWindow) secondsWithLock() {
	self.sortedSamples = append(self.sortedSamples[:0], self.samples...)
	slices.SortFunc(self.sortedSamples, func(a transportDelaySample, b transportDelaySample) int {
		return cmp.Or(
			cmp.Compare(b.endTime.Unix(), a.endTime.Unix()),
			a.connectionId.Cmp(b.connectionId),
			b.endTime.Compare(a.endTime),
		)
	})
	self.upSeconds = self.upSeconds[:0]
	self.downSeconds = self.downSeconds[:0]
	for start := 0; start < len(self.sortedSamples); {
		unixSecond := self.sortedSamples[start].endTime.Unix()
		var up transportDelaySecondAccumulator
		var down transportDelaySecondAccumulator
		end := start
		for ; end < len(self.sortedSamples) && self.sortedSamples[end].endTime.Unix() == unixSecond; end += 1 {
			sample := &self.sortedSamples[end]
			if start < end && self.sortedSamples[end-1].connectionId == sample.connectionId {
				continue
			}
			up.add(sample.up, sample.duration, sample.fallback)
			down.add(sample.down, sample.duration, sample.fallback)
		}
		self.upSeconds = append(self.upSeconds, up.reading())
		self.downSeconds = append(self.downSeconds, down.reading())
		start = end
	}
}

// Scores one direction's seconds, newest first, on each signal's second-worst
// sampled second. Must be called with stateLock.
func (self *transportDelayWindow) scoreWithLock(seconds []transportDelaySecond, thresholds transportDelayThresholds) transportDelayScore {
	var score transportDelayScore
	throughputQuality := 0.0
	throughputFallback := false
	if second, ok := self.secondWorstWithLock(
		seconds,
		func(second transportDelaySecond) bool {
			return second.limited
		},
		func(a transportDelaySecond, b transportDelaySecond) int {
			return cmp.Compare(a.bitsPerSecond, b.bitsPerSecond)
		},
	); ok {
		score.bitsPerSecond = second.bitsPerSecond
		throughputQuality = transportDelayThroughputQuality(second.bitsPerSecond, thresholds.goodBitsPerSecond, thresholds.badBitsPerSecond)
		throughputFallback = second.fallback
	}
	queueQuality := 0.0
	queueFallback := false
	if second, ok := self.secondWorstWithLock(
		seconds,
		func(second transportDelaySecond) bool {
			return second.queueDelayValid
		},
		func(a transportDelaySecond, b transportDelaySecond) int {
			return cmp.Compare(b.queueDelay, a.queueDelay)
		},
	); ok {
		score.queueDelay = second.queueDelay
		queueQuality = transportDelayQueueQuality(second.queueDelay, thresholds.goodQueueDelay, thresholds.badQueueDelay)
		queueFallback = second.fallback
	}
	switch {
	case queueQuality < throughputQuality:
		score.quality = throughputQuality
		score.fallback = throughputFallback
	case throughputQuality < queueQuality:
		score.quality = queueQuality
		score.fallback = queueFallback
	default:
		score.quality = throughputQuality
		score.fallback = 0 < throughputQuality && throughputFallback && queueFallback
	}
	return score
}

// The second-worst of the newest windowCount seconds that have a reading, in
// the order worse sorts them, worst first. It is the fallback's only if every
// second with its reading is: a second of the usual transports that reads as
// badly is the user's network. ok false with fewer than two such seconds.
// Must be called with stateLock.
func (self *transportDelayWindow) secondWorstWithLock(
	seconds []transportDelaySecond,
	has func(second transportDelaySecond) bool,
	worse func(a transportDelaySecond, b transportDelaySecond) int,
) (transportDelaySecond, bool) {
	self.scoreSeconds = self.scoreSeconds[:0]
	for _, second := range seconds {
		if len(self.scoreSeconds) == self.windowCount {
			break
		}
		if has(second) {
			self.scoreSeconds = append(self.scoreSeconds, second)
		}
	}
	if len(self.scoreSeconds) < 2 {
		return transportDelaySecond{}, false
	}
	slices.SortFunc(self.scoreSeconds, worse)
	scored := self.scoreSeconds[1]
	for _, second := range self.scoreSeconds {
		if worse(second, scored) == 0 && !second.fallback {
			scored.fallback = false
			break
		}
	}
	return scored, true
}

// A sample is live while it is younger than the timeout on both the monotonic
// and the wall clock. Go's monotonic clock stops while a phone sleeps (DELAY.md
// §5.2), so a sample from before a sleep would read as fresh for most of the
// timeout after the phone wakes; the wall clock counts the sleep, and the
// monotonic clock still ages a sample when the wall clock is set back.
func (self *transportDelayWindow) live(sample transportDelaySample, now time.Time) bool {
	return now.Sub(sample.endTime) < self.windowTimeout &&
		now.Round(0).Sub(sample.endTime.Round(0)) < self.windowTimeout
}
