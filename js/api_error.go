//go:build js

package main

import (
	"context"
	"encoding/json"
	"errors"
	"syscall/js"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// Api call failures as the page sees them, and the balance-code redeem that
// must not report a credited code as a failure (UPGRADE.md W5).
//
// An api promise rejects with an Error carrying the same `kind` the generated
// TS client sets (src/api_client.ts URNetworkApiErrorKind), so a page treats a
// wasm host call and a TS client call the same way:
//   - "http": a non-2xx response (`status` is set)
//   - "timeout": the call outlived a deadline (`isTimeout` is true). The
//     server may still have committed the call after the client stopped
//     waiting, so a money path must message that ambiguity.
//   - "parse": the response body was not the expected json
//   - "network": any other failure to get an answer
//
// Under js/wasm the connect strategy sends one fetch and reports any fetch
// failure as a plain error without its cause, so most transport failures
// there are "network", not "timeout". Neither tells the page whether the
// server committed; for the redeem, redeemBalanceCodeOutcome answers that
// from the network's redeemed-code list instead of from the error.

const (
	apiErrorKindHttp    = "http"
	apiErrorKindTimeout = "timeout"
	apiErrorKindParse   = "parse"
	apiErrorKindNetwork = "network"
)

func apiErrorKind(err error) string {
	var statusErr *connect.HttpStatusError
	if errors.As(err, &statusErr) {
		return apiErrorKindHttp
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apiErrorKindTimeout
	}
	// net.Error, url.Error and os.ErrDeadlineExceeded all report Timeout()
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return apiErrorKindTimeout
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return apiErrorKindParse
	}
	return apiErrorKindNetwork
}

// apiCallError marks a rejection that came from an api call, so jsError
// attaches the kind fields.
type apiCallError struct {
	err error
}

func (self *apiCallError) Error() string {
	return self.err.Error()
}

func (self *apiCallError) Unwrap() error {
	return self.err
}

// jsError is the Error a promise rejects with: the message, plus for an api
// call `kind`, `isTimeout` and (for "http") `status`.
func jsError(err error) js.Value {
	jsErr := js.Global().Get("Error").New(err.Error())
	var callErr *apiCallError
	if errors.As(err, &callErr) {
		kind := apiErrorKind(callErr.err)
		jsErr.Set("kind", kind)
		jsErr.Set("isTimeout", kind == apiErrorKindTimeout)
		var statusErr *connect.HttpStatusError
		if errors.As(callErr.err, &statusErr) {
			jsErr.Set("status", statusErr.StatusCode)
		} else {
			jsErr.Set("status", 0)
		}
	}
	return jsErr
}

// apiErrorInfo is a transport failure inside a resolved result.
type apiErrorInfo struct {
	Message   string `json:"message"`
	Kind      string `json:"kind"`
	IsTimeout bool   `json:"is_timeout"`
}

func newApiErrorInfo(err error) *apiErrorInfo {
	kind := apiErrorKind(err)
	return &apiErrorInfo{
		Message:   err.Error(),
		Kind:      kind,
		IsTimeout: kind == apiErrorKindTimeout,
	}
}

// balanceCodeRedeemOutcome is what redeemBalanceCodeOutcome resolves with.
// `outcome` is one of the sdk BalanceCodeRedeemOutcome* values.
type balanceCodeRedeemOutcome struct {
	Outcome         string                                `json:"outcome"`
	TransferBalance *sdk.RedeemBalanceCodeTransferBalance `json:"transfer_balance,omitempty"`
	// the server's rejection, when it answered
	Error *sdk.RedeemBalanceCodeError `json:"error,omitempty"`
	// the redeem call's own failure, when it got no answer
	TransportError *apiErrorInfo `json:"transport_error,omitempty"`
}

// redeemBalanceCodeOutcome redeems `secret` and classifies the result with
// sdk.ClassifyBalanceCodeRedeem. When the redeem did not credit (the server
// rejected it, or the call got no answer) it fetches the network's
// redeemed-code list first: a code already in that list was credited to this
// network, by an earlier attempt or by this one before its answer was lost,
// and must read "already redeemed", never "invalid" or "failed". If the list
// cannot be fetched either, a transport failure stays "unknown".
//
// Blocking; call from a goroutine (jsPromise runs it in one).
func redeemBalanceCodeOutcome(
	secret string,
	redeem func(args *sdk.RedeemBalanceCodeArgs, callback sdk.RedeemBalanceCodeCallback),
	redeemedCodes func(callback sdk.GetNetworkRedeemedBalanceCodesCallback),
) *balanceCodeRedeemOutcome {
	type redeemResult struct {
		result *sdk.RedeemBalanceCodeResult
		err    error
	}
	redeemDone := make(chan redeemResult, 1)
	redeem(&sdk.RedeemBalanceCodeArgs{Secret: secret}, connect.NewApiCallback(func(result *sdk.RedeemBalanceCodeResult, err error) {
		redeemDone <- redeemResult{result: result, err: err}
	}))
	r := <-redeemDone

	outcome := &balanceCodeRedeemOutcome{}
	var result *sdk.RedeemBalanceCodeResult
	if r.err != nil {
		outcome.TransportError = newApiErrorInfo(r.err)
	} else {
		result = r.result
		if result != nil {
			outcome.TransferBalance = result.TransferBalance
			outcome.Error = result.Error
		}
	}

	var list *sdk.RedeemedBalanceCodeList
	if result == nil || result.TransferBalance == nil {
		listDone := make(chan *sdk.RedeemedBalanceCodeList, 1)
		redeemedCodes(connect.NewApiCallback(func(listResult *sdk.GetNetworkRedeemedBalanceCodesResult, err error) {
			if err != nil || listResult == nil || listResult.Error != nil {
				listDone <- nil
				return
			}
			listDone <- listResult.BalanceCodes
		}))
		list = <-listDone
	}

	outcome.Outcome = sdk.ClassifyBalanceCodeRedeem(result, list, secret)
	return outcome
}
