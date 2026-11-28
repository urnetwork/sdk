package sdk

// Disconnecting the network's payout wallet (server POST
// /sn/wallet/disconnect-code and POST /sn/wallet/disconnect). The network
// owner confirms it with a code the server emails to the account's sign-in
// addresses, so a stolen session alone cannot do it, and the server emails
// the payout wallet security notice. Afterwards the network has no payout
// wallet: its earnings can't be paid out until a wallet is connected and a
// payout consent is signed again. Earnings already paid stay claimable by the
// wallet they were paid to.
//
// Only a wallet connected without a signed payout consent can be
// disconnected: the network's connected Bittensor wallet and the providers'
// wallets set without a consent. A signed network consent or hotkey
// delegation ends only by a signed replacement (Change wallet), so the server
// answers SnWalletDisconnectErrorCodeConsentInForce while one is in force,
// and a provider's own signed consent is never touched.
//
// The routes administer the account, so the api sends them with the network
// credential (apiAdminRouteAccess).

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

// The codes of a disconnect refusal (SnWalletDisconnectError.Code).
const (
	// only the network's own sign-in can disconnect its wallet (not an API
	// key)
	SnWalletDisconnectErrorCodeOwnerOnly = "owner_only"
	// the network has no wallet to disconnect
	SnWalletDisconnectErrorCodeNoWallet = "no_wallet"
	// a signed payout consent or hotkey delegation pays the network's
	// earnings; only a signed replacement changes it (Change wallet)
	SnWalletDisconnectErrorCodeConsentInForce = "consent_in_force"
	// the account has no sign-in email to send the code to; add an email
	// sign-in first
	SnWalletDisconnectErrorCodeEmailRequired = "email_required"
	// too many codes; RetryAfterSeconds says when to ask again
	SnWalletDisconnectErrorCodeRateLimited = "rate_limited"
	// the code could not be sent; ask again
	SnWalletDisconnectErrorCodeSendFailed = "send_failed"
	// the code is wrong; it still works for its remaining tries
	SnWalletDisconnectErrorCodeCodeInvalid = "code_invalid"
	// no code is waiting (expired, used, or ended by too many wrong tries);
	// ask for a new one
	SnWalletDisconnectErrorCodeCodeExpired = "code_expired"
)

type SnWalletDisconnectError struct {
	// one of the SnWalletDisconnectErrorCode* values, or a newer code the app
	// does not know
	Code    string `json:"code"`
	Message string `json:"message"`
	// for SnWalletDisconnectErrorCodeRateLimited, seconds until a code can be
	// asked again; 0 when unknown
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
}

type SnWalletDisconnectCodeArgs struct {
}

type SnWalletDisconnectCodeResult struct {
	// the masked sign-in email addresses the code was sent to
	SentTo *StringList `json:"sent_to,omitempty"`
	// when the code stops working, unix milliseconds
	ExpiresAtMillis int64                    `json:"expires_at_millis,omitempty"`
	Error           *SnWalletDisconnectError `json:"error,omitempty"`
}

type SnWalletDisconnectCodeCallback connect.ApiCallback[*SnWalletDisconnectCodeResult]

type SnWalletDisconnectArgs struct {
	Code string `json:"code"`
}

type SnWalletDisconnectResult struct {
	// the network now has no connected payout wallet
	Disconnected bool                     `json:"disconnected"`
	Error        *SnWalletDisconnectError `json:"error,omitempty"`
}

type SnWalletDisconnectCallback connect.ApiCallback[*SnWalletDisconnectResult]

// Whether the wallets include one the network owner can disconnect: a wallet
// connected without a consent (the network's connected wallet, or a
// provider's wallet set without one), while no signed network consent or
// hotkey delegation pays the network. A provider's own consent neither counts
// nor blocks: the disconnect leaves it. The server decides again when the
// disconnect is asked for, at the current epoch.
func (self *SnGetWalletResult) Disconnectable() bool {
	if self == nil || self.Wallets == nil {
		return false
	}
	unsigned := false
	for _, wallet := range self.Wallets.getAll() {
		if wallet == nil || wallet.ColdkeySs58 == "" {
			continue
		}
		switch wallet.ConsentScope {
		case "":
			unsigned = true
		case SnWalletConsentScopeNetwork, SnWalletConsentScopeHotkey:
			return false
		}
	}
	return unsigned
}

// Whether any wallet is attached to the network: when there is none, the
// network's earnings can't be paid out.
func (self *SnGetWalletResult) HasWallet() bool {
	if self == nil || self.Wallets == nil {
		return false
	}
	for _, wallet := range self.Wallets.getAll() {
		if wallet != nil && wallet.ColdkeySs58 != "" {
			return true
		}
	}
	return false
}

// SnGetWalletResult.Disconnectable as a function, for the c abi, where the
// result crosses as json.
func SnWalletDisconnectable(result *SnGetWalletResult) bool {
	return result.Disconnectable()
}

// SnGetWalletResult.HasWallet as a function, for the c abi.
func SnWalletHasWallet(result *SnGetWalletResult) bool {
	return result.HasWallet()
}

// SnWalletDisconnectCode emails the code that confirms the disconnect (POST
// /sn/wallet/disconnect-code). A new code replaces the last one.
func (self *Api) SnWalletDisconnectCode(callback SnWalletDisconnectCodeCallback) {
	runAsyncApiRequest[*SnWalletDisconnectCodeResult](callback, func(callback connect.ApiCallback[*SnWalletDisconnectCodeResult]) {
		self.snWalletDisconnectCodeWithCallback(self.ctx, callback)
	})
}

//gomobile:noexport
func (self *Api) SnWalletDisconnectCodeSyncWithContext(ctx context.Context) (*SnWalletDisconnectCodeResult, error) {
	return self.snWalletDisconnectCodeWithCallback(ctx, connect.NewNoopApiCallback[*SnWalletDisconnectCodeResult]())
}

func (self *Api) snWalletDisconnectCodeWithCallback(ctx context.Context, callback connect.ApiCallback[*SnWalletDisconnectCodeResult]) (*SnWalletDisconnectCodeResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet/disconnect-code", self.apiUrl),
		&SnWalletDisconnectCodeArgs{},
		self.GetByJwt(),
		&SnWalletDisconnectCodeResult{},
		callback,
	)
}

// SnWalletDisconnect disconnects the network's payout wallet with the emailed
// code (POST /sn/wallet/disconnect). The code is normalized as an email
// change code is (NormalizeEmailChangeCode).
func (self *Api) SnWalletDisconnect(code string, callback SnWalletDisconnectCallback) {
	runAsyncApiRequest[*SnWalletDisconnectResult](callback, func(callback connect.ApiCallback[*SnWalletDisconnectResult]) {
		self.snWalletDisconnectWithCallback(self.ctx, code, callback)
	})
}

//gomobile:noexport
func (self *Api) SnWalletDisconnectSyncWithContext(ctx context.Context, code string) (*SnWalletDisconnectResult, error) {
	return self.snWalletDisconnectWithCallback(ctx, code, connect.NewNoopApiCallback[*SnWalletDisconnectResult]())
}

func (self *Api) snWalletDisconnectWithCallback(ctx context.Context, code string, callback connect.ApiCallback[*SnWalletDisconnectResult]) (*SnWalletDisconnectResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet/disconnect", self.apiUrl),
		&SnWalletDisconnectArgs{Code: NormalizeEmailChangeCode(code)},
		self.GetByJwt(),
		&SnWalletDisconnectResult{},
		callback,
	)
}
