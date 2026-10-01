package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// One status leaf cannot erase another hard cause in the actual registration
// method. A complete refusal and an unknown reply keep separate ownership.
func TestNetworkClientRegistrationPreservesMixedPhysicalCauses(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	canary := errors.New("synthetic independent custody failure")
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable} {
		for _, hard := range []error{canary, &ClientControlResponseError{detail: "synthetic complete contradiction"}, &os.PathError{Op: "read", Path: "synthetic-owned-file", Err: io.ErrUnexpectedEOF}} {
			joined := errors.Join(&connect.HttpStatusError{StatusCode: status}, hard)
			api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) { return nil, joined })
			_, err := api.RegisterNetworkClientSyncWithContext(ctx, networkClientRegistrationTestArgs())
			var unavailable *NetworkClientRegistrationUnavailableError
			var unsupported *NetworkClientRegistrationUnsupportedError
			if !errors.Is(err, hard) || errors.As(err, &unavailable) || errors.As(err, &unsupported) {
				t.Fatal("one HTTP status hid a joined registration integrity cause")
			}
		}
	}
}

// The configured HTTP server breaks a declared response body. A second real
// request proves the first body failed before controlled owner exhaustion;
// no timeout duration or fabricated transport callback supplies that proof.
func TestClientRefreshPhysicalBodyInterruptionIsUnavailable(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Uint64
	var enteredOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr := io.Copy(io.Discard, io.LimitReader(r.Body, 16*1024))
		closeErr := r.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Error("incomplete refresh fixture input")
			return
		}
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if requests.Add(1) > 1 {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{")
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	defer server.Close()
	defer close(release)
	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Minute
	settings.ConnectTimeout = time.Minute
	settings.ReconnectTimeout = time.Millisecond // Pacing only; the proof is the second request barrier.
	strategy := connect.NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	api := NewApi(t.Context(), strategy, server.URL)
	defer api.CloseAndWait(context.Background())
	api.SetByJwt(testingRefreshableJwtWithMarker(t, "broken-body"))
	owned := &clientControlDeadlineTestContext{Context: context.WithoutCancel(t.Context()), done: make(chan struct{})}
	defer owned.expire()
	done := make(chan error, 1)
	go func() { _, err := api.RefreshJwtSyncWithContext(owned); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("refresh did not observe a real broken-body retry: %v", err)
	case <-t.Context().Done():
		owned.expire()
		<-done
		t.Fatal(t.Context().Err())
	}
	owned.expire()
	err := <-done
	var unavailable *ClientControlUnavailableError
	var exhausted *connect.HttpRequestExhaustedError
	if !errors.As(err, &unavailable) || !errors.As(err, &exhausted) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("physical refresh body loss became a completed integrity verdict: %v", err)
	}
}

// This clock seam only ends an actual operation context after I/O is proven.
// The embedded WithoutCancel prevents context.Cause from discovering a
// different ancestor cancellation while Err truthfully exposes owner expiry.
type clientControlDeadlineTestContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func (self *clientControlDeadlineTestContext) Done() <-chan struct{} { return self.done }
func (self *clientControlDeadlineTestContext) Err() error {
	select {
	case <-self.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (self *clientControlDeadlineTestContext) expire() { self.once.Do(func() { close(self.done) }) }
