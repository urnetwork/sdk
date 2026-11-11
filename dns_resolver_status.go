// The status of the device's dns resolver (connect ADBLOCK3RD C5, C6, C8 and
// decision 16): whether the selected resolver answers, has failed over to
// URnetwork's dns or fails closed, what verification found out about the
// selected preset, and this connection's blocked-lookups count.
//
// The state follows the live mux's resolver health (connect
// UpgradeMux.DnsResolverHealth), which counts failures only while the window
// carries traffic (UpgradeMux.SetTunnelReady), so the state is unknown while
// the window forms, and a network change or a settings swap starts it over.
// An ad-blocking or no-filtering preset fails over to URnetwork's default
// resolver; a family, malware or personal preset, and every other selection,
// fails closed (decision 3).
//
// Verification (C6): once the tunnel resolver has answered or failed with a
// preset selected, and again after a resolver generation change at most once
// per DeviceLocalSettings.DnsResolverVerifyMinInterval, the device asks every
// url of the preset for one name through the tunnel: the preset's canary, a
// name it is known to block, or example.com for a personal preset. A one-tap
// preset without a canary has nothing to verify. The device says only what it
// knows: a canary blocked is confirmed and a canary resolved is not
// filtering; a personal preset is at best unconfirmed, since a wrong id or a
// spent free quota answers unfiltered, except that Control D refuses an id it
// does not know and AdGuard fails a malformed one.
//
// The blocked count (I3) is the live mux's: it starts at zero with each mux,
// so at each connect, and the device zeroes it when the selection changes, so
// it always names the selected provider. It stays on the device: it is never
// logged, saved or sent anywhere but over the local device rpc.
//
// The status names a selection by its preset id, never the personal id, and
// the app log line `[dns]preset=<id> personal=<yes|no> state=<state>
// verification=<result>` carries neither the personal id nor the count (I2,
// D5).
package sdk

import (
	"context"
	"math"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect"
)

// The states of the dns resolver (DnsResolverStatus.State).
const (
	// No resolver has answered or failed yet: the window is forming, the
	// network or the settings just changed, or the device is not connected.
	DnsResolverStateUnknown = "unknown"
	// The selected resolver answers.
	DnsResolverStateOk = "ok"
	// The selected resolver is failing and lookups fail closed: a family,
	// malware or personal preset, or any other selection, or a preset whose
	// failover has not answered yet.
	DnsResolverStateFailing = "failing"
	// The selected ad-blocking or no-filtering preset is failing and lookups
	// go to URnetwork's default dns through the tunnel until it answers again.
	DnsResolverStateFailedOver = "failedOver"
)

// The verification of the selected preset (DnsResolverStatus.Verification).
const (
	// Nothing to report: no preset, a preset without a canary, a probe that
	// got no answer, or a probe not run yet on this connection.
	DnsResolverVerificationNone = "none"
	// A one-tap preset answered its canary with a block.
	DnsResolverVerificationConfirmed = "confirmed"
	// A one-tap preset resolved its canary: it is not filtering from this
	// exit.
	DnsResolverVerificationNotFiltering = "notFiltering"
	// Control D refused the personal preset: it does not recognize the
	// resolver id.
	DnsResolverVerificationRejected = "rejected"
	// AdGuard DNS failed the personal preset: it rejected the device id.
	DnsResolverVerificationMalformed = "malformed"
	// The personal preset answered normally, which a wrong id or a spent free
	// quota also does: saved, but the profile cannot be confirmed.
	DnsResolverVerificationUnconfirmed = "unconfirmed"
)

// The name a personal preset is verified with: any name a resolver answers.
const dnsResolverVerifyName = "example.com"

// The extended dns error INFO-CODEs (RFC 8914) that mark a block: Blocked,
// Censored and Filtered, the ones connect's blocked count reads (B9).
var dnsResolverBlockEdeCodes = []uint16{15, 16, 17}

// The status of the device's dns resolver. All values are plain, so the
// status crosses the device rpc as it stands.
type DnsResolverStatus struct {
	// DnsResolverState*
	State string
	// DnsResolverVerification*
	Verification string
	// the selection the status describes, as DnsResolverSettings.PresetId
	PresetId string
	// unix milliseconds when State last changed, 0 when it never has
	SinceTime int64
	// this connection's blocked-lookups count (GetDnsBlockedCount) when the
	// status was read
	BlockedCount int64
}

func unknownDnsResolverStatus() *DnsResolverStatus {
	return &DnsResolverStatus{
		State:        DnsResolverStateUnknown,
		Verification: DnsResolverVerificationNone,
	}
}

func cloneDnsResolverStatus(status *DnsResolverStatus) *DnsResolverStatus {
	if status == nil {
		return nil
	}
	cloned := *status
	return &cloned
}

// The blocked-lookups count of a live mux (connect UpgradeMux).
type dnsBlockedCounter interface {
	DnsBlockedCount() uint64
	ResetDnsBlockedCount()
}

// The selection in force and the status bookkeeping of a device: its live
// mux, the mapped state of the mux's resolver health, and the verification
// probe's last run. Not safe for concurrent use; the device guards it with
// its stateLock.
type dnsResolverStatusTracker struct {
	// the selection the mux settings derive from (DnsResolverSettings
	// PresetId and PresetPersonalId)
	presetId   string
	personalId string

	// the mux whose resolver health the state follows, nil while there is
	// none, and the removal of its health callback
	upgradeMux *connect.UpgradeMux
	healthSub  func()
	// the live mux's blocked count, nil while there is none
	blockedCounter dnsBlockedCounter

	state        string
	sinceTime    time.Time
	verification string

	// the mux and selection of the last probe started, and when
	verifyMux  *connect.UpgradeMux
	verifyKey  string
	verifyTime time.Time
}

// A device starts on URnetwork's default dns, with no resolver.
func newDnsResolverStatusTracker() dnsResolverStatusTracker {
	return dnsResolverStatusTracker{
		presetId:     DnsPresetIdDefault,
		state:        DnsResolverStateUnknown,
		verification: DnsResolverVerificationNone,
	}
}

// Records the selection; returns whether it changed, which starts the
// verification over.
func (self *dnsResolverStatusTracker) setSelection(presetId string, personalId string) bool {
	if dnsResolverSelectionKey(self.presetId, self.personalId) == dnsResolverSelectionKey(presetId, personalId) {
		return false
	}
	self.presetId = presetId
	self.personalId = personalId
	self.verification = DnsResolverVerificationNone
	return true
}

// Follows a new mux, or none. A new mux's resolver has not answered yet, and
// its exit may filter differently, so the state and the verification start
// over. Returns whether the mux changed.
func (self *dnsResolverStatusTracker) setMux(upgradeMux *connect.UpgradeMux, now time.Time) bool {
	if self.upgradeMux == upgradeMux {
		return false
	}
	self.upgradeMux = upgradeMux
	if upgradeMux != nil {
		self.blockedCounter = upgradeMux
	} else {
		self.blockedCounter = nil
	}
	if self.state != DnsResolverStateUnknown {
		self.state = DnsResolverStateUnknown
		self.sinceTime = now
	}
	self.verification = DnsResolverVerificationNone
	return true
}

// Takes a health report of the live mux. Returns whether the state changed,
// and whether a verification probe is due: the resolver has answered or
// failed, the selection has something to verify, and no probe of this
// selection ran on this mux within verifyMinInterval.
func (self *dnsResolverStatusTracker) healthChanged(
	upgradeMux *connect.UpgradeMux,
	health connect.DnsResolverHealth,
	selection dnsResolverSelection,
	now time.Time,
	verifyMinInterval time.Duration,
) (changed bool, probe bool) {
	if upgradeMux == nil || upgradeMux != self.upgradeMux {
		// a report from a retired mux describes nothing in force
		return false, false
	}
	failover := selection.preset != nil && selection.preset.failover
	state := dnsResolverStateOf(health.State, failover)
	if state != self.state {
		self.state = state
		self.sinceTime = now
		changed = true
	}
	if state == DnsResolverStateUnknown || dnsResolverVerifyNameOf(selection.preset) == "" {
		return changed, false
	}
	key := dnsResolverSelectionKey(self.presetId, self.personalId)
	if self.verifyMux == upgradeMux && self.verifyKey == key &&
		0 < verifyMinInterval && now.Sub(self.verifyTime) < verifyMinInterval {
		return changed, false
	}
	self.verifyMux = upgradeMux
	self.verifyKey = key
	self.verifyTime = now
	return changed, true
}

// Takes a probe's verification. Dropped when the mux or the selection changed
// since the probe started. Returns whether the verification changed.
func (self *dnsResolverStatusTracker) verified(upgradeMux *connect.UpgradeMux, key string, verification string) bool {
	if upgradeMux != self.upgradeMux || key != dnsResolverSelectionKey(self.presetId, self.personalId) {
		return false
	}
	if verification == self.verification {
		return false
	}
	self.verification = verification
	return true
}

// The status, without the blocked count.
func (self *dnsResolverStatusTracker) status() *DnsResolverStatus {
	status := unknownDnsResolverStatus()
	if self.state != "" {
		status.State = self.state
	}
	if self.verification != "" {
		status.Verification = self.verification
	}
	status.PresetId = self.presetId
	if !self.sinceTime.IsZero() {
		status.SinceTime = self.sinceTime.UnixMilli()
	}
	return status
}

// The state of a connect resolver health. A selection without a failover
// never reads failed over.
func dnsResolverStateOf(healthState connect.DnsResolverHealthState, failover bool) string {
	switch healthState {
	case connect.DnsResolverHealthOk:
		return DnsResolverStateOk
	case connect.DnsResolverHealthFailing:
		return DnsResolverStateFailing
	case connect.DnsResolverHealthFailedOver:
		if failover {
			return DnsResolverStateFailedOver
		}
		return DnsResolverStateFailing
	default:
		return DnsResolverStateUnknown
	}
}

// The name a preset is verified with, or empty when it has nothing to
// verify: a personal preset asks any name, a one-tap preset its canary.
func dnsResolverVerifyNameOf(preset *dnsPreset) string {
	if preset == nil {
		return ""
	}
	if preset.personalId != nil {
		return dnsResolverVerifyName
	}
	return preset.canaryName
}

// The verification of a preset from its urls' answers to the probe (C6).
// Only answered results count; with none, verification has nothing to say
// and the health state applies.
func dnsResolverVerificationOf(preset *dnsPreset, results []connect.DnsResolverProbeResult) string {
	if preset == nil {
		return DnsResolverVerificationNone
	}
	answered := []connect.DnsResolverProbeResult{}
	for _, result := range results {
		if result.Answered {
			answered = append(answered, result)
		}
	}
	if len(answered) == 0 {
		return DnsResolverVerificationNone
	}
	if personal := preset.personalId; personal != nil {
		// a personal preset is never confirmed: a wrong id or a spent quota
		// answers unfiltered too
		normal := false
		for _, result := range answered {
			switch result.RCode {
			case dnsmessage.RCodeRefused:
				if personal.unknownIdAnswer == dnsPresetUnknownIdRefused {
					return DnsResolverVerificationRejected
				}
			case dnsmessage.RCodeServerFailure:
				if personal.unknownIdAnswer == dnsPresetUnknownIdUnfilteredOrServfail {
					return DnsResolverVerificationMalformed
				}
			case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
				normal = true
			}
		}
		if normal {
			return DnsResolverVerificationUnconfirmed
		}
		return DnsResolverVerificationNone
	}
	if preset.canaryName == "" {
		return DnsResolverVerificationNone
	}
	// a one-tap preset's canary: any url that resolves it is not filtering,
	// and every answer a block confirms the preset
	blockedCount := 0
	consideredCount := 0
	for _, result := range answered {
		switch result.RCode {
		case dnsmessage.RCodeSuccess:
			consideredCount += 1
			resolved := false
			sinks := 0
			for _, addr := range result.Addrs {
				if dnsResolverSinkAddr(addr) {
					sinks += 1
				} else {
					resolved = true
				}
			}
			if resolved {
				return DnsResolverVerificationNotFiltering
			}
			if 0 < sinks {
				blockedCount += 1
			}
		case dnsmessage.RCodeNameError:
			consideredCount += 1
			for _, edeCode := range result.EdeCodes {
				if containsUint16(dnsResolverBlockEdeCodes, edeCode) {
					blockedCount += 1
					break
				}
			}
		}
	}
	if 0 < blockedCount && blockedCount == consideredCount {
		return DnsResolverVerificationConfirmed
	}
	return DnsResolverVerificationNone
}

func containsUint16(values []uint16, value uint16) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// A preset id as the app log names it: a catalog or reserved id, custom,
// default or a regional pick, never anything else a record may hold.
func dnsPresetIdForLog(presetId string) string {
	switch {
	case presetId == "":
		return "custom"
	case presetId == DnsPresetIdDefault:
		return DnsPresetIdDefault
	case strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix):
		if dnsRegionalCountryCodeValid(strings.TrimPrefix(presetId, DnsPresetIdRegionalPrefix)) {
			return presetId
		}
		return "unknown"
	}
	catalog := currentDnsPresetCatalog()
	if catalog.preset(presetId) != nil {
		return presetId
	}
	if _, reserved := catalog.reservedPresets[presetId]; reserved {
		return presetId
	}
	return "unknown"
}

// GetDnsResolverStatus is the status of the device's dns resolver, with this
// connection's blocked-lookups count. Never nil.
func (self *DeviceLocal) GetDnsResolverStatus() *DnsResolverStatus {
	status := func() *DnsResolverStatus {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.dnsResolver.status()
	}()
	status.BlockedCount = self.GetDnsBlockedCount()
	return status
}

// AddDnsResolverStatusChangeListener subscribes to changes of the status's
// state, verification and selection. The blocked count changes without a
// callback.
func (self *DeviceLocal) AddDnsResolverStatusChangeListener(listener DnsResolverStatusChangeListener) Sub {
	callbackId := self.dnsResolverStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.dnsResolverStatusChangeListeners.Remove(callbackId)
	})
}

// GetDnsBlockedCount is this connection's blocked-lookups count (Device). Zero
// without a mux: before a connect, after a disconnect, and on a hosted device.
func (self *DeviceLocal) GetDnsBlockedCount() int64 {
	blockedCounter := func() dnsBlockedCounter {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.dnsResolver.blockedCounter
	}()
	if blockedCounter == nil {
		return 0
	}
	count := blockedCounter.DnsBlockedCount()
	if math.MaxInt64 < count {
		return math.MaxInt64
	}
	return int64(count)
}

// Logs the status and hands it to the listeners, each its own copy. Called
// with no lock held.
func (self *DeviceLocal) dnsResolverStatusChanged(status *DnsResolverStatus) {
	if status == nil {
		return
	}
	if self.log != nil {
		personal := "no"
		if preset := currentDnsPresetCatalog().preset(status.PresetId); preset != nil && preset.personalId != nil {
			personal = "yes"
		}
		self.log.Infof(
			"[dns]preset=%s personal=%s state=%s verification=%s",
			dnsPresetIdForLog(status.PresetId),
			personal,
			status.State,
			status.Verification,
		)
	}
	if self.dnsResolverStatusChangeListeners == nil {
		return
	}
	for _, listener := range self.dnsResolverStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.DnsResolverStatusChanged(cloneDnsResolverStatus(status))
		})
	}
}

// Follows a new mux, or none: the health callback moves to it and the state
// starts over. The caller publishes the status after unlock.
func (self *DeviceLocal) dnsResolverMuxChangedWithLock(upgradeMux *connect.UpgradeMux) {
	if self.dnsResolver.upgradeMux == upgradeMux {
		return
	}
	if healthSub := self.dnsResolver.healthSub; healthSub != nil {
		self.dnsResolver.healthSub = nil
		healthSub()
	}
	self.dnsResolver.setMux(upgradeMux, time.Now())
	if upgradeMux == nil {
		return
	}
	self.dnsResolver.healthSub = upgradeMux.AddDnsResolverHealthCallback(func(health connect.DnsResolverHealth) {
		self.dnsResolverHealthChanged(upgradeMux, health)
	})
	// subscribed, then read: the health as it stands
	self.dnsResolverHealthChangedWithLock(upgradeMux, upgradeMux.DnsResolverHealth())
}

// The mux's health callback, which connect calls outside its locks on every
// state change.
func (self *DeviceLocal) dnsResolverHealthChanged(upgradeMux *connect.UpgradeMux, health connect.DnsResolverHealth) {
	status := func() *DnsResolverStatus {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if !self.dnsResolverHealthChangedWithLock(upgradeMux, health) {
			return nil
		}
		return self.dnsResolver.status()
	}()
	if status != nil {
		status.BlockedCount = self.GetDnsBlockedCount()
		self.dnsResolverStatusChanged(status)
	}
}

// Takes a health report and starts a due verification probe. Returns whether
// the state changed.
func (self *DeviceLocal) dnsResolverHealthChangedWithLock(upgradeMux *connect.UpgradeMux, health connect.DnsResolverHealth) bool {
	selection := resolveDnsResolverSelection(self.dnsResolver.presetId, self.dnsResolver.personalId)
	changed, probe := self.dnsResolver.healthChanged(
		upgradeMux,
		health,
		selection,
		time.Now(),
		self.settings.DnsResolverVerifyMinInterval,
	)
	if probe && !self.closed {
		self.startDnsResolverVerifyWithLock(upgradeMux, selection)
	}
	return changed
}

// Probes the selected preset through the tunnel in a lifecycle worker and
// publishes the verification when it changed (C6). The probe bypasses the
// answer cache, the reverse index, the server scores, the health and the
// blocked count, and each request is bounded by the device's lifetime and
// the resolver's request timeout.
func (self *DeviceLocal) startDnsResolverVerifyWithLock(upgradeMux *connect.UpgradeMux, selection dnsResolverSelection) {
	name := dnsResolverVerifyNameOf(selection.preset)
	key := dnsResolverSelectionKey(self.dnsResolver.presetId, self.dnsResolver.personalId)
	probe := self.testingDnsResolverProbe
	if probe == nil {
		probe = func(ctx context.Context, upgradeMux *connect.UpgradeMux, name string) []connect.DnsResolverProbeResult {
			return upgradeMux.ProbeDnsResolver(ctx, name)
		}
	}
	ctx := self.ctx
	self.startLifecycleWorkerWithLock(func() {
		verification := dnsResolverVerificationOf(selection.preset, probe(ctx, upgradeMux, name))
		status := func() *DnsResolverStatus {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed || !self.dnsResolver.verified(upgradeMux, key, verification) {
				return nil
			}
			return self.dnsResolver.status()
		}()
		if status != nil {
			status.BlockedCount = self.GetDnsBlockedCount()
			self.dnsResolverStatusChanged(status)
		}
	})
}

// An address the verification reads as a sink: 0.0.0.0 or ::.
func dnsResolverSinkAddr(addr netip.Addr) bool {
	return addr.Unmap().IsUnspecified()
}
