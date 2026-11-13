package sdk

// The unique-exit wire models (connect api/bringyour.yml, server
// HOSTED-PROXY-UNIQUE-EXIT.md §2): the request option on /network/auth-client,
// the exit reported in proxy_config_result and beside proxy_client in the
// /network/proxies listing, the refusals, and the rotate-exit call. The json
// names are the API's; an sdk field that drifts from them is silently dropped
// by the server or left empty on decode, so each name is pinned here.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

const (
	testUniqueExitClientId = "019a5d6e-1d8e-7a3c-9c44-6b7f2a1e0c55"
	testUniqueExitId       = "019a5d70-2c11-7b0e-8a51-0f6c3d2e9b47"
)

func TestProxyConfigUniqueExitWireNames(t *testing.T) {
	config := &ProxyConfig{
		UniqueExit:        true,
		UniqueExitReplace: ProxyUniqueExitReplaceManual,
		InitialDeviceState: &ProxyDeviceState{
			CountryCode: "de",
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, fields["unique_exit"], true)
	connect.AssertEqual(t, fields["unique_exit_replace"], "manual")

	var decoded ProxyConfig
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, decoded.UniqueExit, true)
	connect.AssertEqual(t, decoded.UniqueExitReplace, ProxyUniqueExitReplaceManual)
}

// A proxy created without the option sends exactly the request it sent before
// the option existed, so a server that predates unique exits sees no change.
func TestProxyConfigWithoutUniqueExitOmitsTheOption(t *testing.T) {
	raw, err := json.Marshal(DefaultProxyConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "unique_exit") {
		t.Fatalf("a default proxy config sends the unique exit option: %s", raw)
	}
}

// The exit as the server reports it in proxy_config_result: an opaque id, the
// exit's location, since when it holds, its state and replace mode. There is
// no IP field.
func TestProxyConfigResultDecodesTheUniqueExit(t *testing.T) {
	raw := `{
		"keepalive_seconds": 300,
		"client_id": "` + testUniqueExitClientId + `",
		"https_proxy_url": "https://token.proxy.example:443",
		"socks_proxy_url": "tcp://proxy.example:1080",
		"auth_token": "token",
		"exit": {
			"exit_id": "` + testUniqueExitId + `",
			"country_code": "de",
			"region": "Berlin",
			"city": "Berlin",
			"exit_since": "2026-11-05T19:00:00Z",
			"state": "assigned",
			"replace_mode": "auto"
		}
	}`
	var result ProxyConfigResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Exit == nil {
		t.Fatal("the unique exit was dropped on decode")
	}
	if result.Exit.ExitId == nil || result.Exit.ExitId.String() != testUniqueExitId {
		t.Fatalf("exit_id = %v", result.Exit.ExitId)
	}
	connect.AssertEqual(t, result.Exit.CountryCode, "de")
	connect.AssertEqual(t, result.Exit.Region, "Berlin")
	connect.AssertEqual(t, result.Exit.City, "Berlin")
	connect.AssertEqual(t, result.Exit.State, ProxyExitStateAssigned)
	connect.AssertEqual(t, result.Exit.StateReason, "")
	connect.AssertEqual(t, result.Exit.ReplaceMode, ProxyUniqueExitReplaceAuto)
	if result.Exit.ExitSince == nil {
		t.Fatal("exit_since was dropped on decode")
	}

	// a proxy without the option carries no exit, and encodes none
	var plain ProxyConfigResult
	if err := json.Unmarshal([]byte(`{"keepalive_seconds": 300, "auth_token": "token"}`), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Exit != nil {
		t.Fatalf("a proxy without a unique exit decoded one: %+v", plain.Exit)
	}
	encoded, err := json.Marshal(&plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"exit"`) {
		t.Fatalf("a proxy without a unique exit encodes one: %s", encoded)
	}
}

// A failing exit reports why while it waits; a proxy with no exit yet has no
// exit_id. state and replace_mode are always sent.
func TestProxyExitDecodesAWaitingExit(t *testing.T) {
	var exit ProxyExit
	if err := json.Unmarshal([]byte(`{"state": "no_supply", "state_reason": "offline", "replace_mode": "manual"}`), &exit); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, exit.State, ProxyExitStateNoSupply)
	connect.AssertEqual(t, exit.StateReason, ProxyExitStateReasonOffline)
	connect.AssertEqual(t, exit.ReplaceMode, ProxyUniqueExitReplaceManual)
	if exit.ExitId != nil {
		t.Fatalf("a proxy with no exit decoded exit_id %v", exit.ExitId)
	}
	encoded, err := json.Marshal(&ProxyExit{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"state"`, `"replace_mode"`} {
		if !strings.Contains(string(encoded), name) {
			t.Fatalf("an empty exit does not encode %s: %s", name, encoded)
		}
	}
}

// The /network/proxies listing carries the exit at item level, beside
// proxy_client, not inside it.
func TestNetworkProxyListingDecodesTheUniqueExitBesideTheProxyClient(t *testing.T) {
	raw := `{
		"clients": [{
			"client_id": "` + testUniqueExitClientId + `",
			"proxy_client": {"client_id": "` + testUniqueExitClientId + `", "auth_token": "token", "wg_config": null},
			"exit": {"exit_id": "` + testUniqueExitId + `", "country_code": "us", "state": "replacing", "state_reason": "unreachable", "replace_mode": "auto"},
			"connections": null
		}]
	}`
	var listing struct {
		Clients []*NetworkClientInfo `json:"clients"`
	}
	if err := json.Unmarshal([]byte(raw), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Clients) != 1 {
		t.Fatalf("clients = %d", len(listing.Clients))
	}
	item := listing.Clients[0]
	if item.ProxyClient == nil {
		t.Fatal("the listing's proxy_client was dropped on decode")
	}
	if item.Exit == nil {
		t.Fatal("the listing's item-level unique exit was dropped on decode")
	}
	connect.AssertEqual(t, item.Exit.CountryCode, "us")
	connect.AssertEqual(t, item.Exit.State, ProxyExitStateReplacing)
	connect.AssertEqual(t, item.Exit.StateReason, ProxyExitStateReasonUnreachable)
}

// The auth-client refusal of a unique exit with no supply names itself; the
// Pro gate is the plan limit's upgrade_required.
func TestAuthNetworkClientErrorDecodesTheUniqueExitRefusals(t *testing.T) {
	var noSupply AuthNetworkClientResult
	if err := json.Unmarshal([]byte(`{"error": {"client_limit_exceeded": false, "no_unique_supply": true, "message": "No unique exit IP is available in this location right now."}}`), &noSupply); err != nil {
		t.Fatal(err)
	}
	if noSupply.Error == nil || !noSupply.Error.NoUniqueSupply || noSupply.Error.UpgradeRequired {
		t.Fatalf("no supply refusal = %+v", noSupply.Error)
	}

	var notPro AuthNetworkClientResult
	if err := json.Unmarshal([]byte(`{"error": {"client_limit_exceeded": false, "upgrade_required": true, "message": "Unique exit IPs are a UR Pro feature."}}`), &notPro); err != nil {
		t.Fatal(err)
	}
	if notPro.Error == nil || !notPro.Error.UpgradeRequired || notPro.Error.NoUniqueSupply {
		t.Fatalf("pro refusal = %+v", notPro.Error)
	}
}

func TestRotateProxyExitWireNames(t *testing.T) {
	clientId, err := ParseId(testUniqueExitClientId)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(&RotateProxyExitArgs{ClientId: clientId})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(raw), `{"client_id":"`+testUniqueExitClientId+`"}`)

	var rotated RotateProxyExitResult
	if err := json.Unmarshal([]byte(`{"exit": {"exit_id": "`+testUniqueExitId+`", "country_code": "fr", "state": "assigned", "replace_mode": "auto"}}`), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Exit == nil || rotated.Error != nil {
		t.Fatalf("rotate result = %+v", rotated)
	}
	connect.AssertEqual(t, rotated.Exit.CountryCode, "fr")

	for _, c := range []struct {
		raw            string
		noUniqueSupply bool
		rateLimited    bool
	}{
		{`{"error": {"no_unique_supply": true, "message": "No unique exit IP is available in this location right now."}}`, true, false},
		{`{"error": {"rate_limited": true, "message": "This proxy rotated in the last minute."}}`, false, true},
		{`{"error": {"message": "Not a unique-exit proxy of this network."}}`, false, false},
	} {
		var refused RotateProxyExitResult
		if err := json.Unmarshal([]byte(c.raw), &refused); err != nil {
			t.Fatal(err)
		}
		if refused.Error == nil || refused.Exit != nil {
			t.Fatalf("refused rotate result = %+v", refused)
		}
		connect.AssertEqual(t, refused.Error.NoUniqueSupply, c.noUniqueSupply)
		connect.AssertEqual(t, refused.Error.RateLimited, c.rateLimited)
		if refused.Error.Message == "" {
			t.Fatal("the refusal message was dropped on decode")
		}
	}
}

// Rotating administers the network's proxies, so it carries the network
// credential, never a device's client token (the server answers a client
// token with 403).
func TestRotateProxyExitIsANetworkRoute(t *testing.T) {
	connect.AssertEqual(t, apiRouteAccessFor("POST", "/network/proxy/rotate-exit"), apiRouteAccessNetwork)
}
