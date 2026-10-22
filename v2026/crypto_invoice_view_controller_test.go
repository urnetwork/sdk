package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The invoice state machine on a fake clock and a fake api, driven step by
// step: no loop, no sleeps, no network. js/test/crypto_invoice.test.ts runs
// the same cases against the TypeScript twin.

const (
	cryptoInvoiceTestNetworkId      = "00000000-0000-0000-0000-00000000c0f1"
	cryptoInvoiceTestOtherNetworkId = "00000000-0000-0000-0000-00000000c0f2"
	cryptoInvoiceTestQuoteTtl       = 30 * time.Minute
)

func cryptoInvoiceTestTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// The server's xmr yearly invoice, quoted at now.
func cryptoInvoiceTestIntent(now time.Time) *CryptoPaymentIntentResult {
	expiresAt := now.Add(cryptoInvoiceTestQuoteTtl)
	return &CryptoPaymentIntentResult{
		IntentId:              cryptoPayTestIntentId,
		StatusToken:           cryptoPayTestStatusToken,
		Asset:                 CryptoAssetXmr,
		Plan:                  PlanYearly,
		Delivery:              CryptoPaymentDeliveryAccount,
		Tier:                  "standard",
		PriceUsd:              40,
		RegularPriceUsd:       40,
		RateUsd:               555.36,
		Address:               cryptoPayTestXmrAddress,
		Amount:                "0.072387",
		AmountAtomic:          "72387000000",
		Uri:                   cryptoPayTestXmrUri,
		QuotedAt:              cryptoInvoiceTestTime(now),
		ExpiresAt:             cryptoInvoiceTestTime(expiresAt),
		ConfirmationsRequired: 1,
		Status:                cryptoInvoiceTestStatus(CryptoPaymentStatusPending, expiresAt),
	}
}

// The status of the invoice above.
func cryptoInvoiceTestStatus(status string, expiresAt time.Time) *CryptoPaymentStatusResult {
	result := &CryptoPaymentStatusResult{
		IntentId:              cryptoPayTestIntentId,
		Asset:                 CryptoAssetXmr,
		Plan:                  PlanYearly,
		Delivery:              CryptoPaymentDeliveryAccount,
		Status:                status,
		Amount:                "0.072387",
		AmountAtomic:          "72387000000",
		Received:              "0",
		ReceivedAtomic:        "0",
		Remaining:             "0.072387",
		RemainingAtomic:       "72387000000",
		ConfirmationsRequired: 1,
		ExpiresAt:             cryptoInvoiceTestTime(expiresAt),
	}
	switch status {
	case CryptoPaymentStatusSeen:
		result.Received, result.ReceivedAtomic = "0.05", "50000000000"
		result.Remaining, result.RemainingAtomic = "0.022387", "22387000000"
	case CryptoPaymentStatusConfirming, CryptoPaymentStatusPaid:
		result.Received, result.ReceivedAtomic = "0.072387", "72387000000"
		result.Remaining, result.RemainingAtomic = "0", "0"
		if status == CryptoPaymentStatusPaid {
			result.Confirmations = 1
			result.CreditedAt = cryptoInvoiceTestTime(expiresAt.Add(-cryptoInvoiceTestQuoteTtl / 2))
		}
	case CryptoPaymentStatusReview:
		result.ReviewReason = "underpaid"
		result.Received, result.ReceivedAtomic = "0.05", "50000000000"
		result.Remaining, result.RemainingAtomic = "0.022387", "22387000000"
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeUnderpaid, Message: "Underpaid."}
	case CryptoPaymentStatusExpired:
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeQuoteExpired, Message: "The quote expired."}
	case CryptoPaymentStatusUnknown:
		return &CryptoPaymentStatusResult{Status: CryptoPaymentStatusUnknown}
	}
	return result
}

const (
	cryptoInvoiceTestBalanceCode = "SYNTHETICBALANCECODE0000000"
	// the sdk's fixture key, never the merchant
	cryptoInvoiceTestUsdcAddress = "4wsaez5pzZPytskrQgV4TEc7mBqhT4WStJXfvMKRc2tQ"
	cryptoInvoiceTestUsdcMemo    = "7cVfgArCheMR6Cs4t6vz5rfnqd56vZq4ndaBrY5xkxXy"
)

// A 1 TiB data pack bought with USDC for a balance code, quoted at now. USDC
// reports only pending, paid and expired, so its status never says that money
// is on its way.
func cryptoInvoiceTestCodeIntent(intentId string, statusToken string, now time.Time) *CryptoPaymentIntentResult {
	expiresAt := now.Add(cryptoInvoiceTestQuoteTtl)
	return &CryptoPaymentIntentResult{
		IntentId:        intentId,
		StatusToken:     statusToken,
		Asset:           CryptoAssetUsdcSolana,
		ItemId:          StripeItemData1Tib,
		Delivery:        CryptoPaymentDeliveryCode,
		PriceUsd:        40,
		RegularPriceUsd: 40,
		RateUsd:         1,
		Address:         cryptoInvoiceTestUsdcAddress,
		Amount:          "40.001234",
		AmountAtomic:    "40001234",
		Memo:            cryptoInvoiceTestUsdcMemo,
		Uri: "solana:" + cryptoInvoiceTestUsdcAddress + "?amount=40.001234&spl-token=EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" +
			"&reference=" + cryptoInvoiceTestUsdcMemo + "&label=URnetwork&message=URnetwork%201%20TiB%20data",
		QuotedAt:              cryptoInvoiceTestTime(now),
		ExpiresAt:             cryptoInvoiceTestTime(expiresAt),
		ConfirmationsRequired: 1,
		Status:                cryptoInvoiceTestCodeStatus(intentId, CryptoPaymentStatusPending, expiresAt),
	}
}

// A 1 TiB data pack for the network "alice", bought without a sign-in and
// paid in Monero, quoted at now.
func cryptoInvoiceTestNetworkIntent(intentId string, statusToken string, now time.Time) *CryptoPaymentIntentResult {
	intent := cryptoInvoiceTestIntent(now)
	intent.IntentId, intent.StatusToken = intentId, statusToken
	intent.Plan, intent.ItemId = "", StripeItemData1Tib
	intent.Delivery, intent.NetworkName = CryptoPaymentDeliveryNetwork, "alice"
	intent.Status = cryptoInvoiceTestNetworkStatus(intentId, CryptoPaymentStatusPending, now.Add(cryptoInvoiceTestQuoteTtl))
	return intent
}

// The status of a network invoice above.
func cryptoInvoiceTestNetworkStatus(intentId string, status string, expiresAt time.Time) *CryptoPaymentStatusResult {
	result := cryptoInvoiceTestStatus(status, expiresAt)
	if status == CryptoPaymentStatusUnknown {
		return result
	}
	result.IntentId = intentId
	result.Plan, result.ItemId = "", StripeItemData1Tib
	result.Delivery, result.NetworkName = CryptoPaymentDeliveryNetwork, "alice"
	return result
}

// The status of a balance-code invoice above.
func cryptoInvoiceTestCodeStatus(intentId string, status string, expiresAt time.Time) *CryptoPaymentStatusResult {
	if status == CryptoPaymentStatusUnknown {
		return &CryptoPaymentStatusResult{Status: CryptoPaymentStatusUnknown}
	}
	result := &CryptoPaymentStatusResult{
		IntentId:              intentId,
		Asset:                 CryptoAssetUsdcSolana,
		ItemId:                StripeItemData1Tib,
		Delivery:              CryptoPaymentDeliveryCode,
		Status:                status,
		Amount:                "40.001234",
		AmountAtomic:          "40001234",
		Received:              "0",
		ReceivedAtomic:        "0",
		Remaining:             "40.001234",
		RemainingAtomic:       "40001234",
		ConfirmationsRequired: 1,
		ExpiresAt:             cryptoInvoiceTestTime(expiresAt),
	}
	switch status {
	case CryptoPaymentStatusPaid:
		result.Received, result.ReceivedAtomic = "40.001234", "40001234"
		result.Remaining, result.RemainingAtomic = "0", "0"
		result.Confirmations = 1
		result.BalanceCode = cryptoInvoiceTestBalanceCode
	case CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusReview:
		result.Received, result.ReceivedAtomic = "20", "20000000"
		result.Remaining, result.RemainingAtomic = "20.001234", "20001234"
	case CryptoPaymentStatusExpired:
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeQuoteExpired, Message: "The quote expired."}
	}
	return result
}

// The api calls the controller made, answered at once from the fields, or
// kept for the test to answer later while hold is set.
type cryptoInvoiceFakeApi struct {
	intentCalls  []*CreateCryptoPaymentIntentArgs
	statusCalls  []*CryptoPaymentStatusArgs
	requoteCalls []*CryptoPaymentStatusArgs

	intentResult  *CryptoPaymentIntentResult
	intentErr     error
	statusResult  *CryptoPaymentStatusResult
	statusErr     error
	requoteResult *CryptoPaymentIntentResult
	requoteErr    error

	holdIntent  bool
	holdStatus  bool
	holdRequote bool
	heldIntent  []CryptoPaymentIntentCallback
	heldStatus  []CryptoPaymentStatusCallback
	heldRequote []CryptoPaymentIntentCallback
}

func (self *cryptoInvoiceFakeApi) createIntent(args *CreateCryptoPaymentIntentArgs, callback CryptoPaymentIntentCallback) {
	self.intentCalls = append(self.intentCalls, args)
	if self.holdIntent {
		self.heldIntent = append(self.heldIntent, callback)
		return
	}
	callback.Result(self.intentResult, self.intentErr)
}

func (self *cryptoInvoiceFakeApi) status(args *CryptoPaymentStatusArgs, callback CryptoPaymentStatusCallback) {
	self.statusCalls = append(self.statusCalls, args)
	if self.holdStatus {
		self.heldStatus = append(self.heldStatus, callback)
		return
	}
	callback.Result(self.statusResult, self.statusErr)
}

func (self *cryptoInvoiceFakeApi) requote(args *CryptoPaymentStatusArgs, callback CryptoPaymentIntentCallback) {
	self.requoteCalls = append(self.requoteCalls, args)
	if self.holdRequote {
		self.heldRequote = append(self.heldRequote, callback)
		return
	}
	callback.Result(self.requoteResult, self.requoteErr)
}

type cryptoInvoiceHarness struct {
	t         *testing.T
	ctx       context.Context
	dir       string
	clock     *stepTestClock
	api       *cryptoInvoiceFakeApi
	networkId string
	vc        *CryptoInvoiceViewController
	changes   int
}

func (self *cryptoInvoiceHarness) CryptoInvoiceChanged() {
	self.changes += 1
}

// A controller saving in dir ("" for memory) on a fresh fake api, signed in to
// the test network at 2026-10-20 18:00 UTC.
func newCryptoInvoiceHarness(t *testing.T, dir string) *cryptoInvoiceHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	harness := &cryptoInvoiceHarness{
		t:         t,
		ctx:       ctx,
		dir:       dir,
		clock:     &stepTestClock{now: time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)},
		api:       &cryptoInvoiceFakeApi{},
		networkId: cryptoInvoiceTestNetworkId,
	}
	harness.vc = harness.newController()
	return harness
}

// Another controller on the same directory, clock and api, as after a relaunch.
func (self *cryptoInvoiceHarness) newController() *CryptoInvoiceViewController {
	vc := newCryptoInvoiceViewControllerWithoutRun(self.ctx, nil, self.dir)
	vc.nowFunc = self.clock.Now
	vc.networkIdFunc = func() string { return self.networkId }
	vc.createIntentFunc = self.api.createIntent
	vc.statusFunc = self.api.status
	vc.requoteFunc = self.api.requote
	vc.AddCryptoInvoiceListener(self)
	self.t.Cleanup(vc.Close)
	return vc
}

// Started, with the xmr yearly invoice quoted now and its pending status
// answering every poll.
func (self *cryptoInvoiceHarness) startOpenInvoice() {
	self.t.Helper()
	self.api.intentResult = cryptoInvoiceTestIntent(self.clock.now)
	self.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusPending, self.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	self.vc.Start()
	self.vc.step()
	self.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "US")
	self.vc.step()
	connect.AssertEqual(self.t, self.vc.GetState().Phase, CryptoInvoicePhaseOpen)
}

// Starts a network invoice bought without a sign-in on vc, answered at once;
// returns the quote's end in the server's clock.
func (self *cryptoInvoiceHarness) startNetworkInvoice(vc *CryptoInvoiceViewController, intentId string, statusToken string) time.Time {
	self.t.Helper()
	intent := cryptoInvoiceTestNetworkIntent(intentId, statusToken, self.clock.now)
	self.api.intentResult = intent
	vc.StartInvoice(CryptoAssetXmr, "", StripeItemData1Tib, "")
	connect.AssertEqual(self.t, vc.GetState().Phase, CryptoInvoicePhaseOpen)
	expiresAt, err := time.Parse(time.RFC3339, intent.ExpiresAt)
	if err != nil {
		self.t.Fatal(err)
	}
	return expiresAt
}

// Starts a balance-code invoice on vc, answered at once; returns the quote's
// end in the server's clock.
func (self *cryptoInvoiceHarness) startCodeInvoice(vc *CryptoInvoiceViewController, intentId string, statusToken string) time.Time {
	self.t.Helper()
	intent := cryptoInvoiceTestCodeIntent(intentId, statusToken, self.clock.now)
	self.api.intentResult = intent
	vc.StartInvoice(CryptoAssetUsdcSolana, "", StripeItemData1Tib, "")
	connect.AssertEqual(self.t, vc.GetState().Phase, CryptoInvoicePhaseOpen)
	expiresAt, err := time.Parse(time.RFC3339, intent.ExpiresAt)
	if err != nil {
		self.t.Fatal(err)
	}
	return expiresAt
}

func (self *cryptoInvoiceHarness) advance(d time.Duration) {
	self.clock.advance(d)
	self.vc.step()
}

func (self *cryptoInvoiceHarness) statusCount() int {
	return len(self.api.statusCalls)
}

// The network's invoice as saved on disk; nil when there is none.
func (self *cryptoInvoiceHarness) saved() *cryptoInvoiceRecord {
	self.t.Helper()
	return self.savedFile(cryptoInvoiceFileName)
}

// An invoice bought without a sign-in as saved on disk; nil when there is
// none.
func (self *cryptoInvoiceHarness) savedOwned(intentId string) *cryptoInvoiceRecord {
	self.t.Helper()
	return self.savedFile(cryptoInvoiceIntentFileNamePrefix + intentId)
}

func (self *cryptoInvoiceHarness) savedFile(name string) *cryptoInvoiceRecord {
	self.t.Helper()
	data, err := os.ReadFile(filepath.Join(self.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		self.t.Fatal(err)
	}
	record := &cryptoInvoiceRecord{}
	if err := json.Unmarshal(data, record); err != nil {
		self.t.Fatalf("the saved invoice is not json: %v", err)
	}
	return record
}

// ---- start -------------------------------------------------------------------

func TestCryptoInvoiceStartShowsTheQuoteAndSavesIt(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	vc := harness.vc
	connect.AssertEqual(t, vc.GetState().Phase, CryptoInvoicePhaseNone)

	harness.api.holdIntent = true
	vc.Start()
	vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "US")
	state := vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseStarting)
	connect.AssertEqual(t, state.Asset, CryptoAssetXmr)
	connect.AssertEqual(t, state.Ticker, "XMR")
	connect.AssertEqual(t, len(harness.api.intentCalls), 1)
	connect.AssertEqual(t, *harness.api.intentCalls[0], CreateCryptoPaymentIntentArgs{
		Asset:             CryptoAssetXmr,
		Plan:              PlanYearly,
		StorefrontCountry: "US",
	})
	if harness.saved() != nil {
		t.Fatal("an invoice was saved before the server quoted it")
	}

	harness.clock.advance(300 * time.Millisecond)
	harness.api.heldIntent[0].Result(cryptoInvoiceTestIntent(harness.clock.now), nil)
	state = vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.IntentId, cryptoPayTestIntentId)
	connect.AssertEqual(t, state.Plan, PlanYearly)
	connect.AssertEqual(t, state.Delivery, CryptoPaymentDeliveryAccount)
	connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
	connect.AssertEqual(t, state.Amount, "0.072387")
	connect.AssertEqual(t, state.AmountAtomic, "72387000000")
	connect.AssertEqual(t, state.Uri, cryptoPayTestXmrUri)
	connect.AssertEqual(t, state.Memo, "")
	connect.AssertEqual(t, state.PriceUsd, 40.0)
	connect.AssertEqual(t, state.RateUsd, 555.36)
	connect.AssertEqual(t, state.SecondsLeft, 1800)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, state.Remaining, "0.072387")
	connect.AssertEqual(t, state.ConfirmationsRequired, 1)
	connect.AssertEqual(t, state.Refreshing, false)
	connect.AssertEqual(t, state.ErrorCode, "")

	// the status token is the invoice's secret: saved on the device, never
	// handed to the ui
	stateJson, _ := json.Marshal(state)
	if strings.Contains(string(stateJson), cryptoPayTestStatusToken) {
		t.Fatal("the state shows the status token")
	}
	saved := harness.saved()
	if saved == nil {
		t.Fatal("the quoted invoice was not saved")
	}
	connect.AssertEqual(t, *saved, cryptoInvoiceRecord{
		NetworkId:    cryptoInvoiceTestNetworkId,
		IntentId:     cryptoPayTestIntentId,
		StatusToken:  cryptoPayTestStatusToken,
		Asset:        CryptoAssetXmr,
		Plan:         PlanYearly,
		CreatedAt:    "2026-10-20T18:00:00Z",
		Delivery:     CryptoPaymentDeliveryAccount,
		Address:      cryptoPayTestXmrAddress,
		Amount:       "0.072387",
		AmountAtomic: "72387000000",
		Uri:          cryptoPayTestXmrUri,
		PriceUsd:     40,
		RateUsd:      555.36,
		// the quote in the server's clock, and when it arrived here
		QuotedAt:   "2026-10-20T18:00:00Z",
		ExpiresAt:  "2026-10-20T18:30:00Z",
		ReceivedAt: "2026-10-20T18:00:00.300Z",
	})
	info, err := os.Stat(filepath.Join(harness.dir, cryptoInvoiceFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != LocalStorageFilePermissions {
		t.Fatalf("the saved invoice is %v, want %v", info.Mode().Perm(), os.FileMode(LocalStorageFilePermissions))
	}
}

func TestCryptoInvoiceStartFailures(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.api.intentResult = &CryptoPaymentIntentResult{Error: &CryptoPaymentError{
			Code:    PurchaseErrorCodeAssetUnavailable,
			Message: "Monero is temporarily unavailable.",
		}}
		harness.vc.Start()
		harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
		state := harness.vc.GetState()
		connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseFailed)
		connect.AssertEqual(t, state.ErrorCode, PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, state.ErrorMessage, "Monero is temporarily unavailable.")
		if harness.saved() != nil {
			t.Fatal("a refused invoice was saved")
		}
		harness.advance(time.Minute)
		connect.AssertEqual(t, harness.statusCount(), 0)
	})
	t.Run("transport", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.api.intentErr = errors.New("synthetic network down")
		harness.vc.StartInvoice(CryptoAssetZec, "", StripeItemData1Tib, "")
		state := harness.vc.GetState()
		connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseFailed)
		connect.AssertEqual(t, state.ErrorCode, "")
		connect.AssertEqual(t, state.ErrorMessage, "synthetic network down")
	})
	t.Run("no token", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		intent := cryptoInvoiceTestIntent(harness.clock.now)
		intent.StatusToken = ""
		harness.api.intentResult = intent
		harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
		connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseFailed)
		if harness.saved() != nil {
			t.Fatal("an invoice without a token was saved")
		}
	})
	t.Run("an older hidden invoice stays saved", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.startOpenInvoice()
		harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
		harness.advance(5 * time.Second)
		harness.vc.Dismiss()

		harness.api.intentResult = &CryptoPaymentIntentResult{Error: &CryptoPaymentError{Code: PurchaseErrorCodeAssetUnavailable, Message: "No."}}
		harness.vc.StartInvoice(CryptoAssetZec, PlanYearly, "", "")
		connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseFailed)
		if harness.vc.Resume() != true {
			t.Fatal("the hidden invoice was lost to a refused start")
		}
		connect.AssertEqual(t, harness.vc.GetState().IntentId, cryptoPayTestIntentId)
	})
}

// A uri that does not pay the address on screen is never shown.
func TestCryptoInvoiceRefusesAUriThatDoesNotPayTheAddress(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	intent := cryptoInvoiceTestIntent(harness.clock.now)
	intent.Uri = "monero:8BSyntheticXmrSubaddressOther?tx_amount=0.072387"
	harness.api.intentResult = intent
	harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
	connect.AssertEqual(t, state.Uri, "")
	connect.AssertEqual(t, harness.saved().Uri, "")
}

// ---- polling -----------------------------------------------------------------

// 5 s while a payment may be on its way, 30 s once expired, 60 s in review,
// and never once paid.
func TestCryptoInvoicePollCadence(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	expiresAt := harness.clock.now.Add(cryptoInvoiceTestQuoteTtl)
	connect.AssertEqual(t, harness.statusCount(), 0)

	harness.advance(4999 * time.Millisecond)
	connect.AssertEqual(t, harness.statusCount(), 0)
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, expiresAt)
	harness.advance(time.Millisecond)
	connect.AssertEqual(t, harness.statusCount(), 1)
	connect.AssertEqual(t, *harness.api.statusCalls[0], CryptoPaymentStatusArgs{
		IntentId:    cryptoPayTestIntentId,
		StatusToken: cryptoPayTestStatusToken,
	})
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, state.Received, "0.05")
	connect.AssertEqual(t, state.Remaining, "0.022387")

	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusConfirming, expiresAt)
	harness.advance(4999 * time.Millisecond)
	connect.AssertEqual(t, harness.statusCount(), 1)
	harness.advance(time.Millisecond)
	connect.AssertEqual(t, harness.statusCount(), 2)
	connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusConfirming)

	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusExpired, expiresAt)
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 3)
	connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusExpired)

	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusReview, expiresAt)
	harness.advance(29 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 3)
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.statusCount(), 4)
	state = harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusReview)
	connect.AssertEqual(t, state.ReviewReason, "underpaid")

	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusPaid, expiresAt)
	harness.advance(59 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 4)
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.statusCount(), 5)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhasePaid)

	for i := 0; i < 10; i += 1 {
		harness.advance(time.Minute)
	}
	connect.AssertEqual(t, harness.statusCount(), 5)
}

// A failed poll keeps the invoice as it is and asks again on its cadence.
func TestCryptoInvoiceFailedPollRetriesOnItsCadence(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.statusResult = nil
	harness.api.statusErr = errors.New("429 Too many status checks")
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 1)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, state.ErrorCode, "")
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 2)
}

// The countdown ticks once a second, announces each second once, and stops at
// 0.
func TestCryptoInvoiceTicksOnceASecond(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1800)

	changes := harness.changes
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1799)
	connect.AssertEqual(t, harness.changes, changes+1)
	harness.advance(500 * time.Millisecond)
	connect.AssertEqual(t, harness.changes, changes+1)
	// rounded up: 1798.5 s left shows as 1799
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1799)
	harness.advance(500 * time.Millisecond)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1798)
	connect.AssertEqual(t, harness.changes, changes+2)

	harness.clock.now = time.Date(2026, 10, 20, 18, 29, 59, 0, time.UTC)
	harness.vc.step()
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1)
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 0)
	delay, arm := harness.vc.step()
	if !arm || delay != cryptoInvoiceActivePollInterval-time.Second {
		// only the poll is due later: the countdown is done
		t.Fatalf("the next wake is %s (armed %t), want the poll's", delay, arm)
	}
	changes = harness.changes
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.changes, changes)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 0)
}

// Hidden, nothing runs: no poll and no tick, however long. Visible again, it
// polls at once.
func TestCryptoInvoiceNothingRunsWhileStopped(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.vc.Stop()
	changes := harness.changes
	for i := 0; i < 60; i += 1 {
		harness.clock.advance(10 * time.Second)
		if _, arm := harness.vc.step(); arm {
			t.Fatal("a stopped controller armed a wait")
		}
	}
	connect.AssertEqual(t, harness.statusCount(), 0)
	connect.AssertEqual(t, harness.changes, changes)

	harness.vc.Start()
	harness.vc.step()
	connect.AssertEqual(t, harness.statusCount(), 1)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1200)
	if harness.changes == changes {
		t.Fatal("the countdown's new second was not announced")
	}
}

// An answer that was in flight when the screen hid still lands.
func TestCryptoInvoiceAnswerInFlightLandsAfterStop(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.holdStatus = true
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 1)
	harness.vc.Stop()
	harness.api.heldStatus[0].Result(cryptoInvoiceTestStatus(CryptoPaymentStatusConfirming, harness.clock.now.Add(time.Minute)), nil)
	connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusConfirming)
	harness.advance(time.Minute)
	connect.AssertEqual(t, harness.statusCount(), 1)
}

// ---- endings -----------------------------------------------------------------

// Paid is delivered once and then forgotten: no saved invoice and no poll.
func TestCryptoInvoicePaidIsForgottenOnceShown(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusPaid, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	changes := harness.changes
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhasePaid)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPaid)
	connect.AssertEqual(t, state.SecondsLeft, 0)
	if harness.changes == changes {
		t.Fatal("paid was not announced")
	}
	if harness.saved() != nil {
		t.Fatal("a paid invoice stayed saved")
	}
	// on screen until dismissed
	connect.AssertEqual(t, harness.vc.Resume(), true)
	harness.vc.Dismiss()
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
	connect.AssertEqual(t, harness.vc.Resume(), false)
}

// A paid balance code stays saved, in a file of its own, until it is
// dismissed after it was shown, so it can be shown again after a relaunch.
func TestCryptoInvoiceBalanceCodeStaysSavedUntilDismissed(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.vc.Start()
	expiresAt := harness.startCodeInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
	harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoPayTestIntentId, CryptoPaymentStatusPaid, expiresAt)
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhasePaid)
	connect.AssertEqual(t, state.BalanceCode, cryptoInvoiceTestBalanceCode)
	saved := harness.savedOwned(cryptoPayTestIntentId)
	if saved == nil {
		t.Fatal("the paid code was forgotten before it was dismissed")
	}
	connect.AssertEqual(t, saved.Status, CryptoPaymentStatusPaid)
	connect.AssertEqual(t, saved.BalanceCode, cryptoInvoiceTestBalanceCode)
	if harness.saved() != nil {
		t.Fatal("a code invoice was saved in the network's slot")
	}

	// shown again at once after a relaunch, signed in or not, without a poll
	harness.networkId = ""
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.Resume(), true)
	state = relaunched.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhasePaid)
	connect.AssertEqual(t, state.BalanceCode, cryptoInvoiceTestBalanceCode)
	connect.AssertEqual(t, harness.statusCount(), 1)
	relaunched.Dismiss()
	if harness.savedOwned(cryptoPayTestIntentId) != nil {
		t.Fatal("the dismissed code stayed saved")
	}
}

// The server no longer knows the invoice: it fails, and is forgotten.
func TestCryptoInvoiceUnknownIsForgotten(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusUnknown, time.Time{})
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseFailed)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusUnknown)
	if harness.saved() != nil {
		t.Fatal("an unknown invoice stayed saved")
	}
	harness.advance(time.Minute)
	connect.AssertEqual(t, harness.statusCount(), 1)
}

// ---- dismiss and resume --------------------------------------------------------

func TestCryptoInvoiceDismissWithNothingReceivedForgetsIt(t *testing.T) {
	for _, status := range []string{CryptoPaymentStatusPending, CryptoPaymentStatusExpired} {
		t.Run(status, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.startOpenInvoice()
			harness.api.statusResult = cryptoInvoiceTestStatus(status, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
			harness.advance(5 * time.Second)
			harness.vc.Dismiss()
			connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
			if harness.saved() != nil {
				t.Fatal("a dismissed unpaid invoice stayed saved")
			}
			connect.AssertEqual(t, harness.vc.Resume(), false)
			harness.advance(time.Minute)
			connect.AssertEqual(t, harness.statusCount(), 1)
		})
	}
}

// Once money arrived, Dismiss only hides the invoice: Resume brings it back,
// in this controller or after a relaunch, and polls it at once.
func TestCryptoInvoiceDismissAfterMoneyArrivedOnlyHidesIt(t *testing.T) {
	for _, status := range []string{CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusReview} {
		t.Run(status, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.startOpenInvoice()
			harness.api.statusResult = cryptoInvoiceTestStatus(status, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
			harness.advance(5 * time.Second)
			harness.vc.Dismiss()
			connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
			if harness.saved() == nil {
				t.Fatal("dismissing an invoice money arrived for forgot it")
			}
			harness.advance(time.Minute)
			connect.AssertEqual(t, harness.statusCount(), 1)

			connect.AssertEqual(t, harness.vc.Resume(), true)
			state := harness.vc.GetState()
			connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
			connect.AssertEqual(t, state.Status, "")
			connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
			connect.AssertEqual(t, state.Uri, cryptoPayTestXmrUri)
			harness.vc.step()
			connect.AssertEqual(t, harness.statusCount(), 2)
			connect.AssertEqual(t, harness.vc.GetState().Status, status)

			relaunched := harness.newController()
			connect.AssertEqual(t, relaunched.Resume(), true)
			connect.AssertEqual(t, relaunched.GetState().IntentId, cryptoPayTestIntentId)
			// a resumed invoice with no status yet may have money on it
			relaunched.Dismiss()
			if harness.saved() == nil {
				t.Fatal("dismissing a resumed invoice before its first poll forgot it")
			}
		})
	}
}

func TestCryptoInvoiceResumeReadsOnlyTheSignedInNetwork(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()

	harness.networkId = cryptoInvoiceTestOtherNetworkId
	connect.AssertEqual(t, harness.newController().Resume(), false)
	harness.networkId = ""
	connect.AssertEqual(t, harness.newController().Resume(), false)
	harness.networkId = cryptoInvoiceTestNetworkId
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.Resume(), true)
	state := relaunched.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.IntentId, cryptoPayTestIntentId)
	connect.AssertEqual(t, state.Amount, "0.072387")
	connect.AssertEqual(t, state.Ticker, "XMR")
	connect.AssertEqual(t, state.SecondsLeft, 1800)
}

func TestCryptoInvoiceResumeDropsAWeekOldInvoice(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()

	harness.clock.advance(cryptoInvoiceMaxAge)
	connect.AssertEqual(t, harness.newController().Resume(), true)
	harness.clock.advance(time.Second)
	connect.AssertEqual(t, harness.newController().Resume(), false)
	if harness.saved() != nil {
		t.Fatal("a week-old invoice stayed saved")
	}
}

func TestCryptoInvoiceResumeIgnoresAnUnreadableFile(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(harness.dir, cryptoInvoiceFileName), []byte("{not json"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, harness.vc.Resume(), false)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
}

// The answer to a start that was dismissed is dropped.
func TestCryptoInvoiceDismissWhileStartingDropsTheAnswer(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.api.holdIntent = true
	harness.vc.Start()
	harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	harness.vc.Dismiss()
	harness.api.heldIntent[0].Result(cryptoInvoiceTestIntent(harness.clock.now), nil)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
	if harness.saved() != nil {
		t.Fatal("a dismissed start was saved")
	}
	harness.advance(time.Minute)
	connect.AssertEqual(t, harness.statusCount(), 0)
}

// An answer that lands after a sign-out never writes the invoice back.
func TestCryptoInvoiceNeverSavesForANetworkThatIsNotSignedIn(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	// a sign-out clears the directory and the credential
	if err := os.Remove(filepath.Join(harness.dir, cryptoInvoiceFileName)); err != nil {
		t.Fatal(err)
	}
	harness.networkId = ""
	// an answer that changes what is saved: the quote changed elsewhere
	harness.api.statusResult = cryptoInvoiceTestRequote(harness.clock.now).Status
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.vc.GetState().Amount, "0.071111")
	if harness.saved() != nil {
		t.Fatal("the invoice was written back after a sign-out")
	}
}

// Without a directory the invoice lives in memory: Resume still brings a
// hidden one back.
func TestCryptoInvoiceWithoutStorageKeepsTheInvoiceInMemory(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, "")
	harness.startOpenInvoice()
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	harness.advance(5 * time.Second)
	harness.vc.Dismiss()
	connect.AssertEqual(t, harness.vc.Resume(), true)
	connect.AssertEqual(t, harness.vc.GetState().IntentId, cryptoPayTestIntentId)
	// a new controller has a memory of its own
	connect.AssertEqual(t, harness.newController().Resume(), false)
}

// ---- requote -----------------------------------------------------------------

func cryptoInvoiceTestRequote(now time.Time) *CryptoPaymentIntentResult {
	intent := cryptoInvoiceTestIntent(now)
	intent.Amount, intent.AmountAtomic = "0.071111", "71111000000"
	intent.RateUsd = 562.5
	intent.Uri = "monero:" + cryptoPayTestXmrAddress + "?tx_amount=0.071111&tx_description=URnetwork%20Pro%20yearly"
	intent.Status.Amount, intent.Status.AmountAtomic = intent.Amount, intent.AmountAtomic
	intent.Status.Remaining, intent.Status.RemainingAtomic = intent.Amount, intent.AmountAtomic
	return intent
}

func TestCryptoInvoiceRequote(t *testing.T) {
	for _, status := range []string{CryptoPaymentStatusPending, CryptoPaymentStatusExpired} {
		t.Run(status, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.startOpenInvoice()
			harness.api.statusResult = cryptoInvoiceTestStatus(status, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
			harness.clock.advance(31 * time.Minute)
			harness.vc.step()
			connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 0)

			harness.api.holdRequote = true
			changes := harness.changes
			harness.vc.Requote()
			connect.AssertEqual(t, harness.vc.GetState().Refreshing, true)
			connect.AssertEqual(t, harness.changes, changes+1)
			connect.AssertEqual(t, *harness.api.requoteCalls[0], CryptoPaymentStatusArgs{
				IntentId:    cryptoPayTestIntentId,
				StatusToken: cryptoPayTestStatusToken,
			})
			// one at a time, and no poll while it is in flight
			harness.vc.Requote()
			connect.AssertEqual(t, len(harness.api.requoteCalls), 1)
			polls := harness.statusCount()
			harness.advance(time.Minute)
			connect.AssertEqual(t, harness.statusCount(), polls)

			requote := cryptoInvoiceTestRequote(harness.clock.now)
			harness.api.statusResult = requote.Status
			harness.api.heldRequote[0].Result(requote, nil)
			state := harness.vc.GetState()
			connect.AssertEqual(t, state.Refreshing, false)
			connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
			connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
			connect.AssertEqual(t, state.Amount, "0.071111")
			connect.AssertEqual(t, state.RateUsd, 562.5)
			connect.AssertEqual(t, state.SecondsLeft, 1800)
			connect.AssertEqual(t, state.Uri, "monero:"+cryptoPayTestXmrAddress+"?tx_amount=0.071111&tx_description=URnetwork%20Pro%20yearly")
			saved := harness.saved()
			connect.AssertEqual(t, saved.Amount, "0.071111")
			connect.AssertEqual(t, saved.ExpiresAt, cryptoInvoiceTestTime(harness.clock.now.Add(cryptoInvoiceTestQuoteTtl)))
			// the invoice is as old as it was
			connect.AssertEqual(t, saved.CreatedAt, "2026-10-20T18:00:00Z")

			// the new quote is polled at once, and the countdown and the polls
			// start over from it
			harness.vc.step()
			connect.AssertEqual(t, harness.statusCount(), polls+1)
			connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusPending)
			harness.advance(time.Second)
			connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1799)
			harness.advance(4 * time.Second)
			connect.AssertEqual(t, harness.statusCount(), polls+2)
		})
	}
}

func TestCryptoInvoiceRequoteOnlyWithNothingReceived(t *testing.T) {
	for _, status := range []string{CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusReview} {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.startOpenInvoice()
		harness.api.statusResult = cryptoInvoiceTestStatus(status, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
		harness.advance(5 * time.Second)
		harness.vc.Requote()
		connect.AssertEqual(t, len(harness.api.requoteCalls), 0)
		connect.AssertEqual(t, harness.vc.GetState().Refreshing, false)
	}
}

func TestCryptoInvoiceRequoteFailureKeepsTheInvoice(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.startOpenInvoice()
		harness.api.requoteResult = &CryptoPaymentIntentResult{Error: &CryptoPaymentError{
			Code:    PurchaseErrorCodeAssetUnavailable,
			Message: "Monero is temporarily unavailable.",
		}}
		harness.vc.Requote()
		state := harness.vc.GetState()
		connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
		connect.AssertEqual(t, state.Refreshing, false)
		connect.AssertEqual(t, state.ErrorCode, PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, state.ErrorMessage, "Monero is temporarily unavailable.")
		connect.AssertEqual(t, state.Amount, "0.072387")
		connect.AssertEqual(t, state.Uri, cryptoPayTestXmrUri)

		// the next try clears the error
		harness.api.requoteResult = cryptoInvoiceTestRequote(harness.clock.now)
		harness.vc.Requote()
		connect.AssertEqual(t, harness.vc.GetState().ErrorCode, "")
		connect.AssertEqual(t, harness.vc.GetState().Amount, "0.071111")
	})
	t.Run("transport", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.startOpenInvoice()
		harness.api.requoteErr = errors.New("synthetic network down")
		harness.vc.Requote()
		state := harness.vc.GetState()
		connect.AssertEqual(t, state.Refreshing, false)
		connect.AssertEqual(t, state.ErrorCode, "")
		connect.AssertEqual(t, state.ErrorMessage, "synthetic network down")
		connect.AssertEqual(t, state.Amount, "0.072387")
	})
}

// A poll sent for the old quote cannot undo the new one.
func TestCryptoInvoiceRequoteDropsAStalePoll(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	staleStatus := cryptoInvoiceTestStatus(CryptoPaymentStatusPending, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	harness.api.holdStatus = true
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 1)

	harness.api.requoteResult = cryptoInvoiceTestRequote(harness.clock.now)
	harness.vc.Requote()
	connect.AssertEqual(t, harness.vc.GetState().Amount, "0.071111")
	harness.api.heldStatus[0].Result(staleStatus, nil)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Amount, "0.071111")
	connect.AssertEqual(t, state.Uri, "monero:"+cryptoPayTestXmrAddress+"?tx_amount=0.071111&tx_description=URnetwork%20Pro%20yearly")
}

// Another device requoted this network's invoice: the uri asks for the old
// amount, so it is dropped and the new amount shown.
func TestCryptoInvoiceAQuoteChangedElsewhereDropsTheUri(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	requoted := cryptoInvoiceTestRequote(harness.clock.now.Add(2 * time.Minute)).Status
	harness.api.statusResult = requoted
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Amount, "0.071111")
	connect.AssertEqual(t, state.AmountAtomic, "71111000000")
	connect.AssertEqual(t, state.Uri, "")
	connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
	connect.AssertEqual(t, state.ExpiresAt, requoted.ExpiresAt)
	connect.AssertEqual(t, state.SecondsLeft, 1915)
	saved := harness.saved()
	connect.AssertEqual(t, saved.Amount, "0.071111")
	connect.AssertEqual(t, saved.Uri, "")
}

// ---- payment method ------------------------------------------------------------

func TestCryptoInvoicePaymentMethodIsRememberedPerDevice(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	connect.AssertEqual(t, harness.vc.GetPaymentMethod(), "")
	harness.vc.SetPaymentMethod(CryptoAssetXmr)
	connect.AssertEqual(t, harness.vc.GetPaymentMethod(), CryptoAssetXmr)
	path := filepath.Join(harness.dir, cryptoPaymentMethodFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(data), `"xmr"`)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, info.Mode().Perm(), os.FileMode(LocalStorageFilePermissions))

	// read again after a relaunch; a method the sdk does not know is ignored
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.GetPaymentMethod(), CryptoAssetXmr)
	relaunched.SetPaymentMethod("btc")
	connect.AssertEqual(t, relaunched.GetPaymentMethod(), CryptoAssetXmr)
	relaunched.SetPaymentMethod(PaymentMethodCard)
	connect.AssertEqual(t, harness.vc.GetPaymentMethod(), PaymentMethodCard)
	relaunched.SetPaymentMethod("")
	connect.AssertEqual(t, relaunched.GetPaymentMethod(), "")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a forgotten method left its file: %v", err)
	}

	// a file written by something else that is not a method reads as none
	if err := os.WriteFile(path, []byte(`"btc"`), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, relaunched.GetPaymentMethod(), "")

	memory := newCryptoInvoiceHarness(t, "")
	memory.vc.SetPaymentMethod(CryptoAssetZec)
	connect.AssertEqual(t, memory.vc.GetPaymentMethod(), CryptoAssetZec)
}

// A directory that cannot be written costs the saving, never the invoice.
func TestCryptoInvoiceUnwritableStorageCostsOnlyTheSaving(t *testing.T) {
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, []byte("x"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	harness := newCryptoInvoiceHarness(t, notADirectory)
	harness.startOpenInvoice()
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusPaid, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhasePaid)
	harness.vc.Dismiss()
	connect.AssertEqual(t, harness.vc.Resume(), false)
	harness.vc.SetPaymentMethod(CryptoAssetXmr)
	connect.AssertEqual(t, harness.vc.GetPaymentMethod(), "")
}

// A closed controller drops the answer still in flight.
func TestCryptoInvoiceCloseDropsAnswersInFlight(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.holdStatus = true
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.statusCount(), 1)
	harness.vc.Close()
	changes := harness.changes
	harness.api.heldStatus[0].Result(cryptoInvoiceTestStatus(CryptoPaymentStatusPaid, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl)), nil)
	connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, harness.changes, changes)
	if harness.saved() == nil {
		t.Fatal("Close dropped the saved invoice")
	}
}

// A sign-out clears the saved invoice and the payment method with the rest of
// the account's local state.
func TestCryptoInvoiceSignOutClearsTheSavedInvoiceAndMethod(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	harness := newCryptoInvoiceHarness(t, localState.localStorageDir)
	harness.startOpenInvoice()
	harness.vc.SetPaymentMethod(CryptoAssetZec)
	if harness.saved() == nil {
		t.Fatal("the invoice was not saved")
	}

	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{cryptoInvoiceFileName, cryptoPaymentMethodFileName} {
		if _, err := os.Stat(filepath.Join(localState.localStorageDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sign-out: %v", name, err)
		}
	}
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.Resume(), false)
	connect.AssertEqual(t, relaunched.GetPaymentMethod(), "")
}

// ---- loop and lifecycle ----------------------------------------------------------

type cryptoInvoiceTestWait struct {
	delay time.Duration
	fire  chan time.Time
}

// The real loop with its waits in the test's hands: Start wakes it into a
// poll, the answer arms a wait for the next one, the wait polls again, a
// stopped controller arms no wait, and Close ends the loop. The server says
// the quote expired, which ends the countdown, so no tick arms a wait beside
// the polls. Each step is
// awaited on the loop's own calls, never on a sleep; the tests above cover
// each decision step makes.
func TestCryptoInvoiceViewControllerRunLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quotedAt := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	var nowUnixNano atomic.Int64
	nowUnixNano.Store(quotedAt.Add(cryptoInvoiceTestQuoteTtl + time.Minute).UnixNano())
	vc := newCryptoInvoiceViewControllerWithoutRun(ctx, nil, "")
	vc.nowFunc = func() time.Time {
		return time.Unix(0, nowUnixNano.Load())
	}
	vc.networkIdFunc = func() string { return cryptoInvoiceTestNetworkId }
	vc.createIntentFunc = func(args *CreateCryptoPaymentIntentArgs, callback CryptoPaymentIntentCallback) {
		intent := cryptoInvoiceTestIntent(quotedAt)
		intent.Status = cryptoInvoiceTestStatus(CryptoPaymentStatusExpired, quotedAt.Add(cryptoInvoiceTestQuoteTtl))
		callback.Result(intent, nil)
	}
	statusCallbacks := make(chan CryptoPaymentStatusCallback, 8)
	vc.statusFunc = func(args *CryptoPaymentStatusArgs, callback CryptoPaymentStatusCallback) {
		statusCallbacks <- callback
	}
	waits := make(chan cryptoInvoiceTestWait, 8)
	vc.afterFunc = func(delay time.Duration) <-chan time.Time {
		wait := cryptoInvoiceTestWait{delay: delay, fire: make(chan time.Time, 1)}
		waits <- wait
		return wait.fire
	}
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		vc.run()
	}()
	nextPoll := func(message string) CryptoPaymentStatusCallback {
		t.Helper()
		select {
		case callback := <-statusCallbacks:
			return callback
		case <-time.After(10 * time.Second):
			t.Fatal(message)
			return nil
		}
	}
	nextWait := func(message string) cryptoInvoiceTestWait {
		t.Helper()
		select {
		case wait := <-waits:
			return wait
		case <-time.After(10 * time.Second):
			t.Fatal(message)
			return cryptoInvoiceTestWait{}
		}
	}
	expired := cryptoInvoiceTestStatus(CryptoPaymentStatusExpired, quotedAt.Add(cryptoInvoiceTestQuoteTtl))

	vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	vc.Start()
	nextPoll("Start did not poll").Result(expired, nil)
	wait := nextWait("the answer armed no wait for the next poll")
	if wait.delay != cryptoInvoiceExpiredPollInterval {
		t.Fatalf("the next poll waits %s, want %s", wait.delay, cryptoInvoiceExpiredPollInterval)
	}

	nowUnixNano.Add(int64(cryptoInvoiceExpiredPollInterval))
	wait.fire <- time.Unix(0, nowUnixNano.Load())
	inFlight := nextPoll("the due wait did not poll")

	// stopped, the late answer lands and nothing waits; Start polls at once,
	// and a wait armed while stopped would be queued ahead of that poll
	vc.Stop()
	inFlight.Result(expired, nil)
	vc.Start()
	nextPoll("Start after Stop did not poll")
	select {
	case wait := <-waits:
		t.Fatalf("a wait of %s was armed while stopped", wait.delay)
	default:
	}

	vc.Close()
	select {
	case <-loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the loop")
	}
}

// Opened through the device, the controller saves in the device's
// local-state directory and is released by its close.
func TestOpenCryptoInvoiceViewControllerUsesTheLocalStateDirectory(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)
	vc := device.OpenCryptoInvoiceViewController()
	if vc.storage.dir == "" {
		t.Fatal("the device's controller keeps its invoice in memory only")
	}
	connect.AssertEqual(t, vc.storage.dir, device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().localStorageDir)
	device.CloseCryptoInvoiceViewController(vc)
	requireNoOwnedViewControllers(t, device)
}

// ---- the pre-merge review (mmm/ZCASH-CONTRACT.md §4.4.2) ------------------------------

// The Dismiss and countdown vectors of js/test/crypto_pay_vectors.json, which
// js/test/crypto_invoice.test.ts replays against the TypeScript twin.
type cryptoInvoiceVectors struct {
	InvoiceDismiss []struct {
		Name     string `json:"name"`
		Delivery string `json:"delivery"`
		Status   string `json:"status"`
		Expected string `json:"expected"`
	} `json:"invoiceDismiss"`
	InvoiceCountdown []struct {
		Name       string `json:"name"`
		QuotedAt   string `json:"quoted_at"`
		ExpiresAt  string `json:"expires_at"`
		ReceivedAt string `json:"received_at"`
		AfterMs    int64  `json:"after_ms"`
		Expected   int    `json:"expected"`
	} `json:"invoiceCountdown"`
}

func loadCryptoInvoiceVectors(t *testing.T) *cryptoInvoiceVectors {
	t.Helper()
	data, err := os.ReadFile("js/test/crypto_pay_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := &cryptoInvoiceVectors{}
	if err := json.Unmarshal(data, vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.InvoiceDismiss) == 0 || len(vectors.InvoiceCountdown) == 0 {
		t.Fatal("the invoice vectors were not read")
	}
	return vectors
}

const (
	cryptoInvoiceTestOtherIntentId    = "0b6f0b6f-0000-4000-8000-0000000000ff"
	cryptoInvoiceTestOtherStatusToken = "kq3-synthetic-status-token-other"
)

// §4.4.2.1: Dismiss drops an account or network invoice with nothing
// received, and a code invoice only once it is paid and its code was shown.
// Otherwise an account invoice is kept for the next Resume, and one bought
// without a sign-in (network or code) is hidden.
func TestCryptoInvoiceDismissRules(t *testing.T) {
	for _, vector := range loadCryptoInvoiceVectors(t).InvoiceDismiss {
		t.Run(vector.Name, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.vc.Start()
			var saved func() *cryptoInvoiceRecord
			switch vector.Delivery {
			case CryptoPaymentDeliveryCode:
				expiresAt := harness.startCodeInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
				harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoPayTestIntentId, vector.Status, expiresAt)
				saved = func() *cryptoInvoiceRecord { return harness.savedOwned(cryptoPayTestIntentId) }
			case CryptoPaymentDeliveryNetwork:
				expiresAt := harness.startNetworkInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
				harness.api.statusResult = cryptoInvoiceTestNetworkStatus(cryptoPayTestIntentId, vector.Status, expiresAt)
				saved = func() *cryptoInvoiceRecord { return harness.savedOwned(cryptoPayTestIntentId) }
			default:
				harness.api.intentResult = cryptoInvoiceTestIntent(harness.clock.now)
				harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
				harness.api.statusResult = cryptoInvoiceTestStatus(vector.Status, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
				saved = harness.saved
			}
			harness.advance(5 * time.Second)
			connect.AssertEqual(t, harness.vc.GetState().Status, vector.Status)
			if saved() == nil {
				t.Fatal("the invoice was not saved before the Dismiss")
			}

			harness.vc.Dismiss()
			connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
			record := saved()
			switch vector.Expected {
			case "drop":
				if record != nil {
					t.Fatal("the dismissed invoice stayed saved")
				}
			case "keep":
				if record == nil || record.Hidden {
					t.Fatalf("the dismissed account invoice was not kept as it was: %+v", record)
				}
				connect.AssertEqual(t, harness.newController().Resume(), true)
			case "hide":
				if record == nil {
					t.Fatal("the dismissed invoice was dropped; it may still pay")
				}
				connect.AssertEqual(t, record.Hidden, true)
			default:
				t.Fatalf("unknown expectation %q", vector.Expected)
			}
		})
	}
}

// §4.4.2.1: a code invoice dismissed while its status still read pending (the
// buyer had sent USDC; USDC goes from pending straight to paid) is polled once
// at the next Resume, and shown with its code when it turns out paid.
func TestCryptoInvoiceHiddenCodeInvoiceIsCheckedOnceAndShownWhenPaid(t *testing.T) {
	for _, answer := range []string{CryptoPaymentStatusPaid, CryptoPaymentStatusPending, CryptoPaymentStatusUnknown} {
		t.Run(answer, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.networkId = ""
			harness.vc.Start()
			expiresAt := harness.startCodeInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
			harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoPayTestIntentId, CryptoPaymentStatusPending, expiresAt)
			harness.advance(5 * time.Second)
			harness.vc.Dismiss()
			connect.AssertEqual(t, harness.savedOwned(cryptoPayTestIntentId).Hidden, true)

			harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoPayTestIntentId, answer, expiresAt)
			relaunched := harness.newController()
			polls := harness.statusCount()
			shown := relaunched.Resume()
			connect.AssertEqual(t, harness.statusCount(), polls+1)
			connect.AssertEqual(t, *harness.api.statusCalls[polls], CryptoPaymentStatusArgs{
				IntentId:    cryptoPayTestIntentId,
				StatusToken: cryptoPayTestStatusToken,
			})
			state := relaunched.GetState()
			saved := harness.savedOwned(cryptoPayTestIntentId)
			switch answer {
			case CryptoPaymentStatusPaid:
				connect.AssertEqual(t, shown, true)
				connect.AssertEqual(t, state.Phase, CryptoInvoicePhasePaid)
				connect.AssertEqual(t, state.BalanceCode, cryptoInvoiceTestBalanceCode)
				connect.AssertEqual(t, saved.Status, CryptoPaymentStatusPaid)
				connect.AssertEqual(t, saved.BalanceCode, cryptoInvoiceTestBalanceCode)
				connect.AssertEqual(t, saved.Hidden, false)
			case CryptoPaymentStatusPending:
				// still hidden, still saved: it may yet pay
				connect.AssertEqual(t, shown, false)
				connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseNone)
				connect.AssertEqual(t, saved.Hidden, true)
			case CryptoPaymentStatusUnknown:
				connect.AssertEqual(t, shown, false)
				if saved != nil {
					t.Fatal("an invoice the server no longer knows stayed saved")
				}
			}
			// once per Resume, not on a cadence
			relaunched.Start()
			for i := 0; i < 12; i += 1 {
				harness.clock.advance(5 * time.Second)
				relaunched.step()
			}
			connect.AssertEqual(t, harness.statusCount(), polls+1)
		})
	}
}

// §4.4.2.2: each code invoice has a record of its own, so two controllers on
// one directory (two windows, a relaunch beside an old one) never overwrite
// each other's, and Resume shows a paid one first.
func TestCryptoInvoiceCodeInvoicesAreSavedPerIntent(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.networkId = ""
	vc1 := harness.vc
	vc1.Start()
	expiresAt := harness.startCodeInvoice(vc1, cryptoPayTestIntentId, cryptoPayTestStatusToken)
	vc2 := harness.newController()
	harness.startCodeInvoice(vc2, cryptoInvoiceTestOtherIntentId, cryptoInvoiceTestOtherStatusToken)
	if harness.savedOwned(cryptoPayTestIntentId) == nil || harness.savedOwned(cryptoInvoiceTestOtherIntentId) == nil {
		t.Fatal("one code invoice replaced the other")
	}

	harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoPayTestIntentId, CryptoPaymentStatusPaid, expiresAt)
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, vc1.GetState().BalanceCode, cryptoInvoiceTestBalanceCode)
	connect.AssertEqual(t, harness.savedOwned(cryptoPayTestIntentId).Status, CryptoPaymentStatusPaid)
	connect.AssertEqual(t, harness.savedOwned(cryptoInvoiceTestOtherIntentId).Status, CryptoPaymentStatusPending)

	// a relaunch shows the paid one first, and checks the other once
	harness.api.statusResult = cryptoInvoiceTestCodeStatus(cryptoInvoiceTestOtherIntentId, CryptoPaymentStatusPending, expiresAt)
	relaunched := harness.newController()
	polls := harness.statusCount()
	connect.AssertEqual(t, relaunched.Resume(), true)
	state := relaunched.GetState()
	connect.AssertEqual(t, state.IntentId, cryptoPayTestIntentId)
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhasePaid)
	connect.AssertEqual(t, state.BalanceCode, cryptoInvoiceTestBalanceCode)
	connect.AssertEqual(t, harness.statusCount(), polls+1)
	connect.AssertEqual(t, harness.api.statusCalls[polls].IntentId, cryptoInvoiceTestOtherIntentId)

	// the shown code is dismissed and dropped; the other comes next
	relaunched.Dismiss()
	if harness.savedOwned(cryptoPayTestIntentId) != nil {
		t.Fatal("the dismissed code stayed saved")
	}
	connect.AssertEqual(t, relaunched.Resume(), true)
	state = relaunched.GetState()
	connect.AssertEqual(t, state.IntentId, cryptoInvoiceTestOtherIntentId)
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.Status, "")
}

// §4.4.2.2: the network's slot is compare-and-delete. One controller's
// answer for its invoice never deletes the invoice another controller saved
// there since.
func TestCryptoInvoiceNetworkSlotIsCompareAndDelete(t *testing.T) {
	for _, ending := range []string{"unknown", "dismiss"} {
		t.Run(ending, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.startOpenInvoice()
			vc1 := harness.vc

			vc2 := harness.newController()
			vc2.Start()
			other := cryptoInvoiceTestIntent(harness.clock.now)
			other.IntentId, other.StatusToken = cryptoInvoiceTestOtherIntentId, cryptoInvoiceTestOtherStatusToken
			other.Status.IntentId = cryptoInvoiceTestOtherIntentId
			harness.api.intentResult = other
			vc2.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
			connect.AssertEqual(t, harness.saved().IntentId, cryptoInvoiceTestOtherIntentId)

			switch ending {
			case "unknown":
				harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusUnknown, time.Time{})
				harness.clock.advance(5 * time.Second)
				vc1.step()
				connect.AssertEqual(t, vc1.GetState().Phase, CryptoInvoicePhaseFailed)
			case "dismiss":
				// nothing received on vc1's invoice: Dismiss drops it, but only
				// from where it still is
				vc1.Dismiss()
			}
			saved := harness.saved()
			if saved == nil || saved.IntentId != cryptoInvoiceTestOtherIntentId {
				t.Fatalf("vc1's ending deleted vc2's invoice: %+v", saved)
			}

			// vc2's invoice receives money and stays saved for a relaunch
			seen := cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
			seen.IntentId = cryptoInvoiceTestOtherIntentId
			harness.api.statusResult = seen
			harness.clock.advance(5 * time.Second)
			vc2.step()
			connect.AssertEqual(t, vc2.GetState().Status, CryptoPaymentStatusSeen)
			relaunched := harness.newController()
			connect.AssertEqual(t, relaunched.Resume(), true)
			connect.AssertEqual(t, relaunched.GetState().IntentId, cryptoInvoiceTestOtherIntentId)
		})
	}
}

// §4.4.2.1: a saved code invoice is dropped at the 7-day cap, unpolled.
func TestCryptoInvoiceCodeInvoiceSevenDayCap(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startCodeInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
	harness.vc.Dismiss()
	connect.AssertEqual(t, harness.savedOwned(cryptoPayTestIntentId).Hidden, true)

	harness.clock.advance(cryptoInvoiceMaxAge + time.Second)
	polls := harness.statusCount()
	connect.AssertEqual(t, harness.newController().Resume(), false)
	if harness.savedOwned(cryptoPayTestIntentId) != nil {
		t.Fatal("a week-old code invoice stayed saved")
	}
	connect.AssertEqual(t, harness.statusCount(), polls)
}

// §4.4.2.3: the countdown runs the quote's own duration from when the quote
// arrived, whatever the device clock says about the server's.
func TestCryptoInvoiceCountdownDoesNotTrustTheDeviceClock(t *testing.T) {
	for _, vector := range loadCryptoInvoiceVectors(t).InvoiceCountdown {
		t.Run(vector.Name, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			receivedAt, err := time.Parse(time.RFC3339, vector.ReceivedAt)
			if err != nil {
				t.Fatal(err)
			}
			harness.clock.now = receivedAt
			intent := cryptoInvoiceTestIntent(receivedAt)
			intent.QuotedAt, intent.ExpiresAt = vector.QuotedAt, vector.ExpiresAt
			intent.Status.ExpiresAt = vector.ExpiresAt
			harness.api.intentResult = intent
			harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
			harness.clock.advance(time.Duration(vector.AfterMs) * time.Millisecond)
			connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, vector.Expected)
			saved := harness.saved()
			connect.AssertEqual(t, saved.QuotedAt, vector.QuotedAt)
			connect.AssertEqual(t, saved.ReceivedAt, receivedAt.UTC().Format(cryptoInvoiceLocalTimeLayout))
		})
	}
}

// §4.4.2.3: the arrival time is saved, so a resumed countdown keeps the
// quote's duration, and a quote moved elsewhere moves by the server's own
// difference.
func TestCryptoInvoiceResumedCountdownKeepsTheQuoteDuration(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	// the device clock runs 31 minutes fast
	serverNow := harness.clock.now
	harness.clock.now = serverNow.Add(31 * time.Minute)
	harness.api.intentResult = cryptoInvoiceTestIntent(serverNow)
	harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1800)

	harness.clock.advance(10 * time.Minute)
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.Resume(), true)
	connect.AssertEqual(t, relaunched.GetState().SecondsLeft, 1200)

	// requoted on another device: two minutes later in the server's clock
	moved := cryptoInvoiceTestRequote(serverNow.Add(2 * time.Minute)).Status
	harness.api.statusResult = moved
	relaunched.Start()
	relaunched.step()
	connect.AssertEqual(t, relaunched.GetState().SecondsLeft, 1320)
}

// The server's expired ends the countdown, whatever is left of it here: an
// open invoice handed out again keeps its first quote time, so its count from
// arrival runs long.
func TestCryptoInvoiceServerExpiredEndsTheCountdown(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.api.intentResult = cryptoInvoiceTestIntent(harness.clock.now.Add(-10 * time.Minute))
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusPending, harness.clock.now.Add(20*time.Minute))
	harness.vc.Start()
	harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 1800)

	harness.clock.advance(20*time.Minute - 5*time.Second)
	harness.vc.step()
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 605)
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusExpired, harness.clock.now.Add(5*time.Second))
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, state.SecondsLeft, 0)
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.vc.GetState().SecondsLeft, 0)
}

// §4.4.2.4: a poll in flight when a requote starts answers for the old quote
// and is dropped; the new quote is polled as soon as it lands.
func TestCryptoInvoiceRequoteDropsThePollInFlight(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.holdStatus = true
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, len(harness.api.heldStatus), 1)

	harness.api.holdRequote = true
	harness.vc.Requote()
	// the poll answers first, for the old quote: dropped
	harness.api.heldStatus[0].Result(cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl)), nil)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, state.Refreshing, true)

	// the requote lands, and its quote is polled at once
	requote := cryptoInvoiceTestRequote(harness.clock.now)
	harness.api.holdStatus = false
	harness.api.statusResult = cryptoInvoiceTestStatus(CryptoPaymentStatusSeen, harness.clock.now.Add(cryptoInvoiceTestQuoteTtl))
	harness.api.statusResult.Amount, harness.api.statusResult.AmountAtomic = requote.Amount, requote.AmountAtomic
	harness.api.heldRequote[0].Result(requote, nil)
	connect.AssertEqual(t, harness.vc.GetState().Amount, "0.071111")
	polls := harness.statusCount()
	harness.vc.step()
	connect.AssertEqual(t, harness.statusCount(), polls+1)
	connect.AssertEqual(t, harness.vc.GetState().Status, CryptoPaymentStatusSeen)
}

// §4.4.2.4: a requote's answer never moves a status past pending or expired
// back to pending.
func TestCryptoInvoiceRequoteNeverMakesReceivedMoneyPayable(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.api.holdRequote = true
	harness.vc.Requote()
	// money seen by now, however that reached the state
	func() {
		harness.vc.stateLock.Lock()
		defer harness.vc.stateLock.Unlock()
		harness.vc.state.Status = CryptoPaymentStatusSeen
	}()
	harness.api.heldRequote[0].Result(cryptoInvoiceTestRequote(harness.clock.now), nil)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Refreshing, false)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusSeen)
}

// §4.4.2.5: once Close returns nothing is written, so a sign-out that closes
// the controller and then clears the directory leaves it clear. An answer
// being applied when Close is called finishes first.
func TestCryptoInvoiceNothingIsWrittenAfterClose(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	vc := harness.vc

	entered := make(chan struct{})
	release := make(chan struct{})
	var armed atomic.Bool
	vc.networkIdFunc = func() string {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return harness.networkId
	}
	// an answer that changes what is saved: the quote changed elsewhere
	harness.api.statusResult = cryptoInvoiceTestRequote(harness.clock.now).Status
	armed.Store(true)
	stepped := make(chan struct{})
	go func() {
		defer close(stepped)
		harness.clock.advance(5 * time.Second)
		vc.step()
	}()
	<-entered
	signedOut := make(chan struct{})
	go func() {
		defer close(signedOut)
		vc.Close()
		// the sign-out clears the directory once the controller is closed
		if err := os.Remove(filepath.Join(harness.dir, cryptoInvoiceFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	}()
	close(release)
	<-stepped
	<-signedOut
	if harness.saved() != nil {
		t.Fatal("the invoice was written back after Close and the sign-out")
	}

	// a closed controller writes nothing at all
	vc.Dismiss()
	vc.SetPaymentMethod(CryptoAssetXmr)
	vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
	connect.AssertEqual(t, vc.Resume(), false)
	for _, name := range []string{cryptoInvoiceFileName, cryptoPaymentMethodFileName} {
		if _, err := os.Stat(filepath.Join(harness.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written after Close: %v", name, err)
		}
	}
}

// §4.4.2.6: an empty network id is signed out (or a credential that names no
// network, an API key): an invoice is neither saved for it nor resumed.
func TestCryptoInvoiceEmptyNetworkIdNeitherSavesNorResumes(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.networkId = ""
	harness.startOpenInvoice()
	if harness.saved() != nil {
		t.Fatal("an invoice was saved for no network")
	}
	connect.AssertEqual(t, harness.newController().Resume(), false)

	// nor is a leftover saved for no network resumed
	leftover := &cryptoInvoiceRecord{
		IntentId:    cryptoPayTestIntentId,
		StatusToken: cryptoPayTestStatusToken,
		Asset:       CryptoAssetXmr,
		CreatedAt:   cryptoInvoiceTestTime(harness.clock.now),
		Delivery:    CryptoPaymentDeliveryAccount,
	}
	data, _ := json.Marshal(leftover)
	if err := os.WriteFile(filepath.Join(harness.dir, cryptoInvoiceFileName), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, harness.newController().Resume(), false)
}

// §4.4.2.6: Resume checks the network of the invoice it already shows: one
// from before a sign-out, or of another account, is put away; a code invoice
// belongs to no network and stays.
func TestCryptoInvoiceResumePutsAwayAnotherNetworksInvoice(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.networkId = cryptoInvoiceTestOtherNetworkId
	changes := harness.changes
	connect.AssertEqual(t, harness.vc.Resume(), false)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
	if harness.changes == changes {
		t.Fatal("putting the invoice away was not announced")
	}
	// still saved for its own network
	harness.networkId = cryptoInvoiceTestNetworkId
	connect.AssertEqual(t, harness.vc.Resume(), true)
	connect.AssertEqual(t, harness.vc.GetState().IntentId, cryptoPayTestIntentId)

	harness.startCodeInvoice(harness.vc, cryptoInvoiceTestOtherIntentId, cryptoInvoiceTestOtherStatusToken)
	harness.networkId = ""
	connect.AssertEqual(t, harness.vc.Resume(), true)
	connect.AssertEqual(t, harness.vc.GetState().IntentId, cryptoInvoiceTestOtherIntentId)
}

// §4.4.2.7: an answer that cannot be read is no answer. A requote without an
// intent id, an address or an amount leaves the invoice as it was; a start
// without them fails; a status without one reads as "" (not yet known).
func TestCryptoInvoiceMalformedAnswersAreNoAnswer(t *testing.T) {
	for _, name := range []string{"empty", "no intent id", "another intent", "no address", "no amount"} {
		t.Run("requote "+name, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.startOpenInvoice()
			before := *harness.saved()
			requote := cryptoInvoiceTestRequote(harness.clock.now)
			switch name {
			case "empty":
				requote = &CryptoPaymentIntentResult{}
			case "no intent id":
				requote.IntentId = ""
			case "another intent":
				requote.IntentId = cryptoInvoiceTestOtherIntentId
			case "no address":
				requote.Address = ""
			case "no amount":
				requote.AmountAtomic = ""
			}
			harness.api.requoteResult = requote
			harness.vc.Requote()
			state := harness.vc.GetState()
			connect.AssertEqual(t, state.Refreshing, false)
			connect.AssertEqual(t, state.ErrorCode, "")
			connect.AssertEqual(t, state.ErrorMessage, "no quote")
			connect.AssertEqual(t, state.Address, cryptoPayTestXmrAddress)
			connect.AssertEqual(t, state.Amount, "0.072387")
			connect.AssertEqual(t, state.Uri, cryptoPayTestXmrUri)
			connect.AssertEqual(t, *harness.saved(), before)
		})
	}
	t.Run("start without an address", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		intent := cryptoInvoiceTestIntent(harness.clock.now)
		intent.Address = ""
		harness.api.intentResult = intent
		harness.vc.StartInvoice(CryptoAssetXmr, PlanYearly, "", "")
		connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseFailed)
		connect.AssertEqual(t, harness.vc.GetState().ErrorMessage, "no invoice")
		if harness.saved() != nil {
			t.Fatal("an invoice that cannot be paid was saved")
		}
	})
	t.Run("status without a status", func(t *testing.T) {
		harness := newCryptoInvoiceHarness(t, t.TempDir())
		harness.startOpenInvoice()
		harness.api.statusResult = &CryptoPaymentStatusResult{}
		harness.advance(5 * time.Second)
		connect.AssertEqual(t, harness.vc.GetState().Status, "")
		// not yet known counts as money that may have arrived
		harness.vc.Dismiss()
		if harness.saved() == nil {
			t.Fatal("an invoice of unknown status was dropped")
		}
	})
}

// An invoice for a named network bought without a sign-in (a gift, or an
// owner not signed in on this device) belongs to the device: it is saved
// whoever is signed in, and a reload brings it back to see it through.
func TestCryptoInvoiceNetworkInvoiceBoughtWithoutSignInSurvivesAReload(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.networkId = ""
	harness.vc.Start()
	expiresAt := harness.startNetworkInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
	saved := harness.savedOwned(cryptoPayTestIntentId)
	if saved == nil {
		t.Fatal("the network invoice bought without a sign-in was not saved")
	}
	connect.AssertEqual(t, saved.Delivery, CryptoPaymentDeliveryNetwork)
	connect.AssertEqual(t, saved.NetworkName, "alice")
	if harness.saved() != nil {
		t.Fatal("a network invoice was saved in the signed-in network's slot")
	}

	// a reload, signed in to another network by now
	harness.networkId = cryptoInvoiceTestOtherNetworkId
	relaunched := harness.newController()
	relaunched.Start()
	connect.AssertEqual(t, relaunched.Resume(), true)
	state := relaunched.GetState()
	connect.AssertEqual(t, state.IntentId, cryptoPayTestIntentId)
	connect.AssertEqual(t, state.NetworkName, "alice")
	connect.AssertEqual(t, state.Status, "")
	// it stays on screen whoever signs in or out
	harness.networkId = ""
	connect.AssertEqual(t, relaunched.Resume(), true)

	// paid: shown once, then done, its data on the network
	harness.api.statusResult = cryptoInvoiceTestNetworkStatus(cryptoPayTestIntentId, CryptoPaymentStatusPaid, expiresAt)
	relaunched.step()
	connect.AssertEqual(t, relaunched.GetState().Phase, CryptoInvoicePhasePaid)
	if harness.savedOwned(cryptoPayTestIntentId) != nil {
		t.Fatal("a paid network invoice stayed saved")
	}
}

// A hidden network invoice is polled once at the next Resume: paid, its data
// is on the network and it is dropped, as it is when the server no longer
// knows it; otherwise it stays hidden.
func TestCryptoInvoiceHiddenNetworkInvoiceIsCheckedOnce(t *testing.T) {
	for _, answer := range []string{CryptoPaymentStatusPaid, CryptoPaymentStatusConfirming, CryptoPaymentStatusUnknown} {
		t.Run(answer, func(t *testing.T) {
			harness := newCryptoInvoiceHarness(t, t.TempDir())
			harness.networkId = ""
			harness.vc.Start()
			expiresAt := harness.startNetworkInvoice(harness.vc, cryptoPayTestIntentId, cryptoPayTestStatusToken)
			harness.api.statusResult = cryptoInvoiceTestNetworkStatus(cryptoPayTestIntentId, CryptoPaymentStatusSeen, expiresAt)
			harness.advance(5 * time.Second)
			harness.vc.Dismiss()
			connect.AssertEqual(t, harness.savedOwned(cryptoPayTestIntentId).Hidden, true)

			harness.api.statusResult = cryptoInvoiceTestNetworkStatus(cryptoPayTestIntentId, answer, expiresAt)
			relaunched := harness.newController()
			polls := harness.statusCount()
			connect.AssertEqual(t, relaunched.Resume(), false)
			connect.AssertEqual(t, harness.statusCount(), polls+1)
			connect.AssertEqual(t, relaunched.GetState().Phase, CryptoInvoicePhaseNone)
			saved := harness.savedOwned(cryptoPayTestIntentId)
			if answer == CryptoPaymentStatusConfirming {
				if saved == nil || !saved.Hidden {
					t.Fatalf("the hidden network invoice was not kept hidden: %+v", saved)
				}
			} else if saved != nil {
				t.Fatalf("a %s network invoice stayed saved", answer)
			}
		})
	}
}

// Resume's order: a paid code not yet dismissed, then an invoice bought
// without a sign-in that was on screen, then the signed-in network's own.
func TestCryptoInvoiceResumeOrder(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.clock.advance(time.Second)

	network := harness.newController()
	network.Start()
	harness.startNetworkInvoice(network, cryptoInvoiceTestOtherIntentId, cryptoInvoiceTestOtherStatusToken)
	harness.clock.advance(time.Second)

	const codeIntentId = "0b6f0b6f-0000-4000-8000-0000000000c0"
	code := harness.newController()
	code.Start()
	codeExpiresAt := harness.startCodeInvoice(code, codeIntentId, "kq3-synthetic-status-token-code")
	harness.api.statusResult = cryptoInvoiceTestCodeStatus(codeIntentId, CryptoPaymentStatusPaid, codeExpiresAt)
	harness.clock.advance(5 * time.Second)
	code.step()
	connect.AssertEqual(t, code.GetState().Phase, CryptoInvoicePhasePaid)

	harness.api.statusResult = cryptoInvoiceTestNetworkStatus(cryptoInvoiceTestOtherIntentId, CryptoPaymentStatusPending, codeExpiresAt)
	relaunched := harness.newController()
	connect.AssertEqual(t, relaunched.Resume(), true)
	connect.AssertEqual(t, relaunched.GetState().IntentId, codeIntentId)
	connect.AssertEqual(t, relaunched.GetState().BalanceCode, cryptoInvoiceTestBalanceCode)

	relaunched.Dismiss()
	connect.AssertEqual(t, relaunched.Resume(), true)
	connect.AssertEqual(t, relaunched.GetState().IntentId, cryptoInvoiceTestOtherIntentId)

	// nothing received: the network invoice is dropped, and the account's
	// own comes next
	relaunched.Start()
	relaunched.step()
	connect.AssertEqual(t, relaunched.GetState().Status, CryptoPaymentStatusPending)
	relaunched.Dismiss()
	if harness.savedOwned(cryptoInvoiceTestOtherIntentId) != nil {
		t.Fatal("a dismissed network invoice with nothing received stayed saved")
	}
	connect.AssertEqual(t, relaunched.Resume(), true)
	connect.AssertEqual(t, relaunched.GetState().IntentId, cryptoPayTestIntentId)
}
