package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// testingTokenManagerTransportLogger exposes the scheduled state after a
// successful refresh without polling the worker or relying on a short sleep.
type testingTokenManagerTransportLogger struct {
	countingLogger
	scheduled     chan struct{}
	scheduledOnce sync.Once
	requestErrors atomic.Int64
}

// testingRemoteTransportListener exposes completed RPC publications without
// polling DeviceRemote state.
type testingRemoteTransportListener struct {
	connected chan struct{}
}

// RemoteChanged records only completed publications; disconnects are not a
// usable API transport.
func (self *testingRemoteTransportListener) RemoteChanged(remoteConnected bool) {
	if remoteConnected {
		select {
		case self.connected <- struct{}{}:
		default:
		}
	}
}

// Infof forwards attempt counting and exposes the post-success schedule edge.
func (self *testingTokenManagerTransportLogger) Infof(format string, args ...any) {
	self.countingLogger.Infof(format, args...)
	if strings.Contains(format, "[api-token]waiting") {
		self.scheduledOnce.Do(func() {
			close(self.scheduled)
		})
	}
}

// Errorf counts refresh request errors so deliberate supersession cannot hide
// behind an otherwise successful retry.
func (self *testingTokenManagerTransportLogger) Errorf(format string, args ...any) {
	if strings.Contains(format, "[api-token]failed to refresh JWT:") {
		self.requestErrors.Add(1)
	}
}

// waitForTokenManagerTransportSignal keeps the barriers below bounded if the
// worker regresses while leaving their ordering deterministic.
func waitForTokenManagerTransportSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

// waitForPromptTokenManagerTransportSignal distinguishes context-driven
// release from the 15-second strategy timeout observed in Linux acceptance.
func waitForPromptTokenManagerTransportSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

// A DeviceRemote can publish its RPC service while the startup refresh is
// still blocked on the pre-service direct path. That generation change must
// cancel the obsolete request promptly and retry through the new path. The
// canceled attempt is not an outage and must not log an error or log out.
func TestApiTokenManagerRemoteTransportAvailabilityRetriesInFlightFailure(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "transport-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "transport-refreshed")
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64
	var logoutCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if requestUrl != "https://unused.invalid/auth/refresh" {
			return nil, fmt.Errorf("unexpected refresh URL %q", requestUrl)
		}
		if byJwt != initialJwt {
			return nil, fmt.Errorf("refresh JWT = %q, want initial JWT", byJwt)
		}
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return nil, ctx.Err()
		case 2:
			close(secondStarted)
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
		default:
			return nil, errors.New("duplicate transport refresh")
		}
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	logoutSub := api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		logoutCount.Add(1)
	}))
	defer logoutSub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, firstStarted, "startup refresh did not begin")
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, firstCanceled, "transport publication did not promptly cancel the obsolete refresh")
	waitForTokenManagerTransportSignal(t, secondStarted, "new RPC transport did not retry the in-flight failure")
	if got := receiveStringWithin(t, refreshed, "transport retry did not refresh the JWT"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "successful transport retry did not return to the refresh schedule")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("refresh requests = %d, want one canceled request and one retry", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("transport supersession logged %d request errors, want 0", got)
	}
	if got := logoutCount.Load(); got != 0 {
		t.Fatalf("transport supersession fired %d logout callbacks, want 0", got)
	}
}

// Publication before request ownership is installed is part of the same
// ordering contract: the attempt starts after the new generation and must not
// cancel or duplicate itself.
func TestApiTokenManagerRemoteTransportAvailabilityBeforeAttemptUsesOneRequest(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "before-attempt-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "before-attempt-refreshed")
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("new-generation request started canceled: %w", ctx.Err())
		}
		requestCount.Add(1)
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()

	api.remoteTransportAvailable()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	if got := receiveStringWithin(t, refreshed, "new-generation refresh did not succeed"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "new-generation refresh did not return to the schedule")
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("new-generation refresh requests = %d, want 1", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("pre-attempt publication logged %d request errors, want 0", got)
	}
}

// A server may finish the idempotent refresh GET just as publication cancels
// the old client path. A complete valid response wins and must not be replayed.
func TestApiTokenManagerTransportSupersessionPreservesCompletedSuccess(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "completed-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "completed-refreshed")
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if got := requestCount.Add(1); got != 1 {
			return nil, fmt.Errorf("completed refresh requests = %d, want 1", got)
		}
		close(requestStarted)
		<-ctx.Done()
		close(requestCanceled)
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, requestStarted, "completed refresh did not begin")
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, requestCanceled, "completed refresh did not observe publication")
	if got := receiveStringWithin(t, refreshed, "completed refresh response was not accepted"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "completed refresh did not return to the schedule")
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("completed refresh requests = %d, want 1", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("completed refresh logged %d request errors, want 0", got)
	}
}

// Cancellation does not downgrade an authoritative API rejection. If a 401
// response wins the race, the exact current JWT is rejected and is not retried.
func TestApiTokenManagerTransportSupersessionHonorsAuthoritativeRejection(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "rejected-initial")
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	loggedOut := make(chan struct{}, 1)
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		requestCount.Add(1)
		close(requestStarted)
		<-ctx.Done()
		close(requestCanceled)
		return nil, &connect.HttpStatusError{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
		}
	})
	sub := api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		loggedOut <- struct{}{}
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, requestStarted, "authoritative rejection refresh did not begin")
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, requestCanceled, "authoritative rejection did not observe publication")
	waitForTokenManagerTransportSignal(t, loggedOut, "authoritative rejection did not log out")
	if got := api.GetByJwt(); got != "" {
		t.Fatalf("authoritatively rejected JWT remains installed: %q", got)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("authoritative rejection refresh requests = %d, want 1", got)
	}
}

// An explicit refresh requested during an old-path attempt remains the owner
// after transport cancellation. The outer loop consumes that level once and
// performs exactly one replacement request.
func TestApiTokenManagerTransportSupersessionPreservesExplicitRefresh(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "explicit-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "explicit-refreshed")
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if byJwt != initialJwt {
			return nil, fmt.Errorf("explicit refresh JWT = %q, want initial JWT", byJwt)
		}
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return nil, ctx.Err()
		case 2:
			close(secondStarted)
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
		default:
			return nil, errors.New("duplicate explicit refresh")
		}
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, firstStarted, "explicit-refresh race did not begin")
	api.RequestJwtRefresh()
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, firstCanceled, "explicit-refresh race did not cancel the old path")
	waitForTokenManagerTransportSignal(t, secondStarted, "explicit refresh did not own the replacement attempt")
	if got := receiveStringWithin(t, refreshed, "explicit replacement refresh did not succeed"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "explicit replacement refresh did not return to the schedule")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("explicit replacement refresh requests = %d, want 2", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("explicit transport supersession logged %d request errors, want 0", got)
	}
}

// A new login/token installed during an old-path attempt has stronger
// ownership than the transport retry. The replacement request must read the
// current JWT instead of replaying the canceled attempt's credential.
func TestApiTokenManagerTransportSupersessionUsesReplacementJwt(t *testing.T) {
	oldJwt := testingRefreshableJwtWithMarker(t, "replacement-old")
	newJwt := testingRefreshableJwtWithMarker(t, "replacement-new")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "replacement-refreshed")
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			if byJwt != oldJwt {
				return nil, fmt.Errorf("first refresh JWT = %q, want old JWT", byJwt)
			}
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return nil, ctx.Err()
		case 2:
			if byJwt != newJwt {
				return nil, fmt.Errorf("replacement refresh JWT = %q, want new JWT", byJwt)
			}
			close(secondStarted)
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
		default:
			return nil, errors.New("duplicate replacement refresh")
		}
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(oldJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, firstStarted, "replacement race did not begin")
	api.SetByJwt(newJwt)
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, firstCanceled, "replacement race did not cancel the old path")
	waitForTokenManagerTransportSignal(t, secondStarted, "replacement JWT did not own the next attempt")
	if got := receiveStringWithin(t, refreshed, "replacement JWT refresh did not succeed"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "replacement JWT refresh did not return to the schedule")
	if got := api.GetByJwt(); got != refreshedJwt {
		t.Fatalf("Api JWT = %q, want replacement refresh %q", got, refreshedJwt)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("replacement refresh requests = %d, want 2", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("replacement transport supersession logged %d request errors, want 0", got)
	}
}

// Availability can also land after the failed attempt has checked its state
// but before it begins the retry wait. The monitor must already be subscribed
// at that boundary or the worker sleeps through the new usable transport.
func TestApiTokenManagerRemoteTransportAvailabilityWakesFailedWait(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "wait-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "wait-refreshed")
	retryTimeoutEntered := make(chan struct{})
	releaseRetryTimeout := make(chan struct{})
	secondStarted := make(chan struct{})
	var retryTimeoutOnce sync.Once
	var releaseRetryTimeoutOnce sync.Once
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.tokenManager.retryTimeout = func() time.Duration {
		retryTimeoutOnce.Do(func() {
			close(retryTimeoutEntered)
		})
		<-releaseRetryTimeout
		return time.Hour
	}
	t.Cleanup(func() {
		releaseRetryTimeoutOnce.Do(func() {
			close(releaseRetryTimeout)
		})
	})
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			return nil, errors.New("pre-service transport timeout")
		case 2:
			close(secondStarted)
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
		default:
			return nil, errors.New("duplicate transport refresh")
		}
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, retryTimeoutEntered, "failed refresh did not reach its retry wait")
	api.remoteTransportAvailable()
	releaseRetryTimeoutOnce.Do(func() {
		close(releaseRetryTimeout)
	})
	waitForTokenManagerTransportSignal(t, secondStarted, "availability edge was lost before the retry wait")
	if got := receiveStringWithin(t, refreshed, "failed-wait retry did not refresh the JWT"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "failed-wait retry did not return to the refresh schedule")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("refresh requests = %d, want one failure and one availability retry", got)
	}
	if got := log.requestErrors.Load(); got != 1 {
		t.Fatalf("genuine failed refresh logged %d request errors, want 1", got)
	}
}

// A healthy token manager has no failed work for RPC availability to wake.
// Reconnects and repeated SetRpcServer-driven publications therefore remain
// auth-traffic no-ops after success.
func TestApiTokenManagerRemoteTransportAvailabilityDoesNotRefreshAfterSuccess(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "healthy-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "healthy-refreshed")
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		requestCount.Add(1)
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
	})
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	if got := receiveStringWithin(t, refreshed, "startup refresh did not succeed"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "successful refresh did not return to the schedule")
	for range 16 {
		api.remoteTransportAvailable()
	}

	api.tokenManager.stateLock.Lock()
	failedRefreshPending := api.tokenManager.failedRefreshPending
	transportRetryPending := api.tokenManager.transportRetryPending
	api.tokenManager.stateLock.Unlock()
	if failedRefreshPending || transportRetryPending || api.tokenManager.refreshPending.Load() {
		t.Fatalf(
			"healthy availability armed refresh: failed=%t transport=%t explicit=%t",
			failedRefreshPending,
			transportRetryPending,
			api.tokenManager.refreshPending.Load(),
		)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("healthy reconnect refresh requests = %d, want 1 startup request", got)
	}
}

// The production DeviceRemote publication point owns the availability signal:
// a SetRpcServer replacement wakes an in-flight failed refresh, while an
// ordinary later reconnect and repeated idempotent setters do not refresh a
// healthy token.
func TestDeviceRemoteRpcPublicationWakesOnlyOutstandingRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)
	deviceRemote := testing_newRpcDeviceRemote(
		t,
		deviceLocal,
		settings,
		deviceLocal.GetInstanceId(),
		DeviceRpcVersion,
	)
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(5 * time.Second) {
		t.Fatal("device remote did not complete its initial sync")
	}

	initialJwt := testingRefreshableJwtWithMarker(t, "rpc-publication-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "rpc-publication-refreshed")
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64

	api := deviceRemote.GetApi()
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(requestCtx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			<-requestCtx.Done()
			close(firstCanceled)
			return nil, requestCtx.Err()
		case 2:
			close(secondStarted)
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, refreshedJwt)), nil
		default:
			return nil, errors.New("duplicate RPC-publication refresh")
		}
	})
	refreshed := make(chan string, 1)
	refreshSub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer refreshSub.Close()
	remoteListener := &testingRemoteTransportListener{connected: make(chan struct{}, 1)}
	remoteSub := deviceRemote.AddRemoteChangeListener(remoteListener)
	defer remoteSub.Close()

	api.SetByJwt(initialJwt)
	waitForTokenManagerTransportSignal(t, firstStarted, "RPC publication test refresh did not begin")
	if err := deviceRemote.SetRpcServer("", "", settings.Address.HostPort()); err != nil {
		t.Fatal(err)
	}
	waitForTokenManagerTransportSignal(t, remoteListener.connected, "SetRpcServer transport did not publish")
	waitForPromptTokenManagerTransportSignal(t, firstCanceled, "SetRpcServer publication did not promptly cancel the obsolete refresh")
	waitForTokenManagerTransportSignal(t, secondStarted, "published RPC transport did not retry the failed refresh")
	if got := receiveStringWithin(t, refreshed, "RPC publication retry did not refresh the JWT"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "RPC publication retry did not return to the refresh schedule")

	for range 8 {
		if err := deviceRemote.SetRpcServer("", "", settings.Address.HostPort()); err != nil {
			t.Fatal(err)
		}
	}
	deviceRemote.Sync()
	waitForTokenManagerTransportSignal(t, remoteListener.connected, "healthy RPC reconnect did not republish")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("refresh requests after setters/reconnect = %d, want 2", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("RPC publication logged %d supersession errors, want 0", got)
	}
}

// CloseAndWait must cancel and join an immediate transport retry just as it
// joins the scheduled worker; the new wake-up path cannot outlive its Api.
func TestApiTokenManagerCloseJoinsRemoteTransportRetry(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "close-initial")
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	secondCanceled := make(chan struct{})
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return nil, ctx.Err()
		case 2:
			close(secondStarted)
			<-ctx.Done()
			close(secondCanceled)
			return nil, ctx.Err()
		default:
			return nil, errors.New("refresh continued after close")
		}
	})
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	waitForTokenManagerTransportSignal(t, firstStarted, "startup refresh did not begin")
	api.remoteTransportAvailable()
	waitForPromptTokenManagerTransportSignal(t, firstCanceled, "transport publication did not cancel the first close-test request")
	waitForTokenManagerTransportSignal(t, secondStarted, "transport retry did not begin")

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	if err := api.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait did not join transport retry: %v", err)
	}
	waitForTokenManagerTransportSignal(t, secondCanceled, "transport retry did not observe Api cancellation")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("refresh requests at close = %d, want 2", got)
	}
	if got := log.requestErrors.Load(); got != 0 {
		t.Fatalf("joined cancellation logged %d request errors, want 0", got)
	}
}
