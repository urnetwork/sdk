package sdk

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Payment catalog, checkout bridge envelope, and balance-code client rules.
//
// Every constant and helper in this file was previously duplicated (with
// drift) across the platform apps. The platform hardcode sites named in the
// doc comments below are the migration targets: each should be replaced by a
// reference to the SDK value.

// ---- product / plan catalog --------------------------------------------------

// SubscriptionPlanSupporter is the wire value of Subscription.Plan (server
// subscription_type) for the Pro subscription. The store product id is ALSO
// "supporter" on the mobile stores.
//
// Current hardcode sites:
//   - apple: SubscriptionBalanceViewModel.swift ("case supporter = \"supporter\"")
//   - android: google/MainActivity.kt + google/PlanViewModel.kt
//     (.setProductId("supporter"), productDetails.productId == "supporter")
//   - web: mmm/ur.io react/src/auth/AuthContext.jsx
//     (plan.toLowerCase() === "supporter")
//   - server: model/subscription_model.go SubscriptionTypeSupporter
const SubscriptionPlanSupporter = "supporter"

// Stripe checkout item ids, accepted by CreateStripeCheckoutSession
// (StripeCreateCheckoutSessionArgs.ItemId). They mirror the server's
// controller/subscription_stripe_controller.go StripeItem* constants.
//
// Current hardcode sites:
//   - windows: app/src/App/BalanceSheets.cpp ("pro_yearly" : "pro_monthly")
//   - linux: app/src/UpgradeSheet.cpp (kItemProMonthly/kItemProYearly)
//   - web: mmm/ur.io react/src/components/AccountPanel.jsx,
//     FreeTrialPanel.jsx (pro items); components/BuyDataPacks.jsx and
//     pages/BuyData.jsx (data packs)
const (
	StripeItemProMonthly = "pro_monthly"
	StripeItemProYearly  = "pro_yearly"
	StripeItemData1Tib   = "data_1tib"
	StripeItemData10Tib  = "data_10tib"
)

// Stripe checkout ui modes, accepted by CreateStripeCheckoutSession
// (StripeCreateCheckoutSessionArgs.UiMode). These are Stripe's own ui_mode
// values and are mutually exclusive on a single session: hosted returns
// checkout_url only, embedded returns client_secret + publishable_key only.
//
// Current hardcode sites:
//   - windows: app/src/App/CheckoutSessionMode.h (with redirect_on_completion)
//   - linux: app/src/CheckoutSessionMode.hpp (with redirect_on_completion)
//   - web: mmm/ur.io react/src/auth/api.js (ui_mode: "embedded")
const (
	StripeUiModeHosted   = "hosted"
	StripeUiModeEmbedded = "embedded"
)

// StripeRedirectOnCompletionNever keeps an EMBEDDED checkout fully inline:
// Stripe fires the client's onComplete callback instead of redirecting, so the
// page the customer is on never navigates. Only valid with ui_mode "embedded"
// (StripeCreateCheckoutSessionArgs.RedirectOnCompletion). The desktop apps
// pair it with BuildInlineCheckoutBridgeUrl, which tells the bridge page to
// hand control back from that callback.
const StripeRedirectOnCompletionNever = "never"

// ---- store classification ----------------------------------------------------

// Normalized store families for Subscription.Store. The raw store value is a
// server-side store id; each family maps to the place the subscription must be
// cancelled (Apple subscriptions page, Play subscriptions page, the Stripe
// billing portal).
const (
	SubscriptionStoreApple  = "apple"
	SubscriptionStoreGoogle = "google"
	SubscriptionStoreStripe = "stripe"
	// SubscriptionStoreOther is any non-empty store id that matches no known
	// family (e.g. a crypto rail). There is no per-store manage link for it.
	SubscriptionStoreOther = "other"
)

// ported from mmm/ur.io react/src/app/screens/Subscription.jsx normStore
var (
	subscriptionStoreStripeRe = regexp.MustCompile(`stripe`)
	subscriptionStoreAppleRe  = regexp.MustCompile(`apple|itunes|ios|app.?store`)
	subscriptionStoreGoogleRe = regexp.MustCompile(`google|play|android`)
)

// ClassifySubscriptionStore maps a raw Subscription.Store id to one of the
// SubscriptionStore* families, or "" for an empty store. Matching is
// case-insensitive substring matching, ported verbatim from the web app's
// normStore (mmm/ur.io react/src/app/screens/Subscription.jsx).
func ClassifySubscriptionStore(store string) string {
	s := strings.ToLower(store)
	switch {
	case s == "":
		return ""
	case subscriptionStoreStripeRe.MatchString(s):
		return SubscriptionStoreStripe
	case subscriptionStoreAppleRe.MatchString(s):
		return SubscriptionStoreApple
	case subscriptionStoreGoogleRe.MatchString(s):
		return SubscriptionStoreGoogle
	default:
		return SubscriptionStoreOther
	}
}

// ---- checkout bridge envelope (desktop) --------------------------------------

// The ur.io bridge page (mmm/ur.io react EmbeddedCheckout.jsx): mounts
// Stripe's Embedded Checkout for a session's client_secret -- the card form
// stays in Stripe's iframe, no card data ever touches the app -- and hands
// control back by navigating to the redirect_link:
//
//	done:  urnetwork://checkout?status=complete&session_id=cs_...
//	error: urnetwork://checkout?errorCode=<code>&errorMessage=...
//
// The error's code is one of CheckoutBridgeError* (-1 from pages before the
// codes) and its message the page's English text, or Stripe's.
//
// The done hand-back comes from one of two places, by how the session was
// created:
//   - redirect_on_completion "never" (BuildInlineCheckoutBridgeUrl): Stripe
//     fires the bridge's onComplete callback and the page hands back in place,
//     with no round trip through the server's return_url.
//   - unset (BuildCheckoutBridgeUrl, older clients): Stripe redirects the
//     webview to the server's return_url, which points back at the bridge
//     with ?complete=1, and the bridge hands back from there.
//
// There is no cancel url: Stripe's embedded flow never leaves the page, so the
// checkout chrome's own close control is the only way out.
//
// Previous hardcode sites (both build and parse duplicated verbatim):
//   - windows: app/src/App/BalanceSheets.cpp (kCheckoutPage/kCheckoutRedirect
//     plus a local percent-encoder)
//   - linux: app/src/UpgradeSheet.cpp (kCheckoutPage/kCheckoutRedirect plus
//     g_uri_escape_string)
const (
	CheckoutBridgeUrl     = "https://ur.io/checkout"
	CheckoutRedirectLink  = "urnetwork://checkout"
	checkoutRedirectHost  = "checkout"
	checkoutRedirectProto = "urnetwork"
	// the bridge page query key that says the session completes through
	// onComplete (mmm EmbeddedCheckout.jsx reads the same key)
	checkoutBridgeRedirectOnCompletionKey = "redirect_on_completion"
)

// The bridge page's codes for a failure it hands back, in
// CheckoutRedirect.ErrorCode. An app shows its own words for a code it knows
// and ErrorMessage for any other, as it does for the -1 of pages before these
// codes.
const (
	// the page was opened without a checkout session (client_secret)
	CheckoutBridgeErrorInvalidRequest = "invalid_request"
	// the page has no Stripe publishable key (a misconfigured site build)
	CheckoutBridgeErrorUnavailable = "checkout_unavailable"
	// Stripe.js did not load (offline, or blocked)
	CheckoutBridgeErrorStripeUnavailable = "stripe_unavailable"
	// Stripe could not start the session; ErrorMessage says why
	CheckoutBridgeErrorCheckout = "checkout_error"
)

// BuildCheckoutBridgeUrl builds the ur.io bridge page url for an embedded
// checkout session's client secret, using the standard urnetwork://checkout
// redirect link. Query values are percent-encoded (spaces as "+", which the
// bridge page's URLSearchParams decodes back to spaces).
func BuildCheckoutBridgeUrl(clientSecret string) string {
	return BuildCheckoutBridgeUrlWithRedirect(clientSecret, CheckoutRedirectLink)
}

// BuildCheckoutBridgeUrlWithRedirect is BuildCheckoutBridgeUrl with a custom
// redirect link, for a platform registered under a different scheme. The
// bridge page validates the redirect scheme; only app schemes it recognizes
// are honored.
func BuildCheckoutBridgeUrlWithRedirect(clientSecret string, redirectLink string) string {
	return fmt.Sprintf(
		"%s?client_secret=%s&redirect_link=%s",
		CheckoutBridgeUrl,
		url.QueryEscape(clientSecret),
		url.QueryEscape(redirectLink),
	)
}

// BuildInlineCheckoutBridgeUrl builds the bridge page url for an embedded
// session created with RedirectOnCompletion StripeRedirectOnCompletionNever.
// Stripe never redirects such a session, so the bridge must hand control back
// from Stripe's onComplete callback; the url carries
// redirect_on_completion=never to say so. Pairing a "never" session with
// BuildCheckoutBridgeUrl would leave the webview on a finished checkout with no
// hand-back.
func BuildInlineCheckoutBridgeUrl(clientSecret string) string {
	return fmt.Sprintf(
		"%s&%s=%s",
		BuildCheckoutBridgeUrl(clientSecret),
		checkoutBridgeRedirectOnCompletionKey,
		url.QueryEscape(StripeRedirectOnCompletionNever),
	)
}

// CheckoutRedirect is the parsed urnetwork://checkout hand-back from the
// bridge page.
type CheckoutRedirect struct {
	// Complete is true when the payment finished inside the bridge
	// (status=complete). The server still only believes the Stripe webhook: a
	// client saying "I paid" is not evidence, so a Complete redirect should
	// start the confirmation poll
	// (SubscriptionBalanceViewController.StartPurchaseConfirmation), not flip
	// any entitlement locally.
	Complete bool
	// SessionId is the Stripe checkout session id (cs_...), when present.
	SessionId string
	// the bridge page's code for the failure, as sent: one of
	// CheckoutBridgeError*, or "-1" from pages before the codes; empty on
	// success
	ErrorCode string
	// ErrorMessage is the bridge's human-readable error, empty on success.
	ErrorMessage string
}

// IsCheckoutRedirect reports whether the uri is a urnetwork://checkout
// hand-back, for webview navigation filters.
func IsCheckoutRedirect(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, checkoutRedirectProto) &&
		strings.EqualFold(u.Host, checkoutRedirectHost)
}

// ParseCheckoutRedirect parses a urnetwork://checkout?... hand-back uri from
// the bridge page. It returns an error for uris that are not checkout
// redirects (use IsCheckoutRedirect to filter first) or that carry a
// malformed query.
//
// The bridge page assembles the query with URLSearchParams
// (form-urlencoding), so "+" in a value is a space; parsing here matches
// that. (The previous linux copy decoded with g_uri_unescape_string, which
// leaves "+" alone -- one of the drifts this shared parser removes.)
func ParseCheckoutRedirect(uri string) (*CheckoutRedirect, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Scheme, checkoutRedirectProto) ||
		!strings.EqualFold(u.Host, checkoutRedirectHost) {
		return nil, fmt.Errorf("not a %s redirect: %s", CheckoutRedirectLink, uri)
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, err
	}
	return &CheckoutRedirect{
		Complete:     values.Get("status") == "complete",
		SessionId:    values.Get("session_id"),
		ErrorCode:    values.Get("errorCode"),
		ErrorMessage: values.Get("errorMessage"),
	}, nil
}

// ---- balance-code client rules -----------------------------------------------

// BalanceCodeLength is the exact length of a redeemable balance code secret.
// The server mints codes with crypto/rand.Text(): 26 base32 characters.
// Previously gated separately in windows BalanceSheets.cpp
// (kBalanceCodeLength) and macOS RedeemBalanceCodeSheet.
const BalanceCodeLength = 26

// IsBalanceCodeFormatValid reports whether the (whitespace-trimmed) secret
// has the exact balance-code length. It is a cheap pre-submit gate only; the
// server is the authority on whether the code exists.
func IsBalanceCodeFormatValid(secret string) bool {
	return len(strings.TrimSpace(secret)) == BalanceCodeLength
}

// Outcomes of a balance-code redeem attempt, for ClassifyBalanceCodeRedeem.
const (
	// BalanceCodeRedeemOutcomeRedeemed: the redeem succeeded and the balance
	// was credited to this network.
	BalanceCodeRedeemOutcomeRedeemed = "redeemed"
	// BalanceCodeRedeemOutcomeAlreadyRedeemed: this network already redeemed
	// the code (it appears in the network's own redeemed-code list). The most
	// important case is a network failure AFTER the server committed: the
	// redeem call fails on the client, but the data was credited. The UI must
	// say "already redeemed" -- never "invalid code" -- because the user has
	// the data.
	BalanceCodeRedeemOutcomeAlreadyRedeemed = "already_redeemed"
	// BalanceCodeRedeemOutcomeInvalid: the server rejected the code. The
	// server's payload for this is {"error":{"message":"Unknown balance
	// code."}} -- and it sends that SAME payload for a code redeemed by a
	// DIFFERENT network (its lookup filters redeem_balance_id IS NULL), so
	// "invalid" here means "not redeemable by you", not proof the code never
	// existed.
	BalanceCodeRedeemOutcomeInvalid = "invalid"
	// BalanceCodeRedeemOutcomeUnknown: the call itself failed (transport
	// error) and the outcome is NOT known -- the server may have committed
	// the redeem. The UI must not report failure; it should refetch the
	// redeemed-code list (Api.GetNetworkRedeemedBalanceCodes) and classify
	// again, or ask the user to retry (a retry after a committed redeem
	// classifies as already_redeemed once the list is consulted).
	BalanceCodeRedeemOutcomeUnknown = "unknown"
)

// ClassifyBalanceCodeRedeem classifies the outcome of a RedeemBalanceCode
// call into one of the BalanceCodeRedeemOutcome* values.
//
//   - result is the callback's result; nil when the call failed in transport.
//   - redeemedCodes is the network's own redeemed-code list
//     (Api.GetNetworkRedeemedBalanceCodes), used to distinguish "already
//     redeemed by this network" from "invalid". Pass nil if unavailable; the
//     classification then cannot detect already_redeemed.
//   - secret is the code the user submitted.
//
// The server's redeem error payload is a single string ("Unknown balance
// code.") for both a nonexistent code and an already-redeemed one, so the
// already-redeemed distinction can only come from the redeemed-code list --
// that is why it is a parameter here instead of a string match.
func ClassifyBalanceCodeRedeem(
	result *RedeemBalanceCodeResult,
	redeemedCodes *RedeemedBalanceCodeList,
	secret string,
) string {
	if result != nil && result.TransferBalance != nil {
		return BalanceCodeRedeemOutcomeRedeemed
	}
	if redeemedCodes != nil {
		trimmed := strings.TrimSpace(secret)
		for i := 0; i < redeemedCodes.Len(); i += 1 {
			code := redeemedCodes.Get(i)
			if code != nil && strings.EqualFold(strings.TrimSpace(code.Secret), trimmed) {
				return BalanceCodeRedeemOutcomeAlreadyRedeemed
			}
		}
	}
	if result != nil && result.Error != nil {
		return BalanceCodeRedeemOutcomeInvalid
	}
	return BalanceCodeRedeemOutcomeUnknown
}
