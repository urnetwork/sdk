// Why a Google sign-in failed before the app had an ID token, read the same
// way on every app.
//
// Android signed in through Google Play services, whose
// GoogleSignIn.getSignedInAccountFromIntent fails with an ApiException and its
// status code (CommonStatusCodes, GoogleSignInStatusCodes). It signs in through
// Credential Manager now (androidx.credentials with the Google ID library,
// android#271), whose getCredential fails with a GetCredentialException that
// carries a type string and no code. iOS and macOS sign
// in through the GoogleSignIn SDK, which fails with an NSError, in the SDK's
// com.google.GIDSignIn domain or in another one (a network error). The apps
// dropped the code and showed one message for every failure, a cancel
// included, so neither a screenshot nor an uploaded log could tell the causes
// apart. On Android, 10 (developer_error) is an OAuth client or signing
// certificate the Google project does not know, 7 a network error, and 12500
// Google Play services refusing the sign-in.
//
// A failure is read in its source's scheme (GoogleSignInFailureSource*). The
// user closing the account picker or the sign-in sheet is a cancel, which
// shows nothing. Every other failure names its code in the message
// (GoogleSignInFailureMessage) and in the log the feedback upload carries
// (LogGoogleSignInFailure). Neither says anything about the account.
package sdk

import (
	"strconv"
	"strings"
)

// Where a failure's code comes from: the source argument of
// GoogleSignInFailureMessage and LogGoogleSignInFailure.
const (
	// Google Play services on Android: the ApiException's status code, with no
	// domain
	GoogleSignInFailureSourceAndroid = "android"
	// the GoogleSignIn SDK on iOS and macOS: the NSError's domain and code
	GoogleSignInFailureSourceApple = "apple"
	// Credential Manager on Android: the GetCredentialException's type as the
	// domain, with no code (0); or the type of a credential that is not a
	// Google ID token, or the Google ID library's parsing error, the same way
	GoogleSignInFailureSourceAndroidCredential = "android_credential"
)

// The domain an Android app passes with GoogleSignInFailureSourceAndroidCredential
// when the Google ID library could not read the credential it got
// (GoogleIdTokenParsingException).
const GoogleSignInCredentialParsingType = "TYPE_GOOGLE_ID_TOKEN_PARSING"

// The codes this reads.
const (
	// GoogleSignInStatusCodes.SIGN_IN_CANCELLED
	googleSignInAndroidCancelledCode = 12501
	// kGIDSignInErrorDomain
	googleSignInAppleDomain = "com.google.GIDSignIn"
	// kGIDSignInErrorCodeCanceled
	googleSignInAppleCancelledCode = -5
	// GetCredentialCancellationException's type: the user closed the account
	// picker
	googleSignInAndroidCredentialCancelledType = "android.credentials.GetCredentialException.TYPE_USER_CANCELED"
	// the message when the app passes none
	googleSignInDefaultMessage = "Error signing in with Google"
)

// The names of the Google Play services codes a sign-in can fail with
// (CommonStatusCodes and GoogleSignInStatusCodes), for the log. A code missing
// here is logged by number alone.
var googleSignInAndroidCodeNames = map[int]string{
	2:     "service_version_update_required",
	3:     "service_disabled",
	4:     "sign_in_required",
	5:     "invalid_account",
	7:     "network_error",
	8:     "internal_error",
	10:    "developer_error",
	13:    "error",
	14:    "interrupted",
	15:    "timeout",
	16:    "canceled",
	17:    "api_not_connected",
	12500: "sign_in_failed",
	12501: "sign_in_cancelled",
	12502: "sign_in_currently_in_progress",
}

// The names of the GoogleSignIn SDK's codes (GIDSignInErrorCode), for the log.
var googleSignInAppleCodeNames = map[int]string{
	-1: "unknown",
	-2: "keychain",
	-4: "has_no_auth_in_keychain",
	-5: "canceled",
	-6: "emm",
	-8: "scopes_already_granted",
	-9: "mismatch_with_current_user",
}

// A failed Google sign-in as an app shows and logs it.
type googleSignInFailure struct {
	// the user closed the account picker or the sign-in sheet
	cancelled bool
	// the code as the message names it: the status code on Android ("10"),
	// and on Apple the code, after its domain unless it is the GoogleSignIn
	// SDK's ("-4", "NSURLErrorDomain -1009")
	code string
	// the code's name, "" when this build does not know it
	name string
}

// The failure for `code` read in the scheme of `source`. An unknown source
// reads no cancel and names the code by number, after its domain if any.
func googleSignInFailureFor(source string, domain string, code int) googleSignInFailure {
	codeText := strconv.Itoa(code)
	switch source {
	case GoogleSignInFailureSourceAndroid:
		return googleSignInFailure{
			cancelled: code == googleSignInAndroidCancelledCode,
			code:      codeText,
			name:      googleSignInAndroidCodeNames[code],
		}
	case GoogleSignInFailureSourceAndroidCredential:
		// the type names the cause, so it is the code; there is no number
		return googleSignInFailure{
			cancelled: domain == googleSignInAndroidCredentialCancelledType,
			code:      googleSignInCredentialTypeCode(domain),
		}
	case GoogleSignInFailureSourceApple:
		if domain == googleSignInAppleDomain {
			return googleSignInFailure{
				cancelled: code == googleSignInAppleCancelledCode,
				code:      codeText,
				name:      googleSignInAppleCodeNames[code],
			}
		}
	}
	// another domain's code, a network error on Apple for one, is never a
	// cancel of the sign-in sheet
	if domain != "" {
		codeText = domain + " " + codeText
	}
	return googleSignInFailure{
		code: codeText,
	}
}

// A Credential Manager type as a failure's code: its last part, after any "/"
// and ".", without the TYPE_ prefix, in lower case, as in "no_credential" for
// android.credentials.GetCredentialException.TYPE_NO_CREDENTIAL; "unknown"
// for none.
func googleSignInCredentialTypeCode(credentialType string) string {
	code := credentialType
	if i := strings.LastIndex(code, "/"); 0 <= i {
		code = code[i+1:]
	}
	if i := strings.LastIndex(code, "."); 0 <= i {
		code = code[i+1:]
	}
	code = strings.ToLower(strings.TrimPrefix(code, "TYPE_"))
	if code == "" {
		return "unknown"
	}
	return code
}

// The message an app shows for a failed Google sign-in: "" for a cancel, which
// shows nothing; otherwise `message`, the app's own localized text for a
// failed Google sign-in, followed by the failure's code in parentheses, as in
// "Error signing in with Google (10)". `domain` is the NSError's on Apple, the
// exception's type with Credential Manager on Android, and "" for Google Play
// services.
// Exported to gomobile and the c abi.
func GoogleSignInFailureMessage(source string, domain string, code int, message string) string {
	failure := googleSignInFailureFor(source, domain, code)
	if failure.cancelled {
		return ""
	}
	if message == "" {
		message = googleSignInDefaultMessage
	}
	return message + " (" + failure.code + ")"
}

// Writes a failed Google sign-in to the sdk log the feedback upload carries,
// as one "[app][login] google sign-in ..." line (LogAppInfo) with the source,
// the code and its name. A cancel is logged too, as cancelled.
// Exported to gomobile and the c abi.
func LogGoogleSignInFailure(source string, domain string, code int) {
	LogAppInfo("login", googleSignInFailureLogLine(source, domain, code))
}

// The message of LogGoogleSignInFailure's line.
func googleSignInFailureLogLine(source string, domain string, code int) string {
	failure := googleSignInFailureFor(source, domain, code)
	outcome := "failed"
	if failure.cancelled {
		outcome = "cancelled"
	}
	if source == "" {
		source = "unknown"
	}
	line := "google sign-in " + outcome + ": " + source + " " + failure.code
	if failure.name != "" {
		line += " " + failure.name
	}
	return line
}
