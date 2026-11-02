package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// extender_release.go — the gated release fetch and its refresh schedule
// (connect/EXTENDER-UPDATE2.md §1.1, §1.3; connect/EXTENDER.md R3, R5).
//
// The release route hands the authenticated identity -- the account and the
// device of the client token of the device that dials through the space -- a
// few records of the gated tier, signed fresh at each release, so the client
// refreshes them by asking again. One fetcher serves one space and the one
// identity bound to it (bindExtenderRelease): every trigger -- the schedule, a
// path change, the directory's low watermark and fallback (connect
// ExtenderDirectory.GatedRefreshMonitor), the seed bootstrap's reach
// (requestExtenderRelease) -- lands on the same coalesced request and the same
// retry budget, and a trigger that arrives while a request is out is answered
// by it.
//
// The answer (contract R) is read tolerantly: the records, `outcome` and
// `retry_after_seconds`, an unknown outcome read as empty and the retry time
// clamped to [60 s, 6 h]. Each record is validated and applied by the
// directory with the source release (ApplyReleaseRecord), never published on
// an open channel, and at most ExtenderGatedReleaseMaxRecordCount of them. What
// the request came to names the directory's fallback when nothing usable was
// released (SetGatedReleaseOutcome).
//
// The schedule runs from the earliest expiry of the gated records held, never
// from the seven-day disclosure epoch: the operator signs a record for a day,
// so the normal refresh is six hours after an answer, a jitter drawn once per
// answer brings it earlier and never later, and the refresh is due no later
// than ExpiryMargin before the earliest expiry, which leaves room for bounded
// retries. A failure backs off from a minute to half an hour, until control
// reach is newly established (requestExtenderRelease), which clears the backoff;
// a refusal waits out the retry time the operator gave, or a wait of its own;
// and the client never asks more than RateLimitCount times an hour, nor twice
// within MinRequestInterval of an answer, whatever triggered it.
//
// An account change -- a client token of another network or client, a
// sign-out -- cancels the request that is out and discards its answer, which
// is never applied once the change has been seen: answers are applied under
// the binding lock, which the change takes too. The gated records released to
// the previous identity are forgotten (ExtenderDirectory.ForgetGatedRecords),
// since what the operator disclosed to one identity is not the next one's to
// dial. A directory reset (E7) drops an answer requested before it, by the
// reset generation the request captured.
//
// The fetcher runs only while the gated preference is on for the space, which
// is the cohort control (NetworkSpaceValues.ExtenderGatedPreference), so a
// client outside the cohort spends no disclosure. A hosted space never binds.
// Nothing here logs an address, a token or an id.

const extenderReleasePath = "/network/extender-release"

// The persisted schedule and the identity it belongs to, beside the directory.
const extenderReleaseStateFileName = ".extender_release"

const extenderReleaseStateVersion = 1

// The schedule numbers, all seams so a test pins each transition.
type extenderReleaseSettings struct {
	// The normal refresh, after an answer that released a record.
	RefreshInterval time.Duration
	// The most a refresh is brought earlier by its jitter, drawn once per
	// answer. It never makes one later, and it stays within ExpiryMargin.
	RefreshJitter time.Duration
	// The refresh is due no later than this before the earliest expiry of the
	// gated records held, so the retries it may need fit before they lapse.
	ExpiryMargin time.Duration
	// The deadline of one request.
	RequestTimeout time.Duration
	// The deadline of one control reach (NetworkSpace.obtainExtenderControlReach).
	ControlReachTimeout time.Duration
	// No two requests within this of an answer, and no more than
	// RateLimitCount within RateLimitWindow, whatever triggered them.
	MinRequestInterval time.Duration
	RateLimitCount     int
	RateLimitWindow    time.Duration
	// The failure backoff, doubling.
	RetryMinBackoff time.Duration
	RetryMaxBackoff time.Duration
	// The bounds on a retry time the operator gives (contract R).
	MinRetryAfter time.Duration
	MaxRetryAfter time.Duration
	// The wait after an answer that released nothing and gave no retry time
	// (empty, country_capacity, incompatible): doubling from the minimum per
	// answer in a row, to the refresh interval.
	NoRecordMinWait time.Duration

	// The only clock the fetcher reads.
	Now func() time.Time
	// The wait between two looks. Tests read the wait the loop chose through
	// it and hold the loop on it.
	After func(wait time.Duration) <-chan time.Time
	// The release request: the space's api transport.
	Post connect.HttpPostRawFunction
	// A uniform [0, 1) for the jitter.
	Random func() float64
	// The families the client dials extenders of (connect
	// ExtenderDialIpVersions), named in the request.
	IpVersions func() []int
	// The capabilities named in the request (extenderReleaseCapabilities).
	Capabilities func() []string
}

func defaultExtenderReleaseSettings() *extenderReleaseSettings {
	return &extenderReleaseSettings{
		RefreshInterval:     6 * time.Hour,
		RefreshJitter:       30 * time.Minute,
		ExpiryMargin:        1 * time.Hour,
		RequestTimeout:      30 * time.Second,
		ControlReachTimeout: 60 * time.Second,
		MinRequestInterval:  60 * time.Second,
		RateLimitCount:      4,
		RateLimitWindow:     time.Hour,
		RetryMinBackoff:     time.Minute,
		RetryMaxBackoff:     30 * time.Minute,
		MinRetryAfter:       60 * time.Second,
		MaxRetryAfter:       6 * time.Hour,
		NoRecordMinWait:     time.Hour,
		Now:                 time.Now,
		After:               time.After,
		Random:              func() float64 { return 0 },
		IpVersions:          connect.ExtenderDialIpVersions,
		Capabilities:        func() []string { return nil },
	}
}

// The capabilities this client names in a release request (contract R): the
// carriers it can use, which change the operator's eligibility and never the
// disclosure budget. `countryCode` is the directory's SpoofCountryCode, ""
// for the global list. The decision is connect's
// ExtenderClientCapabilities (EXTENDER-UPDATE2 §3.2), which names none while the
// bundled borrow list is an empty placeholder.
func extenderReleaseCapabilities(connectSettings *connect.ConnectSettings, countryCode string) []string {
	return connect.ExtenderClientCapabilities(connectSettings, countryCode)
}

// The request (contract R). An empty `ip_versions` is any family.
type extenderReleaseArgs struct {
	IpVersions   []int    `json:"ip_versions"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// One answer as the fetcher reads it.
type extenderReleaseAnswer struct {
	records []string
	// normalized: ok, a non-ok outcome of contract R, or empty for one this
	// client does not know
	outcome string
	// the operator's retry time, clamped; zero for none
	retryAfter time.Duration
}

// The kinds of request that got no answer to read, a fixed label set.
const (
	extenderReleaseFailureTransport = "transport"
	extenderReleaseFailureTimeout   = "timeout"
	extenderReleaseFailureStatus    = "status"
	extenderReleaseFailureMalformed = "malformed"
)

// What asked for a request out of schedule, a fixed label set: the seed
// bootstrap or an import that regained reach (requestExtenderRelease), a path
// change, or the directory's low watermark or fallback (connect
// ExtenderDirectory.GatedRefreshMonitor).
const (
	extenderReleaseTriggerRequest   = "request"
	extenderReleaseTriggerPath      = "path"
	extenderReleaseTriggerDirectory = "directory"
)

// How a refresh landed against the records it replaced (§1.3), a fixed label
// set: before the refresh was due by their expiry, after it but before they
// expired, or after they had expired -- a gap in gated coverage.
const (
	extenderReleaseLatenessOnTime  = "on_time"
	extenderReleaseLatenessLate    = "late"
	extenderReleaseLatenessExpired = "expired"
)

// Reads one answer of contract R tolerantly: each field on its own, a field
// that does not parse read as absent, and a body that is not an object
// unreadable. A legacy answer, with no outcome, is ok when it carries no
// refusal and unavailable when it does.
func parseExtenderReleaseAnswer(body []byte, settings *extenderReleaseSettings) (*extenderReleaseAnswer, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("the release answer is not an object")
	}
	answer := &extenderReleaseAnswer{}
	if raw, ok := fields["records"]; ok {
		records := []string{}
		if json.Unmarshal(raw, &records) == nil {
			answer.records = records
		}
	}
	outcome := ""
	if raw, ok := fields["outcome"]; ok {
		json.Unmarshal(raw, &outcome)
	}
	refusal := ""
	if raw, ok := fields["error"]; ok {
		json.Unmarshal(raw, &refusal)
	}
	switch {
	case outcome != "":
		answer.outcome = connect.NormalizeExtenderGatedReleaseOutcome(outcome)
	case refusal != "":
		answer.outcome = connect.ExtenderReleaseOutcomeUnavailable
	default:
		answer.outcome = connect.ExtenderReleaseOutcomeOk
	}
	if raw, ok := fields["retry_after_seconds"]; ok {
		var seconds float64
		if json.Unmarshal(raw, &seconds) == nil && 0 < seconds && !math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
			retryAfter := settings.MaxRetryAfter
			if seconds < settings.MaxRetryAfter.Seconds() {
				retryAfter = time.Duration(seconds * float64(time.Second))
			}
			answer.retryAfter = min(max(retryAfter, settings.MinRetryAfter), settings.MaxRetryAfter)
		}
	}
	return answer, nil
}

// The identity a release is keyed by: the network and the client of a client
// token, read without verification -- it is this client's own token, and the
// operator verifies it.
type extenderReleaseIdentity struct {
	networkId connect.Id
	clientId  connect.Id
}

func parseExtenderReleaseIdentity(byJwt string) extenderReleaseIdentity {
	if byJwt == "" {
		return extenderReleaseIdentity{}
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return extenderReleaseIdentity{}
	}
	parse := func(name string) (connect.Id, bool) {
		text, ok := claims[name].(string)
		if !ok {
			return connect.Id{}, false
		}
		id, err := connect.ParseId(text)
		return id, err == nil
	}
	networkId, networkOk := parse("network_id")
	clientId, clientOk := parse("client_id")
	if !networkOk || !clientOk {
		return extenderReleaseIdentity{}
	}
	return extenderReleaseIdentity{networkId: networkId, clientId: clientId}
}

func (self extenderReleaseIdentity) empty() bool {
	return self == extenderReleaseIdentity{}
}

// A digest of the identity, the form the schedule is stored under: it says
// whether the stored schedule and the gated records are this identity's, and
// nothing more.
func (self extenderReleaseIdentity) digest() string {
	if self.empty() {
		return ""
	}
	sum := sha256.New()
	sum.Write([]byte("ur-extender-release-identity-v1"))
	sum.Write(self.networkId.Bytes())
	sum.Write(self.clientId.Bytes())
	return hex.EncodeToString(sum.Sum(nil))
}

// The schedule of one identity's requests. Guarded by the fetcher's state
// lock.
type extenderReleaseSchedule struct {
	// when the last request ended, answered or failed, zero for none
	lastTime time.Time
	// the wait the last end chose: the refresh interval after an answer that
	// released a record, the operator's or its own wait after another, the
	// backoff after a failure
	wait time.Duration
	// how much earlier the jitter brings the refresh, drawn at each answer
	jitter time.Duration
	// no request before this: a retry time, a backoff, an answer's interval
	notBefore time.Time
	// the failure backoff, zero after an answer
	backoff time.Duration
	// answers in a row that released nothing and gave no retry time
	noRecordCount int
	// whether the last answer released a usable record, and the earliest
	// expiry of what it released, which the next refresh is late against
	released           bool
	releasedExpireTime time.Time
	// the requests in the rate window, oldest first, answered or not
	requestTimes []time.Time
	// a trigger arrived and the request it asks for is due once the limits
	// allow it
	early bool
	// a control reach made something dialable after a failure: the next
	// request goes at once, MinRequestInterval aside, once per failure run
	reachRetry bool
	reachTried bool
	// the last request ended with no answer to read, so `backoff` and
	// `notBefore` are about the route it took, not an answer of the operator's
	unanswered bool
	// control reach was established while a request was out, or while the walk
	// after it ran; if that request failed, its failure was the old route's
	reachEstablishedOut bool
	// the families the last request named
	ipVersions []int
}

// When the next request is due at `now`, given the gated records held. Never
// before a retry time or a backoff, and never past the client's rate limit.
func (self *extenderReleaseSchedule) dueTime(
	now time.Time,
	summary connect.ExtenderGatedRecordSummary,
	settings *extenderReleaseSettings,
) time.Time {
	due := now
	if !self.lastTime.IsZero() && !self.early {
		due = self.lastTime.Add(self.wait)
		switch {
		case 0 < summary.CurrentCount && !summary.EarliestExpireTime.IsZero():
			if deadline := summary.EarliestExpireTime.Add(-settings.ExpiryMargin); deadline.Before(due) {
				due = deadline
			}
		case self.released && summary.CurrentCount == 0:
			// what was released is gone -- expired, revoked, reset, forgotten:
			// the low watermark
			due = now
		}
		if self.released {
			due = due.Add(-self.jitter)
		}
	}
	if due.Before(self.notBefore) {
		due = self.notBefore
	}
	if limitedUntil := self.limitedUntil(now, settings); due.Before(limitedUntil) {
		due = limitedUntil
	}
	return due
}

// The earliest time the rate limit allows another request.
func (self *extenderReleaseSchedule) limitedUntil(now time.Time, settings *extenderReleaseSettings) time.Time {
	self.requestTimes = slices.DeleteFunc(self.requestTimes, func(requestTime time.Time) bool {
		return !now.Before(requestTime.Add(settings.RateLimitWindow))
	})
	var limitedUntil time.Time
	if 0 < settings.RateLimitCount && settings.RateLimitCount <= len(self.requestTimes) {
		limitedUntil = self.requestTimes[len(self.requestTimes)-settings.RateLimitCount].Add(settings.RateLimitWindow)
	}
	if !self.reachRetry && 0 < len(self.requestTimes) && !self.lastTime.IsZero() && self.backoff == 0 {
		// after an answer; a failure is spaced by its backoff instead
		if minTime := self.lastTime.Add(settings.MinRequestInterval); limitedUntil.Before(minTime) {
			limitedUntil = minTime
		}
	}
	return limitedUntil
}

func (self *extenderReleaseSchedule) requested(now time.Time, ipVersions []int) {
	self.requestTimes = append(self.requestTimes, now)
	self.ipVersions = slices.Clone(ipVersions)
	self.early = false
	self.reachRetry = false
}

// An answer at `now`. `released` is whether it applied a usable record.
func (self *extenderReleaseSchedule) answered(
	now time.Time,
	outcome string,
	released bool,
	releasedExpireTime time.Time,
	retryAfter time.Duration,
	settings *extenderReleaseSettings,
) {
	self.lastTime = now
	self.backoff = 0
	self.reachTried = false
	self.unanswered = false
	self.reachEstablishedOut = false
	self.released = released
	self.jitter = 0
	switch {
	case released:
		self.releasedExpireTime = releasedExpireTime
		self.noRecordCount = 0
		self.wait = settings.RefreshInterval
		jitter := min(settings.RefreshJitter, settings.ExpiryMargin/2)
		self.jitter = time.Duration(settings.Random() * float64(jitter))
		self.notBefore = now.Add(retryAfter)
	case 0 < retryAfter:
		self.wait = retryAfter
		self.notBefore = now.Add(retryAfter)
	case outcome == connect.ExtenderReleaseOutcomeRateLimited:
		self.wait = settings.RateLimitWindow
		self.notBefore = now.Add(self.wait)
	case outcome == connect.ExtenderReleaseOutcomeUnavailable:
		// the operator could not answer, which may pass soon: a failure's
		// backoff, without its control reach
		self.failedBackoff(settings)
		self.wait = self.backoff
		self.notBefore = now.Add(self.wait)
	default:
		self.noRecordCount += 1
		wait := settings.NoRecordMinWait
		for i := 1; i < self.noRecordCount && wait < settings.RefreshInterval; i += 1 {
			wait *= 2
		}
		self.wait = min(wait, settings.RefreshInterval)
		self.notBefore = now.Add(self.wait)
	}
}

// A request that got no answer at `now`. The records held are not lost by it.
func (self *extenderReleaseSchedule) failed(now time.Time, settings *extenderReleaseSettings) {
	self.lastTime = now
	self.unanswered = true
	self.failedBackoff(settings)
	self.wait = self.backoff
	self.notBefore = now.Add(self.backoff)
}

// Control reach was newly established at `now` by something other than this
// fetch's own walk: the seed bootstrap's trigger, or an import. A backoff from a
// request with no answer was about the route that failed, so it no longer holds
// the next request, and a later failure starts a new run with a walk of its own.
// With no backoff the limits are the rate limit: the hourly cap and
// MinRequestInterval from the last request's end (limitedUntil). What an answer
// asked for, a retry time above all, is the operator's and stands.
func (self *extenderReleaseSchedule) reachEstablished(now time.Time) {
	if !self.unanswered {
		return
	}
	self.unanswered = false
	self.backoff = 0
	self.wait = 0
	self.notBefore = now
	self.reachTried = false
}

// A fetch ended, having coalesced reach established while it ran. A request that
// failed began on the route before that reach, so the next one goes at once
// within the hourly cap, as after the fetch's own walk found reach. A trigger that
// landed after an answer is an ordinary trigger, under every limit.
func (self *extenderReleaseSchedule) fetchEnded(now time.Time) {
	if !self.reachEstablishedOut {
		return
	}
	self.reachEstablishedOut = false
	self.early = true
	if self.unanswered {
		self.reachEstablished(now)
		self.reachRetry = true
	}
}

func (self *extenderReleaseSchedule) failedBackoff(settings *extenderReleaseSettings) {
	if self.backoff <= 0 {
		self.backoff = settings.RetryMinBackoff
	} else {
		self.backoff = min(2*self.backoff, settings.RetryMaxBackoff)
	}
}

// The fetcher's counts, all over fixed label sets.
type extenderReleaseStats struct {
	RequestCount uint64
	// answers by outcome, and `failed` for a request with none to read
	OutcomeCounts map[string]uint64
	// requests with no answer, by kind
	FailureCounts      map[string]uint64
	AppliedRecordCount uint64
	InvalidRecordCount uint64
	// answers an account change or a reset discarded unapplied
	DiscardedCount uint64
	// refreshes that replaced held records, by how late they landed (§1.3),
	// and the latest one past its due time
	LatenessCounts map[string]uint64
	MaxLateness    time.Duration
	// control reaches that made something dialable after a failure
	ControlReachCount uint64
	// triggers that asked for a request out of schedule, by what asked;
	// several may share one request
	TriggerCounts map[string]uint64
}

func (self *extenderReleaseStats) clone() extenderReleaseStats {
	copied := *self
	copied.OutcomeCounts = cloneExtenderReleaseCounts(self.OutcomeCounts)
	copied.FailureCounts = cloneExtenderReleaseCounts(self.FailureCounts)
	copied.LatenessCounts = cloneExtenderReleaseCounts(self.LatenessCounts)
	copied.TriggerCounts = cloneExtenderReleaseCounts(self.TriggerCounts)
	return copied
}

func cloneExtenderReleaseCounts(counts map[string]uint64) map[string]uint64 {
	copied := map[string]uint64{}
	for label, count := range counts {
		copied[label] = count
	}
	return copied
}

func addExtenderReleaseCount(counts *map[string]uint64, label string) {
	if *counts == nil {
		*counts = map[string]uint64{}
	}
	(*counts)[label] += 1
}

// Where the schedule is kept: the space's local state, or nowhere.
type extenderReleaseStateStore interface {
	load() ([]byte, error)
	save([]byte) error
}

type localStateExtenderReleaseStore struct {
	localStorageDir string
}

func (self *localStateExtenderReleaseStore) load() ([]byte, error) {
	return os.ReadFile(filepath.Join(self.localStorageDir, extenderReleaseStateFileName))
}

// Writes the whole state or none of it: a staged file, synced, renamed over
// the last one. A torn write would read as no state, which forgets the
// identity's records and asks again.
func (self *localStateExtenderReleaseStore) save(stateBytes []byte) error {
	// CreateTemp makes the file 0600, LocalStorageFilePermissions
	temp, err := os.CreateTemp(self.localStorageDir, extenderReleaseStateFileName+".*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	// nothing to remove once the rename has committed
	defer os.Remove(tempPath)
	if _, err := temp.Write(stateBytes); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, filepath.Join(self.localStorageDir, extenderReleaseStateFileName))
}

// The stored schedule, times in unix milliseconds.
type extenderReleaseStoredState struct {
	Version          int     `json:"version"`
	IdentityDigest   string  `json:"identity"`
	LastTimeMs       int64   `json:"last_time_ms,omitempty"`
	WaitMs           int64   `json:"wait_ms,omitempty"`
	JitterMs         int64   `json:"jitter_ms,omitempty"`
	NotBeforeMs      int64   `json:"not_before_ms,omitempty"`
	BackoffMs        int64   `json:"backoff_ms,omitempty"`
	Unanswered       bool    `json:"unanswered,omitempty"`
	NoRecordCount    int     `json:"no_record_count,omitempty"`
	Released         bool    `json:"released,omitempty"`
	ReleasedExpireMs int64   `json:"released_expire_ms,omitempty"`
	RequestTimesMs   []int64 `json:"request_times_ms,omitempty"`
	IpVersions       []int   `json:"ip_versions,omitempty"`
	OwnsGatedRecord  bool    `json:"owns_gated_records,omitempty"`
}

// The fetcher of one space.
type extenderReleaseFetcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	log    connect.Logger

	settings  *extenderReleaseSettings
	directory *connect.ExtenderDirectory
	apiUrl    string
	// regains control reach after a request that reached nothing
	// (NetworkSpace.obtainExtenderControlReach)
	controlReach func(ctx context.Context) bool
	// nil keeps the schedule in memory
	store extenderReleaseStateStore

	wake *connect.Monitor
	// advances on every account change of the bound identity
	accountMonitor     *connect.MonitorValue[uint64]
	unsubNetworkChange func()

	// Orders an account change against applying an answer: both run under
	// it, so an answer is applied before the change or not at all. Taken
	// before the state lock, never inside it, and never across a request.
	bindingLock sync.Mutex

	stateLock sync.Mutex
	enabled   bool
	// the bound device's client token source, nil while none is bound
	credential func() string
	bindToken  uint64
	identity   extenderReleaseIdentity
	// advances with every account change; a request carries the one it
	// started in
	identityGeneration uint64
	// whether the gated records the directory holds were released to the
	// bound identity, which is what an account change forgets
	ownsGatedRecords bool
	// the identity the stored schedule is for, read once
	storedState  *extenderReleaseStoredState
	storedLoaded bool
	// the request that is out, nil while none is
	requestCancel context.CancelFunc
	schedule      extenderReleaseSchedule
	stats         extenderReleaseStats
}

func newExtenderReleaseFetcher(
	ctx context.Context,
	directory *connect.ExtenderDirectory,
	apiUrl string,
	controlReach func(ctx context.Context) bool,
	accountMonitor *connect.MonitorValue[uint64],
	store extenderReleaseStateStore,
	settings *extenderReleaseSettings,
	log connect.Logger,
) *extenderReleaseFetcher {
	if settings == nil {
		settings = defaultExtenderReleaseSettings()
	}
	if log == nil {
		log = connect.DefaultLogger()
	}
	if accountMonitor == nil {
		accountMonitor = connect.NewMonitorValue[uint64](0)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &extenderReleaseFetcher{
		ctx:            cancelCtx,
		cancel:         cancel,
		done:           make(chan struct{}),
		log:            log,
		settings:       settings,
		directory:      directory,
		apiUrl:         apiUrl,
		controlReach:   controlReach,
		store:          store,
		wake:           connect.NewMonitor(),
		accountMonitor: accountMonitor,
	}
	// a path change may change the families the client can dial, which is
	// what the request names
	self.unsubNetworkChange = connect.AddNetworkChangeListener(self.networkChanged)
	go connect.HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// Ends the loop and joins it. A request that is out is canceled.
func (self *extenderReleaseFetcher) Close() {
	if self.unsubNetworkChange != nil {
		self.unsubNetworkChange()
	}
	self.cancel()
	<-self.done
}

// Turns the fetch on or off with the gated preference (the cohort control).
func (self *extenderReleaseFetcher) setEnabled(enabled bool) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.enabled == enabled {
			return false
		}
		self.enabled = enabled
		if !enabled && self.requestCancel != nil {
			self.requestCancel()
		}
		return true
	}()
	if changed {
		self.wake.NotifyAll()
	}
}

// Binds the client token source of the device that dials through the space.
// The latest bind wins; its unbind is a no-op once another bind replaced it.
func (self *extenderReleaseFetcher) bind(credential func() string) (unbind func()) {
	if credential == nil {
		return func() {}
	}
	token := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.bindToken += 1
		self.credential = credential
		return self.bindToken
	}()
	self.credentialChanged()
	return func() {
		unbound := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.bindToken != token {
				return false
			}
			self.credential = nil
			return true
		}()
		if unbound {
			self.credentialChanged()
		}
	}
}

// The bound credential changed, or may have: a token installed, refreshed or
// rejected. A token of another identity, or none, is an account change.
func (self *extenderReleaseFetcher) credentialChanged() {
	self.bindingLock.Lock()
	defer self.bindingLock.Unlock()

	credential := func() func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.credential
	}()
	identity := extenderReleaseIdentity{}
	if credential != nil {
		identity = parseExtenderReleaseIdentity(credential())
	}

	storedState := self.loadStoredState()
	forget := false
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if identity == self.identity {
			return false
		}
		previous := self.identity
		self.identity = identity
		self.identityGeneration += 1
		if self.requestCancel != nil {
			self.requestCancel()
		}
		if identity.empty() {
			// signed out or unbound: nothing is asked for, and what the last
			// identity was released stays until another identity binds
			return true
		}
		if previous.empty() && storedState != nil && storedState.IdentityDigest == identity.digest() {
			// the identity the stored schedule is for, after a restart or a
			// rebind: its schedule and its records carry on
			self.schedule = restoreExtenderReleaseSchedule(storedState)
			self.ownsGatedRecords = storedState.OwnsGatedRecord
			return true
		}
		// another identity: a fresh schedule, due now, and the gated records
		// the directory holds are not its own
		forget = self.enabled || self.ownsGatedRecords ||
			(storedState != nil && storedState.OwnsGatedRecord)
		self.schedule = extenderReleaseSchedule{}
		self.ownsGatedRecords = false
		return true
	}()
	if !changed {
		return
	}
	if forget {
		if count := self.directory.ForgetGatedRecords(); 0 < count {
			self.log.Infof("[extender]release account changed; %d gated records forgotten\n", count)
		}
	}
	self.accountMonitor.Update(func(generation uint64) uint64 {
		return generation + 1
	})
	self.saveState()
	self.wake.NotifyAll()
}

// Asks for a request once the limits allow it: the seed bootstrap's reach, a
// path change, the directory's low watermark or fallback (`source`). A
// request that is out answers it. Nothing is asked while the fetch is off or
// no identity is bound.
func (self *extenderReleaseFetcher) trigger(source string) {
	triggered := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.enabled || self.identity.empty() || self.credential == nil {
			return false
		}
		addExtenderReleaseCount(&self.stats.TriggerCounts, source)
		if self.requestCancel != nil {
			// coalesced into the request that is out
			if source == extenderReleaseTriggerRequest {
				self.schedule.reachEstablishedOut = true
			}
			return false
		}
		if source == extenderReleaseTriggerRequest {
			self.schedule.reachEstablished(self.settings.Now())
		}
		self.schedule.early = true
		return true
	}()
	if triggered {
		self.wake.NotifyAll()
	}
}

// A path change asks for a release when the families the client dials
// changed since the last request, or when no gated record is usable now.
func (self *extenderReleaseFetcher) networkChanged() {
	ipVersions := self.settings.IpVersions()
	summary := self.directory.GatedRecordSummary()
	wanted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return !slices.Equal(ipVersions, self.schedule.ipVersions) || summary.UsableCount == 0
	}()
	if wanted {
		self.trigger(extenderReleaseTriggerPath)
	}
}

func (self *extenderReleaseFetcher) statsValue() extenderReleaseStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stats.clone()
}

// The loop: look whether a request is due, make it, or wait for the earliest
// of the due time, a wake and the directory's refresh ask.
func (self *extenderReleaseFetcher) run() {
	for {
		// subscribe before the look, so a wake that lands during it is carried
		// into the wait
		wake := self.wake.NotifyChannel()
		refreshWanted := self.directory.GatedRefreshMonitor().NotifyChannel()

		wait, due, active := self.next()
		if due {
			self.fetch()
			continue
		}
		var after <-chan time.Time
		if active {
			after = self.settings.After(wait)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-refreshWanted:
			self.trigger(extenderReleaseTriggerDirectory)
		case <-after:
		}
	}
}

// Whether a request is due now, and else the wait until it is; inactive while
// the fetch is off or no identity is bound.
func (self *extenderReleaseFetcher) next() (wait time.Duration, due bool, active bool) {
	summary := self.directory.GatedRecordSummary()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if !self.enabled || self.identity.empty() || self.credential == nil {
		return 0, false, false
	}
	dueTime := self.schedule.dueTime(now, summary, self.settings)
	if !now.Before(dueTime) {
		return 0, true, true
	}
	return dueTime.Sub(now), false, true
}

// One request and what it came to.
func (self *extenderReleaseFetcher) fetch() {
	ipVersions := self.settings.IpVersions()
	capabilities := self.settings.Capabilities()
	resetGeneration := self.directory.ResetGeneration()

	credential, identity := func() (func() string, extenderReleaseIdentity) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.credential, self.identity
	}()
	if credential == nil || identity.empty() {
		return
	}
	// the token is read with no lock held, and a token of another identity is
	// an account change the binding has not seen yet: it is seen now, and the
	// next look asks for the identity it brings
	byJwt := credential()
	if parseExtenderReleaseIdentity(byJwt) != identity {
		self.credentialChanged()
		return
	}
	argsBytes, err := json.Marshal(&extenderReleaseArgs{
		IpVersions:   ipVersions,
		Capabilities: capabilities,
	})
	if err != nil {
		return
	}

	requestCtx, requestCancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer requestCancel()
	identityGeneration, ok := func() (uint64, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.enabled || self.identity != identity {
			return 0, false
		}
		self.requestCancel = requestCancel
		self.schedule.requested(self.settings.Now(), ipVersions)
		self.stats.RequestCount += 1
		return self.identityGeneration, true
	}()
	if !ok {
		return
	}
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requestCancel = nil
		self.schedule.fetchEnded(self.settings.Now())
	}()

	responseBytes, requestErr := self.settings.Post(requestCtx, self.apiUrl+extenderReleasePath, argsBytes, byJwt)

	var answer *extenderReleaseAnswer
	failure := ""
	switch {
	case requestErr == nil:
		answer, err = parseExtenderReleaseAnswer(responseBytes, self.settings)
		if err != nil {
			failure = extenderReleaseFailureMalformed
		}
	case self.ctx.Err() != nil:
		// closing
		return
	default:
		var statusErr *connect.HttpStatusError
		switch {
		case errors.As(requestErr, &statusErr):
			failure = extenderReleaseFailureStatus
		case errors.Is(requestCtx.Err(), context.DeadlineExceeded):
			failure = extenderReleaseFailureTimeout
		case requestCtx.Err() != nil:
			// canceled by an account change or a disable: nothing to judge
			failure = ""
		default:
			failure = extenderReleaseFailureTransport
		}
		if failure == "" {
			self.discarded()
			return
		}
	}

	reach := self.land(identity, identityGeneration, resetGeneration, answer, failure)
	if reach {
		self.obtainControlReach()
	}
}

// Applies one answer, or the failure of a request, unless an account change
// has been seen since the request began. Reports whether a control reach
// should be tried: the request reached nothing and none was tried in this run
// of failures.
func (self *extenderReleaseFetcher) land(
	identity extenderReleaseIdentity,
	identityGeneration uint64,
	resetGeneration uint64,
	answer *extenderReleaseAnswer,
	failure string,
) (reach bool) {
	self.bindingLock.Lock()
	defer self.bindingLock.Unlock()

	current := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.identityGeneration == identityGeneration && self.identity == identity && self.enabled
	}()
	if !current {
		self.discarded()
		return false
	}
	now := self.settings.Now()

	if failure != "" {
		self.directory.SetGatedReleaseOutcome(connect.ExtenderGatedReleaseFailed)
		reach = func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			addExtenderReleaseCount(&self.stats.OutcomeCounts, connect.ExtenderGatedReleaseFailed)
			addExtenderReleaseCount(&self.stats.FailureCounts, failure)
			self.schedule.failed(now, self.settings)
			// with reach established meanwhile there is nothing for a walk to find
			reach := !self.schedule.reachEstablishedOut &&
				(failure == extenderReleaseFailureTransport || failure == extenderReleaseFailureTimeout) &&
				!self.schedule.reachTried
			if reach {
				self.schedule.reachTried = true
			}
			return reach
		}()
		if self.log.V(1).Enabled() {
			self.log.Infof("[extender]release %s\n", failure)
		}
		self.saveState()
		return reach
	}

	applied := 0
	invalid := 0
	discarded := false
	for i, recordBase64 := range answer.records {
		if connect.ExtenderGatedReleaseMaxRecordCount <= i {
			invalid += len(answer.records) - i
			break
		}
		record, err := connect.DecodeExtenderRecordBase64(recordBase64)
		if err != nil {
			invalid += 1
			continue
		}
		if _, err := self.directory.ApplyReleaseRecord(record, resetGeneration); err != nil {
			if resetGeneration != self.directory.ResetGeneration() {
				// a reset (E7) landed while the request was out
				discarded = true
				break
			}
			invalid += 1
			continue
		}
		applied += 1
	}
	if discarded {
		self.discarded()
		return false
	}
	summary := self.directory.GatedRecordSummary()
	released := 0 < applied && 0 < summary.CurrentCount
	outcome := answer.outcome
	if outcome == connect.ExtenderReleaseOutcomeOk && !released {
		// a route that answered ok with nothing usable is empty to the policy
		outcome = connect.ExtenderReleaseOutcomeEmpty
	}
	self.directory.SetGatedReleaseOutcome(outcome)

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		addExtenderReleaseCount(&self.stats.OutcomeCounts, outcome)
		self.stats.AppliedRecordCount += uint64(applied)
		self.stats.InvalidRecordCount += uint64(invalid)
		if released {
			self.ownsGatedRecords = true
			// a refresh of what the previous answer released is late against
			// its expiry: due by the margin before it, a gap after it
			if previousExpireTime := self.schedule.releasedExpireTime; self.schedule.released && !previousExpireTime.IsZero() {
				deadline := previousExpireTime.Add(-self.settings.ExpiryMargin)
				switch {
				case !now.After(deadline):
					addExtenderReleaseCount(&self.stats.LatenessCounts, extenderReleaseLatenessOnTime)
				case !now.After(previousExpireTime):
					addExtenderReleaseCount(&self.stats.LatenessCounts, extenderReleaseLatenessLate)
				default:
					addExtenderReleaseCount(&self.stats.LatenessCounts, extenderReleaseLatenessExpired)
				}
				if lateness := now.Sub(deadline); self.stats.MaxLateness < lateness {
					self.stats.MaxLateness = lateness
				}
			}
		}
		self.schedule.answered(now, outcome, released, summary.EarliestExpireTime, answer.retryAfter, self.settings)
	}()
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]release %s, %d applied, %d invalid\n", outcome, applied, invalid)
	}
	self.saveState()
	return false
}

func (self *extenderReleaseFetcher) discarded() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stats.DiscardedCount += 1
}

// Tries the space's reaches after a request that reached nothing (§1.1 rule
// 4). When one made something dialable the next request goes at once, within
// the hourly limit.
func (self *extenderReleaseFetcher) obtainControlReach() {
	if self.controlReach == nil {
		return
	}
	reachCtx, reachCancel := context.WithTimeout(self.ctx, self.settings.ControlReachTimeout)
	defer reachCancel()
	if !self.controlReach(reachCtx) {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.stats.ControlReachCount += 1
		self.schedule.notBefore = self.settings.Now()
		self.schedule.early = true
		self.schedule.reachRetry = true
	}()
	self.wake.NotifyAll()
}

// The stored schedule, read once.
func (self *extenderReleaseFetcher) loadStoredState() *extenderReleaseStoredState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.storedLoaded {
		return self.storedState
	}
	self.storedLoaded = true
	if self.store == nil {
		return nil
	}
	stateBytes, err := self.store.load()
	if err != nil || len(stateBytes) == 0 {
		return nil
	}
	storedState := &extenderReleaseStoredState{}
	if err := json.Unmarshal(stateBytes, storedState); err != nil || storedState.Version != extenderReleaseStateVersion {
		return nil
	}
	self.storedState = storedState
	return storedState
}

// Writes the schedule of the bound identity. Called under the binding lock.
func (self *extenderReleaseFetcher) saveState() {
	if self.store == nil {
		return
	}
	stateBytes, ok := func() ([]byte, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.identity.empty() {
			return nil, false
		}
		storedState := storedExtenderReleaseSchedule(&self.schedule)
		storedState.IdentityDigest = self.identity.digest()
		storedState.OwnsGatedRecord = self.ownsGatedRecords
		self.storedState = storedState
		self.storedLoaded = true
		stateBytes, err := json.Marshal(storedState)
		return stateBytes, err == nil
	}()
	if !ok {
		return
	}
	if err := self.store.save(stateBytes); err != nil {
		self.log.Infof("[extender]release state err = %s\n", err)
	}
}

func storedExtenderReleaseSchedule(schedule *extenderReleaseSchedule) *extenderReleaseStoredState {
	storedState := &extenderReleaseStoredState{
		Version:       extenderReleaseStateVersion,
		LastTimeMs:    extenderReleaseTimeMs(schedule.lastTime),
		WaitMs:        schedule.wait.Milliseconds(),
		JitterMs:      schedule.jitter.Milliseconds(),
		NotBeforeMs:   extenderReleaseTimeMs(schedule.notBefore),
		BackoffMs:     schedule.backoff.Milliseconds(),
		Unanswered:    schedule.unanswered,
		NoRecordCount: schedule.noRecordCount,
		Released:      schedule.released,
		IpVersions:    slices.Clone(schedule.ipVersions),
	}
	storedState.ReleasedExpireMs = extenderReleaseTimeMs(schedule.releasedExpireTime)
	for _, requestTime := range schedule.requestTimes {
		storedState.RequestTimesMs = append(storedState.RequestTimesMs, extenderReleaseTimeMs(requestTime))
	}
	return storedState
}

func restoreExtenderReleaseSchedule(storedState *extenderReleaseStoredState) extenderReleaseSchedule {
	schedule := extenderReleaseSchedule{
		lastTime:      extenderReleaseTimeFromMs(storedState.LastTimeMs),
		wait:          time.Duration(storedState.WaitMs) * time.Millisecond,
		jitter:        time.Duration(storedState.JitterMs) * time.Millisecond,
		notBefore:     extenderReleaseTimeFromMs(storedState.NotBeforeMs),
		backoff:       time.Duration(storedState.BackoffMs) * time.Millisecond,
		unanswered:    storedState.Unanswered,
		noRecordCount: storedState.NoRecordCount,
		released:      storedState.Released,
		ipVersions:    slices.Clone(storedState.IpVersions),
	}
	schedule.releasedExpireTime = extenderReleaseTimeFromMs(storedState.ReleasedExpireMs)
	for _, requestTimeMs := range storedState.RequestTimesMs {
		schedule.requestTimes = append(schedule.requestTimes, extenderReleaseTimeFromMs(requestTimeMs))
	}
	return schedule
}

func extenderReleaseTimeMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func extenderReleaseTimeFromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
