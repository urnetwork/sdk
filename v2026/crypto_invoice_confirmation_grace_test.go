package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Bitcoin's confirmation grace on the invoice screen (mmm/BTCPAY.md §16.8).
// The server sees a Bitcoin payment only once it is mined, so it credits one
// sent within the quote at its price while it is mined within 3 hours of the
// expiry, keeps the invoice pending through that grace, and refuses a requote
// in it. The countdown still ends at the quote's expiry, which stays the time
// to send by. Past it AwaitingConfirmation says a payment sent in time may
// still confirm: the screen shows the coin's awaiting line in place of the
// expired copy and no Refresh quote, a requote is not asked, and Pay another
// way puts the invoice away instead of dropping it. A coin without a grace is
// as before. On the fake clock and the fake api; js/test/crypto_invoice.test.ts
// runs the same cases against the TypeScript twin.

const (
	cryptoInvoiceTestBtcAddress    = "bc1q0wd209cv5k9pd9mhk7nspacywcj038xxdhnt5u"
	cryptoInvoiceTestBtcGrace      = 3 * time.Hour
	cryptoInvoiceTestBtcGraceInSec = 10800
)

// The server's Bitcoin yearly invoice, quoted at now: $40 at $73,596.90.
func cryptoInvoiceTestBtcIntent(now time.Time) *CryptoPaymentIntentResult {
	expiresAt := now.Add(cryptoInvoiceTestQuoteTtl)
	return &CryptoPaymentIntentResult{
		IntentId:                 cryptoPayTestIntentId,
		StatusToken:              cryptoPayTestStatusToken,
		Asset:                    CryptoAssetBtc,
		Plan:                     PlanYearly,
		Delivery:                 CryptoPaymentDeliveryAccount,
		Tier:                     "standard",
		PriceUsd:                 40,
		RegularPriceUsd:          40,
		RateUsd:                  73596.90,
		Address:                  cryptoInvoiceTestBtcAddress,
		Amount:                   "0.00054622",
		AmountAtomic:             "54622",
		Uri:                      "bitcoin:" + cryptoInvoiceTestBtcAddress + "?amount=0.00054622&label=URnetwork&message=URnetwork%20Pro%20yearly",
		QuotedAt:                 cryptoInvoiceTestTime(now),
		ExpiresAt:                cryptoInvoiceTestTime(expiresAt),
		ConfirmationGraceSeconds: cryptoInvoiceTestBtcGraceInSec,
		ConfirmationsRequired:    1,
		Status:                   cryptoInvoiceTestBtcStatus(CryptoPaymentStatusPending, expiresAt),
	}
}

// The status of the Bitcoin invoice above.
func cryptoInvoiceTestBtcStatus(status string, expiresAt time.Time) *CryptoPaymentStatusResult {
	result := cryptoInvoiceTestStatus(status, expiresAt)
	if status == CryptoPaymentStatusUnknown {
		return result
	}
	result.Asset = CryptoAssetBtc
	result.Amount, result.AmountAtomic = "0.00054622", "54622"
	switch status {
	case CryptoPaymentStatusConfirming, CryptoPaymentStatusPaid:
		result.Received, result.ReceivedAtomic = "0.00054622", "54622"
		result.Remaining, result.RemainingAtomic = "0", "0"
	default:
		result.Received, result.ReceivedAtomic = "0", "0"
		result.Remaining, result.RemainingAtomic = "0.00054622", "54622"
	}
	result.ConfirmationGraceSeconds = cryptoInvoiceTestBtcGraceInSec
	return result
}

// Started, with the Bitcoin yearly invoice quoted now and its pending status
// answering every poll; returns the quote's end.
func (self *cryptoInvoiceHarness) startBtcInvoice() time.Time {
	self.t.Helper()
	intent := cryptoInvoiceTestBtcIntent(self.clock.now)
	expiresAt := self.clock.now.Add(cryptoInvoiceTestQuoteTtl)
	self.api.intentResult = intent
	self.api.statusResult = cryptoInvoiceTestBtcStatus(CryptoPaymentStatusPending, expiresAt)
	self.vc.Start()
	self.vc.step()
	self.vc.StartInvoice(CryptoAssetBtc, PlanYearly, "", "US")
	self.vc.step()
	connect.AssertEqual(self.t, self.vc.GetState().Phase, CryptoInvoicePhaseOpen)
	return expiresAt
}

// The countdown ends at the quote's expiry, as for every coin; then, while the
// server keeps the invoice pending through the grace, it awaits confirmation:
// no requote is asked. Once the grace is over, or the server says expired, it
// is an expired quote again, and Refresh quote works; a payment that confirms
// in the grace ends the wait too.
func TestCryptoInvoiceBitcoinAwaitsConfirmationThroughTheGrace(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startBtcInvoice()
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.ConfirmationGraceSeconds, cryptoInvoiceTestBtcGraceInSec)
	connect.AssertEqual(t, state.SecondsLeft, 1800)
	connect.AssertEqual(t, state.AwaitingConfirmation, false)
	connect.AssertEqual(t, harness.saved().ConfirmationGraceSeconds, cryptoInvoiceTestBtcGraceInSec)

	// the window's last second: payable
	harness.advance(cryptoInvoiceTestQuoteTtl - time.Second)
	state = harness.vc.GetState()
	connect.AssertEqual(t, state.SecondsLeft, 1)
	connect.AssertEqual(t, state.AwaitingConfirmation, false)

	// the quote ended, the server still says pending: awaiting confirmation
	harness.advance(time.Second)
	state = harness.vc.GetState()
	connect.AssertEqual(t, state.SecondsLeft, 0)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, state.AwaitingConfirmation, true)
	// no requote: the server refuses one in the grace
	harness.vc.Requote()
	connect.AssertEqual(t, len(harness.api.requoteCalls), 0)
	connect.AssertEqual(t, harness.vc.GetState().Refreshing, false)

	// through the grace, on the quote's own clock
	harness.advance(cryptoInvoiceTestBtcGrace - time.Second)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, true)
	harness.advance(time.Second)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, false)

	// the server's expired: an expired quote, refreshed as before
	harness.api.statusResult = cryptoInvoiceTestBtcStatus(CryptoPaymentStatusExpired, harness.clock.now.Add(-cryptoInvoiceTestBtcGrace))
	harness.advance(time.Minute)
	state = harness.vc.GetState()
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusExpired)
	connect.AssertEqual(t, state.AwaitingConfirmation, false)
	harness.api.holdRequote = true
	harness.vc.Requote()
	connect.AssertEqual(t, len(harness.api.requoteCalls), 1)
	connect.AssertEqual(t, harness.vc.GetState().Refreshing, true)
}

// The end of the grace is announced when it comes, on the quote's clock, even
// with no poll answering: the loop wakes for it, and the listeners hear that
// AwaitingConfirmation ended.
func TestCryptoInvoiceBitcoinGraceEndWakesTheScreen(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startBtcInvoice()
	// the countdown's last tick, and a poll left in flight
	harness.api.holdStatus = true
	harness.advance(cryptoInvoiceTestQuoteTtl)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, true)
	// the countdown done and the poll in flight: the grace's end is the next wake
	delay, arm := harness.vc.step()
	connect.AssertEqual(t, arm, true)
	connect.AssertEqual(t, delay, cryptoInvoiceTestBtcGrace)
	changes := harness.changes
	harness.advance(cryptoInvoiceTestBtcGrace)
	connect.AssertEqual(t, harness.changes, changes+1)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, false)
	// nothing more to wake for
	_, arm = harness.vc.step()
	connect.AssertEqual(t, arm, false)
}

// A payment mined in the grace ends the wait: confirming, then paid.
func TestCryptoInvoiceBitcoinPaymentMinedInTheGraceEndsTheWait(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	expiresAt := harness.startBtcInvoice()
	harness.advance(cryptoInvoiceTestQuoteTtl + time.Hour)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, true)
	harness.api.statusResult = cryptoInvoiceTestBtcStatus(CryptoPaymentStatusConfirming, expiresAt)
	harness.advance(5 * time.Second)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusConfirming)
	connect.AssertEqual(t, state.AwaitingConfirmation, false)
	harness.api.statusResult = cryptoInvoiceTestBtcStatus(CryptoPaymentStatusPaid, expiresAt)
	harness.advance(5 * time.Second)
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhasePaid)
}

// Pay another way in the grace only puts the invoice away, as once money
// arrived: a payment may be on its way unseen, and the next Resume shows it
// again. Past the grace, with nothing received, it is dropped as before.
func TestCryptoInvoiceBitcoinDismissInTheGracePutsItAway(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startBtcInvoice()
	harness.advance(cryptoInvoiceTestQuoteTtl + time.Minute)
	connect.AssertEqual(t, harness.vc.GetState().AwaitingConfirmation, true)
	harness.vc.Dismiss()
	connect.AssertEqual(t, harness.vc.GetState().Phase, CryptoInvoicePhaseNone)
	saved := harness.saved()
	if saved == nil {
		t.Fatal("an invoice whose payment may still confirm was dropped")
	}
	connect.AssertEqual(t, saved.ConfirmationGraceSeconds, cryptoInvoiceTestBtcGraceInSec)

	// after a relaunch it comes back knowing its grace, before its first poll
	vc := harness.newController()
	harness.vc = vc
	connect.AssertEqual(t, vc.Resume(), true)
	state := vc.GetState()
	connect.AssertEqual(t, state.Phase, CryptoInvoicePhaseOpen)
	connect.AssertEqual(t, state.ConfirmationGraceSeconds, cryptoInvoiceTestBtcGraceInSec)
	// not polled yet: no status, so nothing to wait for yet
	connect.AssertEqual(t, state.Status, "")
	connect.AssertEqual(t, state.AwaitingConfirmation, false)
	vc.Start()
	vc.step()
	connect.AssertEqual(t, vc.GetState().AwaitingConfirmation, true)

	// past the grace, expired with nothing received: dropped
	harness.api.statusResult = cryptoInvoiceTestBtcStatus(CryptoPaymentStatusExpired, harness.clock.now)
	harness.advance(cryptoInvoiceTestBtcGrace)
	connect.AssertEqual(t, vc.GetState().Status, CryptoPaymentStatusExpired)
	vc.Dismiss()
	if harness.saved() != nil {
		t.Fatal("an expired unpaid invoice stayed saved")
	}
}

// A coin without a grace never awaits: Monero's quote ends at its expiry
// whatever the server's next poll says, and Refresh quote works at once.
func TestCryptoInvoiceCoinsWithoutAGraceNeverAwait(t *testing.T) {
	harness := newCryptoInvoiceHarness(t, t.TempDir())
	harness.startOpenInvoice()
	harness.advance(cryptoInvoiceTestQuoteTtl + time.Minute)
	state := harness.vc.GetState()
	connect.AssertEqual(t, state.SecondsLeft, 0)
	connect.AssertEqual(t, state.Status, CryptoPaymentStatusPending)
	connect.AssertEqual(t, state.ConfirmationGraceSeconds, 0)
	connect.AssertEqual(t, state.AwaitingConfirmation, false)
	harness.api.holdRequote = true
	harness.vc.Requote()
	connect.AssertEqual(t, len(harness.api.requoteCalls), 1)
}

// Bitcoin's lines say how long a payment may take to confirm, under the
// countdown, and what to do while it may still confirm; no other coin has
// either line.
func TestCryptoAssetLinesSayBitcoinsConfirmationTime(t *testing.T) {
	lines := CryptoAssetLinesFor(CryptoAssetBtc)
	connect.AssertEqual(t, lines.ConfirmationNoteKey, "crypto_invoice_bitcoin_confirmation_time")
	connect.AssertEqual(t, lines.ConfirmationNote, "Bitcoin payments can take up to 3 hours to confirm. A payment sent before the quote expires is credited at this price.")
	connect.AssertEqual(t, lines.AwaitingConfirmationNoteKey, "crypto_invoice_bitcoin_awaiting_confirmation")
	connect.AssertEqual(t, lines.AwaitingConfirmationNote, "This quote expired. A payment you sent before it expired is credited at this price once it confirms, which can take up to 3 hours. If you didn't send one, pay another way.")
	for _, asset := range []string{CryptoAssetUsdcSolana, CryptoAssetZec, CryptoAssetXmr, CryptoAssetTao, CryptoAssetEth, CryptoAssetSol, "ltc"} {
		other := CryptoAssetLinesFor(asset)
		connect.AssertEqual(t, other.ConfirmationNote, "")
		connect.AssertEqual(t, other.ConfirmationNoteKey, "")
		connect.AssertEqual(t, other.AwaitingConfirmationNote, "")
		connect.AssertEqual(t, other.AwaitingConfirmationNoteKey, "")
	}
}
