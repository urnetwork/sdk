package sdk

// device_local_provide_share.go -- the share limits a device's provider runs
// under (provide_share.go): the limiter its providers pace their returns to,
// the month's data its provider stats feed, and the budget pause.
//
// One deviceLocalProvideShare lives as long as the device's provider. The
// provider's packet stats add the data the device carries for others (add);
// the owner's setter changes the limits (setLimits). Everything those cause
// beyond the meter itself -- the budget pause, saving the month's data,
// telling the status listeners -- is done by one loop (run), so no caller
// does file or listener work, and the loop wakes at the end of the month to
// start the next one, which lifts a budget pause by itself.
//
// The budget pause composes with the platform's pause (SetProvidePaused, the
// battery and power policy): the provider's contract manager is paused while
// either holds (DeviceLocal.applyProvidePaused), and GetProvidePaused keeps
// answering the platform's own value, so the platform's round trips never
// carry the budget.
//
// The month is the calendar month in UTC, the same on the phone, its tunnel
// extension, the desktop service and the sn miner; the apps show when it
// ends (PeriodEndMillis) in the reader's own time zone.

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

type deviceLocalProvideShareSettings struct {
	// The month's data is saved at most this often while it grows, so a
	// device that stops without warning loses at most this much of it.
	UsageSaveInterval time.Duration
	// It is also saved once this much more has been shared since the last
	// save, so a fast share loses little more than this.
	UsageSaveByteCount ByteCount
	// The status listeners hear of the data growing at most this often. A
	// change of the limits, of the month or of the budget state is told at
	// once.
	StatusInterval time.Duration
	// How the sharing speed is paced.
	LimiterSettings *connect.ProviderShareLimiterSettings
}

// A minute and 64 MiB of saves, and a status a second, which is as often as
// the apps redraw a figure.
func defaultDeviceLocalProvideShareSettings() *deviceLocalProvideShareSettings {
	return &deviceLocalProvideShareSettings{
		UsageSaveInterval:  time.Minute,
		UsageSaveByteCount: 64 * 1024 * 1024,
		StatusInterval:     time.Second,
		LimiterSettings:    connect.DefaultProviderShareLimiterSettings(),
	}
}

// Safe for concurrent use. The loop, run, owns every side effect.
type deviceLocalProvideShare struct {
	ctx      context.Context
	settings *deviceLocalProvideShareSettings
	log      connect.Logger
	// nil keeps the limits and the month's data for the life of the device
	store *provideShareStore
	// the zone the month is a calendar month of
	location *time.Location
	// the pacing every provider of the device is built with
	limiter *connect.ProviderShareLimiter
	// applies the budget pause; called only from the loop, with no lock held
	budgetPausedChanged func(budgetPaused bool)

	// a buffered wake of the loop; a wake already pending covers another
	wake chan struct{}

	// serializes the owner's setter, so the stored limits are the last set.
	// Taken before stateLock.
	limitsLock sync.Mutex

	stateLock sync.Mutex
	meter     *provideShareMeter
	// the budget state the pause was last applied for
	budgetPaused bool
	// the data last saved, and when; a new month or a budget change saves at
	// once
	savedByteCount ByteCount
	saveTime       time.Time
	saveDue        bool
	// a change the listeners have not heard of, whether it is told at once,
	// and when the last was told
	statusDue   bool
	statusForce bool
	statusTime  time.Time

	statusListeners *connect.CallbackList[ProvideShareStatusChangeListener]

	// time.Now and time.After outside tests
	now   func() time.Time
	after func(delay time.Duration) <-chan time.Time
}

// The share state of a device, from what store keeps, without its loop; the
// device starts run. The loop's first step applies a budget the stored month
// already reached, so a device that restarts paused stays paused.
func newDeviceLocalProvideShareWithoutRun(
	ctx context.Context,
	settings *deviceLocalProvideShareSettings,
	store *provideShareStore,
	location *time.Location,
	log connect.Logger,
	budgetPausedChanged func(budgetPaused bool),
	now func() time.Time,
	after func(delay time.Duration) <-chan time.Time,
) *deviceLocalProvideShare {
	limits := ProvideShareLimits{}
	var usage *provideShareUsage
	if store != nil {
		limits, _ = store.loadLimits()
		usage = store.loadUsage()
	}
	meter := newProvideShareMeter(limits, usage, now(), location)
	limiter := connect.NewProviderShareLimiter(settings.LimiterSettings)
	limiter.SetByteRate(limits.MaxBytesPerSecond)
	share := &deviceLocalProvideShare{
		ctx:                 ctx,
		settings:            settings,
		log:                 log,
		store:               store,
		location:            location,
		limiter:             limiter,
		budgetPausedChanged: budgetPausedChanged,
		wake:                make(chan struct{}, 1),
		meter:               meter,
		savedByteCount:      meter.byteCount,
		saveTime:            now(),
		statusListeners:     connect.NewCallbackList[ProvideShareStatusChangeListener](),
		now:                 now,
		after:               after,
	}
	share.scheduleWake()
	return share
}

// Steps on every wake and when the next save, status or month is due, until
// the device closes.
func (self *deviceLocalProvideShare) run() {
	var due <-chan time.Time
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-due:
		}
		due = self.after(max(self.step(), 0))
	}
}

// Wakes the loop without blocking.
func (self *deviceLocalProvideShare) scheduleWake() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// The limits in force.
func (self *deviceLocalProvideShare) limits() ProvideShareLimits {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.meter.limits
}

// Puts limits in force at once: the sharing speed of every provider of the
// device, and the budget, which pauses or resumes providing at the next step.
// The limits are kept by the store, whose error is returned; they apply for
// the session either way.
func (self *deviceLocalProvideShare) setLimits(limits ProvideShareLimits) error {
	self.limitsLock.Lock()
	defer self.limitsLock.Unlock()
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.meter.limits == limits {
			return false
		}
		self.meter.limits = limits
		self.statusDue = true
		self.statusForce = true
		return true
	}()
	if !changed {
		return nil
	}
	self.limiter.SetByteRate(limits.MaxBytesPerSecond)
	self.scheduleWake()
	if self.store == nil {
		return nil
	}
	return self.store.saveLimits(limits)
}

// Adds data the device carried for others.
func (self *deviceLocalProvideShare) add(byteCount ByteCount) {
	if byteCount <= 0 {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.meter.add(self.now(), self.location, byteCount)
		self.statusDue = true
	}()
	self.scheduleWake()
}

// Whether the budget pause holds, as last applied.
func (self *deviceLocalProvideShare) isBudgetPaused() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.budgetPaused
}

func (self *deviceLocalProvideShare) status() *ProvideShareStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.meter.status()
}

func (self *deviceLocalProvideShare) addStatusChangeListener(listener ProvideShareStatusChangeListener) Sub {
	callbackId := self.statusListeners.Add(listener)
	return newSub(func() {
		self.statusListeners.Remove(callbackId)
	})
}

// Starts a new month when the current one ended, applies a change of the
// budget state, saves the month's data when a save is due and tells the
// listeners of a change when one is due. Returns how long until the next
// thing is due.
func (self *deviceLocalProvideShare) step() time.Duration {
	now := self.now()
	var budgetChanged bool
	var budgetPaused bool
	var saveUsage *provideShareUsage
	var status *ProvideShareStatus
	var next time.Time
	nextAt := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.meter.roll(now, self.location) {
			self.saveDue = true
			self.statusDue = true
			self.statusForce = true
		}
		if reached := self.meter.budgetReached(); reached != self.budgetPaused {
			self.budgetPaused = reached
			budgetChanged = true
			budgetPaused = reached
			self.saveDue = true
			self.statusDue = true
			self.statusForce = true
		}
		if self.store != nil && (self.saveDue || self.savedByteCount != self.meter.byteCount) {
			saveAt := self.saveTime.Add(self.settings.UsageSaveInterval)
			if self.saveDue ||
				self.settings.UsageSaveByteCount <= self.meter.byteCount-self.savedByteCount ||
				!now.Before(saveAt) {
				usage := self.meter.usage()
				saveUsage = &usage
				self.savedByteCount = self.meter.byteCount
				self.saveTime = now
				self.saveDue = false
			} else {
				nextAt(saveAt)
			}
		}
		if self.statusDue {
			statusAt := self.statusTime.Add(self.settings.StatusInterval)
			if self.statusForce || !now.Before(statusAt) {
				status = self.meter.status()
				self.statusDue = false
				self.statusForce = false
				self.statusTime = now
			} else {
				nextAt(statusAt)
			}
		}
		nextAt(self.meter.periodEnd)
	}()

	if budgetChanged {
		self.log.Infof("[device]provide share budget reached = %t\n", budgetPaused)
		if self.budgetPausedChanged != nil {
			self.budgetPausedChanged(budgetPaused)
		}
	}
	if saveUsage != nil {
		if err := self.store.saveUsage(*saveUsage); err != nil {
			self.log.Infof("[device]provide share usage save err = %s\n", err)
		}
	}
	if status != nil {
		for _, listener := range self.statusListeners.Get() {
			published := *status
			connect.HandleError(func() {
				listener.ProvideShareStatusChanged(&published)
			})
		}
	}
	return next.Sub(now)
}

// The store of the device's share state: the space's local state, else the
// embedder's directory, else none.
func (self *DeviceLocal) provideShareStoreForSettings(settings *DeviceLocalSettings) *provideShareStore {
	if asyncLocalState := self.networkSpaceAsyncLocalState(); asyncLocalState != nil {
		return &provideShareStore{dir: asyncLocalState.GetLocalState().localStorageDir}
	}
	if settings != nil && settings.ProvideShareStorageDir != "" {
		return &provideShareStore{dir: settings.ProvideShareStorageDir}
	}
	return nil
}

// The owner's limits on how much this device shares (provide_share.go). Zero
// limits on a device without a provider.
func (self *DeviceLocal) GetProvideShareLimits() *ProvideShareLimits {
	if self.provideShare == nil {
		return &ProvideShareLimits{}
	}
	limits := self.provideShare.limits()
	return &limits
}

// Sets the owner's limits and applies them at once: the sharing speed of the
// running provider, and the monthly budget, which pauses providing to other
// people once this month's data reaches it and resumes it when the budget is
// raised or the next month starts. Kept across restarts. A negative limit is
// no limit. A hosted device has no provider to limit, so the setter is
// guarded there as SetProvideMode is.
func (self *DeviceLocal) SetProvideShareLimits(limits *ProvideShareLimits) {
	if self.hostedIncompatibleGuarded("SetProvideShareLimits") || self.provideShare == nil {
		return
	}
	normal := normalProvideShareLimits(limits)
	if err := self.provideShare.setLimits(normal); err != nil {
		self.log.Infof("[device]provide share limits err = %s\n", err)
	}
	self.log.Infof("[device]provide share limits = %d B/s, %d B/month\n", normal.MaxBytesPerSecond, normal.MonthlyByteCount)
}

// This month's sharing against the owner's limits. Never nil; a device
// without a provider shares nothing.
func (self *DeviceLocal) GetProvideShareStatus() *ProvideShareStatus {
	if self.provideShare == nil {
		start, end := provideSharePeriod(time.Now(), time.UTC)
		return &ProvideShareStatus{
			PeriodStartMillis: start.UnixMilli(),
			PeriodEndMillis:   end.UnixMilli(),
		}
	}
	return self.provideShare.status()
}

func (self *DeviceLocal) AddProvideShareStatusChangeListener(listener ProvideShareStatusChangeListener) Sub {
	if self.provideShare == nil {
		return newSub(func() {})
	}
	return self.provideShare.addStatusChangeListener(listener)
}

// Pauses the provider's contract manager while the platform's pause or the
// share budget holds, and resumes it when neither does.
func (self *DeviceLocal) applyProvidePaused() {
	client, platformPaused := func() (*connect.Client, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.providerClient(), self.platformProvidePaused
	}()
	if client == nil {
		return
	}
	budgetPaused := self.provideShare != nil && self.provideShare.isBudgetPaused()
	if client.ContractManager().SetProvidePaused(platformPaused || budgetPaused) {
		self.log.Infof("[device]provider paused = %t (platform %t, share budget %t)\n", platformPaused || budgetPaused, platformPaused, budgetPaused)
	}
}
