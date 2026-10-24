package sdk

import (
	"net/url"
	"strings"
	"time"
)

// Crypto payment rules, shared by every app and by ur.io.
//
// This is the Go half of a pair: sdk/js/src/utils/crypto_pay.ts is the
// TypeScript twin, with the same names in camelCase for the functions and in
// UPPER_SNAKE_CASE for the constants, as the JS package names its constants.
// The two must stay in step; crypto_pay_test.go and js/test/crypto_pay.test.ts
// pin the same vectors.
//
// The rule they encode is the Solana Pay one (solana_pay.go): the client never
// names its own price, address or payment uri. The server quotes the invoice
// and builds its uri, and the client only checks the uri against the address
// it shows (CheckCryptoPaymentUri) before it renders or opens it.

// The coins (the `asset` of every crypto payment route). Each is also the
// payment method of its coin; PaymentMethodCard is the other.
const (
	CryptoAssetUsdcSolana = "usdc_solana"
	CryptoAssetZec        = "zec"
	CryptoAssetXmr        = "xmr"
)

// PaymentMethodCard is the fiat method: Stripe on every surface outside an
// app store. The coin methods are the CryptoAsset* values.
const PaymentMethodCard = "card"

// An invoice's state (CryptoPaymentStatusResult.Status), first match wins.
const (
	// nothing received yet
	CryptoPaymentStatusPending = "pending"
	// something arrived, short of the amount; Remaining says how much more
	CryptoPaymentStatusSeen = "seen"
	// the amount arrived, short of the confirmations it needs
	CryptoPaymentStatusConfirming = "confirming"
	// credited
	CryptoPaymentStatusPaid = "paid"
	// past the quote's expiry with nothing received; Requote gives a new one
	CryptoPaymentStatusExpired = "expired"
	// a payment needs an operator (ReviewReason)
	CryptoPaymentStatusReview = "review"
	// no such invoice, or the status token does not match
	CryptoPaymentStatusUnknown = "unknown"
)

// Where a paid invoice goes (CryptoPaymentIntentResult.Delivery).
const (
	// the signed-in network
	CryptoPaymentDeliveryAccount = "account"
	// a network named on the ur.io buy-data page
	CryptoPaymentDeliveryNetwork = "network"
	// a balance code, shown once paid and emailed when an address was given
	CryptoPaymentDeliveryCode = "code"
)

// The crypto refusals, beside the other PurchaseErrorCode* values (api.go).
const (
	// the coin is switched off, or cannot be quoted right now; the UI says
	// "Temporarily unavailable"
	PurchaseErrorCodeAssetUnavailable = "asset_unavailable"
	// the quote ran out with nothing received; the UI offers "Refresh quote"
	PurchaseErrorCodeQuoteExpired = "quote_expired"
	// the invoice ended short; the UI shows the review copy
	PurchaseErrorCodeUnderpaid = "underpaid"
)

// The coins in the order every surface shows them, after Card. The order of
// the server's list means nothing.
var cryptoAssetOrder = []string{
	CryptoAssetUsdcSolana,
	CryptoAssetZec,
	CryptoAssetXmr,
}

func isCryptoAsset(asset string) bool {
	switch asset {
	case CryptoAssetUsdcSolana, CryptoAssetZec, CryptoAssetXmr:
		return true
	default:
		return false
	}
}

// PaymentMethods is the method row for a purchase surface: PaymentMethodCard,
// then each coin that sells at least one of `items` (the plans or the packs
// the surface offers), always in the order USDC, Zcash, Monero. A coin the
// server did not list, or one this sdk does not know, is never offered.
func PaymentMethods(assets *CryptoPaymentAssetList, items *StringList) *StringList {
	methods := NewStringList()
	methods.Add(PaymentMethodCard)
	if items == nil {
		return methods
	}
	for _, asset := range cryptoAssetOrder {
		for _, item := range items.values {
			if CryptoAssetSells(assets, asset, item) {
				methods.Add(asset)
				break
			}
		}
	}
	return methods
}

// CryptoAssetSells reports whether the server's list names `asset` as selling
// `item`, a plan (PlanYearly, PlanYearlyOnboarding) or a data pack
// (StripeItemData1Tib, StripeItemData10Tib). Monthly is card only, whatever
// the list says.
func CryptoAssetSells(assets *CryptoPaymentAssetList, asset string, item string) bool {
	if assets == nil || !isCryptoAsset(asset) || !PaymentMethodAllowsPlan(asset, item) {
		return false
	}
	for _, listed := range assets.values {
		if listed == nil || listed.Asset != asset {
			continue
		}
		if (listed.Plans != nil && listed.Plans.Contains(item)) ||
			(listed.Items != nil && listed.Items.Contains(item)) {
			return true
		}
	}
	return false
}

// PaymentMethodAllowsPlan is false only for a coin with PlanMonthly: no coin
// sells monthly, so picking a coin greys the monthly plan out ("Monthly is
// card only"). One network fee would be a large part of a regional monthly
// price.
func PaymentMethodAllowsPlan(method string, plan string) bool {
	return !(isCryptoAsset(method) && plan == PlanMonthly)
}

// CryptoAssetTicker is the ticker shown beside an amount ("0.072387 XMR"), ""
// for anything else.
func CryptoAssetTicker(asset string) string {
	switch asset {
	case CryptoAssetUsdcSolana:
		return "USDC"
	case CryptoAssetZec:
		return "ZEC"
	case CryptoAssetXmr:
		return "XMR"
	default:
		return ""
	}
}

// CryptoWalletSuggestions are the wallets the invoice names as working for a
// coin ("Wallets that work: ..."), in the order to show them. Every Zcash one
// sends to a shielded address. Empty for anything else.
func CryptoWalletSuggestions(asset string) *StringList {
	wallets := NewStringList()
	switch asset {
	case CryptoAssetXmr:
		wallets.addAll("Cake", "Feather", "Monerujo", "Stack", "Unstoppable")
	case CryptoAssetZec:
		wallets.addAll("Zodl", "Cake", "Zingo", "Zkool", "Vizor")
	case CryptoAssetUsdcSolana:
		wallets.addAll("Phantom", "Solflare")
	}
	return wallets
}

// the uri scheme of each coin's payment uri
func cryptoPaymentUriScheme(asset string) string {
	switch asset {
	case CryptoAssetUsdcSolana:
		return "solana"
	case CryptoAssetZec:
		return "zcash"
	case CryptoAssetXmr:
		return "monero"
	default:
		return ""
	}
}

// CheckCryptoPaymentUri reports whether `uri` is a payment uri of `asset`'s
// coin that pays exactly `address`, the address the invoice shows. An app
// refuses to render or open a uri that fails it: the QR code and the "Open in
// wallet" button must never pay somewhere other than the address on screen.
//
// The uri is the server's `<scheme>:<address>?<query>` (Solana Pay, ZIP 321 or
// the Monero uri scheme). The scheme must be the coin's, compared without
// case as RFC 3986 says; the address between the colon and the query must be
// `address` byte for byte; and the uri must be plain printable ASCII with no
// fragment, as the server's percent-encoded uris are. A Zcash uri must not
// name a second recipient: ZIP 321 lets one uri pay several addresses
// (`address.1=...`), and a wallet would pay them all. A Monero uri that names
// several recipients puts them in the path, which the exact address match
// already refuses.
func CheckCryptoPaymentUri(asset string, uri string, address string) bool {
	scheme := cryptoPaymentUriScheme(asset)
	if scheme == "" || address == "" || uri == "" {
		return false
	}
	for i := 0; i < len(uri); i += 1 {
		if c := uri[i]; c <= ' ' || '~' < c || c == '#' {
			return false
		}
	}
	uriScheme, rest, ok := strings.Cut(uri, ":")
	if !ok || !strings.EqualFold(uriScheme, scheme) {
		return false
	}
	uriAddress, query, _ := strings.Cut(rest, "?")
	if uriAddress != address {
		return false
	}
	if asset == CryptoAssetZec && query != "" {
		for _, param := range strings.Split(query, "&") {
			name, _, _ := strings.Cut(param, "=")
			name, err := url.PathUnescape(name)
			if err != nil {
				return false
			}
			name = strings.ToLower(name)
			if name == "address" || strings.HasPrefix(name, "address.") {
				return false
			}
		}
	}
	return true
}

// CryptoTermNoticeDue reports whether an app shows the renewal notice for a
// one-time crypto credit now: from the end of its paid term until its grace
// ends, and never before (the credit cannot renew by itself, and a notice
// during the paid term would only nag). False for nil or for times that do
// not parse.
func CryptoTermNoticeDue(term *CryptoTerm) bool {
	return cryptoTermNoticeDueAt(term, time.Now())
}

func cryptoTermNoticeDueAt(term *CryptoTerm, now time.Time) bool {
	if term == nil {
		return false
	}
	termEnd, err := time.Parse(time.RFC3339, term.TermEndTime)
	if err != nil {
		return false
	}
	graceEnd, err := time.Parse(time.RFC3339, term.GraceEndTime)
	if err != nil {
		return false
	}
	return !now.Before(termEnd) && now.Before(graceEnd)
}
