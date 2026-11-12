//go:build js

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"syscall/js"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// UR protocol (subnet 25) exports for the web app. The site has no
// DeviceLocal: it reads a coldkey's entitlements straight from the chain
// with explicit settings and signs claims with the reader's injected EVM
// wallet, so only the settings, the claims scan, the v2 accounts, the v1 and
// v2 unsigned transactions (checked against the paying wallet's balance), what
// a mined v2 claim did, and the local address check are exposed, with the
// claim relay's status and Claim now, and a client's or a network's capture
// the flag wins, over what the page reads (sn_claim_relay.go).

func jsPromise(run func(resolve func(any), reject func(error))) js.Value {
	handler := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]
		go connect.HandleError(func() {
			run(
				func(value any) { resolve.Invoke(value) },
				// an api call's failure carries its kind fields (api_error.go)
				func(err error) { reject.Invoke(jsError(err)) },
			)
		}, func(err error) {
			reject.Invoke(js.Global().Get("Error").New(err.Error()))
		})
		return nil
	})
	defer handler.Release()
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
		"RpcUrls":             urls,
		"ChainId":             settings.ChainId,
		"VaultAddress":        settings.VaultAddress,
		"Vault2Address":       settings.Vault2Address,
		"ClaimBatcherAddress": settings.ClaimBatcherAddress,
		"CoordinatorAddress":  settings.CoordinatorAddress,
		"NoId":                settings.NoId,
		"Netuid":              settings.Netuid,
		"ExplorerTxUrl":       settings.ExplorerTxUrl,
		"ArtifactBaseUrl":     settings.ArtifactBaseUrl,
		"TxType":              settings.TxType,
		"LookbackEpochs":      settings.LookbackEpochs,
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
	overrides.Vault2Address = str("vault2address")
	overrides.ClaimBatcherAddress = str("claimbatcheraddress")
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

// The schedule as a page reads it, with the Go field names; nil without one.
func jsSnEpochSchedule(schedule *sdk.SnEpochSchedule) any {
	if schedule == nil {
		return nil
	}
	return map[string]any{
		"Epoch":                 schedule.Epoch,
		"EpochBlocks":           schedule.EpochBlocks,
		"ClaimOpenOffsetBlocks": schedule.ClaimOpenOffsetBlocks,
		"ClaimTtlEpochs":        schedule.ClaimTtlEpochs,
		"ClaimGraceEpochs":      schedule.ClaimGraceEpochs,
		"EndBlock":              schedule.EndBlock,
		"ClaimOpenBlock":        schedule.ClaimOpenBlock,
		"ExpiryBlock":           schedule.ExpiryBlock,
		"HeadBlock":             schedule.HeadBlock,
		"HeadMillis":            schedule.HeadMillis,
		"EndMillis":             schedule.EndMillis,
		"ClaimOpenMillis":       schedule.ClaimOpenMillis,
		"ExpiryMillis":          schedule.ExpiryMillis,
	}
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
		"Schedule":          jsSnEpochSchedule(result.Schedule),
		"Error":             jsSnError(result.Error),
	})
}

// The v2 accounts as a page reads them, with the Go field names: one per pool
// read, with its kind and label, and each pool that could not be read in
// UnavailablePools, which the page shows as unavailable. An Error means the
// balances are unavailable, never zero.
func jsSnLedgerAccountsResult(result *sdk.SnLedgerAccountsResult) js.Value {
	accounts := []any{}
	if result.Accounts != nil {
		for i := 0; i < result.Accounts.Len(); i++ {
			a := result.Accounts.Get(i)
			accounts = append(accounts, map[string]any{
				"Vault":        a.Vault,
				"NoId":         a.NoId,
				"Kind":         a.Kind,
				"Label":        a.Label,
				"Index":        a.Index,
				"FinalizedRao": a.FinalizedRao,
				"ClaimableRao": a.ClaimableRao,
				"PaidRao":      a.PaidRao,
				"Pending":      a.Pending,
				"PendingRao":   a.PendingRao,
			})
		}
	}
	unavailable := []any{}
	if result.UnavailablePools != nil {
		for i := 0; i < result.UnavailablePools.Len(); i++ {
			pool := result.UnavailablePools.Get(i)
			unavailable = append(unavailable, map[string]any{
				"Vault": pool.Vault,
				"NoId":  pool.NoId,
				"Kind":  pool.Kind,
				"Label": pool.Label,
			})
		}
	}
	return js.ValueOf(map[string]any{
		"Accounts":         accounts,
		"UnavailablePools": unavailable,
		"Partial":          result.Partial,
		"ColdkeySs58":      result.ColdkeySs58,
		"Error":            jsSnError(result.Error),
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

// URnetworkSnLedgerAccountsFor(settings, coldkeySs58) -> Promise<v2 accounts result>
func SnLedgerAccountsFor(this js.Value, args []js.Value) any {
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
		done := make(chan *sdk.SnLedgerAccountsResult, 1)
		sdk.SnLedgerAccountsFor(settings, args[1].String(), connect.NewApiCallback[*sdk.SnLedgerAccountsResult](func(result *sdk.SnLedgerAccountsResult, err error) {
			done <- result
		}))
		resolve(jsSnLedgerAccountsResult(<-done))
	})
}

// URnetworkSnClaimTransactionsFor(settings, coldkeySs58, epochs) -> Promise<[{To, Data, Value, ChainId, Epoch, AmountRao, Gas}]>
//
// One v1 claim transaction per epoch the coldkey can claim, for the reader's
// wallet to send with Gas as its gas limit: below it the vault defers the
// payment, and a wallet's own estimate finds that.
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
		resolve(jsSnUnsignedTxs(txs))
	})
}

// The v1 claim transactions as a page reads them, with the Go field names.
func jsSnUnsignedTxs(txs *sdk.SnUnsignedTxList) js.Value {
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
			"Gas":       tx.Gas,
		})
	}
	return js.ValueOf(out)
}

// URnetworkSnLedgerClaimTransactionsFor(settings, coldkeySs58) -> Promise<[{NoId, Index, RootIndex, ChainId, To, Data, Value, AmountRao, Gas, Vault, Batch, Items}]>
//
// The v2 claim transactions for the reader's wallet to send, in order, each
// with Gas as its gas limit: with two or more accounts to claim (a pool and the
// capture the flag pool), one batch to the settings' ClaimBatcherAddress
// (Batch, NoId 0, Items the accounts); else one claim per account to the
// settings' Vault2Address, whose Gas covers vault2's staking-hotkey list, which
// third parties can lengthen before the claim is included. Items lists the
// accounts a transaction claims: [{NoId, Index, RootIndex, Kind, Label,
// AmountRao}]. It rejects with "<code>: <detail>" when there is none:
// already_claimed, not_claimable, proof_mismatch, chain_not_configured (no
// Vault2Address), ledger_unavailable.
func SnLedgerClaimTransactionsFor(this js.Value, args []js.Value) any {
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
		txs, err := sdk.SnLedgerClaimTransactionsFor(settings, args[1].String())
		if err != nil {
			reject(err)
			return
		}
		resolve(jsSnLedgerUnsignedTxs(txs))
	})
}

// URnetworkSnLedgerClaimTransactionsForPayer(settings, coldkeySs58, payerAddress) -> Promise<{transactions, still_claimable, payer_address, payer_balance_wei, fee_per_gas_wei, max_cost_wei, all_cost_wei, needs_gas}>
//
// The v2 claim transactions for the reader's wallet at payerAddress, which pays
// their gas, checked against its balance at the chain's price: those of
// URnetworkSnLedgerClaimTransactionsFor when it pays for them all, else those
// of the most accounts it can pay for, the largest first (a batch when they
// can be), else the largest account's claim alone, and every other account in
// still_claimable ({no_id, index, status "still_claimable", amount_rao,
// message}): no transaction claims it now, a later claim does. needs_gas: it
// cannot pay even for the largest account's claim alone, so transactions is []
// and every account is in still_claimable, with max_cost_wei what that claim
// can cost. Every key is snake_case, the sdk's json; a transactions entry
// passes to URnetworkSnLedgerClaimOutcomes as it is. It rejects as
// URnetworkSnLedgerClaimTransactionsFor does, and with
// "invalid_ss58_address: ..." for a payer that is not an EVM address.
func SnLedgerClaimTransactionsForPayer(this js.Value, args []js.Value) any {
	return jsPromise(func(resolve func(any), reject func(error)) {
		if len(args) < 3 || args[1].Type() != js.TypeString || args[2].Type() != js.TypeString {
			reject(errors.New("settings, coldkey and the paying wallet's address are required"))
			return
		}
		settings, err := parseSnChainSettings(args[0])
		if err != nil {
			reject(err)
			return
		}
		result, err := sdk.SnLedgerClaimTransactionsForPayer(settings, args[1].String(), args[2].String())
		if err != nil {
			reject(err)
			return
		}
		resolve(jsJson(result))
	})
}

// The v2 claim transactions as a page reads them, with the Go field names.
func jsSnLedgerUnsignedTxs(txs *sdk.SnLedgerUnsignedTxList) js.Value {
	out := []any{}
	for i := 0; i < txs.Len(); i++ {
		tx := txs.Get(i)
		items := []any{}
		if tx.Items != nil {
			for j := 0; j < tx.Items.Len(); j++ {
				item := tx.Items.Get(j)
				items = append(items, map[string]any{
					"NoId":      item.NoId,
					"Index":     item.Index,
					"RootIndex": item.RootIndex,
					"Kind":      item.Kind,
					"Label":     item.Label,
					"AmountRao": item.AmountRao,
				})
			}
		}
		out = append(out, map[string]any{
			"NoId":      tx.NoId,
			"Index":     tx.Index,
			"RootIndex": tx.RootIndex,
			"ChainId":   tx.ChainId,
			"To":        tx.To,
			"Data":      tx.Data,
			"Value":     tx.Value,
			"AmountRao": tx.AmountRao,
			"Gas":       tx.Gas,
			"Vault":     tx.Vault,
			"Batch":     tx.Batch,
			"Items":     items,
		})
	}
	return js.ValueOf(out)
}

// URnetworkSnLedgerClaimOutcomes(tx, receipt) -> Promise<[{no_id, index, status, amount_rao, message}]>
//
// What a mined transaction of URnetworkSnLedgerClaimTransactionsFor did for
// each account it claims, in its Items' order: status "confirmed" with the
// amount_rao vault2's Claimed event paid, "failed" with the sdk's "<code>:
// <detail>" message (already_claimed is success for the account), or
// "not_attempted", for an account a batch ran short of gas before, which the
// page claims again with the transactions built anew. tx is the entry as the
// export gave it, in any key casing; receipt is the wallet's receipt (JSON-RPC,
// ethers or viem), of which only status and logs are read. It rejects when
// either cannot be read.
func SnLedgerClaimOutcomes(this js.Value, args []js.Value) any {
	var txRaw []byte
	var txErr error
	if 0 < len(args) {
		txRaw, txErr = jsSnakeJson(args[0])
	}
	receiptJson, receiptErr := jsReceiptJson(firstJsArg(args[min(1, len(args)):]))
	return jsPromise(func(resolve func(any), reject func(error)) {
		switch {
		case txErr != nil || txRaw == nil:
			reject(errors.New("the transaction is required"))
			return
		case receiptErr != nil:
			reject(receiptErr)
			return
		}
		var tx sdk.SnLedgerUnsignedTx
		if err := json.Unmarshal(txRaw, &tx); err != nil {
			reject(err)
			return
		}
		outcomes, err := sdk.SnLedgerClaimOutcomesOf(&tx, receiptJson)
		if err != nil {
			reject(err)
			return
		}
		resolve(jsJson(outcomes))
	})
}

// URnetworkSnLedgerClaimOutcomesFor(settings, coldkeySs58, tx, receipt) -> Promise<[{no_id, index, status, amount_rao, message}]>
//
// URnetworkSnLedgerClaimOutcomes with the reasons read from the chain, as the
// device's claim reads them: an account a batch's call refused without a
// reason has vault2's transfer of its amount to coldkeySs58 simulated, and
// under the runtime's transfer floor it is "failed" with "not_claimable:
// AmountTooLow: ..." (Below the claim minimum), any other refusal
// "claim_failed: <the runtime's reason>"; a claim alone that reverted is
// called again, which says why (already_claimed when another claim paid it
// first). settings are the chain settings the transactions were built with,
// coldkeySs58 the coldkey they claim for. It rejects as
// URnetworkSnLedgerClaimOutcomes does, and for settings or a coldkey
// URnetworkSnLedgerClaimTransactionsFor refuses, or a transaction of another
// vault2 or chain.
func SnLedgerClaimOutcomesFor(this js.Value, args []js.Value) any {
	var txRaw []byte
	var txErr error
	if 2 < len(args) {
		txRaw, txErr = jsSnakeJson(args[2])
	}
	receiptJson, receiptErr := jsReceiptJson(firstJsArg(args[min(3, len(args)):]))
	return jsPromise(func(resolve func(any), reject func(error)) {
		switch {
		case len(args) < 2 || args[1].Type() != js.TypeString:
			reject(errors.New("settings and coldkey are required"))
			return
		case txErr != nil || txRaw == nil:
			reject(errors.New("the transaction is required"))
			return
		case receiptErr != nil:
			reject(receiptErr)
			return
		}
		settings, err := parseSnChainSettings(args[0])
		if err != nil {
			reject(err)
			return
		}
		var tx sdk.SnLedgerUnsignedTx
		if err := json.Unmarshal(txRaw, &tx); err != nil {
			reject(err)
			return
		}
		outcomes, err := sdk.SnLedgerClaimOutcomesFor(settings, args[1].String(), &tx, receiptJson)
		if err != nil {
			reject(err)
			return
		}
		resolve(jsJson(outcomes))
	})
}

// jsReceiptJson is a wallet's receipt as json of its status and logs only, so
// the bigints of ethers and viem receipts never reach JSON.stringify.
func jsReceiptJson(receipt js.Value) (receiptJson string, err error) {
	defer func() {
		if r := recover(); r != nil {
			receiptJson, err = "", jsThrownError(r)
		}
	}()
	if receipt.Type() != js.TypeObject {
		return "", errors.New("the receipt is required")
	}
	// "0x1", 1, 1n, true or "success", as text, which the sdk reads
	status := js.Global().Get("String").Invoke(receipt.Get("status")).String()
	logs := []any{}
	if list := receipt.Get("logs"); list.Type() == js.TypeObject && list.Get("length").Type() == js.TypeNumber {
		for i := 0; i < list.Get("length").Int(); i++ {
			log := list.Index(i)
			if log.Type() != js.TypeObject {
				continue
			}
			topics := []any{}
			if t := log.Get("topics"); t.Type() == js.TypeObject && t.Get("length").Type() == js.TypeNumber {
				for j := 0; j < t.Get("length").Int(); j++ {
					topics = append(topics, jsString(t.Index(j)))
				}
			}
			logs = append(logs, map[string]any{"address": jsString(log.Get("address")), "topics": topics, "data": jsString(log.Get("data"))})
		}
	}
	raw, err := json.Marshal(map[string]any{"status": status, "logs": logs})
	return string(raw), err
}

func jsString(v js.Value) string {
	if v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// URnetworkSnCtfClientOf(answer) -> { client_id, as_of_millis, epochs, wins, unclaimed_wins, latest_win, error }
//
// A client's or a network's capture the flag wins from GET /sn/ctf/client's
// json, which the page read with its own token (an object or its json text).
// unclaimed_wins are the wins the "you won a slot" line shows, each with its
// client_id; with an error the status is unavailable, never "no wins".
// Synchronous, and no request leaves the wasm.
func SnCtfClientOf(this js.Value, args []js.Value) any {
	return jsJson(sdk.SnCtfClientOf(jsCtfAnswer(args)))
}

// URnetworkSnCtfClientOfResponse(status, answer, networkCredential) -> as URnetworkSnCtfClientOf
//
// The page's read of GET /sn/ctf/client with the HTTP status it was answered
// (a number), and whether it asked with the network's own credential, its JWT
// or an API key (networkCredential, the boolean true), or with a client token:
// 200 reads answer as URnetworkSnCtfClientOf does; a 401 to the network
// credential, a server before the network answer refusing it, is no wins and
// never a reason to sign out; a 401 when networkCredential is not true (a
// client token, or a page that does not say) is unavailable, and so is any
// other status. Synchronous.
func SnCtfClientOfResponse(this js.Value, args []js.Value) any {
	status := 0
	if 0 < len(args) && args[0].Type() == js.TypeNumber {
		status = args[0].Int()
	}
	networkCredential := 2 < len(args) && args[2].Type() == js.TypeBoolean && args[2].Bool()
	return jsJson(sdk.SnCtfClientOfResponse(status, jsCtfAnswer(args[min(1, len(args)):]), networkCredential))
}

// jsCtfAnswer is the route's json a page passes first: its text, or an object
// as json.
func jsCtfAnswer(args []js.Value) string {
	if len(args) == 0 {
		return ""
	}
	if args[0].Type() == js.TypeString {
		return args[0].String()
	}
	if raw, err := jsSnakeJson(args[0]); err == nil && raw != nil {
		return string(raw)
	}
	return ""
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
	js.Global().Set("URnetworkSnLedgerAccountsFor", js.FuncOf(SnLedgerAccountsFor))
	js.Global().Set("URnetworkSnClaimTransactionsFor", js.FuncOf(SnClaimTransactionsFor))
	js.Global().Set("URnetworkSnLedgerClaimTransactionsFor", js.FuncOf(SnLedgerClaimTransactionsFor))
	js.Global().Set("URnetworkSnLedgerClaimTransactionsForPayer", js.FuncOf(SnLedgerClaimTransactionsForPayer))
	js.Global().Set("URnetworkSnLedgerClaimOutcomes", js.FuncOf(SnLedgerClaimOutcomes))
	js.Global().Set("URnetworkSnLedgerClaimOutcomesFor", js.FuncOf(SnLedgerClaimOutcomesFor))
	js.Global().Set("URnetworkSnGasBalanceFor", js.FuncOf(SnGasBalanceFor))
	js.Global().Set("URnetworkFormatAlpha", js.FuncOf(FormatAlpha))
	// the claim relay over what the page reads (sn_claim_relay.go)
	js.Global().Set("URnetworkSnClaimStatusOf", js.FuncOf(SnClaimStatusOf))
	js.Global().Set("URnetworkSnClaimNow", js.FuncOf(SnClaimNow))
	// capture the flag over what the page reads
	js.Global().Set("URnetworkSnCtfClientOf", js.FuncOf(SnCtfClientOf))
	js.Global().Set("URnetworkSnCtfClientOfResponse", js.FuncOf(SnCtfClientOfResponse))
}
