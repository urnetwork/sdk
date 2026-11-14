package sdk

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// extender_seed.go — the seed bootstrap of a network space (connect
// EXTENDER-UPDATE2.md §2.3, §2.4).
//
// A network space finds extenders through the dns bootstrap, the feed and the
// mesh, and reaches its operator through the control route (E3). When the
// dns bootstrap has left the directory with nothing usable and the control
// route does not answer either, the space has no verified extender to dial
// and no operator to ask. The seed bootstrap is the way back: an
// authenticated bundle of ordinary open-tier records, each with its operator
// signature, taken from wherever one can be had without the routes that
// failed.
//
// One pass takes the sources in order, cheapest first:
//
//  1. the bundles this build carries (extender_seed_embedded.go), each judged
//     and, when acceptable, accepted into the seed store;
//  2. the accepted bundle of each network host in the seed store, judged
//     again under the root keys in force and the local clock;
//  3. the mirrors the space's values configure (extender_seed_mirrors.go),
//     under connect's shared budget, deadline and cancellation.
//
// Whatever is accepted is applied to the directory with source `seed`:
// revocations first, then records, every one verified again under the
// directory's own root keys and network hosts (connect ApplyExtenderSeed). A
// bundle never adds an address, so a seed never makes an unsigned hint
// dialable, and nothing here changes a trust anchor or a resolver setting.
// The pass stops at the first source that makes a record newly usable.
//
// A pass runs for two callers, and one at a time: a second caller joins the
// pass in flight rather than starting another.
//
//   - The release fetch (extender_release.go), through the control reach it
//     walks when `POST /network/extender-release` cannot reach the operator
//     (extender_control_reach.go). The answer is whether a verified open
//     record became usable, and the fetch retries at once when it did.
//   - The space's own trigger, for a cold start no release fetch is waiting
//     on -- a signed-out app that cannot reach its login: once the network
//     client's first attempt has ended with nothing usable, it asks the
//     control route directly, and only when that fails too does it run a
//     pass. When the pass made something usable it asks the control route
//     again, now through the seed's extenders, and on an answer requests the
//     gated release (`requestExtenderRelease`), which the release fetch
//     coalesces with its own.
//
// An account change of the bound identity and a path change of the physical
// network each cancel the pass in flight; nothing a pass has already accepted
// or applied is undone, since a verified open record is extender knowledge,
// kept per installation (E7). A path change also clears the backoffs, so the
// new path is tried at once. A reset of the extender section holds passes off
// until the directory and the seed store are both reset.
//
// The accepted floors bind only while their bundles verify under the root keys
// in force (connect ExtenderSeedAcceptor). Whenever the keys change, the store
// is revalidated so a floor set under a key the space no longer trusts is
// dropped: at once where the space installs keys itself (a settings change or
// an import of settings), and at the next wake of the trigger or the next pass
// or import where a hello installed them. connect applies the same rule to
// every read, so a revalidation that has not run yet never lets a void floor
// bind.
//
// What a pass does is reported on the space's seed status, with the codes
// connect gives its refusals (the fixed set of ExtenderSeedCode*). Refusals
// come from what a mirror, a build or a paste hands the client, so the log
// lines are V(1) and carry no address or key.

// The outcome of the last pass (ExtenderSeedStatus.LastPassOutcome).
const (
	// a verified bundle made a record newly usable
	ExtenderSeedPassOutcomeApplied = "applied"
	// a verified bundle was in force, but it made nothing new usable
	ExtenderSeedPassOutcomeNothingNew = "nothing_new"
	// no source produced a bundle; the status names why
	ExtenderSeedPassOutcomeFailed = "failed"
	// an account change, a path change or the space's close ended the pass
	ExtenderSeedPassOutcomeCanceled = "canceled"
)

// Where the bundle in force came from (ExtenderSeedStatus.Source).
const (
	ExtenderSeedSourceEmbedded = "embedded"
	ExtenderSeedSourceStore    = "store"
	ExtenderSeedSourceMirror   = "mirror"
	ExtenderSeedSourceImport   = "import"
)

// The seed bootstrap's numbers and seams. Every clock and every side effect
// that leaves the process is a field, so the whole lifecycle is deterministic
// in tests.
type extenderSeedSettings struct {
	// The deadline of one whole pass: the local sources, every mirror and the
	// apply. connect's mirror pass shares it.
	PassTimeout time.Duration
	// One attempt at one mirror within the pass. Zero is connect's default.
	MirrorAttemptTimeout time.Duration
	// The budget of one ask of the control route.
	ControlProbeTimeout time.Duration
	// After a pass that made nothing usable, the mirrors and the automatic
	// trigger wait this long, doubling with each further such pass up to the
	// max. A path change, or a pass that makes something usable, starts over.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// After the control route answered directly while the directory had
	// nothing usable, the automatic trigger asks it again only this long
	// later: the space needs no seed while its operator answers.
	ControlRecheckTimeout time.Duration
	// While the directory has something usable, the automatic trigger looks
	// again this often, which is how it notices every usable address going on
	// hold without listening to every change of the directory.
	RecheckTimeout time.Duration

	// The only clock the seed bootstrap reads.
	Now func() time.Time
	// When set, replaces time.After as every wait of the automatic trigger.
	After func(wait time.Duration) <-chan time.Time
	// When set, replaces the ask of the control route. Nil reads `/hello`
	// through the space's client strategy, which dials the seed's extenders
	// once they are usable.
	ControlProbe func(ctx context.Context) error
	// When set, replaces the mirror pass. Nil is connect's.
	MirrorPass func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult
}

func defaultExtenderSeedSettings() *extenderSeedSettings {
	return &extenderSeedSettings{
		PassTimeout:         60 * time.Second,
		ControlProbeTimeout: 20 * time.Second,
		MinBackoff:          1 * time.Minute,
		// A client here is cut off: nothing it dials reaches the operator. An
		// hour bounds how long it waits once the mirrors answer again, and
		// the fetches of a blocked fleet to one an hour each; a path change
		// tries again at once.
		MaxBackoff:            1 * time.Hour,
		ControlRecheckTimeout: 15 * time.Minute,
		RecheckTimeout:        30 * time.Second,
		Now:                   time.Now,
	}
}

// When set, adjusts the settings of every seed bootstrap built after it.
// Production never sets it; the tests install their clocks, probes and
// mirrors through it.
var extenderSeedSettingsConfigure func(settings *extenderSeedSettings)

// A process-wide switch for the automatic loop -- the trigger, and the
// revalidation on its wakes -- on unless set. The sdk test suite sets it
// (TestMain) so a test that builds a space with a dotted host never probes or
// fetches anything, and a store is revalidated only where a test acts; the
// seed tests that exercise the loop clear it. Passes, imports and the space's
// own key changes revalidate either way.
var extenderSeedLoopDisabled atomic.Bool

// What one mirror pass is asked: the mirrors, and what everything they serve
// is judged by.
type extenderSeedMirrorRequest struct {
	mirrors      []*extenderSeedMirror
	rootKeys     *connect.ExtenderRootKeySet
	networkHosts []string
	acceptor     *connect.ExtenderSeedAcceptor
	directory    *connect.ExtenderDirectory
	now          func() time.Time
}

// What one mirror pass did: the bundle it accepted and applied, with what the
// apply changed, or the seed code of why there was none.
type extenderSeedMirrorResult struct {
	bundle *connect.ExtenderSeedBundle
	apply  *connect.ExtenderSeedApplyResult
	code   string
	// connect's own account of the pass -- each mirror's outcome, and the
	// attempts, dials and bytes it took -- nil for a pass that is not
	// connect's
	pass *connect.ExtenderSeedBootstrapResult
}

// The seed bootstrap of one network space.
type spaceExtenderSeed struct {
	ctx      context.Context
	cancel   context.CancelFunc
	log      connect.Logger
	settings *extenderSeedSettings

	space     *NetworkSpace
	directory *connect.ExtenderDirectory
	// the platform storage under the acceptor: the local state file, or
	// memory for a space with none
	store    connect.ExtenderSeedStore
	acceptor *connect.ExtenderSeedAcceptor
	embedded *extenderSeedEmbeddedBundles

	removeControlReach func()
	unsubNetworkChange func()
	closeOnce          sync.Once
	runDone            chan struct{}
	passes             sync.WaitGroup
	// re-evaluates the automatic trigger: a path change, a close
	wake *connect.Monitor

	stateLock sync.Mutex
	closed    bool
	// holds of a reset in progress (suspend); no pass starts while one is held
	suspendCount int
	// the pass in flight, nil when none; a caller joins it
	pass *extenderSeedPass
	// when the mirrors may next be fetched, and the backoff that set it
	mirrorBackoff extenderSeedBackoff
	// when the automatic trigger may next act, and the backoff that set it
	attemptBackoff extenderSeedBackoff
	// the root keys the store was last revalidated under (connect void floors)
	revalidatedRootKeys *connect.ExtenderRootKeySet
	status              ExtenderSeedStatus
}

// One pass in flight. `result` is written before `done` closes.
type extenderSeedPass struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	result *extenderSeedPassResult
}

// What one pass did.
type extenderSeedPassResult struct {
	outcome string
	// the seed code of a failure, empty when the outcome is not a failure
	code string
	// whether a record became newly usable
	usable bool
}

// A backoff that doubles from its min to its max with each failure and starts
// over on a success. The zero value allows the next attempt at once.
type extenderSeedBackoff struct {
	nextTime time.Time
	timeout  time.Duration
}

// Whether the next attempt is allowed at `now`, and if not how long until it is.
func (self *extenderSeedBackoff) wait(now time.Time) time.Duration {
	if self.nextTime.IsZero() {
		return 0
	}
	return self.nextTime.Sub(now)
}

// Records a failure at `now`: the next attempt waits the backoff, which then
// doubles.
func (self *extenderSeedBackoff) NextFailure(now time.Time, minTimeout time.Duration, maxTimeout time.Duration) {
	if self.timeout <= 0 {
		self.timeout = minTimeout
	}
	self.nextTime = now.Add(self.timeout)
	self.timeout = min(2*self.timeout, maxTimeout)
}

// Defers the next attempt by `timeout` without advancing the backoff.
func (self *extenderSeedBackoff) postpone(now time.Time, timeout time.Duration) {
	self.nextTime = now.Add(timeout)
}

// Starts over: the next attempt is allowed at once.
func (self *extenderSeedBackoff) reset() {
	self.nextTime = time.Time{}
	self.timeout = 0
}

// Builds and starts the seed bootstrap of a space, or nil for a space that
// runs none: one without a directory, and the space a cloud host shares among
// its hosted devices, whose host never needs it and must dial nothing a seed
// or a user names. It registers itself as a control reach of the release
// fetch.
func newSpaceExtenderSeed(space *NetworkSpace) *spaceExtenderSeed {
	if space.extenderDirectory == nil || space.hostedIncompatible {
		return nil
	}
	settings := defaultExtenderSeedSettings()
	if extenderSeedSettingsConfigure != nil {
		extenderSeedSettingsConfigure(settings)
	}
	if settings.Now == nil {
		settings.Now = time.Now
	}

	var store connect.ExtenderSeedStore
	if space.asyncLocalState != nil {
		store = newLocalStateExtenderSeedStore(space.asyncLocalState.GetLocalState())
	} else {
		// a space with no local state keeps its floor for its own life
		store = connect.NewExtenderSeedMemoryStore()
	}
	embedded := parseExtenderSeedEmbedded(extenderSeedEmbeddedResource())
	values := space.valuesCopy()
	primaryNetworkHost := extenderNetworkHostName(&space.key, &values)

	cancelCtx, cancel := context.WithCancel(space.ctx)
	self := &spaceExtenderSeed{
		ctx:       cancelCtx,
		cancel:    cancel,
		log:       space.logger(),
		settings:  settings,
		space:     space,
		directory: space.extenderDirectory,
		store:     store,
		acceptor: connect.NewExtenderSeedAcceptor(store, &connect.ExtenderSeedAcceptorSettings{
			// the build's floor for the space's own network, which binds what
			// the mirrors serve; an import and an embedded bundle are judged
			// against the floor of whichever host they name (verify)
			MinSequence: embedded.minSequence(primaryNetworkHost),
			Now:         settings.Now,
		}),
		embedded: embedded,
		runDone:  make(chan struct{}),
		wake:     connect.NewMonitor(),
	}
	self.status.EmbeddedCount = len(embedded.texts)
	if embedded.err != nil {
		self.status.EmbeddedError = extenderSeedEmbeddedInvalid
		self.log.Infof("[extender]embedded seed resource refused: %s\n", embedded.err)
	}
	self.mirrors()

	self.unsubNetworkChange = connect.AddNetworkChangeListener(self.networkChanged)
	self.removeControlReach = space.addExtenderControlReach(self)
	go connect.HandleError(func() {
		defer close(self.runDone)
		self.run()
	}, cancel)
	return self
}

// The embedded resource's error code when it does not parse: not one of
// connect's seed codes, since nothing was decoded.
const extenderSeedEmbeddedInvalid = "embedded_invalid"

// Ends the trigger and the pass in flight, and joins both. Idempotent.
func (self *spaceExtenderSeed) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.closed = true
		}()
		self.removeControlReach()
		self.unsubNetworkChange()
		self.cancel()
		self.wake.NotifyAll()
		<-self.runDone
		self.passes.Wait()
	})
}

// Starts the seed bootstrap of a space its constructor just built, once the
// directory, the network client and the node exist.
func (self *NetworkSpace) startExtenderSeed() {
	seed := newSpaceExtenderSeed(self)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.extenderSeed = seed
}

// Stops the space's seed bootstrap and joins it. The status stays readable.
func (self *NetworkSpace) closeExtenderSeed() {
	self.getExtenderSeed().Close()
}

// The reset of the extender section clears the seed store with the directory
// (E7). Called where the directory is reset: in place, at construction before
// the bootstrap exists, and on the space a rebuild replaces.
func (self *NetworkSpace) resetExtenderSeed() {
	if seed := self.getExtenderSeed(); seed != nil {
		seed.reset()
		return
	}
	if self.asyncLocalState != nil {
		if err := newLocalStateExtenderSeedStore(self.asyncLocalState.GetLocalState()).Clear(); err != nil {
			self.logger().Infof("[extender]seed reset err = %s\n", err)
		}
	}
}

// Revalidates the space's seed store under the root keys the directory holds
// now: what the space calls once it has installed new keys itself.
func (self *NetworkSpace) revalidateExtenderSeed() {
	if seed := self.getExtenderSeed(); seed != nil {
		seed.revalidate(seed.directory.RootKeys())
	}
}

// Ends the space's pass in flight and starts none until the returned resume,
// so a reset lands between passes. A space with no seed bootstrap has nothing
// to hold.
func (self *NetworkSpace) suspendExtenderSeed() (resume func()) {
	if seed := self.getExtenderSeed(); seed != nil {
		return seed.suspend()
	}
	return func() {}
}

// The space's mirror values changed: the next pass fetches from the new
// mirrors, and at once rather than after the backoff the old ones built up.
func (self *NetworkSpace) extenderSeedMirrorsChanged() {
	if seed := self.getExtenderSeed(); seed != nil {
		seed.mirrorsChanged()
	}
}

// The mirrors the space's values configure now, which the status counts.
func (self *spaceExtenderSeed) mirrors() []*extenderSeedMirror {
	values := self.space.valuesCopy()
	mirrors := spaceExtenderSeedMirrors(&self.space.key, &values)
	dnsIndependentCount := 0
	for _, mirror := range mirrors {
		if mirror.dnsIndependent() {
			dnsIndependentCount += 1
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status.MirrorCount = len(mirrors)
	self.status.DnsIndependentMirrorCount = dnsIndependentCount
	return mirrors
}

func (self *spaceExtenderSeed) mirrorsChanged() {
	self.mirrors()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.mirrorBackoff.reset()
		self.attemptBackoff.reset()
	}()
	self.wake.NotifyAll()
}

// Ends the pass in flight, waits for it, and starts no other until the
// returned resume, which is idempotent; holds nest. A caller that asks for
// reach meanwhile gets none, at once. The trigger waits on the wake that the
// resume sends.
func (self *spaceExtenderSeed) suspend() (resume func()) {
	pass := func() *extenderSeedPass {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.suspendCount += 1
		return self.pass
	}()
	if pass != nil {
		pass.cancel()
		<-pass.done
	}
	var resumeOnce sync.Once
	return func() {
		resumeOnce.Do(func() {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.suspendCount -= 1
			}()
			self.wake.NotifyAll()
		})
	}
}

func (self *spaceExtenderSeed) suspended() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < self.suspendCount
}

// The hosts whose records and bundles the space accepts (B2), each once.
func (self *spaceExtenderSeed) networkHosts() []string {
	values := self.space.valuesCopy()
	networkHosts := []string{}
	for _, networkHost := range extenderNetworkHosts(&self.space.key, &values) {
		networkHost = connect.NormalizeExtenderNetworkHost(networkHost)
		if networkHost != "" && !slices.Contains(networkHosts, networkHost) {
			networkHosts = append(networkHosts, networkHost)
		}
	}
	return networkHosts
}

// The host the space's extender network is keyed by, whose bundle the status
// reports.
func (self *spaceExtenderSeed) primaryNetworkHost() string {
	values := self.space.valuesCopy()
	return connect.NormalizeExtenderNetworkHost(extenderNetworkHostName(&self.space.key, &values))
}

// extenderControlReach: makes verified extender candidates available that can
// carry the control route without the route that failed.
// True when a verified open record became newly usable, so the release fetch
// retries at once; false when there was nothing new, when the pass failed or
// was canceled, and when ctx ended first. A caller that gives up leaves the
// pass running for whoever else waits on it.
func (self *spaceExtenderSeed) obtainExtenderControlReach(ctx context.Context) bool {
	return self.awaitPass(ctx).usable
}

// Joins the pass in flight, or starts one, and waits for it or for ctx.
func (self *spaceExtenderSeed) awaitPass(ctx context.Context) *extenderSeedPassResult {
	pass := self.joinPass()
	if pass == nil {
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeCanceled,
			code:    connect.ExtenderSeedCodeCanceled,
		}
	}
	select {
	case <-pass.done:
		return pass.result
	case <-ctx.Done():
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeCanceled,
			code:    connect.ExtenderSeedCodeCanceled,
		}
	}
}

// The pass in flight, or a new one started, nil once the bootstrap is closed
// and while a reset holds it (suspend).
func (self *spaceExtenderSeed) joinPass() *extenderSeedPass {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || 0 < self.suspendCount {
		return nil
	}
	if self.pass != nil {
		return self.pass
	}
	passCtx, passCancel := context.WithTimeout(self.ctx, self.settings.PassTimeout)
	pass := &extenderSeedPass{
		ctx:    passCtx,
		cancel: passCancel,
		done:   make(chan struct{}),
	}
	self.pass = pass
	self.passes.Add(1)
	go connect.HandleError(func() {
		defer self.passes.Done()
		// the result a panic leaves, so the waiters and the next pass are
		// never left behind a pass that will not finish
		result := &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeFailed,
			code:    connect.ExtenderSeedCodeCanceled,
		}
		defer func() {
			passCancel()
			self.finishPass(pass, result)
		}()
		result = self.runPass(pass)
	})
	return pass
}

// Publishes a pass's result to its waiters and the status, and frees the slot.
func (self *spaceExtenderSeed) finishPass(pass *extenderSeedPass, result *extenderSeedPassResult) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		pass.result = result
		if self.pass == pass {
			self.pass = nil
		}
		self.status.LastPassTime = extenderSeedTimeMillis(self.settings.Now())
		self.status.LastPassOutcome = result.outcome
		self.status.LastPassError = result.code
	}()
	if result.usable {
		self.wakeNetworkClient()
	}
	close(pass.done)
}

// Wakes the space's network client once a seed has made verified extenders
// dialable: it samples through them and reads hello and the hint again at
// once, rather than after the backoffs their failures built up (connect
// ExtenderNetworkClient.Wake). A space with no network client has nothing to
// wake.
func (self *spaceExtenderSeed) wakeNetworkClient() {
	if networkClient := self.space.getExtenderNetworkClient(); networkClient != nil {
		networkClient.Wake()
	}
}

// Ends the pass in flight, if any. Its waiters see it canceled.
func (self *spaceExtenderSeed) cancelPass() {
	pass := func() *extenderSeedPass {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.pass
	}()
	if pass != nil {
		pass.cancel()
	}
}

// A path change of the physical network: the pass in flight is bound to the
// old path, so it ends, and the backoffs start over, so the new path is tried
// at once. Nothing accepted is forgotten.
func (self *spaceExtenderSeed) networkChanged() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.mirrorBackoff.reset()
		self.attemptBackoff.reset()
	}()
	self.cancelPass()
	self.wake.NotifyAll()
}

// Cancels `pass` when the bound identity changes while it runs (the release
// fetch's account monitor, extender_release_space.go), and returns what stops
// the watch. A space that binds no identity never moves its monitor.
func (self *spaceExtenderSeed) cancelOnAccountChange(pass *extenderSeedPass) (stop func()) {
	accountMonitor := self.space.extenderAccountMonitor()
	account, update := accountMonitor.Get()
	stopped := make(chan struct{})
	watchDone := make(chan struct{})
	go connect.HandleError(func() {
		defer close(watchDone)
		for {
			select {
			case <-stopped:
				return
			case <-pass.ctx.Done():
				return
			case <-update:
			}
			var next uint64
			next, update = accountMonitor.Get()
			if next != account {
				pass.cancel()
				return
			}
		}
	})
	return func() {
		close(stopped)
		<-watchDone
	}
}

// One pass: the local sources, then the mirrors, under the pass's deadline
// and cancellation.
func (self *spaceExtenderSeed) runPass(pass *extenderSeedPass) *extenderSeedPassResult {
	stopAccountWatch := self.cancelOnAccountChange(pass)
	defer stopAccountWatch()

	canceled := func() *extenderSeedPassResult {
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeCanceled,
			code:    connect.ExtenderSeedCodeCanceled,
		}
	}
	// A cancel -- an account change, a path change, a reset or the close --
	// ends a pass without judging it. The pass's own deadline is a failure
	// like any other: the mirrors had their time, and back off.
	canceledByOwner := func() bool {
		return errors.Is(pass.ctx.Err(), context.Canceled)
	}

	rootKeys := self.directory.RootKeys()
	if rootKeys == nil || rootKeys.Len() == 0 {
		// nothing can verify until a trust anchor is in force; the network
		// client waits for the first hello, and a pass would refuse everything
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeFailed,
			code:    connect.ExtenderSeedCodeNoRootKeys,
		}
	}
	networkHosts := self.networkHosts()
	self.revalidate(rootKeys)
	usableCount := self.directory.UsableCount(0)

	// 1, 2: the bundles this build carries and the accepted ones, no network
	failureCode := self.acceptEmbedded(rootKeys, networkHosts)
	inForce, storeFailureCode := self.applyStored(rootKeys, networkHosts)
	if failureCode == "" {
		failureCode = storeFailureCode
	}
	if usableCount < self.directory.UsableCount(0) {
		self.mirrorSucceeded()
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeApplied,
			usable:  true,
		}
	}
	if pass.ctx.Err() != nil {
		if canceledByOwner() {
			return canceled()
		}
		return self.localResult(inForce, failureCode, connect.ExtenderSeedCodeFetch)
	}

	// 3: the mirrors, unless a recent fetch left them in backoff
	mirrors := self.mirrors()
	if len(mirrors) == 0 {
		return self.localResult(inForce, failureCode, connect.ExtenderSeedCodeNoMirror)
	}
	if wait := self.mirrorWait(); 0 < wait {
		self.log.V(1).Infof("[extender]seed mirrors wait %s\n", wait)
		return self.localResult(inForce, failureCode, connect.ExtenderSeedCodeFetch)
	}
	mirrorResult := self.mirrorPass(pass.ctx, &extenderSeedMirrorRequest{
		mirrors:      mirrors,
		rootKeys:     rootKeys,
		networkHosts: networkHosts,
		acceptor:     self.acceptor,
		directory:    self.directory,
		now:          self.settings.Now,
	})
	// what a mirror's bundle made usable counts whatever ended the pass after
	// it: it was accepted, and stays accepted
	if mirrorResult.bundle != nil {
		self.recordBundle(mirrorResult.bundle, ExtenderSeedSourceMirror)
	}
	if usableCount < self.directory.UsableCount(0) {
		self.mirrorSucceeded()
		return &extenderSeedPassResult{
			outcome: ExtenderSeedPassOutcomeApplied,
			usable:  true,
		}
	}
	if pass.ctx.Err() != nil && canceledByOwner() {
		return canceled()
	}
	self.mirrorFailed()
	if mirrorResult.bundle != nil {
		return &extenderSeedPassResult{outcome: ExtenderSeedPassOutcomeNothingNew}
	}
	code := mirrorResult.code
	if code == "" || code == connect.ExtenderSeedCodeCanceled {
		// the pass's deadline ended the mirrors: none served a bundle in time
		code = connect.ExtenderSeedCodeFetch
	}
	self.log.V(1).Infof("[extender]seed mirrors produced no bundle: %s\n", code)
	return self.localResult(inForce, failureCode, code)
}

// The result of a pass that made nothing usable: nothing new when a bundle
// was in force, else a failure with the most telling code -- a refusal of a
// local source names its cause better than a missing mirror does.
func (self *spaceExtenderSeed) localResult(inForce bool, localCode string, code string) *extenderSeedPassResult {
	if inForce {
		return &extenderSeedPassResult{outcome: ExtenderSeedPassOutcomeNothingNew}
	}
	if localCode != "" && (code == connect.ExtenderSeedCodeNoMirror || code == connect.ExtenderSeedCodeFetch) {
		code = localCode
	}
	return &extenderSeedPassResult{
		outcome: ExtenderSeedPassOutcomeFailed,
		code:    code,
	}
}

// Drops the floors that no longer bind (connect's void rule), once per key set
// the directory installs: the directory replaces its set whenever the keys
// change, so a set it has not been revalidated under is a change. Every
// acceptor operation applies the same rule to what it reads, so this keeps the
// store from holding a void floor for longer than the next look -- for another
// process that shares the store, and for a later return of the removed key. A
// failed revalidation is tried again at the next look.
func (self *spaceExtenderSeed) revalidate(rootKeys *connect.ExtenderRootKeySet) {
	due := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.revalidatedRootKeys != rootKeys
	}()
	if !due {
		return
	}
	droppedCount, err := self.acceptor.Revalidate(rootKeys)
	if err != nil {
		self.log.V(1).Infof("[extender]seed store revalidate err = %s\n", err)
		return
	}
	// the status names the bundle of the space's own host; once that floor
	// is dropped there is none
	primaryDropped := false
	if 0 < droppedCount {
		self.log.Infof("[extender]seed store dropped %d floors no longer signed by a root in force\n", droppedCount)
		sequence, err := self.acceptor.Sequence(self.primaryNetworkHost(), rootKeys)
		primaryDropped = err == nil && sequence == 0
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.revalidatedRootKeys = rootKeys
		if primaryDropped {
			self.status.Sequence = 0
			self.status.ExpireTime = 0
			self.status.Source = ""
		}
	}()
}

// Judges every bundle this build carries for one of the space's hosts and
// accepts the acceptable ones into the store, where applyStored finds the best
// of each host. Returns the seed code of the first refusal, empty when none:
// an expired embedded bundle is a freshness failure the status names, never
// one that is used.
//
// A release carries the bundles of every network it ships with, so a bundle
// that names another network is skipped, not refused. Which network a bundle
// names is read before its signature is checked, for that routing alone: a
// bundle that names this space's network is then verified in full, and one
// that only claims another is merely not used here.
func (self *spaceExtenderSeed) acceptEmbedded(rootKeys *connect.ExtenderRootKeySet, networkHosts []string) string {
	failureCode := ""
	if self.embedded.err != nil {
		failureCode = extenderSeedEmbeddedInvalid
	}
	for _, text := range self.embedded.texts {
		envelope, err := connect.DecodeExtenderSeedText(text)
		if err == nil && !extenderSeedEnvelopeClaimsHost(envelope, networkHosts) {
			continue
		}
		var bundle *connect.ExtenderSeedBundle
		if err == nil {
			bundle, err = self.verify(envelope, rootKeys, networkHosts)
		}
		if err == nil {
			_, err = self.acceptor.Accept(bundle, rootKeys)
		}
		code := connect.ExtenderSeedErrorCode(err)
		switch {
		case err == nil:
			self.recordBundle(bundle, ExtenderSeedSourceEmbedded)
		case code == connect.ExtenderSeedCodeRollback:
			// the store holds a newer bundle than the build: not a failure
		default:
			if code == "" {
				code = connect.ExtenderSeedCodeMalformed
			}
			if failureCode == "" {
				failureCode = code
			}
			self.log.V(1).Infof("[extender]embedded seed refused: %s\n", code)
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.status.EmbeddedError = failureCode
	}()
	return failureCode
}

// Whether an envelope's body names one of `networkHosts`, read without
// checking the signature. It routes a bundle to the space it is for and
// decides nothing else; VerifyExtenderSeed judges the same body under the root
// keys. An envelope whose body does not decode is not skipped, so the
// verification names its fault.
func extenderSeedEnvelopeClaimsHost(envelope *protocol.ExtenderSeedEnvelope, networkHosts []string) bool {
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.GetBody(), body); err != nil {
		return true
	}
	return connect.ExtenderNetworkHostAllowed(body.NetworkHost, networkHosts...)
}

// Verifies one envelope under the space's keys, hosts and clock, and the floor
// the build configures for its host (§2.3).
func (self *spaceExtenderSeed) verify(
	envelope *protocol.ExtenderSeedEnvelope,
	rootKeys *connect.ExtenderRootKeySet,
	networkHosts []string,
) (*connect.ExtenderSeedBundle, error) {
	bundle, err := connect.VerifyExtenderSeed(envelope, rootKeys, networkHosts, self.settings.Now())
	if err != nil {
		return nil, err
	}
	if minSequence := self.embedded.minSequence(bundle.NetworkHost); bundle.Sequence < minSequence {
		return nil, &connect.ExtenderSeedError{
			Code:   connect.ExtenderSeedCodeRollback,
			Detail: "below the minimum sequence this build configures",
		}
	}
	return bundle, nil
}

// Applies the accepted bundle of every host the space accepts, each judged
// again under the keys in force and the local clock. Returns whether a bundle
// was in force, and the seed code of the first refusal of a stored one -- an
// expired bundle keeps its floor and is not applied.
//
// This is the rule of connect's ApplyStoredExtenderSeed -- every host's bundle
// in force is applied, and a refused one is passed over with its floor kept --
// in a loop of its own, so the status records the space's own host's bundle
// whichever host's comes first, and freshness is judged by the seed's clock.
func (self *spaceExtenderSeed) applyStored(rootKeys *connect.ExtenderRootKeySet, networkHosts []string) (bool, string) {
	inForce := false
	failureCode := ""
	for _, networkHost := range networkHosts {
		bundle, err := self.acceptor.Stored(networkHost, rootKeys, networkHosts, self.settings.Now())
		if err != nil {
			code := connect.ExtenderSeedErrorCode(err)
			if code == "" {
				code = connect.ExtenderSeedCodeStore
			}
			if failureCode == "" {
				failureCode = code
			}
			self.log.V(1).Infof("[extender]stored seed refused: %s\n", code)
			continue
		}
		if bundle == nil {
			continue
		}
		inForce = true
		if _, err := connect.ApplyExtenderSeed(self.directory, bundle); err != nil {
			self.log.V(1).Infof("[extender]stored seed apply: %s\n", connect.ExtenderSeedErrorCode(err))
		}
		self.recordBundle(bundle, "")
	}
	return inForce, failureCode
}

// The mirror pass of the settings, or connect's.
func (self *spaceExtenderSeed) mirrorPass(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
	if self.settings.MirrorPass != nil {
		return self.settings.MirrorPass(ctx, request)
	}
	return runExtenderSeedMirrorPass(ctx, self.space, self.settings, request)
}

func (self *spaceExtenderSeed) mirrorWait() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.mirrorBackoff.wait(self.settings.Now())
}

func (self *spaceExtenderSeed) mirrorFailed() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mirrorBackoff.NextFailure(self.settings.Now(), self.settings.MinBackoff, self.settings.MaxBackoff)
}

func (self *spaceExtenderSeed) mirrorSucceeded() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mirrorBackoff.reset()
}

// Records the bundle now in force for the status. `source` empty keeps the
// source already recorded for the same sequence, since a stored bundle came
// from somewhere first.
func (self *spaceExtenderSeed) recordBundle(bundle *connect.ExtenderSeedBundle, source string) {
	if bundle == nil || bundle.NetworkHost != self.primaryNetworkHost() {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		sequence := int64(min(bundle.Sequence, uint64(1<<63-1)))
		if sequence < self.status.Sequence {
			return
		}
		if source == "" {
			if sequence == self.status.Sequence && self.status.Source != "" {
				source = self.status.Source
			} else {
				source = ExtenderSeedSourceStore
			}
		}
		self.status.Sequence = sequence
		self.status.ExpireTime = extenderSeedTimeMillis(bundle.ExpireTime)
		self.status.Source = source
	}()
}

// Forgets every accepted bundle and floor, which is what "Reset extenders"
// leaves (E7): a fresh install's seed state. The status forgets them too. The
// pass in flight is ended and waited out first, so nothing it accepted lands
// after the store is cleared.
func (self *spaceExtenderSeed) reset() {
	if self == nil {
		return
	}
	pass := func() *extenderSeedPass {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.pass
	}()
	if pass != nil {
		pass.cancel()
		<-pass.done
	}
	if err := self.acceptor.Reset(); err != nil {
		self.log.Infof("[extender]seed reset err = %s\n", err)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.status.Sequence = 0
		self.status.ExpireTime = 0
		self.status.Source = ""
		self.mirrorBackoff.reset()
		self.attemptBackoff.reset()
		self.revalidatedRootKeys = nil
	}()
	self.wake.NotifyAll()
}

// The automatic trigger. It acts only while the network client's first
// attempt has ended and the directory has nothing usable, and then at most on
// its backoff. A space with no network client has no dns bootstrap to fail,
// and the trigger sleeps until a settings change builds one; the release
// fetch can still run a pass through the control reach. Every wake also
// revalidates the store when the directory's root keys changed, which is how
// keys a hello installed reach it, and a reset in progress is waited out.
func (self *spaceExtenderSeed) run() {
	after := self.settings.After
	if after == nil {
		after = time.After
	}
	for {
		// subscribed before anything is read, so a change that lands while
		// this pass looks is carried into the wait
		wake := self.wake.NotifyChannel()
		clientChanged := self.space.extenderNodeMonitor.NotifyChannel()
		loopEnabled := !extenderSeedLoopDisabled.Load()
		if loopEnabled {
			self.revalidate(self.directory.RootKeys())
		}
		var clientUpdate chan struct{}
		var timer <-chan time.Time
		if !loopEnabled || self.suspended() {
			// a resume wakes this loop
		} else if networkClient := self.space.getExtenderNetworkClient(); networkClient != nil {
			var status connect.ExtenderNetworkClientStatus
			status, clientUpdate = networkClient.StatusMonitor().Get()
			switch {
			case !status.InitialAttemptDone:
				// the dns bootstrap has not failed yet; the client says when
				// its first attempt ends
			case 0 < self.directory.UsableCount(0):
				// nothing to recover; every usable address going on hold is
				// noticed on the recheck rather than on every change
				timer = after(self.settings.RecheckTimeout)
			default:
				wait := func() time.Duration {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					return self.attemptBackoff.wait(self.settings.Now())
				}()
				if wait <= 0 {
					self.attempt()
					continue
				}
				timer = after(wait)
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-clientChanged:
		case <-clientUpdate:
		case <-timer:
		}
	}
}

// One act of the automatic trigger: ask the control route, and only when it
// does not answer run a pass; when the pass made something usable, ask again
// through it and on an answer request the gated release.
func (self *spaceExtenderSeed) attempt() {
	if err := self.probeControl(); err == nil {
		// the operator answers without a seed
		self.deferAttempt(self.settings.ControlRecheckTimeout)
		return
	}
	select {
	case <-self.ctx.Done():
		return
	default:
	}
	result := self.awaitPass(self.ctx)
	switch {
	case result.outcome == ExtenderSeedPassOutcomeCanceled:
		// an account change, a path change or a close: look again at once,
		// the pass was not a failure
	case result.usable:
		if err := self.probeControl(); err == nil {
			self.resetAttempt()
			self.space.requestExtenderRelease()
		} else {
			self.failAttempt()
		}
	default:
		self.failAttempt()
	}
}

func (self *spaceExtenderSeed) deferAttempt(timeout time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attemptBackoff.postpone(self.settings.Now(), timeout)
}

func (self *spaceExtenderSeed) failAttempt() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attemptBackoff.NextFailure(self.settings.Now(), self.settings.MinBackoff, self.settings.MaxBackoff)
}

func (self *spaceExtenderSeed) resetAttempt() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attemptBackoff.reset()
}

// Asks the control route: `/hello` through the space's client strategy, which
// takes whatever route the strategy has -- direct while the directory is
// empty, the seed's extenders once they are usable.
func (self *spaceExtenderSeed) probeControl() error {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.ControlProbeTimeout)
	defer cancel()
	if self.settings.ControlProbe != nil {
		return self.settings.ControlProbe(ctx)
	}
	return self.space.askExtenderControl(ctx)
}

// The seed state of a space as the apps and the diagnostics show it. Times
// are unix milliseconds, 0 for none.
type ExtenderSeedStatus struct {
	// When the last pass ended, its outcome (ExtenderSeedPassOutcome*), and the
	// seed code of its failure, empty when it did not fail.
	LastPassTime    int64
	LastPassOutcome string
	LastPassError   string
	// The accepted bundle of the space's network host: its sequence, its
	// expiry and where it came from (ExtenderSeedSource*). 0 and empty when
	// there is none.
	Sequence   int64
	ExpireTime int64
	Source     string
	// The bundles this build carries, and the seed code of the last refusal
	// of one -- `expired` for an embedded bundle past its validity -- empty
	// when none was refused.
	EmbeddedCount int
	EmbeddedError string
	// The mirrors of the space, and how many dial without dns.
	MirrorCount               int
	DnsIndependentMirrorCount int
}

// GetExtenderSeedStatus reports the space's seed state. Never nil: a space
// that runs no seed bootstrap reports the zero status.
func (self *NetworkSpace) GetExtenderSeedStatus() *ExtenderSeedStatus {
	seed := self.getExtenderSeed()
	if seed == nil {
		return &ExtenderSeedStatus{}
	}
	seed.stateLock.Lock()
	defer seed.stateLock.Unlock()
	status := seed.status
	return &status
}

// The seed bootstrap of this space, nil when it runs none.
func (self *NetworkSpace) getExtenderSeed() *spaceExtenderSeed {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.extenderSeed
}

// Unix milliseconds, 0 for the zero time.
func extenderSeedTimeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
