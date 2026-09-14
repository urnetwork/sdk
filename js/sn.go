//go:build js

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"syscall/js"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// UR protocol (subnet 25) exports for the web app. The site has no
// DeviceLocal: it reads a coldkey's entitlements straight from the chain
// with explicit settings and signs claims with the reader's injected EVM
// wallet, so only the settings, the claims scan, the unsigned transactions
// and the local address check are exposed.

func jsPromise(run func(resolve func(any), reject func(error))) js.Value {
	handler := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]
		go connect.HandleError(func() {
			run(
				func(value any) { resolve.Invoke(value) },
				func(err error) { reject.Invoke(js.Global().Get("Error").New(err.Error())) },
			)
		}, func(err error) {
			reject.Invoke(js.Global().Get("Error").New(err.Error()))
		})
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

func jsSnChainSettings(settings *sdk.SnChainSettings) js.Value {
	if settings == nil {
		return js.Null()
	}
	urls := []any{}
	list := settings.RpcUrls()
	for i := 0; i < list.Len(); i++ {
		urls = append(urls, list.Get(i))
	}
	return js.ValueOf(map[string]any{
		"RpcUrls":            urls,
		"ChainId":            settings.ChainId,
		"VaultAddress":       settings.VaultAddress,
		"CoordinatorAddress": settings.CoordinatorAddress,
		"NoId":               settings.NoId,
		"Netuid":             settings.Netuid,
		"ExplorerTxUrl":      settings.ExplorerTxUrl,
		"ArtifactBaseUrl":    settings.ArtifactBaseUrl,
		"TxType":             settings.TxType,
		"LookbackEpochs":     settings.LookbackEpochs,
	})
}

// parseSnChainSettings accepts the object returned by
// URnetworkDefaultSnChainSettings (any key casing: PascalCase, camelCase or
// snake_case) or a json string of it, overlaid on the compiled defaults.
func parseSnChainSettings(v js.Value) (*sdk.SnChainSettings, error) {
	settings := sdk.DefaultSnChainSettings()
	if v.IsNull() || v.IsUndefined() {
		return settings, nil
	}
	var text string
	switch v.Type() {
	case js.TypeString:
		text = v.String()
	case js.TypeObject:
		text = js.Global().Get("JSON").Call("stringify", v).String()
	default:
		return nil, errors.New("settings must be an object or a json string")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, err
	}
	normalized := map[string]any{}
	for key, value := range raw {
		normalized[strings.ToLower(strings.ReplaceAll(key, "_", ""))] = value
	}
	overrides := sdk.NewSnChainSettings()
	str := func(key string) string {
		if s, ok := normalized[key].(string); ok {
			return s
		}
		if f, ok := normalized[key].(float64); ok {
			return json.Number(strconvItoa(int64(f))).String()
		}
		return ""
	}
	num := func(key string) int64 {
		switch n := normalized[key].(type) {
		case float64:
			return int64(n)
		case string:
			var out int64
			for _, c := range strings.TrimSpace(n) {
				if c < '0' || c > '9' {
					return 0
				}
				out = out*10 + int64(c-'0')
			}
			return out
		}
		return 0
	}
	overrides.ChainId = num("chainid")
	overrides.VaultAddress = str("vaultaddress")
	overrides.CoordinatorAddress = str("coordinatoraddress")
	overrides.NoId = str("noid")
	overrides.Netuid = num("netuid")
	overrides.ExplorerTxUrl = str("explorertxurl")
	overrides.ArtifactBaseUrl = str("artifactbaseurl")
	overrides.TxType = str("txtype")
	overrides.LookbackEpochs = num("lookbackepochs")
	if urls, ok := normalized["rpcurls"].([]any); ok {
		for _, u := range urls {
			if s, ok := u.(string); ok {
				overrides.AddRpcUrl(s)
			}
		}
	} else if u, ok := normalized["rpcurls"].(string); ok {
		for _, s := range strings.Split(u, ",") {
			overrides.AddRpcUrl(s)
		}
	}
	return settings.Merge(overrides), nil
}

func strconvItoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsSnError(err *sdk.SnError) any {
	if err == nil {
		return nil
	}
	return map[string]any{"Code": err.Code, "Message": err.Message}
}

func jsSnClaimsResult(result *sdk.SnClaimsResult) js.Value {
	claims := []any{}
	if result.Claims != nil {
		for i := 0; i < result.Claims.Len(); i++ {
			c := result.Claims.Get(i)
			claims = append(claims, map[string]any{
				"Epoch":          c.Epoch,
				"ShareBps":       c.ShareBps,
				"AmountRao":      c.AmountRao,
				"Status":         c.Status,
				"ClaimOpenBlock": c.ClaimOpenBlock,
				"ExpiryBlock":    c.ExpiryBlock,
				"TxHash":         c.TxHash,
				"PayoutRoot":     c.PayoutRoot,
				"ArtifactHash":   c.ArtifactHash,
				"Message":        c.Message,
			})
		}
	}
	return js.ValueOf(map[string]any{
		"Claims":            claims,
		"TotalClaimableRao": result.TotalClaimableRao,
		"CurrentEpoch":      result.CurrentEpoch,
		"BlockNumber":       result.BlockNumber,
		"ColdkeySs58":       result.ColdkeySs58,
		"Error":             jsSnError(result.Error),
	})
}

func parseEpochs(v js.Value) *sdk.Int64List {
	epochs := sdk.NewInt64List()
	if v.IsNull() || v.IsUndefined() {
		return epochs
	}
	if v.Type() == js.TypeObject && v.Get("length").Type() == js.TypeNumber {
		n := v.Get("length").Int()
		for i := 0; i < n; i++ {
			item := v.Index(i)
			switch item.Type() {
			case js.TypeNumber:
				epochs.Add(int64(item.Float()))
			case js.TypeString:
				var epoch int64
				for _, c := range strings.TrimSpace(item.String()) {
					if c >= '0' && c <= '9' {
						epoch = epoch*10 + int64(c-'0')
					}
				}
				epochs.Add(epoch)
			}
		}
	} else if v.Type() == js.TypeNumber {
		epochs.Add(int64(v.Float()))
	}
	return epochs
}

// URnetworkDefaultSnChainSettings() -> settings object
func DefaultSnChainSettings(this js.Value, args []js.Value) any {
	return jsSnChainSettings(sdk.DefaultSnChainSettings())
}

// URnetworkValidateSs58(address) -> boolean
func ValidateSs58(this js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeString {
		return js.ValueOf(false)
	}
	return js.ValueOf(sdk.ValidateSs58(args[0].String()))
}

// URnetworkSnClaimsFor(settings, coldkeySs58[, fromEpoch]) -> Promise<claims result>
func SnClaimsFor(this js.Value, args []js.Value) any {
	return jsPromise(func(resolve func(any), reject func(error)) {
		if len(args) < 2 || args[1].Type() != js.TypeString {
			reject(errors.New("settings and coldkey are required"))
			return
		}
		settings, err := parseSnChainSettings(args[0])
		if err != nil {
			reject(err)
			return
		}
		var fromEpoch int64
		if len(args) > 2 && args[2].Type() == js.TypeNumber {
			fromEpoch = int64(args[2].Float())
		}
		done := make(chan *sdk.SnClaimsResult, 1)
		sdk.SnClaimsFor(settings, args[1].String(), fromEpoch, connect.NewApiCallback[*sdk.SnClaimsResult](func(result *sdk.SnClaimsResult, err error) {
			if err != nil {
				result = &sdk.SnClaimsResult{Claims: sdk.NewSnEpochClaimList(), Error: &sdk.SnError{Message: err.Error()}}
			}
			done <- result
		}))
		resolve(jsSnClaimsResult(<-done))
	})
}

// URnetworkSnClaimTransactionsFor(settings, coldkeySs58, epochs) -> Promise<[{To, Data, Value, ChainId, Epoch, AmountRao}]>
func SnClaimTransactionsFor(this js.Value, args []js.Value) any {
	return jsPromise(func(resolve func(any), reject func(error)) {
		if len(args) < 3 || args[1].Type() != js.TypeString {
			reject(errors.New("settings, coldkey and epochs are required"))
			return
		}
		settings, err := parseSnChainSettings(args[0])
		if err != nil {
			reject(err)
			return
		}
		txs, err := sdk.SnClaimTransactionsFor(settings, args[1].String(), parseEpochs(args[2]))
		if err != nil {
			reject(err)
			return
		}
		out := []any{}
		for i := 0; i < txs.Len(); i++ {
			tx := txs.Get(i)
			out = append(out, map[string]any{
				"Epoch":     tx.Epoch,
				"ChainId":   tx.ChainId,
				"To":        tx.To,
				"Data":      tx.Data,
				"Value":     tx.Value,
				"AmountRao": tx.AmountRao,
			})
		}
		resolve(js.ValueOf(out))
	})
}

// URnetworkSnGasBalanceFor(settings, address) -> Promise<{Address, Wei, Tao, Error}>
func SnGasBalanceFor(this js.Value, args []js.Value) any {
	return jsPromise(func(resolve func(any), reject func(error)) {
		if len(args) < 2 || args[1].Type() != js.TypeString {
			reject(errors.New("settings and address are required"))
			return
		}
		settings, err := parseSnChainSettings(args[0])
		if err != nil {
			reject(err)
			return
		}
		done := make(chan *sdk.SnGasBalanceResult, 1)
		sdk.SnGasBalanceFor(settings, args[1].String(), connect.NewApiCallback[*sdk.SnGasBalanceResult](func(result *sdk.SnGasBalanceResult, err error) {
			done <- result
		}))
		result := <-done
		resolve(js.ValueOf(map[string]any{
			"Address": result.Address,
			"Wei":     result.Wei,
			"Tao":     result.Tao,
			"Error":   jsSnError(result.Error),
		}))
	})
}

// URnetworkFormatAlpha(rao) -> "3.2410 SN25α"
func FormatAlpha(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf("")
	}
	var rao int64
	switch args[0].Type() {
	case js.TypeNumber:
		rao = int64(args[0].Float())
	case js.TypeString:
		for _, c := range strings.TrimSpace(args[0].String()) {
			if c >= '0' && c <= '9' {
				rao = rao*10 + int64(c-'0')
			}
		}
	}
	return js.ValueOf(sdk.FormatAlpha(rao))
}

func registerSnExports() {
	js.Global().Set("URnetworkDefaultSnChainSettings", js.FuncOf(DefaultSnChainSettings))
	js.Global().Set("URnetworkValidateSs58", js.FuncOf(ValidateSs58))
	js.Global().Set("URnetworkSnClaimsFor", js.FuncOf(SnClaimsFor))
	js.Global().Set("URnetworkSnClaimTransactionsFor", js.FuncOf(SnClaimTransactionsFor))
	js.Global().Set("URnetworkSnGasBalanceFor", js.FuncOf(SnGasBalanceFor))
	js.Global().Set("URnetworkFormatAlpha", js.FuncOf(FormatAlpha))
}
