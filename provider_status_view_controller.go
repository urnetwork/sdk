//go:build !ios_extension

package sdk

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// How often a started controller reads the provider status. The server's
// histogram buckets are a minute wide and its ranking is cached for about
// five minutes, so a faster poll shows nothing new.
const defaultProviderStatusPollInterval = 60 * time.Second

// ProviderStatusListener fires whenever the published status changes: a poll
// succeeded or failed, or the controller stopped. The app re-reads the
// getters.
type ProviderStatusListener interface {
	ProviderStatusChanged()
}

// ProviderStatusViewController reads GET /network/provider-status about once
// a minute while started and publishes this device's provider status: how
// often the server's provider search offered the device per minute over the
// last hour (60 buckets, oldest first), the numbers it was ranked by with
// what each means, the gates it passes, and the first reason holding it back
// (ProviderStatusReason*). The network's other provider clients are in
// GetProviderStatuses.
//
// Stop pauses polling and keeps the last snapshot, so a screen that comes
// back shows it at once while the next poll runs; Close ends the controller.
// A failed poll keeps the last snapshot, records the error
// (GetLastFetchError) and retries at the next interval.
type ProviderStatusViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	// this device's client, whose status the getters publish
	clientId *Id

	wake chan struct{}

	stateLock sync.Mutex

	pollInterval time.Duration
	started      bool
	// invalidates in-flight fetches across Stop/Start
	generation    int
	fetchInFlight bool
	// next scheduled poll; the zero time means "due now"
	nextPollAt time.Time
	forcePoll  bool

	loaded    bool
	statuses  *ProviderStatusList
	status    *ProviderStatus
	truncated bool
	// the last failed poll's error, "" once a poll succeeds
	lastFetchError string

	listeners *connect.CallbackList[ProviderStatusListener]

	// test seams (unexported; not bound)
	nowFunc   func() time.Time
	fetchFunc func(callback GetProviderStatusCallback)
}

func newProviderStatusViewController(ctx context.Context, device Device) *ProviderStatusViewController {
	vc := newProviderStatusViewControllerWithoutRun(ctx, device.GetApi(), device.GetClientId())
	go connect.HandleError(vc.run)
	return vc
}

// The controller without its polling loop; a test drives step() itself.
func newProviderStatusViewControllerWithoutRun(ctx context.Context, api *Api, clientId *Id) *ProviderStatusViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &ProviderStatusViewController{
		ctx:          cancelCtx,
		cancel:       cancel,
		clientId:     clientId,
		wake:         make(chan struct{}, 1),
		pollInterval: defaultProviderStatusPollInterval,
		statuses:     NewProviderStatusList(),
		listeners:    connect.NewCallbackList[ProviderStatusListener](),
		nowFunc:      time.Now,
	}
	vc.fetchFunc = func(callback GetProviderStatusCallback) {
		api.GetProviderStatus(callback)
	}
	return vc
}

// Start polls at once and then about once a minute.
func (self *ProviderStatusViewController) Start() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.started {
			return
		}
		self.started = true
		self.forcePoll = true
		self.nextPollAt = time.Time{}
	}()
	self.scheduleWake()
}

// Stop pauses polling and drops the poll in flight; the last snapshot stays
// published.
func (self *ProviderStatusViewController) Stop() {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started {
			return
		}
		self.started = false
		self.generation += 1
		changed = self.fetchInFlight
		self.fetchInFlight = false
	}()
	if changed {
		self.statusChanged()
	}
	self.scheduleWake()
}

func (self *ProviderStatusViewController) Close() {
	self.cancel()
}

// Refresh polls now, if started.
func (self *ProviderStatusViewController) Refresh() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.forcePoll = true
	}()
	self.scheduleWake()
}

func (self *ProviderStatusViewController) run() {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	armTimer := func(delay time.Duration, arm bool) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !arm {
			timerC = nil
			return
		}
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerC = timer.C
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-timerC:
		}
		delay, arm := self.step()
		armTimer(delay, arm)
	}
}

// Launches a due poll and returns when the next one is due; arm is false
// while stopped or while a poll is in flight (its completion wakes the loop).
func (self *ProviderStatusViewController) step() (delay time.Duration, arm bool) {
	var launchFetch bool
	var generation int
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started {
			return
		}
		now := self.nowFunc()
		if (self.forcePoll || !now.Before(self.nextPollAt)) && !self.fetchInFlight {
			self.fetchInFlight = true
			self.forcePoll = false
			self.nextPollAt = now.Add(self.pollInterval)
			generation = self.generation
			launchFetch = true
		}
		if !self.fetchInFlight {
			delay = self.nextPollAt.Sub(now)
			arm = true
		}
	}()

	if launchFetch {
		self.fetchFunc(connect.NewApiCallback[*GetProviderStatusResult](
			func(result *GetProviderStatusResult, err error) {
				self.fetchDone(generation, result, err)
			},
		))
	}
	return
}

func (self *ProviderStatusViewController) fetchDone(generation int, result *GetProviderStatusResult, err error) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if generation != self.generation {
			// stopped (and maybe restarted) while this poll was in flight
			return
		}
		self.fetchInFlight = false
		applied = true
		switch {
		case err != nil:
			self.lastFetchError = err.Error()
		case result == nil:
			self.lastFetchError = "no provider status"
		default:
			self.lastFetchError = ""
			self.loaded = true
			self.truncated = result.Truncated
			self.statuses = NewProviderStatusList()
			self.status = nil
			if result.Providers != nil {
				for _, status := range result.Providers.getAll() {
					if status == nil {
						continue
					}
					self.statuses.Add(status)
					if self.status == nil && self.clientId != nil && status.ClientId != nil && status.ClientId.Cmp(self.clientId) == 0 {
						self.status = status
					}
				}
			}
		}
	}()
	if applied {
		self.statusChanged()
		self.scheduleWake()
	}
}

func (self *ProviderStatusViewController) scheduleWake() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// GetIsLoaded is true once a poll has succeeded.
func (self *ProviderStatusViewController) GetIsLoaded() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loaded
}

// GetIsLoading is true while a poll is in flight.
func (self *ProviderStatusViewController) GetIsLoading() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.fetchInFlight
}

// GetLastFetchError is the last failed poll's error, "" once a poll succeeds.
func (self *ProviderStatusViewController) GetLastFetchError() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lastFetchError
}

// GetProviderStatus is this device's status, nil when the device is not one
// of the network's provider clients (or nothing has loaded yet).
func (self *ProviderStatusViewController) GetProviderStatus() *ProviderStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status
}

// GetProviderStatuses is every provider client the last poll covered, this
// device first.
func (self *ProviderStatusViewController) GetProviderStatuses() *ProviderStatusList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.statuses
}

// GetTruncated is true when the network has more provider clients than one
// answer covers.
func (self *ProviderStatusViewController) GetTruncated() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.truncated
}

// GetReason is this device's reason code (ProviderStatusReason*), "" without
// a status.
func (self *ProviderStatusViewController) GetReason() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil {
		return ""
	}
	return self.status.Reason
}

// GetReasonText is the server's English text for GetReason.
func (self *ProviderStatusViewController) GetReasonText() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil {
		return ""
	}
	return self.status.ReasonText
}

// GetAdmission is this device's gates, nil without a status.
func (self *ProviderStatusViewController) GetAdmission() *ProviderAdmission {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil {
		return nil
	}
	return self.status.Admission
}

// GetRankingNumbers is this device's ranking numbers in display order, empty
// without a status.
func (self *ProviderStatusViewController) GetRankingNumbers() *ProviderRankingNumberList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil || self.status.Ranking == nil {
		return NewProviderRankingNumberList()
	}
	return self.status.Ranking
}

// GetAppearances is this device's histogram, nil when there is none (no
// status, or the server could not read it).
func (self *ProviderStatusViewController) GetAppearances() *ProviderAppearanceHistogram {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil {
		return nil
	}
	return self.status.Appearances
}

// GetAppearancesPerMinute is the histogram's 60 counts, oldest first, empty
// when there is no histogram.
func (self *ProviderStatusViewController) GetAppearancesPerMinute() *Int64List {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.status == nil || self.status.Appearances == nil || self.status.Appearances.AppearancesPerMinute == nil {
		return NewInt64List()
	}
	return self.status.Appearances.AppearancesPerMinute
}

// GetAppearanceTotal is the sum of the histogram's counts.
func (self *ProviderStatusViewController) GetAppearanceTotal() int64 {
	total := int64(0)
	for _, count := range self.GetAppearancesPerMinute().getAll() {
		total += count
	}
	return total
}

// GetAppearanceMaxCount is the largest bucket, for scaling the bars.
func (self *ProviderStatusViewController) GetAppearanceMaxCount() int64 {
	maxCount := int64(0)
	for _, count := range self.GetAppearancesPerMinute().getAll() {
		maxCount = max(maxCount, count)
	}
	return maxCount
}

func (self *ProviderStatusViewController) AddProviderStatusListener(listener ProviderStatusListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}

func (self *ProviderStatusViewController) statusChanged() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.ProviderStatusChanged()
		})
	}
}
