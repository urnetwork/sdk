package sdk

// transport_connect.go -- how promptly the window transports connect, the
// connect half of the transport quality (transport_delay.go).
//
// The delay window reads only connections that are up: a transport still
// dialing publishes no delay stats, and a direction with no live sample reads
// 0 (connect/DELAY.md D4). So a device whose transports took a long time to
// connect, as on a weak cell signal, read normal operation the whole time
// (field report, 2026-11-07). This window reads the window transports'
// connect state instead.
//
// It holds the platform transports of one multi client generation, which the
// generator reports as it builds each one: the first transport of a window
// client, and every migration's and move's replacement
// (ApiMultiClientGeneratorSettings.PlatformTransportCreated). A transport is
//   - trying while it dials or waits out its own backoff
//     (PlatformTransportStateConnecting, less a wait for memory budget);
//   - connected while a connection of it has routes registered;
//   - otherwise held: its owner disabled it, its pinned family is away, or a
//     client limit holds its dials. A hold says nothing about the network.
//
// The window stalls while at least one transport is trying and no more are
// connected than trying: nothing connected yet, every connection lost, or
// more of the window dialing than carrying. One transport stuck beside a
// working window, such as on a broken edge, is no stall. A stall reads the
// longest stretch in it without a transport connecting, from its start or
// from the last transport that connected in it. Counting from the last
// connect keeps connect's own dial pacing out of the reading: it spreads a
// burst of new dials over up to 10 s (ClientStrategy.NextConnectTime) and a
// lost connection's re-dial over ReconnectJitter, but on a working network
// the dials it lets through keep connecting, one at least every pacing step.
//
// The reading scores like a standing queue: 0 at or below the good stall, 1 at
// or above the bad stall, linear between. The transport delay window folds it
// into both directions, where the level's quantization and hysteresis apply,
// so an ongoing stall deepens the level as it grows. An ended stall longer
// than the good stall stays evidence for the window timeout after it ends, as
// a delay sample does, so connections that keep dropping and slowly
// reconnecting do not flap the level. A transport that starts an attempt
// after a stall ended and connects within the prompt connect timeout clears
// that stall sooner: connects are prompt again. A stall is the fallback's
// when at least one transport is connected and DNS carries every connected
// one (D10).
//
// Safe for concurrent use. The state is behind the window's own lock, under
// which only the transports' own state is read, so neither the generator's
// setup path nor a transport watcher ever waits on the device. The readings
// take the clock as an argument, so the tests construct the times
// (transport_connect_test.go); each transport's watcher stamps time.Now.

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// The defaults of the DeviceLocalSettings connect fields, from the window
// transports' own connect timing (connect.DefaultPlatformTransportSettings,
// pinned by TestTransportConnectThresholdsFollowConnectDefaults).
const (
	// ReconnectTimeout: the span connect gives one dial attempt before it
	// paces the next. By then Auto has started H1, H3 and DNS, which start
	// ModeInitialDelay apart.
	defaultTransportDelayGoodConnectStall = 5 * time.Second
	// HttpConnectTimeout, WsHandshakeTimeout, QuicConnectTimeout and
	// QuicHandshakeTimeout: a stall this long outlasted a whole dial timeout.
	defaultTransportDelayBadConnectStall = 15 * time.Second
	// ModeInitialDelay: Auto's head start for its preferred carrier, inside
	// which a healthy connect lands.
	defaultTransportDelayPromptConnectTimeout = 2 * time.Second
)

// The view of a window platform transport the window reads.
// *connect.PlatformTransport is the one production implementation.
type transportConnectCarrier interface {
	State() connect.PlatformTransportState
	IsWaitingForBudget() bool
	// the elected carrier mode, TransportModeNone while nothing is connected
	ActiveMode() connect.TransportMode
	// closes on the next connect change; take it before reading State
	ConnectedNotify() <-chan struct{}
	// closes once the transport has closed
	Done() <-chan struct{}
}

// What the window holds of one transport.
type transportConnectCarrierState struct {
	// connected when last observed
	connected bool
	// when its current attempt began: when the generator built it, or when it
	// lost its last connection
	attemptStartTime time.Time
}

// One ended stall that is still evidence.
type transportConnectStall struct {
	// the longest stretch in the stall without a transport connecting
	longest  time.Duration
	endTime  time.Time
	fallback bool
}

// One reading of the window, which the transport delay window folds into
// both directions.
type transportConnectScore struct {
	// the stall reading that set the quality, 0 with none
	stall time.Duration
	// 0 (normal operation) to 1 (worst), unquantized
	quality float64
	// the stall that set the quality was the fallback's. false while the
	// quality is 0
	fallback bool
}

// transportConnectWindow is the connect state of one multi client generation's
// window transports and the stalls it read in them.
type transportConnectWindow struct {
	ctx                  context.Context
	goodStall            time.Duration
	badStall             time.Duration
	promptConnectTimeout time.Duration
	// how long an ended stall stays evidence: the delay window's timeout
	timeout time.Duration
	// wakes the reader (DeviceLocal.notifyTransportDelay). Called outside the
	// lock, and must not block
	changed func()

	stateLock sync.Mutex
	closed    bool
	// the generation the window takes transports from, and the context its
	// watchers run under, canceled by the next reset
	generation       int64
	generationCtx    context.Context
	generationCancel context.CancelFunc
	carrierStates    map[transportConnectCarrier]*transportConnectCarrierState
	// the current stall: whether there is one, the later of its start and the
	// last connect in it, and its longest stretch before that
	stalled           bool
	stallProgressTime time.Time
	stallLongest      time.Duration
	// the ended stalls longer than the good stall, oldest first
	endedStalls []transportConnectStall

	// the transport watchers, joined by wait
	watchers sync.WaitGroup
}

// Builds the window from the device settings. The stall thresholds and the
// prompt connect timeout are taken as set: a prompt timeout of zero or less
// clears no stall early. The evidence lasts the delay window's timeout, whose
// zero or less takes its default.
func newTransportConnectWindow(ctx context.Context, settings *DeviceLocalSettings, changed func()) *transportConnectWindow {
	timeout := settings.TransportDelayWindowTimeout
	if timeout <= 0 {
		timeout = defaultTransportDelayWindowTimeout
	}
	if changed == nil {
		changed = func() {}
	}
	return &transportConnectWindow{
		ctx:                  ctx,
		goodStall:            settings.TransportDelayGoodConnectStall,
		badStall:             settings.TransportDelayBadConnectStall,
		promptConnectTimeout: settings.TransportDelayPromptConnectTimeout,
		timeout:              timeout,
		changed:              changed,
		carrierStates:        map[transportConnectCarrier]*transportConnectCarrierState{},
	}
}

// q_connect: 0 with no stall and at or below good, 1 at or above bad, and
// linear between them, as a standing queue scores.
func transportConnectStallQuality(stall time.Duration, goodStall time.Duration, badStall time.Duration) float64 {
	if stall <= 0 {
		return 0
	}
	return transportDelayQueueQuality(stall, goodStall, badStall)
}

// Starts the window over for a generation: every transport, the stall and the
// evidence are dropped, and the retired generation's watchers end.
func (self *transportConnectWindow) reset(generation int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.generationCancel != nil {
		self.generationCancel()
	}
	self.generation = generation
	self.generationCtx, self.generationCancel = context.WithCancel(self.ctx)
	self.carrierStates = map[transportConnectCarrier]*transportConnectCarrierState{}
	self.stalled = false
	self.stallLongest = 0
	self.endedStalls = nil
}

// Ends the window with the device: no transport is taken from here on and
// every watcher ends. wait joins them.
func (self *transportConnectWindow) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	if self.generationCancel != nil {
		self.generationCancel()
	}
	self.carrierStates = map[transportConnectCarrier]*transportConnectCarrierState{}
	self.stalled = false
	self.endedStalls = nil
}

// Joins every watcher. Call after close.
func (self *transportConnectWindow) wait() {
	self.watchers.Wait()
}

// Takes a transport the generator just built for the generation and watches
// it until it closes or the generation retires. A transport of any other
// generation is dropped: a retired generator can still be building. Called on
// the generator's setup path, so it only takes the window's lock and starts
// the watcher.
func (self *transportConnectWindow) track(generation int64, carrier transportConnectCarrier) {
	var watchCtx context.Context
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || !self.addWithLock(generation, carrier, time.Now()) {
			return
		}
		watchCtx = self.generationCtx
		// under the lock with closed false, so the add cannot race wait
		self.watchers.Add(1)
	}()
	if watchCtx == nil {
		return
	}
	go connect.HandleError(func() {
		defer self.watchers.Done()
		self.watchCarrier(watchCtx, generation, carrier)
	})
	self.changed()
}

// Observes the transport on each connect change until it closes or the
// generation retires. Each round subscribes before it reads, so a change after
// the read wakes the next round, and every wake is a change of the
// transport's registered connections, so the loop never spins.
func (self *transportConnectWindow) watchCarrier(ctx context.Context, generation int64, carrier transportConnectCarrier) {
	for {
		connectedNotify := carrier.ConnectedNotify()
		if self.observe(generation, carrier, time.Now()) {
			self.changed()
		}
		select {
		case <-ctx.Done():
			return
		case <-carrier.Done():
			if self.remove(generation, carrier, time.Now()) {
				self.changed()
			}
			return
		case <-connectedNotify:
		}
	}
}

// Takes a transport of the generation at now, without a watcher, and reports
// whether the window kept it.
func (self *transportConnectWindow) add(generation int64, carrier transportConnectCarrier, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.closed && self.addWithLock(generation, carrier, now)
}

// Its first attempt starts now, and a transport that already connected
// connected now. Must be called with stateLock.
func (self *transportConnectWindow) addWithLock(generation int64, carrier transportConnectCarrier, now time.Time) bool {
	if carrier == nil || generation != self.generation || self.generationCtx == nil {
		return false
	}
	if _, ok := self.carrierStates[carrier]; ok {
		return false
	}
	carrierState := &transportConnectCarrierState{
		attemptStartTime: now,
	}
	self.carrierStates[carrier] = carrierState
	self.observeWithLock(carrier, carrierState, now)
	return true
}

// Reads the transport's connect state at now and reports whether the window
// holds it.
func (self *transportConnectWindow) observe(generation int64, carrier transportConnectCarrier, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if generation != self.generation {
		return false
	}
	carrierState, ok := self.carrierStates[carrier]
	if !ok {
		return false
	}
	self.observeWithLock(carrier, carrierState, now)
	return true
}

// A connect is progress of the current stall, and when its attempt started
// after an ended stall and took no longer than the prompt connect timeout,
// that stall is no longer evidence. A lost connection starts the transport's
// next attempt. Must be called with stateLock.
func (self *transportConnectWindow) observeWithLock(
	carrier transportConnectCarrier,
	carrierState *transportConnectCarrierState,
	now time.Time,
) {
	connected := carrier.State() == connect.PlatformTransportStateConnected
	if connected != carrierState.connected {
		carrierState.connected = connected
		if connected {
			if self.stalled {
				self.stallLongest = max(self.stallLongest, transportConnectElapsed(self.stallProgressTime, now))
				self.stallProgressTime = now
			}
			if transportConnectElapsed(carrierState.attemptStartTime, now) <= self.promptConnectTimeout {
				attemptStartTime := carrierState.attemptStartTime
				self.endedStalls = slices.DeleteFunc(self.endedStalls, func(stall transportConnectStall) bool {
					return !stall.endTime.After(attemptStartTime)
				})
			}
		} else {
			carrierState.attemptStartTime = now
		}
	}
	self.updateWithLock(now)
}

// Drops a transport that closed and reports whether the window held it.
func (self *transportConnectWindow) remove(generation int64, carrier transportConnectCarrier, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if generation != self.generation {
		return false
	}
	if _, ok := self.carrierStates[carrier]; !ok {
		return false
	}
	delete(self.carrierStates, carrier)
	self.updateWithLock(now)
	return true
}

// Ages out the ended stalls older than the timeout and scores the window: the
// longest stall reading among the current stall, as it stands at now, and the
// live ended ones. Returns whether the caller must evaluate again while time
// passes: during a stall the reading grows, and ended stalls age out.
func (self *transportConnectWindow) evaluate(now time.Time) (transportConnectScore, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.updateWithLock(now)
	self.endedStalls = slices.DeleteFunc(self.endedStalls, func(stall transportConnectStall) bool {
		return !self.liveWithLock(stall.endTime, now)
	})
	var score transportConnectScore
	take := func(stall time.Duration, fallback bool) {
		switch {
		case score.stall < stall:
			score.stall = stall
			score.fallback = fallback
		case 0 < stall && score.stall == stall:
			// a reading the usual transports share is the user's network
			score.fallback = score.fallback && fallback
		}
	}
	if self.stalled {
		take(
			max(self.stallLongest, transportConnectElapsed(self.stallProgressTime, now)),
			self.fallbackWithLock(),
		)
	}
	for _, stall := range self.endedStalls {
		take(stall.longest, stall.fallback)
	}
	score.quality = transportConnectStallQuality(score.stall, self.goodStall, self.badStall)
	if score.quality <= 0 {
		score.fallback = false
	}
	return score, self.stalled || 0 < len(self.endedStalls)
}

// Counts the trying and connected transports at now and moves the stall: one
// starts when at least one transport is trying and no more are connected than
// trying, and ends when that no longer holds. An ended stall longer than the
// good stall stays evidence. Must be called with stateLock.
func (self *transportConnectWindow) updateWithLock(now time.Time) {
	tryingCount := 0
	connectedCount := 0
	for carrier := range self.carrierStates {
		switch carrier.State() {
		case connect.PlatformTransportStateConnecting:
			// waiting for memory budget is the device's limit, not the
			// network's
			if !carrier.IsWaitingForBudget() {
				tryingCount += 1
			}
		case connect.PlatformTransportStateConnected:
			connectedCount += 1
		}
	}
	stalled := 0 < tryingCount && connectedCount <= tryingCount
	switch {
	case stalled && !self.stalled:
		self.stalled = true
		self.stallProgressTime = now
		self.stallLongest = 0
	case !stalled && self.stalled:
		self.stalled = false
		longest := max(self.stallLongest, transportConnectElapsed(self.stallProgressTime, now))
		if self.goodStall < longest {
			self.endedStalls = append(self.endedStalls, transportConnectStall{
				longest:  longest,
				endTime:  now,
				fallback: self.fallbackWithLock(),
			})
		}
	}
}

// At least one transport is connected and DNS carries every connected one: a
// stall then waited on the fallback, not on the user's network (D10). Must be
// called with stateLock.
func (self *transportConnectWindow) fallbackWithLock() bool {
	connectedCount := 0
	for carrier := range self.carrierStates {
		if carrier.State() != connect.PlatformTransportStateConnected {
			continue
		}
		connectedCount += 1
		switch carrier.ActiveMode() {
		case connect.TransportModeH3Dns, connect.TransportModeH3DnsPump:
		default:
			return false
		}
	}
	return 0 < connectedCount
}

// An ended stall is evidence while it ended less than the timeout ago on both
// the monotonic and the wall clock, as a delay sample is live
// (transportDelayWindow.live): the wall clock counts a sleep the monotonic
// clock skips. Must be called with stateLock.
func (self *transportConnectWindow) liveWithLock(endTime time.Time, now time.Time) bool {
	return now.Sub(endTime) < self.timeout &&
		now.Round(0).Sub(endTime.Round(0)) < self.timeout
}

// The time from start to now, on the monotonic clock when both carry it, so a
// sleep is not time spent trying. Never negative.
func transportConnectElapsed(start time.Time, now time.Time) time.Duration {
	return max(now.Sub(start), 0)
}
