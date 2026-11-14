//go:build !ios_extension

package sdk

// pro_features_view_controller.go -- the network's Pro features in use
// (pro_features_status.go), read from its proxies list for the notice an app
// shows on its main screen.
//
// It reads GET /network/proxies with the network credential the api keeps,
// when it starts, every RefreshInterval while it is open, and at each Refresh:
// an app calls Refresh when it comes to the front, after a purchase, and after
// its proxies change. The snapshot says the status, the prompt the list of
// proxies shows, and MainPrompt, the notice the main screen shows, which
// ProFeaturesNoticeTracker decides; DismissMainNotice is the user closing
// that notice. A failed read keeps the last status. Without a network
// credential the controller sends nothing and reports
// ProFeaturesErrorSignInRequired, as it does when the server refuses the
// credential.
//
// Safe for concurrent use. Listeners are called on a goroutine of the
// controller's, never with its lock held.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	ProFeaturesErrorNone = ""
	// there is no network credential, or the server refused it
	ProFeaturesErrorSignInRequired = "sign_in_required"
	// the list could not be loaded; the last status stays, and Refresh tries
	// again
	ProFeaturesErrorLoadFailed = "load_failed"
)

// ProFeaturesSnapshot is one readout of the controller.
type ProFeaturesSnapshot struct {
	// the network's Pro features in use. Its Prompt is what the list of
	// proxies shows. Never nil; it has no prompt before the first load.
	Status *ProFeaturesStatus
	// the notice the main screen shows: Status.Prompt, unless the user
	// dismissed it (ProFeaturesNoticeTracker); ProFeaturesPromptNone for none
	MainPrompt string
	// the list loaded at least once
	Loaded bool
	// a load is in flight
	Loading bool
	// the last failure, a ProFeaturesError constant; ProFeaturesErrorNone
	// after a success
	ErrorId string
}

// ProFeaturesListener receives every change of the snapshot.
type ProFeaturesListener interface {
	ProFeaturesChanged(snapshot *ProFeaturesSnapshot)
}

// proFeaturesSettings tunes the controller.
type proFeaturesSettings struct {
	// how often it reads the list again while open; zero or less never does
	RefreshInterval time.Duration
	// bounds each request
	RequestTimeout time.Duration
	// the delay of RefreshInterval; time.After, replaced only by tests
	after func(timeout time.Duration) <-chan time.Time
}

func defaultProFeaturesSettings() *proFeaturesSettings {
	return &proFeaturesSettings{
		RefreshInterval: time.Hour,
		RequestTimeout:  30 * time.Second,
		after:           time.After,
	}
}

type ProFeaturesViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api       *Api
	settings  *proFeaturesSettings
	listeners *connect.CallbackList[ProFeaturesListener]

	stateLock      sync.Mutex
	started        bool
	status         *ProFeaturesStatus
	tracker        *ProFeaturesNoticeTracker
	loaded         bool
	loading        bool
	loadGeneration uint64
	errorId        string
}

func newProFeaturesViewController(ctx context.Context, api *Api, settings *proFeaturesSettings) *ProFeaturesViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ProFeaturesViewController{
		ctx:       cancelCtx,
		cancel:    cancel,
		api:       api,
		settings:  settings,
		listeners: connect.NewCallbackList[ProFeaturesListener](),
		status:    ProFeaturesStatusFor(nil),
		tracker:   NewProFeaturesNoticeTracker(),
	}
}

// OpenProFeaturesViewController opens the network's Pro features in use on
// this api, with the network credential it keeps. An app opens one per
// sign-in and closes it at sign out, which resets the main screen's notice;
// it ends with the api.
func (self *Api) OpenProFeaturesViewController() *ProFeaturesViewController {
	return newProFeaturesViewController(self.ctx, self, defaultProFeaturesSettings())
}

// Start loads the list, then again every RefreshInterval until Close.
func (self *ProFeaturesViewController) Start() {
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.started || self.ctx.Err() != nil {
			return false
		}
		self.started = true
		return true
	}()
	if !start {
		return
	}
	self.Refresh()
	if 0 < self.settings.RefreshInterval {
		go connect.HandleError(self.run)
	}
}

func (self *ProFeaturesViewController) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.settings.after(self.settings.RefreshInterval):
		}
		self.Refresh()
	}
}

func (self *ProFeaturesViewController) Stop() {}

func (self *ProFeaturesViewController) Close() {
	self.cancel()
}

func (self *ProFeaturesViewController) AddProFeaturesListener(listener ProFeaturesListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}

// GetSnapshot is the current readout. Never nil.
func (self *ProFeaturesViewController) GetSnapshot() *ProFeaturesSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotWithLock()
}

func (self *ProFeaturesViewController) snapshotWithLock() *ProFeaturesSnapshot {
	return &ProFeaturesSnapshot{
		Status:     cloneProFeaturesStatus(self.status),
		MainPrompt: self.tracker.GetShownPrompt(),
		Loaded:     self.loaded,
		Loading:    self.loading,
		ErrorId:    self.errorId,
	}
}

func (self *ProFeaturesViewController) publish() {
	snapshot := self.GetSnapshot()
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.ProFeaturesChanged(snapshot)
		})
	}
}

// DismissMainNotice is the user closing the main screen's notice: it stays
// withdrawn while its prompt stays the same (ProFeaturesNoticeTracker.Dismiss).
// The list of proxies keeps showing the prompt.
func (self *ProFeaturesViewController) DismissMainNotice() {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.tracker.Dismiss() != ProFeaturesNoticeNone
	}()
	if changed {
		self.publish()
	}
}

// Refresh reads the list again. A refresh replaces one in flight.
func (self *ProFeaturesViewController) Refresh() {
	networkByJwt := self.api.networkCredential()
	generation, ok := func() (uint64, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ctx.Err() != nil {
			return 0, false
		}
		self.loadGeneration += 1
		if networkByJwt == "" {
			self.loading = false
			self.errorId = ProFeaturesErrorSignInRequired
		} else {
			self.loading = true
		}
		return self.loadGeneration, true
	}()
	if !ok {
		return
	}
	self.publish()
	if networkByJwt == "" {
		return
	}
	go connect.HandleError(func() {
		status, errorId := self.load(networkByJwt)
		applied := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if generation != self.loadGeneration || self.ctx.Err() != nil {
				return false
			}
			self.loading = false
			self.errorId = errorId
			if errorId == ProFeaturesErrorNone {
				self.status = status
				self.loaded = true
				self.tracker.Update(status)
			}
			return true
		}()
		if applied {
			self.publish()
		}
	})
}

func (self *ProFeaturesViewController) load(networkByJwt string) (*ProFeaturesStatus, string) {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()
	result, err := connect.HttpGetWithRawFunction(
		ctx,
		self.api.getHttpGetRaw(),
		fmt.Sprintf("%s/network/proxies", self.api.apiUrl),
		networkByJwt,
		&NetworkClientsResult{},
		connect.NewNoopApiCallback[*NetworkClientsResult](),
	)
	if err != nil {
		return nil, proFeaturesErrorFor(err)
	}
	return ProFeaturesStatusFor(result), ProFeaturesErrorNone
}

// proFeaturesErrorFor maps a failed read: a missing network credential, or one
// the server refused (401, 403), needs a new sign-in; any other failure is
// retryable.
func proFeaturesErrorFor(err error) string {
	if errors.Is(err, ErrNetworkCredentialRequired) {
		return ProFeaturesErrorSignInRequired
	}
	var statusError *connect.HttpStatusError
	if errors.As(err, &statusError) &&
		(statusError.StatusCode == http.StatusUnauthorized || statusError.StatusCode == http.StatusForbidden) {
		return ProFeaturesErrorSignInRequired
	}
	return ProFeaturesErrorLoadFailed
}
