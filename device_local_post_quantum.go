package sdk

// device_local_post_quantum.go -- the device's notice that the selected
// location has no post-quantum exits right now (post_quantum_unavailable.go).
// The device decides it, so the connect screen and every surface that runs
// without it read one notice from one check. On ios the device runs in the
// packet tunnel extension, and the app's DeviceRemote mirrors it.
//
// The rule (owner, 2026-11-24: "Tell them, let them choose"): while
// post-quantum encryption is on, a location or group is selected and the
// connection is not connected, a find-providers2 request with
// require_modern_provider that answers none, then one without it that answers
// some, give the notice for that location. A location with no exits at all is
// not a post-quantum notice, and best available and a client id are never
// checked. Both requests ask as a window does, not strict, so a city a window
// answers from its region or country is answered the same way here. A notice
// stands until the location changes, post-quantum turns off, the connection
// connects, or a recheck, every RecheckInterval, finds post-quantum exits. A
// check that fails or times out gives no notice, and is retried after
// RecheckInterval.
//
// Isolation: the notice never touches the tunnel or the connect path. It
// learns of changes through the device's own listeners for the connect
// location, the performance profile and the window status, each of which only
// wakes it with a non-blocking send, and it reads the device's state on its
// own goroutine. The only requests it makes are a check's two discovery
// requests, on a check goroutine it cancels and joins, so a request that
// fails, stalls or answers late only leaves the notice off. Its listeners are
// called on its goroutine, in the order of the changes.
//
// Cost, which matters most in the ios network extension: no request unless
// post-quantum is on, a checkable location is selected and the connection is
// not connected; at most two small requests per check; one check per
// selection, then one per RecheckInterval only while the notice stands or the
// checks fail; and a connection that drops in and out of connected checks its
// location at most once per MinCheckInterval. Memory is one parked goroutine,
// a wake channel and the notice. A hosted device never runs it: it runs
// without post-quantum encryption.

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Owned by one DeviceLocal for its life. The notice and the listeners are
// safe for concurrent use; the rest belongs to run.
type deviceLocalPostQuantumNotice struct {
	ctx      context.Context
	device   *DeviceLocal
	settings *postQuantumSupplySettings
	log      connect.Logger
	// reads the window status the connected rule reads; the device's own
	// unless a test installs another, and then still woken by the device's
	// window status listeners
	windowStatus func() *WindowStatus
	// a test's barrier, called after each check answers; nil in production
	testingChecked func()

	// the wake of run: buffered, so a change never waits on the notice
	wake chan struct{}

	stateLock              sync.Mutex
	postQuantumUnavailable *PostQuantumUnavailable

	listeners *connect.CallbackList[PostQuantumUnavailableChangeListener]
}

// The answer of one check.
type postQuantumSupplyAnswer struct {
	// the location has no post-quantum exits but has others
	unavailable bool
	err         error
}

// Wakes the notice on a change of what it reads: the connect location, the
// performance profile and the window status. Called on the device's emit
// paths, so it only signals.
type deviceLocalPostQuantumNoticeWake struct {
	wake chan struct{}
}

func (self *deviceLocalPostQuantumNoticeWake) notify() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

func (self *deviceLocalPostQuantumNoticeWake) ConnectLocationChanged(*ConnectLocation) {
	self.notify()
}

func (self *deviceLocalPostQuantumNoticeWake) PerformanceProfileChanged(*PerformanceProfile) {
	self.notify()
}

func (self *deviceLocalPostQuantumNoticeWake) WindowStatusChanged(*WindowStatus) {
	self.notify()
}

// The notice of `device`, with no check running until run starts. windowStatus
// is nil for the device's own.
func newDeviceLocalPostQuantumNotice(
	ctx context.Context,
	device *DeviceLocal,
	settings *postQuantumSupplySettings,
	windowStatus func() *WindowStatus,
	testingChecked func(),
) *deviceLocalPostQuantumNotice {
	if windowStatus == nil {
		windowStatus = device.GetWindowStatus
	}
	return &deviceLocalPostQuantumNotice{
		ctx:            ctx,
		device:         device,
		settings:       settings,
		log:            device.log,
		windowStatus:   windowStatus,
		testingChecked: testingChecked,
		wake:           make(chan struct{}, 1),
		listeners:      connect.NewCallbackList[PostQuantumUnavailableChangeListener](),
	}
}

// GetPostQuantumUnavailable is the device's notice that the selected location
// has no post-quantum exits right now, nil while there is none: while
// post-quantum encryption is off, while the selection is not a location or a
// group, while disconnected or connected, and until a check has answered (see
// device_local_post_quantum.go). Never waits on a check.
func (self *DeviceLocal) GetPostQuantumUnavailable() *PostQuantumUnavailable {
	self.postQuantumNotice.stateLock.Lock()
	defer self.postQuantumNotice.stateLock.Unlock()
	return self.postQuantumNotice.postQuantumUnavailable
}

// AddPostQuantumUnavailableChangeListener subscribes to every change of the
// notice. The listener is called on the notice's goroutine.
func (self *DeviceLocal) AddPostQuantumUnavailableChangeListener(listener PostQuantumUnavailableChangeListener) Sub {
	callbackId := self.postQuantumNotice.listeners.Add(listener)
	return newSub(func() {
		self.postQuantumNotice.listeners.Remove(callbackId)
	})
}

// Subscribes to the device's changes, then decides the notice and checks the
// selected location until the device closes. Subscribing first means a change
// that lands while run reads the device's state still wakes it.
func (self *deviceLocalPostQuantumNotice) run() {
	wake := &deviceLocalPostQuantumNoticeWake{wake: self.wake}
	subs := []Sub{
		self.device.AddConnectLocationChangeListener(wake),
		self.device.AddPerformanceProfileChangeListener(wake),
		self.device.AddWindowStatusChangeListener(wake),
	}
	defer func() {
		for _, sub := range subs {
			sub.Close()
		}
	}()

	// the checks started and not yet answered, a canceled one included, all
	// joined before run returns
	var checks sync.WaitGroup
	defer checks.Wait()

	// the location the notice and its check are for, nil while no check
	// applies
	var location *ConnectLocation
	// the check of location in flight: its answer arrives on checkDone, and
	// checkCancel ends it. nil while none is in flight
	var checkDone chan postQuantumSupplyAnswer
	var checkCancel context.CancelFunc
	// fires when the next check of location starts, nil while none is due
	var checkDue <-chan time.Time
	// the location checked last and when, kept while only the connection moves
	// in and out of connected, so those moves space their checks
	// (MinCheckInterval)
	var lastCheckLocation *ConnectLocation
	var lastCheckTime time.Time

	endCheck := func() {
		if checkCancel != nil {
			checkCancel()
			checkCancel = nil
		}
		checkDone = nil
		checkDue = nil
	}
	defer endCheck()
	startCheck := func() {
		lastCheckLocation = location
		lastCheckTime = time.Now()
		var checkCtx context.Context
		var cancel context.CancelFunc
		if 0 < self.settings.CheckTimeout {
			checkCtx, cancel = context.WithTimeout(self.ctx, self.settings.CheckTimeout)
		} else {
			checkCtx, cancel = context.WithCancel(self.ctx)
		}
		// buffered, so an abandoned check finishes and exits on its own
		done := make(chan postQuantumSupplyAnswer, 1)
		checkLocation := location
		checks.Add(1)
		go connect.HandleError(func() {
			defer checks.Done()
			defer cancel()
			unavailable, err := self.findPostQuantumSupply(checkCtx, checkLocation)
			done <- postQuantumSupplyAnswer{unavailable: unavailable, err: err}
		})
		checkDone = done
		checkCancel = cancel
		checkDue = nil
	}

	for {
		next, selectedBySetting := self.applicableLocation()
		if selectedBySetting {
			// the user's selection or setting, not the connection, decided:
			// the next check of a location starts at once
			lastCheckLocation = nil
		}
		if next == nil || location == nil || !connectLocationTransportEqual(next, location) {
			// another location, or none: the notice and the check were for
			// the one before
			self.setPostQuantumUnavailable(nil)
			endCheck()
			location = next
			if location != nil {
				var delay time.Duration
				if lastCheckLocation != nil && connectLocationTransportEqual(lastCheckLocation, location) {
					delay = time.Until(lastCheckTime.Add(self.settings.MinCheckInterval))
				}
				if 0 < delay {
					checkDue = time.After(delay)
				} else {
					startCheck()
				}
			}
		}

		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-checkDue:
			startCheck()
		case answer := <-checkDone:
			checkCancel()
			checkCancel = nil
			checkDone = nil
			if answer.err != nil {
				// a failed check is no notice
				self.log.Infof("[pq]post-quantum supply check failed: %v", answer.err)
				self.setPostQuantumUnavailable(nil)
			} else if answer.unavailable {
				self.setPostQuantumUnavailable(&PostQuantumUnavailable{Location: location})
			} else {
				self.setPostQuantumUnavailable(nil)
			}
			if (answer.err != nil || answer.unavailable) && 0 < self.settings.RecheckInterval {
				checkDue = time.After(self.settings.RecheckInterval)
			}
			if self.testingChecked != nil {
				self.testingChecked()
			}
		}
	}
}

// The location a check applies to now, nil when none does: post-quantum
// encryption is on, the selection is a location or a group, and the
// connection is not connected. selectedBySetting says the profile or the
// selection decided, not the connection.
func (self *deviceLocalPostQuantumNotice) applicableLocation() (location *ConnectLocation, selectedBySetting bool) {
	performanceProfile := self.device.GetPerformanceProfile()
	if performanceProfile == nil || !performanceProfile.PostQuantumEncryption {
		return nil, true
	}
	location = self.device.GetConnectLocation()
	if !postQuantumSupplyCheckable(location) {
		return nil, true
	}
	if windowStatus := self.windowStatus(); windowStatus != nil && connectionSatisfied(
		windowStatus.MinSatisfied,
		windowStatus.ProviderStateAdded,
		location.ProviderCount,
	) {
		return nil, false
	}
	return location, false
}

// Whether the location has no post-quantum exits but has others, so turning
// post-quantum encryption off would connect: a find-providers2 request with
// the post-quantum requirement answers none, and one without it answers some.
// A location with no exits at all is not a post-quantum notice. Both requests
// ask as a window does (not strict).
func (self *deviceLocalPostQuantumNotice) findPostQuantumSupply(ctx context.Context, location *ConnectLocation) (bool, error) {
	api := self.device.GetApi()
	find := func(requireModernProvider bool) (int, error) {
		result, err := api.FindProviders2SyncWithContext(ctx, &FindProviders2Args{
			Specs:                 locationProviderSpecs(location),
			Count:                 self.settings.ProviderCount,
			RequireModernProvider: requireModernProvider,
		})
		if err != nil {
			return 0, err
		}
		if result == nil || result.ProviderStats == nil {
			return 0, nil
		}
		return result.ProviderStats.Len(), nil
	}
	modernCount, err := find(true)
	if err != nil || 0 < modernCount {
		return false, err
	}
	count, err := find(false)
	if err != nil {
		return false, err
	}
	return 0 < count, nil
}

// Publishes a change of the notice. Only run calls it, so the listeners hear
// the changes in order.
func (self *deviceLocalPostQuantumNotice) setPostQuantumUnavailable(postQuantumUnavailable *PostQuantumUnavailable) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if postQuantumUnavailableEqual(self.postQuantumUnavailable, postQuantumUnavailable) {
			return
		}
		self.postQuantumUnavailable = postQuantumUnavailable
		changed = true
	}()
	if changed {
		for _, listener := range self.listeners.Get() {
			connect.HandleError(func() {
				listener.PostQuantumUnavailableChanged(postQuantumUnavailable)
			})
		}
	}
}
