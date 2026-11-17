package sdk

// The App Store purchase report's sandbox refusal (owner, 2026-11-17: "Keep
// buttons, neutral notice"): the server credits a sandbox purchase only for an
// internal tester and otherwise answers invalid with the sandbox_purchase
// code, which the apps read here to show their neutral line. The server is a
// local test server.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"
)

func TestVerifyAppleTransactionReadsTheSandboxRefusal(t *testing.T) {
	answer := `{"status":"invalid","error":{"code":"sandbox_purchase","message":"Test purchases in test builds don't unlock Pro."}}`
	paths := make(chan string, 1)
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer)
	})

	callback, c := connect.NewBlockingApiCallback[*VerifyStorePurchaseResult](context.Background())
	api.VerifyAppleTransaction(&VerifyAppleTransactionArgs{SignedTransaction: "header.payload.signature"}, callback)
	r := awaitApiResult(t, c, "VerifyAppleTransaction never called back")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if path := <-paths; path != "/subscription/verify-apple-transaction" {
		t.Fatalf("path = %s", path)
	}
	if !IsPurchaseReportTerminal(r.Result.Status) {
		t.Fatalf("status %q is not terminal: the transaction must be finished", r.Result.Status)
	}
	if !IsSandboxPurchaseRefusal(r.Result) {
		t.Fatalf("result = %+v, want the sandbox refusal", r.Result)
	}
	if code := PurchaseReportErrorCode(r.Result); code != PurchaseErrorCodeSandboxPurchase {
		t.Fatalf("code = %q", code)
	}
}

// Only an invalid answer with the code is the sandbox refusal: a plain
// invalid, another code, a credit, an older server's answer and no answer
// are not.
func TestIsSandboxPurchaseRefusalNeedsTheCode(t *testing.T) {
	for _, test := range []struct {
		result  *VerifyStorePurchaseResult
		code    string
		refusal bool
	}{
		{result: nil},
		{result: &VerifyStorePurchaseResult{Status: PurchaseReportStatusInvalid}},
		{
			result: &VerifyStorePurchaseResult{
				Status: PurchaseReportStatusInvalid,
				Error:  &OnboardingError{Code: PurchaseErrorCodeIncorrectCvc, Message: "cvc"},
			},
			code: PurchaseErrorCodeIncorrectCvc,
		},
		{
			result: &VerifyStorePurchaseResult{
				Status: PurchaseReportStatusInvalid,
				Error:  &OnboardingError{Code: PurchaseErrorCodeSandboxPurchase},
			},
			code:    PurchaseErrorCodeSandboxPurchase,
			refusal: true,
		},
		{
			// the code counts only with the invalid status
			result: &VerifyStorePurchaseResult{
				Status: PurchaseReportStatusCredited,
				Error:  &OnboardingError{Code: PurchaseErrorCodeSandboxPurchase},
			},
		},
		{result: &VerifyStorePurchaseResult{Status: PurchaseReportStatusWrongNetwork}},
	} {
		if got := PurchaseReportErrorCode(test.result); got != test.code {
			t.Errorf("PurchaseReportErrorCode(%+v) = %q, want %q", test.result, got, test.code)
		}
		if got := IsSandboxPurchaseRefusal(test.result); got != test.refusal {
			t.Errorf("IsSandboxPurchaseRefusal(%+v) = %t, want %t", test.result, got, test.refusal)
		}
	}
}
