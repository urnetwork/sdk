package sdk

import (
	"testing"
)

// A cancel of the account picker (Android) or the sign-in sheet (Apple) shows
// nothing. The apps showed their sign-in error for it.
func TestGoogleSignInFailureCancelShowsNothing(t *testing.T) {
	cases := []struct {
		source string
		domain string
		code   int
	}{
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 12501},
		{source: GoogleSignInFailureSourceApple, domain: "com.google.GIDSignIn", code: -5},
		{source: GoogleSignInFailureSourceAndroidCredential, domain: "android.credentials.GetCredentialException.TYPE_USER_CANCELED", code: 0},
	}
	for _, c := range cases {
		if got := GoogleSignInFailureMessage(c.source, c.domain, c.code, "Error signing in with Google"); got != "" {
			t.Errorf("GoogleSignInFailureMessage(%q, %q, %d) = %q, want nothing shown", c.source, c.domain, c.code, got)
		}
	}
}

// Every other failure keeps the app's message and names its code, so a
// screenshot tells the causes apart. The apps showed the message alone.
func TestGoogleSignInFailureMessageNamesTheCode(t *testing.T) {
	cases := []struct {
		source  string
		domain  string
		code    int
		message string
		want    string
	}{
		// an OAuth client or signing certificate the Google project does not know
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 10, message: "Error signing in with Google", want: "Error signing in with Google (10)"},
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 7, message: "Error signing in with Google", want: "Error signing in with Google (7)"},
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 12500, message: "Fehler bei der Anmeldung mit Google", want: "Fehler bei der Anmeldung mit Google (12500)"},
		// Google Play services' generic CANCELED (16) is not the user closing the picker
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 16, message: "Error signing in with Google", want: "Error signing in with Google (16)"},
		// a code this build does not name
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 99999, message: "Error signing in with Google", want: "Error signing in with Google (99999)"},
		// Android has no domain, so one passed by mistake changes nothing
		{source: GoogleSignInFailureSourceAndroid, domain: "com.example.Other", code: 12501, message: "Error signing in with Google", want: ""},
		{source: GoogleSignInFailureSourceApple, domain: "com.google.GIDSignIn", code: -4, message: "There was an error logging in", want: "There was an error logging in (-4)"},
		// another domain's code is named after its domain, and its -5 is no cancel
		{source: GoogleSignInFailureSourceApple, domain: "NSURLErrorDomain", code: -1009, message: "There was an error logging in", want: "There was an error logging in (NSURLErrorDomain -1009)"},
		{source: GoogleSignInFailureSourceApple, domain: "NSURLErrorDomain", code: -5, message: "There was an error logging in", want: "There was an error logging in (NSURLErrorDomain -5)"},
		// an unknown source reads no cancel
		{source: "other", domain: "", code: 12501, message: "Error signing in with Google", want: "Error signing in with Google (12501)"},
		// an app that passes no message gets the English one
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 10, message: "", want: "Error signing in with Google (10)"},
	}
	for _, c := range cases {
		if got := GoogleSignInFailureMessage(c.source, c.domain, c.code, c.message); got != c.want {
			t.Errorf("GoogleSignInFailureMessage(%q, %q, %d, %q) = %q, want %q", c.source, c.domain, c.code, c.message, got, c.want)
		}
	}
}

// The log line names the source, the code and, when known, its name, so an
// uploaded log tells the causes apart; a cancel is logged as cancelled.
func TestGoogleSignInFailureLogLine(t *testing.T) {
	cases := []struct {
		source string
		domain string
		code   int
		want   string
	}{
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 10, want: "google sign-in failed: android 10 developer_error"},
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 12500, want: "google sign-in failed: android 12500 sign_in_failed"},
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 12501, want: "google sign-in cancelled: android 12501 sign_in_cancelled"},
		{source: GoogleSignInFailureSourceAndroid, domain: "", code: 99999, want: "google sign-in failed: android 99999"},
		{source: GoogleSignInFailureSourceApple, domain: "com.google.GIDSignIn", code: -5, want: "google sign-in cancelled: apple -5 canceled"},
		{source: GoogleSignInFailureSourceApple, domain: "com.google.GIDSignIn", code: -4, want: "google sign-in failed: apple -4 has_no_auth_in_keychain"},
		{source: GoogleSignInFailureSourceApple, domain: "NSURLErrorDomain", code: -1009, want: "google sign-in failed: apple NSURLErrorDomain -1009"},
		{source: "", domain: "", code: 10, want: "google sign-in failed: unknown 10"},
	}
	for _, c := range cases {
		if got := googleSignInFailureLogLine(c.source, c.domain, c.code); got != c.want {
			t.Errorf("googleSignInFailureLogLine(%q, %q, %d) = %q, want %q", c.source, c.domain, c.code, got, c.want)
		}
	}
	// the line goes through LogAppInfo's tag and bounds
	if got, want := appLogLine("login", googleSignInFailureLogLine(GoogleSignInFailureSourceAndroid, "", 10)), "[app][login] google sign-in failed: android 10 developer_error"; got != want {
		t.Errorf("app log line = %q, want %q", got, want)
	}
}

// Credential Manager (android#271) fails with a type string and no code: the
// type's last part is the code the message and the log name, and the user
// closing the account picker is a cancel. Before, the apps had only Google
// Play services' status codes to name, and a type read as no cancel.
func TestGoogleSignInFailureCredentialManagerType(t *testing.T) {
	const message = "Error signing in with Google"
	cases := []struct {
		domain string
		code   int
		want   string
		log    string
	}{
		{
			domain: "android.credentials.GetCredentialException.TYPE_USER_CANCELED",
			want:   "",
			log:    "google sign-in cancelled: android_credential user_canceled",
		},
		{
			domain: "android.credentials.GetCredentialException.TYPE_NO_CREDENTIAL",
			want:   "Error signing in with Google (no_credential)",
			log:    "google sign-in failed: android_credential no_credential",
		},
		{
			// a device with no credential provider (no Google Play services)
			domain: "androidx.credentials.TYPE_GET_CREDENTIAL_PROVIDER_CONFIGURATION_EXCEPTION",
			want:   "Error signing in with Google (get_credential_provider_configuration_exception)",
			log:    "google sign-in failed: android_credential get_credential_provider_configuration_exception",
		},
		{
			domain: "android.credentials.GetCredentialException.TYPE_INTERRUPTED",
			want:   "Error signing in with Google (interrupted)",
			log:    "google sign-in failed: android_credential interrupted",
		},
		{
			// a DOM error names its own type after a "/"
			domain: "androidx.credentials.TYPE_GET_PUBLIC_KEY_CREDENTIAL_DOM_EXCEPTION/androidx.credentials.TYPE_NOT_ALLOWED_ERROR",
			want:   "Error signing in with Google (not_allowed_error)",
			log:    "google sign-in failed: android_credential not_allowed_error",
		},
		{
			// a credential that is not a Google ID token
			domain: "android.credentials.TYPE_PASSWORD_CREDENTIAL",
			want:   "Error signing in with Google (password_credential)",
			log:    "google sign-in failed: android_credential password_credential",
		},
		{
			domain: GoogleSignInCredentialParsingType,
			want:   "Error signing in with Google (google_id_token_parsing)",
			log:    "google sign-in failed: android_credential google_id_token_parsing",
		},
		{
			// a code passed by mistake changes nothing: Google Play services'
			// 12501 is no cancel here
			domain: "android.credentials.GetCredentialException.TYPE_UNKNOWN",
			code:   12501,
			want:   "Error signing in with Google (unknown)",
			log:    "google sign-in failed: android_credential unknown",
		},
		{
			domain: "",
			want:   "Error signing in with Google (unknown)",
			log:    "google sign-in failed: android_credential unknown",
		},
	}
	for _, c := range cases {
		if got := GoogleSignInFailureMessage(GoogleSignInFailureSourceAndroidCredential, c.domain, c.code, message); got != c.want {
			t.Errorf("GoogleSignInFailureMessage(%q, %d) = %q, want %q", c.domain, c.code, got, c.want)
		}
		if got := googleSignInFailureLogLine(GoogleSignInFailureSourceAndroidCredential, c.domain, c.code); got != c.log {
			t.Errorf("googleSignInFailureLogLine(%q, %d) = %q, want %q", c.domain, c.code, got, c.log)
		}
	}
}
