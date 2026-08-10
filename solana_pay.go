package sdk

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Solana Pay, shared by every platform.
//
// This lived in each app before, and the copies disagreed in ways that cost real
// money:
//
//   - The web app generated the reference as a hex uuid. Solana Pay requires the
//     reference to be a base58 32-byte public key -- the wallet attaches it to the
//     transaction as a read-only account, and the webhook matches the on-chain
//     account list against `solana_payment_intent.payment_reference`. A hex string
//     is not a valid pubkey and never appears in that list, so the customer paid
//     and the payment could never be matched back to their account.
//   - Android built the url with a hardcoded amount of 40 USDC and a hardcoded
//     "Yearly Supporter Subscription" message, so it could not sell the monthly
//     plan at all, and its price was a client-side constant that no longer had to
//     agree with what the server quoted.
//   - Android also hardcoded the merchant address, with the previous one left in a
//     comment above it -- the address has rotated at least once already.
//
// The rule these encode: the client never names its own price. Call
// CreateSolanaPaymentIntent with the plan, take AmountUsd from the result, and pass
// that to BuildSolanaPaymentUrl. See solana_pay_test.go.

// SolanaPayReferenceBytes is the length of a Solana public key, which is what the
// Solana Pay `reference` parameter must be.
const SolanaPayReferenceBytes = 32

// CreatePaymentReference returns a fresh Solana Pay reference: 32 cryptographically
// random bytes, base58 encoded, which is the wire form of a Solana public key.
//
// The value is an identifier, not key material -- nothing signs with it. It has to
// be unpredictable only so two customers paying at the same moment cannot collide
// on one intent row.
func CreatePaymentReference() string {
	b := make([]byte, SolanaPayReferenceBytes)
	// crypto/rand.Read is documented to never fail as of go 1.24; it panics
	// internally if the OS entropy source is broken. A reference we could not
	// generate has no safe fallback -- a predictable one would let anyone claim
	// someone else's pending intent -- so there is deliberately no error path here.
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sdk: no entropy for a payment reference: %s", err))
	}
	return base58Encode(b)
}

// IsValidPaymentReference reports whether s is a well-formed Solana Pay reference:
// base58 that decodes to exactly 32 bytes.
//
// Worth checking before opening a wallet, because the failure it catches is silent.
// A malformed reference does not error -- the wallet either refuses the url or
// drops the parameter, the payment goes through, and the webhook then has nothing
// to match it against.
func IsValidPaymentReference(s string) bool {
	if s == "" {
		return false
	}
	// base58Decode returns an empty slice on any invalid character, so a bad
	// alphabet and a wrong length are the same check.
	return len(base58Decode(s)) == SolanaPayReferenceBytes
}

// SolanaPaymentUrlArgs is the input to BuildSolanaPaymentUrl.
//
// It is a struct rather than a long parameter list because gomobile binds it as an
// object the apps can construct field by field, and because the ordering of six
// strings is exactly the kind of thing that silently swaps a label for a message.
type SolanaPaymentUrlArgs struct {
	// Recipient is the merchant address, base58. Never hardcode it in an app --
	// it has rotated before.
	Recipient string `json:"recipient"`
	// AmountUsd is the price the SERVER quoted, from SolanaPaymentIntentResult.
	// Never a client-side constant.
	AmountUsd float64 `json:"amount_usd"`
	// SplTokenMint is the token mint address, base58 (USDC on mainnet).
	SplTokenMint string `json:"spl_token_mint"`
	// Reference is from CreatePaymentReference, already registered with
	// CreateSolanaPaymentIntent.
	Reference string `json:"reference"`
	// Label is the merchant name the wallet shows.
	Label string `json:"label,omitempty"`
	// Message is the human description of the purchase the wallet shows.
	Message string `json:"message,omitempty"`
}

// BuildSolanaPaymentUrl returns the `solana:` deep link for a payment, or an error
// if any part of it would produce a payment that cannot be credited.
//
// It validates rather than assuming, because every field here has a failure mode
// that is invisible at the call site: a wrong reference format is dropped by the
// wallet, a zero amount is a free year (the webhook's check is `amount >= price -
// tolerance`), and a malformed address sends money nowhere recoverable.
func BuildSolanaPaymentUrl(args *SolanaPaymentUrlArgs) (string, error) {
	if args == nil {
		return "", fmt.Errorf("solana pay: no arguments")
	}
	if !isBase58Address(args.Recipient) {
		return "", fmt.Errorf("solana pay: recipient is not a base58 address")
	}
	if !isBase58Address(args.SplTokenMint) {
		return "", fmt.Errorf("solana pay: spl token mint is not a base58 address")
	}
	if !IsValidPaymentReference(args.Reference) {
		return "", fmt.Errorf("solana pay: reference must be base58 that decodes to %d bytes", SolanaPayReferenceBytes)
	}
	// A zero or negative amount is never a real purchase, and the webhook would
	// treat it as satisfied by any payment at all.
	if args.AmountUsd <= 0 {
		return "", fmt.Errorf("solana pay: amount must be positive, got %v", args.AmountUsd)
	}

	// USDC has 6 decimals on chain, but Solana Pay takes a decimal amount in token
	// units, so the wallet does the scaling. Format it plainly: 'f' with -1
	// precision so 5 stays "5" and 4.5 stays "4.5", never "5e+00", which wallets
	// do not parse.
	amount := strconv.FormatFloat(args.AmountUsd, 'f', -1, 64)

	q := url.Values{}
	q.Set("amount", amount)
	q.Set("spl-token", args.SplTokenMint)
	q.Set("reference", args.Reference)
	if args.Label != "" {
		q.Set("label", args.Label)
	}
	if args.Message != "" {
		q.Set("message", args.Message)
	}

	return fmt.Sprintf("solana:%s?%s", args.Recipient, q.Encode()), nil
}

// isBase58Address reports whether s looks like a Solana address: base58 that
// decodes to 32 bytes. Same shape as a reference -- both are public keys.
func isBase58Address(s string) bool {
	if s == "" {
		return false
	}
	// Reject anything outside the alphabet explicitly rather than relying on the
	// decoder's empty-slice contract, so a 32-byte decode from a string containing
	// '0' or 'O' cannot slip through.
	if strings.ContainsAny(s, "0OIl") {
		return false
	}
	return len(base58Decode(s)) == SolanaPayReferenceBytes
}
