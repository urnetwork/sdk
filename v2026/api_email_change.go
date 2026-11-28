package sdk

// The account email change (server POST /auth/email-change and its confirm,
// cancel and status routes). An app asks for the change with the new address;
// the server sends a code to the current email sign-in and another to the new
// address, and the change completes only when both codes are confirmed before
// the request expires. The password stays the same. The server emails a
// security notice to the account's sign-in addresses when the change is
// requested and when it completes, and completing it signs out the account's
// other sessions.
//
// These routes administer the account, so the api sends them with the network
// credential (apiAdminRouteAccess) and fails before sending without one
// (ErrNetworkCredentialRequired).
//
// Refusals come back in the result's Error with one of the
// AuthEmailChangeErrorCode* codes, which an app words itself, falling back to
// the message for a code it does not know.

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/urnetwork/connect/v2026"
)

// The codes of an email change refusal (AuthEmailChangeError.Code).
const (
	// only the network's own sign-in can change its email (not an API key)
	AuthEmailChangeErrorCodeOwnerOnly = "owner_only"
	// the account has no email sign-in to change; add one instead
	AuthEmailChangeErrorCodeNoEmail = "no_email"
	// the current email sign-in is not verified yet
	AuthEmailChangeErrorCodeEmailUnverified = "email_unverified"
	// the new address is not an email address
	AuthEmailChangeErrorCodeInvalidEmail = "invalid_email"
	// the new address is the current email
	AuthEmailChangeErrorCodeSameEmail = "same_email"
	// another account signs in with the new address
	AuthEmailChangeErrorCodeEmailInUse = "email_in_use"
	// too many requests or codes; RetryAfterSeconds says when to ask again
	AuthEmailChangeErrorCodeRateLimited = "rate_limited"
	// a code could not be sent; ask again
	AuthEmailChangeErrorCodeSendFailed = "send_failed"
	// no email change is waiting: it expired, was cancelled, completed, or was
	// never started
	AuthEmailChangeErrorCodeNoChange = "no_change"
	// a code is wrong; the request is still waiting
	AuthEmailChangeErrorCodeCodeInvalid = "code_invalid"
	// too many wrong codes; the request ended
	AuthEmailChangeErrorCodeTooManyTries = "too_many_tries"
)

// The longest email address the server accepts (RFC 5321 path limit).
const authEmailChangeMaxAddressLength = 254

type AuthEmailChangeError struct {
	// one of the AuthEmailChangeErrorCode* values, or a newer code the app
	// does not know
	Code    string `json:"code"`
	Message string `json:"message"`
	// for AuthEmailChangeErrorCodeRateLimited, seconds until it can be asked
	// again; 0 when unknown
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
}

// Whether the refusal ended the waiting request, so the app goes back to
// asking for the new address: nothing is waiting, too many wrong codes ended
// it, or the new address was taken before the change completed.
func (self *AuthEmailChangeError) EndsRequest() bool {
	if self == nil {
		return false
	}
	switch self.Code {
	case AuthEmailChangeErrorCodeNoChange, AuthEmailChangeErrorCodeTooManyTries, AuthEmailChangeErrorCodeEmailInUse:
		return true
	default:
		return false
	}
}

type AuthEmailChangeArgs struct {
	NewEmail string `json:"new_email"`
}

type AuthEmailChangeResult struct {
	// the current email, masked ("j…@example.com"); it was sent a code
	CurrentEmail string `json:"current_email,omitempty"`
	// the new email as the server normalized it; it was sent a code
	NewEmail string `json:"new_email,omitempty"`
	// when the request ends unless both codes were confirmed, unix
	// milliseconds
	ExpiresAtMillis int64                 `json:"expires_at_millis,omitempty"`
	Error           *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeCallback connect.ApiCallback[*AuthEmailChangeResult]

// The account's waiting email change, for an app that comes back to the flow.
type AuthEmailChangeStatusResult struct {
	Pending bool `json:"pending"`
	// the current email, masked
	CurrentEmail     string                `json:"current_email,omitempty"`
	NewEmail         string                `json:"new_email,omitempty"`
	CurrentConfirmed bool                  `json:"current_confirmed,omitempty"`
	NewConfirmed     bool                  `json:"new_confirmed,omitempty"`
	ExpiresAtMillis  int64                 `json:"expires_at_millis,omitempty"`
	Error            *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeStatusCallback connect.ApiCallback[*AuthEmailChangeStatusResult]

type AuthEmailChangeConfirmArgs struct {
	// the code sent to the current email; "" to confirm only the new one
	CurrentCode string `json:"current_code,omitempty"`
	// the code sent to the new email; "" to confirm only the current one
	NewCode string `json:"new_code,omitempty"`
}

type AuthEmailChangeConfirmResult struct {
	CurrentConfirmed bool `json:"current_confirmed"`
	NewConfirmed     bool `json:"new_confirmed"`
	// both addresses are confirmed and the account's email sign-in is Email
	Complete bool   `json:"complete"`
	Email    string `json:"email,omitempty"`
	// the account's other sessions were signed out when the change completed.
	// False when the current sign-in has no session to keep: the Sessions
	// screen signs them out then.
	OtherSessionsSignedOut bool                  `json:"other_sessions_signed_out,omitempty"`
	Error                  *AuthEmailChangeError `json:"error,omitempty"`
}

// Whether the code sent to the current email is still to be entered.
func (self *AuthEmailChangeConfirmResult) NeedsCurrentCode() bool {
	return self != nil && !self.Complete && !self.CurrentConfirmed
}

// Whether the code sent to the new email is still to be entered.
func (self *AuthEmailChangeConfirmResult) NeedsNewCode() bool {
	return self != nil && !self.Complete && !self.NewConfirmed
}

type AuthEmailChangeConfirmCallback connect.ApiCallback[*AuthEmailChangeConfirmResult]

type AuthEmailChangeCancelArgs struct {
}

type AuthEmailChangeCancelResult struct {
	Error *AuthEmailChangeError `json:"error,omitempty"`
}

type AuthEmailChangeCancelCallback connect.ApiCallback[*AuthEmailChangeCancelResult]

// The local check of a new address before it is sent: "" when it can be an
// email address, else AuthEmailChangeErrorCodeInvalidEmail. The server makes
// the authoritative check (and the in-use check) when the change starts.
func ValidateEmailChangeAddress(email string) string {
	email = strings.TrimSpace(email)
	if email == "" || authEmailChangeMaxAddressLength < len(email) || !utf8.ValidString(email) {
		return AuthEmailChangeErrorCodeInvalidEmail
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return AuthEmailChangeErrorCodeInvalidEmail
		}
	}
	mailbox, domain, found := strings.Cut(email, "@")
	if !found || mailbox == "" || domain == "" || strings.Contains(domain, "@") {
		return AuthEmailChangeErrorCodeInvalidEmail
	}
	// a domain with a dot that neither starts nor ends it
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return AuthEmailChangeErrorCodeInvalidEmail
	}
	return ""
}

// A code as the person typed it, as the server reads it: spaces and dashes
// dropped (a code can be pasted grouped).
func NormalizeEmailChangeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' {
			return -1
		}
		return r
	}, code)
}

// The account's email sign-in, the address an email change replaces: the
// email among UserAuths, else "" (the account has none and adds one
// instead).
func (self *NetworkUser) EmailSignIn() string {
	if self == nil || self.UserAuths == nil {
		return ""
	}
	for _, userAuth := range self.UserAuths.getAll() {
		if userAuth != nil && userAuth.AuthType == "email" && userAuth.UserAuth != "" {
			return userAuth.UserAuth
		}
	}
	return ""
}

// Whether the account has an email sign-in to change.
func (self *NetworkUser) EmailChangeAvailable() bool {
	return self.EmailSignIn() != ""
}

// NetworkUser.EmailSignIn as a function, for the c abi, where the user
// crosses as json.
func NetworkUserEmailSignIn(user *NetworkUser) string {
	return user.EmailSignIn()
}

// AuthEmailChangeError.EndsRequest as a function, for the c abi.
func AuthEmailChangeErrorEndsRequest(err *AuthEmailChangeError) bool {
	return err.EndsRequest()
}

// AuthEmailChange starts the change to args.NewEmail (POST /auth/email-change).
func (self *Api) AuthEmailChange(args *AuthEmailChangeArgs, callback AuthEmailChangeCallback) {
	runAsyncApiRequest[*AuthEmailChangeResult](callback, func(callback connect.ApiCallback[*AuthEmailChangeResult]) {
		self.authEmailChangeWithCallback(self.ctx, args, callback)
	})
}

//gomobile:noexport
func (self *Api) AuthEmailChangeSyncWithContext(ctx context.Context, args *AuthEmailChangeArgs) (*AuthEmailChangeResult, error) {
	return self.authEmailChangeWithCallback(ctx, args, connect.NewNoopApiCallback[*AuthEmailChangeResult]())
}

func (self *Api) authEmailChangeWithCallback(ctx context.Context, args *AuthEmailChangeArgs, callback connect.ApiCallback[*AuthEmailChangeResult]) (*AuthEmailChangeResult, error) {
	if args != nil {
		trimmed := *args
		trimmed.NewEmail = strings.TrimSpace(trimmed.NewEmail)
		args = &trimmed
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/auth/email-change", self.apiUrl),
		args,
		self.GetByJwt(),
		&AuthEmailChangeResult{},
		callback,
	)
}

// AuthEmailChangeStatus reads the account's waiting email change (GET
// /auth/email-change).
func (self *Api) AuthEmailChangeStatus(callback AuthEmailChangeStatusCallback) {
	runAsyncApiRequest[*AuthEmailChangeStatusResult](callback, func(callback connect.ApiCallback[*AuthEmailChangeStatusResult]) {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/auth/email-change", self.apiUrl),
			self.GetByJwt(),
			&AuthEmailChangeStatusResult{},
			callback,
		)
	})
}

// AuthEmailChangeConfirm confirms the waiting change with one or both codes
// (POST /auth/email-change/confirm). The codes are normalized
// (NormalizeEmailChangeCode) before they are sent.
func (self *Api) AuthEmailChangeConfirm(args *AuthEmailChangeConfirmArgs, callback AuthEmailChangeConfirmCallback) {
	runAsyncApiRequest[*AuthEmailChangeConfirmResult](callback, func(callback connect.ApiCallback[*AuthEmailChangeConfirmResult]) {
		self.authEmailChangeConfirmWithCallback(self.ctx, args, callback)
	})
}

//gomobile:noexport
func (self *Api) AuthEmailChangeConfirmSyncWithContext(ctx context.Context, args *AuthEmailChangeConfirmArgs) (*AuthEmailChangeConfirmResult, error) {
	return self.authEmailChangeConfirmWithCallback(ctx, args, connect.NewNoopApiCallback[*AuthEmailChangeConfirmResult]())
}

func (self *Api) authEmailChangeConfirmWithCallback(ctx context.Context, args *AuthEmailChangeConfirmArgs, callback connect.ApiCallback[*AuthEmailChangeConfirmResult]) (*AuthEmailChangeConfirmResult, error) {
	if args != nil {
		normalized := AuthEmailChangeConfirmArgs{
			CurrentCode: NormalizeEmailChangeCode(args.CurrentCode),
			NewCode:     NormalizeEmailChangeCode(args.NewCode),
		}
		args = &normalized
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/auth/email-change/confirm", self.apiUrl),
		args,
		self.GetByJwt(),
		&AuthEmailChangeConfirmResult{},
		callback,
	)
}

// AuthEmailChangeCancel ends the waiting change (POST
// /auth/email-change/cancel); its codes stop working.
func (self *Api) AuthEmailChangeCancel(callback AuthEmailChangeCancelCallback) {
	runAsyncApiRequest[*AuthEmailChangeCancelResult](callback, func(callback connect.ApiCallback[*AuthEmailChangeCancelResult]) {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/email-change/cancel", self.apiUrl),
			&AuthEmailChangeCancelArgs{},
			self.GetByJwt(),
			&AuthEmailChangeCancelResult{},
			callback,
		)
	})
}
