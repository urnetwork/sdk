package sdk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// newSharedNetworkSpaceTestDevice creates a device without provider or network
// destination work so lifecycle assertions observe only API ownership.
func newSharedNetworkSpaceTestDevice(
	t *testing.T,
	networkSpace *NetworkSpace,
	byJwt string,
	hosted bool,
) *DeviceLocal {
	t.Helper()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.HostedIncompatible = hosted
	settings.MemoryTargetByteCount = 24 * 1024 * 1024
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"shared-network-space-test",
		"test",
		"test",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return device
}

// Closing a hosted device cancels its data-plane context before the generated
// clients finish their final authenticated contract retirement. That work must
// still be able to use this device's private control strategy until its join
// completes; parenting the strategy to the device context drops the request.
func TestHostedDeviceControlStrategySurvivesDataPlaneCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: roots}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	platformURL := "wss" + strings.TrimPrefix(server.URL, "https")
	networkSpace := NewNetworkSpaceWithUrls(ctx, server.URL, platformURL, settings)
	defer networkSpace.close()
	device := newSharedNetworkSpaceTestDevice(t, networkSpace, "synthetic-device-token", true)
	strategy := device.clientStrategy

	// Hold one synthetic retirement worker so the test observes the interval
	// after data-plane cancellation but before strategy-owner teardown.
	releaseRetirement := make(chan struct{})
	device.stateLock.Lock()
	device.startLifecycleWorkerWithLock(func() { <-releaseRetirement })
	device.stateLock.Unlock()
	defer func() {
		close(releaseRetirement)
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		if err := device.CloseAndWait(joinCtx); err != nil {
			t.Errorf("join hosted device: %v", err)
		}
	}()
	device.Close()

	requestCtx, requestCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/retire", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strategy.HttpParallel(request); err != nil {
		t.Fatalf("private control strategy lost final request after data-plane cancellation: %v", err)
	}
}

// TestDeviceLocalTeardownUnsubscribesFromSharedApi deterministically pins the
// leak boundary: every ordinary DeviceLocal listener added to a shared API is
// removed on Close, including across repeated create/close churn.
func TestDeviceLocalTeardownUnsubscribesFromSharedApi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"http://127.0.0.1:1",
		"ws://127.0.0.1:1",
		connect.DefaultClientStrategySettings(),
	)
	api := networkSpace.GetApi()
	defer func() {
		networkSpace.close()
		api.tokenManager.Wait()
	}()
	initialRefreshListeners := len(api.jwtRefreshListeners.Get())
	initialLogoutListeners := len(api.authLogoutListeners.Get())

	for i := range 8 {
		device := newSharedNetworkSpaceTestDevice(
			t,
			networkSpace,
			"non-refreshable-device-token",
			false,
		)
		if got := len(api.jwtRefreshListeners.Get()); got != initialRefreshListeners+1 {
			t.Fatalf("iteration %d refresh listeners = %d, want %d", i, got, initialRefreshListeners+1)
		}
		if got := len(api.authLogoutListeners.Get()); got != initialLogoutListeners+1 {
			t.Fatalf("iteration %d logout listeners = %d, want %d", i, got, initialLogoutListeners+1)
		}
		device.Close()
		if got := len(api.jwtRefreshListeners.Get()); got != initialRefreshListeners {
			t.Fatalf("iteration %d retained %d refresh listeners, want %d", i, got, initialRefreshListeners)
		}
		if got := len(api.authLogoutListeners.Get()); got != initialLogoutListeners {
			t.Fatalf("iteration %d retained %d logout listeners, want %d", i, got, initialLogoutListeners)
		}
	}
}

// TestHostedDeviceLocalSessionsIsolateSharedApiLifecycle proves that hosted
// devices reuse one NetworkSpace without sharing mutable credentials,
// listeners, or refresh-worker lifetimes. Closing one session cannot clear or
// cancel its sibling or the manager-owned API.
func TestHostedDeviceLocalSessionsIsolateSharedApiLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"http://127.0.0.1:1",
		"ws://127.0.0.1:1",
		connect.DefaultClientStrategySettings(),
	)
	sharedApi := networkSpace.GetApi()
	defer func() {
		networkSpace.close()
		sharedApi.tokenManager.Wait()
	}()
	first := newSharedNetworkSpaceTestDevice(t, networkSpace, "first-device-token", true)
	second := newSharedNetworkSpaceTestDevice(t, networkSpace, "second-device-token", true)
	firstApi := first.GetApi()
	secondApi := second.GetApi()

	if firstApi == sharedApi || secondApi == sharedApi || firstApi == secondApi {
		t.Fatal("hosted devices shared mutable API credential sessions")
	}
	if first.clientStrategy == networkSpace.clientStrategy || second.clientStrategy == networkSpace.clientStrategy || first.clientStrategy == second.clientStrategy {
		t.Fatal("hosted devices shared control-plane dial pacing or DoH admission")
	}
	if firstApi.clientStrategy != first.clientStrategy || secondApi.clientStrategy != second.clientStrategy {
		t.Fatal("hosted API did not use its device-owned control strategy")
	}
	if first.platformTransportBudget == second.platformTransportBudget {
		t.Fatal("hosted devices shared one platform-carrier admission budget")
	}
	for index, device := range []*DeviceLocal{first, second} {
		usage := device.MemoryUsed()
		if usage.TargetByteCount != 24*1024*1024 {
			t.Fatalf("hosted device %d target = %d, want 24 MiB", index, usage.TargetByteCount)
		}
		if usage.PlatformTransportBudgetByteCount != 6*1024*1024 {
			t.Fatalf(
				"hosted device %d carrier budget = %d, want private 6 MiB share",
				index,
				usage.PlatformTransportBudgetByteCount,
			)
		}
		if usage.PlatformTransportMaxCount != 16 {
			t.Fatalf(
				"hosted device %d carrier count limit = %d, want 16",
				index,
				usage.PlatformTransportMaxCount,
			)
		}
	}
	if got := sharedApi.GetByJwt(); got != "" {
		t.Fatalf("manager-owned API credential = %q, want empty", got)
	}
	if got := len(sharedApi.jwtRefreshListeners.Get()); got != 0 {
		t.Fatalf("manager-owned API retained %d hosted refresh listeners", got)
	}
	if got := len(sharedApi.authLogoutListeners.Get()); got != 0 {
		t.Fatalf("manager-owned API retained %d hosted logout listeners", got)
	}

	first.Close()
	firstApi.tokenManager.Wait()
	if got := len(firstApi.jwtRefreshListeners.Get()); got != 0 {
		t.Fatalf("closed hosted session retained %d refresh listeners", got)
	}
	if got := len(firstApi.authLogoutListeners.Get()); got != 0 {
		t.Fatalf("closed hosted session retained %d logout listeners", got)
	}
	select {
	case <-firstApi.ctx.Done():
	default:
		t.Fatal("closed hosted device retained its API session lifetime")
	}
	if got := secondApi.GetByJwt(); got != "second-device-token" {
		t.Fatalf("closing first hosted device changed sibling token to %q", got)
	}
	select {
	case <-secondApi.ctx.Done():
		t.Fatal("closing first hosted device canceled sibling API session")
	case <-sharedApi.ctx.Done():
		t.Fatal("closing hosted device canceled manager-owned API")
	default:
	}

	second.Close()
	secondApi.tokenManager.Wait()
	select {
	case <-secondApi.ctx.Done():
	default:
		t.Fatal("second hosted API session remained live after close")
	}
}
