package sdk

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// These tests exist because every bug they pin was shipped, and each one took
// money without delivering anything. None of them threw an error at the time --
// that is the point. A Solana Pay payment that cannot be credited looks exactly
// like a successful one from the client's side.

const (
	// A real mainnet USDC mint and a real merchant address, used as valid fixtures.
	testUsdcMint  = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	testRecipient = "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM"
)

func validArgs() *SolanaPaymentUrlArgs {
	return &SolanaPaymentUrlArgs{
		Recipient:    testRecipient,
		AmountUsd:    5,
		SplTokenMint: testUsdcMint,
		Reference:    CreatePaymentReference(),
		Label:        "URnetwork",
		Message:      "UR Pro - Monthly",
	}
}

// THE BUG: the web app used crypto.randomUUID() with the dashes stripped, a
// 32-character hex string. Solana Pay requires the reference to be a base58
// 32-byte public key. The wallet attaches it to the transaction as a read-only
// account and the webhook matches the on-chain account list against the stored
// reference, so a hex string never matched anything.
func TestCreatePaymentReferenceIsASolanaPublicKey(t *testing.T) {
	for i := 0; i < 200; i++ {
		ref := CreatePaymentReference()

		decoded := base58Decode(ref)
		if len(decoded) != SolanaPayReferenceBytes {
			t.Fatalf("reference %q decoded to %d bytes, want %d", ref, len(decoded), SolanaPayReferenceBytes)
		}
		// A 32-byte base58 value is 43 or 44 characters; leading zero bytes
		// shorten it, which is why this is a range and not an equality.
		if len(ref) < 43 || len(ref) > 44 {
			t.Fatalf("reference %q is %d chars, want 43-44", ref, len(ref))
		}
		if strings.ContainsAny(ref, "0OIl") {
			t.Fatalf("reference %q contains a character outside the base58 alphabet", ref)
		}
		if !IsValidPaymentReference(ref) {
			t.Fatalf("IsValidPaymentReference rejected its own output %q", ref)
		}
	}
}

// The old hex-uuid reference must be rejected, not merely different. This is the
// regression guard: if someone reintroduces a hex reference, this fails.
func TestHexUuidReferenceIsRejected(t *testing.T) {
	// crypto.randomUUID().replace(/-/g, "") produces exactly this shape.
	hexRefs := []string{
		"708c762b9f6c44138861a59cdafdfb37",
		"00000000000000000000000000000000",
		"ffffffffffffffffffffffffffffffff",
	}
	for _, ref := range hexRefs {
		if IsValidPaymentReference(ref) {
			t.Errorf("hex uuid %q accepted as a payment reference", ref)
		}
		args := validArgs()
		args.Reference = ref
		if _, err := BuildSolanaPaymentUrl(args); err == nil {
			t.Errorf("BuildSolanaPaymentUrl accepted hex uuid reference %q", ref)
		}
	}
}

func TestPaymentReferencesAreUnique(t *testing.T) {
	// A collision means two customers share one intent row, and the first payment
	// to arrive consumes the other's.
	seen := make(map[string]bool, 5000)
	for i := 0; i < 5000; i++ {
		ref := CreatePaymentReference()
		if seen[ref] {
			t.Fatalf("duplicate reference %q after %d draws", ref, i)
		}
		seen[ref] = true
	}
}

func TestIsValidPaymentReferenceRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"too short":          "abc",
		"31 bytes":           base58Encode(make([]byte, 31)),
		"33 bytes":           base58Encode(make([]byte, 33)),
		"zero char":          "0Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		"capital o":          "OFj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		"lowercase l":        "lFj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		"capital i":          "IFj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
		"whitespace":         " " + testRecipient,
		"url encoded spaces": strings.ReplaceAll(testRecipient, "R", "%52"),
	}
	for name, ref := range cases {
		if IsValidPaymentReference(ref) {
			t.Errorf("%s: %q accepted", name, ref)
		}
	}
}

// A 32-byte value IS a valid reference even though it is also a real address --
// they are the same type. This documents that the check is structural.
func TestValidAddressIsAValidReferenceShape(t *testing.T) {
	if !IsValidPaymentReference(testRecipient) {
		t.Fatalf("a real 32-byte address should satisfy the reference shape")
	}
}

func TestBuildSolanaPaymentUrlShape(t *testing.T) {
	args := validArgs()
	raw, err := BuildSolanaPaymentUrl(args)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if !strings.HasPrefix(raw, "solana:"+testRecipient+"?") {
		t.Fatalf("url does not target the recipient: %q", raw)
	}

	q, err := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
	if err != nil {
		t.Fatalf("query does not parse: %s", err)
	}
	for k, want := range map[string]string{
		"amount":    "5",
		"spl-token": testUsdcMint,
		"reference": args.Reference,
		"label":     "URnetwork",
		"message":   "UR Pro - Monthly",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// THE BUG: Android hardcoded amount=40 and "Yearly Supporter Subscription", so
// the monthly plan could not be sold at all, and the price the customer paid was
// a client-side constant that no longer had to agree with the server's quote.
// The webhook checks `amount >= quoted - tolerance` against the intent, so a
// disagreement means the money arrives and is never credited.
func TestAmountComesFromTheCallerNotAConstant(t *testing.T) {
	for _, amount := range []float64{5, 40, 4.5, 0.01, 12.34} {
		args := validArgs()
		args.AmountUsd = amount
		raw, err := BuildSolanaPaymentUrl(args)
		if err != nil {
			t.Fatalf("amount %v: unexpected error: %s", amount, err)
		}
		q, _ := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
		if got := q.Get("amount"); got != trimFloat(amount) {
			t.Errorf("amount %v encoded as %q, want %q", amount, got, trimFloat(amount))
		}
	}
}

// Wallets do not parse scientific notation. A tiny or large amount formatted as
// "1e-06" is silently unpayable.
func TestAmountNeverUsesScientificNotation(t *testing.T) {
	for _, amount := range []float64{0.000001, 1e21, 0.0000005} {
		args := validArgs()
		args.AmountUsd = amount
		raw, err := BuildSolanaPaymentUrl(args)
		if err != nil {
			continue // rejected outright is also acceptable
		}
		q, _ := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
		if strings.ContainsAny(q.Get("amount"), "eE") {
			t.Errorf("amount %v encoded in scientific notation as %q", amount, q.Get("amount"))
		}
	}
}

// A zero price is the most dangerous input in this whole path: the webhook's
// check is `amount >= price - tolerance`, which at price 0 is satisfied by any
// payment, including none. The server refuses to quote it; the url builder must
// refuse to build it.
func TestZeroAndNegativeAmountsRejected(t *testing.T) {
	for _, amount := range []float64{0, -1, -0.01} {
		args := validArgs()
		args.AmountUsd = amount
		if _, err := BuildSolanaPaymentUrl(args); err == nil {
			t.Errorf("amount %v was accepted", amount)
		}
	}
}

func TestMalformedAddressesRejected(t *testing.T) {
	bad := []string{"", "not-an-address", "0OIl", strings.Repeat("a", 44), testRecipient + "x"}

	for _, r := range bad {
		args := validArgs()
		args.Recipient = r
		if _, err := BuildSolanaPaymentUrl(args); err == nil {
			t.Errorf("recipient %q accepted", r)
		}
	}
	for _, m := range bad {
		args := validArgs()
		args.SplTokenMint = m
		if _, err := BuildSolanaPaymentUrl(args); err == nil {
			t.Errorf("spl token mint %q accepted", m)
		}
	}
}

func TestNilArgsRejected(t *testing.T) {
	if _, err := BuildSolanaPaymentUrl(nil); err == nil {
		t.Fatal("nil args accepted")
	}
}

// Label and message are free text and must survive encoding intact -- an
// unescaped '&' or '#' in a plan name would truncate the query and drop the
// reference, which is the same silent failure as a malformed reference.
func TestLabelAndMessageAreEscaped(t *testing.T) {
	args := validArgs()
	args.Label = "UR & Co #1"
	args.Message = "UR Pro — Yearly (100% off?)"

	raw, err := BuildSolanaPaymentUrl(args)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	q, err := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
	if err != nil {
		t.Fatalf("query does not parse after escaping: %s", err)
	}
	if q.Get("label") != args.Label {
		t.Errorf("label round trip: got %q, want %q", q.Get("label"), args.Label)
	}
	if q.Get("message") != args.Message {
		t.Errorf("message round trip: got %q, want %q", q.Get("message"), args.Message)
	}
	if q.Get("reference") != args.Reference {
		t.Errorf("reference was lost to query truncation: got %q", q.Get("reference"))
	}
}

func TestEmptyLabelAndMessageAreOmitted(t *testing.T) {
	args := validArgs()
	args.Label = ""
	args.Message = ""
	raw, err := BuildSolanaPaymentUrl(args)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	q, _ := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
	if _, ok := q["label"]; ok {
		t.Error("empty label should be omitted, not sent blank")
	}
	if _, ok := q["message"]; ok {
		t.Error("empty message should be omitted, not sent blank")
	}
}

// THE BUG: the sdk posted only {reference}. The server derives the price from
// pro.yml keyed by plan and returns "Unknown plan." for an empty one, so every
// Solana upgrade from an sdk-backed app failed before the wallet ever opened.
func TestPaymentIntentArgsCarryThePlan(t *testing.T) {
	args := &SolanaPaymentIntentArgs{
		Reference: CreatePaymentReference(),
		Plan:      "yearly",
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	if wire["plan"] != "yearly" {
		t.Errorf("plan not on the wire: %v", wire)
	}
	if wire["reference"] != args.Reference {
		t.Errorf("reference not on the wire: %v", wire)
	}
	// The client must NOT be able to name its own price.
	for _, forbidden := range []string{"amount", "amount_usd", "price", "price_usd"} {
		if _, ok := wire[forbidden]; ok {
			t.Errorf("client sent %q; the server derives the price, never the client", forbidden)
		}
	}
}

// The quoted price must survive the round trip, because it is what the url is
// built from and what the webhook checks against.
func TestPaymentIntentResultCarriesTheQuotedAmount(t *testing.T) {
	var result SolanaPaymentIntentResult
	if err := json.Unmarshal([]byte(`{"amount_usd":40}`), &result); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	if result.AmountUsd != 40 {
		t.Fatalf("amount_usd = %v, want 40", result.AmountUsd)
	}
	if result.Error != nil {
		t.Fatalf("unexpected error field: %+v", result.Error)
	}

	// An error response must be distinguishable from a zero quote.
	var errResult SolanaPaymentIntentResult
	if err := json.Unmarshal([]byte(`{"error":{"message":"Unknown plan."}}`), &errResult); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	if errResult.Error == nil || errResult.Error.Message != "Unknown plan." {
		t.Fatalf("error not parsed: %+v", errResult.Error)
	}
	if errResult.AmountUsd != 0 {
		t.Fatalf("error response should not carry an amount, got %v", errResult.AmountUsd)
	}
}

// The end-to-end shape an app must follow: quote from the server, then build the
// url from that quote. Pins the ordering so a refactor cannot reintroduce a
// client-side price.
func TestQuoteThenBuildUsesTheServerPrice(t *testing.T) {
	reference := CreatePaymentReference()

	// what the server returned
	var quoted SolanaPaymentIntentResult
	if err := json.Unmarshal([]byte(`{"amount_usd":40}`), &quoted); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}

	raw, err := BuildSolanaPaymentUrl(&SolanaPaymentUrlArgs{
		Recipient:    testRecipient,
		AmountUsd:    quoted.AmountUsd,
		SplTokenMint: testUsdcMint,
		Reference:    reference,
		Label:        "URnetwork",
		Message:      "UR Pro - Yearly",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	q, _ := url.ParseQuery(strings.SplitN(raw, "?", 2)[1])
	if q.Get("amount") != "40" {
		t.Errorf("url amount %q does not match the server quote 40", q.Get("amount"))
	}
	if q.Get("reference") != reference {
		t.Errorf("url reference %q does not match the registered intent", q.Get("reference"))
	}
}

// If the server refuses to quote, no url may be built -- an app that ignores the
// error and falls back to a constant is the original bug.
func TestUnquotedPlanCannotProduceAUrl(t *testing.T) {
	var refused SolanaPaymentIntentResult
	if err := json.Unmarshal([]byte(`{"error":{"message":"Unknown plan."}}`), &refused); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	args := validArgs()
	args.AmountUsd = refused.AmountUsd // 0
	if _, err := BuildSolanaPaymentUrl(args); err == nil {
		t.Fatal("built a payment url from a refused quote")
	}
}

// mirrors the formatting rule in BuildSolanaPaymentUrl
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
