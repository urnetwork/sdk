package sdk

import (
	"fmt"

	"github.com/urnetwork/connect/v2026"
)

// Crypto invoices: Pro and data packs paid in USDC on Solana, Zcash or Monero
// (connect/api/bringyour.yml, /pay/crypto/*). The server builds every quote,
// address and payment uri; a client shows them and polls the invoice's status.
// The pure rules (which methods to offer, uri checks, tickers) are in
// crypto_pay.go, and the invoice state machine every app renders is
// CryptoInvoiceViewController.
//
// Atomic amounts are decimal strings, never numbers: a piconero amount does not
// fit a float, and a rounded amount is a payment the server cannot match. Times
// are RFC 3339 strings, as the server sends them.
//
// The routes that need no sign-in (the asset list, the buy-data invoice, the
// status and the requote) are sent without any credential. The status token is
// the only credential an invoice needs, and a code bought without signing in
// must not arrive carrying the device's network.

// An asset the server takes payment in now, and what it sells. A client never
// offers an asset or an item the list does not name (CryptoAssetSells).
type CryptoPaymentAsset struct {
	// one of the CryptoAsset* values
	Asset string `json:"asset"`
	// the plans the asset sells (PlanYearly, PlanYearlyOnboarding); never
	// PlanMonthly, which is card only
	Plans *StringList `json:"plans,omitempty"`
	// the data packs the asset sells (StripeItemData1Tib, StripeItemData10Tib)
	Items *StringList `json:"items,omitempty"`
	// the confirmations a payment needs before it is credited
	ConfirmationsRequired int `json:"confirmations_required"`
	// how long a quote holds
	QuoteTtlSeconds int `json:"quote_ttl_seconds"`
}

type CryptoPaymentAssetList struct {
	exportedList[*CryptoPaymentAsset]
}

func NewCryptoPaymentAssetList() *CryptoPaymentAssetList {
	return &CryptoPaymentAssetList{
		exportedList: *newExportedList[*CryptoPaymentAsset](),
	}
}

type CryptoPaymentAssetsResult struct {
	// the enabled assets, in no particular order (PaymentMethods orders them)
	Assets *CryptoPaymentAssetList `json:"assets"`
}

// The network's latest one-time crypto credit whose grace has not ended
// (SubscriptionBalanceResult.CryptoTerm). A one-time credit never renews, so
// an app shows its renewal notice while TermEndTime <= now < GraceEndTime, and
// never before (CryptoTermNoticeDue).
type CryptoTerm struct {
	// the subscription market: "solana", "monero" or "zcash"
	Market string `json:"market"`
	// when the paid term ends and the grace begins, RFC 3339
	TermEndTime string `json:"term_end_time"`
	// when the grace ends, and Pro with it, RFC 3339
	GraceEndTime string `json:"grace_end_time"`
}

// Starts an invoice for the signed-in network (delivery "account"). Exactly
// one of Plan and ItemId. The price is never a field here: the server quotes
// it from the plan or the pack and the caller's regional tier.
type CreateCryptoPaymentIntentArgs struct {
	// one of the CryptoAsset* values
	Asset string `json:"asset"`
	// PlanYearly, or PlanYearlyOnboarding for the welcome offer. PlanMonthly
	// is card only, and the server refuses it.
	Plan string `json:"plan,omitempty"`
	// StripeItemData1Tib or StripeItemData10Tib
	ItemId string `json:"item_id,omitempty"`
	// the store's storefront country, when the app knows it; else the server
	// resolves the regional tier
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

// Starts an invoice for a data pack without a sign-in (the ur.io buy-data
// page). With NetworkName the data goes to that network (delivery "network");
// without it the payment buys a balance code (delivery "code"), shown once
// paid and emailed too when Email is set.
type CreateDataCryptoPaymentIntentArgs struct {
	Asset       string `json:"asset"`
	ItemId      string `json:"item_id"`
	NetworkName string `json:"network_name"`
	Email       string `json:"email"`
}

// Names an invoice for its status and requote. StatusToken is the invoice's
// bearer secret: it travels in this POST body, never in a url query or a log.
type CryptoPaymentStatusArgs struct {
	IntentId    string `json:"intent_id"`
	StatusToken string `json:"status_token"`
}

type CryptoPaymentError struct {
	// one of the `PurchaseErrorCode*` values, when the refusal has one
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// An invoice's state (POST /pay/crypto/status). Amounts are exact decimal
// strings in the coin, each beside its atomic twin.
type CryptoPaymentStatusResult struct {
	IntentId string `json:"intent_id,omitempty"`
	Asset    string `json:"asset,omitempty"`
	Plan     string `json:"plan,omitempty"`
	ItemId   string `json:"item_id,omitempty"`
	// one of the CryptoPaymentDelivery* values
	Delivery    string `json:"delivery,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	// one of the CryptoPaymentStatus* values. For CryptoPaymentStatusUnknown
	// (no such invoice, or a wrong token) nothing else is filled in.
	Status string `json:"status"`
	// why the invoice is in review: "underpaid" (short at expiry),
	// "late_underpaid", "orphaned" (a credited payment left the chain) or
	// "voided" (a payment to an invoice an operator closed)
	ReviewReason    string `json:"review_reason,omitempty"`
	Amount          string `json:"amount,omitempty"`
	AmountAtomic    string `json:"amount_atomic,omitempty"`
	Received        string `json:"received,omitempty"`
	ReceivedAtomic  string `json:"received_atomic,omitempty"`
	Remaining       string `json:"remaining,omitempty"`
	RemainingAtomic string `json:"remaining_atomic,omitempty"`
	// the lowest among the payments that make up the amount; 0 in the mempool
	Confirmations         int    `json:"confirmations"`
	ConfirmationsRequired int    `json:"confirmations_required"`
	ExpiresAt             string `json:"expires_at,omitempty"`
	CreditedAt            string `json:"credited_at,omitempty"`
	// the code a paid invoice bought, for delivery "code"
	BalanceCode string `json:"balance_code,omitempty"`
	// PurchaseErrorCodeQuoteExpired while expired, PurchaseErrorCodeUnderpaid
	// while in review for a short payment
	Error *CryptoPaymentError `json:"error,omitempty"`
}

// An invoice (POST /pay/crypto/intent, /pay/data/crypto-intent and
// /pay/crypto/requote), or a refusal in Error.
type CryptoPaymentIntentResult struct {
	IntentId string `json:"intent_id,omitempty"`
	// the invoice's bearer secret for its status and requote: keep it in
	// memory or device storage, never in a url query or a log. An open
	// invoice returned again comes with a new token; every token handed out
	// stays valid.
	StatusToken string `json:"status_token,omitempty"`
	Asset       string `json:"asset,omitempty"`
	Plan        string `json:"plan,omitempty"`
	ItemId      string `json:"item_id,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	// the regional tier the price came from, and whether the welcome offer
	// was applied
	Tier            string  `json:"tier,omitempty"`
	OfferApplied    bool    `json:"offer_applied,omitempty"`
	PriceUsd        float64 `json:"price_usd,omitempty"`
	RegularPriceUsd float64 `json:"regular_price_usd,omitempty"`
	// the coin's USD rate the amount was quoted at (1 for USDC)
	RateUsd float64 `json:"rate_usd,omitempty"`
	// the invoice's own receive address
	Address string `json:"address,omitempty"`
	// the exact amount to send. Zcash and Monero amounts are rounded up to 6
	// decimals; a USDC amount carries its unique 6-decimal suffix, so show and
	// pay every decimal.
	Amount       string `json:"amount,omitempty"`
	AmountAtomic string `json:"amount_atomic,omitempty"`
	// usdc_solana only: the reference, for a transfer sent by hand
	Memo string `json:"memo,omitempty"`
	// the payment uri the server built. Show it as a QR code and open it in a
	// wallet only when CheckCryptoPaymentUri passes; never build one.
	Uri                   string                     `json:"uri,omitempty"`
	QuotedAt              string                     `json:"quoted_at,omitempty"`
	ExpiresAt             string                     `json:"expires_at,omitempty"`
	ConfirmationsRequired int                        `json:"confirmations_required,omitempty"`
	Status                *CryptoPaymentStatusResult `json:"status,omitempty"`
	Error                 *CryptoPaymentError        `json:"error,omitempty"`
}

type CryptoPaymentAssetsCallback connect.ApiCallback[*CryptoPaymentAssetsResult]

type CryptoPaymentIntentCallback connect.ApiCallback[*CryptoPaymentIntentResult]

type CryptoPaymentStatusCallback connect.ApiCallback[*CryptoPaymentStatusResult]

// Reads the enabled assets without a sign-in. The apps read the same list from
// SubscriptionBalanceResult.CryptoAssets.
func (self *Api) GetCryptoPaymentAssets(callback CryptoPaymentAssetsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/pay/crypto/assets", self.apiUrl),
			"",
			&CryptoPaymentAssetsResult{},
			callback,
		)
	})
}

// Starts an invoice for the signed-in network, or returns the network's open
// invoice for the same asset and item.
func (self *Api) CreateCryptoPaymentIntent(args *CreateCryptoPaymentIntentArgs, callback CryptoPaymentIntentCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/pay/crypto/intent", self.apiUrl),
			args,
			self.GetByJwt(),
			&CryptoPaymentIntentResult{},
			callback,
		)
	})
}

// Starts a data-pack invoice without a sign-in, for a named network or a
// balance code.
func (self *Api) CreateDataCryptoPaymentIntent(args *CreateDataCryptoPaymentIntentArgs, callback CryptoPaymentIntentCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/pay/data/crypto-intent", self.apiUrl),
			args,
			"",
			&CryptoPaymentIntentResult{},
			callback,
		)
	})
}

// Reads an invoice's state with its status token.
func (self *Api) GetCryptoPaymentStatus(args *CryptoPaymentStatusArgs, callback CryptoPaymentStatusCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/pay/crypto/status", self.apiUrl),
			args,
			"",
			&CryptoPaymentStatusResult{},
			callback,
		)
	})
}

// Quotes an unpaid invoice again (pending or expired): a new amount and expiry
// for the same address, or a later expiry for USDC.
func (self *Api) RequoteCryptoPayment(args *CryptoPaymentStatusArgs, callback CryptoPaymentIntentCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/pay/crypto/requote", self.apiUrl),
			args,
			"",
			&CryptoPaymentIntentResult{},
			callback,
		)
	})
}
