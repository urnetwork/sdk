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
	if ids.Length() != 2 || ids.Index(0).String() != "talisman" || ids.Index(1).String() != "taocom" {
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
