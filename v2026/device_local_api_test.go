package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type localDeviceApiTest struct {
	gets   atomic.Int64
	posts  atomic.Int64
	reject atomic.Bool
}

func (self *localDeviceApiTest) Get(ctx context.Context, url, token string) ([]byte, error) {
	self.gets.Add(1)
	if self.reject.Load() {
		return nil, errors.New("local refusal")
	}
	if strings.HasSuffix(url, "/history") {
		return json.Marshal(&connect.GetClientKeyHistoryResult{History: [][]byte{{1, 2, 3}}})
	}
	if strings.Contains(url, "/key/") {
		return json.Marshal(&connect.GetClientKeyResult{PublicKey: []byte{4, 5, 6}})
	}
	return []byte(`{"by_jwt":"synthetic-refreshed-token"}`), nil
}
func (self *localDeviceApiTest) Post(context.Context, string, []byte, string) ([]byte, error) {
	self.posts.Add(1)
	return []byte(`{}`), nil
}

// Private dispatch must not mutate the shared NetworkSpace API or silently
// fall back after a local error. The ordinary API remains independently usable.
func TestHostedLocalApiIsPrivateAndNeverFallsBack(t *testing.T) {
	var httpCalls atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "API disabled", http.StatusServiceUnavailable)
	}))
	defer trap.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	strategy := connect.DefaultClientStrategySettings()
	strategy.EnableResilient = false
	strategy.RequestTimeout = time.Second
	space := NewNetworkSpaceWithUrls(ctx, trap.URL, "ws://127.0.0.1:1", strategy)
	defer space.close()
	local := &localDeviceApiTest{}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.HostedIncompatible = true
	settings.LocalApi = local
	device, err := newDeviceLocalWithOverrides(space, "synthetic-device-token", "test", "test", "test", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.CloseAndWait(context.Background())
	if _, err = device.GetApi().RefreshJwtSyncWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	local.reject.Store(true)
	if _, err = device.GetApi().RefreshJwtSyncWithContext(ctx); err == nil {
		t.Fatal("local refusal hidden")
	}
	if local.gets.Load() < 2 || httpCalls.Load() != 0 {
		t.Fatal("local API unused or escaped to HTTP")
	}
	if _, err = space.GetApi().getHttpGetRaw()(ctx, trap.URL+"/auth/refresh", "shared"); err == nil {
		t.Fatal("shared API did not retain its ordinary HTTP boundary")
	}
	if httpCalls.Load() == 0 {
		t.Fatal("ordinary API control did not reach the trap")
	}
}

// Both unsigned key comparison and signed-history verification receive their
// ordinary payloads through the local owner; no security mode is downgraded.
func TestHostedLocalKeyFetchersPreserveResultsAndIsolation(t *testing.T) {
	local := &localDeviceApiTest{}
	settings := connect.DefaultClientSettings()
	original := settings.EncryptionSettings
	mode := original.Mode
	applyLocalDeviceApiKeyFetchers(settings, local, "https://control.example")
	if settings.EncryptionSettings == original || original.NewPeerClientPublicKeyFetcher != nil || original.NewPeerClientKeyHistoryFetcher != nil || settings.EncryptionSettings.Mode != mode {
		t.Fatal("local key dispatch mutated shared settings or encryption mode")
	}
	peer := connect.NewId()
	key, err := settings.EncryptionSettings.NewPeerClientPublicKeyFetcher(peer)(t.Context())
	if err != nil || !bytes.Equal(key, []byte{4, 5, 6}) {
		t.Fatal("public key lost", err)
	}
	history, err := settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher(peer)(t.Context())
	if err != nil || len(history) != 1 || !bytes.Equal(history[0], []byte{1, 2, 3}) {
		t.Fatal("history lost", err)
	}
	local.reject.Store(true)
	if _, err = settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher(peer)(t.Context()); err == nil {
		t.Fatal("history availability error became empty evidence")
	}
	if local.gets.Load() != 3 {
		t.Fatal("both key boundaries were not exercised")
	}
}

// The hosted source adapter cannot accidentally acquire a providing client
// whose separate key/control defaults were not supplied by this authority.
func TestHostedLocalApiRejectsUnownedProviderMode(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	settings.HostedIncompatible = true
	settings.LocalApi = &localDeviceApiTest{}
	settings.AllowProvider = true
	_, err := newDeviceLocalWithOverrides(nil, "synthetic", "test", "test", "test", NewId(), settings, connect.NewId())
	if err == nil || !strings.Contains(err.Error(), "source devices only") {
		t.Fatal("unsupported local provider mode was not refused before construction")
	}
}
