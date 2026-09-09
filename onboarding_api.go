package sdk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// The onboarding program's api surface (mmm/onboarding/PLAN.md; the server
// contract is connect/api/bringyour.yml): the regional price tier and the
// welcome offer on the plan response, the offer issue call, the experiment
// assignments, the closed client event endpoint (see client_events.go), the
// inline Stripe payment sheet, and the campaign token endpoints.
//
// Every type here is gomobile-safe: times are RFC 3339 strings, maps are
// exposed as lists with lookup helpers, and the only free-form value (event
// props) never crosses the binding.

// Price tier names (pro.yml pro.price_tiers).
const (
	PriceTierStandard = "standard"
	PriceTierRegional = "regional"
)

// How the plan response resolved the price tier's country. Only storefront and
// billing are what will be charged; ip and default are display estimates.
const (
	PriceTierSourceStorefront = "storefront"
	PriceTierSourceBilling    = "billing"
	PriceTierSourceIp         = "ip"
	PriceTierSourceDefault    = "default"
)

// Plan names shared by every purchase path.
const (
	PlanYearly  = "yearly"
	PlanMonthly = "monthly"
	// the Solana welcome-offer plan: a year at the tier's yearly price less the
	// offer discount, plus the 14-day trial; refused unless the offer is redeemable
	PlanYearlyOnboarding = "yearly_onboarding"
)

// Offer states (OnboardingOffer.State).
const (
	OnboardingOfferStateActive   = "active"
	OnboardingOfferStateRedeemed = "redeemed"
	OnboardingOfferStateExpired  = "expired"
)

// Offer surfaces (OnboardingOfferIssueArgs.Surface and the offer.screen.shown
// event's surface prop).
const (
	OfferSurfaceIntroStep   = "intro_step"
	OfferSurfaceFinalScreen = "final_screen"
	OfferSurfaceEmailLink   = "email_link"
	OfferSurfaceAccount     = "account"
)

// Experiment surfaces the plan response keys its assignments by, and the
// variant that means "show nothing".
const (
	ExperimentSurfaceOfferInApp       = "offer.in_app"
	ExperimentSurfaceOfferIntroStep   = "offer.intro_step"
	ExperimentSurfaceOfferFinalScreen = "offer.final_screen"
	ExperimentSurfaceOfferEmail       = "offer.email"
	ExperimentSurfaceOfferAccount     = "offer.account"
	ExperimentSurfaceEmailSequence    = "email.sequence"
	ExperimentVariantHoldout          = "holdout"
)

// PriceTier is the caller's regional Pro price tier. Prices are USD; the store
// or Stripe shows the local-currency figure. The trial and the welcome offer
// apply on every tier.
type PriceTier struct {
	Name       string  `json:"name"`
	YearlyUsd  float64 `json:"yearly_usd"`
	MonthlyUsd float64 `json:"monthly_usd"`
	Currency   string  `json:"currency"`
	// Source is one of the PriceTierSource* values
	Source string `json:"source"`
	// Estimate is true when the tier is a display estimate (ip or default):
	// the store's storefront or the card's billing country decides what is
	// charged
	Estimate bool `json:"estimate"`
}

// IsRegional reports whether this is the regional tier (no per-month line in
// the picker; see PriceEquivalent).
func (self *PriceTier) IsRegional() bool {
	return self.Name == PriceTierRegional
}

// OnboardingOffer is the network's welcome offer: PercentOff (25) off the first
// year of Pro (MonthsFree = 3 in the copy), valid until ExpiresAt, redeemable
// once on any store. FirstYearUsd/RegularYearUsd are for the caller's current
// tier. Redemption handles per store: AppleOfferCode (App Store offer code
// redeem sheet), PlayOfferTag (the yearly base plan's discounted first-cycle
// offer), StripeCouponId (applied server-side); Solana uses PlanYearlyOnboarding.
type OnboardingOffer struct {
	// RFC 3339
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`

	PercentOff     int     `json:"percent_off"`
	MonthsFree     int     `json:"months_free"`
	FirstYearUsd   float64 `json:"first_year_usd"`
	RegularYearUsd float64 `json:"regular_year_usd"`
	Tier           string  `json:"tier"`
	Currency       string  `json:"currency"`
	// State is one of the OnboardingOfferState* values
	State          string `json:"state"`
	AppleOfferCode string `json:"apple_offer_code,omitempty"`
	PlayOfferTag   string `json:"play_offer_tag,omitempty"`
	StripeCouponId string `json:"stripe_coupon_id,omitempty"`
	RedeemedAt     string `json:"redeemed_at,omitempty"`
	Store          string `json:"store,omitempty"`
}

// IsActive reports whether the offer can still be redeemed as of the server's
// answer.
func (self *OnboardingOffer) IsActive() bool {
	return self.State == OnboardingOfferStateActive
}

// ExpiresAtUnixMillis is ExpiresAt as unix milliseconds, 0 when unparseable.
func (self *OnboardingOffer) ExpiresAtUnixMillis() int64 {
	t, err := time.Parse(time.RFC3339Nano, self.ExpiresAt)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// SecondsUntilExpiry is the remaining validity from the device clock, clamped
// at 0 -- the `expires_in_s` prop of offer.screen.shown.
func (self *OnboardingOffer) SecondsUntilExpiry() int64 {
	expiresAt := self.ExpiresAtUnixMillis()
	if expiresAt == 0 {
		return 0
	}
	remaining := (expiresAt - time.Now().UnixMilli()) / 1000
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ExperimentAssignment is the caller's variant for one experiment surface.
type ExperimentAssignment struct {
	Surface      string `json:"surface"`
	ExperimentId string `json:"experiment_id"`
	Variant      string `json:"variant"`
}

// IsHoldout reports whether the surface shows nothing for this caller.
func (self *ExperimentAssignment) IsHoldout() bool {
	return self.Variant == ExperimentVariantHoldout
}

// ExperimentAssignmentList is the plan response's `experiments` object as a
// list (gomobile binds no maps), keyed on the wire by surface.
type ExperimentAssignmentList struct {
	exportedList[*ExperimentAssignment]
}

func NewExperimentAssignmentList() *ExperimentAssignmentList {
	return &ExperimentAssignmentList{
		exportedList: *newExportedList[*ExperimentAssignment](),
	}
}

// ForSurface is the assignment for a surface, or nil when no experiment runs
// on it.
func (self *ExperimentAssignmentList) ForSurface(surface string) *ExperimentAssignment {
	for _, a := range self.values {
		if a != nil && a.Surface == surface {
			return a
		}
	}
	return nil
}

// VariantForSurface is the variant for a surface, "" when none.
func (self *ExperimentAssignmentList) VariantForSurface(surface string) string {
	if a := self.ForSurface(surface); a != nil {
		return a.Variant
	}
	return ""
}

// IsHoldout reports whether the caller is held out on a surface. The in-app
// offer (ExperimentSurfaceOfferInApp) governs the intro step's welcome price
// and the final offer screen together, so those surfaces consult it.
func (self *ExperimentAssignmentList) IsHoldout(surface string) bool {
	if a := self.ForSurface(surface); a != nil {
		return a.IsHoldout()
	}
	switch surface {
	case ExperimentSurfaceOfferIntroStep, ExperimentSurfaceOfferFinalScreen:
		if a := self.ForSurface(ExperimentSurfaceOfferInApp); a != nil {
			return a.IsHoldout()
		}
	}
	return false
}

func (self *ExperimentAssignmentList) UnmarshalJSON(b []byte) error {
	var bySurface map[string]*ExperimentAssignment
	if err := json.Unmarshal(b, &bySurface); err != nil {
		return err
	}
	self.values = nil
	// deterministic order
	surfaces := make([]string, 0, len(bySurface))
	for surface := range bySurface {
		surfaces = append(surfaces, surface)
	}
	sortStrings(surfaces)
	for _, surface := range surfaces {
		a := bySurface[surface]
		if a == nil {
			continue
		}
		a.Surface = surface
		self.values = append(self.values, a)
	}
	return nil
}

// MarshalJSON keeps the wire shape (an object keyed by surface), which is what
// the web reads.
func (self *ExperimentAssignmentList) MarshalJSON() ([]byte, error) {
	bySurface := map[string]*ExperimentAssignment{}
	for _, a := range self.values {
		if a != nil {
			bySurface[a.Surface] = a
		}
	}
	return json.Marshal(bySurface)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; 0 < j && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ----- the plan response with a storefront -----

// SubscriptionBalanceForStorefront is SubscriptionBalance with the store's
// storefront country (StoreKit Storefront.countryCode, Play billing region), so
// the plan response's price_tier is the store's rather than an ip estimate.
// Pass "" where the app has no store.
func (self *Api) SubscriptionBalanceForStorefront(storefrontCountry string, callback SubscriptionBalanceCallback) {
	go connect.HandleError(func() {
		requestUrl := fmt.Sprintf("%s/subscription/balance", self.apiUrl)
		if storefrontCountry = strings.TrimSpace(storefrontCountry); storefrontCountry != "" {
			requestUrl += "?storefront_country=" + url.QueryEscape(storefrontCountry)
		}
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			requestUrl,
			self.GetByJwt(),
			&SubscriptionBalanceResult{},
			callback,
		)
	})
}

// ----- the welcome offer -----

type OnboardingOfferIssueArgs struct {
	// where the offer is shown: OfferSurfaceIntroStep (default),
	// OfferSurfaceFinalScreen or OfferSurfaceAccount
	Surface string `json:"surface,omitempty"`
	// the store's storefront country, when the app knows it
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

type OnboardingOfferIssueResult struct {
	Offer *OnboardingOffer `json:"offer,omitempty"`
	// Created is true for the call that issued the offer; false when it
	// already existed (in whatever state)
	Created bool             `json:"created"`
	Error   *OnboardingError `json:"error,omitempty"`
}

type OnboardingError struct {
	Message string `json:"message"`
}

type OnboardingOfferIssueCallback connect.ApiCallback[*OnboardingOfferIssueResult]

// OnboardingOfferIssue issues the caller's welcome offer once (idempotent; an
// expired offer is never re-issued). Call it the moment the offer surface
// renders. Refused with Error for the in-app holdout.
func (self *Api) OnboardingOfferIssue(args *OnboardingOfferIssueArgs, callback OnboardingOfferIssueCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/onboarding/offer/issue", self.apiUrl),
			args,
			self.GetByJwt(),
			&OnboardingOfferIssueResult{},
			callback,
		)
	})
}

// ----- the inline Stripe payment sheet -----

// Intent types of a payment sheet.
const (
	StripeIntentTypeSetup   = "setup"
	StripeIntentTypePayment = "payment"
)

type StripePaymentSheetArgs struct {
	// PlanYearly or PlanMonthly
	Plan string `json:"plan"`
	// the store's storefront country, when the app knows it
	StorefrontCountry string `json:"storefront_country,omitempty"`
	// the Stripe API version the mobile SDK requires for its ephemeral key
	StripeVersion string `json:"stripe_version,omitempty"`
}

type StripePaymentSheetResult struct {
	CustomerId         string `json:"customer_id,omitempty"`
	EphemeralKeySecret string `json:"ephemeral_key_secret,omitempty"`
	// the yearly plan (with its trial) confirms a SetupIntent; the monthly plan
	// (no trial) confirms the first invoice's PaymentIntent. IntentType says
	// which secret is set.
	SetupIntentClientSecret   string `json:"setup_intent_client_secret,omitempty"`
	PaymentIntentClientSecret string `json:"payment_intent_client_secret,omitempty"`
	IntentType                string `json:"intent_type,omitempty"`
	SubscriptionId            string `json:"subscription_id,omitempty"`
	PublishableKey            string `json:"publishable_key,omitempty"`
	Tier                      string `json:"tier,omitempty"`
	Currency                  string `json:"currency,omitempty"`
	Plan                      string `json:"plan,omitempty"`
	// the first period's price (the offer applied when eligible), and the
	// regular price of a period
	AmountFirstPeriodUsd float64 `json:"amount_first_period_usd"`
	RegularPeriodUsd     float64 `json:"regular_period_usd"`
	TrialDays            int     `json:"trial_days"`
	// RFC 3339; empty when the plan has no trial
	TrialEndAt   string           `json:"trial_end_at,omitempty"`
	OfferApplied bool             `json:"offer_applied"`
	Error        *OnboardingError `json:"error,omitempty"`
}

type StripePaymentSheetCallback connect.ApiCallback[*StripePaymentSheetResult]

// StripePaymentSheet prepares an inline Stripe PaymentSheet purchase of Pro
// (the non-Play Android flavors, Windows and Linux only).
func (self *Api) StripePaymentSheet(args *StripePaymentSheetArgs, callback StripePaymentSheetCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/subscription/stripe/payment-sheet", self.apiUrl),
			args,
			self.GetByJwt(),
			&StripePaymentSheetResult{},
			callback,
		)
	})
}

type StripePricesResult struct {
	Tier           string  `json:"tier"`
	Currency       string  `json:"currency"`
	YearlyPriceId  string  `json:"yearly_price_id"`
	MonthlyPriceId string  `json:"monthly_price_id"`
	YearlyUsd      float64 `json:"yearly_usd"`
	MonthlyUsd     float64 `json:"monthly_usd"`
	PublishableKey string  `json:"publishable_key"`
	// the welcome-offer coupon, when the caller's offer is redeemable (yearly only)
	OnboardingCouponId string           `json:"onboarding_coupon_id,omitempty"`
	OfferEligible      bool             `json:"offer_eligible"`
	Error              *OnboardingError `json:"error,omitempty"`
}

type StripePricesCallback connect.ApiCallback[*StripePricesResult]

// StripePrices returns the caller's tier's Stripe price ids (for clients that
// build their own Stripe flow, e.g. the Payment Element in a web view).
func (self *Api) StripePrices(storefrontCountry string, callback StripePricesCallback) {
	go connect.HandleError(func() {
		requestUrl := fmt.Sprintf("%s/subscription/stripe/prices", self.apiUrl)
		if storefrontCountry = strings.TrimSpace(storefrontCountry); storefrontCountry != "" {
			requestUrl += "?storefront_country=" + url.QueryEscape(storefrontCountry)
		}
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			requestUrl,
			self.GetByJwt(),
			&StripePricesResult{},
			callback,
		)
	})
}

// ----- campaign tokens -----

// In-app destinations a landing click routes to (OnboardingClickResult.Destination).
const (
	OnboardingDestinationConnect  = "onboarding/connect"
	OnboardingDestinationWidgets  = "onboarding/widgets"
	OnboardingDestinationOffer    = "onboarding/offer"
	OnboardingDestinationFeedback = "onboarding/feedback"
)

type OnboardingClickArgs struct {
	Token string `json:"token"`
}

type OnboardingClickResult struct {
	Ok          bool   `json:"ok"`
	Step        string `json:"step,omitempty"`
	Destination string `json:"destination,omitempty"`
	// invalid | expired
	Error string `json:"error,omitempty"`
}

type OnboardingClickCallback connect.ApiCallback[*OnboardingClickResult]

// OnboardingClick records a campaign landing click (the landing page calls
// this; no auth) and returns the in-app destination.
func (self *Api) OnboardingClick(args *OnboardingClickArgs, callback OnboardingClickCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/onboarding/click", self.apiUrl),
			args,
			self.GetByJwt(),
			&OnboardingClickResult{},
			callback,
		)
	})
}

type OnboardingFeedbackTokenResult struct {
	Ok   bool   `json:"ok"`
	Step string `json:"step,omitempty"`
	// 1-5; 0 when the link carried no rating
	Rating int    `json:"rating,omitempty"`
	Reason string `json:"reason,omitempty"`
	// invalid | expired
	Error string `json:"error,omitempty"`
}

type OnboardingFeedbackTokenCallback connect.ApiCallback[*OnboardingFeedbackTokenResult]

// OnboardingFeedbackToken resolves a feedback link token (ur.io/f/<token>) to
// its pre-filled rating or reason for the in-app feedback screen. rating and
// reason are the link's query values (pass 0 / "" when absent).
func (self *Api) OnboardingFeedbackToken(token string, rating int, reason string, callback OnboardingFeedbackTokenCallback) {
	go connect.HandleError(func() {
		query := url.Values{}
		if 0 < rating {
			query.Set("r", fmt.Sprintf("%d", rating))
		}
		if reason = strings.TrimSpace(reason); reason != "" {
			query.Set("why", reason)
		}
		requestUrl := fmt.Sprintf("%s/onboarding/feedback/%s", self.apiUrl, url.PathEscape(strings.TrimSpace(token)))
		if encoded := query.Encode(); encoded != "" {
			requestUrl += "?" + encoded
		}
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			requestUrl,
			self.GetByJwt(),
			&OnboardingFeedbackTokenResult{},
			callback,
		)
	})
}

// ----- sign-up opt-out -----

// MarshalJSON sends the sign-up form's product-updates choice. The Go zero
// value of NetworkCreateArgs (ProductUpdatesOptOut false) keeps the preference
// ON, matching the form shipping with the line ticked, so no existing caller
// opts anyone out by omission.
func (self NetworkCreateArgs) MarshalJSON() ([]byte, error) {
	type plain NetworkCreateArgs
	return json.Marshal(&struct {
		plain
		ProductUpdates bool `json:"product_updates"`
	}{
		plain:          plain(self),
		ProductUpdates: !self.ProductUpdatesOptOut,
	})
}

// UnmarshalJSON is the inverse: the wire field product_updates sets
// ProductUpdatesOptOut, and an absent field keeps the preference on, so args
// that cross a json boundary (the C ABI marshals every args value) round trip
// exactly. The C wrapper's struct exposes the field as an optional bool.
func (self *NetworkCreateArgs) UnmarshalJSON(data []byte) error {
	type plain NetworkCreateArgs
	var wire struct {
		plain
		ProductUpdates *bool `json:"product_updates"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*self = NetworkCreateArgs(wire.plain)
	self.ProductUpdatesOptOut = wire.ProductUpdates != nil && !*wire.ProductUpdates
	return nil
}
