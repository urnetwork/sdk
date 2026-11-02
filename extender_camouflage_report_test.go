package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The space's side of the camouflage report (extender_camouflage_report.go;
// connect/EXTENDER-SPLICE-ROLLOUT.md). What the sender does on its own is connect's
// to test. These pin what the space gives it: off by default and on and off with the
// cohort control, the identity of the release fetch and its account monitor, the
// space's transport, and never a hosted space or a process that binds nothing.
// A fake extender part stands in for the counters, which are connect's.

// One post of the sender.
type testCamouflageSpacePost struct {
	requestUrl string
	args       *connect.ExtenderCamouflageReportArgs
	byJwt      string
	ctx        context.Context
	reply      chan error
}

// An extender part that always has a verdict to give, and counts its discards.
type testCamouflageSpaceSource struct {
	stateLock sync.Mutex
	spliced   int64
	discards  int
}

func (self *testCamouflageSpaceSource) TakeCamouflageReportCounts() *connect.ExtenderCamouflageExtenderCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	spliced := self.spliced
	self.spliced = 0
	return &connect.ExtenderCamouflageExtenderCounts{Spliced: spliced, DegradedReason: "splice_off"}
}

func (self *testCamouflageSpaceSource) DiscardCamouflageReportCounts() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.discards += 1
	self.spliced = 0
}

func (self *testCamouflageSpaceSource) add(spliced int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.spliced += spliced
}

func (self *testCamouflageSpaceSource) discardCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.discards
}

// The seams of every sender built while a test runs: a fake clock, a timer the test
// fires and watches, and a route the test answers.
type testCamouflageSpaceSeams struct {
	t     *testing.T
	clock *testReleaseClock
	parks chan time.Duration
	fire  chan time.Time
	posts chan *testCamouflageSpacePost
}

func newTestCamouflageSpaceSeams(t *testing.T) *testCamouflageSpaceSeams {
	t.Helper()
	self := &testCamouflageSpaceSeams{
		t:     t,
		clock: &testReleaseClock{now: time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)},
		parks: make(chan time.Duration, 16),
		fire:  make(chan time.Time, 1),
		posts: make(chan *testCamouflageSpacePost, 16),
	}
	previousConfigure := extenderCamouflageReportConfigure
	extenderCamouflageReportConfigure = func(settings *connect.ExtenderCamouflageReporterSettings) {
		settings.Now = self.clock.Now
		settings.After = func(wait time.Duration) <-chan time.Time {
			self.parks <- wait
			return self.fire
		}
		settings.Random = func() float64 { return 0 }
		settings.IpVersions = func() []int { return []int{4} }
		settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			args := &connect.ExtenderCamouflageReportArgs{}
			if err := json.Unmarshal(requestBodyBytes, args); err != nil {
				return nil, err
			}
			post := &testCamouflageSpacePost{
				requestUrl: requestUrl,
				args:       args,
				byJwt:      byJwt,
				ctx:        ctx,
				reply:      make(chan error, 1),
			}
			self.posts <- post
			select {
			case err := <-post.reply:
				if err != nil {
					return nil, err
				}
				return []byte(`{"outcome":"accepted"}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	t.Cleanup(func() {
		extenderCamouflageReportConfigure = previousConfigure
	})
	return self
}

// The next park of the sender.
func (self *testCamouflageSpaceSeams) requireParked(what string) time.Duration {
	self.t.Helper()
	select {
	case wait := <-self.parks:
		return wait
	case post := <-self.posts:
		self.t.Fatalf("%s: a post, want a park: %+v", what, post.args)
	case <-time.After(30 * time.Second):
		self.t.Fatalf("%s: the sender did not park", what)
	}
	return 0
}

// Moves the clock and fires the timer, and the post that makes.
func (self *testCamouflageSpaceSeams) postAfter(wait time.Duration, what string) *testCamouflageSpacePost {
	self.t.Helper()
	self.clock.advance(wait)
	self.fire <- self.clock.Now()
	select {
	case post := <-self.posts:
		return post
	case wait := <-self.parks:
		self.t.Fatalf("%s: a park of %s, want a post", what, wait)
	case <-time.After(30 * time.Second):
		self.t.Fatalf("%s: no post", what)
	}
	return nil
}

// Nothing happens: no timer is set, and no post goes out, within a wait long enough
// for a loop on any load to have taken its look.
func (self *testCamouflageSpaceSeams) requireIdle(what string) {
	self.t.Helper()
	select {
	case wait := <-self.parks:
		self.t.Fatalf("%s: parked for %s", what, wait)
	case post := <-self.posts:
		self.t.Fatalf("%s: posted %+v", what, post.args)
	case <-time.After(500 * time.Millisecond):
	}
}

// A space over a real directory and a release fetch that asks nothing of anyone.
func newTestCamouflageSpace(t *testing.T, gatedPreference bool) *NetworkSpace {
	t.Helper()
	rootSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootSeed)
	if err != nil {
		t.Fatal(err)
	}
	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.NetworkHosts = []string{testReleaseNetworkHost}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	directory := connect.NewExtenderDirectory(ctx, directorySettings)
	t.Cleanup(directory.Close)
	directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPrivateKey.Public().(ed25519.PublicKey)))

	previousConfigure := extenderReleaseConfigure
	extenderReleaseConfigure = func(settings *extenderReleaseSettings) {
		settings.IpVersions = func() []int { return []int{4} }
		settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			return []byte(testReleaseAnswer("empty", 0)), nil
		}
	}
	t.Cleanup(func() {
		extenderReleaseConfigure = previousConfigure
	})
	space := &NetworkSpace{
		ctx:                    ctx,
		apiUrl:                 "https://api." + testReleaseNetworkHost,
		extenderDirectory:      directory,
		clientStrategySettings: connect.DefaultClientStrategySettings(),
		values:                 NetworkSpaceValues{ExtenderGatedPreference: gatedPreference},
	}
	space.startExtenderRelease()
	t.Cleanup(space.closeExtenderRelease)
	space.startExtenderCamouflageReport()
	t.Cleanup(space.closeExtenderCamouflageReport)
	return space
}

// A space outside the cohort sends nothing and sets no timer, with an identity bound
// and an extender's verdict to give.
func TestExtenderCamouflageReportIsOffOutsideTheCohort(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, false)
	source := &testCamouflageSpaceSource{}
	space.setExtenderCamouflageReportSource(source)
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer space.bindExtenderRelease(func() string { return byJwt })()

	seams.requireIdle("outside the cohort")
	if stats := space.extenderCamouflageReportStats(); stats != (connect.ExtenderCamouflageReporterStats{}) {
		t.Errorf("stats = %+v", stats)
	}
}

// The cohort control turns it on and off in place. On, it posts over the identity
// bound to the release fetch, at the route the operator serves, what its source has.
// Off, it cancels what is out, and on again it starts from nothing.
func TestExtenderCamouflageReportFollowsTheCohortControl(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, false)
	source := &testCamouflageSpaceSource{}
	space.setExtenderCamouflageReportSource(source)
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer space.bindExtenderRelease(func() string { return byJwt })()
	seams.requireIdle("off")

	space.applyExtenderGatedPreference(true)
	wait := seams.requireParked("on")
	if wait < 5*time.Minute || 15*time.Minute < wait {
		t.Errorf("first look = %s", wait)
	}
	source.add(5)
	post := seams.postAfter(wait, "the first report")
	if post.requestUrl != "https://api."+testReleaseNetworkHost+"/network/extender-camouflage-report" {
		t.Errorf("url = %q", post.requestUrl)
	}
	if post.byJwt != byJwt {
		t.Errorf("the report went under another credential")
	}
	if post.args.Extender == nil || post.args.Extender.Spliced != 5 || post.args.Extender.DegradedReason != "splice_off" {
		t.Errorf("args = %+v", post.args)
	}
	if post.args.ClientVersion != Version {
		t.Errorf("client version = %q, want the sdk's %q", post.args.ClientVersion, Version)
	}

	space.applyExtenderGatedPreference(false)
	select {
	case <-post.ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the post was not canceled")
	}
	seams.requireIdle("off again")

	source.add(7)
	discards := source.discardCount()
	space.applyExtenderGatedPreference(true)
	wait = seams.requireParked("on again")
	if source.discardCount() <= discards {
		t.Errorf("what was counted while off was kept")
	}
	post = seams.postAfter(wait, "the next report")
	if post.args.Extender.Spliced != 0 {
		t.Errorf("reported %d counted while off", post.args.Extender.Spliced)
	}
	post.reply <- nil
	seams.requireParked("after it")
	if stats := space.extenderCamouflageReportStats(); stats.DeliveredCount != 1 || stats.DroppedCount != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

// An account change through the space's own monitor: the post out is canceled, what
// was counted is dropped, and the next report goes under the new identity.
func TestExtenderCamouflageReportDropsOnAccountChange(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, true)
	source := &testCamouflageSpaceSource{}
	space.setExtenderCamouflageReportSource(source)

	stateLock := sync.Mutex{}
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	credential := func() string {
		stateLock.Lock()
		defer stateLock.Unlock()
		return byJwt
	}
	defer space.bindExtenderRelease(credential)()
	wait := seams.requireParked("bound")

	source.add(5)
	post := seams.postAfter(wait, "the first identity's report")

	otherByJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		byJwt = otherByJwt
	}()
	discards := source.discardCount()
	source.add(3)
	space.extenderReleaseCredentialChanged()
	select {
	case <-post.ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the post was not canceled")
	}
	wait = seams.requireParked("the new identity")
	if source.discardCount() <= discards {
		t.Errorf("the old identity's counts were kept")
	}
	if stats := space.extenderCamouflageReportStats(); stats.DroppedCount != 1 {
		t.Errorf("stats = %+v", stats)
	}

	source.add(1)
	post = seams.postAfter(wait, "the new identity's report")
	if post.byJwt != otherByJwt {
		t.Errorf("the report went under the old credential")
	}
	if post.args.Extender.Spliced != 1 {
		t.Errorf("reported %d, and the new identity did 1", post.args.Extender.Spliced)
	}
	post.reply <- errors.New("no answer")
	seams.requireParked("the retry")
}

// A refresh of the same identity's token is no account change: what is counted stays
// and the report goes under the refreshed token.
func TestExtenderCamouflageReportKeepsCountsAcrossATokenRefresh(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, true)
	source := &testCamouflageSpaceSource{}
	space.setExtenderCamouflageReportSource(source)

	networkId := connect.NewId()
	clientId := connect.NewId()
	stateLock := sync.Mutex{}
	byJwt := testReleaseByJwt(t, networkId, clientId)
	credential := func() string {
		stateLock.Lock()
		defer stateLock.Unlock()
		return byJwt
	}
	defer space.bindExtenderRelease(credential)()
	wait := seams.requireParked("bound")
	discards := source.discardCount()

	source.add(5)
	refreshed := testReleaseByJwt(t, networkId, clientId)
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		byJwt = refreshed
	}()
	space.extenderReleaseCredentialChanged()
	post := seams.postAfter(wait, "the report")
	if post.byJwt != refreshed {
		t.Errorf("the report went under the old token")
	}
	if post.args.Extender.Spliced != 5 || discards != source.discardCount() {
		t.Errorf("spliced = %d, discards = %d, was %d", post.args.Extender.Spliced, source.discardCount(), discards)
	}
}

// Unbound or signed out, nothing goes out, and a token of an identity the binding has
// not seen yet is not sent under.
func TestExtenderCamouflageReportSendsNothingWithoutABoundIdentity(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, true)
	source := &testCamouflageSpaceSource{}
	space.setExtenderCamouflageReportSource(source)
	seams.requireIdle("never bound")

	fetcher := space.extenderRelease.fetcher.Load()
	if byJwt := fetcher.boundByJwt(); byJwt != "" {
		t.Errorf("an unbound fetcher has a credential")
	}

	stateLock := sync.Mutex{}
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	credential := func() string {
		stateLock.Lock()
		defer stateLock.Unlock()
		return byJwt
	}
	unbind := space.bindExtenderRelease(credential)
	seams.requireParked("bound")
	if bound := fetcher.boundByJwt(); bound != byJwt {
		t.Errorf("the bound credential is not the identity's")
	}

	// another identity's token, before anything has told the binding
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		byJwt = testReleaseByJwt(t, connect.NewId(), connect.NewId())
	}()
	if bound := fetcher.boundByJwt(); bound != "" {
		t.Errorf("a token of an identity the binding has not seen was offered")
	}
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		byJwt = ""
	}()
	space.extenderReleaseCredentialChanged()
	seams.requireIdle("signed out")

	unbind()
	if bound := fetcher.boundByJwt(); bound != "" {
		t.Errorf("an unbound fetcher has a credential")
	}
	seams.requireIdle("unbound")
}

// A hosted space runs none, whatever its values say.
func TestExtenderCamouflageReportNeverRunsOnAHostedSpace(t *testing.T) {
	directorySettings := connect.DefaultExtenderDirectorySettings()
	directorySettings.NetworkHosts = []string{testReleaseNetworkHost}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directory := connect.NewExtenderDirectory(ctx, directorySettings)
	defer directory.Close()
	hosted := &NetworkSpace{
		ctx:                ctx,
		hostedIncompatible: true,
		extenderDirectory:  directory,
		values:             NetworkSpaceValues{ExtenderGatedPreference: true},
	}
	hosted.startExtenderRelease()
	hosted.startExtenderCamouflageReport()
	if hosted.extenderCamouflageReport.reporter.Load() != nil {
		t.Fatal("a hosted space runs a sender")
	}
	// and everything that reaches for it does nothing
	hosted.applyExtenderCamouflageReport(true)
	hosted.setExtenderCamouflageReportSource(&testCamouflageSpaceSource{})
	hosted.clearExtenderCamouflageReportSource(nil)
	hosted.closeExtenderCamouflageReport()
	if stats := hosted.extenderCamouflageReportStats(); stats != (connect.ExtenderCamouflageReporterStats{}) {
		t.Errorf("stats = %+v", stats)
	}
}

// The process whose extender store is read-only binds no identity, so it reports
// nothing: the process that writes the directory has the dials.
func TestExtenderCamouflageReportSendsNothingFromAReadOnlyStoreProcess(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	space := newTestCamouflageSpace(t, true)
	space.setExtenderCamouflageReportSource(&testCamouflageSpaceSource{})

	SetExtenderStoreReadOnly(true)
	t.Cleanup(func() { SetExtenderStoreReadOnly(false) })
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer space.bindExtenderRelease(func() string { return byJwt })()
	seams.requireIdle("a read-only store")
}

// The control through the space manager, the way an app sets it, with the sender on
// the space the manager built: off by default and following the value in place, on
// the same space.
func TestExtenderCamouflageReportFollowsTheValueThroughTheSpaceManager(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if networkSpace.extenderCamouflageReport.reporter.Load() == nil {
		t.Fatal("the space runs no sender")
	}
	if networkSpace.valuesCopy().ExtenderGatedPreference {
		t.Fatal("the cohort is on by default")
	}
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer networkSpace.bindExtenderRelease(func() string { return byJwt })()
	seams.requireIdle("off by default")

	enabled := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderGatedPreference = true
	})
	if enabled != networkSpace {
		t.Fatal("turning the cohort on replaced the space")
	}
	seams.requireParked("on")

	disabled := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderGatedPreference = false
	})
	if disabled != networkSpace {
		t.Fatal("turning the cohort off replaced the space")
	}
	seams.requireIdle("off again")
}

// A body holds nothing of who posted it, whatever the space knows.
func TestExtenderCamouflageReportBodyHoldsNoIdentity(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	var capturedBody string
	captured := make(chan struct{})
	previousConfigure := extenderCamouflageReportConfigure
	extenderCamouflageReportConfigure = func(settings *connect.ExtenderCamouflageReporterSettings) {
		previousConfigure(settings)
		post := settings.Post
		settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			capturedBody = string(requestBodyBytes)
			close(captured)
			return post(ctx, requestUrl, requestBodyBytes, byJwt)
		}
	}
	space := newTestCamouflageSpace(t, true)
	space.setExtenderCamouflageReportSource(&testCamouflageSpaceSource{})
	networkId := connect.NewId()
	clientId := connect.NewId()
	byJwt := testReleaseByJwt(t, networkId, clientId)
	defer space.bindExtenderRelease(func() string { return byJwt })()
	post := seams.postAfter(seams.requireParked("bound"), "the report")
	<-captured
	for _, identifier := range []string{byJwt, networkId.String(), clientId.String(), testReleaseNetworkHost} {
		if strings.Contains(capturedBody, identifier) {
			t.Errorf("the body holds %q: %s", identifier, capturedBody)
		}
	}
	post.reply <- nil
	seams.requireParked("after it")
}

// The post goes over the space's own api transport, the one every other call of the
// space takes, so a client whose control route crosses an extender can still report.
func TestExtenderCamouflageReportPostsOverTheSpacesApiTransport(t *testing.T) {
	seams := newTestCamouflageSpaceSeams(t)
	seamsConfigure := extenderCamouflageReportConfigure
	extenderCamouflageReportConfigure = func(settings *connect.ExtenderCamouflageReporterSettings) {
		post := settings.Post
		seamsConfigure(settings)
		settings.Post = post
	}
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderGatedPreference = true
	})
	type transportPost struct {
		requestUrl string
		byJwt      string
	}
	transportPosts := make(chan *transportPost, 8)
	networkSpace.api.setHttpPostRaw(func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		if strings.HasSuffix(requestUrl, connect.ExtenderCamouflageReportPath) {
			transportPosts <- &transportPost{requestUrl: requestUrl, byJwt: byJwt}
			return []byte(`{"outcome":"accepted"}`), nil
		}
		return []byte(testReleaseAnswer("empty", 0)), nil
	})
	networkSpace.setExtenderCamouflageReportSource(&testCamouflageSpaceSource{})
	byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
	defer networkSpace.bindExtenderRelease(func() string { return byJwt })()

	wait := seams.requireParked("bound")
	seams.clock.advance(wait)
	seams.fire <- seams.clock.Now()
	select {
	case post := <-transportPosts:
		if post.byJwt != byJwt {
			t.Errorf("the report went under another credential")
		}
		if !strings.HasPrefix(post.requestUrl, networkSpace.apiUrl) {
			t.Errorf("url = %q, want under %q", post.requestUrl, networkSpace.apiUrl)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the report did not take the api transport")
	}
}
