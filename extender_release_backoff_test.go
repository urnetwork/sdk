package sdk

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A release request after control reach is newly established
// (requestExtenderRelease; connect/EXTENDER-UPDATE2.md §1.1 rule 4, §2.3).
//
// Root cause guarded: only the fetch's own walk of the control reaches cleared
// its failure backoff. A seed-led recovery that followed a failed walk called
// requestExtenderRelease, the request stayed held by the backoff the old route
// built up, and the gated release lagged the recovery by up to half an hour.

var errTestReleaseNoRoute = errors.New("no route to the operator")

// Binds an identity and fails its first `count` requests with nothing to walk
// to, so the fetch is parked on the backoff they built.
func testReleaseFailedRequests(t *testing.T, harness *testReleaseHarness, count int) {
	t.Helper()
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)
	for i := range count {
		if 0 < i {
			harness.advanceToDue()
		}
		harness.nextPost().fail(errTestReleaseNoRoute)
		harness.requireParked("a failed request")
	}
}

// The request goes once the spacing allows it, rather than when the backoff
// ends.
func TestExtenderReleaseReachEstablishedClearsTheFailureBackoff(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 3)
	if wait := harness.dueIn(); wait != 4*time.Minute {
		t.Fatalf("the third failure backs off %s", wait)
	}

	harness.clock.advance(90 * time.Second)
	harness.fetcher.trigger(extenderReleaseTriggerRequest)

	post := harness.nextPost()
	post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
	harness.requireParked("the release after the recovery")
	if stats := harness.fetcher.statsValue(); stats.RequestCount != 4 || stats.AppliedRecordCount != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// The 60 s spacing still holds: a recovery seconds after a failure waits out the
// rest of it, and no more.
func TestExtenderReleaseReachEstablishedKeepsTheSpacing(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 3)

	harness.clock.advance(10 * time.Second)
	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.requireParked("a recovery within the spacing")
	if wait, expected := harness.dueIn(), harness.settings.MinRequestInterval-10*time.Second; wait != expected {
		t.Fatalf("the request is due in %s, expected %s", wait, expected)
	}
	harness.advanceToDue()
	harness.nextPost()
}

// The hourly cap still holds: a fifth request within the hour waits for the
// window, whatever established reach.
func TestExtenderReleaseReachEstablishedKeepsTheHourlyCap(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	startTime := harness.clock.Now()
	testReleaseFailedRequests(t, harness, harness.settings.RateLimitCount)

	harness.clock.advance(2 * time.Minute)
	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.requireParked("a recovery past the cap")
	expected := startTime.Add(harness.settings.RateLimitWindow).Sub(harness.clock.Now())
	if wait := harness.dueIn(); wait != expected {
		t.Fatalf("the request is due in %s, expected %s", wait, expected)
	}
	harness.advanceToDue()
	harness.nextPost()
}

// What an answer asked for is the operator's, not a route's, and reach changes
// none of it: a retry time, a limit, an empty answer's own wait, and the wait of
// an operator that could not answer.
func TestExtenderReleaseReachEstablishedKeepsTheOperatorsWait(t *testing.T) {
	for _, test := range []struct {
		name              string
		outcome           string
		retryAfterSeconds int
		wait              time.Duration
	}{
		{name: "a spent epoch", outcome: connect.ExtenderReleaseOutcomeEpochSpent, retryAfterSeconds: 3600, wait: time.Hour},
		{name: "probation", outcome: connect.ExtenderReleaseOutcomeProbation, retryAfterSeconds: 7200, wait: 2 * time.Hour},
		{name: "a limit", outcome: connect.ExtenderReleaseOutcomeRateLimited, retryAfterSeconds: 120, wait: 2 * time.Minute},
		{name: "an empty answer", outcome: connect.ExtenderReleaseOutcomeEmpty, wait: time.Hour},
		{name: "an operator that could not answer", outcome: connect.ExtenderReleaseOutcomeUnavailable, wait: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newTestReleaseHarness(t, nil)
			harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
			harness.fetcher.bind(harness.credential)
			harness.nextPost().answer(testReleaseAnswer(test.outcome, test.retryAfterSeconds))
			harness.requireParked("the answer")
			if wait := harness.dueIn(); wait != test.wait {
				t.Fatalf("the answer waits %s", wait)
			}
			harness.fetcher.trigger(extenderReleaseTriggerRequest)
			harness.requireParked("reach after the answer")
			if wait := harness.dueIn(); wait != test.wait {
				t.Fatalf("reach shortened the operator's wait to %s", wait)
			}
		})
	}
}

// An answer ends what a failure before it left, so reach established after that
// answer finds the operator's wait and leaves it.
func TestExtenderReleaseReachEstablishedAfterAnAnswerKeepsItsWait(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 1)
	harness.advanceToDue()
	harness.nextPost().answer(testReleaseAnswer(connect.ExtenderReleaseOutcomeEpochSpent, 3600))
	harness.requireParked("the answer")
	if wait := harness.dueIn(); wait != time.Hour {
		t.Fatalf("the answer waits %s", wait)
	}
	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.requireParked("reach after the answer")
	if wait := harness.dueIn(); wait != time.Hour {
		t.Fatalf("reach shortened the operator's wait to %s", wait)
	}
}

// A path change and the directory's own ask are triggers, and neither says reach
// was established: the backoff stands.
func TestExtenderReleaseOtherTriggersKeepTheBackoff(t *testing.T) {
	for _, source := range []string{extenderReleaseTriggerPath, extenderReleaseTriggerDirectory} {
		t.Run(source, func(t *testing.T) {
			harness := newTestReleaseHarness(t, nil)
			testReleaseFailedRequests(t, harness, 3)
			harness.clock.advance(90 * time.Second)
			harness.fetcher.trigger(source)
			harness.requireParked("the trigger")
			if wait, expected := harness.dueIn(), 4*time.Minute-90*time.Second; wait != expected {
				t.Fatalf("the request is due in %s, expected %s", wait, expected)
			}
		})
	}
}

// Reach established while a request is out: the request coalesces it, as any
// trigger. If that request then fails, it failed on the route before the reach,
// so the next one goes at once, as after the fetch's own walk, with no walk ahead
// of it and the hourly cap still in force.
func TestExtenderReleaseReachEstablishedWhileARequestIsOut(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 2)
	harness.advanceToDue()
	post := harness.nextPost()

	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.requireNoPost("a trigger while a request is out")
	harness.clock.advance(5 * time.Second)
	retryTime := harness.clock.Now()
	post.fail(errTestReleaseNoRoute)

	post = harness.nextPost()
	if !harness.clock.Now().Equal(retryTime) {
		t.Fatal("the retry waited")
	}
	if reaches := harness.reaches(); reaches != 1 {
		t.Fatalf("%d walks after a failure that reach had overtaken", reaches)
	}

	// that was the fourth request of the hour: the cap holds the next
	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	post.fail(errTestReleaseNoRoute)
	harness.requireParked("past the hourly cap")
	if wait := harness.dueIn(); wait < 50*time.Minute {
		t.Fatalf("the request past the cap is due in %s", wait)
	}
}

// When that request is the first failure of its run, it takes no walk either:
// reach is already there, and a walk would hold the next request for as long as a
// pass runs.
func TestExtenderReleaseReachEstablishedSkipsTheWalk(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)
	post := harness.nextPost()

	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	post.fail(errTestReleaseNoRoute)

	harness.nextPost()
	if reaches := harness.reaches(); reaches != 0 {
		t.Fatalf("%d walks after a failure that reach had overtaken", reaches)
	}
}

// An answer to the request that was out satisfies the trigger it coalesced: no
// request follows it early, whether the answer released a record or asked for a
// wait.
func TestExtenderReleaseReachEstablishedAnsweredByTheRequestOut(t *testing.T) {
	t.Run("a wait", func(t *testing.T) {
		harness := newTestReleaseHarness(t, nil)
		harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
		harness.fetcher.bind(harness.credential)
		post := harness.nextPost()
		harness.fetcher.trigger(extenderReleaseTriggerRequest)
		post.answer(testReleaseAnswer(connect.ExtenderReleaseOutcomeEpochSpent, 3600))
		harness.requireParked("an answer to the request that was out")
		if wait := harness.dueIn(); wait != time.Hour {
			t.Fatalf("the next request is due in %s", wait)
		}
	})
	t.Run("a record", func(t *testing.T) {
		harness := newTestReleaseHarness(t, nil)
		harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
		harness.fetcher.bind(harness.credential)
		post := harness.nextPost()
		harness.fetcher.trigger(extenderReleaseTriggerRequest)
		post.answer(testReleaseAnswer("ok", 0, harness.gatedRecord(testReleaseExtenderKey(t), harness.clock.Now(), "192.0.2.10")))
		harness.requireParked("an answer to the request that was out")
		if wait := harness.dueIn(); wait < time.Hour {
			t.Fatalf("the next request is due in %s", wait)
		}
	})
}

// Reach established while the fetch's own walk runs, by a pass the walk did not
// get the credit of: the same, where it used to be dropped as coalesced.
func TestExtenderReleaseReachEstablishedDuringTheWalk(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	harness.reachResult = func() bool {
		harness.fetcher.trigger(extenderReleaseTriggerRequest)
		return false
	}
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)

	requestTime := harness.clock.Now()
	harness.nextPost().fail(errTestReleaseNoRoute)
	harness.nextPost()
	if !harness.clock.Now().Equal(requestTime) {
		t.Fatal("the retry waited")
	}
}

// Reach established starts a new run of failures: if the request that follows
// fails too, the backoff starts at its minimum and the walk runs again, since
// the route that was found has failed.
func TestExtenderReleaseReachEstablishedStartsANewFailureRun(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 2)
	if reaches := harness.reaches(); reaches != 1 {
		t.Fatalf("%d walks in one run of failures", reaches)
	}

	harness.clock.advance(90 * time.Second)
	harness.fetcher.trigger(extenderReleaseTriggerRequest)
	harness.nextPost().fail(errTestReleaseNoRoute)
	harness.requireParked("a failure after the recovery")
	if wait := harness.dueIn(); wait != harness.settings.RetryMinBackoff {
		t.Fatalf("the failure after the recovery backs off %s", wait)
	}
	if reaches := harness.reaches(); reaches != 2 {
		t.Fatalf("%d walks after a new run of failures began", reaches)
	}
}

// A restart keeps the backoff and what it was about, so the recovery of a client
// that restarted while cut off does not wait it out either.
func TestExtenderReleaseReachEstablishedAfterARestart(t *testing.T) {
	harness := newTestReleaseHarness(t, nil)
	testReleaseFailedRequests(t, harness, 3)
	harness.fetcher.Close()

	harness.clock.advance(90 * time.Second)
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
	if wait, expected := harness.dueIn(), 4*time.Minute-90*time.Second; wait != expected {
		t.Fatalf("the restored backoff is due in %s, expected %s", wait, expected)
	}

	restarted.trigger(extenderReleaseTriggerRequest)
	harness.nextPost()
}

// The space's own call does this, whichever caller makes it: what the seed
// bootstrap and an import use is the trigger the clearing is keyed on.
func TestExtenderReleaseSpaceRequestClearsTheBackoff(t *testing.T) {
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

	posts := make(chan struct{}, 16)
	previousConfigure := extenderReleaseConfigure
	extenderReleaseConfigure = func(settings *extenderReleaseSettings) {
		settings.Now = clock.Now
		settings.IpVersions = func() []int { return []int{4} }
		settings.RetryMinBackoff = 10 * time.Minute
		settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			posts <- struct{}{}
			return nil, errTestReleaseNoRoute
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
	space.startExtenderRelease()
	defer space.closeExtenderRelease()
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer space.bindExtenderRelease(func() string { return byJwt })()

	next := func(what string) {
		t.Helper()
		select {
		case <-posts:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the fetcher made no request", what)
		}
	}
	next("the first request")
	// the request has landed, and the fetch is held by its backoff
	deadline := time.Now().Add(10 * time.Second)
	for space.extenderReleaseStats().FailureCounts[extenderReleaseFailureTransport] != 1 {
		if deadline.Before(time.Now()) {
			t.Fatal("the failure never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	clock.advance(2 * time.Minute)
	space.requestExtenderRelease()
	next("the request after reach was established")
}

// Nothing goes out for a moment.
func (self *testReleaseHarness) requireNoPost(what string) {
	self.t.Helper()
	for {
		select {
		case event := <-self.events:
			if event.post != nil {
				self.t.Fatalf("%s: the fetcher asked (%v)", what, event.post.args)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}
