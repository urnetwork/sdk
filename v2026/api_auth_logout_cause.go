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

	"github.com/urnetwork/connect/v2026"
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
// "". It is set before the AuthLogout listeners run, so they can read it, and
// a new login clears it.
func (self *Api) GetAuthLogoutCause() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.authLogoutCause
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
	if self.selfRevokedSessionId != nil {
		if sessionId := credentialSessionId(rejectedByJwt); sessionId != nil && *sessionId == *self.selfRevokedSessionId {
			return ""
		}
	}
	return cause
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
