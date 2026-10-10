package sdk

// Actual configured SDK HTTP requests must never transfer a durable allocation
// to an endpoint that did not approve the versioned idempotency contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

func TestNetworkClientRegistrationRefusesRedirectedAllocations(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, crossOrigin := range []bool{false, true} {
			func() {
				args := networkClientRegistrationTestArgs()
				want, err := EncodeNetworkClientRegistration(args)
				if err != nil {
					t.Fatal(err)
				}
				var original, legacy atomic.Uint64
				target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 16*1024))
					_ = r.Body.Close()
					legacy.Add(1)
					_, _ = w.Write(networkClientRegistrationTestResponse(t, want))
				})
				targetServer := httptest.NewServer(target)
				defer targetServer.Close()
				destination := "/network/auth-client"
				if crossOrigin {
					destination = targetServer.URL + destination
				}
				ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/network/auth-client" {
						target.ServeHTTP(w, r)
						return
					}
					raw, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
					closeErr := r.Body.Close()
					if err != nil || closeErr != nil || r.URL.Path != "/network/register-client-v1" || r.Method != http.MethodPost || !bytes.Equal(raw, want) || r.Header.Get("Authorization") != "Bearer synthetic-network-credential" {
						t.Error("redirect refusal changed original request identity or credential")
					}
					original.Add(1)
					w.Header().Set("Location", destination)
					w.WriteHeader(status)
				}))
				api.SetByJwt("synthetic-network-credential")
				result, err := api.RegisterNetworkClientSyncWithContext(ctx, args)
				var invalid *ClientControlResponseError
				var unavailable *NetworkClientRegistrationUnavailableError
				if result != nil || !errors.As(err, &invalid) || errors.As(err, &unavailable) || original.Load() != 1 || legacy.Load() != 0 {
					t.Fatalf("versioned registration followed or retried redirect: cross=%v status=%d original=%d legacy=%d error=%v", crossOrigin, status, original.Load(), legacy.Load(), err)
				}
				if got, err := EncodeNetworkClientRegistration(args); err != nil || !bytes.Equal(got, want) {
					t.Fatal("redirect reply replaced original request bytes")
				}
			}()
		}
	}
}

// Only the direct completed status becomes the endpoint contradiction type.
// A mixed cause must retain its original hard leaf and never become a wait.
func TestNetworkClientRegistrationRedirectPreservesJoinedHardCause(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	api.SetByJwt("synthetic-network-credential")
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		hard := &os.PathError{Op: "read", Path: "synthetic-owned-operation", Err: errors.New("synthetic custody failure")}
		cause := errors.Join(&connect.HttpStatusError{StatusCode: status}, hard)
		api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) { return nil, cause })
		_, err := api.RegisterNetworkClientSyncWithContext(ctx, networkClientRegistrationTestArgs())
		var unavailable *NetworkClientRegistrationUnavailableError
		if !errors.Is(err, hard) || errors.As(err, &unavailable) {
			t.Fatalf("redirect classification hid a joined hard cause: %v", err)
		}
	}
}

// Decoded field maxima cannot imply a physical encoded-body bound: JSON
// escaping can multiply otherwise valid field lengths before the server reads.
func TestNetworkClientRegistrationBoundsEncodedRequestBeforeHttp(t *testing.T) {
	var requests atomic.Uint64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16*1024+1))
		closeErr := r.Body.Close()
		if err != nil || closeErr != nil || len(raw) != 16*1024 {
			t.Errorf("encoded boundary fixture received size %d: %v", len(raw), errors.Join(err, closeErr))
			return
		}
		requests.Add(1)
		_, _ = w.Write(networkClientRegistrationTestResponse(t, raw))
	}))
	api.SetByJwt("synthetic-network-credential")
	args := networkClientRegistrationTestArgs()
	args.DeviceDescription, args.DeviceSpec = strings.Repeat("<", 1024), strings.Repeat("<", 4096)
	if _, err := EncodeNetworkClientRegistration(args); err == nil {
		t.Fatal("encoded registration accepted escaping beyond the server body limit")
	}
	if _, err := api.RegisterNetworkClientSyncWithContext(ctx, args); err == nil || requests.Load() != 0 {
		t.Fatal("oversized encoded registration crossed the physical HTTP boundary")
	}
	args.DeviceDescription, args.DeviceSpec = "", ""
	empty, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	available := 16*1024 - len(empty)
	args.DeviceSpec = strings.Repeat("<", available/6) + strings.Repeat("x", available%6)
	want, err := EncodeNetworkClientRegistration(args)
	if err != nil || len(want) != 16*1024 {
		t.Fatalf("exact encoded boundary refused: size=%d error=%v", len(want), err)
	}
	if result, err := api.RegisterNetworkClientSyncWithContext(ctx, args); err != nil || result == nil || requests.Load() != 1 {
		t.Fatalf("valid exact-boundary request failed physical route: %v", err)
	}
	args.DeviceSpec += "x"
	if _, err := api.RegisterNetworkClientSyncWithContext(ctx, args); err == nil || requests.Load() != 1 {
		t.Fatal("one excess encoded byte reached physical allocation")
	}
}
