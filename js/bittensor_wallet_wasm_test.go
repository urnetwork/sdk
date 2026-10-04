//go:build js

package main

import (
	"strings"
	"syscall/js"
	"testing"
)

// The web exports answer with the sdk's rules (no network, no timers).
func TestBittensorWalletWasmExports(t *testing.T) {
	registerBittensorWalletExports()
	g := js.Global()

	ids := g.Call("URnetworkBittensorWalletIds")
	if ids.Length() != 3 || ids.Index(0).String() != "talisman" || ids.Index(1).String() != "taocom" || ids.Index(2).String() != "walletconnect" {
		t.Fatalf("ids: %v", ids)
	}
	talisman := g.Call("URnetworkBittensorWalletInfo", "talisman")
	if talisman.Get("transport").String() != "extension" || talisman.Get("injectedName").String() != "talisman" || talisman.Get("displayName").String() != "Talisman" {
		t.Fatalf("talisman: %v", talisman)
	}
	taoCom := g.Call("URnetworkBittensorWalletInfo", "taocom", "web")
	if taoCom.Get("transport").String() != "manual" || taoCom.Get("injectedName").String() != "" {
		t.Fatalf("taocom: %v", taoCom)
	}
	walletConnect := g.Call("URnetworkBittensorWalletInfo", "walletconnect")
	if walletConnect.Get("transport").String() != "walletconnect" || walletConnect.Get("displayName").String() != "WalletConnect" ||
		walletConnect.Get("chain").String() != "polkadot:2f0555cc76fc2840a25a6ea3b9637146" || walletConnect.Get("method").String() != "polkadot_signMessage" {
		t.Fatalf("walletconnect: %v", walletConnect)
	}
	if mobile := g.Call("URnetworkBittensorWalletInfo", "walletconnect", "ios"); mobile.Get("transport").String() != "browser_bridge" {
		t.Fatalf("walletconnect on ios: %v", mobile)
	}
	if !g.Call("URnetworkBittensorWalletInfo", "subwallet-js").IsNull() {
		t.Fatal("an unsupported wallet resolved")
	}
	if got := g.Call("URnetworkBittensorSignRawData", "Hi\nü").String(); got != "0x48690ac3bc" {
		t.Fatalf("sign raw data: %s", got)
	}
	sig := strings.Repeat("AB", 64)
	if got := g.Call("URnetworkNormalizeBittensorSignature", sig).String(); got != "0x"+strings.Repeat("ab", 64) {
		t.Fatalf("normalize: %s", got)
	}
	if got := g.Call("URnetworkNormalizeBittensorSignature", "0x12").String(); got != "" {
		t.Fatalf("normalize short: %s", got)
	}
	parsed := g.Call("URnetworkParseBittensorChallengeMessage", "Sign in to URnetwork\nChallenge: abc\nTimestamp: 1757340000")
	if parsed.Get("challenge").String() != "abc" || parsed.Get("timestamp").Int() != 1757340000 {
		t.Fatalf("parse: %v", parsed)
	}
	if g.Call("URnetworkParseBittensorChallengeMessage", "nope").Get("error").IsUndefined() {
		t.Fatal("parse accepted a bad message")
	}
}
