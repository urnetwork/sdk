package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// The crypto invoice routes against a local server answering with the shapes
// of mmm/ZCASH-CONTRACT.md §2: each call's method, path, body and credential, and the
// decode of every field the apps read.

const (
	cryptoPayTestIntentId    = "0b6f0b6f-0000-4000-8000-000000000001"
	cryptoPayTestStatusToken = "kq3-synthetic-status-token"
	cryptoPayTestXmrAddress  = "8BSyntheticXmrSubaddressFixture"
	cryptoPayTestXmrUri      = "monero:" + cryptoPayTestXmrAddress + "?tx_amount=0.072387&tx_description=URnetwork%20Pro%20yearly"
)

// the server's answer to an xmr yearly invoice, as mmm/ZCASH-CONTRACT.md §2.3 shows it
const cryptoPayTestIntentJson = `{
	"intent_id": "` + cryptoPayTestIntentId + `", "status_token": "` + cryptoPayTestStatusToken + `",
	"asset": "xmr", "plan": "yearly", "item_id": "",
	"delivery": "account", "network_name": "",
	"tier": "standard", "offer_applied": false,
	"price_usd": 40, "regular_price_usd": 40, "rate_usd": 555.36,
	"address": "` + cryptoPayTestXmrAddress + `", "amount": "0.072387", "amount_atomic": "72387000000",
	"memo": "",
	"uri": "` + cryptoPayTestXmrUri + `",
	"quoted_at": "2026-10-20T18:00:00Z", "expires_at": "2026-10-20T18:30:00Z",
	"confirmations_required": 1,
	"status": {
		"intent_id": "` + cryptoPayTestIntentId + `", "asset": "xmr", "plan": "yearly", "item_id": "",
		"delivery": "account", "network_name": "",
		"status": "pending", "review_reason": "",
		"amount": "0.072387", "amount_atomic": "72387000000",
		"received": "0", "received_atomic": "0",
		"remaining": "0.072387", "remaining_atomic": "72387000000",
		"confirmations": 0, "confirmations_required": 1,
		"expires_at": "2026-10-20T18:30:00Z", "credited_at": null,
		"balance_code": "",
		"error": null
	}
}`

type cryptoPayTestRequest struct {
	method        string
	path          string
	body          string
	authorization string
}

// A server for the crypto routes that records each request and answers with
// the body set for its path.
func newCryptoPayTestApi(t *testing.T, answers map[string]string) (*Api, func() []cryptoPayTestRequest) {
	t.Helper()
	var mutex sync.Mutex
	requests := []cryptoPayTestRequest{}
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mutex.Lock()
		requests = append(requests, cryptoPayTestRequest{
			method:        r.Method,
			path:          r.URL.Path,
			body:          string(body),
			authorization: r.Header.Get("Authorization"),
		})
		mutex.Unlock()
		answer, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer)
	})
	return api, func() []cryptoPayTestRequest {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]cryptoPayTestRequest{}, requests...)
	}
}

func requireJsonEqual(t *testing.T, got string, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("body %q is not json: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	gotBytes, _ := json.Marshal(gotValue)
	wantBytes, _ := json.Marshal(wantValue)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("body = %s, want %s", gotBytes, wantBytes)
	}
}

func TestGetCryptoPaymentAssets(t *testing.T) {
	api, requests := newCryptoPayTestApi(t, map[string]string{
		"/pay/crypto/assets": `{"assets": [
			{"asset": "usdc_solana", "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
			 "confirmations_required": 1, "quote_ttl_seconds": 3600},
			{"asset": "xmr", "plans": ["yearly", "yearly_onboarding"], "items": ["data_1tib", "data_10tib"],
			 "confirmations_required": 1, "quote_ttl_seconds": 1800}
		]}`,
	})
	callback, c := connect.NewBlockingApiCallback[*CryptoPaymentAssetsResult](context.Background())
	api.GetCryptoPaymentAssets(callback)
	r := awaitApiResult(t, c, "GetCryptoPaymentAssets never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	request := requests()[0]
	connect.AssertEqual(t, request.method, http.MethodGet)
	connect.AssertEqual(t, request.path, "/pay/crypto/assets")
	connect.AssertEqual(t, request.authorization, "")

	assets := r.Result.Assets
	if assets == nil || assets.Len() != 2 {
		t.Fatalf("assets = %+v", assets)
	}
	xmr := assets.Get(1)
	connect.AssertEqual(t, xmr.Asset, CryptoAssetXmr)
	connect.AssertEqual(t, xmr.ConfirmationsRequired, 1)
	connect.AssertEqual(t, xmr.QuoteTtlSeconds, 1800)
	if !xmr.Plans.Contains(PlanYearlyOnboarding) || !xmr.Items.Contains(StripeItemData10Tib) {
		t.Fatalf("xmr sells %v and %v", xmr.Plans.getAll(), xmr.Items.getAll())
	}
	items := NewStringList()
	items.Add(PlanYearly)
	connect.AssertEqual(t, stringListValues(PaymentMethods(assets, items)), []string{PaymentMethodCard, CryptoAssetUsdcSolana, CryptoAssetXmr})
}

// The signed-in intent carries the network's credential and the plan, never
// a price; the answer decodes whole, the nested status included.
func TestCreateCryptoPaymentIntent(t *testing.T) {
	api, requests := newCryptoPayTestApi(t, map[string]string{"/pay/crypto/intent": cryptoPayTestIntentJson})
	callback, c := connect.NewBlockingApiCallback[*CryptoPaymentIntentResult](context.Background())
	api.CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{
		Asset:             CryptoAssetXmr,
		Plan:              PlanYearly,
		StorefrontCountry: "US",
	}, callback)
	r := awaitApiResult(t, c, "CreateCryptoPaymentIntent never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	request := requests()[0]
	connect.AssertEqual(t, request.method, http.MethodPost)
	connect.AssertEqual(t, request.path, "/pay/crypto/intent")
	connect.AssertEqual(t, request.authorization, "Bearer test-network-jwt")
	requireJsonEqual(t, request.body, `{"asset":"xmr","plan":"yearly","storefront_country":"US"}`)

	result := r.Result
	if result.Error != nil {
		t.Fatalf("error = %+v", result.Error)
	}
	connect.AssertEqual(t, result.IntentId, cryptoPayTestIntentId)
	connect.AssertEqual(t, result.StatusToken, cryptoPayTestStatusToken)
	connect.AssertEqual(t, result.Asset, CryptoAssetXmr)
	connect.AssertEqual(t, result.Plan, PlanYearly)
	connect.AssertEqual(t, result.Delivery, CryptoPaymentDeliveryAccount)
	connect.AssertEqual(t, result.Tier, "standard")
	connect.AssertEqual(t, result.PriceUsd, 40.0)
	connect.AssertEqual(t, result.RegularPriceUsd, 40.0)
	connect.AssertEqual(t, result.RateUsd, 555.36)
	connect.AssertEqual(t, result.Address, cryptoPayTestXmrAddress)
	// exact strings: a piconero amount does not survive a float
	connect.AssertEqual(t, result.Amount, "0.072387")
	connect.AssertEqual(t, result.AmountAtomic, "72387000000")
	connect.AssertEqual(t, result.Uri, cryptoPayTestXmrUri)
	connect.AssertEqual(t, result.QuotedAt, "2026-10-20T18:00:00Z")
	connect.AssertEqual(t, result.ExpiresAt, "2026-10-20T18:30:00Z")
	connect.AssertEqual(t, result.ConfirmationsRequired, 1)
	if !CheckCryptoPaymentUri(result.Asset, result.Uri, result.Address) {
		t.Fatal("the server's uri does not pass the check")
	}
	status := result.Status
	if status == nil {
		t.Fatal("no status")
	}
	connect.AssertEqual(t, status.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, status.Received, "0")
	connect.AssertEqual(t, status.Remaining, "0.072387")
	connect.AssertEqual(t, status.RemainingAtomic, "72387000000")
	connect.AssertEqual(t, status.ConfirmationsRequired, 1)
	connect.AssertEqual(t, status.CreditedAt, "")
	if status.Error != nil {
		t.Fatalf("status error = %+v", status.Error)
	}
}

// The buy-data intent and the status and requote routes carry no credential:
// the status token is the invoice's only credential, and a code bought
// without a sign-in must not arrive with the device's network.
func TestCryptoPaymentPublicRoutesSendNoCredential(t *testing.T) {
	api, requests := newCryptoPayTestApi(t, map[string]string{
		"/pay/data/crypto-intent": `{"intent_id": "` + cryptoPayTestIntentId + `", "status_token": "t", "asset": "zec",
			"item_id": "data_1tib", "delivery": "code", "address": "u1synthetic", "amount": "1.5", "amount_atomic": "150000000"}`,
		"/pay/crypto/status": `{"intent_id": "` + cryptoPayTestIntentId + `", "asset": "zec", "item_id": "data_1tib",
			"delivery": "code", "status": "paid", "review_reason": "", "amount": "1.5", "amount_atomic": "150000000",
			"received": "1.5", "received_atomic": "150000000", "remaining": "0", "remaining_atomic": "0",
			"confirmations": 10, "confirmations_required": 10, "expires_at": "2026-10-20T18:30:00Z",
			"credited_at": "2026-10-20T18:20:00Z", "balance_code": "SYNTHETICBALANCECODE0000000", "error": null}`,
		"/pay/crypto/requote": `{"error": {"code": "invalid_request", "message": "Something was already received."}}`,
	})
	api.SetByJwt(credentialTestNetworkJwt(t, credentialTestNetworkId))

	intentCallback, intentC := connect.NewBlockingApiCallback[*CryptoPaymentIntentResult](context.Background())
	api.CreateDataCryptoPaymentIntent(&CreateDataCryptoPaymentIntentArgs{
		Asset:  CryptoAssetZec,
		ItemId: StripeItemData1Tib,
		Email:  "buyer@example.com",
	}, intentCallback)
	intent := awaitApiResult(t, intentC, "CreateDataCryptoPaymentIntent never returned")
	if intent.Error != nil {
		t.Fatal(intent.Error)
	}
	connect.AssertEqual(t, intent.Result.Delivery, CryptoPaymentDeliveryCode)

	statusArgs := &CryptoPaymentStatusArgs{IntentId: cryptoPayTestIntentId, StatusToken: cryptoPayTestStatusToken}
	statusCallback, statusC := connect.NewBlockingApiCallback[*CryptoPaymentStatusResult](context.Background())
	api.GetCryptoPaymentStatus(statusArgs, statusCallback)
	status := awaitApiResult(t, statusC, "GetCryptoPaymentStatus never returned")
	if status.Error != nil {
		t.Fatal(status.Error)
	}
	connect.AssertEqual(t, status.Result.Status, CryptoPaymentStatusPaid)
	connect.AssertEqual(t, status.Result.BalanceCode, "SYNTHETICBALANCECODE0000000")
	connect.AssertEqual(t, status.Result.CreditedAt, "2026-10-20T18:20:00Z")
	connect.AssertEqual(t, status.Result.Confirmations, 10)

	requoteCallback, requoteC := connect.NewBlockingApiCallback[*CryptoPaymentIntentResult](context.Background())
	api.RequoteCryptoPayment(statusArgs, requoteCallback)
	requote := awaitApiResult(t, requoteC, "RequoteCryptoPayment never returned")
	if requote.Error != nil || requote.Result.Error == nil {
		t.Fatalf("requote = %+v", requote)
	}
	connect.AssertEqual(t, requote.Result.Error.Code, "invalid_request")
	connect.AssertEqual(t, requote.Result.Error.Message, "Something was already received.")

	sent := requests()
	if len(sent) != 3 {
		t.Fatalf("requests = %+v", sent)
	}
	for _, request := range sent {
		connect.AssertEqual(t, request.method, http.MethodPost)
		if request.authorization != "" {
			t.Errorf("%s carried a credential: %q", request.path, request.authorization)
		}
		if strings.Contains(request.body, "jwt") {
			t.Errorf("%s carried a token in its body: %s", request.path, request.body)
		}
	}
	connect.AssertEqual(t, sent[0].path, "/pay/data/crypto-intent")
	requireJsonEqual(t, sent[0].body, `{"asset":"zec","item_id":"data_1tib","network_name":"","email":"buyer@example.com"}`)
	connect.AssertEqual(t, sent[1].path, "/pay/crypto/status")
	requireJsonEqual(t, sent[1].body, `{"intent_id":"`+cryptoPayTestIntentId+`","status_token":"`+cryptoPayTestStatusToken+`"}`)
	connect.AssertEqual(t, sent[2].path, "/pay/crypto/requote")
	requireJsonEqual(t, sent[2].body, sent[1].body)
}

// The client can never name its own price or address: the args have no field
// for either, and an empty plan or item is left off the wire.
func TestCryptoPaymentArgsCarryNoPrice(t *testing.T) {
	for _, args := range []any{
		&CreateCryptoPaymentIntentArgs{Asset: CryptoAssetZec, ItemId: StripeItemData1Tib},
		&CreateDataCryptoPaymentIntentArgs{Asset: CryptoAssetZec, ItemId: StripeItemData1Tib, NetworkName: "alice"},
	} {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"amount", "amount_atomic", "amount_usd", "price", "price_usd", "address", "uri"} {
			if _, ok := wire[forbidden]; ok {
				t.Errorf("%T sent %q; the server quotes, never the client", args, forbidden)
			}
		}
		if _, ok := wire["plan"]; ok {
			t.Errorf("%T sent an empty plan beside the item", args)
		}
	}
}

// The refusal codes survive the decode, so an app can pick its own words.
func TestCryptoPaymentRefusalKeepsCode(t *testing.T) {
	for _, code := range []string{
		PurchaseErrorCodeAssetUnavailable,
		PurchaseErrorCodeQuoteExpired,
		PurchaseErrorCodeUnderpaid,
		PurchaseErrorCodeGuestSignInRequired,
	} {
		var result CryptoPaymentIntentResult
		if err := json.Unmarshal([]byte(`{"error": {"code": "`+code+`", "message": "synthetic"}}`), &result); err != nil {
			t.Fatal(err)
		}
		if result.Error == nil || result.Error.Code != code || result.Error.Message != "synthetic" {
			t.Fatalf("error = %+v", result.Error)
		}
		if result.IntentId != "" || result.Status != nil {
			t.Fatalf("a refusal decoded as an invoice: %+v", result)
		}
	}
}

// The balance result carries the enabled coins and the one-time term; an
// older server's answer leaves both nil, so no crypto method is offered.
func TestSubscriptionBalanceDecodesCryptoFields(t *testing.T) {
	var result SubscriptionBalanceResult
	if err := json.Unmarshal([]byte(`{
		"start_balance_byte_count": 0, "balance_byte_count": 0, "open_transfer_byte_count": 0,
		"pending_payout_usd_nano_cents": 0, "update_time": "2026-10-20T00:00:00Z", "onboarding_offer": null,
		"current_subscription": {"subscription_id": "00000000-0000-0000-0000-000000000021", "store": "monero", "plan": "supporter"},
		"crypto_assets": [
			{"asset": "zec", "plans": ["yearly"], "items": ["data_1tib"], "confirmations_required": 10, "quote_ttl_seconds": 1800}
		],
		"crypto_term": {"market": "monero", "term_end_time": "2026-10-20T00:00:00Z", "grace_end_time": "2026-11-19T00:00:00Z"}
	}`), &result); err != nil {
		t.Fatal(err)
	}
	if result.CryptoAssets == nil || result.CryptoAssets.Len() != 1 || result.CryptoAssets.Get(0).Asset != CryptoAssetZec {
		t.Fatalf("crypto assets = %+v", result.CryptoAssets)
	}
	if result.CryptoTerm == nil || result.CryptoTerm.Market != "monero" || result.CryptoTerm.GraceEndTime != "2026-11-19T00:00:00Z" {
		t.Fatalf("crypto term = %+v", result.CryptoTerm)
	}
	connect.AssertEqual(t, ClassifySubscriptionStore(result.CurrentSubscription.Store), SubscriptionStoreCrypto)

	var older SubscriptionBalanceResult
	if err := json.Unmarshal([]byte(`{"start_balance_byte_count": 0, "balance_byte_count": 0,
		"open_transfer_byte_count": 0, "pending_payout_usd_nano_cents": 0, "update_time": "", "onboarding_offer": null}`), &older); err != nil {
		t.Fatal(err)
	}
	if older.CryptoAssets != nil || older.CryptoTerm != nil {
		t.Fatalf("an older answer gave crypto fields: %+v %+v", older.CryptoAssets, older.CryptoTerm)
	}
	connect.AssertEqual(t, stringListValues(PaymentMethods(older.CryptoAssets, NewStringList())), []string{PaymentMethodCard})
}
