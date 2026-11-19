package sdk

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

// extender_release_space.go — the network space's side of the gated release
// (extender_release.go; connect/EXTENDER-UPDATE2.md §1.1, rollout gates).
//
// The space owns the fetcher with its directory and closes it before the
// directory. A device that dials through the space binds its client token
// source (DeviceLocal); a hosted space never runs one, and the process whose
// extender store is read-only (K5) binds none, since only the process that
// writes the directory may add to it.
//
// The cohort control is the space value ExtenderGatedPreference, off by
// default: it turns the directory's gated preference and the fetch on and off
// together, in place, so a client outside the cohort never asks for a release
// and spends no disclosure. Whoever owns the space's values enables it per
// cohort -- the bundled space of a build, or the app through
// UpdateNetworkSpaceValues -- and the operator keeps the authority over what
// each identity is released: an outcome without records (contract R) brings
// the client back to the open fallback within the bounded attempt.
//
// What the other parts of the space reach -- the control reaches, the account
// monitor, the release request -- is ready from construction on, so the order
// the space starts its parts in never drops a reach or splits a monitor.

// The gated release state of one space. The zero value is a space that runs
// no fetch; startExtenderRelease starts one.
type extenderReleaseSpace struct {
	// set once, at construction; nil for a space with no directory and for a
	// hosted one
	fetcher atomic.Pointer[extenderReleaseFetcher]
	// the reaches tried after the space's own cached open records
	reaches extenderControlReachList
	// the account generation of the bound identity, made on first use
	accountOnce sync.Once
	account     *connect.MonitorValue[uint64]
	// requestExtenderRelease calls, whatever each came to
	requestCount atomic.Uint64
	// the block report sender (extender_block_report.go); set once, at
	// construction, nil exactly when `fetcher` is
	blockReporter atomic.Pointer[extenderBlockReporter]
}

func (self *extenderReleaseSpace) accountMonitor() *connect.MonitorValue[uint64] {
	self.accountOnce.Do(func() {
		self.account = connect.NewMonitorValue[uint64](0)
	})
	return self.account
}

// Test seam: adjusts the settings of every fetcher built after it is set.
var extenderReleaseConfigure func(settings *extenderReleaseSettings)

// Test seam: adjusts the settings of every block report sender built after it
// is set.
var extenderBlockReportConfigure func(settings *extenderBlockReportSettings)

// Builds the space's fetcher. Runs once, in the constructor, after the
// directory, the api and the strategy exist.
func (self *NetworkSpace) startExtenderRelease() {
	if self.extenderDirectory == nil || self.hostedIncompatible {
		return
	}
	var store extenderReleaseStateStore
	if self.asyncLocalState != nil {
		store = &localStateExtenderReleaseStore{
			localStorageDir: self.asyncLocalState.GetLocalState().localStorageDir,
		}
	}
	settings := defaultExtenderReleaseSettings()
	api := self.api
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		return api.transportHttpPostRaw()(ctx, requestUrl, requestBodyBytes, byJwt)
	}
	directory := self.extenderDirectory
	connectSettings := &self.clientStrategySettings.ConnectSettings
	settings.Capabilities = func() []string {
		return extenderReleaseCapabilities(connectSettings, directory.SpoofCountryCode())
	}
	if extenderReleaseConfigure != nil {
		extenderReleaseConfigure(settings)
	}
	fetcher := newExtenderReleaseFetcher(
		self.ctx,
		directory,
		self.apiUrl,
		self.obtainExtenderControlReach,
		self.extenderRelease.accountMonitor(),
		store,
		settings,
		self.logger(),
	)
	self.extenderRelease.fetcher.Store(fetcher)

	blockSettings := defaultExtenderBlockReportSettings()
	blockSettings.Post = settings.Post
	blockSettings.ControlProbe = self.askExtenderControl
	blockSettings.Attempt = func(ctx context.Context, suspect connect.ExtenderBlockSuspect) string {
		return directory.AttemptBlockSuspect(ctx, suspect, func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
			// no attestor: the dial identifies no client, and says only whether
			// the extender answers
			_, err := connect.ProbeExtenderLatency(ctx, connectSettings, extenderConfig, nil)
			return err
		})
	}
	if extenderBlockReportConfigure != nil {
		extenderBlockReportConfigure(blockSettings)
	}
	self.extenderRelease.blockReporter.Store(newExtenderBlockReporter(
		self.ctx,
		directory,
		self.apiUrl,
		fetcher.boundCredential,
		self.extenderRelease.accountMonitor(),
		blockSettings,
		self.logger(),
	))

	self.applyExtenderGatedPreference(self.valuesCopy().ExtenderGatedPreference)
}

// Stops the fetcher; the space closes the directory after it.
func (self *NetworkSpace) closeExtenderRelease() {
	if blockReporter := self.extenderRelease.blockReporter.Load(); blockReporter != nil {
		blockReporter.Close()
	}
	if fetcher := self.extenderRelease.fetcher.Load(); fetcher != nil {
		fetcher.Close()
	}
}

// Turns the gated preference and the fetch on or off together.
func (self *NetworkSpace) applyExtenderGatedPreference(enabled bool) {
	if self.extenderDirectory == nil {
		return
	}
	enabled = enabled && !self.hostedIncompatible
	settings := connect.DefaultExtenderGatedPreferenceSettings()
	settings.Enabled = enabled
	self.extenderDirectory.SetGatedPreference(settings)
	if fetcher := self.extenderRelease.fetcher.Load(); fetcher != nil {
		fetcher.setEnabled(enabled)
	}
	if blockReporter := self.extenderRelease.blockReporter.Load(); blockReporter != nil {
		blockReporter.setEnabled(enabled)
	}
	self.applyExtenderCamouflageReport(enabled)
}

// Whether the cohort control differs between two value sets.
func extenderGatedPreferenceValuesChanged(previous *NetworkSpaceValues, next *NetworkSpaceValues) bool {
	return previous.ExtenderGatedPreference != next.ExtenderGatedPreference
}

// Applies the cohort control in place. Returns whether it changed.
func (self *NetworkSpace) applyExtenderGatedPreferenceValues(values *NetworkSpaceValues) bool {
	changed, closed := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !extenderGatedPreferenceValuesChanged(&self.values, values) {
			return false, self.closed
		}
		self.values.ExtenderGatedPreference = values.ExtenderGatedPreference
		return true, self.closed
	}()
	if changed && !closed {
		self.applyExtenderGatedPreference(values.ExtenderGatedPreference)
	}
	return changed
}

// Binds the client token source of the device that dials through this space
// to the gated release, and returns its unbind. The latest bind wins. A
// hosted space never binds.
func (self *NetworkSpace) bindExtenderRelease(credential func() string) (unbind func()) {
	if self == nil || GetExtenderStoreReadOnly() {
		return func() {}
	}
	fetcher := self.extenderRelease.fetcher.Load()
	if fetcher == nil {
		return func() {}
	}
	return fetcher.bind(credential)
}

// The bound device's client token was installed, refreshed or rejected.
func (self *NetworkSpace) extenderReleaseCredentialChanged() {
	if self == nil {
		return
	}
	if fetcher := self.extenderRelease.fetcher.Load(); fetcher != nil {
		fetcher.credentialChanged()
	}
}

// Asks for a gated release now: what the seed bootstrap and an import call once
// they have established control reach. Non-blocking; it shares the one coalesced
// request with every other trigger, and does nothing while no identity is bound
// or the preference is off. A failure backoff is for the route that failed, so
// this clears it; the hourly cap, the spacing and any retry time the operator
// gave still hold (extenderReleaseSchedule.reachEstablished).
func (self *NetworkSpace) requestExtenderRelease() {
	if self == nil {
		return
	}
	self.extenderRelease.requestCount.Add(1)
	if fetcher := self.extenderRelease.fetcher.Load(); fetcher != nil {
		fetcher.trigger(extenderReleaseTriggerRequest)
	}
}

// How many times requestExtenderRelease was called on this space, whatever
// each came to.
func (self *NetworkSpace) extenderReleaseRequestCount() uint64 {
	if self == nil {
		return 0
	}
	return self.extenderRelease.requestCount.Load()
}

// Advances on every account change of the identity bound to the space's gated
// release: a client token of another network or client, a sign-out, an
// unbind. A refresh of the same identity's token does not advance it. It is
// the same monitor for the life of the space, and a space that runs no fetch
// never advances it.
func (self *NetworkSpace) extenderAccountMonitor() *connect.MonitorValue[uint64] {
	if self == nil {
		return connect.NewMonitorValue[uint64](0)
	}
	return self.extenderRelease.accountMonitor()
}

// The block report sender's counts, empty for a space that runs none.
func (self *NetworkSpace) extenderBlockReportStats() extenderBlockReportStats {
	if self == nil {
		return extenderBlockReportStats{}
	}
	if blockReporter := self.extenderRelease.blockReporter.Load(); blockReporter != nil {
		return blockReporter.statsValue()
	}
	return extenderBlockReportStats{}
}

// The fetcher's counts, empty for a space that runs none.
func (self *NetworkSpace) extenderReleaseStats() extenderReleaseStats {
	if self == nil {
		return extenderReleaseStats{}
	}
	if fetcher := self.extenderRelease.fetcher.Load(); fetcher != nil {
		return fetcher.statsValue()
	}
	return extenderReleaseStats{}
}
