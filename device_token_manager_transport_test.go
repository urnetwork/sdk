package sdk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testingTokenManagerTransportLogger exposes the scheduled state after a
// successful refresh without polling the worker or relying on a short sleep.
type testingTokenManagerTransportLogger struct {
	countingLogger
	scheduled     chan struct{}
	scheduledOnce sync.Once
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

// A DeviceRemote can publish its RPC service while the startup refresh is
// still blocked on the pre-service direct path. That generation change must
// make the eventual failure retry immediately through the new path, and many
// availability events must still coalesce into one retry.
func TestApiTokenManagerRemoteTransportAvailabilityRetriesInFlightFailure(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "transport-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "transport-refreshed")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64

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
			select {
			case <-releaseFirst:
				return nil, errors.New("pre-service transport timeout")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
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

	waitForTokenManagerTransportSignal(t, firstStarted, "startup refresh did not begin")
	for range 8 {
		api.remoteTransportAvailable()
	}
	close(releaseFirst)
	waitForTokenManagerTransportSignal(t, secondStarted, "new RPC transport did not retry the in-flight failure")
	if got := receiveStringWithin(t, refreshed, "transport retry did not refresh the JWT"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	waitForTokenManagerTransportSignal(t, log.scheduled, "successful transport retry did not return to the refresh schedule")
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("refresh requests = %d, want one failure and one coalesced retry", got)
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
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var requestCount atomic.Int64

	api := deviceRemote.GetApi()
	log := &testingTokenManagerTransportLogger{scheduled: make(chan struct{})}
	api.setLog(log)
	api.setHttpGetRaw(func(requestCtx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			select {
			case <-releaseFirst:
				return nil, errors.New("pre-service transport timeout")
			case <-requestCtx.Done():
				return nil, requestCtx.Err()
			}
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
	close(releaseFirst)
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
}

// CloseAndWait must cancel and join an immediate transport retry just as it
// joins the scheduled worker; the new wake-up path cannot outlive its Api.
func TestApiTokenManagerCloseJoinsRemoteTransportRetry(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "close-initial")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	secondCanceled := make(chan struct{})
	var requestCount atomic.Int64

	_, api := newTestApiForURL(t, "https://unused.invalid")
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		switch requestCount.Add(1) {
		case 1:
			close(firstStarted)
			select {
			case <-releaseFirst:
				return nil, errors.New("pre-service transport timeout")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
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
	close(releaseFirst)
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
}
