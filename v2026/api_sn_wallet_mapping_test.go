// Checks wallet consent fields and lifetimes through the actual local transport.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Explicit and session-selected providers retain their exact earning interval
// and receive the server's original message bytes without normalization.
func TestApiSnWalletMappingChallengePreservesProviderFields(t *testing.T) {
	const bearerJwt = "synthetic-provider-token"
	const message = "  synthetic wallet consent\noriginal domain and interval\t"
	clientId, err := ParseId("00000000-0000-0000-0000-000000000042")
	if err != nil {
		t.Fatal(err)
	}
	originalClientId := *clientId
	bodyBytes := make(chan []byte, 1)
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != "/sn/wallet/consent" {
			t.Errorf("wallet consent request = %s %s, want POST /sn/wallet/consent", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read wallet consent body: %v", err)
		}
		bodyBytes <- body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"message":%q}`, message)
	}))
	api.SetByJwt(bearerJwt)
	cases := []struct {
		args         *SnWalletMappingChallengeArgs
		expectedBody string
	}{
		{
			args:         &SnWalletMappingChallengeArgs{ClientId: clientId, ColdkeySs58: " synthetic-coldkey ", FromEpoch: 42, ThroughEpoch: math.MaxInt64},
			expectedBody: `{"client_id":"00000000-0000-0000-0000-000000000042","coldkey_ss58":" synthetic-coldkey ","from_epoch":42,"through_epoch":9223372036854775807}`,
		},
		{
			args:         &SnWalletMappingChallengeArgs{ColdkeySs58: "synthetic-coldkey", FromEpoch: 0, ThroughEpoch: 0},
			expectedBody: `{"coldkey_ss58":"synthetic-coldkey","from_epoch":0,"through_epoch":0}`,
		},
	}
	for _, c := range cases {
		originalArgs := *c.args
		result, err := api.SnWalletMappingChallengeSyncWithContext(ctx, c.args)
		if err != nil || result == nil || result.Message != message {
			t.Fatalf("wallet consent result = %+v, %v, want exact server message", result, err)
		}
		select {
		case body := <-bodyBytes:
			requireSnWalletMappingBody(t, body, c.expectedBody)
		default:
			t.Fatal("wallet consent did not send an actual request")
		}
		if *c.args != originalArgs || *clientId != originalClientId || api.GetByJwt() != bearerJwt {
			t.Fatal("wallet consent changed its arguments, provider identity, or credential")
		}
	}
}

// Signed mobile epochs cannot wrap into the server's unsigned representation.
func TestApiSnWalletMappingChallengeRejectsInvalidIntervalsBeforeHttp(t *testing.T) {
	const bearerJwt = "synthetic-provider-token"
	var requestCount atomic.Int64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":"synthetic unexpected consent"}`)
	}))
	api.SetByJwt(bearerJwt)
	for _, args := range []*SnWalletMappingChallengeArgs{
		nil,
		{ColdkeySs58: "synthetic-coldkey", FromEpoch: -1, ThroughEpoch: 0},
		{ColdkeySs58: "synthetic-coldkey", FromEpoch: 0, ThroughEpoch: -1},
		{ColdkeySs58: "synthetic-coldkey", FromEpoch: -2, ThroughEpoch: -1},
		{ColdkeySs58: "synthetic-coldkey", FromEpoch: 43, ThroughEpoch: 42},
	} {
		var originalArgs SnWalletMappingChallengeArgs
		if args != nil {
			originalArgs = *args
		}
		if result, err := api.SnWalletMappingChallengeSyncWithContext(ctx, args); err == nil || result != nil {
			t.Errorf("context-bound invalid interval returned %+v, %v for %+v", result, err, args)
		}
		if result, err := api.SnWalletMappingChallengeSync(args); err == nil || result != nil {
			t.Errorf("api-bound invalid interval returned %+v, %v for %+v", result, err, args)
		}
		if args != nil && *args != originalArgs {
			t.Fatal("invalid interval validation mutated its arguments")
		}
	}
	if requestCount.Load() != 0 || api.GetByJwt() != bearerJwt {
		t.Fatal("invalid interval reached the server or changed the provider credential")
	}
}

// A mapping acceptance preserves the signed submission and exposes the exact
// hash and generation even when the server returns no legacy wallet object.
func TestApiSnSetWalletPreservesMappingAcceptance(t *testing.T) {
	const bearerJwt = "synthetic-provider-token"
	const message = "  synthetic signed wallet consent\noriginal message\t"
	clientId, err := ParseId("00000000-0000-0000-0000-000000000042")
	if err != nil {
		t.Fatal(err)
	}
	originalClientId := *clientId
	args := &SnSetWalletArgs{ClientId: clientId, ColdkeySs58: "synthetic-coldkey", Signature: strings.Repeat("a5", 64), Message: message}
	originalArgs := *args
	mappingHash := strings.Repeat("ab", 32)
	var requestCount atomic.Int64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method != http.MethodPost || r.RequestURI != "/sn/wallet" {
			t.Errorf("wallet acceptance request = %s %s, want POST /sn/wallet", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		var receivedArgs SnSetWalletArgs
		if err := json.NewDecoder(r.Body).Decode(&receivedArgs); err != nil {
			t.Errorf("read signed wallet acceptance: %v", err)
		}
		if receivedArgs.ClientId == nil || receivedArgs.ClientId.String() != clientId.String() || receivedArgs.ColdkeySs58 != args.ColdkeySs58 || receivedArgs.Signature != args.Signature || receivedArgs.Message != message {
			t.Errorf("signed wallet acceptance fields changed: %+v", receivedArgs)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"mapping_hash":%q,"mapping_generation":9223372036854775807}`, mappingHash)
	}))
	api.SetByJwt(bearerJwt)
	result, err := api.SnSetWalletSyncWithContext(ctx, args)
	if err != nil || result == nil || result.Error != nil || result.Wallet != nil || result.MappingHash != mappingHash || result.MappingGeneration != math.MaxInt64 {
		t.Fatalf("wallet mapping acceptance = %+v, %v", result, err)
	}
	if *args != originalArgs || *clientId != originalClientId || api.GetByJwt() != bearerJwt || requestCount.Load() != 1 {
		t.Fatal("wallet acceptance changed caller state or did not issue one request")
	}
}

// Caller cancellation reaches a consent request already observed by the server.
func TestApiSnWalletMappingChallengeRequestContext(t *testing.T) {
	testSnWalletMappingChallengeCancellation(t, false)
}

// The synchronous wrapper remains bounded by the api's lifetime.
func TestApiSnWalletMappingChallengeApiLifetime(t *testing.T) {
	testSnWalletMappingChallengeCancellation(t, true)
}

// Barriers establish request ordering; the deadline only bounds test failures.
func testSnWalletMappingChallengeCancellation(t *testing.T, useApiLifetime bool) {
	t.Helper()
	const bearerJwt = "synthetic-provider-token"
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != "/sn/wallet/consent" {
			t.Errorf("wallet consent request = %s %s, want POST /sn/wallet/consent", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read wallet consent body: %v", err)
		}
		requireSnWalletMappingBody(t, body, `{"coldkey_ss58":"synthetic-coldkey","from_epoch":42,"through_epoch":43}`)
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
	args := &SnWalletMappingChallengeArgs{ColdkeySs58: "synthetic-coldkey", FromEpoch: 42, ThroughEpoch: 43}
	originalArgs := *args
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	guardCtx, cancelGuard := context.WithTimeout(ctx, 10*time.Second)
	defer cancelGuard()
	done := make(chan error, 1)
	go func() {
		var err error
		if useApiLifetime {
			_, err = api.SnWalletMappingChallengeSync(args)
		} else {
			_, err = api.SnWalletMappingChallengeSyncWithContext(requestCtx, args)
		}
		done <- err
	}()
	var wireCtx context.Context
	select {
	case wireCtx = <-started:
	case err := <-done:
		t.Fatalf("wallet consent ended before the server barrier: %v", err)
	case <-guardCtx.Done():
		t.Fatal("wallet consent did not reach the server barrier")
	}
	if useApiLifetime {
		api.Close()
	} else {
		cancelRequest()
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wallet consent cancellation error = %v", err)
		}
	case <-guardCtx.Done():
		t.Fatal("wallet consent ignored its owner's cancellation")
	}
	select {
	case <-wireCtx.Done():
	case <-guardCtx.Done():
		t.Fatal("wallet consent transport remained active after cancellation")
	}
	if *args != originalArgs || api.GetByJwt() != bearerJwt {
		t.Fatal("wallet consent cancellation changed its arguments or credential")
	}
	if useApiLifetime {
		if err := api.CloseAndWait(guardCtx); err != nil {
			t.Fatalf("join api lifetime: %v", err)
		}
	} else if api.ctx.Err() != nil {
		t.Fatal("caller cancellation ended the api lifetime")
	}
}

// Compares wire values without depending on json object field order.
func requireSnWalletMappingBody(t *testing.T, body []byte, expectedBody string) {
	t.Helper()
	var receivedFields, expectedFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &receivedFields); err != nil {
		t.Errorf("decode wallet consent request: %v", err)
		return
	}
	if err := json.Unmarshal([]byte(expectedBody), &expectedFields); err != nil {
		t.Errorf("decode expected wallet consent request: %v", err)
		return
	}
	if !reflect.DeepEqual(receivedFields, expectedFields) {
		t.Errorf("wallet consent body = %s, want %s", body, expectedBody)
	}
}

// The network consent is requested from its own route with the session's
// credential and no client, and an invalid interval never reaches the server.
func TestApiSnNetworkWalletMappingChallengeSendsNoClient(t *testing.T) {
	const bearerJwt = "synthetic-network-token"
	const message = "Approve URnetwork network wallet mapping\nsynthetic original"
	var requestCount atomic.Int64
	bodyBytes := make(chan []byte, 1)
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method != http.MethodPost || r.RequestURI != "/sn/wallet/network-consent" {
			t.Errorf("network consent request = %s %s, want POST /sn/wallet/network-consent", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read network consent body: %v", err)
		}
		bodyBytes <- body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"message":%q}`, message)
	}))
	api.SetByJwt(bearerJwt)
	result, err := api.SnNetworkWalletMappingChallengeSyncWithContext(ctx, &SnNetworkWalletMappingChallengeArgs{ColdkeySs58: "synthetic-coldkey", FromEpoch: 7, ThroughEpoch: 107})
	if err != nil || result == nil || result.Message != message {
		t.Fatalf("network consent result = %+v, %v, want exact server message", result, err)
	}
	requireSnWalletMappingBody(t, <-bodyBytes, `{"coldkey_ss58":"synthetic-coldkey","from_epoch":7,"through_epoch":107}`)
	for _, args := range []*SnNetworkWalletMappingChallengeArgs{nil, {ColdkeySs58: "synthetic-coldkey", FromEpoch: -1, ThroughEpoch: 1}, {ColdkeySs58: "synthetic-coldkey", FromEpoch: 9, ThroughEpoch: 8}} {
		if result, err := api.SnNetworkWalletMappingChallengeSyncWithContext(ctx, args); result != nil || err == nil {
			t.Fatalf("invalid network consent interval %+v was sent", args)
		}
	}
	if requestCount.Load() != 1 {
		t.Fatalf("network consent sent %d requests, want 1", requestCount.Load())
	}
}
