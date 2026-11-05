package sdk

// The unique-exit wire models (server HOSTED-PROXY-UNIQUE-EXIT.md §2): the
// request option on /network/auth-client, the exit reported in
// proxy_config_result and the /network/proxies listing, and the rotate-exit
// call. The json names are the API's; an sdk field that drifts from them is
// silently dropped by the server or left empty on decode, so each name is
// pinned here.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
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
// exit's location, since when it holds and its state. There is no IP field.
func TestProxyConfigResultDecodesTheUniqueExit(t *testing.T) {
	raw := `{
		"keepalive_seconds": 300,
		"client_id": "019a5d6e-1d8e-7a3c-9c44-6b7f2a1e0c55",
		"https_proxy_url": "https://token.proxy.example:443",
		"socks_proxy_url": "tcp://proxy.example:1080",
		"auth_token": "token",
		"exit": {
			"exit_id": "ex_7f3a",
			"country_code": "de",
			"region": "Berlin",
			"city": "Berlin",
			"exit_since": "2026-11-05T19:00:00Z",
			"state": "assigned"
		}
	}`
	var result ProxyConfigResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Exit == nil {
		t.Fatal("the unique exit was dropped on decode")
	}
	connect.AssertEqual(t, result.Exit.ExitId, "ex_7f3a")
	connect.AssertEqual(t, result.Exit.CountryCode, "de")
	connect.AssertEqual(t, result.Exit.Region, "Berlin")
	connect.AssertEqual(t, result.Exit.City, "Berlin")
	connect.AssertEqual(t, result.Exit.State, ProxyExitStateAssigned)
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

// The /network/proxies listing carries the same exit inside proxy_client.
func TestNetworkProxyListingDecodesTheUniqueExit(t *testing.T) {
	raw := `{
		"client_id": "019a5d6e-1d8e-7a3c-9c44-6b7f2a1e0c55",
		"auth_token": "token",
		"wg_config": null,
		"exit": {"exit_id": "ex_9b21", "country_code": "us", "state": "replacing"}
	}`
	var proxyClient ProxyClient
	if err := json.Unmarshal([]byte(raw), &proxyClient); err != nil {
		t.Fatal(err)
	}
	if proxyClient.Exit == nil {
		t.Fatal("the listing's unique exit was dropped on decode")
	}
	connect.AssertEqual(t, proxyClient.Exit.ExitId, "ex_9b21")
	connect.AssertEqual(t, proxyClient.Exit.CountryCode, "us")
	connect.AssertEqual(t, proxyClient.Exit.State, ProxyExitStateReplacing)
}

func TestRotateProxyExitWireNames(t *testing.T) {
	clientId, err := ParseId("019a5d6e-1d8e-7a3c-9c44-6b7f2a1e0c55")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(&RotateProxyExitArgs{ClientId: clientId})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(raw), `{"client_id":"019a5d6e-1d8e-7a3c-9c44-6b7f2a1e0c55"}`)

	var rotated RotateProxyExitResult
	if err := json.Unmarshal([]byte(`{"exit": {"exit_id": "ex_c0de", "country_code": "fr", "state": "assigned"}}`), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Exit == nil || rotated.Error != nil {
		t.Fatalf("rotate result = %+v", rotated)
	}
	connect.AssertEqual(t, rotated.Exit.ExitId, "ex_c0de")

	var refused RotateProxyExitResult
	if err := json.Unmarshal([]byte(`{"error": {"message": "no_unique_supply"}}`), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Error == nil || refused.Exit != nil {
		t.Fatalf("refused rotate result = %+v", refused)
	}
}

// Rotating administers the network's proxies, so it carries the network
// credential, never a device's client token.
func TestRotateProxyExitIsANetworkRoute(t *testing.T) {
	connect.AssertEqual(t, apiRouteAccessFor("POST", "/network/proxy/rotate-exit"), apiRouteAccessNetwork)
}
