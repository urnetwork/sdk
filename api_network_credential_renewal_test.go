package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// Renewal of the network credential (api_network_credential_renewal.go): the
// schedule, the request, the compare-and-swap into LocalState, and every
// outcome. The renewer runs on a controlled clock over an in-memory transport,
// so each step is driven by the test and nothing waits on real time but the
// safety timeouts of a step that never comes.

const (
	renewalTestApiUrl    = "https://api.renewal.test"
	renewalTestStepLimit = 15 * time.Second
)

var renewalTestBaseTime = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// A clock that moves only when the test advances it. Every timer the renewer
// arms is reported on armed.
type renewalTestClock struct {
	mutex  sync.Mutex
	now    time.Time
	timers []*renewalTestTimer
	armed  chan time.Duration
}

type renewalTestTimer struct {
	deadline time.Time
	c        chan time.Time
	done     bool
}

func newRenewalTestClock() *renewalTestClock {
	return &renewalTestClock{
		now:   renewalTestBaseTime,
		armed: make(chan time.Duration, 64),
	}
}

func (self *renewalTestClock) Now() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.now
}

func (self *renewalTestClock) After(d time.Duration) (<-chan time.Time, func()) {
	self.mutex.Lock()
	timer := &renewalTestTimer{
		deadline: self.now.Add(d),
		c:        make(chan time.Time, 1),
	}
	self.timers = append(self.timers, timer)
	self.mutex.Unlock()
	self.armed <- d
	return timer.c, func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		timer.done = true
	}
}

// Advance moves the clock and fires the timers that are due.
func (self *renewalTestClock) Advance(d time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.now = self.now.Add(d)
	for _, timer := range self.timers {
		if !timer.done && !self.now.Before(timer.deadline) {
			timer.done = true
			timer.c <- self.now
		}
	}
}

// The next timer the renewer arms.
func (self *renewalTestClock) requireArmed(t *testing.T, want time.Duration) {
	t.Helper()
	select {
	case got := <-self.armed:
		if got != want {
			t.Fatalf("the renewer armed %s, want %s", got, want)
		}
	case <-time.After(renewalTestStepLimit):
		t.Fatalf("the renewer armed no timer (want %s)", want)
	}
}

func (self *renewalTestClock) requireNotArmed(t *testing.T) {
	t.Helper()
	select {
	case got := <-self.armed:
		t.Fatalf("the renewer armed %s, want no timer", got)
	default:
	}
}

// One request that reached the in-memory transport. The test answers it.
type renewalTestRequest struct {
	method string
	url    string
	path   string
	body   string
	byJwt  string
	reply  chan renewalTestReply
}

type renewalTestReply struct {
	body []byte
	err  error
}

func (self renewalTestRequest) answer(body string) {
	self.reply <- renewalTestReply{body: []byte(body)}
}

func (self renewalTestRequest) fail(err error) {
	self.reply <- renewalTestReply{err: err}
}

type renewalTestTransport struct {
	requests chan renewalTestRequest
}

func (self *renewalTestTransport) exchange(ctx context.Context, request renewalTestRequest) ([]byte, error) {
	request.reply = make(chan renewalTestReply, 1)
	request.path = strings.TrimPrefix(request.url, renewalTestApiUrl)
	select {
	case self.requests <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case reply := <-request.reply:
		return reply.body, reply.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (self *renewalTestTransport) post(ctx context.Context, requestUrl string, body []byte, byJwt string) ([]byte, error) {
	return self.exchange(ctx, renewalTestRequest{method: http.MethodPost, url: requestUrl, body: string(body), byJwt: byJwt})
}

func (self *renewalTestTransport) get(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
	return self.exchange(ctx, renewalTestRequest{method: http.MethodGet, url: requestUrl, byJwt: byJwt})
}

func (self *renewalTestTransport) take(t *testing.T) renewalTestRequest {
	t.Helper()
	select {
	case request := <-self.requests:
		return request
	case <-time.After(renewalTestStepLimit):
		t.Fatal("no request reached the transport")
		return renewalTestRequest{}
	}
}

func (self *renewalTestTransport) requireNone(t *testing.T) {
	t.Helper()
	select {
	case request := <-self.requests:
		t.Fatalf("unexpected request %s %s", request.method, request.path)
	default:
	}
}

// A renewal request: POST /auth/network-refresh with the expected network
// credential and an empty object, to this API.
func (self *renewalTestTransport) takeRenewal(t *testing.T, byJwt string) renewalTestRequest {
	t.Helper()
	request := self.take(t)
	connect.AssertEqual(t, request.method, http.MethodPost)
	connect.AssertEqual(t, request.url, renewalTestApiUrl+"/auth/network-refresh")
	connect.AssertEqual(t, request.body, "{}")
	if request.byJwt != byJwt {
		t.Fatalf("the renewal carried %q, want %q", request.byJwt, byJwt)
	}
	return request
}

type renewalTestApi struct {
	ctx       context.Context
	api       *Api
	clock     *renewalTestClock
	transport *renewalTestTransport
	renewed   chan struct{}
	logouts   chan struct{}
}

// An API on a controlled clock and in-memory transport. Its renewer jitters by
// the whole interval, so each backoff is exact.
func newRenewalTestApi(t *testing.T) *renewalTestApi {
	t.Helper()
	ctx, api := newTestApiForURL(t, renewalTestApiUrl)
	transport := &renewalTestTransport{requests: make(chan renewalTestRequest, 64)}
	api.setHttpPostRaw(transport.post)
	api.setHttpGetRaw(transport.get)
	clock := newRenewalTestClock()
	renewed := make(chan struct{}, 64)
	api.networkRenewer.clock = clock
	api.networkRenewer.jitter = func(interval time.Duration) time.Duration {
		return interval
	}
	api.networkRenewer.testingAfterRenew = func() {
		renewed <- struct{}{}
	}
	logouts := make(chan struct{}, 8)
	api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		logouts <- struct{}{}
	}))
	return &renewalTestApi{
		ctx:       ctx,
		api:       api,
		clock:     clock,
		transport: transport,
		renewed:   renewed,
		logouts:   logouts,
	}
}

// Waits for the renewer to finish the attempt the test answered.
func (self *renewalTestApi) requireRenewed(t *testing.T) {
	t.Helper()
	select {
	case <-self.renewed:
	case <-time.After(renewalTestStepLimit):
		t.Fatal("the renewal attempt did not complete")
	}
}

func (self *renewalTestApi) requireNoLogout(t *testing.T) {
	t.Helper()
	select {
	case <-self.logouts:
		t.Fatal("the app was signed out")
	default:
	}
}

func (self *renewalTestApi) renewerStarted() bool {
	self.api.networkRenewer.startLock.Lock()
	defer self.api.networkRenewer.startLock.Unlock()
	return self.api.networkRenewer.started
}

func renewalTestJwt(t *testing.T, claims gojwt.MapClaims) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A network sign-in token of the test network and user, issued at iat and
// expiring at exp. A zero time leaves the claim out.
func renewalTestNetworkJwt(t *testing.T, iat time.Time, exp time.Time, marker string) string {
	claims := gojwt.MapClaims{
		"network_id":   credentialTestNetworkId,
		"user_id":      credentialTestUserId,
		"network_name": "renewal-test",
		"marker":       marker,
	}
	if !iat.IsZero() {
		claims["iat"] = iat.Unix()
	}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	return renewalTestJwt(t, claims)
}

// The token the server answers a renewal with at the clock's now: 30 days.
func renewalTestRenewedJwt(t *testing.T, now time.Time, marker string) string {
	return renewalTestNetworkJwt(t, now, now.Add(30*24*time.Hour), marker)
}

// A LocalState that pairs a network sign-in token with a device's client
// token, the way the apps save them at sign-in.
func newRenewalTestLocalState(t *testing.T, home string, networkJwt string, clientJwt string) *LocalState {
	t.Helper()
	localState := newLocalState(context.Background(), home)
	t.Cleanup(localState.Close)
	if networkJwt != "" {
		if err := localState.SetByJwt(networkJwt); err != nil {
			t.Fatal(err)
		}
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	return localState
}

// Starts a device from the LocalState, as a relaunched app does.
func (self *renewalTestApi) startDevice(t *testing.T, localState *LocalState, clientJwt string) *deviceAuthPublicationGate {
	t.Helper()
	return installTestDeviceByJwt(t, self.api, localState, clientJwt, localState.GetInstanceId())
}

func renewalTestReplyJwt(byJwt string) string {
	return fmt.Sprintf(`{"by_jwt":%q}`, byJwt)
}

func TestNetworkCredentialRenewalSchedule(t *testing.T) {
	now := renewalTestBaseTime
	day := 24 * time.Hour
	for _, c := range []struct {
		name  string
		byJwt string
		want  time.Duration
	}{
		{"half-life", renewalTestNetworkJwt(t, now.Add(-time.Hour), now.Add(-time.Hour+30*day), "a"), 15*day - time.Hour},
		{"past the half-life: the floor", renewalTestNetworkJwt(t, now.Add(-20*day), now.Add(10*day), "b"), minRefreshTimeout},
		{"a short lifetime: the floor", renewalTestNetworkJwt(t, now, now.Add(2*time.Minute), "c"), minRefreshTimeout},
		{"no iat: half of what remains", renewalTestNetworkJwt(t, time.Time{}, now.Add(30*day), "d"), 15 * day},
		{"no exp (legacy): now", renewalTestNetworkJwt(t, now.Add(-400*day), time.Time{}, "e"), 0},
		{"no claims at all (legacy): now", renewalTestNetworkJwt(t, time.Time{}, time.Time{}, "f"), 0},
		{"expired: now", renewalTestNetworkJwt(t, now.Add(-40*day), now.Add(-10*day), "g"), 0},
		{"expiring this instant: now", renewalTestNetworkJwt(t, now.Add(-30*day), now, "h"), 0},
		{"malformed exp: now", renewalTestJwt(t, gojwt.MapClaims{
			"network_id": credentialTestNetworkId,
			"user_id":    credentialTestUserId,
			"exp":        "soon",
		}), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, networkCredentialRenewalTimeout(c.byJwt, now), c.want)
		})
	}
}

// Renewals that answer a token due at once are spaced from minRefreshTimeout,
// doubling up to noExpirationRefreshTimeout.
func TestNetworkRenewalAnomalySpacingIsBounded(t *testing.T) {
	want := minRefreshTimeout
	for consecutive := 1; consecutive <= 20; consecutive += 1 {
		got := networkRenewalAnomalyTimeout(consecutive)
		connect.AssertEqual(t, got, want)
		want = min(2*want, noExpirationRefreshTimeout)
	}
	connect.AssertEqual(t, networkRenewalAnomalyTimeout(0), minRefreshTimeout)
}

// A failed renewal waits at least networkRenewalMinRetryTimeout whatever the
// jitter, and the jitter interval doubles to its cap.
func TestNetworkRenewalRetryBackoffIsBounded(t *testing.T) {
	full := func(interval time.Duration) time.Duration { return interval }
	none := func(interval time.Duration) time.Duration { return 0 }
	excessive := func(interval time.Duration) time.Duration { return 10 * interval }
	negative := func(interval time.Duration) time.Duration { return -interval }
	interval := networkRenewalRetryJitterBase
	for failures := 1; failures <= 20; failures += 1 {
		connect.AssertEqual(t, networkRenewalRetryTimeout(failures, full), networkRenewalMinRetryTimeout+interval)
		connect.AssertEqual(t, networkRenewalRetryTimeout(failures, none), networkRenewalMinRetryTimeout)
		connect.AssertEqual(t, networkRenewalRetryTimeout(failures, excessive), networkRenewalMinRetryTimeout+interval)
		connect.AssertEqual(t, networkRenewalRetryTimeout(failures, negative), networkRenewalMinRetryTimeout)
		interval = min(2*interval, networkRenewalMaxRetryJitter)
	}
	for i := 0; i < 1000; i += 1 {
		jittered := networkRenewalJitter(networkRenewalMaxRetryJitter)
		if jittered < 0 || networkRenewalMaxRetryJitter <= jittered {
			t.Fatalf("jitter %s is outside [0, %s)", jittered, networkRenewalMaxRetryJitter)
		}
	}
	connect.AssertEqual(t, networkRenewalJitter(0), time.Duration(0))
}

// Only a network token renews: never an API key, a client token, or a bearer
// that is not a JWT. A renewal must name the network and user of the token it
// renews and no client.
func TestOnlyANetworkTokenRenews(t *testing.T) {
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime, renewalTestBaseTime.Add(time.Hour), "network")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	connect.AssertEqual(t, renewableNetworkCredential(networkJwt), true)
	connect.AssertEqual(t, renewableNetworkCredential(apiKeyPrefix+"renewal-test-key"), false)
	connect.AssertEqual(t, renewableNetworkCredential(clientJwt), false)
	connect.AssertEqual(t, renewableNetworkCredential("not-a-jwt"), false)
	connect.AssertEqual(t, renewableNetworkCredential(""), false)

	renewedJwt := renewalTestRenewedJwt(t, renewalTestBaseTime.Add(time.Minute), "renewed")
	if err := validateRenewedNetworkJwt(networkJwt, renewedJwt); err != nil {
		t.Fatal(err)
	}
	for name, renewal := range map[string]string{
		"a client token": clientJwt,
		"an api key":     apiKeyPrefix + "renewal-test-key",
		"empty":          "",
		"another network": renewalTestJwt(t, gojwt.MapClaims{
			"network_id": credentialTestOtherNetworkId,
			"user_id":    credentialTestUserId,
		}),
		"another user": renewalTestJwt(t, gojwt.MapClaims{
			"network_id": credentialTestNetworkId,
			"user_id":    "00000000-0000-0000-0000-00000000b002",
		}),
		"no user": renewalTestJwt(t, gojwt.MapClaims{
			"network_id": credentialTestNetworkId,
		}),
	} {
		if validateRenewedNetworkJwt(networkJwt, renewal) == nil {
			t.Errorf("%s was accepted as a renewal", name)
		}
	}
}

// The scheduled renewal sends the network credential, never the device's
// client token, to POST /auth/network-refresh. The renewal replaces the
// network credential in LocalState and in the API and leaves the client
// token, the instance and the device as they are; the admin calls carry it
// from then on, and the next renewal is at its half-life. A relaunch adopts
// it.
func TestNetworkCredentialRenewalRenewsAndPersists(t *testing.T) {
	r := newRenewalTestApi(t)
	home := t.TempDir()
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-20*day), renewalTestBaseTime.Add(10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, home, networkJwt, clientJwt)
	instanceId := localState.GetInstanceId()
	r.startDevice(t, localState, clientJwt)
	connect.AssertEqual(t, r.api.networkCredential(), networkJwt)

	// past its half-life: renewed after the floor
	r.clock.requireArmed(t, minRefreshTimeout)
	r.transport.requireNone(t)
	r.clock.Advance(minRefreshTimeout)
	request := r.transport.takeRenewal(t, networkJwt)
	if request.byJwt == clientJwt {
		t.Fatal("the renewal carried the client token")
	}
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	request.answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 15*day)

	connect.AssertEqual(t, localState.GetByJwt(), renewedJwt)
	connect.AssertEqual(t, localState.GetByClientJwt(), clientJwt)
	connect.AssertEqual(t, localState.GetInstanceId().String(), instanceId.String())
	connect.AssertEqual(t, r.api.networkCredential(), renewedJwt)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, r.api.HasNetworkCredential(), true)
	r.requireNoLogout(t)

	// the admin calls carry the renewal; the client calls the client token
	adminDone := make(chan error, 1)
	go func() {
		adminDone <- awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { r.api.NetworkDelete(cb) })
	}()
	admin := r.transport.take(t)
	connect.AssertEqual(t, admin.path, "/auth/network-delete")
	connect.AssertEqual(t, admin.byJwt, renewedJwt)
	admin.answer("{}")
	<-adminDone
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { r.api.SubscriptionBalance(cb) })
	}()
	client := r.transport.take(t)
	connect.AssertEqual(t, client.path, "/subscription/balance")
	connect.AssertEqual(t, client.byJwt, clientJwt)
	client.answer("{}")
	<-clientDone

	// a relaunch: a new API whose device starts from the same storage
	relaunched := newRenewalTestApi(t)
	relaunchedState := newLocalState(context.Background(), home)
	t.Cleanup(relaunchedState.Close)
	relaunched.startDevice(t, relaunchedState, clientJwt)
	connect.AssertEqual(t, relaunched.api.networkCredential(), renewedJwt)
	// its clock starts again at the base time, before the renewal was issued
	relaunched.clock.requireArmed(t, 15*day+minRefreshTimeout)
}

// An expired or legacy (no exp) sign-in token renews at once, while the
// server still accepts it.
func TestExpiredAndLegacyNetworkTokensRenewAtOnce(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		name       string
		networkJwt string
	}{
		{"expired", renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-90*day), renewalTestBaseTime.Add(-60*day), "expired")},
		{"no exp", renewalTestNetworkJwt(t, time.Time{}, time.Time{}, "legacy")},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRenewalTestApi(t)
			clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
			localState := newRenewalTestLocalState(t, t.TempDir(), c.networkJwt, clientJwt)
			r.startDevice(t, localState, clientJwt)

			request := r.transport.takeRenewal(t, c.networkJwt)
			r.clock.requireNotArmed(t)
			renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
			request.answer(renewalTestReplyJwt(renewedJwt))
			r.requireRenewed(t)
			r.clock.requireArmed(t, 15*day)
			connect.AssertEqual(t, localState.GetByJwt(), renewedJwt)
			connect.AssertEqual(t, r.api.networkCredential(), renewedJwt)
		})
	}
}

// Renewal continues across a device close and a device restart: it belongs to
// the sign-in, not the device.
func TestNetworkCredentialRenewalOutlivesTheDevice(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-time.Hour), renewalTestBaseTime.Add(30*day-time.Hour), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	owner := r.startDevice(t, localState, clientJwt)
	r.clock.requireArmed(t, 15*day-time.Hour)

	r.api.closeDeviceOwner(owner)
	connect.AssertEqual(t, r.api.GetByJwt(), "")
	// the same device starts again: nothing changed, nothing is re-armed
	r.startDevice(t, localState, clientJwt)
	r.clock.requireNotArmed(t)

	r.clock.Advance(15*day - time.Hour)
	request := r.transport.takeRenewal(t, networkJwt)
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	request.answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	connect.AssertEqual(t, localState.GetByJwt(), renewedJwt)
	connect.AssertEqual(t, r.api.networkCredential(), renewedJwt)
}

// An API key never renews: it does not expire, and the server refuses it. A
// client token that a shipped Apple extension stored as by_jwt is not a
// network credential at all.
func TestApiKeyAndClientTokenNeverRenew(t *testing.T) {
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	t.Run("api key", func(t *testing.T) {
		r := newRenewalTestApi(t)
		apiKey := apiKeyPrefix + "renewal-test-key"
		localState := newRenewalTestLocalState(t, t.TempDir(), apiKey, clientJwt)
		r.startDevice(t, localState, clientJwt)
		connect.AssertEqual(t, r.api.networkCredential(), apiKey)
		_, ok := r.api.currentNetworkRenewalTarget()
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, r.renewerStarted(), false)
		r.transport.requireNone(t)
	})
	t.Run("client token stored as by_jwt", func(t *testing.T) {
		r := newRenewalTestApi(t)
		localState := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(localState.Close)
		instanceId := NewId()
		if err := localState.SetByJwt(clientJwt); err != nil {
			t.Fatal(err)
		}
		if err := localState.SetInstanceId(instanceId); err != nil {
			t.Fatal(err)
		}
		installTestDeviceByJwt(t, r.api, localState, clientJwt, instanceId)
		connect.AssertEqual(t, r.api.HasNetworkCredential(), false)
		connect.AssertEqual(t, r.renewerStarted(), false)
		r.transport.requireNone(t)
	})
}

// Without a LocalState that stores the kept token beside the device's client
// token, nothing renews: a sign-in with no device, a device with no
// LocalState, a LocalState that stores another token, a device that holds no
// client token, and a hosted device's session API.
func TestNoRenewalWithoutALocalStateThatStoresTheToken(t *testing.T) {
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	otherJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-50*day), renewalTestBaseTime.Add(-20*day), "other")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")

	t.Run("sign-in, no device", func(t *testing.T) {
		r := newRenewalTestApi(t)
		r.api.SetByJwt(networkJwt)
		connect.AssertEqual(t, r.renewerStarted(), false)
	})
	t.Run("device without LocalState", func(t *testing.T) {
		r := newRenewalTestApi(t)
		r.api.SetByJwt(networkJwt)
		installTestDeviceByJwt(t, r.api, nil, clientJwt, nil)
		connect.AssertEqual(t, r.api.HasNetworkCredential(), true)
		connect.AssertEqual(t, r.renewerStarted(), false)
	})
	t.Run("LocalState stores another token", func(t *testing.T) {
		r := newRenewalTestApi(t)
		localState := newRenewalTestLocalState(t, t.TempDir(), otherJwt, clientJwt)
		r.api.SetByJwt(networkJwt)
		r.startDevice(t, localState, clientJwt)
		// the API keeps its login's token, which LocalState does not back
		connect.AssertEqual(t, r.api.networkCredential(), networkJwt)
		connect.AssertEqual(t, r.renewerStarted(), false)
	})
	t.Run("device holds no client token", func(t *testing.T) {
		// a device that starts from LocalState always holds a client token
		// (parseStartupClientJwt); the rule holds without that check too
		r := newRenewalTestApi(t)
		localState := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(localState.Close)
		prepared := &deviceAuthStartup{
			localState: localState,
			state:      persistedLocalAuthState{Version: localAuthStateVersion, ByJwt: networkJwt},
		}
		r.api.authMutationLock.Lock()
		r.api.mutex.Lock()
		r.api.keepNetworkByJwtForDeviceWithLock(prepared, networkJwt)
		store := r.api.networkByJwtStore
		r.api.mutex.Unlock()
		r.api.authMutationLock.Unlock()
		if store != nil {
			t.Fatal("LocalState backs the network token of a device without a client token")
		}
	})
	t.Run("hosted session API", func(t *testing.T) {
		r := newRenewalTestApi(t)
		localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
		r.startDevice(t, localState, clientJwt)
		request := r.transport.takeRenewal(t, networkJwt)

		session := r.api.newSession(r.ctx)
		t.Cleanup(func() {
			session.Close()
			_ = session.CloseAndWait(context.Background())
		})
		// a hosted device starts without the space's LocalState
		installTestDeviceByJwt(t, session, nil, credentialTestClientJwt(t, credentialTestNetworkId, "hosted"), nil)
		connect.AssertEqual(t, session.HasNetworkCredential(), false)
		session.networkRenewer.startLock.Lock()
		started := session.networkRenewer.started
		session.networkRenewer.startLock.Unlock()
		connect.AssertEqual(t, started, false)

		request.answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, r.clock.Now(), "renewed")))
		r.requireRenewed(t)
	})
}

// A sign-out, a new sign-in, or a replaced sign-in that races a renewal wins:
// the renewal is discarded and nothing it carried is kept or stored.
func TestSignInAndSignOutDuringARenewalWin(t *testing.T) {
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	otherAccountJwt := renewalTestJwt(t, gojwt.MapClaims{
		"network_id": credentialTestOtherNetworkId,
		"user_id":    "00000000-0000-0000-0000-00000000b002",
		"iat":        renewalTestBaseTime.Unix(),
		"exp":        renewalTestBaseTime.Add(30 * day).Unix(),
	})
	newLoginJwt := renewalTestRenewedJwt(t, renewalTestBaseTime, "new-login")

	for _, c := range []struct {
		name string
		// what the app does while the renewal is in flight
		during func(t *testing.T, r *renewalTestApi, localState *LocalState)
		// what the API and LocalState hold after
		wantApi    string
		wantStored string
	}{
		{"sign-out", func(t *testing.T, r *renewalTestApi, localState *LocalState) {
			if err := localState.Logout(); err != nil {
				t.Fatal(err)
			}
			r.api.SetByJwt("")
		}, "", ""},
		{"sign-out of LocalState only", func(t *testing.T, r *renewalTestApi, localState *LocalState) {
			if err := localState.Logout(); err != nil {
				t.Fatal(err)
			}
		}, networkJwt, ""},
		{"sign-in of another account in LocalState", func(t *testing.T, r *renewalTestApi, localState *LocalState) {
			if err := localState.SetByJwt(otherAccountJwt); err != nil {
				t.Fatal(err)
			}
		}, networkJwt, otherAccountJwt},
		{"a new sign-in", func(t *testing.T, r *renewalTestApi, localState *LocalState) {
			if err := localState.SetByJwt(newLoginJwt); err != nil {
				t.Fatal(err)
			}
			r.api.SetByJwt(newLoginJwt)
		}, newLoginJwt, newLoginJwt},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRenewalTestApi(t)
			localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
			r.startDevice(t, localState, clientJwt)
			request := r.transport.takeRenewal(t, networkJwt)

			c.during(t, r, localState)
			renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
			request.answer(renewalTestReplyJwt(renewedJwt))
			r.requireRenewed(t)

			connect.AssertEqual(t, r.api.networkCredential(), c.wantApi)
			connect.AssertEqual(t, localState.GetByJwt(), c.wantStored)
			// the renewal stopped: nothing is armed and nothing is sent
			r.clock.requireNotArmed(t)
			r.transport.requireNone(t)
		})
	}
}

// A 401 is the server rejecting the network token itself. It is dropped from
// the API, so the admin calls fail locally and HasNetworkCredential is false,
// and from LocalState. An unrelated device keeps its client token and the app
// is not signed out. The API does not adopt
// the rejected token again; a new sign-in starts over.
func TestRejectedNetworkCredentialIsDroppedOnly(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	owner := r.startDevice(t, localState, clientJwt)

	request := r.transport.takeRenewal(t, networkJwt)
	request.fail(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"})
	r.requireRenewed(t)

	connect.AssertEqual(t, r.api.HasNetworkCredential(), false)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, localState.GetByJwt(), "")
	connect.AssertEqual(t, localState.GetByClientJwt(), clientJwt)
	r.requireNoLogout(t)
	r.clock.requireNotArmed(t)
	err := awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { r.api.NetworkDelete(cb) })
	if !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
	}
	r.transport.requireNone(t)

	// a device restart does not adopt the rejected token again
	r.api.closeDeviceOwner(owner)
	r.startDevice(t, localState, clientJwt)
	connect.AssertEqual(t, r.api.HasNetworkCredential(), false)
	r.transport.requireNone(t)

	// a new sign-in is renewed again
	newLoginJwt := renewalTestNetworkJwt(t, r.clock.Now().Add(-20*day), r.clock.Now().Add(10*day), "new-login")
	if err := localState.SetByJwt(newLoginJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	r.api.SetByJwt(newLoginJwt)
	r.startDevice(t, localState, clientJwt)
	connect.AssertEqual(t, r.api.networkCredential(), newLoginJwt)
	r.clock.requireArmed(t, minRefreshTimeout)
}

// A refusal in the result is one the server repeats for this token: renewal
// stops and the token stays, in the API and in LocalState. A device restart
// with the same token does not start it again; a new sign-in does.
func TestRefusedRenewalKeepsTheCredential(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply string
	}{
		{"refusal", `{"error":{"message":"API keys are not renewed"}}`},
		{"another identity", "<renewed of another network>"},
		{"a client token", "<client token>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRenewalTestApi(t)
			day := 24 * time.Hour
			networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
			clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
			localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
			owner := r.startDevice(t, localState, clientJwt)

			reply := c.reply
			switch reply {
			case "<renewed of another network>":
				reply = renewalTestReplyJwt(renewalTestJwt(t, gojwt.MapClaims{
					"network_id": credentialTestOtherNetworkId,
					"user_id":    credentialTestUserId,
				}))
			case "<client token>":
				reply = renewalTestReplyJwt(clientJwt)
			}
			request := r.transport.takeRenewal(t, networkJwt)
			request.answer(reply)
			r.requireRenewed(t)

			connect.AssertEqual(t, r.api.networkCredential(), networkJwt)
			connect.AssertEqual(t, localState.GetByJwt(), networkJwt)
			r.requireNoLogout(t)
			r.clock.requireNotArmed(t)

			r.api.closeDeviceOwner(owner)
			r.startDevice(t, localState, clientJwt)
			r.clock.requireNotArmed(t)
			r.transport.requireNone(t)

			r.api.SetByJwt(networkJwt)
			r.startDevice(t, localState, clientJwt)
			r.transport.takeRenewal(t, networkJwt).answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, r.clock.Now(), "renewed")))
			r.requireRenewed(t)
			r.clock.requireArmed(t, 15*day)
		})
	}
}

// Transient failures back off from the floor with doubling jitter, capped;
// a usable transport ends the wait of a failed renewal; another 4xx (a server
// without the route) waits a day. Nothing retries in a loop.
func TestRenewalFailuresBackOff(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)

	transient := []error{
		&connect.HttpStatusError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"},
		&connect.HttpStatusError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"},
		errors.New("dial failed"),
		nil, // a response that is neither a renewal nor a refusal
		&connect.HttpStatusError{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway"},
		&connect.HttpStatusError{StatusCode: http.StatusGatewayTimeout, Status: "504 Gateway Timeout"},
		&connect.HttpStatusError{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error"},
	}
	interval := networkRenewalRetryJitterBase
	for i, err := range transient {
		request := r.transport.takeRenewal(t, networkJwt)
		if err == nil {
			request.answer("{}")
		} else {
			request.fail(err)
		}
		r.requireRenewed(t)
		backoff := networkRenewalMinRetryTimeout + interval
		r.clock.requireArmed(t, backoff)
		r.transport.requireNone(t)
		if i < len(transient)-1 {
			r.clock.Advance(backoff)
		}
		interval = min(2*interval, networkRenewalMaxRetryJitter)
	}
	connect.AssertEqual(t, interval, networkRenewalMaxRetryJitter)

	// a usable transport retries the failed renewal now
	r.api.remoteTransportAvailable()
	request := r.transport.takeRenewal(t, networkJwt)
	request.fail(&connect.HttpStatusError{StatusCode: http.StatusNotFound, Status: "404 Not Found"})
	r.requireRenewed(t)
	r.clock.requireArmed(t, networkRenewalRefusedRetryTimeout)
	// ... but not a refused one
	r.api.remoteTransportAvailable()
	r.clock.requireNotArmed(t)
	r.transport.requireNone(t)

	r.clock.Advance(networkRenewalRefusedRetryTimeout)
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	r.transport.takeRenewal(t, networkJwt).answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 15*day)
	connect.AssertEqual(t, localState.GetByJwt(), renewedJwt)
	connect.AssertEqual(t, r.api.HasNetworkCredential(), true)
	r.requireNoLogout(t)
}

// A renewal that answers a token due at once (no exp from a server, or a clock
// far ahead of the server's) is spaced, doubling, rather than repeated.
func TestRenewalsOfTokensDueAtOnceAreSpaced(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)

	byJwt := networkJwt
	want := minRefreshTimeout
	for i := 0; i < 8; i += 1 {
		request := r.transport.takeRenewal(t, byJwt)
		now := r.clock.Now()
		if i%2 == 0 {
			// no exp
			byJwt = renewalTestNetworkJwt(t, now, time.Time{}, fmt.Sprintf("no-exp-%d", i))
		} else {
			// already expired by this clock
			byJwt = renewalTestNetworkJwt(t, now.Add(-31*day), now.Add(-day), fmt.Sprintf("expired-%d", i))
		}
		request.answer(renewalTestReplyJwt(byJwt))
		r.requireRenewed(t)
		r.clock.requireArmed(t, want)
		connect.AssertEqual(t, localState.GetByJwt(), byJwt)
		r.clock.Advance(want)
		want = min(2*want, noExpirationRefreshTimeout)
	}

	// a normal token ends the spacing
	request := r.transport.takeRenewal(t, byJwt)
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	request.answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 15*day)
}

// A LocalState error leaves the kept credential as it was and retries later.
func TestRenewalThatCannotBePersistedIsRetried(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)

	request := r.transport.takeRenewal(t, networkJwt)
	// the auth state cannot be read: an oversized file in its place
	if err := os.WriteFile(localState.authStatePath(), make([]byte, localAuthStateMaxBytes+1), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	request.answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, r.clock.Now(), "renewed")))
	r.requireRenewed(t)
	r.clock.requireArmed(t, networkRenewalMinRetryTimeout+networkRenewalRetryJitterBase)
	connect.AssertEqual(t, r.api.networkCredential(), networkJwt)
}

// Two APIs that share a LocalState stay consistent: the first renewal is
// stored, and the other API keeps that one instead of its own. Two processes
// with their own LocalState over the same storage do the same.
func TestApisSharingALocalStateStayConsistent(t *testing.T) {
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-20*day), renewalTestBaseTime.Add(10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	for _, c := range []struct {
		name    string
		process bool
	}{
		{"one LocalState", false},
		{"two processes", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			localState := newRenewalTestLocalState(t, home, networkJwt, clientJwt)
			otherState := localState
			if c.process {
				otherState = newLocalState(context.Background(), home)
				t.Cleanup(otherState.Close)
			}
			first := newRenewalTestApi(t)
			second := newRenewalTestApi(t)
			first.startDevice(t, localState, clientJwt)
			second.startDevice(t, otherState, clientJwt)
			first.clock.requireArmed(t, minRefreshTimeout)
			second.clock.requireArmed(t, minRefreshTimeout)

			first.clock.Advance(minRefreshTimeout)
			second.clock.Advance(minRefreshTimeout)
			firstRequest := first.transport.takeRenewal(t, networkJwt)
			secondRequest := second.transport.takeRenewal(t, networkJwt)

			firstJwt := renewalTestRenewedJwt(t, first.clock.Now(), "first")
			firstRequest.answer(renewalTestReplyJwt(firstJwt))
			first.requireRenewed(t)
			secondRequest.answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, second.clock.Now(), "second")))
			second.requireRenewed(t)

			connect.AssertEqual(t, localState.GetByJwt(), firstJwt)
			connect.AssertEqual(t, otherState.GetByJwt(), firstJwt)
			connect.AssertEqual(t, first.api.networkCredential(), firstJwt)
			connect.AssertEqual(t, second.api.networkCredential(), firstJwt)
			first.clock.requireArmed(t, 15*day)
			second.clock.requireArmed(t, 15*day)
		})
	}
}

// The renewer starts only with something to renew and stops with its API;
// CloseAndWait joins it, and it does not start after Close.
func TestNetworkCredentialRenewerSharesTheApiLifetime(t *testing.T) {
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-time.Hour), renewalTestBaseTime.Add(30*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")

	t.Run("joined while waiting", func(t *testing.T) {
		r := newRenewalTestApi(t)
		localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
		r.startDevice(t, localState, clientJwt)
		r.clock.requireArmed(t, networkCredentialRenewalTimeout(networkJwt, renewalTestBaseTime))
		ctx, cancel := context.WithTimeout(context.Background(), renewalTestStepLimit)
		defer cancel()
		if err := r.api.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-r.api.networkRenewer.doneChannel():
		default:
			t.Fatal("the renewer is still running")
		}
	})
	t.Run("joined during a request", func(t *testing.T) {
		r := newRenewalTestApi(t)
		expiredJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "expired")
		localState := newRenewalTestLocalState(t, t.TempDir(), expiredJwt, clientJwt)
		r.startDevice(t, localState, clientJwt)
		r.transport.takeRenewal(t, expiredJwt)
		ctx, cancel := context.WithTimeout(context.Background(), renewalTestStepLimit)
		defer cancel()
		if err := r.api.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		// the unanswered renewal changed nothing
		connect.AssertEqual(t, localState.GetByJwt(), expiredJwt)
	})
	t.Run("never started", func(t *testing.T) {
		r := newRenewalTestApi(t)
		r.api.Close()
		localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
		owner := newDeviceAuthPublicationGate()
		prepared, err := r.api.prepareDeviceAuth(localState, clientJwt, localState.GetInstanceId(), time.Now(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.api.setDeviceByJwt(prepared, owner, connect.DefaultLogger()); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, r.renewerStarted(), false)
		ctx, cancel := context.WithTimeout(context.Background(), renewalTestStepLimit)
		defer cancel()
		if err := r.api.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// The renewal request is a Network only route of the request seam: with no
// network credential it is refused before it is sent, and it never carries
// the client token.
func TestNetworkRefreshRequestNeedsTheNetworkCredential(t *testing.T) {
	r := newRenewalTestApi(t)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	installTestDeviceByJwt(t, r.api, nil, clientJwt, nil)
	connect.AssertEqual(t, apiRouteAccessFor(http.MethodPost, "/auth/network-refresh"), apiRouteAccessNetwork)

	for _, byJwt := range []string{clientJwt, ""} {
		_, err := r.api.NetworkRefreshSyncWithContextAndJwt(r.ctx, byJwt)
		if !errors.Is(err, ErrNetworkCredentialRequired) {
			t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
		}
	}
	r.transport.requireNone(t)

	// a well-formed exchange decodes to exactly one of a renewal or a refusal
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime, renewalTestBaseTime.Add(time.Hour), "sign-in")
	for _, c := range []struct {
		reply string
		ok    bool
	}{
		{renewalTestReplyJwt("renewed"), true},
		{`{"error":{"message":"refused"}}`, true},
		{`{}`, false},
		{`{"by_jwt":"renewed","error":{"message":"refused"}}`, false},
		{`{"error":{"message":""}}`, false},
		{`{"by_jwt":"a","by_jwt":"b"}`, false},
		{`not json`, false},
	} {
		done := make(chan error, 1)
		go func() {
			_, err := r.api.NetworkRefreshSyncWithContextAndJwt(r.ctx, networkJwt)
			done <- err
		}()
		r.transport.takeRenewal(t, networkJwt).answer(c.reply)
		err := <-done
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%t", c.reply, err, c.ok)
		}
	}
}

// The Apple app's device is a DeviceRemote, whose requests go over device
// rpc. It starts from the app's LocalState like a local device, so its API
// renews the network token over the remote's transport, with the network
// credential, and persists the renewal.
func TestRemoteDeviceRenewsOverItsTransport(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "remote")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	remote := &renewalTestTransport{requests: make(chan renewalTestRequest, 8)}
	owner := newDeviceAuthPublicationGate()
	prepared, err := r.api.prepareDeviceAuth(localState, clientJwt, localState.GetInstanceId(), time.Now(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.api.installDeviceRemote(prepared, owner, remote.post, remote.get, nil, connect.DefaultLogger()); err != nil {
		t.Fatal(err)
	}

	request := remote.takeRenewal(t, networkJwt)
	r.transport.requireNone(t)
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	request.answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 15*day)
	connect.AssertEqual(t, localState.GetByJwt(), renewedJwt)
	connect.AssertEqual(t, localState.GetByClientJwt(), clientJwt)
	connect.AssertEqual(t, r.api.networkCredential(), renewedJwt)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
}

// The exported call is how a Go owner outside a LocalState renews its own
// network token (the subnet miner's token file). It answers the renewal or the
// refusal in /auth/refresh's shape, and its errors classify the way such an
// owner schedules: a confirmed rejection (401), a transient failure, or another
// status the owner treats as refused.
func TestNetworkRefreshSyncWithContextAndJwtClassifiesItsAnswer(t *testing.T) {
	r := newRenewalTestApi(t)
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime, renewalTestBaseTime.Add(time.Hour), "sign-in")
	exchange := func(t *testing.T, respond func(renewalTestRequest)) (*RefreshJwtResult, error) {
		t.Helper()
		type outcome struct {
			result *RefreshJwtResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := r.api.NetworkRefreshSyncWithContextAndJwt(r.ctx, networkJwt)
			done <- outcome{result: result, err: err}
		}()
		respond(r.transport.takeRenewal(t, networkJwt))
		got := <-done
		return got.result, got.err
	}

	result, err := exchange(t, func(request renewalTestRequest) { request.answer(renewalTestReplyJwt("renewed")) })
	if err != nil || result == nil || result.ByJwt != "renewed" || result.Error != nil {
		t.Fatalf("renewal: result=%+v err=%v", result, err)
	}
	result, err = exchange(t, func(request renewalTestRequest) {
		request.answer(`{"error":{"message":"An API key does not expire and is not refreshed."}}`)
	})
	if err != nil || result == nil || result.ByJwt != "" || result.Error == nil || result.Error.Message != "An API key does not expire and is not refreshed." {
		t.Fatalf("refusal: result=%+v err=%v", result, err)
	}

	_, err = exchange(t, func(request renewalTestRequest) {
		request.fail(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized})
	})
	if !ConfirmedClientRefreshRejection(err) {
		t.Fatalf("401 err = %v, want a confirmed rejection", err)
	}
	_, err = exchange(t, func(request renewalTestRequest) {
		request.fail(&connect.HttpStatusError{StatusCode: http.StatusServiceUnavailable})
	})
	var unavailable *ClientControlUnavailableError
	if !errors.As(err, &unavailable) || ConfirmedClientRefreshRejection(err) {
		t.Fatalf("503 err = %v, want a transient ClientControlUnavailableError", err)
	}
	_, err = exchange(t, func(request renewalTestRequest) {
		request.fail(&connect.HttpStatusError{StatusCode: http.StatusNotFound})
	})
	var status *connect.HttpStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound || errors.As(err, &unavailable) || ConfirmedClientRefreshRejection(err) {
		t.Fatalf("404 err = %v, want a plain status a caller treats as refused", err)
	}
	_, err = exchange(t, func(request renewalTestRequest) { request.answer(`{}`) })
	var malformed *ClientControlResponseError
	if !errors.As(err, &malformed) {
		t.Fatalf("empty answer err = %v, want a ClientControlResponseError", err)
	}
}
