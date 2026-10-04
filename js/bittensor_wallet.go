//go:build js

package main

import (
	"syscall/js"

	"github.com/urnetwork/sdk"
)

// Bittensor wallet-connect protocol exports for the web app
// (sdk/bittensor_wallet.go). The site drives the Talisman extension itself;
// these are the shared rules it applies: which transport a wallet uses on
// the web, the signRaw data for a challenge, the challenge format check and
// the signature normalization. Every function is synchronous and pure.

func jsStringArg(args []js.Value, i int) string {
	if i < len(args) && args[i].Type() == js.TypeString {
		return args[i].String()
	}
	return ""
}

// URnetworkBittensorWalletIds() -> ["talisman", "taocom"]
func BittensorWalletIds(this js.Value, args []js.Value) any {
	walletIds := sdk.BittensorWalletIdList()
	out := []any{}
	for i := 0; i < walletIds.Len(); i += 1 {
		out = append(out, walletIds.Get(i))
	}
	return js.ValueOf(out)
}

// URnetworkBittensorWalletInfo(walletId, platform) -> {walletId, displayName, injectedName, transport} | null
func BittensorWalletInfo(this js.Value, args []js.Value) any {
	walletId := jsStringArg(args, 0)
	platform := jsStringArg(args, 1)
	if platform == "" {
		platform = sdk.BittensorWalletPlatformWeb
	}
	transport := sdk.BittensorWalletTransportFor(walletId, platform)
	if transport == "" {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"walletId":     walletId,
		"displayName":  sdk.BittensorWalletDisplayName(walletId),
		"injectedName": sdk.BittensorWalletInjectedName(walletId),
		"transport":    transport,
	})
}

// URnetworkBittensorSignRawData(message) -> "0x…"
func BittensorSignRawData(this js.Value, args []js.Value) any {
	return js.ValueOf(sdk.BittensorSignRawData(jsStringArg(args, 0)))
}

// URnetworkNormalizeBittensorSignature(signature) -> "0x…" | ""
func NormalizeBittensorSignature(this js.Value, args []js.Value) any {
	return js.ValueOf(sdk.NormalizeBittensorSignature(jsStringArg(args, 0)))
}

// URnetworkParseBittensorChallengeMessage(message) -> {challenge, timestamp} | {error}
func ParseBittensorChallengeMessage(this js.Value, args []js.Value) any {
	m, err := sdk.ParseBittensorChallengeMessage(jsStringArg(args, 0))
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(map[string]any{
		"challenge": m.Challenge,
		"timestamp": float64(m.Timestamp),
	})
}

func registerBittensorWalletExports() {
	js.Global().Set("URnetworkBittensorWalletIds", js.FuncOf(BittensorWalletIds))
	js.Global().Set("URnetworkBittensorWalletInfo", js.FuncOf(BittensorWalletInfo))
	js.Global().Set("URnetworkBittensorSignRawData", js.FuncOf(BittensorSignRawData))
	js.Global().Set("URnetworkNormalizeBittensorSignature", js.FuncOf(NormalizeBittensorSignature))
	js.Global().Set("URnetworkParseBittensorChallengeMessage", js.FuncOf(ParseBittensorChallengeMessage))
}
