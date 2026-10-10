package sdk

// Renewal of the network credential the API keeps for the routes that
// administer the network (api_network_credential.go, AUTHZ-CLIENT.md).
//
// The network's sign-in token expires, and the server refreshes only a client
// token on /auth/refresh. POST /auth/network-refresh renews a signed network
// token instead: the server answers a fresh token of the same network and
// user. The route is Network only, so the server refuses a client token there,
// and it refuses an API key, which does not expire.
//
// An API renews the network token it keeps beside a device's client token
// when the LocalState that device started from stores exactly that token. The
// renewal is persisted there with a compare-and-swap, so the device of a
// relaunch adopts it, and a sign-out or a new sign-in that races it wins. An
// API that no LocalState backs never renews: ur.io's account host (ur.io renews
// its own token and sets it), a headless backend, and a hosted device's
// session API, which keeps no network credential.

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

const (
	// a failed renewal is retried after at least this long, plus jitter
	networkRenewalMinRetryTimeout = 10 * time.Second
	// the jitter interval after the first failed renewal, which doubles with
	// each further failure in a row
	networkRenewalRetryJitterBase = 30 * time.Second
	// the largest jitter interval
	networkRenewalMaxRetryJitter = 15 * time.Minute
	// a renewal the server answers with another 4xx (a server that predates
	// the route answers 404) is tried again after this
	networkRenewalRefusedRetryTimeout = 24 * time.Hour
)

// networkCredentialRenewalTimeout is the delay until the scheduled renewal of
// a network token, where 0 means now. A token without a readable expiration (a
// legacy token) or an expired one is renewed now, while the server still
// accepts it (vault auth.yml reject_missing_expiration and reject_expired
// off): renewing is how the installed base moves to tokens that renew before
// those gates flip. Any other token is renewed at its half-life, and never
// sooner than minRefreshTimeout (jwtRefreshTimeout).
func networkCredentialRenewalTimeout(byJwt string, now time.Time) time.Duration {
	expirationTime := jwtExpiration(byJwt)
	if expirationTime.IsZero() || !now.Before(expirationTime) {
		return 0
	}
	return jwtRefreshTimeout(byJwt, now)
}

// networkRenewalAnomalyTimeout spaces renewals that answer a token which is
// itself due at once (a server that mints no expiration, or a clock far ahead
// of the server's). The spacing doubles from minRefreshTimeout with each such
// renewal in a row, up to noExpirationRefreshTimeout, so neither can make
// renewal a loop.
func networkRenewalAnomalyTimeout(consecutive int) time.Duration {
	timeout := minRefreshTimeout
	for i := 1; i < consecutive && timeout < noExpirationRefreshTimeout; i += 1 {
		timeout *= 2
	}
	return min(timeout, noExpirationRefreshTimeout)
}

// networkRenewalRetryTimeout is the backoff after the given number of failed
// renewals in a row: networkRenewalMinRetryTimeout plus jitter over an
// interval that doubles from networkRenewalRetryJitterBase up to
// networkRenewalMaxRetryJitter. The floor keeps any jitter, even none, from
// retrying in a loop.
func networkRenewalRetryTimeout(failures int, jitter func(time.Duration) time.Duration) time.Duration {
	interval := networkRenewalRetryJitterBase
	for i := 1; i < failures && interval < networkRenewalMaxRetryJitter; i += 1 {
		interval *= 2
	}
	interval = min(interval, networkRenewalMaxRetryJitter)
	return networkRenewalMinRetryTimeout + max(0, min(jitter(interval), interval))
}

func networkRenewalJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int63n(int64(interval)))
}

// renewableNetworkCredential reports whether the server renews a credential on
// /auth/network-refresh: a network token, which is a JWT that names no client.
// An API key is a network credential too, but it does not expire and the
// server refuses to renew it.
func renewableNetworkCredential(byJwt string) bool {
	_, ok := unverifiedCredentialClaims(byJwt)
	return ok && !credentialNamesClient(byJwt)
}

// networkCredentialIdentity is the network and user a JWT names, read
// without verifying the signature.
func networkCredentialIdentity(byJwt string) (networkId string, userId string, ok bool) {
	claims, ok := unverifiedCredentialClaims(byJwt)
	if !ok {
		return "", "", false
	}
	networkId, _ = claims["network_id"].(string)
	userId, _ = claims["user_id"].(string)
	return networkId, userId, networkId != "" && userId != ""
}

// validateRenewedNetworkJwt checks that a renewal is a network token of the
// network and user of the token it renews. The server signed it; this keeps a
// response from replacing the sign-in with another identity.
func validateRenewedNetworkJwt(previousByJwt string, renewedByJwt string) error {
	if !renewableNetworkCredential(renewedByJwt) {
		return errors.New("the renewal is not a network token")
	}
	networkId, userId, ok := networkCredentialIdentity(renewedByJwt)
	previousNetworkId, previousUserId, previousOk := networkCredentialIdentity(previousByJwt)
	if !ok || !previousOk || networkId != previousNetworkId || userId != previousUserId {
		return errors.New("the renewal names another network or user")
	}
	return nil
}

// storedRenewalOf reports whether the network token a LocalState stores is a
// later token of the same network and user than previousByJwt: another API
// that shares the LocalState renewed it first, or the app stored a newer
// sign-in of the same account.
func storedRenewalOf(previousByJwt string, storedByJwt string) bool {
	if storedByJwt == previousByJwt || validateRenewedNetworkJwt(previousByJwt, storedByJwt) != nil {
		return false
	}
	return jwtIssuedAt(previousByJwt).Before(jwtIssuedAt(storedByJwt))
}

// NetworkRefreshSyncWithContextAndJwt renews one network token: POST
// /auth/network-refresh with that token. The answer has the shape of
// /auth/refresh's: the renewed network token, or the server's refusal (an API
// key, a client token). The route is Network only, so the request seam sends a
// network credential there, never a client token, and never to another url.
//
// Go owners that keep a network token outside a LocalState renew it with this,
// for example the subnet miner's token file. A confirmed rejection of the
// token (401) satisfies ConfirmedClientRefreshRejection; a transient failure
// is a ClientControlUnavailableError; a malformed answer is a
// ClientControlResponseError.
//
//gomobile:noexport
func (self *Api) NetworkRefreshSyncWithContextAndJwt(ctx context.Context, byJwt string) (*RefreshJwtResult, error) {
	if ctx == nil {
		return nil, &ClientControlResponseError{detail: "network refresh context is absent"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := self.getHttpPostRaw()(ctx, fmt.Sprintf("%s/auth/network-refresh", self.apiUrl), []byte("{}"), byJwt)
	if err != nil {
		if transientClientControlRequestError(err) {
			return nil, &ClientControlUnavailableError{cause: err}
		}
		return nil, err
	}
	var result *RefreshJwtResult
	if err := decodeClientControlJson(raw, &result); err != nil {
		return nil, err
	}
	if result == nil || result.Error == nil && result.ByJwt == "" || result.Error != nil && (result.ByJwt != "" || result.Error.Message == "") {
		return nil, &ClientControlResponseError{detail: "network refresh must contain exactly one complete success or refusal"}
	}
	return result, nil
}

// refusedNetworkRenewal reports a 4xx answer other than a confirmed rejection
// (401) or a transient one (408, 425, 429): the server cannot renew now, and
// asking again soon will not change that.
func refusedNetworkRenewal(err error) bool {
	var unavailable *ClientControlUnavailableError
	if errors.As(err, &unavailable) {
		return false
	}
	var status *connect.HttpStatusError
	return errors.As(err, &status) && 400 <= status.StatusCode && status.StatusCode < 500
}

// networkRenewalTarget is the kept network credential that one renewal reads
// and renews.
type networkRenewalTarget struct {
	byJwt      string
	generation uint64
	store      *LocalState
}

// currentNetworkRenewalTarget is the network credential to renew, if any: a
// network token that LocalState backs, whose renewal has not stopped.
func (self *Api) currentNetworkRenewalTarget() (networkRenewalTarget, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.networkByJwtStore == nil || self.networkRenewalHalted || !renewableNetworkCredential(self.networkByJwt) {
		return networkRenewalTarget{}, false
	}
	return networkRenewalTarget{
		byJwt:      self.networkByJwt,
		generation: self.networkByJwtGeneration,
		store:      self.networkByJwtStore,
	}, true
}

// isNetworkRenewalTargetWithLock reports whether the target is still the kept
// credential to renew. Callers hold mutex.
func (self *Api) isNetworkRenewalTargetWithLock(target networkRenewalTarget) bool {
	return self.networkByJwt == target.byJwt &&
		self.networkByJwtGeneration == target.generation &&
		self.networkByJwtStore == target.store &&
		!self.networkRenewalHalted
}

// commitNetworkRenewal persists a renewal in the target's LocalState with a
// compare-and-swap, then keeps it in place of the target. Both happen under
// authMutationLock, so no sign-in, sign-out or device start interleaves; one
// that came first wins and the renewal is discarded. When the LocalState
// already stores a later token of the same sign-in, the API keeps that one
// instead. When it stores another sign-in, or none, renewal stops: the
// credential stays until the app replaces it. Returns the credential kept.
func (self *Api) commitNetworkRenewal(target networkRenewalTarget, renewedByJwt string) (keptByJwt string, committed bool, err error) {
	self.authMutationLock.Lock()
	defer self.authMutationLock.Unlock()

	self.mutex.Lock()
	current := self.isNetworkRenewalTargetWithLock(target)
	self.mutex.Unlock()
	if !current {
		return "", false, nil
	}

	storedByJwt, accepted, err := target.store.replaceNetworkByJwt(target.byJwt, renewedByJwt)
	if err != nil {
		return "", false, err
	}
	keptByJwt = renewedByJwt
	if !accepted {
		if !storedRenewalOf(target.byJwt, storedByJwt) {
			self.mutex.Lock()
			self.networkRenewalHalted = true
			self.mutex.Unlock()
			return "", false, nil
		}
		keptByJwt = storedByJwt
	}

	// authMutationLock excludes every other change of the kept credential, so
	// the target is still current. A renewal is the same sign-in: the
	// generation, the device's client token and the device owner stay.
	self.mutex.Lock()
	self.networkByJwt = keptByJwt
	self.mutex.Unlock()
	return keptByJwt, true, nil
}

// rejectNetworkRenewalTarget drops a network credential that the server
// rejected (401) while it is still the kept one. Unless the device's client
// token is of the same sign-in session, the app is not signed out: the
// device's client token and LocalState stay as they are, and
// HasNetworkCredential reports false so the app can ask for a sign-in on its
// account screens (rejectNetworkCredential). This API does not adopt the
// rejected token from LocalState again. The cause is the rejection's
// (confirmedRejectionCause).
func (self *Api) rejectNetworkRenewalTarget(target networkRenewalTarget, cause string) bool {
	return self.rejectNetworkCredential(target, cause)
}

// haltNetworkRenewalTarget stops renewing the target while it is still the
// kept credential, which stays: the server refused to renew it in a way it
// will repeat, or answered a token of another identity.
func (self *Api) haltNetworkRenewalTarget(target networkRenewalTarget) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.isNetworkRenewalTargetWithLock(target) {
		return false
	}
	self.networkRenewalHalted = true
	return true
}

// networkRenewalClock is the renewer's time source. Tests install a
// controlled one.
type networkRenewalClock interface {
	Now() time.Time
	// After starts a timer of d; stop releases it.
	After(d time.Duration) (c <-chan time.Time, stop func())
}

type systemNetworkRenewalClock struct{}

func (systemNetworkRenewalClock) Now() time.Time {
	return time.Now()
}

func (systemNetworkRenewalClock) After(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	return timer.C, func() {
		stopApiTokenTimer(timer)
	}
}

var closedNetworkRenewerDone = func() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

// apiNetworkCredentialRenewer is the API's worker that renews its kept
// network credential. Its goroutine starts the first time a LocalState backs a
// network token of the API, and it shares the API's lifetime.
type apiNetworkCredentialRenewer struct {
	ctx     context.Context
	api     *Api
	monitor *connect.Monitor
	// set while a failed renewal waits to retry, when a usable transport may
	// succeed where it failed
	retrying atomic.Bool
	// a transport became usable while a failed renewal waited
	transportRetry atomic.Bool

	startLock sync.Mutex
	started   bool
	done      chan struct{}

	// immutable once started; tests install deterministic ones
	clock  networkRenewalClock
	jitter func(time.Duration) time.Duration
	// tests observe each completed renewal attempt; installed before start
	testingAfterRenew func()

	// owned by run
	// the token the schedule was computed for, and its renewal time
	scheduledByJwt string
	scheduledTime  time.Time
	// the credential generation the state below describes
	generation uint64
	// failed renewals in a row
	failures int
	// the earliest next renewal, after a failure or an anomalous renewal
	notBefore time.Time
	// renewals in a row that answered a token due at once
	anomalies int
}

func newApiNetworkCredentialRenewer(ctx context.Context, api *Api) *apiNetworkCredentialRenewer {
	return &apiNetworkCredentialRenewer{
		ctx:     ctx,
		api:     api,
		monitor: connect.NewMonitor(),
		done:    make(chan struct{}),
		clock:   systemNetworkRenewalClock{},
		jitter:  networkRenewalJitter,
	}
}

// credentialChanged wakes the renewer to read the kept network credential
// again, and starts it the first time a LocalState backs one.
func (self *apiNetworkCredentialRenewer) credentialChanged() {
	if self == nil {
		return
	}
	if _, ok := self.api.currentNetworkRenewalTarget(); ok {
		self.start()
	}
	self.monitor.NotifyAll()
}

// transportAvailable ends the wait of a failed renewal, which retries over
// the newly usable transport. Other waits are left alone.
func (self *apiNetworkCredentialRenewer) transportAvailable() {
	if self == nil || !self.retrying.Load() {
		return
	}
	self.transportRetry.Store(true)
	self.monitor.NotifyAll()
}

func (self *apiNetworkCredentialRenewer) start() {
	self.startLock.Lock()
	defer self.startLock.Unlock()
	if self.started || self.ctx.Err() != nil {
		return
	}
	self.started = true
	go func() {
		defer close(self.done)
		connect.HandleError(self.run)
	}()
}

// doneChannel is closed once the worker stopped, or at once when it never
// started. After the API's Close the worker cannot start.
func (self *apiNetworkCredentialRenewer) doneChannel() <-chan struct{} {
	if self == nil {
		return closedNetworkRenewerDone
	}
	self.startLock.Lock()
	defer self.startLock.Unlock()
	if !self.started {
		return closedNetworkRenewerDone
	}
	return self.done
}

func (self *apiNetworkCredentialRenewer) run() {
	for {
		// Subscribe, then read: a change after the read closes this channel.
		notify := self.monitor.NotifyChannel()
		target, ok := self.api.currentNetworkRenewalTarget()
		if !ok {
			select {
			case <-self.ctx.Done():
				return
			case <-notify:
				continue
			}
		}

		now := self.clock.Now()
		self.track(target, now)
		if self.transportRetry.Swap(false) && self.retrying.Load() {
			self.notBefore = time.Time{}
		}
		due := self.scheduledTime
		if due.Before(self.notBefore) {
			due = self.notBefore
		}
		if now.Before(due) {
			timeout, stop := self.clock.After(due.Sub(now))
			select {
			case <-self.ctx.Done():
				stop()
				return
			case <-notify:
				stop()
				continue
			case <-timeout:
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		}
		self.renew(target)
	}
}

// track starts the state of a new credential generation over, and schedules
// a token the first time it is the target. The schedule of a token is read
// once: a token without `iat` has a half-life relative to the time it is
// read, which would recede if it were read again on every pass.
func (self *apiNetworkCredentialRenewer) track(target networkRenewalTarget, now time.Time) {
	if target.generation != self.generation {
		self.generation = target.generation
		self.failures = 0
		self.anomalies = 0
		self.notBefore = time.Time{}
		self.retrying.Store(false)
	}
	if target.byJwt != self.scheduledByJwt {
		self.scheduledByJwt = target.byJwt
		self.scheduledTime = now.Add(networkCredentialRenewalTimeout(target.byJwt, now))
	}
}

// retryAt waits until the given time before the next renewal. A transport
// that becomes usable ends the wait when transportWakes.
func (self *apiNetworkCredentialRenewer) retryAt(retryTime time.Time, transportWakes bool) {
	self.notBefore = retryTime
	self.retrying.Store(transportWakes)
}

// renew renews the target once and applies the outcome.
func (self *apiNetworkCredentialRenewer) renew(target networkRenewalTarget) {
	if self.testingAfterRenew != nil {
		defer self.testingAfterRenew()
	}
	// renew exactly the target; a change since it was read starts over
	if current, ok := self.api.currentNetworkRenewalTarget(); !ok || current != target {
		return
	}
	log := self.api.logger()
	log.Infof("[api-network]renewing the network credential now")
	result, err := self.api.NetworkRefreshSyncWithContextAndJwt(self.ctx, target.byJwt)
	if self.ctx.Err() != nil {
		return
	}
	now := self.clock.Now()
	if err != nil {
		switch {
		case ConfirmedClientRefreshRejection(err):
			// the server rejected the token itself: rotated credentials, a
			// removed account, an expiration the server enforces, or a revoked
			// session. The request seam has normally rejected it already.
			if self.api.rejectNetworkRenewalTarget(target, confirmedRejectionCause(err)) {
				log.Errorf("[api-network]the network credential was rejected (%d); account administration needs a new sign-in", http.StatusUnauthorized)
			}
		case refusedNetworkRenewal(err):
			self.failures = 0
			self.retryAt(now.Add(networkRenewalRefusedRetryTimeout), false)
			log.Infof("[api-network]renewal unavailable (%v); trying again in %s", err, networkRenewalRefusedRetryTimeout)
		default:
			self.failures += 1
			retryTimeout := networkRenewalRetryTimeout(self.failures, self.jitter)
			self.retryAt(now.Add(retryTimeout), true)
			log.Infof("[api-network]renewal failed (%v); trying again in %.2fs", err, float64(retryTimeout/time.Millisecond)/1000.0)
		}
		return
	}
	if result.Error != nil {
		// a refusal the server repeats for this token, which stays as it is
		if self.api.haltNetworkRenewalTarget(target) {
			log.Errorf("[api-network]renewal refused: %s", result.Error.Message)
		}
		return
	}
	if err := validateRenewedNetworkJwt(target.byJwt, result.ByJwt); err != nil {
		if self.api.haltNetworkRenewalTarget(target) {
			log.Errorf("[api-network]renewal discarded: %v", err)
		}
		return
	}

	keptByJwt, committed, err := self.api.commitNetworkRenewal(target, result.ByJwt)
	if err != nil {
		// LocalState could not be written; the kept credential is unchanged
		self.failures += 1
		retryTimeout := networkRenewalRetryTimeout(self.failures, self.jitter)
		self.retryAt(now.Add(retryTimeout), false)
		log.Errorf("[api-network]failed to persist the renewed network credential: %v", err)
		return
	}
	if !committed {
		// superseded by a sign-in, sign-out or device start, or the LocalState
		// holds another sign-in; the next pass reads the kept credential again
		return
	}
	self.failures = 0
	self.retryAt(time.Time{}, false)
	if networkCredentialRenewalTimeout(keptByJwt, now) == 0 {
		self.anomalies += 1
		self.notBefore = now.Add(networkRenewalAnomalyTimeout(self.anomalies))
	} else {
		self.anomalies = 0
	}
	log.Infof("[api-network]renewed the network credential")
}
