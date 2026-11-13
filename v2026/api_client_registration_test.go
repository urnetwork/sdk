package sdk

// The request contract is exercised at the actual configured HTTP client as
// well as the completed-response decoder. Synthetic identities carry no keys.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func networkClientRegistrationTestArgs() *RegisterNetworkClientArgs {
	return &RegisterNetworkClientArgs{Schema: NetworkClientRegistrationSchema, RegistrationId: strings.Repeat("12", 32), ScopeSha256: strings.Repeat("34", 32), DeviceDescription: "synthetic validator", DeviceSpec: "synthetic headless"}
}

func networkClientRegistrationTestResponse(t *testing.T, request []byte) []byte {
	t.Helper()
	var args RegisterNetworkClientArgs
	if err := json.Unmarshal(request, &args); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(request)
	clientId, err := ParseId("00000000-0000-0000-0000-000000000101")
	if err != nil {
		t.Fatal(err)
	}
	deviceId, err := ParseId("00000000-0000-0000-0000-000000000202")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(RegisterNetworkClientResult{Schema: args.Schema, RegistrationId: args.RegistrationId, RequestSha256: hex.EncodeToString(digest[:]), ClientId: clientId, DeviceId: deviceId, ByClientJwt: "synthetic-client-credential"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The real API transport sends precisely one versioned payload; legacy and
// proxy/payment routes are not an implicit capability fallback.
func TestNetworkClientRegistrationExactVersionedHttpRoute(t *testing.T) {
	args := networkClientRegistrationTestArgs()
	want, err := EncodeNetworkClientRegistration(args)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Uint64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
		closeErr := r.Body.Close()
		if err != nil || closeErr != nil || r.Method != http.MethodPost || r.URL.Path != "/network/register-client-v1" || !bytes.Equal(body, want) || r.Header.Get("Authorization") != "Bearer synthetic-network-credential" {
			t.Error("versioned registration changed method, scope, bytes or authorization")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(networkClientRegistrationTestResponse(t, body))
	}))
	api.SetByJwt("synthetic-network-credential")
	result, err := api.RegisterNetworkClientSyncWithContext(ctx, args)
	if err != nil || result == nil || result.Error != nil || requests.Load() != 1 || result.ClientId.String() != "00000000-0000-0000-0000-000000000101" {
		t.Fatalf("versioned operation failed its actual route: requests=%d error=%v", requests.Load(), err)
	}
}

func TestNetworkClientRegistrationUnsupportedDoesNotAllocateLegacy(t *testing.T) {
	var legacy, versioned atomic.Uint64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 16*1024))
		_ = r.Body.Close()
		switch r.URL.Path {
		case "/hello":
			w.WriteHeader(http.StatusOK)
		case "/network/register-client-v1":
			versioned.Add(1)
			http.NotFound(w, r)
		case "/network/auth-client":
			legacy.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Error("registration attempted an unapproved capability route")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	api.SetByJwt("synthetic-network-credential")
	_, err := api.RegisterNetworkClientSyncWithContext(ctx, networkClientRegistrationTestArgs())
	var unsupported *NetworkClientRegistrationUnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Status != http.StatusNotFound || legacy.Load() != 0 || versioned.Load() != 1 {
		t.Fatalf("unsupported capability fell back or lost explicit status: %v", err)
	}
}

// Cancel immediately after the server has observed complete creation bytes.
// A later caller retries those same bytes; no response creates another key.
func TestNetworkClientRegistrationLostHttpReplyRetainsExactRequest(t *testing.T) {
	requestCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var phase atomic.Uint64
	seen := make(chan []byte, 8)
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
		closeErr := r.Body.Close()
		if err != nil || closeErr != nil || r.URL.Path != "/network/register-client-v1" {
			t.Error("lost-reply fixture received an incomplete or unrelated mutation")
			return
		}
		select {
		case seen <- bytes.Clone(body):
		default:
			t.Error("unexpected repeated registration requests")
		}
		if phase.Load() == 0 {
			cancel()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		_, _ = w.Write(networkClientRegistrationTestResponse(t, body))
	}))
	api.SetByJwt("synthetic-network-credential")
	args := networkClientRegistrationTestArgs()
	if _, err := api.RegisterNetworkClientSyncWithContext(requestCtx, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("actual lost reply did not retain cancellation: %v", err)
	}
	first := <-seen
	phase.Store(1)
	result, err := api.RegisterNetworkClientSyncWithContext(t.Context(), args)
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("exact registration replay failed after lost reply: %v", err)
	}
	want, err := EncodeNetworkClientRegistration(args)
	if err != nil || !bytes.Equal(first, want) {
		t.Fatal("first unknown operation changed its original request")
	}
	for len(seen) > 0 {
		if !bytes.Equal(<-seen, want) {
			t.Fatal("lost-reply replay regenerated request identity or payload")
		}
	}
}

// Only a physical request failure is unavailable. Completed null/malformed
// bytes or changed request/identity claims stay explicit integrity failures.
func TestNetworkClientRegistrationRejectsCompletedContradictions(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	api.SetByJwt("synthetic-network-credential")
	args := networkClientRegistrationTestArgs()
	request, err := EncodeNetworkClientRegistration(args)
	if err != nil {
		t.Fatal(err)
	}
	good := networkClientRegistrationTestResponse(t, request)
	for _, fault := range []string{"null", "malformed", "request", "partial", "mixed", "duplicate", "schema-alias", "identity-alias", "verdict-alias", "error-code-alias", "error-message-alias", "zero"} {
		raw := bytes.Clone(good)
		var decoded RegisterNetworkClientResult
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "null":
			raw = []byte("null")
		case "malformed":
			raw = []byte("{")
		case "duplicate":
			raw = bytes.Replace(raw, []byte(`"schema":`), []byte(`"schema":"discarded","schema":`), 1)
		case "schema-alias":
			raw = bytes.Replace(raw, []byte(`"schema":`), []byte(`"Schema":"discarded","schema":`), 1)
		case "identity-alias":
			raw = bytes.Replace(raw, []byte(`"client_id":`), []byte(`"CLIENT_ID":"00000000-0000-0000-0000-000000000999","client_id":`), 1)
		case "verdict-alias":
			raw = append(raw[:len(raw)-1], []byte(`,"error":{"code":"identity_unavailable","message":"refused"},"Error":null}`)...)
		case "error-code-alias":
			raw = []byte(`{"error":{"code":"identity_unavailable","Code":"conflict","message":"refused"}}`)
		case "error-message-alias":
			raw = []byte(`{"error":{"code":"identity_unavailable","message":"first","Message":"second"}}`)
		case "zero":
			decoded.ClientId = newId([16]byte{})
			raw, _ = json.Marshal(decoded)
		case "request":
			decoded.RequestSha256 = strings.Repeat("56", 32)
			raw, _ = json.Marshal(decoded)
		case "partial":
			decoded.DeviceId = nil
			raw, _ = json.Marshal(decoded)
		case "mixed":
			decoded.Error = &RegisterNetworkClientError{Code: "identity_unavailable", Message: "synthetic refusal"}
			raw, _ = json.Marshal(decoded)
		}
		api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) { return raw, nil })
		_, err := api.RegisterNetworkClientSyncWithContext(ctx, args)
		var unavailable *NetworkClientRegistrationUnavailableError
		if err == nil || errors.As(err, &unavailable) {
			t.Fatalf("completed %s response was admitted or retried as unavailable: %v", fault, err)
		}
	}
}
