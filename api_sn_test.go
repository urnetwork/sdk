package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/urnetwork/connect"
)

func TestApiSubnetHeadlessBindings(t *testing.T) {
	clientId := NewId()
	const bearerJwt = "subnet-bearer"
	var requestCount int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/key/" + clientId.String():
			if r.Method != http.MethodGet {
				t.Errorf("key method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("key authorization = %q, want unauthenticated", got)
			}
			fmt.Fprint(w, `{"public_key":"AQID"}`)
		case "/verify/keys":
			if r.Method != http.MethodGet {
				t.Errorf("verify keys method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("verify keys authorization = %q, want unauthenticated", got)
			}
			fmt.Fprint(w, `{"keys":[{"server_key_id":7,"public_key":"BAU="}]}`)
		case "/sn/wallet":
			if r.Method != http.MethodPost {
				t.Errorf("wallet method = %s, want POST", r.Method)
			}
			requireRequestBearer(t, r, bearerJwt)
			var args SnSetWalletArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				t.Errorf("decode wallet args: %v", err)
			}
			if args.ColdkeySs58 != "coldkey" {
				t.Errorf("wallet coldkey = %q, want coldkey", args.ColdkeySs58)
			}
			fmt.Fprint(w, `{}`)
		case "/sn/pool/claim":
			if r.Method != http.MethodGet {
				t.Errorf("pool claim method = %s, want GET", r.Method)
			}
			requireRequestBearer(t, r, bearerJwt)
			if got := r.URL.Query().Get("epoch"); got != "42" {
				t.Errorf("pool claim epoch = %q, want 42", got)
			}
			fmt.Fprint(w, `{"epoch":42,"no_id":"AQI=","coldkey":"AwQ=","share_bps":2500,"proof":["BQY="],"payout_root":"Bw==","contract_address":"0xabc","chain_id":9,"claim_open_block":100}`)
		case "/sn/epoch":
			if r.Method != http.MethodGet {
				t.Errorf("epoch method = %s, want GET", r.Method)
			}
			requireRequestBearer(t, r, bearerJwt)
			fmt.Fprint(w, `{"epoch":43,"start_block":10,"commit_deadline_block":20,"trails_deadline_block":30,"finalize_block":40,"t_epoch_blocks":50,"chain_id":9,"contract_address":"0xdef"}`)
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	api.SetByJwt(bearerJwt)

	keyResult, err := api.GetClientKeySyncWithContext(ctx, &GetClientKeyArgs{ClientId: clientId})
	if err != nil {
		t.Fatal(err)
	}
	if string(keyResult.PublicKey) != string([]byte{1, 2, 3}) {
		t.Fatalf("public key = %v, want [1 2 3]", keyResult.PublicKey)
	}

	keysResult, err := api.VerifyKeysSyncWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keysResult.Keys) != 1 || keysResult.Keys[0].ServerKeyId != 7 || string(keysResult.Keys[0].PublicKey) != string([]byte{4, 5}) {
		t.Fatalf("verify keys result = %+v, want decoded key 7", keysResult.Keys)
	}

	walletResult, err := api.SnSetWalletSyncWithContext(ctx, &SnSetWalletArgs{ColdkeySs58: "coldkey"})
	if err != nil {
		t.Fatal(err)
	}
	if walletResult.Error != nil {
		t.Fatalf("wallet result error = %+v", walletResult.Error)
	}

	claimResult, err := api.SnPoolClaimSyncWithContext(ctx, &SnPoolClaimArgs{Epoch: 42})
	if err != nil {
		t.Fatal(err)
	}
	if claimResult.Epoch != 42 || claimResult.ShareBps != 2500 || claimResult.ContractAddress != "0xabc" || claimResult.ChainId != 9 {
		t.Fatalf("pool claim result = %+v", claimResult)
	}
	if string(claimResult.NoId) != string([]byte{1, 2}) || len(claimResult.Proof) != 1 || string(claimResult.Proof[0]) != string([]byte{5, 6}) {
		t.Fatalf("pool claim byte fields were not decoded: %+v", claimResult)
	}

	epochResult, err := api.SnEpochSyncWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epochResult.Epoch != 43 || epochResult.FinalizeBlock != 40 || epochResult.ContractAddress != "0xdef" {
		t.Fatalf("epoch result = %+v", epochResult)
	}
	if requestCount != 5 {
		t.Fatalf("request count = %d, want 5", requestCount)
	}

	if _, err := api.GetClientKeySyncWithContext(ctx, nil); err == nil {
		t.Fatal("nil GetClientKey args did not fail")
	}
	if _, err := api.GetClientKeySyncWithContext(ctx, &GetClientKeyArgs{}); err == nil {
		t.Fatal("nil client id did not fail")
	}
	if _, err := api.SnPoolClaimSyncWithContext(ctx, nil); err == nil {
		t.Fatal("nil pool claim args did not fail")
	}
	if requestCount != 5 {
		t.Fatalf("invalid arguments sent %d extra requests", requestCount-5)
	}
}

func requireRequestBearer(t *testing.T, request *http.Request, jwt string) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer "+jwt {
		t.Errorf("authorization = %q, want bearer JWT", got)
	}
}

func TestApiHeadlessAuthAndProviderBindings(t *testing.T) {
	const bearerJwt = "headless-bearer"
	type capturedRequest struct {
		method string
		path   string
		body   []byte
	}
	requests := make(chan capturedRequest, 4)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		requests <- capturedRequest{method: r.Method, path: r.URL.Path, body: body}
		requireRequestBearer(t, r, bearerJwt)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/login-with-password":
			fmt.Fprint(w, `{"network":{"by_jwt":"password-jwt","name":"network"}}`)
		case "/auth/code-login":
			fmt.Fprint(w, `{"by_jwt":"code-jwt"}`)
		case "/network/auth-client":
			fmt.Fprint(w, `{"by_client_jwt":"client-jwt"}`)
		case "/network/find-providers2":
			fmt.Fprint(w, `{"providers":[{"client_id":"00000000-0000-0000-0000-000000000009","estimated_bytes_per_second":1234}]}`)
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	api.SetByJwt(bearerJwt)

	passwordResult, err := api.AuthLoginWithPasswordSyncWithContext(ctx, &AuthLoginWithPasswordArgs{
		UserAuth: "user@example.com",
		Password: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if passwordResult.Network == nil || passwordResult.Network.ByJwt != "password-jwt" {
		t.Fatalf("password login result = %+v", passwordResult)
	}

	codeResult, err := api.AuthCodeLoginSyncWithContext(ctx, &AuthCodeLoginArgs{AuthCode: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if codeResult.Jwt != "code-jwt" {
		t.Fatalf("code login JWT = %q, want code-jwt", codeResult.Jwt)
	}

	clientResult, err := api.AuthNetworkClientSyncWithContext(ctx, &AuthNetworkClientArgs{
		ClientId:          NewId(),
		DeviceDescription: "validator",
		DeviceSpec:        "linux",
	})
	if err != nil {
		t.Fatal(err)
	}
	if clientResult.ByClientJwt != "client-jwt" {
		t.Fatalf("client JWT = %q, want client-jwt", clientResult.ByClientJwt)
	}

	providerResult, err := api.FindProviders2SyncWithContext(ctx, &FindProviders2Args{
		Specs:            NewProviderSpecList(),
		Count:            2,
		ExcludeClientIds: NewIdList(),
		RankMode:         "quality",
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerResult.ProviderStats == nil || providerResult.ProviderStats.Len() != 1 || providerResult.ProviderStats.Get(0).EstimatedBytesPerSecond != 1234 {
		t.Fatalf("provider result = %+v", providerResult.ProviderStats)
	}

	expectedPaths := []string{
		"/auth/login-with-password",
		"/auth/code-login",
		"/network/auth-client",
		"/network/find-providers2",
	}
	for _, expectedPath := range expectedPaths {
		request := <-requests
		if request.method != http.MethodPost || request.path != expectedPath {
			t.Fatalf("request = %s %s, want POST %s", request.method, request.path, expectedPath)
		}
		if expectedPath == "/network/find-providers2" {
			values := map[string]any{}
			if err := json.Unmarshal(request.body, &values); err != nil {
				t.Fatal(err)
			}
			if values["rank_mode"] != "quality" {
				t.Fatalf("find-providers rank_mode = %#v, want quality", values["rank_mode"])
			}
		}
	}
}

func TestApiSubnetPoolClaimEscapesNoUserInputIntoQuery(t *testing.T) {
	// Epoch is formatted as an integer rather than interpolated from a string.
	// Pin the exact URL shape because this binding is used with an authenticated
	// validator credential.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan *url.URL, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		copy := *r.URL
		seen <- &copy
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"epoch":9223372036854775807}`)
	}))
	defer ts.Close()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	// math.MaxInt64, was ^uint64(0): Epoch is int64 so the type binds to
	// android/apple (gomobile cannot bind uint64). The extreme value is here
	// to pin %d formatting, not because an epoch is ever this large, so the
	// largest value the field can hold still serves the purpose.
	result, err := api.SnPoolClaimSyncWithContext(ctx, &SnPoolClaimArgs{Epoch: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if result.Epoch != math.MaxInt64 {
		t.Fatalf("epoch = %d, want max int64", result.Epoch)
	}
	requestUrl := <-seen
	if requestUrl.Path != "/sn/pool/claim" || requestUrl.RawQuery != "epoch=9223372036854775807" {
		t.Fatalf("pool claim URL = %s, want exact epoch query", requestUrl.String())
	}
}
