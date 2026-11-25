//go:build !ios_extension && !js

// The network's compute balance for the apps (DESIGN-COMPUTE §15.3, §9; BAL-M12-19): Compute(), gomobile-safe, its
// structures JSON text, over the local core's ComputeClient (the root's compute_client.go) in process, or, on the
// remote backend, over ur daemon's compute methods (the root's compute_daemon.go, the ones `ur compute` calls), so a
// desktop app shows what its daemon holds and both answer the same JSON.
//
//	Balance()               the balance, held, the state, auto-renew, whether the person may buy and whether sales
//	                        are open (Q-BAL-13); remote: compute.balance
//	Packs(market)           §9.5's packs and plans, or a store's compute.* products with their product ids, only
//	                        where Balance says the person may buy and sales are open; otherwise none, with why;
//	                        remote: compute.balance, then compute.packs
//	Charges(requestJson)    a page of charges, every id named on this device; remote: compute.charges
//	Checkout(requestJson)   the Stripe Checkout url for a pack or the monthly plan, with the auto-renew mode the
//	                        paying person turns on in that act; remote: compute.checkout
//	StorePurchase(api, market, purchaseJson)
//	                        a StoreKit transaction or a Play purchase reported through the app's own Api with the
//	                        network's credential (POST /subscription/verify-apple-transaction or verify-play-purchase),
//	                        as Pro's are, under sdk/purchase_report.go's contract; nothing of the daemon's
//	Redeem(code)            a promotion code, the person's own act in the app's UI: the redemption's lot, or the
//	                        refusal's reason; remote: the Confirm method compute.redeem, sent confirmed
//	AutoRenew(changeJson)   one of §9.8's modes changed, the person's own act; remote: the Confirm method
//	                        compute.auto_renew.set, sent confirmed
//	RaiseBudget(requestJson)
//	                        kind 21 filed for the person to approve (§16.3): done, or approval_pending with the
//	                        approval's id; remote: compute.budget.raise
//	Spend(requestJson)      §9.13's spend view, its ids named on this device; remote: compute.spend
//
// A refusal api answered with a code reads, for the methods that answer JSON, as {"refusal": {"code",
// "retry_after_ms"}}, the daemon's own shape, with no error, so an app shows the code's fix whichever backend it runs
// on; Checkout and AutoRenew, which answer no JSON, answer it as an error that names the code alone. Terms and
// AcceptTerms are BAL-M13-03's.
//
// The store purchase's contract (sdk/purchase_report.go): the app persists the proof before it reports, reports it
// again after the answer's retry_after_ms while the status is not terminal, and only then finalizes: finish() on the
// App Store's transaction, an acknowledgment of a Play subscription, and nothing for a Play consumable, which the server
// consumes (§9.5). The app sets the purchase's appAccountToken (Play: its obfuscated account id) to the network's id
// beforehand, so the credit lands in that network (§15.3). The Api is the app's own, holding the network's credential,
// which never enters the Sidecar (I-6), so it is passed on each call.
//
// Nothing here runs in the VPN extension. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/sidecar"
)

// The network's compute balance for the apps: its credits, purchases, auto-renew, budgets and where credits went. Made
// once per Sidecar, at Compute()'s first call; safe for concurrent use.
type SidecarCompute struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarCompute(args *sidecarComputeArgs) *SidecarCompute {
	return &SidecarCompute{args: args}
}

// The daemon's compute methods, as compute_daemon.go names them; TestComputeControlWireMatchesTheDaemon holds them to
// that source on the legs that build it.
const (
	sidecarControlMethodComputeBalance      = "compute.balance"
	sidecarControlMethodComputePacks        = "compute.packs"
	sidecarControlMethodComputeCheckout     = "compute.checkout"
	sidecarControlMethodComputeRedeem       = "compute.redeem"
	sidecarControlMethodComputeAutoRenewSet = "compute.auto_renew.set"
	sidecarControlMethodComputeCharges      = "compute.charges"
	sidecarControlMethodComputeSpend        = "compute.spend"
	sidecarControlMethodComputeBudgetRaise  = "compute.budget.raise"
)

// The stores StorePurchase reports to, as §9.4 names their markets.
const (
	sidecarComputeMarketAppStore   = "app_store"
	sidecarComputeMarketGooglePlay = "google_play"
)

// What a store purchase's answer tells the app to do with the store once it is terminal.
const (
	// Keep the proof and report again after retry_after_ms: the status is not terminal.
	SidecarComputeFinalizeKeep = "keep"
	// Call finish() on the App Store's transaction.
	SidecarComputeFinalizeFinish = "finish"
	// Acknowledge the Play subscription.
	SidecarComputeFinalizeAcknowledge = "acknowledge"
	// Nothing: the server consumed the Play consumable (§9.5), or the purchase never verified.
	SidecarComputeFinalizeNone = "none"
)

var (
	// What Compute answers with neither a local core nor a remote backend: a closed Sidecar.
	errSidecarComputeNoCore = errors.New("compute: this Sidecar has no core, local or remote")
	// A request the method cannot take; the text names no value it held.
	errSidecarComputeRequest = errors.New("compute: the request is malformed")
)

// A refusal api answered with a code, for the methods that answer no JSON: its text names the code alone.
type sidecarComputeRefusalError struct {
	code string
}

func (self *sidecarComputeRefusalError) Error() string {
	return "compute refused: " + self.code
}

// The refusal reply's shape, the daemon's DaemonComputeRefusalReply.
type sidecarComputeRefusalReply struct {
	Refusal *sidecarComputeRefusal `json:"refusal"`
}

type sidecarComputeRefusal struct {
	Code         string `json:"code"`
	RetryAfterMs uint32 `json:"retry_after_ms,omitempty"`
}

// The params the daemon's methods take, as compute_daemon.go names them; the daemon reads params strictly.
type sidecarControlComputeParams struct {
	Network string `json:"network,omitempty"`
}

type sidecarControlComputePacksParams struct {
	Network string `json:"network,omitempty"`
	Market  string `json:"market"`
}

type sidecarControlComputeRedeemParams struct {
	Network string `json:"network,omitempty"`
	Code    string `json:"code"`
}

type sidecarControlComputeChargesParams struct {
	Network string `json:"network,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
}

type sidecarControlComputeSpendParams struct {
	Network string `json:"network,omitempty"`
	By      string `json:"by,omitempty"`
}

// The local core's compute client, or the remote backend to call; one of them.
func (self *SidecarCompute) backend() (*sidecar.ComputeClient, sidecarBackend, error) {
	switch {
	case self.args.local != nil:
		client, err := self.args.local.ComputeClient()
		return client, nil, err
	case self.args.remote != nil:
		return nil, self.args.remote, nil
	}
	return nil, nil, errSidecarComputeNoCore
}

// The Sidecar's context, which its Close ends; a Sidecar made with none, as a test makes one, never ends.
func (self *SidecarCompute) ctx() context.Context {
	if self.args.ctx != nil {
		return self.args.ctx
	}
	return context.Background()
}

// The refusal reply err carries, where api answered it with a code; nil otherwise.
func sidecarComputeRefusalOf(err error) *sidecarComputeRefusalReply {
	var apiErr *sidecar.SidecarApiError
	if errors.As(err, &apiErr) && apiErr.Code != "" {
		return &sidecarComputeRefusalReply{Refusal: &sidecarComputeRefusal{Code: apiErr.Code, RetryAfterMs: apiErr.RetryAfterMs}}
	}
	return nil
}

// A local answer as JSON: the value, a refusal reply where api refused with a code, or the error.
func sidecarComputeJson(value any, err error) (string, error) {
	if refusal := sidecarComputeRefusalOf(err); refusal != nil {
		value = refusal
	} else if err != nil {
		return "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// A remote call's answer as JSON, as the daemon wrote it.
func (self *SidecarCompute) remoteJson(remote sidecarBackend, method string, params any, confirmed bool) (string, error) {
	var result json.RawMessage
	if err := remote.call(self.ctx(), method, params, confirmed, &result); err != nil {
		return "", err
	}
	return string(result), nil
}

// The refusal a remote answer is, nil for any other answer.
func sidecarComputeRemoteRefusal(answer string) *sidecarComputeRefusal {
	reply := &sidecarComputeRefusalReply{}
	if json.Unmarshal([]byte(answer), reply) == nil && reply.Refusal != nil && reply.Refusal.Code != "" {
		return reply.Refusal
	}
	return nil
}

// The network's balance as the person may see it, JSON (§9.7, §9.10).
func (self *SidecarCompute) Balance() (string, error) {
	return traceWithReturnError(func() (string, error) {
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodComputeBalance, &sidecarControlComputeParams{Network: sidecarSupportNetwork(remote)}, false)
		}
		return sidecarComputeJson(client.Balance(self.ctx()))
	})
}

// What Packs answers: the products, offered only where the person may buy and sales are open, and why none are.
type sidecarComputePacks struct {
	Market    string                 `json:"market"`
	MayBuy    bool                   `json:"may_buy"`
	SalesOpen bool                   `json:"sales_open"`
	Packs     []*sidecar.ComputePack `json:"packs"`
}

// The balance's two facts Packs reads, from its JSON.
type sidecarComputeBuyable struct {
	MayBuy    bool `json:"may_buy"`
	SalesOpen bool `json:"sales_open"`
}

// What market sells for compute credit (stripe, app_store or google_play), JSON, only where Balance says the person
// may buy and sales are open (Q-BAL-13); otherwise no products, with may_buy and sales_open saying why.
func (self *SidecarCompute) Packs(market string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		parsed, err := sidecar.ParseComputeMarket(market)
		if err != nil {
			return "", errSidecarComputeRequest
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		answer := &sidecarComputePacks{Market: parsed.String(), Packs: []*sidecar.ComputePack{}}
		if remote != nil {
			network := sidecarSupportNetwork(remote)
			balanceJson, err := self.remoteJson(remote, sidecarControlMethodComputeBalance, &sidecarControlComputeParams{Network: network}, false)
			if err != nil {
				return "", err
			}
			if sidecarComputeRemoteRefusal(balanceJson) != nil {
				return balanceJson, nil
			}
			buyable := &sidecarComputeBuyable{}
			if err := json.Unmarshal([]byte(balanceJson), buyable); err != nil {
				return "", err
			}
			answer.MayBuy, answer.SalesOpen = buyable.MayBuy, buyable.SalesOpen
			if !answer.MayBuy || !answer.SalesOpen {
				return sidecarComputeJson(answer, nil)
			}
			reply := struct {
				Packs []*sidecar.ComputePack `json:"packs"`
			}{}
			var result json.RawMessage
			if err := remote.call(self.ctx(), sidecarControlMethodComputePacks, &sidecarControlComputePacksParams{Network: network, Market: parsed.String()}, false, &result); err != nil {
				return "", err
			}
			if err := json.Unmarshal(result, &reply); err != nil {
				return "", err
			}
			answer.Packs = reply.Packs
			return sidecarComputeJson(answer, nil)
		}
		balance, err := client.Balance(self.ctx())
		if err != nil {
			return sidecarComputeJson(nil, err)
		}
		answer.MayBuy, answer.SalesOpen = balance.MayBuy, balance.SalesOpen
		if !answer.MayBuy || !answer.SalesOpen {
			return sidecarComputeJson(answer, nil)
		}
		packs, err := client.Packs(parsed)
		if err != nil {
			return "", err
		}
		answer.Packs = packs
		return sidecarComputeJson(answer, nil)
	})
}

// Charges' request.
type sidecarComputeChargesRequest struct {
	// The page's cursor, as the last page answered it; empty for the first.
	Cursor string `json:"cursor,omitempty"`
}

// A page of the charges the person may see, JSON, every id named on this device (§9.10, §9.13).
func (self *SidecarCompute) Charges(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarComputeChargesRequest{}
		if err := sidecarComputeDecode(requestJson, request); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodComputeCharges, &sidecarControlComputeChargesParams{Network: sidecarSupportNetwork(remote), Cursor: request.Cursor}, false)
		}
		return sidecarComputeJson(client.Charges(self.ctx(), request.Cursor))
	})
}

// A request's JSON into request, strictly: an unknown field is refused, and an empty request is the zero one.
func sidecarComputeDecode(requestJson string, request any) error {
	if requestJson == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(requestJson))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fmt.Errorf("%w: %w", errSidecarComputeRequest, err)
	}
	return nil
}

// The Stripe Checkout url for the purchase requestJson names (§9.5): a pack's credits, the auto-renew mode its paying
// person turns on in that act, and that mode's amounts, as POST /compute/checkout takes them. The app opens the url in
// the person's browser; the person pays there.
func (self *SidecarCompute) Checkout(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		args := &sidecar.ComputeCheckoutArgs{}
		if err := sidecarComputeDecode(requestJson, args); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		var checkout *sidecar.ComputeCheckout
		if remote != nil {
			params := &struct {
				Network string `json:"network,omitempty"`
				*sidecar.ComputeCheckoutArgs
			}{Network: sidecarSupportNetwork(remote), ComputeCheckoutArgs: args}
			answer, err := self.remoteJson(remote, sidecarControlMethodComputeCheckout, params, false)
			if err != nil {
				return "", err
			}
			if refusal := sidecarComputeRemoteRefusal(answer); refusal != nil {
				return "", &sidecarComputeRefusalError{code: refusal.Code}
			}
			checkout = &sidecar.ComputeCheckout{}
			if err := json.Unmarshal([]byte(answer), checkout); err != nil {
				return "", err
			}
		} else {
			checkout, err = client.Checkout(self.ctx(), args)
			if refusal := sidecarComputeRefusalOf(err); refusal != nil {
				return "", &sidecarComputeRefusalError{code: refusal.Refusal.Code}
			}
			if err != nil {
				return "", err
			}
		}
		if checkout.Url == "" {
			return "", fmt.Errorf("%w: the checkout has no url", errSidecarComputeRequest)
		}
		return checkout.Url, nil
	})
}

// A promotion code redeemed for this network (§9.11): the person's own act in the app's UI, after they typed the code.
// JSON: the redemption's lot, its credit and the day it lapses, or {"refusal": {"code"}} with the code's reason
// (code_unknown, code_ended, code_redeemed, redeemed_recently, paid_lot_required, daily_limit, balance_cap,
// rate_limited). The code is never logged.
func (self *SidecarCompute) Redeem(code string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			// the Confirm method, sent confirmed: the person typed the code in the app's UI
			return self.remoteJson(remote, sidecarControlMethodComputeRedeem, &sidecarControlComputeRedeemParams{Network: sidecarSupportNetwork(remote), Code: code}, true)
		}
		return sidecarComputeJson(client.Redeem(self.ctx(), code))
	})
}

// One of §9.8's modes changed, as POST /compute/auto-renew takes it: the person's own act in the app's UI. A mode
// turned on names a card the person turning it on saved for it, else the change is refused card_required and the person
// checks out a pack with the mode on.
func (self *SidecarCompute) AutoRenew(changeJson string) error {
	return traceWithError(func() error {
		change := &sidecar.ComputeAutoRenewChange{}
		if err := sidecarComputeDecode(changeJson, change); err != nil {
			return err
		}
		if change.Mode != sidecar.ComputeAutoRenewThreshold && change.Mode != sidecar.ComputeAutoRenewMonthly {
			return errSidecarComputeRequest
		}
		client, remote, err := self.backend()
		if err != nil {
			return err
		}
		if remote != nil {
			params := &struct {
				Network string `json:"network,omitempty"`
				*sidecar.ComputeAutoRenewChange
			}{Network: sidecarSupportNetwork(remote), ComputeAutoRenewChange: change}
			// the Confirm method, sent confirmed: the person changed it in the app's UI
			answer, err := self.remoteJson(remote, sidecarControlMethodComputeAutoRenewSet, params, true)
			if err != nil {
				return err
			}
			if refusal := sidecarComputeRemoteRefusal(answer); refusal != nil {
				return &sidecarComputeRefusalError{code: refusal.Code}
			}
			return nil
		}
		_, err = client.SetAutoRenew(self.ctx(), change)
		if refusal := sidecarComputeRefusalOf(err); refusal != nil {
			return &sidecarComputeRefusalError{code: refusal.Refusal.Code}
		}
		return err
	})
}

// A raise of an agent's or a gateway grant's budget for a day, a month or a run, filed as kind 21 for the person to
// approve on an approval-capable device (§16.3): JSON, done once the approved raise reached the broker, or
// approval_pending with the approval's id. Refused before anything is filed while kind 21's effect is not in this
// build (BAL-M12-16).
func (self *SidecarCompute) RaiseBudget(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		args := &sidecar.ComputeBudgetRaiseArgs{}
		if err := sidecarComputeDecode(requestJson, args); err != nil {
			return "", err
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			params := &struct {
				Network string `json:"network,omitempty"`
				*sidecar.ComputeBudgetRaiseArgs
			}{Network: sidecarSupportNetwork(remote), ComputeBudgetRaiseArgs: args}
			return self.remoteJson(remote, sidecarControlMethodComputeBudgetRaise, params, false)
		}
		return sidecarComputeJson(client.RaiseBudget(self.ctx(), args))
	})
}

// Spend's request.
type sidecarComputeSpendRequest struct {
	// provider, model, enclave, member or agent; empty for none.
	By string `json:"by,omitempty"`
}

// §9.13's spend view, JSON: each service's total since the network's first charge and its last 7 UTC days, with the
// window's bounds, net of refunds, broken down by the request's dimension, every id named on this device from its
// signed sources, an unresolved one as unknown.
func (self *SidecarCompute) Spend(requestJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		request := &sidecarComputeSpendRequest{}
		if err := sidecarComputeDecode(requestJson, request); err != nil {
			return "", err
		}
		by, err := sidecar.ParseComputeSpendBy(request.By)
		if err != nil {
			return "", errSidecarComputeRequest
		}
		client, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodComputeSpend, &sidecarControlComputeSpendParams{Network: sidecarSupportNetwork(remote), By: string(by)}, false)
		}
		return sidecarComputeJson(client.Spend(self.ctx(), by))
	})
}

// The app's Api calls a store purchase's report takes: *Api's, which hold the network's credential.
type sidecarComputeStoreApi interface {
	VerifyAppleTransactionSyncWithContext(ctx context.Context, args *VerifyAppleTransactionArgs) (*VerifyStorePurchaseResult, error)
	VerifyPlayPurchaseSyncWithContext(ctx context.Context, args *VerifyPlayPurchaseArgs) (*VerifyStorePurchaseResult, error)
}

// A store purchase's report, as the app hands it.
type sidecarComputeStorePurchase struct {
	// app_store: the StoreKit transaction's JWS (Transaction.jwsRepresentation).
	SignedTransaction string `json:"signed_transaction,omitempty"`
	// google_play: the product the app believes it bought, its purchase token and, optionally, the package.
	ProductId     string `json:"product_id,omitempty"`
	PurchaseToken string `json:"purchase_token,omitempty"`
	PackageName   string `json:"package_name,omitempty"`
	// google_play: a consumable, which the server consumes, rather than a subscription, which the app acknowledges.
	Consumable bool `json:"consumable,omitempty"`
	// How many reports of this proof went before, from 0: the next retry's wait is PurchaseReportBackoffMillis's.
	Attempt int32 `json:"attempt,omitempty"`
}

// What StorePurchase answers.
type sidecarComputeStorePurchaseAnswer struct {
	Market string `json:"market"`
	// One of PurchaseReportStatus*.
	Status string `json:"status"`
	// IsPurchaseReportTerminal's: the report loop ends.
	Terminal bool `json:"terminal"`
	// What to do with the store now: keep, finish, acknowledge or none.
	Finalize string `json:"finalize"`
	// While not terminal, how long to wait before the next report.
	RetryAfterMs int64 `json:"retry_after_ms"`
}

// Reports a store purchase of compute credit (§9.5, §14.3, D-BAL-8) through api, the app's own, with the network's
// credential, as Pro's purchases report: market is app_store or google_play and purchaseJson its proof. JSON: the
// status, whether it is terminal, what to do with the store now and, while not terminal, when to report again. A
// transport failure is an error, never terminal: keep the proof and report again (sdk/purchase_report.go).
func (self *SidecarCompute) StorePurchase(api *Api, market string, purchaseJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		if api == nil {
			return "", fmt.Errorf("%w: the app's Api reports a purchase", errSidecarComputeRequest)
		}
		return sidecarComputeStorePurchaseReport(self.ctx(), api, market, purchaseJson)
	})
}

// StorePurchase over any Api the report's calls take.
func sidecarComputeStorePurchaseReport(ctx context.Context, api sidecarComputeStoreApi, market string, purchaseJson string) (string, error) {
	purchase := &sidecarComputeStorePurchase{}
	if err := sidecarComputeDecode(purchaseJson, purchase); err != nil {
		return "", err
	}
	answer := &sidecarComputeStorePurchaseAnswer{Market: market}
	var result *VerifyStorePurchaseResult
	var err error
	switch market {
	case sidecarComputeMarketAppStore:
		if purchase.SignedTransaction == "" || purchase.PurchaseToken != "" {
			return "", errSidecarComputeRequest
		}
		result, err = api.VerifyAppleTransactionSyncWithContext(ctx, &VerifyAppleTransactionArgs{SignedTransaction: purchase.SignedTransaction})
	case sidecarComputeMarketGooglePlay:
		if purchase.PurchaseToken == "" || purchase.ProductId == "" || purchase.SignedTransaction != "" {
			return "", errSidecarComputeRequest
		}
		result, err = api.VerifyPlayPurchaseSyncWithContext(ctx, &VerifyPlayPurchaseArgs{
			PackageName: purchase.PackageName, ProductId: purchase.ProductId, PurchaseToken: purchase.PurchaseToken,
		})
	default:
		return "", errSidecarComputeRequest
	}
	if err != nil {
		return "", err
	}
	if result == nil || result.Status == "" {
		return "", fmt.Errorf("%w: the report's answer names no status", errSidecarComputeRequest)
	}
	answer.Status = result.Status
	answer.Terminal = IsPurchaseReportTerminal(result.Status)
	switch {
	case !answer.Terminal:
		answer.Finalize = SidecarComputeFinalizeKeep
		answer.RetryAfterMs = PurchaseReportBackoffMillis(purchase.Attempt)
	case market == sidecarComputeMarketAppStore:
		// a terminal answer of any kind: finish, so StoreKit stops redelivering it
		answer.Finalize = SidecarComputeFinalizeFinish
	case purchase.Consumable || result.Status == PurchaseReportStatusInvalid:
		answer.Finalize = SidecarComputeFinalizeNone
	default:
		answer.Finalize = SidecarComputeFinalizeAcknowledge
	}
	return sidecarComputeJson(answer, nil)
}
