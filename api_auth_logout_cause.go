package sdk

// Why the server ended a sign-in, for apps that explain a logout
// (server session/REVOKE-FINAL.md §9-§10, REVOKE-UI-FINAL.md §5).
//
// A confirmed rejection (an all-leaf HTTP 401, ConfirmedClientRefreshRejection)
// signs the API out. Only the server's structured refusal of a revoked session,
// a 401 whose JSON body is {"code":"session_revoked",...} (server
// session.SessionError), is a trusted cause. A generic 401, any other code, a
// body that is not exactly that JSON, and any other status carry no cause, so
// apps fall back to generic sign-in-required wording.
//
// A session this API asked the server to revoke itself (Api.SignOut, or a
// revoke of its own current session) answers later requests with the same
// code. That cutoff is this sign-out, not another device's, so it carries no
// cause either.
//
// The cause travels with the rejection: rejectByJwt and rejectNetworkCredential
// record it with the rejected credential, under the same generation check that
// keeps a delayed 401 for an older credential from touching a newer login.

import (
	"net/http"

	"github.com/urnetwork/connect"
)

// The AuthLogout cause of a sign-in session the server confirmed as revoked,
// for example signed out from another device. Every other logout has the
// cause "".
const AuthLogoutCauseSessionRevoked = "session_revoked"

// AuthLogoutCauseSessionRevoked when every leaf of the error is a 401 whose
// body is the server's structured session_revoked refusal, and "" otherwise.
// The body must be one JSON object with exactly one canonical "code" string
// field (decodeClientControlJson), so a duplicated, case-folded or trailing
// field cannot introduce the code.
func confirmedRejectionCause(err error) string {
	if clientControlErrorIsNil(err) {
		return ""
	}
	if status, ok := err.(*connect.HttpStatusError); ok {
		if status.StatusCode != http.StatusUnauthorized {
			return ""
		}
		var refusal struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if decodeClientControlJson(status.Body, &refusal) != nil || refusal.Code != AuthLogoutCauseSessionRevoked {
			return ""
		}
		return AuthLogoutCauseSessionRevoked
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return ""
		}
		for _, cause := range causes {
			if confirmedRejectionCause(cause) == "" {
				return ""
			}
		}
		return AuthLogoutCauseSessionRevoked
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return confirmedRejectionCause(wrapped.Unwrap())
	}
	return ""
}

// The sign-in session a credential names, read unverified, or nil for an API
// key or an untagged credential.
func credentialSessionId(byJwt string) *connect.Id {
	claims, err := connect.ParseByJwtUnverified(byJwt)
	if err != nil {
		return nil
	}
	return claims.SessionId
}

// Why the server ended the sign-in that the last AuthLogout reported:
// AuthLogoutCauseSessionRevoked when it confirmed the session was revoked,
// which apps show as "This session was signed out from another device.", else
// "". A new login clears this mutable state, even before an older callback
// runs. AddAuthLogoutEventListener carries the original cause and owner.
func (self *Api) GetAuthLogoutCause() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.authLogoutCause
}

// Why the account's sign-in needs signing in again while this device stays
// signed in, for the app's notice when AccountSignInRequired fires (server
// session/REVOKE-FINAL.md R9: the rejected account credential is cleared, the
// unrelated client or device is kept, and the app says that the account needs
// a sign-in): AuthLogoutCauseSessionRevoked when the server confirmed the
// account sign-in's session was revoked from another device, which apps say
// as "This session was signed out from another device.", else "", for the
// generic "Sign in again to manage sessions.". Read it inside the listener;
// a new login clears it.
func (self *Api) GetAccountSignInRequiredCause() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.signInRejected {
		return ""
	}
	return self.signInRejectionCause
}

// Records that a request asks the server to revoke the session of the
// credential it carries, before the request is sent: the server may enforce
// it even when the answer is lost. The record names only that session, so it
// can only withhold a cause from that session's rejections, and it is kept.
func (self *Api) noteSelfRevocation(sentByJwt string, sessionId *Id) {
	if sessionId == nil {
		return
	}
	sentSessionId := credentialSessionId(sentByJwt)
	if sentSessionId == nil || *sentSessionId != sessionId.toConnectId() {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.selfRevokedSessionId = sentSessionId
}

// Withholds the session_revoked cause from the rejection of a session this API
// revoked itself. Callers hold mutex.
func (self *Api) trustedRejectionCauseWithLock(rejectedByJwt string, cause string) string {
	if cause != AuthLogoutCauseSessionRevoked {
		return ""
	}
	if self.selfRevokedWithLock(rejectedByJwt) {
		return ""
	}
	return cause
}

// Whether rejectedByJwt carries the session this API asked the server to
// revoke itself (noteSelfRevocation). Its rejection is that sign-out,
// whichever answer comes first: a rejection of another request can land after
// the server enforced the revoke and before the revoke's own answer. Callers
// hold mutex.
func (self *Api) selfRevokedWithLock(rejectedByJwt string) bool {
	if self.selfRevokedSessionId == nil {
		return false
	}
	sessionId := credentialSessionId(rejectedByJwt)
	return sessionId != nil && *sessionId == *self.selfRevokedSessionId
}

// The cause a confirmed rejection of the sent credential carries, for a caller
// that reports the error without applying the rejection (the session
// controller's error).
func (self *Api) trustedRejectionCause(sentByJwt string, err error) string {
	cause := confirmedRejectionCause(err)
	if cause == "" {
		return ""
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.trustedRejectionCauseWithLock(sentByJwt, cause)
}

// Records a confirmed rejection that ended this API's sign-in, or its network
// credential, with its trusted cause. Callers hold mutex.
func (self *Api) noteSignInRejectionWithLock(cause string) {
	self.signInRejected = true
	self.signInRejectionCause = cause
}

// Forgets why an earlier sign-in ended once a new credential is installed.
// Clearing the credential is not a new login: the cause stays readable after
// an app's own cleanup. Callers hold mutex.
func (self *Api) newLoginWithLock(byJwt string) {
	// Explicit setters also retire queued events when they clear a credential.
	self.authLogoutGeneration = 0
	// a sign-in or a sign-out ends the account sign-in notice (a device's
	// start takes up a kept one again, adoptAccountSignInNoticeWithLock)
	self.accountSignInNoticePending = false
	self.accountSignInNoticeStore = nil
	if byJwt == "" {
		return
	}
	self.authLogoutCause = ""
	self.signInRejected = false
	self.signInRejectionCause = ""
}

// Whether the server rejected the sign-in this API held, its own or that of
// its network credential, and no new login has replaced it, with the
// rejection's trusted cause. The session controller shows sign-in required,
// not a retryable error, for an API in this state.
func (self *Api) sessionSignInRejection() (bool, string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.signInRejected, self.signInRejectionCause
}
