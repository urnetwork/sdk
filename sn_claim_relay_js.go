//go:build js

package sdk

// sn_claim_relay_js.go: the claim relay for a page with no device, the web's wasm
// (js/sn.go URnetworkSnClaimStatusOf and URnetworkSnClaimNow). The page reads GET
// /sn/claim-relay and the connected wallet's ledger accounts, and sends Claim now,
// with its own client; the sdk decides what they mean, with the rules a device
// follows (sn_claim_relay.go), so ur.io keeps no copy of them.

import (
	"strings"
)

// SnClaimStatusOf is SnClaimStatus over what a page read. relay is GET
// /sn/claim-relay's answer, nil when the page could not read it, with relayErr
// saying why. ledger is the connected wallet's v2 accounts, which stand in for an
// answer without balances; nil when the page could not read them.
// selfClaimConfigured says whether the page's own v2 claim can be sent. That claim
// pays its gas from the reader's wallet, which the sdk cannot read, so it is never
// short of gas, as on a device whose gas key cannot be read. settings links each
// claim's transaction; nil links none.
func SnClaimStatusOf(
	relay *SnClaimRelayResult,
	relayErr error,
	connectedSs58 string,
	selfClaimConfigured bool,
	ledger *SnLedgerAccountsResult,
	settings *SnChainSettings,
) *SnClaimStatusResult {
	connectedSs58 = strings.TrimSpace(connectedSs58)
	return snClaimStatusOf(
		relay,
		relayErr,
		connectedSs58,
		selfClaimConfigured,
		func() (*SnClaimRelayAccountList, *SnError) {
			return snClaimLedgerRelayAccounts(ledger, connectedSs58)
		},
		nil,
		settings,
	)
}

// SnClaimNowOf is SnClaimNow for a page: post sends POST
// /sn/claim-relay/claim-now with the page's own client. connectedSs58 is the
// connected wallet's coldkey, "" without one.
func SnClaimNowOf(
	coldkeySs58 string,
	connectedSs58 string,
	post func(*SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error),
) *SnClaimNowResult {
	connectedSs58 = strings.TrimSpace(connectedSs58)
	return snClaimNowOf(
		coldkeySs58,
		func() (string, bool) {
			return connectedSs58, connectedSs58 != ""
		},
		post,
	)
}
