//go:build js

package main

// The claim relay for the site, which has no device (sdk sn_claim_relay_js.go):
// the sdk's SnClaimStatus and SnClaimNow over what the page reads and sends with
// its own client. The page passes the server's json and its own reads as they
// are, in any key casing (the server's snake_case, Go's names or camelCase); the
// sdk decides what they mean and the page draws the answer, so ur.io keeps no
// copy of the rules.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall/js"
	"unicode"

	"github.com/urnetwork/sdk/v2026"
)

// URnetworkSnClaimStatusOf({ relay, relayError, ledger, connectedSs58, selfClaimConfigured, settings }) -> status
//
//   - relay: GET /sn/claim-relay?partial=true's json as the server sends it,
//     whose unavailable_pools are drawn as unavailable; null when the request
//     failed, with relayError saying why
//   - ledger: the connected wallet's v2 accounts, URnetworkSnLedgerAccountsFor's
//     result or a page's normalized form of it ({ accounts, error }), which stand
//     in for a relay answer without balances; null when not read
//   - connectedSs58: the wallet the page's own v2 claim claims, "" without one
//   - selfClaimConfigured: the page can send that claim
//   - settings: chain settings, as the other exports take them, for the
//     explorer's links; null links none
//
// Answers the sdk's SnClaimStatusResult with its json names (relay_state, action,
// reason, claimable_rao, ..., shows_tile, shows_claimed, accounts, error).
// Synchronous, and no request leaves the wasm.
func SnClaimStatusOf(this js.Value, args []js.Value) any {
	input := map[string]json.RawMessage{}
	if 0 < len(args) {
		if raw, err := jsSnakeJson(args[0]); err == nil && raw != nil {
			// anything but an object reads as no input
			_ = json.Unmarshal(raw, &input)
		}
	}

	var relay *sdk.SnClaimRelayResult
	var relayErr error
	if raw := input["relay"]; !rawJsonNull(raw) {
		var answer sdk.SnClaimRelayResult
		if err := json.Unmarshal(raw, &answer); err != nil {
			relayErr = fmt.Errorf("the claim relay's answer could not be read: %w", err)
		} else {
			relay = &answer
		}
	} else if message := rawJsonString(input["relay_error"]); message != "" {
		relayErr = errors.New(message)
	}

	var ledger *sdk.SnLedgerAccountsResult
	if raw := input["ledger"]; !rawJsonNull(raw) {
		var read sdk.SnLedgerAccountsResult
		// a read the sdk cannot read is none: the ledger is unavailable
		if err := json.Unmarshal(raw, &read); err == nil {
			ledger = &read
		}
	}

	var settings *sdk.SnChainSettings
	if raw := input["settings"]; !rawJsonNull(raw) {
		if parsed, err := parseSnChainSettings(js.ValueOf(string(raw))); err == nil {
			settings = parsed
		}
	}

	return jsJson(sdk.SnClaimStatusOf(
		relay,
		relayErr,
		rawJsonString(input["connected_ss58"]),
		rawJsonBool(input["self_claim_configured"]),
		ledger,
		settings,
	))
}

// URnetworkSnClaimNow(coldkeySs58, connectedSs58, post) -> Promise<{ requested, reason, next_at_millis, error }>
//
// The sdk's SnClaimNow for a page. coldkeySs58 is the account to claim, "" for the
// connected wallet's. post(coldkeySs58) sends POST /sn/claim-relay/claim-now with
// the page's own client and answers the server's json, or a Promise of it; it is
// not called for an address that is not one. A failed tap resolves with the sdk's
// error codes, as SnClaimNowResult; only a call without post rejects.
func SnClaimNow(this js.Value, args []js.Value) any {
	return jsPromise(func(resolve func(any), reject func(error)) {
		if len(args) < 3 || args[2].Type() != js.TypeFunction {
			reject(errors.New("coldkey, connected wallet and post are required"))
			return
		}
		post := args[2]
		result := sdk.SnClaimNowOf(stringArg(args, 0), stringArg(args, 1), func(claimNow *sdk.SnClaimRelayClaimNowArgs) (*sdk.SnClaimRelayClaimNowResult, error) {
			answer, err := jsInvokeAwait(post, claimNow.ColdkeySs58)
			if err != nil {
				return nil, err
			}
			raw, err := jsSnakeJson(answer)
			if err != nil {
				return nil, err
			}
			if raw == nil {
				// no answer: the sdk says the relay did not answer
				return nil, nil
			}
			var out sdk.SnClaimRelayClaimNowResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("the claim relay's answer could not be read: %w", err)
			}
			return &out, nil
		})
		resolve(jsJson(result))
	})
}

// jsInvokeAwait calls a page's function and waits for its answer, which may be a
// Promise. A throw or a rejection is an error with the page's message. Call it off
// the event loop's goroutine, as jsPromise's work runs.
func jsInvokeAwait(fn js.Value, args ...any) (value js.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			value, err = js.Undefined(), jsThrownError(r)
		}
	}()
	out := fn.Invoke(args...)
	if out.Type() != js.TypeObject || out.Get("then").Type() != js.TypeFunction {
		return out, nil
	}
	type settled struct {
		value js.Value
		err   error
	}
	done := make(chan settled, 1)
	onResolve := js.FuncOf(func(_ js.Value, a []js.Value) any {
		done <- settled{value: firstJsArg(a)}
		return nil
	})
	onReject := js.FuncOf(func(_ js.Value, a []js.Value) any {
		done <- settled{value: js.Undefined(), err: errors.New(jsMessageOf(firstJsArg(a)))}
		return nil
	})
	defer onResolve.Release()
	defer onReject.Release()
	out.Call("then", onResolve, onReject)
	s := <-done
	return s.value, s.err
}

func firstJsArg(args []js.Value) js.Value {
	if 0 < len(args) {
		return args[0]
	}
	return js.Undefined()
}

func jsThrownError(r any) error {
	if thrown, ok := r.(js.Error); ok {
		return errors.New(jsMessageOf(thrown.Value))
	}
	return fmt.Errorf("%v", r)
}

// jsMessageOf is what a thrown or rejected value says: an Error's message, or the
// value as text.
func jsMessageOf(v js.Value) string {
	message := ""
	switch {
	case v.Type() == js.TypeObject && v.Get("message").Type() == js.TypeString:
		message = v.Get("message").String()
	case v.Type() == js.TypeString:
		message = v.String()
	case !v.IsUndefined() && !v.IsNull():
		message = js.Global().Get("String").Invoke(v).String()
	}
	if message == "" {
		message = "the page's client failed"
	}
	return message
}

// jsSnakeJson is a page's value as json with every key in the sdk's json case,
// snake_case, so the server's json, Go's names and camelCase read alike. nil for
// null and undefined.
func jsSnakeJson(v js.Value) (raw []byte, err error) {
	if v.IsNull() || v.IsUndefined() {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, jsThrownError(r)
		}
	}()
	text := js.Global().Get("JSON").Call("stringify", v)
	if text.Type() != js.TypeString {
		// a function or a symbol has no json
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(text.String()))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(snakeCaseKeys(value))
}

func snakeCaseKeys(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[snakeCaseKey(key)] = snakeCaseKeys(item)
		}
		return out
	case []any:
		for i := range v {
			v[i] = snakeCaseKeys(v[i])
		}
		return v
	}
	return value
}

// "ColdkeySs58" and "coldkeySs58" are "coldkey_ss58"; "no_id" stays.
func snakeCaseKey(key string) string {
	var out strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if 0 < i && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
				out.WriteByte('_')
			}
			out.WriteRune(unicode.ToLower(r))
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func rawJsonNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func rawJsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func rawJsonBool(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b
}
