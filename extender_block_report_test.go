package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The block report sender (extender_block_report.go; connect/EXTENDER-UPDATE2.md
// §1.3). A test steps the sender's own look (`next`) against a fake clock, a fake
// control route, a fake dial and a fake report route, and reads what it did as a
// timeline. What is about the loop itself -- cancellation of an operation in
// flight, the wakes -- runs the loop, with the fake clock still the only one the
// sender reads.

var (
	errTestBlockDial    = errors.New("the dial failed")
	errTestBlockControl = errors.New("the control route does not answer")
	errTestBlockPost    = errors.New("the report got no answer")
)

// One report the sender made.
type testBlockReportPost struct {
	url          string
	publicKeyHex string
	byJwt        string
	elapsed      time.Duration
	ctx          context.Context
}

type testBlockReportHarness struct {
	t              *testing.T
	clock          *testReleaseClock
	startTime      time.Time
	directory      *connect.ExtenderDirectory
	rootPrivateKey ed25519.PrivateKey
	settings       *extenderBlockReportSettings
	accountMonitor *connect.MonitorValue[uint64]
	log            *testBlockReportLogger
	reporter       *extenderBlockReporter
	started        bool

	stateLock sync.Mutex
	byJwt     string
	credBound bool
	identity  extenderReleaseIdentity
	// what happened, in order, by the fake clock
	events []string
	posts  []*testBlockReportPost
	// answers each seam, nil for the default
	controlAnswer func() error
	dialAnswer    func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error
	postAnswer    func(post *testBlockReportPost) ([]byte, error)
}

func newTestBlockReportHarness(t *testing.T) *testBlockReportHarness {
	t.Helper()
	clock := &testReleaseClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	rootSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootSeed)
	if err != nil {
		t.Fatal(err)
	}
	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	directorySettings.NetworkHosts = []string{testReleaseNetworkHost}
	ctx, cancel := context.WithCancel(context.Background())
	directory := connect.NewExtenderDirectory(ctx, directorySettings)
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey)))
	directory.SetGatedPreference(&connect.ExtenderGatedPreferenceSettings{Enabled: true})

	harness := &testBlockReportHarness{
		t:              t,
		clock:          clock,
		startTime:      clock.Now(),
		directory:      directory,
		rootPrivateKey: rootPrivateKey,
		accountMonitor: connect.NewMonitorValue[uint64](0),
		log:            &testBlockReportLogger{},
	}
	settings := defaultExtenderBlockReportSettings()
	settings.Now = clock.Now
	settings.After = func(wait time.Duration) <-chan time.Time {
		return nil
	}
	settings.IpVersions = func() []int { return []int{4, 6} }
	settings.ControlProbe = func(ctx context.Context) error {
		harness.event("control")
		return harness.answerControl()
	}
	settings.Attempt = func(ctx context.Context, suspect connect.ExtenderBlockSuspect) string {
		return directory.AttemptBlockSuspect(ctx, suspect, func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
			harness.event(fmt.Sprintf("dial %s %s", extenderConfig.Ip, extenderConfig.Profile.ConnectMode))
			return harness.answerDial(ctx, extenderConfig)
		})
	}
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		args := &extenderBlockReportArgs{}
		if err := json.Unmarshal(requestBodyBytes, args); err != nil {
			t.Errorf("the report is not json: %s", err)
		}
		post := &testBlockReportPost{
			url:          requestUrl,
			publicKeyHex: args.PublicKeyHex,
			byJwt:        byJwt,
			elapsed:      clock.Now().Sub(harness.startTime),
			ctx:          ctx,
		}
		harness.event("post " + args.PublicKeyHex)
		func() {
			harness.stateLock.Lock()
			defer harness.stateLock.Unlock()
			harness.posts = append(harness.posts, post)
		}()
		return harness.answerPost(post)
	}
	harness.settings = settings
	harness.reporter = buildExtenderBlockReporter(
		ctx,
		directory,
		"https://api."+testReleaseNetworkHost,
		harness.binding,
		harness.accountMonitor,
		settings,
		harness.log,
	)
	t.Cleanup(func() {
		if harness.started {
			harness.reporter.Close()
		}
		directory.Close()
		cancel()
	})
	harness.signIn()
	harness.reporter.setEnabled(true)
	harness.settle()
	return harness
}

func (self *testBlockReportHarness) binding() (func() string, extenderReleaseIdentity) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.credBound {
		return nil, extenderReleaseIdentity{}
	}
	return func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.byJwt
	}, self.identity
}

// Binds a new identity, which is what the release fetch does on a token of
// another network or client, and advances the account monitor.
func (self *testBlockReportHarness) signIn() {
	self.t.Helper()
	byJwt := testReleaseByJwt(self.t, connect.NewId(), connect.NewId())
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = byJwt
		self.credBound = true
		self.identity = parseExtenderReleaseIdentity(byJwt)
	}()
	self.accountMonitor.Update(func(generation uint64) uint64 {
		return generation + 1
	})
}

func (self *testBlockReportHarness) signOut() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = ""
		self.credBound = false
		self.identity = extenderReleaseIdentity{}
	}()
	self.accountMonitor.Update(func(generation uint64) uint64 {
		return generation + 1
	})
}

func (self *testBlockReportHarness) event(event string) {
	elapsed := self.clock.Now().Sub(self.startTime)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, fmt.Sprintf("%s %s", elapsed, event))
}

func (self *testBlockReportHarness) answerControl() error {
	answer := func() func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.controlAnswer
	}()
	if answer == nil {
		return nil
	}
	return answer()
}

func (self *testBlockReportHarness) answerDial(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
	answer := func() func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.dialAnswer
	}()
	if answer == nil {
		return errTestBlockDial
	}
	return answer(ctx, extenderConfig)
}

func (self *testBlockReportHarness) answerPost(post *testBlockReportPost) ([]byte, error) {
	answer := func() func(post *testBlockReportPost) ([]byte, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.postAnswer
	}()
	if answer == nil {
		return []byte(`{"recorded":true}`), nil
	}
	return answer(post)
}

func (self *testBlockReportHarness) setControlAnswer(answer func() error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.controlAnswer = answer
}

func (self *testBlockReportHarness) setDialAnswer(answer func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dialAnswer = answer
}

func (self *testBlockReportHarness) setPostAnswer(answer func(post *testBlockReportPost) ([]byte, error)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postAnswer = answer
}

// What happened so far, and forgets it.
func (self *testBlockReportHarness) takeEvents() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	events := self.events
	self.events = nil
	return events
}

func (self *testBlockReportHarness) reports() []*testBlockReportPost {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.posts)
}

// Does what the sender asks for now, and returns the wait it then asks for, zero
// when it waits for nothing but a change.
func (self *testBlockReportHarness) settle() time.Duration {
	self.t.Helper()
	for range 64 {
		act, wait := self.reporter.next()
		if act == nil {
			return wait
		}
		act()
	}
	self.t.Fatal("the sender never settled: a look that always acts is a storm")
	return 0
}

// Runs the sender for `duration` of the fake clock.
func (self *testBlockReportHarness) runFor(duration time.Duration) {
	self.t.Helper()
	endTime := self.clock.Now().Add(duration)
	for {
		wait := self.settle()
		remaining := endTime.Sub(self.clock.Now())
		if remaining <= 0 {
			return
		}
		if wait <= 0 || remaining < wait {
			wait = remaining
		}
		self.clock.advance(wait)
	}
}

// Signs one record of `tier` for a day, with every carrier on each address.
func (self *testBlockReportHarness) sign(key ed25519.PublicKey, tier int, ips ...string) *protocol.ExtenderRecord {
	self.t.Helper()
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		ipVersion := 4
		if netip.MustParseAddr(ip).Is6() {
			ipVersion = 6
		}
		addresses = append(addresses, &protocol.ExtenderAddress{
			Ip:        ip,
			IpVersion: uint32(ipVersion),
			Carriers:  []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns},
		})
	}
	record, err := connect.SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     key,
		Addresses:     addresses,
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        "x.example.",
		IssueTimeMs:   uint64(self.clock.Now().UnixMilli()),
		ExpireTimeMs:  uint64(self.clock.Now().Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testReleaseNetworkHost,
		DirectoryTier: uint32(tier),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return record
}

// A gated record released to this client at `ips`.
func (self *testBlockReportHarness) released(ips ...string) ed25519.PublicKey {
	self.t.Helper()
	key := testReleaseExtenderKey(self.t)
	record := self.sign(key, connect.ExtenderDirectoryTierGated, ips...)
	if _, err := self.directory.ApplyReleaseRecord(record, self.directory.ResetGeneration()); err != nil {
		self.t.Fatal(err)
	}
	return key
}

// A failed dial of the client's own, of any kind.
func (self *testBlockReportHarness) dialFailed(ip string) {
	self.directory.RecordFailure(netip.MustParseAddr(ip), connect.ExtenderConnectModeTcpTls)
}

// The timeline of one attempt pass on one address at `elapsed`, with the control
// route answering on both sides.
func testBlockReportPassEvents(elapsed time.Duration, ips ...string) []string {
	events := []string{fmt.Sprintf("%s control", elapsed)}
	for _, ip := range ips {
		for _, connectMode := range []connect.ExtenderConnectMode{
			connect.ExtenderConnectModeTcpTls,
			connect.ExtenderConnectModeQuic,
			connect.ExtenderConnectModeDns,
		} {
			events = append(events, fmt.Sprintf("%s dial %s %s", elapsed, ip, connectMode))
		}
	}
	return append(events, fmt.Sprintf("%s control", elapsed))
}

// The whole rule, as a timeline: a failed dial arms, three attempts half an hour
// apart each dial every carrier between two answers of the control route, and
// only then does one report go, with the record's key and the client's token, an
// hour after the first attempt. After it there is nothing more to do.
func TestExtenderBlockReportFollowsThreeAttemptsWithProofOfReach(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1")

	harness.dialFailed("192.0.2.1")
	if wait := harness.settle(); wait != harness.settings.Evidence.AttemptSettleTimeout {
		t.Fatalf("the first attempt waits %s", wait)
	}
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("a failed dial alone did %v", events)
	}

	harness.runFor(2 * time.Hour)

	expected := []string{}
	expected = append(expected, testBlockReportPassEvents(time.Minute, "192.0.2.1")...)
	expected = append(expected, testBlockReportPassEvents(31*time.Minute, "192.0.2.1")...)
	expected = append(expected, testBlockReportPassEvents(61*time.Minute, "192.0.2.1")...)
	expected = append(expected, fmt.Sprintf("%s post %s", 61*time.Minute, hex.EncodeToString(key)))
	if events := harness.takeEvents(); !slices.Equal(events, expected) {
		t.Fatalf("the timeline was\n%s\nexpected\n%s", strings.Join(events, "\n"), strings.Join(expected, "\n"))
	}

	reports := harness.reports()
	if len(reports) != 1 {
		t.Fatalf("%d reports", len(reports))
	}
	if reports[0].url != "https://api."+testReleaseNetworkHost+"/network/extender-block-report" {
		t.Fatalf("the report went to %s", reports[0].url)
	}
	if reports[0].byJwt != harness.byJwt {
		t.Fatal("the report did not carry the bound token")
	}
	stats := harness.reporter.statsValue()
	if stats.PassCount != 3 || stats.CountedAttemptCount != 3 || stats.ControlCount != 6 ||
		stats.AttemptCounts[connect.ExtenderBlockAttemptFailed] != 3 ||
		stats.ReportCounts[extenderBlockReportRecorded] != 1 {
		t.Fatalf("stats %+v", stats)
	}

	// the record is left alone: its next failure arms nothing for a day
	harness.dialFailed("192.0.2.1")
	harness.runFor(6 * time.Hour)
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("after the report the sender did %v", events)
	}
}

// One failed dial is no report, and an attempt the record answers ends it: no
// report, no further dial, and the control route is asked no more.
func TestExtenderBlockReportAnsweredAttemptEndsIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error
		dials  int
	}{
		{
			name: "tcp answers",
			answer: func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
				return nil
			},
			dials: 1,
		},
		{
			name: "dns answers after the rest failed",
			answer: func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
				if extenderConfig.Profile.ConnectMode == connect.ExtenderConnectModeDns {
					return nil
				}
				return errTestBlockDial
			},
			dials: 3,
		},
		{
			name: "the extender limits the dial",
			answer: func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
				return &connect.ExtenderLimitedError{RetryAfter: time.Minute}
			},
			dials: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockReportHarness(t)
			harness.released("192.0.2.1")
			harness.dialFailed("192.0.2.1")
			harness.setDialAnswer(test.answer)
			harness.runFor(4 * time.Hour)

			if reports := harness.reports(); 0 < len(reports) {
				t.Fatalf("a record that answered was reported")
			}
			events := harness.takeEvents()
			if len(events) != 1+test.dials {
				t.Fatalf("the timeline was %v", events)
			}
			stats := harness.reporter.statsValue()
			if stats.AttemptCounts[connect.ExtenderBlockAttemptAnswered] != 1 || stats.CountedAttemptCount != 0 {
				t.Fatalf("stats %+v", stats)
			}
		})
	}

	// the first two attempts failed, and the third is answered: no report
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + harness.settings.Evidence.AttemptInterval)
	harness.setDialAnswer(func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
		return nil
	})
	harness.runFor(4 * time.Hour)
	if reports := harness.reports(); 0 < len(reports) {
		t.Fatal("a record that answered its last attempt was reported")
	}

	// any other dial of the client that the record answers does the same
	other := newTestBlockReportHarness(t)
	other.released("192.0.2.1")
	other.dialFailed("192.0.2.1")
	other.runFor(other.settings.Evidence.AttemptSettleTimeout + other.settings.Evidence.AttemptInterval)
	other.directory.RecordSuccess(netip.MustParseAddr("192.0.2.1"), connect.ExtenderConnectModeQuic)
	other.takeEvents()
	other.runFor(4 * time.Hour)
	if events := other.takeEvents(); 0 < len(events) {
		t.Fatalf("after another dial was answered the sender did %v", events)
	}
}

// A client with no network: the control route does not answer, so nothing is
// dialed, the asks back off, and none of it counts. When the route answers again
// the attempts begin from nothing.
func TestExtenderBlockReportDialsNothingWhileTheControlRouteIsDown(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.setControlAnswer(func() error {
		return errTestBlockControl
	})

	harness.runFor(2 * time.Hour)

	events := harness.takeEvents()
	if len(events) == 0 {
		t.Fatal("the control route was never asked")
	}
	expected := []string{
		"1m0s control",
		"6m0s control",
		"16m0s control",
		"36m0s control",
		"1h16m0s control",
	}
	if !slices.Equal(events, expected) {
		t.Fatalf("offline the sender did %v", events)
	}
	stats := harness.reporter.statsValue()
	if stats.PassCount != 0 || stats.CountedAttemptCount != 0 || 0 < len(harness.reports()) {
		t.Fatalf("stats %+v, %d reports", stats, len(harness.reports()))
	}

	// back: the same suspect, while its window lasts, needs all three
	harness.setControlAnswer(nil)
	harness.runFor(3 * time.Hour)
	if stats := harness.reporter.statsValue(); stats.CountedAttemptCount != 3 {
		t.Fatalf("stats %+v", stats)
	}
	reports := harness.reports()
	if len(reports) != 1 || reports[0].publicKeyHex != hex.EncodeToString(key) {
		t.Fatalf("%d reports", len(reports))
	}

	// a path change starts the asks over at once
	blocked := newTestBlockReportHarness(t)
	blocked.released("192.0.2.1")
	blocked.dialFailed("192.0.2.1")
	blocked.setControlAnswer(func() error {
		return errTestBlockControl
	})
	blocked.runFor(40 * time.Minute)
	blocked.takeEvents()
	blocked.reporter.networkChanged()
	blocked.dialFailed("192.0.2.1")
	blocked.runFor(blocked.settings.Evidence.AttemptSettleTimeout)
	if events := blocked.takeEvents(); len(events) != 1 {
		t.Fatalf("after a path change the sender did %v", events)
	}
}

// A failed attempt counts only if the control route answers after it: a client
// that lost its network during the attempt saw nothing of the extender's.
func TestExtenderBlockReportAttemptNeedsAnAnswerAfterIt(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	answers := 0
	harness.setControlAnswer(func() error {
		answers += 1
		if answers%2 == 0 {
			return errTestBlockControl
		}
		return nil
	})

	harness.runFor(5 * time.Hour)

	stats := harness.reporter.statsValue()
	if stats.PassCount == 0 || stats.CountedAttemptCount != 0 || 0 < len(harness.reports()) {
		t.Fatalf("stats %+v, %d reports", stats, len(harness.reports()))
	}
	if stats.AttemptCounts[connect.ExtenderBlockAttemptFailed] != stats.PassCount {
		t.Fatalf("stats %+v", stats)
	}
}

// A path change drops what was found, and an attempt in flight across it counts
// for nothing (a failure while the network changed never counts).
func TestExtenderBlockReportPathChangeStartsOver(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + harness.settings.Evidence.AttemptInterval)
	if stats := harness.reporter.statsValue(); stats.CountedAttemptCount != 2 {
		t.Fatalf("stats %+v", stats)
	}

	// the third attempt: the path changes during its first dial, which then fails
	harness.setDialAnswer(func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
		harness.reporter.networkChanged()
		return ctx.Err()
	})
	harness.runFor(harness.settings.Evidence.AttemptInterval)
	stats := harness.reporter.statsValue()
	if stats.CountedAttemptCount != 2 || stats.AttemptCounts[connect.ExtenderBlockAttemptAborted] != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if 0 < len(harness.reports()) {
		t.Fatal("a report went on evidence a path change dropped")
	}

	// nothing is left to attempt until a dial fails again
	harness.setDialAnswer(nil)
	harness.takeEvents()
	harness.runFor(4 * time.Hour)
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("with no suspect the sender did %v", events)
	}
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatalf("%d suspects after a path change", look.SuspectCount)
	}

	// and then it takes all three
	harness.dialFailed("192.0.2.1")
	harness.runFor(2 * time.Hour)
	reports := harness.reports()
	if len(reports) != 1 || reports[0].publicKeyHex != hex.EncodeToString(key) {
		t.Fatalf("%d reports", len(reports))
	}

	// the listener is registered with the rest of the process's
	started := newTestBlockReportHarness(t)
	started.reporter = buildExtenderBlockReporter(
		context.Background(),
		started.directory,
		"https://api."+testReleaseNetworkHost,
		started.binding,
		started.accountMonitor,
		started.settings,
		started.log,
	)
	started.reporter.setEnabled(true)
	started.settle()
	started.released("192.0.2.1")
	started.dialFailed("192.0.2.1")
	if look := started.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 1 {
		t.Fatalf("%d suspects before the path change", look.SuspectCount)
	}
	started.reporter.start()
	started.started = true
	connect.NetworkChanged()
	if look := started.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatalf("%d suspects after connect's path change", look.SuspectCount)
	}
}

// An account change forgets what was found, and the next identity has a budget
// of its own and nothing of the last one's to send.
func TestExtenderBlockReportAccountChangeForgets(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + harness.settings.Evidence.AttemptInterval)

	harness.signIn()
	harness.takeEvents()
	harness.runFor(4 * time.Hour)
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("after an account change the sender did %v", events)
	}
	if 0 < len(harness.reports()) {
		t.Fatal("a report went on what the last identity found")
	}

	// a sign-out turns the evidence off altogether
	harness.released("192.0.2.2")
	harness.dialFailed("192.0.2.2")
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 1 {
		t.Fatalf("%d suspects while bound", look.SuspectCount)
	}
	harness.signOut()
	harness.runFor(time.Hour)
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatalf("%d suspects signed out", look.SuspectCount)
	}
	harness.dialFailed("192.0.2.2")
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatal("a sign-out still keeps evidence")
	}
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("signed out the sender did %v", events)
	}
}

// A report the sender is about to send under a token of another identity does
// not go: the binding has not seen the account change yet, and what was found is
// the last identity's.
func TestExtenderBlockReportNeverSendsUnderAnotherIdentitysToken(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.setControlAnswer(nil)

	// through the third attempt, stopping short of the report
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + 2*harness.settings.Evidence.AttemptInterval - time.Second)
	otherByJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	func() {
		harness.stateLock.Lock()
		defer harness.stateLock.Unlock()
		harness.byJwt = otherByJwt
	}()
	harness.runFor(2 * time.Minute)
	if 0 < len(harness.reports()) {
		t.Fatal("a report went out under another identity's token")
	}
}

// The limits of the reports: at most RateLimitCount within any window, at least
// MinRequestInterval apart, however many are due.
func TestExtenderBlockReportIsRateLimited(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.settings.Evidence.MaxAttemptSuspectCount = 8
	harness.settings.Evidence.MaxWindow = 24 * time.Hour
	harness.directory.SetBlockEvidence(harness.settings.Evidence)
	keys := []string{}
	for i := range 8 {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		keys = append(keys, hex.EncodeToString(harness.released(ip)))
		harness.dialFailed(ip)
	}

	harness.runFor(6 * time.Hour)

	reports := harness.reports()
	if len(reports) != len(keys) {
		t.Fatalf("%d reports of %d due", len(reports), len(keys))
	}
	reported := []string{}
	for i, report := range reports {
		reported = append(reported, report.publicKeyHex)
		if 0 < i && report.elapsed-reports[i-1].elapsed < harness.settings.MinRequestInterval {
			t.Fatalf("reports at %s and %s", reports[i-1].elapsed, report.elapsed)
		}
		if harness.settings.RateLimitCount <= i &&
			report.elapsed-reports[i-harness.settings.RateLimitCount].elapsed < harness.settings.RateLimitWindow {
			t.Fatalf("report %d at %s is past the hourly limit from %s", i, report.elapsed, reports[i-harness.settings.RateLimitCount].elapsed)
		}
	}
	slices.Sort(reported)
	slices.Sort(keys)
	if !slices.Equal(reported, keys) {
		t.Fatal("a record was reported twice or not at all")
	}
}

// The limit is per identity: what one spent is not the next one's, while what
// bounds the dials carries over.
func TestExtenderBlockReportBudgetIsPerIdentity(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.settings.RateLimitWindow = 12 * time.Hour
	harness.settings.Evidence.MaxAttemptSuspectCount = 8
	harness.directory.SetBlockEvidence(harness.settings.Evidence)
	ips := []string{}
	for i := range 6 {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		ips = append(ips, ip)
		harness.released(ip)
		harness.dialFailed(ip)
	}
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + 2*harness.settings.Evidence.AttemptInterval)
	harness.runFor(10 * time.Minute)
	if count := len(harness.reports()); count != harness.settings.RateLimitCount {
		t.Fatalf("%d reports up to the limit", count)
	}
	passCount := harness.reporter.statsValue().PassCount

	harness.signIn()
	harness.settle()
	for _, ip := range ips {
		harness.dialFailed(ip)
	}
	// the pass that was due carries over the change: nothing is dialed sooner
	if wait := harness.settle(); harness.settings.Evidence.AttemptInterval < wait || wait <= 0 {
		t.Fatalf("the next identity waits %s", wait)
	}
	if count := harness.reporter.statsValue().PassCount; count != passCount {
		t.Fatalf("the account change brought a pass forward: %d, then %d", passCount, count)
	}
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + 3*harness.settings.Evidence.AttemptInterval)
	reports := harness.reports()
	if len(reports) <= harness.settings.RateLimitCount {
		t.Fatal("the next identity was held by what the last one spent")
	}
	if reports[len(reports)-1].byJwt != harness.byJwt {
		t.Fatal("the next identity's report did not carry its token")
	}
}

// The attempts are bounded the same way: one pass per interval, of a few records,
// with gated records first, however many suspects there are.
func TestExtenderBlockReportBoundsThePasses(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.directory.ConnectCandidates([]int{4}, 8, true, nil)
	for i := range 6 {
		key := testReleaseExtenderKey(t)
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		if _, err := harness.directory.ApplyRecord(harness.sign(key, connect.ExtenderDirectoryTierOpen, ip), connect.ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		harness.directory.ConnectCandidates([]int{4}, 8, false, nil)
		harness.dialFailed(ip)
	}
	harness.clock.advance(time.Second)
	harness.dialFailed("192.0.2.1")

	harness.runFor(time.Hour)

	stats := harness.reporter.statsValue()
	if 3 < stats.PassCount {
		t.Fatalf("%d passes in an hour", stats.PassCount)
	}
	attempts := uint64(0)
	for _, count := range stats.AttemptCounts {
		attempts += count
	}
	if uint64(stats.PassCount)*uint64(harness.settings.Evidence.MaxAttemptSuspectCount) < attempts {
		t.Fatalf("%d attempts in %d passes", attempts, stats.PassCount)
	}
	events := harness.takeEvents()
	if len(events) < 2 || !strings.Contains(events[1], "dial 192.0.2.1 ") {
		t.Fatalf("the gated record does not lead the pass: %v", events)
	}
}

// Records armed at different times share the passes: one every interval however
// many are due at their own times, so what bounds the dials is the pass and not the
// record count.
func TestExtenderBlockReportSpacesThePassesAcrossRecords(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	for i := range 3 {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		harness.released(ip)
		harness.dialFailed(ip)
		harness.runFor(10 * time.Minute)
	}
	harness.runFor(3 * time.Hour)

	if len(harness.reports()) != 3 {
		t.Fatalf("%d reports", len(harness.reports()))
	}
	passTimes := []time.Duration{}
	for _, event := range harness.takeEvents() {
		if !strings.Contains(event, " dial ") {
			continue
		}
		elapsed, err := time.ParseDuration(strings.SplitN(event, " ", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(passTimes) == 0 || passTimes[len(passTimes)-1] != elapsed {
			passTimes = append(passTimes, elapsed)
		}
	}
	if len(passTimes) < 3 {
		t.Fatalf("the passes were at %v", passTimes)
	}
	for i := 1; i < len(passTimes); i += 1 {
		if gap := passTimes[i] - passTimes[i-1]; gap < harness.settings.Evidence.AttemptInterval {
			t.Fatalf("the passes were at %v: %s apart", passTimes, gap)
		}
	}
}

// A failed report is retried on a doubling backoff, within the hourly limit and
// while its evidence stays fresh, and no storm comes of it.
func TestExtenderBlockReportFailedReportBacksOff(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.setPostAnswer(func(post *testBlockReportPost) ([]byte, error) {
		return nil, errTestBlockPost
	})

	// past the third attempt, and the fresh timeout of what it found
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + 2*harness.settings.Evidence.AttemptInterval)
	harness.runFor(harness.settings.Evidence.ReportFreshTimeout)
	reports := harness.reports()
	if len(reports) < 2 {
		t.Fatalf("%d reports", len(reports))
	}
	if gap := reports[1].elapsed - reports[0].elapsed; gap != harness.settings.RetryMinBackoff {
		t.Fatalf("the retry came %s after the failure", gap)
	}
	if 2 < len(reports) {
		if gap := reports[2].elapsed - reports[1].elapsed; gap != 2*harness.settings.RetryMinBackoff {
			t.Fatalf("the second retry came %s after the first", gap)
		}
	}

	// hours of a route that never answers stay under the hourly limit
	harness.runFor(5 * time.Hour)
	reports = harness.reports()
	for i := harness.settings.RateLimitCount; i < len(reports); i += 1 {
		if reports[i].elapsed-reports[i-harness.settings.RateLimitCount].elapsed < harness.settings.RateLimitWindow {
			t.Fatalf("report %d is past the hourly limit", i)
		}
	}
	if stats := harness.reporter.statsValue(); stats.ReportCounts[extenderBlockReportFailed] != uint64(len(reports)) {
		t.Fatalf("stats %+v", stats)
	}

	// the route comes back while the evidence lasts: one report lands, and that
	// ends it
	recovering := newTestBlockReportHarness(t)
	recoveringKey := recovering.released("192.0.2.1")
	recovering.dialFailed("192.0.2.1")
	failures := 0
	recovering.setPostAnswer(func(post *testBlockReportPost) ([]byte, error) {
		failures += 1
		if failures == 1 {
			return nil, errTestBlockPost
		}
		return []byte(`{"recorded":true}`), nil
	})
	recovering.runFor(3 * time.Hour)
	reports = recovering.reports()
	if len(reports) != 2 || reports[1].publicKeyHex != hex.EncodeToString(recoveringKey) {
		t.Fatalf("%d reports", len(reports))
	}
}

// What the operator answers. An answer of any kind ends the record's evidence,
// since another report would say the same, and a refusal holds the next report
// for the window. No answer keeps it to try again.
func TestExtenderBlockReportReadsTheAnswer(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		err     error
		label   string
		quiet   bool
		refused bool
	}{
		{name: "recorded", body: `{"recorded":true}`, label: extenderBlockReportRecorded, quiet: true},
		{name: "an unknown key or an outage", body: `{"recorded":false}`, label: extenderBlockReportNotRecorded, quiet: true},
		{name: "an empty answer", body: `{}`, label: extenderBlockReportNotRecorded, quiet: true},
		{name: "a field it does not know", body: `{"recorded":true,"later":[1]}`, label: extenderBlockReportRecorded, quiet: true},
		{name: "a refusal", body: `{"recorded":false,"error":"too many"}`, label: extenderBlockReportRefused, quiet: true, refused: true},
		{name: "not json", body: `<html>`, label: extenderBlockReportFailed},
		{name: "the wrong shape", body: `[1]`, label: extenderBlockReportFailed},
		{name: "a status", err: &connect.HttpStatusError{StatusCode: 502}, label: extenderBlockReportFailed},
		{name: "a transport failure", err: errTestBlockPost, label: extenderBlockReportFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockReportHarness(t)
			harness.released("192.0.2.1")
			harness.released("192.0.2.2")
			harness.dialFailed("192.0.2.1")
			harness.dialFailed("192.0.2.2")
			reported := 0
			harness.setPostAnswer(func(post *testBlockReportPost) ([]byte, error) {
				reported += 1
				if reported == 1 {
					return []byte(test.body), test.err
				}
				return []byte(`{"recorded":true}`), nil
			})

			// to the first report
			harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + 2*harness.settings.Evidence.AttemptInterval)
			harness.settle()
			if reported != 1 {
				t.Fatalf("%d reports at the first report's time", reported)
			}
			if stats := harness.reporter.statsValue(); stats.ReportCounts[test.label] != 1 {
				t.Fatalf("stats %+v", stats)
			}
			look := harness.directory.BlockEvidenceLook([]int{4, 6})
			if test.quiet && len(look.DuePublicKeys) != 1 {
				t.Fatalf("%d due after an answer", len(look.DuePublicKeys))
			}
			if !test.quiet && len(look.DuePublicKeys) != 2 {
				t.Fatalf("%d due after no answer", len(look.DuePublicKeys))
			}

			// the other record waits for the spacing, the window after a refusal
			wait := harness.settle()
			expected := harness.settings.MinRequestInterval
			if test.refused {
				expected = harness.settings.RateLimitWindow
			} else if !test.quiet {
				expected = harness.settings.RetryMinBackoff
			}
			if wait != expected {
				t.Fatalf("the next report waits %s, expected %s", wait, expected)
			}
		})
	}
}

// Off by default, and off is off: no evidence, no ask, no dial, no report. Turned
// on it works, and turned off in the middle it forgets.
func TestExtenderBlockReportIsOffOutsideTheCohort(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	harness.reporter.setEnabled(false)
	harness.settle()
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.runFor(3 * time.Hour)
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("outside the cohort the sender did %v", events)
	}
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatal("outside the cohort the directory keeps evidence")
	}

	// no bound identity is the same
	harness.reporter.setEnabled(true)
	harness.signOut()
	harness.settle()
	harness.dialFailed("192.0.2.1")
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatal("with no identity the directory keeps evidence")
	}

	// on, part way, and off again
	harness.signIn()
	harness.settle()
	harness.clock.advance(10 * time.Minute)
	key := harness.released("192.0.2.5")
	harness.dialFailed("192.0.2.5")
	harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + harness.settings.Evidence.AttemptInterval)
	if stats := harness.reporter.statsValue(); stats.CountedAttemptCount != 2 {
		t.Fatalf("stats %+v", stats)
	}
	harness.reporter.setEnabled(false)
	harness.settle()
	if look := harness.directory.BlockEvidenceLook([]int{4, 6}); look.SuspectCount != 0 {
		t.Fatal("turning the cohort off left evidence")
	}
	harness.takeEvents()
	harness.runFor(3 * time.Hour)
	if events := harness.takeEvents(); 0 < len(events) {
		t.Fatalf("after the cohort went off the sender did %v", events)
	}

	// on again takes all three
	harness.reporter.setEnabled(true)
	harness.settle()
	harness.dialFailed("192.0.2.5")
	harness.runFor(2 * time.Hour)
	reports := harness.reports()
	if len(reports) != 1 || reports[0].publicKeyHex != hex.EncodeToString(key) {
		t.Fatalf("%d reports", len(reports))
	}
}

// A report never changes what the client trusts or dials: the attempts and the
// report move no hold, count or mode, and the reported record stays a candidate.
func TestExtenderBlockReportLeavesSelectionAlone(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1")
	harness.directory.ConnectCandidates([]int{4}, 8, true, nil)
	harness.dialFailed("192.0.2.1")
	entry := func() connect.ExtenderDirectoryEntry {
		for _, entry := range harness.directory.Snapshot().Entries {
			if entry.Ip == netip.MustParseAddr("192.0.2.1") {
				return *entry
			}
		}
		t.Fatal("the address left the directory")
		return connect.ExtenderDirectoryEntry{}
	}
	before := entry()
	beforeMode := harness.directory.GatedPreference().Mode

	harness.runFor(2 * time.Hour)
	if len(harness.reports()) != 1 {
		t.Fatalf("%d reports", len(harness.reports()))
	}

	after := entry()
	if after.FailureCount != before.FailureCount || after.SuccessCount != before.SuccessCount ||
		!after.LastFailureTime.Equal(before.LastFailureTime) {
		t.Fatalf("the sender moved the evidence of the directory: %+v, then %+v", before, after)
	}
	if mode := harness.directory.GatedPreference().Mode; mode != beforeMode {
		t.Fatalf("the sender moved the policy from %s to %s", beforeMode, mode)
	}
	drawn := harness.directory.ConnectCandidates([]int{4}, 8, true, nil)
	if len(drawn[0]) != 1 || !slices.Equal(drawn[0][0].PublicKey, key) {
		t.Fatalf("a reported record stopped being a candidate: %v", drawn[0])
	}
}

// One logger that keeps every line, at every level.
type testBlockReportLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testBlockReportLogger) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testBlockReportLogger) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "\n")
}

func (self *testBlockReportLogger) Info(args ...any) { self.add(fmt.Sprint(args...)) }
func (self *testBlockReportLogger) Infof(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}
func (self *testBlockReportLogger) Warningf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}
func (self *testBlockReportLogger) Errorf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}
func (self *testBlockReportLogger) V(level int32) connect.Verbose {
	return testBlockReportVerbose{logger: self}
}

type testBlockReportVerbose struct {
	logger *testBlockReportLogger
}

func (self testBlockReportVerbose) Enabled() bool    { return true }
func (self testBlockReportVerbose) Info(args ...any) { self.logger.Info(args...) }
func (self testBlockReportVerbose) Infof(format string, args ...any) {
	self.logger.Infof(format, args...)
}

// At every verbosity, over every path that logs, the sender writes no address,
// no key, no token and no id.
func TestExtenderBlockReportLogsNoAddressOrKey(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1", "2001:db8::1")
	harness.dialFailed("192.0.2.1")
	answers := 0
	harness.setPostAnswer(func(post *testBlockReportPost) ([]byte, error) {
		answers += 1
		switch answers {
		case 1:
			return nil, fmt.Errorf("dial tcp 192.0.2.1:443 %s: refused", hex.EncodeToString(key))
		case 2:
			return []byte(`{"recorded":false,"error":"192.0.2.1 refused"}`), nil
		default:
			return []byte(`{"recorded":true}`), nil
		}
	})
	harness.setControlAnswer(func() error {
		return nil
	})
	harness.runFor(4 * time.Hour)
	harness.reporter.networkChanged()
	harness.signIn()
	harness.settle()

	text := harness.log.text()
	if text == "" {
		t.Fatal("the logger saw nothing, so this test pins nothing")
	}
	for _, secret := range []string{
		"192.0.2.1",
		"2001:db8::1",
		hex.EncodeToString(key),
		harness.byJwt,
		harness.identity.networkId.String(),
		harness.identity.clientId.String(),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("the log carries %q:\n%s", secret, text)
		}
	}
}

// A hosted space builds no sender and turns no evidence on (never report in a
// hosted space).
func TestExtenderBlockReportNeverRunsInAHostedSpace(t *testing.T) {
	clock := &testReleaseClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directory := connect.NewExtenderDirectory(ctx, directorySettings)
	defer directory.Close()

	hosted := &NetworkSpace{
		hostedIncompatible: true,
		extenderDirectory:  directory,
		values:             NetworkSpaceValues{ExtenderGatedPreference: true},
	}
	hosted.startExtenderRelease()
	if hosted.extenderRelease.blockReporter.Load() != nil {
		t.Fatal("a hosted space runs a block report sender")
	}
	hosted.applyExtenderGatedPreference(true)
	hosted.bindExtenderRelease(func() string { return testReleaseByJwt(t, connect.NewId(), connect.NewId()) })()
	if stats := hosted.extenderBlockReportStats(); stats.ControlCount != 0 || 0 < len(stats.ReportCounts) {
		t.Fatalf("a hosted space's sender counted %+v", stats)
	}
	var none *NetworkSpace
	if stats := none.extenderBlockReportStats(); stats.ControlCount != 0 {
		t.Fatalf("a nil space counted %+v", stats)
	}
}

// Waits for `done` on the wall clock, for what the loop does on its own.
func testBlockReportWait(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if deadline.Before(time.Now()) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The loop itself: it starts on the look, wakes on a newly armed suspect, waits
// out each time the look asks for, and joins on close.
func TestExtenderBlockReportRunsOnItsOwnLoop(t *testing.T) {
	harness := newTestBlockReportHarness(t)
	key := harness.released("192.0.2.1")

	waits := make(chan time.Duration, 64)
	fire := make(chan time.Time, 1)
	harness.settings.After = func(wait time.Duration) <-chan time.Time {
		waits <- wait
		return fire
	}
	nextWait := func() time.Duration {
		t.Helper()
		select {
		case wait := <-waits:
			return wait
		case <-time.After(30 * time.Second):
			t.Fatal("the loop never waited")
			return 0
		}
	}
	harness.started = true
	harness.reporter.start()

	// a failed dial wakes it, and it waits for the settle timeout
	harness.dialFailed("192.0.2.1")
	if wait := nextWait(); wait != harness.settings.Evidence.AttemptSettleTimeout {
		t.Fatalf("the loop waits %s after arming", wait)
	}

	for attempt := 1; attempt <= harness.settings.Evidence.MinAttemptCount; attempt += 1 {
		if attempt == 1 {
			harness.clock.advance(harness.settings.Evidence.AttemptSettleTimeout)
		} else {
			harness.clock.advance(harness.settings.Evidence.AttemptInterval)
		}
		fire <- harness.clock.Now()
		testBlockReportWait(t, "the attempt", func() bool {
			return uint64(attempt) == harness.reporter.statsValue().CountedAttemptCount
		})
		if attempt < harness.settings.Evidence.MinAttemptCount {
			if wait := nextWait(); wait != harness.settings.Evidence.AttemptInterval {
				t.Fatalf("the loop waits %s after attempt %d", wait, attempt)
			}
		}
	}
	testBlockReportWait(t, "the report", func() bool {
		return len(harness.reports()) == 1
	})
	if harness.reports()[0].publicKeyHex != hex.EncodeToString(key) {
		t.Fatal("the loop reported another key")
	}
}

// An account change while a report is out cancels it and discards the answer; a
// path change leaves it alone, since what it reports is complete.
func TestExtenderBlockReportAccountChangeCancelsTheReportInFlight(t *testing.T) {
	prepare := func(t *testing.T) (*testBlockReportHarness, chan *testBlockReportPost, chan struct{}) {
		harness := newTestBlockReportHarness(t)
		harness.released("192.0.2.1")
		harness.dialFailed("192.0.2.1")
		out := make(chan *testBlockReportPost, 1)
		release := make(chan struct{})
		harness.setPostAnswer(func(post *testBlockReportPost) ([]byte, error) {
			select {
			case out <- post:
			case <-post.ctx.Done():
				return nil, post.ctx.Err()
			}
			select {
			case <-post.ctx.Done():
				return nil, post.ctx.Err()
			case <-release:
				return []byte(`{"recorded":true}`), nil
			}
		})
		// up to the last moment before the report is due, stepped
		harness.runFor(harness.settings.Evidence.AttemptSettleTimeout + harness.settings.Evidence.AttemptInterval)
		harness.clock.advance(harness.settings.Evidence.AttemptInterval)
		harness.started = true
		harness.reporter.start()
		return harness, out, release
	}
	awaitPost := func(t *testing.T, out chan *testBlockReportPost) *testBlockReportPost {
		t.Helper()
		select {
		case post := <-out:
			return post
		case <-time.After(30 * time.Second):
			t.Fatal("no report went out")
			return nil
		}
	}

	t.Run("an account change", func(t *testing.T) {
		harness, out, _ := prepare(t)
		post := awaitPost(t, out)
		harness.signIn()
		select {
		case <-post.ctx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the account change did not cancel the report")
		}
		testBlockReportWait(t, "the discard", func() bool {
			return harness.reporter.statsValue().ReportCounts[extenderBlockReportDiscarded] == 1
		})
		if stats := harness.reporter.statsValue(); stats.ReportCounts[extenderBlockReportFailed] != 0 {
			t.Fatalf("a canceled report counted as failed: %+v", stats)
		}
		testBlockReportWait(t, "the evidence forgotten", func() bool {
			return harness.directory.BlockEvidenceLook([]int{4, 6}).SuspectCount == 0
		})
	})
	t.Run("a path change", func(t *testing.T) {
		harness, out, release := prepare(t)
		post := awaitPost(t, out)
		harness.reporter.networkChanged()
		select {
		case <-post.ctx.Done():
			t.Fatal("a path change canceled a report whose evidence is complete")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		testBlockReportWait(t, "the answer", func() bool {
			return harness.reporter.statsValue().ReportCounts[extenderBlockReportRecorded] == 1
		})
	})
	t.Run("the cohort going off", func(t *testing.T) {
		harness, out, _ := prepare(t)
		post := awaitPost(t, out)
		harness.reporter.setEnabled(false)
		select {
		case <-post.ctx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the cohort going off did not cancel the report")
		}
		testBlockReportWait(t, "the discard", func() bool {
			return harness.reporter.statsValue().ReportCounts[extenderBlockReportDiscarded] == 1
		})
	})
}

// An account change or a path change while an attempt pass is out ends it, and
// what it found counts for nothing.
func TestExtenderBlockReportChangesCancelThePassInFlight(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(harness *testBlockReportHarness)
	}{
		{
			name: "an account change",
			change: func(harness *testBlockReportHarness) {
				harness.signIn()
			},
		},
		{
			name: "a path change",
			change: func(harness *testBlockReportHarness) {
				harness.reporter.networkChanged()
			},
		},
		{
			name: "the cohort going off",
			change: func(harness *testBlockReportHarness) {
				harness.reporter.setEnabled(false)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestBlockReportHarness(t)
			harness.released("192.0.2.1")
			harness.dialFailed("192.0.2.1")
			dialing := make(chan struct{})
			var dialingOnce sync.Once
			harness.setDialAnswer(func(ctx context.Context, extenderConfig *connect.ExtenderConfig) error {
				dialingOnce.Do(func() { close(dialing) })
				<-ctx.Done()
				return ctx.Err()
			})
			harness.clock.advance(harness.settings.Evidence.AttemptSettleTimeout)
			harness.started = true
			harness.reporter.start()
			select {
			case <-dialing:
			case <-time.After(30 * time.Second):
				t.Fatal("the pass never dialed")
			}
			test.change(harness)
			testBlockReportWait(t, "the abort", func() bool {
				return harness.reporter.statsValue().AttemptCounts[connect.ExtenderBlockAttemptAborted] == 1
			})
			if stats := harness.reporter.statsValue(); stats.CountedAttemptCount != 0 || stats.ControlFailureCount != 0 {
				t.Fatalf("stats %+v", stats)
			}
		})
	}
}
