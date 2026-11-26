package sdk

// The mirror of api's compute routes (DESIGN-COMPUTE §14.3, §9; DESIGN CONVENTIONS §8; CSRV-M12-04): one Api method a
// route, each in api.go's three forms, the callback, SyncWithContext and Sync, with the client's jwt as bearer:
//
//	GET  /compute/balance        ComputeBalance          GET  /compute/spend         ComputeSpend(by)
//	POST /compute/checkout       ComputeCheckout         POST /compute/refund        ComputeRefund
//	GET  /compute/auto-renew     ComputeAutoRenew        POST /compute/promo/redeem  ComputePromoRedeem
//	POST /compute/auto-renew     ComputeSetAutoRenew     GET  /compute/terms         ComputeTerms
//	GET  /compute/charges        ComputeCharges(cursor)  POST /compute/terms         ComputeAcceptTerms
//	GET  /compute/statement      ComputeStatement(month)
//
// Each carries its route's body as JSON text, a POST's in ComputeApiArgs and every answer in ComputeApiResult, so the
// formats stay the routes' own (their handlers', BAL's; the sidecar's compute client reads them into its types,
// sidecar/compute_client_api.go) and a gomobile binding needs none of theirs. A GET's one query value is an argument,
// sent only when set. A POST's body that is no JSON object is refused before it is sent (ErrComputeApiArgs), as the
// route would refuse it. A refusal whose body names a code is a *ComputeApiError; any other failure is connect's
// error as it came. These are the routes' raw calls: an app's screens bind the sdk's compute surface (Compute(),
// BAL-M12-19), which reads them through the sidecar's typed client.
//
// The model routes (api_compute_models.go) take no credential and are not these.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/urnetwork/connect"
)

// A POST's body, the route's JSON object as text.
type ComputeApiArgs struct {
	// The request's JSON object; empty sends {}.
	Json string
}

// A route's answer, its JSON as text.
type ComputeApiResult struct {
	Json string
}

type ComputeApiCallback connect.ApiCallback[*ComputeApiResult]

// A POST's body that is no JSON object, refused before it is sent.
var ErrComputeApiArgs = errors.New("a compute route's body is one JSON object")

// A refusal of a compute route, as its body names it: the status, the route's or the frame's code (not_built,
// may_not_buy, rate_limited ...), and the wait before the same request goes again, 0 for none.
type ComputeApiError struct {
	StatusCode   int
	Code         string
	RetryAfterMs int64
}

func (self *ComputeApiError) Error() string {
	return fmt.Sprintf("compute api refused: %d %s", self.StatusCode, self.Code)
}

// The code of a compute route's refusal, for a binding that reads an error as text; "" for an error that is none.
func ComputeApiErrorCode(err error) string {
	var refusal *ComputeApiError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// The milliseconds a compute route's refusal asks the caller to wait, 0 for none or an error that is no refusal.
func ComputeApiErrorRetryAfterMs(err error) int64 {
	var refusal *ComputeApiError
	if errors.As(err, &refusal) {
		return refusal.RetryAfterMs
	}
	return 0
}

// The network's compute balance: GET /compute/balance.
func (self *Api) ComputeBalance(callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeBalanceSyncWithContext(self.ctx))
	})
}

//gomobile:noexport
func (self *Api) ComputeBalanceSyncWithContext(ctx context.Context) (*ComputeApiResult, error) {
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/balance", self.apiUrl))
}

//gomobile:noexport
func (self *Api) ComputeBalanceSync() (*ComputeApiResult, error) {
	return self.ComputeBalanceSyncWithContext(self.ctx)
}

// A Stripe Checkout session for a pack or the monthly plan: POST /compute/checkout.
func (self *Api) ComputeCheckout(args *ComputeApiArgs, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeCheckoutSyncWithContext(self.ctx, args))
	})
}

//gomobile:noexport
func (self *Api) ComputeCheckoutSyncWithContext(ctx context.Context, args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.computeApiPost(ctx, fmt.Sprintf("%s/compute/checkout", self.apiUrl), args)
}

//gomobile:noexport
func (self *Api) ComputeCheckoutSync(args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.ComputeCheckoutSyncWithContext(self.ctx, args)
}

// The two auto-renew modes: GET /compute/auto-renew.
func (self *Api) ComputeAutoRenew(callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeAutoRenewSyncWithContext(self.ctx))
	})
}

//gomobile:noexport
func (self *Api) ComputeAutoRenewSyncWithContext(ctx context.Context) (*ComputeApiResult, error) {
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/auto-renew", self.apiUrl))
}

//gomobile:noexport
func (self *Api) ComputeAutoRenewSync() (*ComputeApiResult, error) {
	return self.ComputeAutoRenewSyncWithContext(self.ctx)
}

// One change of one auto-renew mode: POST /compute/auto-renew.
func (self *Api) ComputeSetAutoRenew(args *ComputeApiArgs, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeSetAutoRenewSyncWithContext(self.ctx, args))
	})
}

//gomobile:noexport
func (self *Api) ComputeSetAutoRenewSyncWithContext(ctx context.Context, args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.computeApiPost(ctx, fmt.Sprintf("%s/compute/auto-renew", self.apiUrl), args)
}

//gomobile:noexport
func (self *Api) ComputeSetAutoRenewSync(args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.ComputeSetAutoRenewSyncWithContext(self.ctx, args)
}

// A page of per-request charges, the first for an empty cursor: GET /compute/charges.
func (self *Api) ComputeCharges(cursor string, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeChargesSyncWithContext(self.ctx, cursor))
	})
}

//gomobile:noexport
func (self *Api) ComputeChargesSyncWithContext(ctx context.Context, cursor string) (*ComputeApiResult, error) {
	if cursor == "" {
		return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/charges", self.apiUrl))
	}
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/charges?cursor=%s", self.apiUrl, url.QueryEscape(cursor)))
}

//gomobile:noexport
func (self *Api) ComputeChargesSync(cursor string) (*ComputeApiResult, error) {
	return self.ComputeChargesSyncWithContext(self.ctx, cursor)
}

// A UTC month's statement, its month YYYY-MM: GET /compute/statement.
func (self *Api) ComputeStatement(month string, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeStatementSyncWithContext(self.ctx, month))
	})
}

//gomobile:noexport
func (self *Api) ComputeStatementSyncWithContext(ctx context.Context, month string) (*ComputeApiResult, error) {
	if month == "" {
		return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/statement", self.apiUrl))
	}
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/statement?month=%s", self.apiUrl, url.QueryEscape(month)))
}

//gomobile:noexport
func (self *Api) ComputeStatementSync(month string) (*ComputeApiResult, error) {
	return self.ComputeStatementSyncWithContext(self.ctx, month)
}

// The spend view, broken down by by (provider, model, enclave, member, agent; empty for the route's own default): GET
// /compute/spend.
func (self *Api) ComputeSpend(by string, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeSpendSyncWithContext(self.ctx, by))
	})
}

//gomobile:noexport
func (self *Api) ComputeSpendSyncWithContext(ctx context.Context, by string) (*ComputeApiResult, error) {
	if by == "" {
		return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/spend", self.apiUrl))
	}
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/spend?by=%s", self.apiUrl, url.QueryEscape(by)))
}

//gomobile:noexport
func (self *Api) ComputeSpendSync(by string) (*ComputeApiResult, error) {
	return self.ComputeSpendSyncWithContext(self.ctx, by)
}

// A refund of a lot's unspent part: POST /compute/refund.
func (self *Api) ComputeRefund(args *ComputeApiArgs, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeRefundSyncWithContext(self.ctx, args))
	})
}

//gomobile:noexport
func (self *Api) ComputeRefundSyncWithContext(ctx context.Context, args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.computeApiPost(ctx, fmt.Sprintf("%s/compute/refund", self.apiUrl), args)
}

//gomobile:noexport
func (self *Api) ComputeRefundSync(args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.ComputeRefundSyncWithContext(self.ctx, args)
}

// One promotion code's redemption, the code in the body, never a url: POST /compute/promo/redeem.
func (self *Api) ComputePromoRedeem(args *ComputeApiArgs, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputePromoRedeemSyncWithContext(self.ctx, args))
	})
}

//gomobile:noexport
func (self *Api) ComputePromoRedeemSyncWithContext(ctx context.Context, args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.computeApiPost(ctx, fmt.Sprintf("%s/compute/promo/redeem", self.apiUrl), args)
}

//gomobile:noexport
func (self *Api) ComputePromoRedeemSync(args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.ComputePromoRedeemSyncWithContext(self.ctx, args)
}

// The compute terms' version and the clauses the network accepted: GET /compute/terms.
func (self *Api) ComputeTerms(callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeTermsSyncWithContext(self.ctx))
	})
}

//gomobile:noexport
func (self *Api) ComputeTermsSyncWithContext(ctx context.Context) (*ComputeApiResult, error) {
	return self.computeApiGet(ctx, fmt.Sprintf("%s/compute/terms", self.apiUrl))
}

//gomobile:noexport
func (self *Api) ComputeTermsSync() (*ComputeApiResult, error) {
	return self.ComputeTermsSyncWithContext(self.ctx)
}

// Accepts a compute terms clause: POST /compute/terms.
func (self *Api) ComputeAcceptTerms(args *ComputeApiArgs, callback ComputeApiCallback) {
	runAsyncApiRequest[*ComputeApiResult](callback, func(callback connect.ApiCallback[*ComputeApiResult]) {
		callback.Result(self.ComputeAcceptTermsSyncWithContext(self.ctx, args))
	})
}

//gomobile:noexport
func (self *Api) ComputeAcceptTermsSyncWithContext(ctx context.Context, args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.computeApiPost(ctx, fmt.Sprintf("%s/compute/terms", self.apiUrl), args)
}

//gomobile:noexport
func (self *Api) ComputeAcceptTermsSync(args *ComputeApiArgs) (*ComputeApiResult, error) {
	return self.ComputeAcceptTermsSyncWithContext(self.ctx, args)
}

// One GET of a compute route, with the client's jwt.
func (self *Api) computeApiGet(ctx context.Context, requestUrl string) (*ComputeApiResult, error) {
	body, err := self.getHttpGetRaw()(ctx, requestUrl, self.GetByJwt())
	return computeApiResultOf(body, err)
}

// One POST of a compute route, args's JSON object as its body, with the client's jwt.
func (self *Api) computeApiPost(ctx context.Context, requestUrl string, args *ComputeApiArgs) (*ComputeApiResult, error) {
	requestBody := []byte("{}")
	if args != nil && args.Json != "" {
		requestBody = bytes.TrimSpace([]byte(args.Json))
		if len(requestBody) == 0 || requestBody[0] != '{' || !json.Valid(requestBody) {
			return nil, ErrComputeApiArgs
		}
	}
	body, err := self.getHttpPostRaw()(ctx, requestUrl, requestBody, self.GetByJwt())
	return computeApiResultOf(body, err)
}

// The answer of a compute route: its JSON, or its refusal as a *ComputeApiError where its body names a code.
func computeApiResultOf(body []byte, err error) (*ComputeApiResult, error) {
	if err != nil {
		var statusErr *connect.HttpStatusError
		if errors.As(err, &statusErr) {
			refusal := struct {
				Code         string `json:"code"`
				RetryAfterMs int64  `json:"retry_after_ms"`
			}{}
			if json.Unmarshal(statusErr.Body, &refusal) == nil && refusal.Code != "" {
				return nil, &ComputeApiError{StatusCode: statusErr.StatusCode, Code: refusal.Code, RetryAfterMs: max(refusal.RetryAfterMs, 0)}
			}
		}
		return nil, err
	}
	return &ComputeApiResult{Json: string(body)}, nil
}
