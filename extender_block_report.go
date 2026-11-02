package sdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// extender_block_report.go — the sender of the extender block report
// (connect/EXTENDER-UPDATE2.md §1.3; connect/EXTENDER.md R4).
//
// A record is skipped in a country's releases only when enough clients there
// reported it while the operator's own probe reaches it from outside. This is
// the reporting half, and it is conservative. The rule of what counts is
// connect's (net_extender_directory_gated_block.go): a failed dial of a record
// the client was released or uses arms it as a suspect, and only attempts count.
// An attempt dials every carrier of every address the record names in the
// client's families, tcp among them, and all of them must fail, with the control
// route answering both before and after. It takes three attempts half an hour
// apart, so at least an hour, on one network path, with nothing answering
// meanwhile.
//
// The report names no carrier, and that is settled here rather than on the wire.
// The operator's probe dials tcp alone, so a report goes only after every carrier
// failed, tcp included: tcp failing for the client while the probe's tcp answers
// is exactly the comparison the blocked state makes, and a client that has any
// carrier left does not report at all.
//
// Why attempts rather than the dials a client makes anyway. Once a gated record
// fails, the client falls back to an open one, and with a working dialer the
// strategy has no reason to dial anything else. The failed record sits on hold,
// so the dials that would make evidence do not come: an hour on one path can pass
// with no second dial of it. A dial also fails for reasons past the extender, and
// the strategy's tries one carrier in a race. So a failed dial only says a record
// is worth checking, and the check is made here, at a pace of its own. A client
// with nothing failing arms nothing, dials nothing and asks nothing.
//
// This loop is the only caller, so everything is coalesced by construction: one
// control ask, one attempt pass or one report at a time. Its bounds:
//
//   - at most one attempt pass per AttemptInterval, each of at most
//     MaxAttemptSuspectCount records, and none while the control route does not
//     answer, backing off from five minutes to an hour;
//   - at most RateLimitCount reports an hour for an identity, none within
//     MinRequestInterval of another, a failed one backing off, and a record the
//     operator answered about left alone for a day.
//
// A report is the whole of what this does with what it finds. It never calls
// RecordFailure or RecordSuccess, so nothing in the directory's holds, counts,
// order or gated policy moves. Everything is in memory: a restart starts over,
// which takes at least another hour, so the hourly limit needs no store.
//
// It runs under the same cohort control as the gated preference
// (NetworkSpaceValues.ExtenderGatedPreference), off by default, for the identity
// bound to the release fetch. A hosted space builds none, and a process whose
// extender store is read-only binds none. An account change, seen through the
// space's account monitor, cancels what is in flight, discards its answer and
// forgets every suspect. A path change drops every suspect and ends a pass in
// flight. Nothing here logs an address, a key, a token or an id, and the counts
// are over fixed label sets.

const extenderBlockReportPath = "/network/extender-block-report"

// What a report came to, a fixed label set.
const (
	// the operator recorded it
	extenderBlockReportRecorded = "recorded"
	// the operator answered and did not record it: an unknown key, or an
	// extender its own probe does not reach now, which is an outage
	extenderBlockReportNotRecorded = "not_recorded"
	// the operator refused it in the answer's `error`
	extenderBlockReportRefused = "refused"
	// no answer to read: transport, deadline, status, body
	extenderBlockReportFailed = "failed"
	// an account change or a close ended it, whatever it would have said
	extenderBlockReportDiscarded = "discarded"
)

// The sender's numbers and seams.
type extenderBlockReportSettings struct {
	// The rule (connect ExtenderBlockEvidenceSettings).
	Evidence *connect.ExtenderBlockEvidenceSettings
	// The budget of one ask of the control route.
	ControlProbeTimeout time.Duration
	// The deadline of the dials of one attempt pass: its records at the carrier
	// budget, for two addresses of three carriers each, with some to spare. A
	// record it does not reach is not counted, and is first again next pass. The
	// asks on either side have their own budget.
	AttemptPassTimeout time.Duration
	// The deadline of one report.
	RequestTimeout time.Duration
	// No two reports within this, and no more than RateLimitCount within
	// RateLimitWindow, answered or not.
	MinRequestInterval time.Duration
	RateLimitCount     int
	RateLimitWindow    time.Duration
	// A report that got no answer waits, doubling.
	RetryMinBackoff time.Duration
	RetryMaxBackoff time.Duration
	// A control ask that got no answer holds the next pass, doubling. A path
	// change starts over.
	ControlMinBackoff time.Duration
	ControlMaxBackoff time.Duration

	// The only clock the sender reads.
	Now func() time.Time
	// The wait between two looks.
	After func(wait time.Duration) <-chan time.Time
	// The report: the space's api transport.
	Post connect.HttpPostRawFunction
	// One ask of the control route.
	ControlProbe func(ctx context.Context) error
	// One attempt of one suspect (connect ExtenderDirectory.AttemptBlockSuspect).
	Attempt func(ctx context.Context, suspect connect.ExtenderBlockSuspect) string
	// The families the client dials extenders of (connect
	// ExtenderDialIpVersions).
	IpVersions func() []int
}

func defaultExtenderBlockReportSettings() *extenderBlockReportSettings {
	evidence := connect.DefaultExtenderBlockEvidenceSettings()
	evidence.Enabled = true
	return &extenderBlockReportSettings{
		Evidence:            evidence,
		ControlProbeTimeout: 20 * time.Second,
		AttemptPassTimeout:  6 * time.Minute,
		RequestTimeout:      30 * time.Second,
		MinRequestInterval:  60 * time.Second,
		RateLimitCount:      4,
		RateLimitWindow:     time.Hour,
		RetryMinBackoff:     5 * time.Minute,
		RetryMaxBackoff:     30 * time.Minute,
		ControlMinBackoff:   5 * time.Minute,
		ControlMaxBackoff:   time.Hour,
		Now:                 time.Now,
		After:               time.After,
		IpVersions:          connect.ExtenderDialIpVersions,
	}
}

// The request of `POST /network/extender-block-report`.
type extenderBlockReportArgs struct {
	PublicKeyHex string `json:"public_key_hex"`
}

// The answer, with `recorded` read as false and `error` as none when absent.
type extenderBlockReportAnswer struct {
	Recorded bool   `json:"recorded"`
	Error    string `json:"error"`
}

// The sender's pace. Guarded by the sender's state lock.
type extenderBlockReportSchedule struct {
	// the reports of the bound identity, in the rate window, oldest first
	requestTimes []time.Time
	// when the last report began
	lastRequestTime time.Time
	// no report before this: a backoff, or the operator's refusal
	requestNotBefore time.Time
	requestBackoff   time.Duration

	// Per process, not per identity, since these bound dials and asks: no
	// attempt pass before this
	attemptNotBefore time.Time
	// the next ask of the control route waits for this
	controlNotBefore time.Time
	controlBackoff   time.Duration
}

// When the next report may go, at `now`.
func (self *extenderBlockReportSchedule) requestDueTime(now time.Time, settings *extenderBlockReportSettings) time.Time {
	self.requestTimes = slices.DeleteFunc(self.requestTimes, func(requestTime time.Time) bool {
		return !now.Before(requestTime.Add(settings.RateLimitWindow))
	})
	due := self.requestNotBefore
	if !self.lastRequestTime.IsZero() {
		if minTime := self.lastRequestTime.Add(settings.MinRequestInterval); due.Before(minTime) {
			due = minTime
		}
	}
	if settings.RateLimitCount <= len(self.requestTimes) {
		limit := self.requestTimes[len(self.requestTimes)-settings.RateLimitCount].Add(settings.RateLimitWindow)
		if due.Before(limit) {
			due = limit
		}
	}
	return due
}

func (self *extenderBlockReportSchedule) requested(now time.Time) {
	self.requestTimes = append(self.requestTimes, now)
	self.lastRequestTime = now
}

// A report got no answer at `now`. The next one waits out a doubling backoff.
func (self *extenderBlockReportSchedule) NextRequestFailure(now time.Time, settings *extenderBlockReportSettings) {
	if self.requestBackoff <= 0 {
		self.requestBackoff = settings.RetryMinBackoff
	} else {
		self.requestBackoff = min(2*self.requestBackoff, settings.RetryMaxBackoff)
	}
	self.requestNotBefore = now.Add(self.requestBackoff)
}

func (self *extenderBlockReportSchedule) requestAnswered() {
	self.requestBackoff = 0
}

// A control ask got no answer at `now`. The next one waits out a doubling
// backoff.
func (self *extenderBlockReportSchedule) NextControlFailure(now time.Time, settings *extenderBlockReportSettings) {
	if self.controlBackoff <= 0 {
		self.controlBackoff = settings.ControlMinBackoff
	} else {
		self.controlBackoff = min(2*self.controlBackoff, settings.ControlMaxBackoff)
	}
	self.controlNotBefore = now.Add(self.controlBackoff)
}

func (self *extenderBlockReportSchedule) controlAnswered() {
	self.controlBackoff = 0
	self.controlNotBefore = time.Time{}
}

// Another identity starts with its own report budget. What bounds the dials
// carries over.
func (self *extenderBlockReportSchedule) identityChanged() {
	self.requestTimes = nil
	self.lastRequestTime = time.Time{}
	self.requestNotBefore = time.Time{}
	self.requestBackoff = 0
}

// The sender's counts, all over fixed label sets.
type extenderBlockReportStats struct {
	// control asks, and those with no answer
	ControlCount        uint64
	ControlFailureCount uint64
	// attempt passes that dialed
	PassCount uint64
	// attempts by what they came to (connect ExtenderBlockAttempt*)
	AttemptCounts map[string]uint64
	// failed attempts the control route then answered after, which is what
	// counts toward a report
	CountedAttemptCount uint64
	// reports by what they came to (extenderBlockReport*)
	ReportCounts map[string]uint64
}

func (self *extenderBlockReportStats) clone() extenderBlockReportStats {
	copied := *self
	copied.AttemptCounts = cloneExtenderReleaseCounts(self.AttemptCounts)
	copied.ReportCounts = cloneExtenderReleaseCounts(self.ReportCounts)
	return copied
}

// The sender of one space.
type extenderBlockReporter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	runDone   chan struct{}
	watchDone chan struct{}
	log       connect.Logger

	settings  *extenderBlockReportSettings
	directory *connect.ExtenderDirectory
	apiUrl    string
	// the bound credential and identity of the release fetch
	binding func() (func() string, extenderReleaseIdentity)
	// advances on every account change of the bound identity
	accountMonitor     *connect.MonitorValue[uint64]
	unsubNetworkChange func()
	wake               *connect.Monitor

	stateLock sync.Mutex
	enabled   bool
	// the operation in flight, nil when none
	operationCancel    context.CancelFunc
	operationPathBound bool
	schedule           extenderBlockReportSchedule
	stats              extenderBlockReportStats

	// read and written by the loop alone
	evidenceOn        bool
	accountGeneration uint64
	identity          extenderReleaseIdentity
}

func newExtenderBlockReporter(
	ctx context.Context,
	directory *connect.ExtenderDirectory,
	apiUrl string,
	binding func() (func() string, extenderReleaseIdentity),
	accountMonitor *connect.MonitorValue[uint64],
	settings *extenderBlockReportSettings,
	log connect.Logger,
) *extenderBlockReporter {
	self := buildExtenderBlockReporter(ctx, directory, apiUrl, binding, accountMonitor, settings, log)
	self.start()
	return self
}

// The sender with nothing running yet. A test steps it with `next` alone.
func buildExtenderBlockReporter(
	ctx context.Context,
	directory *connect.ExtenderDirectory,
	apiUrl string,
	binding func() (func() string, extenderReleaseIdentity),
	accountMonitor *connect.MonitorValue[uint64],
	settings *extenderBlockReportSettings,
	log connect.Logger,
) *extenderBlockReporter {
	if log == nil {
		log = connect.DefaultLogger()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	return &extenderBlockReporter{
		ctx:               cancelCtx,
		cancel:            cancel,
		runDone:           make(chan struct{}),
		watchDone:         make(chan struct{}),
		log:               log,
		settings:          settings,
		directory:         directory,
		apiUrl:            apiUrl,
		binding:           binding,
		accountMonitor:    accountMonitor,
		wake:              connect.NewMonitor(),
		accountGeneration: accountMonitor.Value(),
	}
}

func (self *extenderBlockReporter) start() {
	self.unsubNetworkChange = connect.AddNetworkChangeListener(self.networkChanged)
	go connect.HandleError(func() {
		defer close(self.runDone)
		self.run()
	}, self.cancel)
	go connect.HandleError(func() {
		defer close(self.watchDone)
		self.watchAccount()
	}, self.cancel)
}

// Ends the loop and joins it. What is in flight is canceled, and the directory
// keeps no evidence after.
func (self *extenderBlockReporter) Close() {
	self.unsubNetworkChange()
	self.cancel()
	<-self.runDone
	<-self.watchDone
	self.directory.SetBlockEvidence(nil)
}

// Turns the sender on or off with the gated preference (the cohort control).
func (self *extenderBlockReporter) setEnabled(enabled bool) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.enabled == enabled {
			return false
		}
		self.enabled = enabled
		if !enabled && self.operationCancel != nil {
			self.operationCancel()
		}
		return true
	}()
	if changed {
		self.wake.NotifyAll()
	}
}

// A path change: what the suspects found describes the old path, and an attempt
// pass in flight is on it. A report in flight is on evidence already complete,
// and goes on.
func (self *extenderBlockReporter) networkChanged() {
	self.directory.BlockEvidencePathChanged()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.schedule.controlAnswered()
		if self.operationCancel != nil && self.operationPathBound {
			self.operationCancel()
		}
	}()
	self.wake.NotifyAll()
}

// Ends what is in flight on an account change, which the loop cannot see while
// it is in an operation, and wakes the loop to forget. A bind, an unbind and a
// token of another identity all advance the monitor, so the loop needs no other
// word of the binding.
func (self *extenderBlockReporter) watchAccount() {
	account, update := self.accountMonitor.Get()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-update:
		}
		var next uint64
		next, update = self.accountMonitor.Get()
		if next == account {
			continue
		}
		account = next
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.operationCancel != nil {
				self.operationCancel()
			}
		}()
		self.wake.NotifyAll()
	}
}

func (self *extenderBlockReporter) statsValue() extenderBlockReportStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stats.clone()
}

// The loop: look what the evidence asks for, do it, or wait for its time, a wake
// or a newly armed suspect.
func (self *extenderBlockReporter) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		// subscribed before the look, so a change during it is carried into the
		// wait
		wake := self.wake.NotifyChannel()
		armed := self.directory.BlockEvidenceMonitor().NotifyChannel()

		act, wait := self.next()
		if act != nil {
			act()
			continue
		}
		var after <-chan time.Time
		if 0 < wait {
			after = self.settings.After(wait)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-armed:
		case <-after:
		}
	}
}

// What to do now, or else how long to wait for the next thing to come due, zero
// for nothing to wait for.
func (self *extenderBlockReporter) next() (act func(), wait time.Duration) {
	accountGeneration := self.accountMonitor.Value()
	credential, identity := self.binding()
	if accountGeneration != self.accountGeneration || identity != self.identity {
		// forgotten first, so what was found under one identity is never sent
		// under the next
		self.accountGeneration = accountGeneration
		self.identity = identity
		self.directory.ForgetBlockEvidence()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.schedule.identityChanged()
		}()
	}
	enabled := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.enabled
	}()
	active := enabled && credential != nil && !identity.empty()
	if active != self.evidenceOn {
		self.evidenceOn = active
		if active {
			self.directory.SetBlockEvidence(self.settings.Evidence)
		} else {
			self.directory.SetBlockEvidence(nil)
		}
	}
	if !active {
		return nil, 0
	}

	look := self.directory.BlockEvidenceLook(self.settings.IpVersions())
	now := self.settings.Now()
	requestDueTime, attemptDueTime := func() (time.Time, time.Time) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		attemptDueTime := self.schedule.attemptNotBefore
		if attemptDueTime.Before(self.schedule.controlNotBefore) {
			attemptDueTime = self.schedule.controlNotBefore
		}
		return self.schedule.requestDueTime(now, self.settings), attemptDueTime
	}()
	if 0 < len(look.DuePublicKeys) {
		if !now.Before(requestDueTime) {
			publicKey := look.DuePublicKeys[0]
			return func() {
				self.report(publicKey, credential, identity, accountGeneration)
			}, 0
		}
		wait = requestDueTime.Sub(now)
	}
	if !look.NextAttemptTime.IsZero() {
		if attemptDueTime.Before(look.NextAttemptTime) {
			attemptDueTime = look.NextAttemptTime
		}
		if !now.Before(attemptDueTime) {
			if 0 < len(look.AttemptSuspects) {
				return func() {
					self.attempt(look.AttemptSuspects)
				}, 0
			}
		} else if attemptWait := attemptDueTime.Sub(now); wait <= 0 || attemptWait < wait {
			wait = attemptWait
		}
	}
	return nil, wait
}

// Starts one operation: what an account change, a disable or a close ends, and a
// path change too when `pathBound`. Returns its context and its end.
func (self *extenderBlockReporter) operation(timeout time.Duration, pathBound bool) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(self.ctx, timeout)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.operationCancel = cancel
		self.operationPathBound = pathBound
	}()
	return ctx, func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.operationCancel = nil
		}()
		cancel()
	}
}

// Asks the control route whether it answers. An ask its caller ended says
// nothing, and holds nothing back.
func (self *extenderBlockReporter) askControl(ctx context.Context) bool {
	err := func() error {
		probeCtx, cancel := context.WithTimeout(ctx, self.settings.ControlProbeTimeout)
		defer cancel()
		return self.settings.ControlProbe(probeCtx)
	}()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.stats.ControlCount += 1
	if err == nil {
		self.schedule.controlAnswered()
		return true
	}
	select {
	case <-ctx.Done():
		return false
	default:
	}
	self.stats.ControlFailureCount += 1
	self.schedule.NextControlFailure(now, self.settings)
	return false
}

// One attempt pass (rule 2 and 3): the control route answers, or nothing is
// dialed, then each suspect is dialed, and what failed counts only if the
// control route answers again.
func (self *extenderBlockReporter) attempt(suspects []connect.ExtenderBlockSuspect) {
	ctx, done := self.operation(self.settings.AttemptPassTimeout+2*self.settings.ControlProbeTimeout, true)
	defer done()

	if !self.askControl(ctx) {
		return
	}
	startTime := self.settings.Now()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.stats.PassCount += 1
		self.schedule.attemptNotBefore = startTime.Add(self.settings.Evidence.AttemptInterval)
	}()
	failed := []connect.ExtenderBlockSuspect{}
	func() {
		dialCtx, dialCancel := context.WithTimeout(ctx, self.settings.AttemptPassTimeout)
		defer dialCancel()
		for _, suspect := range suspects {
			outcome := self.settings.Attempt(dialCtx, suspect)
			if outcome == connect.ExtenderBlockAttemptFailed {
				failed = append(failed, suspect)
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				addExtenderReleaseCount(&self.stats.AttemptCounts, outcome)
			}()
		}
	}()
	if 0 < len(failed) && self.askControl(ctx) {
		self.directory.RecordBlockAttempts(startTime, failed)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.stats.CountedAttemptCount += uint64(len(failed))
		}()
	}
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]block attempt pass, %d of %d failed\n", len(failed), len(suspects))
	}
}

// One report of one record. It is sent once the limits allow it, under the
// identity whose client found it, and whatever the operator answered the record
// is left alone: `recorded` is false for an unknown key and for an extender its
// probe does not reach, neither of which another report would change.
func (self *extenderBlockReporter) report(
	publicKey []byte,
	credential func() string,
	identity extenderReleaseIdentity,
	accountGeneration uint64,
) {
	now := self.settings.Now()
	// the token is read with no lock held, and a token of another identity is
	// an account change this has not seen yet: nothing goes out under it
	byJwt := credential()
	if parseExtenderReleaseIdentity(byJwt) != identity {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.schedule.requestNotBefore = now.Add(self.settings.MinRequestInterval)
		}()
		return
	}
	argsBytes, err := json.Marshal(&extenderBlockReportArgs{
		PublicKeyHex: hex.EncodeToString(publicKey),
	})
	if err != nil {
		return
	}

	ctx, done := self.operation(self.settings.RequestTimeout, false)
	defer done()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.schedule.requested(now)
	}()
	responseBytes, requestErr := self.settings.Post(ctx, self.apiUrl+extenderBlockReportPath, argsBytes, byJwt)

	if self.accountMonitor.Value() != accountGeneration || errors.Is(ctx.Err(), context.Canceled) {
		self.reportCame(extenderBlockReportDiscarded)
		return
	}
	answer := &extenderBlockReportAnswer{}
	if requestErr == nil {
		requestErr = json.Unmarshal(responseBytes, answer)
	}
	endTime := self.settings.Now()
	if requestErr != nil {
		// kept, while its evidence stays fresh
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.schedule.NextRequestFailure(endTime, self.settings)
		}()
		self.reportCame(extenderBlockReportFailed)
		return
	}
	self.directory.BlockEvidenceReported(publicKey)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.schedule.requestAnswered()
		if answer.Error != "" {
			// a limit, or a client the route does not take: the window waits it
			// out, and the operator's reason is no one else's to read
			self.schedule.requestNotBefore = endTime.Add(self.settings.RateLimitWindow)
		}
	}()
	switch {
	case answer.Error != "":
		self.reportCame(extenderBlockReportRefused)
	case answer.Recorded:
		self.reportCame(extenderBlockReportRecorded)
	default:
		self.reportCame(extenderBlockReportNotRecorded)
	}
}

func (self *extenderBlockReporter) reportCame(label string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		addExtenderReleaseCount(&self.stats.ReportCounts, label)
	}()
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]block report %s\n", label)
	}
}
