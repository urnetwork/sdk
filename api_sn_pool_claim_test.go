package sdk

// Exercises legacy proof selection and request lifetimes over local http.
// Deadlines only bound failures; server barriers establish cancellation order.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// An empty selector preserves the original authenticated get request exactly.
func TestApiSubnetPoolClaimEmptyLegacyColdkeyPreservesRequest(t *testing.T) {
	const bearerJwt = "synthetic-pool-claim-token"
	var requestCount atomic.Int64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		requireSnPoolClaimRequest(t, r, "/sn/pool/claim?epoch=9223372036854775807", bearerJwt)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"epoch":9223372036854775807}`)
	}))
	api.SetByJwt(bearerJwt)
	args := &SnPoolClaimArgs{Epoch: math.MaxInt64}
	originalArgs := *args

	result, err := api.SnPoolClaimSyncWithContext(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Epoch != originalArgs.Epoch {
		t.Fatalf("pool claim result = %+v, want original epoch %d", result, originalArgs.Epoch)
	}
	if *args != originalArgs || api.GetByJwt() != bearerJwt {
		t.Fatal("pool claim changed its arguments or api credential")
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("pool claim wire request count = %d, want 1", got)
	}
}

// Query metacharacters exercise transport escaping; the server validates ss58.
func TestApiSubnetPoolClaimLegacyColdkeyEscapesQuery(t *testing.T) {
	const bearerJwt = "synthetic-pool-claim-token"
	const legacyColdkey = "synthetic original coldkey +/&epoch=7#%?"
	const expectedUri = "/sn/pool/claim?epoch=9223372036854775807&legacy_coldkey=synthetic+original+coldkey+%2B%2F%26epoch%3D7%23%25%3F"
	var requestCount atomic.Int64
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		requireSnPoolClaimRequest(t, r, expectedUri, bearerJwt)
		if query := r.URL.Query(); len(query) != 2 || query.Get("epoch") != "9223372036854775807" || query.Get("legacy_coldkey") != legacyColdkey {
			t.Errorf("pool claim query = %v, want original epoch and exact legacy selector", query)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"epoch":9223372036854775807}`)
	}))
	api.SetByJwt(bearerJwt)
	args := &SnPoolClaimArgs{Epoch: math.MaxInt64, LegacyColdkey: legacyColdkey}
	originalArgs := *args

	result, err := api.SnPoolClaimSync(args)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Epoch != originalArgs.Epoch {
		t.Fatalf("pool claim result = %+v, want original epoch %d", result, originalArgs.Epoch)
	}
	if *args != originalArgs || api.GetByJwt() != bearerJwt {
		t.Fatal("legacy proof selection changed its arguments or api credential")
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("pool claim wire request count = %d, want 1", got)
	}
}

// Caller cancellation ends the in-flight get and leaves the api usable for a
// later ordinary claim; a server barrier establishes the cancellation order.
func TestApiSubnetPoolClaimLegacyColdkeyRequestContext(t *testing.T) {
	const bearerJwt = "synthetic-pool-claim-token"
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("legacy_coldkey") != "" {
			requireSnPoolClaimRequest(t, r, "/sn/pool/claim?epoch=42&legacy_coldkey=synthetic-original-coldkey", bearerJwt)
			select {
			case started <- r.Context():
			case <-release:
				return
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		requireSnPoolClaimRequest(t, r, "/sn/pool/claim?epoch=43", bearerJwt)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"epoch":43}`)
	}))
	t.Cleanup(func() { close(release) })
	api.SetByJwt(bearerJwt)
	args := &SnPoolClaimArgs{Epoch: 42, LegacyColdkey: "synthetic-original-coldkey"}
	originalArgs := *args
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	guardCtx, cancelGuard := context.WithTimeout(ctx, 10*time.Second)
	defer cancelGuard()
	done := make(chan error, 1)
	go func() {
		_, err := api.SnPoolClaimSyncWithContext(requestCtx, args)
		done <- err
	}()

	var wireCtx context.Context
	select {
	case wireCtx = <-started:
	case err := <-done:
		t.Fatalf("pool claim ended before the server barrier: %v", err)
	case <-guardCtx.Done():
		t.Fatal("pool claim did not reach the server barrier")
	}
	cancelRequest()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pool claim cancellation error = %v, want context cancellation", err)
		}
	case <-guardCtx.Done():
		t.Fatal("pool claim ignored caller cancellation")
	}
	select {
	case <-wireCtx.Done():
	case <-guardCtx.Done():
		t.Fatal("pool claim transport remained active after caller cancellation")
	}
	if *args != originalArgs || api.GetByJwt() != bearerJwt || api.ctx.Err() != nil {
		t.Fatal("caller cancellation changed claim arguments, api credential, or api lifetime")
	}
	result, err := api.SnPoolClaimSyncWithContext(guardCtx, &SnPoolClaimArgs{Epoch: 43})
	if err != nil || result == nil || result.Epoch != 43 {
		t.Fatalf("ordinary claim after legacy cancellation = %+v, %v", result, err)
	}
}

// The context-free wrapper retains the api lifetime while carrying a selector.
func TestApiSubnetPoolClaimLegacyColdkeyApiLifetime(t *testing.T) {
	const bearerJwt = "synthetic-pool-claim-token"
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireSnPoolClaimRequest(t, r, "/sn/pool/claim?epoch=42&legacy_coldkey=synthetic-original-coldkey", bearerJwt)
		select {
		case started <- r.Context():
		case <-release:
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	api.SetByJwt(bearerJwt)
	args := &SnPoolClaimArgs{Epoch: 42, LegacyColdkey: "synthetic-original-coldkey"}
	originalArgs := *args
	guardCtx, cancelGuard := context.WithTimeout(ctx, 10*time.Second)
	defer cancelGuard()
	done := make(chan error, 1)
	go func() {
		_, err := api.SnPoolClaimSync(args)
		done <- err
	}()

	var wireCtx context.Context
	select {
	case wireCtx = <-started:
	case err := <-done:
		t.Fatalf("pool claim ended before the server barrier: %v", err)
	case <-guardCtx.Done():
		t.Fatal("pool claim did not reach the server barrier")
	}
	api.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pool claim api-close error = %v, want context cancellation", err)
		}
	case <-guardCtx.Done():
		t.Fatal("pool claim ignored api closure")
	}
	select {
	case <-wireCtx.Done():
	case <-guardCtx.Done():
		t.Fatal("pool claim transport remained active after api closure")
	}
	if err := api.CloseAndWait(guardCtx); err != nil {
		t.Fatalf("join api lifetime: %v", err)
	}
	if *args != originalArgs || api.GetByJwt() != bearerJwt {
		t.Fatal("api closure changed claim arguments or api credential")
	}
}

// Checks the actual request without treating its selector as a credential.
func requireSnPoolClaimRequest(t *testing.T, request *http.Request, expectedUri string, bearerJwt string) {
	t.Helper()
	if request.Method != http.MethodGet || request.RequestURI != expectedUri {
		t.Errorf("pool claim request = %s %s, want GET %s", request.Method, request.RequestURI, expectedUri)
	}
	requireRequestBearer(t, request, bearerJwt)
	body, err := io.ReadAll(request.Body)
	if err != nil || len(body) != 0 {
		t.Errorf("pool claim request body = %q, %v, want empty", body, err)
	}
}
