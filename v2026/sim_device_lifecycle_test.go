// Headless simulator lifecycle tests pin completion at the SDK ownership
// boundary used by the production latency fleet.
package sdk

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Builds a real headless provider whose platform dial has acknowledged
// cancellation but cannot return until the test releases it.
func newBlockedSimProvider(t *testing.T) (*SimProvider, <-chan struct{}, func()) {
	t.Helper()
	dialEntered := make(chan struct{})
	dialCanceled := make(chan struct{})
	dialRelease := make(chan struct{})
	var enteredOnce sync.Once
	var canceledOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(dialRelease)
		})
	}

	provider := NewSimProvider(context.Background(), &SimProviderConfig{
		ApiUrl:      "http://api.invalid",
		PlatformUrl: "ws://platform.test:18080",
		ByJwt:       "test-jwt",
		ClientId:    connect.NewId(),
		InstanceId:  connect.NewId(),
		AppVersion:  "0.0.0",
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			if !strings.HasPrefix(address, "platform.test:") {
				return nil, errors.New("test API is offline")
			}
			enteredOnce.Do(func() {
				close(dialEntered)
			})
			<-ctx.Done()
			canceledOnce.Do(func() {
				close(dialCanceled)
			})
			<-dialRelease
			return nil, ctx.Err()
		},
		DisableSecurityPolicy: true,
		Log:                   connect.NewNoopLogger(),
	})
	t.Cleanup(func() {
		release()
		provider.Close()
	})

	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(testCancel)
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-dialEntered:
	}
	return provider, dialCanceled, release
}

// Bounds an environmental failure without replacing the lifecycle barrier
// that proves the ordering under test.
func waitForSimProviderLifecycleEdge(t *testing.T, edge <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	case <-edge:
	}
}

// A churn disconnect is complete only after the old transport has released
// its admitted dial and can no longer overlap the replacement generation.
func TestSimProviderDisconnectJoinsPendingTransportDial(t *testing.T) {
	provider, dialCanceled, release := newBlockedSimProvider(t)
	disconnected := make(chan struct{})
	go func() {
		provider.SetConnected(false)
		close(disconnected)
	}()
	waitForSimProviderLifecycleEdge(t, dialCanceled, "disconnect cancellation")
	select {
	case <-disconnected:
		t.Fatal("disconnect returned before the admitted transport dial")
	default:
	}
	release()
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not finish after the dial returned")
	case <-disconnected:
	}
}

// Full provider teardown has the same synchronous ownership boundary as a
// churn disconnect and remains idempotent after completion.
func TestSimProviderCloseJoinsPendingTransportDial(t *testing.T) {
	provider, dialCanceled, release := newBlockedSimProvider(t)
	closed := make(chan struct{})
	go func() {
		provider.Close()
		close(closed)
	}()
	waitForSimProviderLifecycleEdge(t, dialCanceled, "provider-close cancellation")
	select {
	case <-closed:
		t.Fatal("provider close returned before the admitted transport dial")
	default:
	}
	release()
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("provider close did not finish after the dial returned")
	case <-closed:
	}
	provider.Close()
}

// Hosted devices own an API session distinct from their shared NetworkSpace.
// Callback-safe Close requests cancellation; the external join must retain
// ownership until an admitted refresh request has fully unwound.
func TestDeviceLocalCloseAndWaitJoinsOwnedApiRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"http://unused.invalid",
		"ws://unused.invalid",
		strategySettings,
	)
	requestEntered := make(chan struct{})
	requestCanceled := make(chan struct{})
	requestRelease := make(chan struct{})
	var enteredOnce sync.Once
	var canceledOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(requestRelease)
		})
	}
	networkSpace.api.setHttpGetRaw(func(requestCtx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		enteredOnce.Do(func() {
			close(requestEntered)
		})
		<-requestCtx.Done()
		canceledOnce.Do(func() {
			close(requestCanceled)
		})
		<-requestRelease
		return nil, requestCtx.Err()
	})
	settings := DefaultDeviceLocalSettings()
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		testingRefreshableJwt(t),
		"owned-api-close",
		"test",
		"0.0.0",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		release()
		networkSpace.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		_ = device.CloseAndWait(context.Background())
		networkSpace.Close()
		cancel()
	})

	waitForSimProviderLifecycleEdge(t, requestEntered, "owned API refresh request")
	device.Close()
	waitForSimProviderLifecycleEdge(t, requestCanceled, "owned API refresh cancellation")
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- device.CloseAndWait(context.Background())
	}()
	select {
	case err := <-closeResult:
		t.Fatalf("device join returned before its API request cleanup: %v", err)
	default:
	}
	release()
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("device join did not finish after API request cleanup")
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	}
}
