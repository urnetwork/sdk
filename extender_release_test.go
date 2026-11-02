package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The gated release fetch and refresh (extender_release.go;
// connect/EXTENDER-UPDATE2.md §1.1, §1.3, §1.4). The fetcher runs its own loop
// against a fake clock, a fake timer that hands the test every wait the loop
// parks on, and a fake release route that hands the test every request and
// answers as the test says. Nothing waits on wall time except a request's own
// deadline in the timeout case.

const testReleaseNetworkHost = "release.example"

type testReleaseClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testReleaseClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testReleaseClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// One request the fetcher made, and the way to answer it.
type testReleasePost struct {
	url   string
	args  extenderReleaseArgs
	byJwt string
	ctx   context.Context
	reply chan testReleaseReply
}

type testReleaseReply struct {
	body []byte
	err  error
}

func (self *testReleasePost) answer(body string) {
	self.reply <- testReleaseReply{body: []byte(body)}
}

func (self *testReleasePost) fail(err error) {
	self.reply <- testReleaseReply{err: err}
}

// What the loop did next: parked on its timer, or made a request. The loop
// does exactly one of the two per pass, so the events are its passes in
// order.
type testReleaseEvent struct {
	parked bool
	post   *testReleasePost
}

type testReleaseMemoryStore struct {
	stateLock  sync.Mutex
	stateBytes []byte
}

func (self *testReleaseMemoryStore) load() ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stateBytes, nil
}

func (self *testReleaseMemoryStore) save(stateBytes []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stateBytes = slices.Clone(stateBytes)
	return nil
}

type testReleaseHarness struct {
	t              *testing.T
	clock          *testReleaseClock
	directory      *connect.ExtenderDirectory
	rootPrivateKey ed25519.PrivateKey
	settings       *extenderReleaseSettings
	store          *testReleaseMemoryStore
	fetcher        *extenderReleaseFetcher

	events chan testReleaseEvent
	// every park waits on this one timer, so a fire reaches whichever park is
	// current, and a fire before the park is kept for it
	fire chan time.Time

	stateLock   sync.Mutex
	byJwt       string
	ipVersions  []int
	reachCount  int
	reachResult func() bool
	// the route answers only when the test says, a cancel notwithstanding,
	// as an answer already on its way does
	ignoreCancel bool
}

func newTestReleaseHarness(t *testing.T, configure func(harness *testReleaseHarness)) *testReleaseHarness {
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

	harness := &testReleaseHarness{
		t:              t,
		clock:          clock,
		directory:      directory,
		rootPrivateKey: rootPrivateKey,
		store:          &testReleaseMemoryStore{},
		events:         make(chan testReleaseEvent, 256),
		fire:           make(chan time.Time, 1),
		ipVersions:     []int{4},
	}
	settings := defaultExtenderReleaseSettings()
	settings.Now = clock.Now
	settings.Random = func() float64 { return 0.5 }
	settings.After = func(wait time.Duration) <-chan time.Time {
		harness.events <- testReleaseEvent{parked: true}
		return harness.fire
	}
	settings.IpVersions = func() []int {
		harness.stateLock.Lock()
		defer harness.stateLock.Unlock()
		return slices.Clone(harness.ipVersions)
	}
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		post := &testReleasePost{
			url:   requestUrl,
			byJwt: byJwt,
			ctx:   ctx,
			reply: make(chan testReleaseReply, 1),
		}
		if err := json.Unmarshal(requestBodyBytes, &post.args); err != nil {
			return nil, err
		}
		harness.stateLock.Lock()
		ignoreCancel := harness.ignoreCancel
		harness.stateLock.Unlock()
		harness.events <- testReleaseEvent{post: post}
		if ignoreCancel {
			reply := <-post.reply
			return reply.body, reply.err
		}
		select {
		case reply := <-post.reply:
			return reply.body, reply.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	harness.settings = settings
	if configure != nil {
		configure(harness)
	}
	controlReach := func(ctx context.Context) bool {
		harness.stateLock.Lock()
		harness.reachCount += 1
		reachResult := harness.reachResult
		harness.stateLock.Unlock()
		if directory.AllowOpenForControlReach() {
			return true
		}
		return reachResult != nil && reachResult()
	}
	harness.fetcher = newExtenderReleaseFetcher(
		ctx,
		directory,
		"https://api."+testReleaseNetworkHost,
		controlReach,
		nil,
		harness.store,
		settings,
		nil,
	)
	harness.fetcher.setEnabled(true)
	t.Cleanup(func() {
		harness.fetcher.Close()
		directory.Close()
		cancel()
	})
	return harness
}

// A client token of one identity, unsigned: the fetcher reads its claims
// without verification, as the operator verifies it.
func testReleaseByJwt(t *testing.T, networkId connect.Id, clientId connect.Id) string {
	t.Helper()
	byJwt, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"network_id": networkId.String(),
		"client_id":  clientId.String(),
		"device_id":  connect.NewId().String(),
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return byJwt
}

func (self *testReleaseHarness) setByJwt(byJwt string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.byJwt = byJwt
}

func (self *testReleaseHarness) credential() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byJwt
}

func (self *testReleaseHarness) setIpVersions(ipVersions ...int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ipVersions = ipVersions
}

func (self *testReleaseHarness) reaches() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.reachCount
}

// The next request, which must come; the parks before it are passed over.
func (self *testReleaseHarness) nextPost() *testReleasePost {
	self.t.Helper()
	for {
		select {
		case event := <-self.events:
			if event.post != nil {
				return event.post
			}
		case <-time.After(10 * time.Second):
			self.t.Fatal("the fetcher made no request")
			return nil
		}
	}
}

// The loop's next pass parks rather than asking, and nothing asks right after.
func (self *testReleaseHarness) requireParked(what string) {
	self.t.Helper()
	select {
	case event := <-self.events:
		if event.post != nil {
			self.t.Fatalf("%s: the fetcher asked again (%v)", what, event.post.args)
		}
	case <-time.After(10 * time.Second):
		self.t.Fatalf("%s: the fetcher did not park", what)
	}
	for {
		select {
		case event := <-self.events:
			if event.post != nil {
				self.t.Fatalf("%s: the fetcher asked again (%v)", what, event.post.args)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

// The wait until the next request is due, as the loop computes it.
func (self *testReleaseHarness) dueIn() time.Duration {
	self.t.Helper()
	wait, due, active := self.fetcher.next()
	if !active {
		self.t.Fatal("the fetcher is inactive")
	}
	if due {
		return 0
	}
	return wait
}

// Moves the clock to the due time and fires the loop's timer.
func (self *testReleaseHarness) advanceToDue() {
	self.t.Helper()
	self.clock.advance(self.dueIn())
	select {
	case self.fire <- self.clock.Now():
	default:
	}
}

// The base64 of one gated record of `extenderKey` at `ips`, issued at
// `issueTime` for a day.
func (self *testReleaseHarness) gatedRecord(extenderKey ed25519.PublicKey, issueTime time.Time, ips ...string) string {
	return self.record(extenderKey, issueTime, connect.ExtenderDirectoryTierGated, ips...)
}

func (self *testReleaseHarness) record(extenderKey ed25519.PublicKey, issueTime time.Time, tier int, ips ...string) string {
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
			Carriers:  []string{connect.ExtenderCarrierTcp},
		})
	}
	record, err := connect.SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:     extenderKey,
		Addresses:     addresses,
		TcpPort:       443,
		IssueTimeMs:   uint64(issueTime.UnixMilli()),
		ExpireTimeMs:  uint64(issueTime.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   testReleaseNetworkHost,
		DirectoryTier: uint32(tier),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	recordBytes, err := proto.Marshal(record)
	if err != nil {
		self.t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(recordBytes)
}

func testReleaseExtenderKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// An answer of contract R.
func testReleaseAnswer(outcome string, retryAfterSeconds int, records ...string) string {
	answer := map[string]any{
		"records":   records,
		"epoch":     1,
		"count":     3,
		"probation": false,
	}
	if outcome != "" {
		answer["outcome"] = outcome
	}
	if 0 < retryAfterSeconds {
		answer["retry_after_seconds"] = retryAfterSeconds
	}
	answerBytes, _ := json.Marshal(answer)
	return string(answerBytes)
}

// The addresses of one connection draw of the family.
func (self *testReleaseHarness) draw(ipVersion int) []string {
	ips := []string{}
	for _, candidate := range self.directory.ConnectCandidates([]int{ipVersion}, 16, false, nil)[0] {
		ips = append(ips, candidate.Ip.String())
	}
	return ips
}

// Applies one verified open record from the feed.
func (self *testReleaseHarness) applyOpen(ip string) {
	self.t.Helper()
	record, err := connect.DecodeExtenderRecordBase64(self.record(testReleaseExtenderKey(self.t), self.clock.Now(), connect.ExtenderDirectoryTierOpen, ip))
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.directory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		self.t.Fatal(err)
	}
}

// The fetch asks for the bound identity's release with its token, names the
// families the client dials and no capability yet, applies the gated record it
// is handed and nothing else, and the connection draw prefers it (§1.1 rules
// 1 and 2). The next refresh is the six-hour interval less the jitter drawn.
func TestExtenderReleaseFetchesAndAppliesTheGatedRelease(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)

	post := harness.nextPost()
	if !strings.HasSuffix(post.url, "/network/extender-release") || post.byJwt != harness.credential() {
		t.Fatalf("the request went to %s with another token", post.url)
	}
	if !slices.Equal(post.args.IpVersions, []int{4}) || len(post.args.Capabilities) != 0 {
		t.Fatalf("the request named %v", post.args)
	}
	post.answer(testReleaseAnswer(
		"ok",
		0,
		harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10"),
		// the route returns gated records only; an open one is refused
		harness.record(testReleaseExtenderKey(t), harness.clock.Now(), connect.ExtenderDirectoryTierOpen, "192.0.2.11"),
		"not a record",
	))
	harness.requireParked("after the answer")
	if wait, expected := harness.dueIn(), 6*time.Hour-15*time.Minute; wait != expected {
		t.Fatalf("the refresh is due in %s, expected the interval less its jitter, %s", wait, expected)
	}
	stats := harness.fetcher.statsValue()
	if stats.RequestCount != 1 || stats.AppliedRecordCount != 1 || stats.InvalidRecordCount != 2 || stats.OutcomeCounts["ok"] != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"192.0.2.10"}) {
		t.Fatalf("the draw after the release was %v", drawn)
	}
}

// Every trigger shares the one request: simultaneous dials, path changes and
// the directory's refresh asks while a request is out are answered by it, and
// a trigger after it waits out the minimum interval (§1.1, §1.3).
func TestExtenderReleaseCoalescesTriggersIntoOneRequest(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)

	post := harness.nextPost()
	var triggers sync.WaitGroup
	for range 32 {
		triggers.Add(1)
		go func() {
			defer triggers.Done()
			harness.fetcher.trigger(extenderReleaseTriggerRequest)
			harness.fetcher.networkChanged()
			harness.draw(4)
		}()
	}
	triggers.Wait()
	post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("triggers while the request was out")
	if count := harness.fetcher.statsValue().RequestCount; count != 1 {
		t.Fatalf("%d requests, expected the triggers to share one", count)
	}

	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.requireParked("a trigger right after the answer")
	if wait := harness.dueIn(); wait != harness.settings.MinRequestInterval {
		t.Fatalf("a trigger is due in %s, expected the minimum interval", wait)
	}
	harness.advanceToDue()
	harness.nextPost().answer(testReleaseAnswer("ok", 0))
	harness.requireParked("after the triggered refresh")
}

// However often a trigger lands, the schedule asks at most the hourly limit,
// never closer than the minimum interval after an answer, and never before a
// retry time (§1.3: early triggers are subject to the same limits).
func TestExtenderReleaseScheduleHoldsItsLimitsAgainstTriggers(t *testing.T) {
	settings := defaultExtenderReleaseSettings()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	summary := connect.ExtenderGatedRecordSummary{CurrentCount: 1, UsableCount: 1, EarliestExpireTime: now.Add(24 * time.Hour)}
	schedule := &extenderReleaseSchedule{}
	requestTimes := []time.Time{}
	for end := now.Add(3 * time.Hour); now.Before(end); now = now.Add(30 * time.Second) {
		schedule.early = true
		if now.Before(schedule.dueTime(now, summary, settings)) {
			continue
		}
		schedule.requested(now, []int{4})
		requestTimes = append(requestTimes, now)
		schedule.answered(now, "ok", true, now.Add(24*time.Hour), 0, settings)
	}
	for i, requestTime := range requestTimes {
		if 0 < i && requestTime.Sub(requestTimes[i-1]) < settings.MinRequestInterval {
			t.Fatalf("requests %s apart", requestTime.Sub(requestTimes[i-1]))
		}
		inWindow := 0
		for _, other := range requestTimes {
			if !other.Before(requestTime) && other.Before(requestTime.Add(settings.RateLimitWindow)) {
				inWindow += 1
			}
		}
		if settings.RateLimitCount < inWindow {
			t.Fatalf("%d requests within an hour of %s", inWindow, requestTime)
		}
	}
	if len(requestTimes) < settings.RateLimitCount {
		t.Fatalf("only %d requests in three hours of triggers", len(requestTimes))
	}

	// a retry time holds against a trigger
	schedule = &extenderReleaseSchedule{}
	schedule.requested(now, []int{4})
	schedule.answered(now, "epoch_spent", false, time.Time{}, 2*time.Hour, settings)
	schedule.early = true
	if due := schedule.dueTime(now, summary, settings); !due.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("a trigger is due at %s, before the retry time", due)
	}
}

// The refresh is scheduled from the earliest expiry of the gated records
// held, never from the disclosure epoch: due no later than the margin before
// it, the jitter only ever bringing it earlier; each refresh is counted on
// time, late, or after the records it replaced expired (§1.3).
func TestExtenderReleaseRefreshesBeforeTheEarliestExpiry(t *testing.T) {
	for _, random := range []float64{0, 0.5, 0.999} {
		harness := newTestReleaseHarness(t, func(harness *testReleaseHarness) {
			harness.settings.Random = func() float64 { return random }
		})
		harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
		harness.fetcher.bind(harness.credential)
		gatedKey := testReleaseExtenderKey(t)
		jitter := time.Duration(random * float64(harness.settings.RefreshJitter))

		// a release of a record issued 22 hours ago: it expires in two
		issueTime := harness.clock.Now().Add(-22 * time.Hour)
		harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, issueTime, "192.0.2.10")))
		harness.requireParked("after the first release")
		deadline := issueTime.Add(24 * time.Hour).Add(-harness.settings.ExpiryMargin)
		if due := harness.clock.Now().Add(harness.dueIn()); !due.Equal(deadline.Add(-jitter)) {
			t.Fatalf("random %v: the refresh is due at %s, expected the margin and the jitter before the expiry, %s", random, due, deadline.Add(-jitter))
		}

		// at the due time it goes, and lands on time
		harness.advanceToDue()
		harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, harness.clock.Now(), "192.0.2.10")))
		harness.requireParked("after the on-time refresh")
		if wait := harness.dueIn(); wait != harness.settings.RefreshInterval-jitter {
			t.Fatalf("random %v: the next refresh is due in %s", random, wait)
		}

		// a refresh held up past the margin is late, and one past the expiry
		// is a gap
		for _, c := range []struct {
			after    time.Duration
			lateness string
		}{
			{after: 23*time.Hour + 30*time.Minute, lateness: extenderReleaseLatenessLate},
			{after: 25 * time.Hour, lateness: extenderReleaseLatenessExpired},
		} {
			harness.clock.advance(c.after)
			harness.fire <- harness.clock.Now()
			harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, harness.clock.Now(), "192.0.2.10")))
			harness.requireParked(c.lateness)
			if count := harness.fetcher.statsValue().LatenessCounts[c.lateness]; count != 1 {
				t.Fatalf("random %v: lateness %v", random, harness.fetcher.statsValue().LatenessCounts)
			}
		}
		// the latest was 25 hours after a release due 23 hours after it
		if stats := harness.fetcher.statsValue(); stats.LatenessCounts[extenderReleaseLatenessOnTime] != 1 || stats.MaxLateness != 2*time.Hour {
			t.Fatalf("random %v: lateness %v, max %s", random, stats.LatenessCounts, stats.MaxLateness)
		}
	}
}

// Each answer that releases nothing leads the connection draw to the verified
// open fallback at once, named by the outcome, and the next request waits the
// operator's retry time clamped to [60 s, 6 h], or a wait of the client's own;
// an unknown outcome is empty, and a request that reached nothing backs off
// and tries a control reach (§1.1 rule 3, contract R).
func TestExtenderReleaseOutcomesFallBackAndWaitTheirTime(t *testing.T) {
	statusErr := &connect.HttpStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	cases := []struct {
		name    string
		body    string
		err     error
		reason  string
		wait    time.Duration
		failure string
	}{
		{name: "empty", body: testReleaseAnswer("empty", 0), reason: connect.ExtenderGatedFallbackReleaseEmpty, wait: time.Hour},
		{name: "ok with nothing", body: testReleaseAnswer("ok", 0), reason: connect.ExtenderGatedFallbackReleaseEmpty, wait: time.Hour},
		{name: "epoch spent soon", body: testReleaseAnswer("epoch_spent", 1), reason: connect.ExtenderGatedFallbackReleaseEpochSpent, wait: 60 * time.Second},
		{name: "probation for long", body: testReleaseAnswer("probation", 1_000_000_000), reason: connect.ExtenderGatedFallbackReleaseProbation, wait: 6 * time.Hour},
		{name: "country capacity", body: testReleaseAnswer("country_capacity", 0), reason: connect.ExtenderGatedFallbackReleaseCountryCapacity, wait: time.Hour},
		{name: "rate limited", body: testReleaseAnswer("rate_limited", 0), reason: connect.ExtenderGatedFallbackReleaseRateLimited, wait: time.Hour},
		{name: "rate limited with a time", body: testReleaseAnswer("rate_limited", 1801), reason: connect.ExtenderGatedFallbackReleaseRateLimited, wait: 1801 * time.Second},
		{name: "incompatible", body: testReleaseAnswer("incompatible", 0), reason: connect.ExtenderGatedFallbackReleaseIncompatible, wait: time.Hour},
		{name: "unavailable", body: testReleaseAnswer("unavailable", 0), reason: connect.ExtenderGatedFallbackReleaseUnavailable, wait: time.Minute},
		{name: "a newer outcome", body: testReleaseAnswer("closed_for_lunch", 0), reason: connect.ExtenderGatedFallbackReleaseEmpty, wait: time.Hour},
		{name: "a legacy refusal", body: `{"records":[],"error":"the identity has reached its release request limit"}`, reason: connect.ExtenderGatedFallbackReleaseUnavailable, wait: time.Minute},
		{name: "a retry time that is no number", body: `{"records":[],"outcome":"epoch_spent","retry_after_seconds":"soon"}`, reason: connect.ExtenderGatedFallbackReleaseEpochSpent, wait: time.Hour},
		{name: "malformed", body: `<html>`, reason: connect.ExtenderGatedFallbackReleaseFailed, wait: time.Minute, failure: extenderReleaseFailureMalformed},
		{name: "status", err: statusErr, reason: connect.ExtenderGatedFallbackReleaseFailed, wait: time.Minute, failure: extenderReleaseFailureStatus},
		{name: "transport", err: errors.New("no route to the operator"), reason: connect.ExtenderGatedFallbackReleaseFailed, wait: time.Minute, failure: extenderReleaseFailureTransport},
	}
	for _, c := range cases {
		harness := newTestReleaseHarness(t, nil)
		harness.applyOpen("198.51.100.1")
		harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
		harness.fetcher.bind(harness.credential)
		post := harness.nextPost()
		if c.err != nil {
			post.fail(c.err)
		} else {
			post.answer(c.body)
		}
		harness.requireParked(c.name)
		if wait := harness.dueIn(); wait != c.wait {
			t.Fatalf("%s: the next request waits %s, expected %s", c.name, wait, c.wait)
		}
		if drawn := harness.draw(4); !slices.Equal(drawn, []string{"198.51.100.1"}) {
			t.Fatalf("%s: the draw was %v, expected the open fallback at once", c.name, drawn)
		}
		if reason := harness.directory.GatedPreference().FallbackReason; reason != c.reason {
			t.Fatalf("%s: the fallback reason is %q, expected %q", c.name, reason, c.reason)
		}
		stats := harness.fetcher.statsValue()
		if c.failure != "" && stats.FailureCounts[c.failure] != 1 {
			t.Fatalf("%s: failures %v", c.name, stats.FailureCounts)
		}
		// only a request that reached nothing tries to regain control reach
		if reached := 0 < harness.reaches(); reached != (c.failure == extenderReleaseFailureTransport) {
			t.Fatalf("%s: control reach tried %t", c.name, reached)
		}
	}

	// a request that outlives its deadline is a timeout, which tries a reach
	harness := newTestReleaseHarness(t, func(harness *testReleaseHarness) {
		harness.settings.RequestTimeout = 50 * time.Millisecond
	})
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)
	harness.nextPost()
	harness.requireParked("timeout")
	if stats := harness.fetcher.statsValue(); stats.FailureCounts[extenderReleaseFailureTimeout] != 1 || harness.reaches() != 1 {
		t.Fatalf("timeout: failures %v, reaches %d", stats.FailureCounts, harness.reaches())
	}
}

// An account change cancels the request that is out and discards its answer,
// even one that arrives anyway; the records released to the previous identity
// are forgotten; and the next request is the new identity's, with its token
// (§1.1 rule 2).
func TestExtenderReleaseAccountChangeDiscardsTheStaleAnswer(t *testing.T) {
	harness := newTestReleaseHarness(t, func(harness *testReleaseHarness) {
		harness.ignoreCancel = true
	})
	firstGeneration, _ := harness.fetcher.accountMonitor.Get()
	byJwtA := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	harness.setByJwt(byJwtA)
	harness.fetcher.bind(harness.credential)
	harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("identity a's release")
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"192.0.2.10"}) {
		t.Fatalf("identity a's release drew %v", drawn)
	}
	boundGeneration, _ := harness.fetcher.accountMonitor.Get()

	// a refresh for identity a is out when the account changes
	harness.advanceToDue()
	post := harness.nextPost()
	if post.byJwt != byJwtA {
		t.Fatal("the refresh was not identity a's")
	}
	byJwtB := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	harness.setByJwt(byJwtB)
	harness.fetcher.credentialChanged()
	if post.ctx.Err() == nil {
		t.Fatal("the account change did not cancel identity a's request")
	}
	if summary := harness.directory.GatedRecordSummary(); summary.CurrentCount != 0 {
		t.Fatal("identity a's gated record survived the account change")
	}
	// its answer arrives anyway, after the change
	post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.20")))

	post = harness.nextPost()
	if post.byJwt != byJwtB {
		t.Fatal("the request after the change was not identity b's")
	}
	if summary := harness.directory.GatedRecordSummary(); summary.CurrentCount != 0 {
		t.Fatal("identity a's late answer was applied after the account change")
	}
	if stats := harness.fetcher.statsValue(); stats.DiscardedCount != 1 {
		t.Fatalf("discarded %d answers", stats.DiscardedCount)
	}
	if generation, _ := harness.fetcher.accountMonitor.Get(); generation != boundGeneration+1 || boundGeneration != firstGeneration+1 {
		t.Fatalf("the account generation went %d, %d, %d", firstGeneration, boundGeneration, generation)
	}
	post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.30")))
	harness.requireParked("identity b's release")
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"192.0.2.30"}) {
		t.Fatalf("identity b's release drew %v", drawn)
	}

	// a refreshed token of identity b is no account change
	generation, _ := harness.fetcher.accountMonitor.Get()
	harness.fetcher.credentialChanged()
	if again, _ := harness.fetcher.accountMonitor.Get(); again != generation {
		t.Fatal("a refresh of the same identity advanced the account generation")
	}
}

// A cold client whose release request reaches nothing regains control reach
// first -- the cached open records, then the seed bootstrap -- and asks again
// at once, without waiting out the backoff; the release it then gets is
// preferred (§1.1 rule 4, §1.4).
func TestExtenderReleaseColdClientReachesControlFirst(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	seedKey := testReleaseExtenderKey(t)
	harness.reachResult = func() bool {
		// the seed bootstrap: a verified open record from the bundle
		record, err := connect.DecodeExtenderRecordBase64(harness.record(seedKey, harness.clock.Now(), connect.ExtenderDirectoryTierOpen, "198.51.100.7"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = harness.directory.ApplyRecord(record, connect.ExtenderSourceBootstrap)
		return err == nil
	}
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)

	requestTime := harness.clock.Now()
	harness.nextPost().fail(errors.New("no route to the operator"))
	post := harness.nextPost()
	if harness.reaches() != 1 {
		t.Fatalf("%d control reaches before the retry", harness.reaches())
	}
	if !harness.clock.Now().Equal(requestTime) {
		t.Fatal("the retry waited out the backoff")
	}
	// the control route is reachable through the seeded open record before
	// any gated record is released
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"198.51.100.7"}) {
		t.Fatalf("the cold draw was %v", drawn)
	}
	post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("the first release")
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"192.0.2.10"}) {
		t.Fatalf("the first gated release drew %v", drawn)
	}
	if stats := harness.fetcher.statsValue(); stats.ControlReachCount != 1 || stats.RequestCount != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

// Outside the cohort the fetch never asks, whatever triggers it, and turning
// the preference on asks at once (rollout gates).
func TestExtenderReleaseAsksNothingOutsideTheCohort(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.fetcher.setEnabled(false)
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)
	for range 8 {
		harness.fetcher.trigger(extenderReleaseTriggerRequest)
		harness.fetcher.networkChanged()
	}
	harness.clock.advance(24 * time.Hour)
	if _, _, active := harness.fetcher.next(); active {
		t.Fatal("the fetch is active outside the cohort")
	}
	select {
	case event := <-harness.events:
		if event.post != nil {
			t.Fatalf("a request outside the cohort: %v", event.post.args)
		}
	case <-time.After(200 * time.Millisecond):
	}
	harness.fetcher.setEnabled(true)
	harness.nextPost()
}

// The request names the families the client dials, so an IPv4-only and an
// IPv6-only client are each released records they can dial. A record whose
// old endpoint disappears before the refresh is recovered by the refresh the
// fallback asks for, and a path that changes the family asks again (§1.3).
func TestExtenderReleaseFollowsTheClientFamilies(t *testing.T) {
	for _, c := range []struct {
		ipVersion int
		other     int
		oldIp     string
		newIp     string
		otherIp   string
	}{
		{ipVersion: 4, other: 6, oldIp: "192.0.2.10", newIp: "192.0.2.11", otherIp: "2001:db8::10"},
		{ipVersion: 6, other: 4, oldIp: "2001:db8::10", newIp: "2001:db8::11", otherIp: "192.0.2.10"},
	} {
		harness := newTestReleaseHarness(t, nil)
		harness.setIpVersions(c.ipVersion)
		harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
		harness.fetcher.bind(harness.credential)
		gatedKey := testReleaseExtenderKey(t)

		post := harness.nextPost()
		if !slices.Equal(post.args.IpVersions, []int{c.ipVersion}) {
			t.Fatalf("v%d: the request named %v", c.ipVersion, post.args.IpVersions)
		}
		post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, harness.clock.Now(), c.oldIp)))
		harness.requireParked("the first release")
		if drawn := harness.draw(c.ipVersion); !slices.Equal(drawn, []string{c.oldIp}) {
			t.Fatalf("v%d: drew %v", c.ipVersion, drawn)
		}
		harness.directory.RecordSuccess(netip.MustParseAddr(c.oldIp), connect.ExtenderConnectModeTcpTls)

		// the old endpoint disappears before the refresh: the fallback asks
		// for a release at once
		harness.clock.advance(time.Hour)
		harness.directory.RecordFailure(netip.MustParseAddr(c.oldIp), connect.ExtenderConnectModeTcpTls)
		harness.draw(c.ipVersion)
		post = harness.nextPost()
		post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, harness.clock.Now(), c.newIp)))
		harness.requireParked("the refreshed endpoint")
		if drawn := harness.draw(c.ipVersion); !slices.Equal(drawn, []string{c.newIp}) {
			t.Fatalf("v%d: after the endpoint was replaced drew %v", c.ipVersion, drawn)
		}

		// the path changes family: the request names the new one
		harness.clock.advance(time.Hour)
		harness.setIpVersions(c.other)
		harness.fetcher.networkChanged()
		post = harness.nextPost()
		if !slices.Equal(post.args.IpVersions, []int{c.other}) {
			t.Fatalf("v%d: after the path change the request named %v", c.ipVersion, post.args.IpVersions)
		}
		post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(gatedKey, harness.clock.Now(), c.otherIp)))
		harness.requireParked("the other family's release")
		if drawn := harness.draw(c.other); !slices.Equal(drawn, []string{c.otherIp}) {
			t.Fatalf("v%d: after the path change drew %v", c.ipVersion, drawn)
		}
	}
}

// A revoked record the release hands back is never a candidate: the
// directory keeps the revocation, the release counts as empty to the policy,
// and the draw falls back (§1.4).
func TestExtenderReleaseNeverDrawsARevokedRecord(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	revokedKey := testReleaseExtenderKey(t)
	revocation, err := connect.SignExtenderRevocation(harness.rootPrivateKey, &protocol.ExtenderRevocationBody{
		PublicKey:   revokedKey,
		IssueTimeMs: uint64(harness.clock.Now().Add(time.Minute).UnixMilli()),
		NetworkHost: testReleaseNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.directory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}
	harness.applyOpen("198.51.100.1")
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)
	harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(revokedKey, harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("the revoked release")
	if drawn := harness.draw(4); !slices.Equal(drawn, []string{"198.51.100.1"}) {
		t.Fatalf("the draw was %v, expected the open fallback and never the revoked record", drawn)
	}
	if reason := harness.directory.GatedPreference().FallbackReason; reason != connect.ExtenderGatedFallbackReleaseEmpty {
		t.Fatalf("reason %q", reason)
	}
}

// A restart of the same identity carries its schedule on, so it does not ask
// again before its refresh is due; another identity starts over and does not
// inherit the records of the first (§1.1, §1.3).
func TestExtenderReleaseRestoresTheScheduleOfTheSameIdentity(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	unbind := harness.fetcher.bind(harness.credential)
	harness.nextPost().answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("the release")
	unbind()
	harness.fetcher.Close()

	// a new process: the same store and directory
	harness.clock.advance(time.Hour)
	restarted := newExtenderReleaseFetcher(
		context.Background(),
		harness.directory,
		"https://api."+testReleaseNetworkHost,
		nil,
		nil,
		harness.store,
		harness.settings,
		nil,
	)
	defer restarted.Close()
	harness.fetcher = restarted
	restarted.setEnabled(true)
	restarted.bind(harness.credential)
	harness.requireParked("a restart of the same identity")
	if wait, expected := harness.dueIn(), 6*time.Hour-15*time.Minute-time.Hour; wait != expected {
		t.Fatalf("the restored refresh is due in %s, expected %s", wait, expected)
	}
	if summary := harness.directory.GatedRecordSummary(); summary.CurrentCount != 1 {
		t.Fatal("a restart of the same identity forgot its gated record")
	}

	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	restarted.credentialChanged()
	harness.nextPost()
	if summary := harness.directory.GatedRecordSummary(); summary.CurrentCount != 0 {
		t.Fatal("another identity inherited the first identity's gated records")
	}
}

// The parser reads contract R tolerantly and clamps the retry time.
func TestExtenderReleaseAnswerIsReadTolerantly(t *testing.T) {
	settings := defaultExtenderReleaseSettings()
	for _, c := range []struct {
		body       string
		outcome    string
		retryAfter time.Duration
		records    int
		ok         bool
	}{
		{body: `{"records":["a"],"epoch":3}`, outcome: "ok", records: 1, ok: true},
		{body: `{"records":[],"outcome":"EPOCH_SPENT","retry_after_seconds":7200,"partition":4}`, outcome: "epoch_spent", retryAfter: 2 * time.Hour, ok: true},
		{body: `{"outcome":"rate_limited","retry_after_seconds":0.5}`, outcome: "rate_limited", retryAfter: 60 * time.Second, ok: true},
		{body: `{"outcome":"probation","retry_after_seconds":1e300}`, outcome: "probation", retryAfter: 6 * time.Hour, ok: true},
		{body: `{"outcome":"empty","retry_after_seconds":-5}`, outcome: "empty", ok: true},
		{body: `{"outcome":17,"records":"x"}`, outcome: "ok", ok: true},
		{body: `{"outcome":"a_newer_one"}`, outcome: "empty", ok: true},
		{body: `[]`, ok: false},
		{body: `null`, ok: false},
		{body: ``, ok: false},
	} {
		answer, err := parseExtenderReleaseAnswer([]byte(c.body), settings)
		if (err == nil) != c.ok {
			t.Fatalf("%s: err %v", c.body, err)
		}
		if err != nil {
			continue
		}
		if answer.outcome != c.outcome || answer.retryAfter != c.retryAfter || len(answer.records) != c.records {
			t.Fatalf("%s: read %+v", c.body, answer)
		}
	}
}

type testControlReachFunc func(ctx context.Context) bool

func (self testControlReachFunc) obtainExtenderControlReach(ctx context.Context) bool {
	return self(ctx)
}

// The space's reaches run in their fixed order, the cached open records
// first, and stop at the first that made something dialable (§1.1 rule 4).
func TestExtenderControlReachTriesTheCachedOpenRecordsFirst(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.applyOpen("198.51.100.1")
	gatedRecord, err := connect.DecodeExtenderRecordBase64(harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.directory.ApplyReleaseRecord(gatedRecord, harness.directory.ResetGeneration()); err != nil {
		t.Fatal(err)
	}
	// a gated attempt holds the open record back
	harness.draw(4)

	networkSpace := &NetworkSpace{
		extenderDirectory: harness.directory,
	}
	calls := []string{}
	add := func(name string, result bool) func() {
		return networkSpace.addExtenderControlReach(testControlReachFunc(func(ctx context.Context) bool {
			calls = append(calls, name)
			return result
		}))
	}
	removeNothing := add("nothing", false)
	add("seed", true)
	ctx := context.Background()
	if !networkSpace.obtainExtenderControlReach(ctx) || 0 < len(calls) {
		t.Fatalf("the cached open record did not answer first: %v", calls)
	}
	if drawn := harness.draw(4); !slices.Contains(drawn, "198.51.100.1") {
		t.Fatalf("the cached open record is still held back: %v", drawn)
	}
	if !networkSpace.obtainExtenderControlReach(ctx) || !slices.Equal(calls, []string{"nothing", "seed"}) {
		t.Fatalf("the added reaches ran %v", calls)
	}
	removeNothing()
	removeNothing()
	calls = nil
	networkSpace.obtainExtenderControlReach(ctx)
	if !slices.Equal(calls, []string{"seed"}) {
		t.Fatalf("after a removal the reaches ran %v", calls)
	}
	// a space with nothing to reach with keeps nothing
	(&NetworkSpace{}).addExtenderControlReach(testControlReachFunc(nil))()
}

// The cohort control is a space value applied in place; a hosted space runs
// no fetch, binds nothing and never turns the preference on.
func TestExtenderReleaseCohortControlIsASpaceValue(t *testing.T) {
	key := &NetworkSpaceKey{HostName: testReleaseNetworkHost}
	previous := &NetworkSpaceValues{}
	next := &NetworkSpaceValues{ExtenderGatedPreference: true}
	if !inPlaceValuesChanged(key, previous, next) || !onlyInPlaceValuesChanged(key, previous, next) {
		t.Fatal("the cohort control is not applied in place")
	}

	harness := newTestReleaseHarness(t, nil)
	harness.directory.SetGatedPreference(nil)
	hosted := &NetworkSpace{
		hostedIncompatible: true,
		extenderDirectory:  harness.directory,
		values:             NetworkSpaceValues{ExtenderGatedPreference: true},
	}
	hosted.startExtenderRelease()
	if hosted.extenderRelease.fetcher.Load() != nil {
		t.Fatal("a hosted space runs a release fetch")
	}
	hosted.applyExtenderGatedPreference(true)
	if harness.directory.GatedPreferenceEnabled() {
		t.Fatal("a hosted space turned the gated preference on")
	}
	hosted.bindExtenderRelease(func() string { return testReleaseByJwt(t, connect.NewId(), connect.NewId()) })()
	hosted.requestExtenderRelease()
	if generation := hosted.extenderAccountMonitor().Value(); generation != 0 {
		t.Fatalf("a hosted space's account generation is %d", generation)
	}
}

// Whatever order the space starts its parts in, nothing is dropped: a reach
// added before the release fetch starts is tried after the cached open
// records, a release request made before it is counted and asks nothing, the
// account monitor is one object for the life of the space, and the fetch
// advances that very monitor when an identity binds and unbinds (§1.1 rules
// 2 and 4).
func TestExtenderReleaseHooksAreReadyBeforeTheFetchStarts(t *testing.T) {
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
	defer cancel()
	directory := connect.NewExtenderDirectory(ctx, directorySettings)
	defer directory.Close()
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey)))

	posts := make(chan string, 16)
	previousConfigure := extenderReleaseConfigure
	extenderReleaseConfigure = func(settings *extenderReleaseSettings) {
		settings.Now = clock.Now
		settings.IpVersions = func() []int { return []int{4} }
		settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			posts <- byJwt
			return []byte(testReleaseAnswer("empty", 0)), nil
		}
	}
	defer func() {
		extenderReleaseConfigure = previousConfigure
	}()

	space := &NetworkSpace{
		ctx:                    ctx,
		apiUrl:                 "https://api." + testReleaseNetworkHost,
		extenderDirectory:      directory,
		clientStrategySettings: connect.DefaultClientStrategySettings(),
		values:                 NetworkSpaceValues{ExtenderGatedPreference: true},
	}
	accountMonitor := space.extenderAccountMonitor()
	calls := []string{}
	space.addExtenderControlReach(testControlReachFunc(func(ctx context.Context) bool {
		calls = append(calls, "seed")
		return true
	}))
	space.requestExtenderRelease()

	space.startExtenderRelease()
	defer space.closeExtenderRelease()
	if space.extenderAccountMonitor() != accountMonitor {
		t.Fatal("the account monitor changed when the fetch started")
	}
	if !space.obtainExtenderControlReach(ctx) || !slices.Equal(calls, []string{"seed"}) {
		t.Fatalf("the reach added before the fetch started ran %v", calls)
	}
	// unbound, a request asks nothing, and is counted all the same
	space.requestExtenderRelease()
	if count := space.extenderReleaseRequestCount(); count != 2 {
		t.Fatalf("%d release requests counted, expected both", count)
	}
	if triggers := space.extenderReleaseStats().TriggerCounts; 0 < len(triggers) {
		t.Fatalf("an unbound space triggered %v", triggers)
	}

	generation, update := accountMonitor.Get()
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	unbind := space.bindExtenderRelease(func() string { return byJwt })
	select {
	case <-update:
	case <-time.After(10 * time.Second):
		t.Fatal("the bind did not advance the account monitor")
	}
	if next := accountMonitor.Value(); next != generation+1 {
		t.Fatalf("the bind moved the account generation from %d to %d", generation, next)
	}
	select {
	case posted := <-posts:
		if posted != byJwt {
			t.Fatal("the release was asked for with another token")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bound identity's release was never asked for")
	}
	unbind()
	if next := accountMonitor.Value(); next != generation+2 {
		t.Fatalf("the unbind left the account generation at %d", next)
	}
}

// The cohort control through the space manager: off by default, with the
// fetch idle; turned on in place, the same space reports the preference and,
// when a connection first needs an extender with no gated record held, the
// verified open fallback and its reason; turned off in place, the space is
// back to the candidate order of every client (rollout gates; §1.1 rule 3).
func TestExtenderReleaseCohortControlThroughTheSpaceManager(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if networkSpace.extenderDirectory == nil || networkSpace.extenderRelease.fetcher.Load() == nil {
		t.Fatal("the space runs no directory or no release fetch")
	}
	if status := networkSpace.GetExtenderStatus(); status.GatedPreference || status.GatedMode != connect.ExtenderGatedModeOff {
		t.Fatalf("the gated preference is on by default: on=%t mode=%q", status.GatedPreference, status.GatedMode)
	}

	enabled := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderGatedPreference = true
	})
	if enabled != networkSpace {
		t.Fatal("turning the gated preference on replaced the space")
	}
	networkSpace.extenderDirectory.ConnectCandidates([]int{4}, 8, false, nil)
	status := networkSpace.GetExtenderStatus()
	if !status.GatedPreference || status.GatedMode != connect.ExtenderGatedModeFallback || status.GatedFallbackReason != connect.ExtenderGatedFallbackNoRecord {
		t.Fatalf("on: on=%t mode=%q reason=%q", status.GatedPreference, status.GatedMode, status.GatedFallbackReason)
	}
	if status.GatedUsableCount != 0 || status.GatedRecordExpireTime != 0 {
		t.Fatalf("on: %d usable gated records expiring at %d, expected none", status.GatedUsableCount, status.GatedRecordExpireTime)
	}

	disabled := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderGatedPreference = false
	})
	if disabled != networkSpace {
		t.Fatal("turning the gated preference off replaced the space")
	}
	if status := networkSpace.GetExtenderStatus(); status.GatedPreference || status.GatedMode != connect.ExtenderGatedModeOff || status.GatedFallbackReason != "" {
		t.Fatalf("off: on=%t mode=%q reason=%q", status.GatedPreference, status.GatedMode, status.GatedFallbackReason)
	}
}

// The schedule is stored beside the directory whole or not at all, and a
// file that does not read as a state of this version is no state: the
// identity starts over rather than inheriting a half-written schedule.
func TestExtenderReleaseStateFileStore(t *testing.T) {
	dir := t.TempDir()
	store := &localStateExtenderReleaseStore{localStorageDir: dir}
	if stateBytes, err := store.load(); err == nil || 0 < len(stateBytes) {
		t.Fatalf("an empty directory read as a state: %q, %v", stateBytes, err)
	}
	stored := []byte(`{"version":1,"identity":"a"}`)
	if err := store.save(stored); err != nil {
		t.Fatal(err)
	}
	if stateBytes, err := store.load(); err != nil || string(stateBytes) != string(stored) {
		t.Fatalf("read back %q, %v", stateBytes, err)
	}
	if err := store.save([]byte(`{"version":1,"identity":"b"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != extenderReleaseStateFileName {
		t.Fatalf("the store left %v beside the state", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != LocalStorageFilePermissions {
		t.Fatalf("the state is %v", info.Mode().Perm())
	}

	for _, stateBytes := range []string{`{"version":1,"identity":`, `{"version":2,"identity":"b"}`, `[]`} {
		if err := os.WriteFile(filepath.Join(dir, extenderReleaseStateFileName), []byte(stateBytes), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		fetcher := &extenderReleaseFetcher{store: store}
		if storedState := fetcher.loadStoredState(); storedState != nil {
			t.Fatalf("%s read as a state: %+v", stateBytes, storedState)
		}
	}
}
