package sdk

// Purchase reporting policy, shared by every store client (UPGRADE.md §4
// item 2).
//
// THE CONTRACT every platform purchase flow implements:
//
//  1. PERSIST the proof of purchase (the Play purchase token / the StoreKit
//     transaction JWS) durably BEFORE anything else, so process death loses
//     nothing.
//  2. REPORT it (Api.VerifyPlayPurchase / Api.VerifyAppleTransaction) and
//     RETRY on transport failure or a "pending" answer, sleeping
//     PurchaseReportBackoffMillis between attempts, until the server answers
//     a TERMINAL status: credited, already_credited, wrong_network, or
//     invalid.
//  3. Only THEN finalize with the store: acknowledge the purchase (android) /
//     call Transaction.finish() (apple), and drop the persisted proof.
//
// Finalizing early destroys the safety net: an acknowledged/finished purchase
// is never redelivered by the store, so if the server never saw the proof
// (lost webhook AND lost report), the money is gone. Finalizing late is safe:
// the server-side crediting gates make every re-report idempotent.
//
// wrong_network is terminal for THIS session but not a loss: the purchase is
// linked to a different network, which the webhook/reconciler paths still
// credit. Surface it as "purchased under a different account", finalize, and
// keep the store receipt UI as the user's recourse.

// Purchase report statuses, the wire values of VerifyStorePurchaseResult
// .Status from POST /subscription/verify-play-purchase and
// POST /subscription/verify-apple-transaction.
const (
	// PurchaseReportStatusCredited: this report landed the credit. Terminal.
	PurchaseReportStatusCredited = "credited"
	// PurchaseReportStatusAlreadyCredited: the credit was already there (an
	// earlier report, the webhook, or the reconciler). Terminal.
	PurchaseReportStatusAlreadyCredited = "already_credited"
	// PurchaseReportStatusPending: the store says the purchase is not
	// complete yet (e.g. Play PENDING awaiting approval). NOT terminal --
	// keep the proof, keep retrying, do not finalize.
	PurchaseReportStatusPending = "pending"
	// PurchaseReportStatusInvalid: the proof will never verify (unknown
	// token, failed JWS verification, disallowed product, canceled before
	// completing). Terminal.
	PurchaseReportStatusInvalid = "invalid"
	// PurchaseReportStatusWrongNetwork: the proof verified but the purchase
	// is linked to a DIFFERENT network than this session. Terminal for this
	// session; the linked network is credited through the webhook paths.
	PurchaseReportStatusWrongNetwork = "wrong_network"
)

// IsPurchaseReportTerminal reports whether status ends the retry loop: after
// a terminal status the client finalizes with the store (acknowledge/finish)
// and drops the persisted proof. A transport failure has no status; it is
// never terminal.
func IsPurchaseReportTerminal(status string) bool {
	switch status {
	case PurchaseReportStatusCredited,
		PurchaseReportStatusAlreadyCredited,
		PurchaseReportStatusInvalid,
		PurchaseReportStatusWrongNetwork:
		return true
	}
	return false
}

// purchaseReportBackoffMillis is the retry schedule: quick retries first (the
// common failure is a blip), then settling at a 5 minute cap -- which also
// keeps a well-behaved client polling a pending purchase safely under the
// server's per-account verify budget (30/hour).
var purchaseReportBackoffMillis = []int64{
	1_000,   // 1s
	5_000,   // 5s
	30_000,  // 30s
	300_000, // 5m cap
}

// PurchaseReportBackoffMillis returns how long to sleep before retry number
// `attempt` of a purchase report, in millis. attempt 0 is the first RETRY
// (i.e. after the first report failed or answered pending): 1s, then 5s, 30s,
// and 5m for every attempt after, uncapped in count -- the loop only ends on
// a terminal status (IsPurchaseReportTerminal). Out-of-range attempts clamp
// (negative -> first delay). Pure; no clock, no jitter.
func PurchaseReportBackoffMillis(attempt int32) int64 {
	if attempt < 0 {
		attempt = 0
	}
	if int(attempt) < len(purchaseReportBackoffMillis) {
		return purchaseReportBackoffMillis[attempt]
	}
	return purchaseReportBackoffMillis[len(purchaseReportBackoffMillis)-1]
}
