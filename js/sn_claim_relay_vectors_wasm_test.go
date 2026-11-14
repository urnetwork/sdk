//go:build js

package main

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"syscall/js"
	"testing"
	"unicode"

	"github.com/urnetwork/sdk"
)

// The export is the sdk's SnClaimStatusOf, whatever the page's casing: over
// generated relay answers, ledger reads, wallets and settings, given to the wasm
// as a page's js values in snake_case, camelCase or Go's names, it answers
// exactly what the sdk answers for the same reads in Go. The rules themselves are
// the sdk's own tests' (sn_claim_relay_test.go); this holds the page's side of the
// boundary to them.
func TestSnClaimStatusOfAnswersTheSdkOverGeneratedInputs(t *testing.T) {
	const explorer = "https://explorer.invalid/tx/%s"
	coldkeys := []string{"a", "b", "c"}
	claimables := []int64{0, 0, 5, 23_999_999, 24_000_000, 1_000_000_000}
	pendings := []int64{0, 0, 5}
	reasons := []string{"", sdk.SnClaimReasonNothingPending, sdk.SnClaimReasonUnderThreshold, sdk.SnClaimReasonQueued, sdk.SnClaimReasonCapReached,
		sdk.SnClaimReasonRefused, sdk.SnClaimReasonRelayNotRunning, sdk.SnClaimReasonRelayPaused, "unknown"}
	nowReasons := []string{"", sdk.SnClaimNowReasonUnderFloor, sdk.SnClaimNowReasonRequested, sdk.SnClaimNowReasonLimit, sdk.SnClaimReasonRelayNotRunning}
	hashes := []string{"", "", testClaimHash, "0x1234", testClaimHash + "00", "0X" + testClaimHash[2:]}
	amounts := []int64{-1, 0, 9, 842_000_000}
	connected := []string{"", "", "a", "b", "z", " a "}
	floors := []int64{0, 24_000_000}

	// a fixed seed, so a failing input replays
	random := rand.New(rand.NewPCG(25, 0))
	pick := func(n int) int { return random.IntN(n) }
	// what the inputs reached, so the comparison is not of nothing
	seen := map[string]int{}
	defer func() {
		for _, outcome := range []string{"error", "no error", "fallback", "tile", "claimed", "linked", "self claim configured",
			"action " + sdk.SnClaimActionNone, "action " + sdk.SnClaimActionWait, "action " + sdk.SnClaimActionClaimNow, "action " + sdk.SnClaimActionSelfClaim} {
			if seen[outcome] < 10 {
				t.Errorf("CONTROL FAILED: %d inputs reached %q", seen[outcome], outcome)
			}
		}
	}()
	for i := 0; i < 1500; i++ {
		var relayJson []byte
		relayErr := ""
		switch pick(8) {
		case 0:
			// the request failed
			relayErr = []string{"", "Failed to fetch"}[pick(2)]
		default:
			list := sdk.NewSnClaimRelayAccountList()
			for j, n := 0, pick(4); j < n; j++ {
				list.Add(&sdk.SnClaimRelayAccount{
					ColdkeySs58:          coldkeys[pick(len(coldkeys))],
					NoId:                 int64(pick(3)),
					Index:                int64(pick(5)),
					ClaimableRao:         claimables[pick(len(claimables))],
					ValueTaoRao:          int64(pick(2)) * 4_224_000,
					PendingRao:           pendings[pick(len(pendings))],
					Reason:               reasons[pick(len(reasons))],
					ClaimNowAvailable:    pick(2) == 0,
					ClaimNowReason:       nowReasons[pick(len(nowReasons))],
					ClaimNowNextAtMillis: int64(pick(2)) * 1_800_000_000_000,
					ClaimTxHash:          hashes[pick(len(hashes))],
					ClaimAmountRao:       amounts[pick(len(amounts))],
				})
			}
			relay := &sdk.SnClaimRelayResult{
				State:                    []string{"running", "cap_reached", "not_running", "no_key"}[pick(4)],
				AutomaticThresholdTaoRao: 12_500_000,
				TransferFloorTaoRao:      100_000,
				AutomaticThresholdRao:    int64(pick(2)) * 3_000_000_000,
				TransferFloorRao:         floors[pick(len(floors))],
				ClaimNowPeriodDays:       30,
				Accounts:                 list,
			}
			if pick(6) == 0 {
				relay.Error = &sdk.SnClaimRelayError{Message: "The balances could not be read."}
			}
			relayJson = mustJson(t, relay)
		}

		var ledgerJson []byte
		switch pick(4) {
		case 0:
			// not read
		case 1:
			ledgerJson = mustJson(t, &sdk.SnLedgerAccountsResult{Error: &sdk.SnError{Code: []string{"", sdk.SnErrorCodeChainRpcUnreachable}[pick(2)], Message: "down"}})
		default:
			accounts := sdk.NewSnLedgerAccountList()
			for j, n := 0, pick(3); j < n; j++ {
				finalized := int64(pick(3)) * 1_000_000_000
				accounts.Add(&sdk.SnLedgerAccount{Vault: "0x2222222222222222222222222222222222abcdef", NoId: int64(j + 1), Index: int64(pick(9)),
					FinalizedRao: finalized, ClaimableRao: claimables[pick(len(claimables))], PaidRao: 5, Pending: pick(2) == 0, PendingRao: finalized + int64(pick(2))*7})
			}
			ledgerJson = mustJson(t, &sdk.SnLedgerAccountsResult{Accounts: accounts})
		}

		wallet := connected[pick(len(connected))]
		configured := pick(2) == 0
		linked := pick(2) == 0

		// what the sdk answers in Go, from its own decoding of the same reads
		var relay *sdk.SnClaimRelayResult
		if relayJson != nil {
			relay = &sdk.SnClaimRelayResult{}
			mustDecode(t, relayJson, relay)
		}
		var goErr error
		if relayErr != "" {
			goErr = errors.New(relayErr)
		}
		var ledger *sdk.SnLedgerAccountsResult
		if ledgerJson != nil {
			ledger = &sdk.SnLedgerAccountsResult{}
			mustDecode(t, ledgerJson, ledger)
		}
		var settings *sdk.SnChainSettings
		if linked {
			settings, _ = parseSnChainSettings(js.ValueOf(`{"explorerTxUrl":"` + explorer + `"}`))
		}
		want := map[string]any{}
		mustDecode(t, mustJson(t, sdk.SnClaimStatusOf(relay, goErr, wallet, configured, ledger, settings)), &want)

		// and what the page gets, in its casing
		input := map[string]any{"connected_ss58": wallet, "self_claim_configured": configured}
		if relayJson != nil {
			input["relay"] = json.RawMessage(relayJson)
		} else {
			input["relay"] = nil
			if relayErr != "" {
				input["relay_error"] = relayErr
			}
		}
		if ledgerJson != nil {
			input["ledger"] = json.RawMessage(ledgerJson)
		}
		if linked {
			input["settings"] = map[string]any{"explorer_tx_url": explorer}
		}
		casing := []string{"snake", "camel", "pascal"}[i%3]
		var decoded any
		mustDecode(t, mustJson(t, input), &decoded)
		got := claimStatusOf(t, string(mustJson(t, recase(decoded, casing))))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("input %d (%s): %s\ngot  %v\nwant %v", i, casing, mustJson(t, recase(decoded, casing)), got, want)
		}
		if got["error"] != nil {
			seen["error"]++
		} else {
			seen["no error"]++
			if got["relay_state"] == sdk.SnClaimRelayStateUnavailable || relay != nil && relay.Error != nil {
				seen["fallback"]++
			}
		}
		if got["shows_tile"] == true {
			seen["tile"]++
		}
		if got["shows_claimed"] == true {
			seen["claimed"]++
		}
		if url, _ := got["claim_tx_url"].(string); url != "" {
			seen["linked"]++
		}
		if got["self_claim_available"] == true {
			seen["self claim configured"]++
		}
		seen["action "+got["action"].(string)]++
	}
}

func mustJson(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustDecode(t *testing.T, raw []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
}

// recase writes snake_case keys as camelCase or Go's PascalCase.
func recase(value any, casing string) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			out[recaseKey(key, casing)] = recase(item, casing)
		}
		return out
	case []any:
		out := []any{}
		for _, item := range v {
			out = append(out, recase(item, casing))
		}
		return out
	}
	return value
}

func recaseKey(key string, casing string) string {
	if casing == "snake" {
		return key
	}
	parts := strings.Split(key, "_")
	for i, part := range parts {
		if part == "" || (i == 0 && casing == "camel") {
			continue
		}
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	return strings.Join(parts, "")
}
