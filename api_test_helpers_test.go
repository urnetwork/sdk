package sdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"
)

// Separates the transport availability probe from application traffic. Tests
// that count or sequence API operations must not count strategy discovery.
func testApiHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// Owns the complete API transport lifetime around a local server.
func newTestApi(t *testing.T, handler http.Handler) (context.Context, *Api) {
	t.Helper()
	server := httptest.NewServer(testApiHandler(handler))
	t.Cleanup(server.Close)
	return newTestApiForURL(t, server.URL)
}

// Keeps API fixtures on one direct dialer. Parallel GET attempt semantics are
// covered in Connect; binding tests need one request per logical operation.
func newTestClientStrategy(ctx context.Context) *connect.ClientStrategy {
	settings := connect.DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	return connect.NewClientStrategy(ctx, settings)
}

// Owns the API, its strategy, and their workers around an externally managed
// endpoint.
func newTestApiForURL(t *testing.T, apiUrl string) (context.Context, *Api) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientStrategy := newTestClientStrategy(ctx)
	api := NewApi(ctx, clientStrategy, apiUrl)
	t.Cleanup(func() {
		api.Close()
		cancel()
		_ = api.CloseAndWait(context.Background())
		clientStrategy.Close()
	})
	return ctx, api
}
