package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Support inbox 7956, 7973: each app kept its own list of the failure texts
// that mean URnetwork could not be reached, and the four lists drifted. These
// are the can't-reach cases of all four apps' tests (android
// ConnectionRegionTest, apple CantReachUrnetworkTests, windows
// connection-region-tests, linux ConnectionRegionTest), which the one rule now
// answers for every app.
func TestIsCantReachErrorAnswersEveryAppsCases(t *testing.T) {
	for _, message := range []string{
		// what a sign-in from a network that blocks URnetwork's names fails with
		"http request attempts exhausted",
		"http request failed: Post \"https://api.bringyour.com/auth/login\": dial tcp: lookup api.bringyour.com: no such host",
		"http request failed: unexpected status",
		"Post \"https://api.bringyour.com/auth/login\": dial tcp: lookup api.bringyour.com: no such host",
		"dial tcp: lookup api.bringyour.com: no such host",
		"Post \"https://api.bringyour.com/auth/login\": dial tcp 1.2.3.4:443: i/o timeout",
		"Post \"https://api.bringyour.com/auth/login\": context deadline exceeded",
		"context deadline exceeded",
		"read tcp 10.0.0.2:5000->1.2.3.4:443: i/o timeout",
		"read tcp 10.0.0.2:5555->1.2.3.4:443: read: connection reset by peer",
		"read tcp: connection reset by peer",
		"dial tcp 1.2.3.4:443: i/o timeout",
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"dial tcp 1.2.3.4:443: connect: network is unreachable",
		"lookup api.bringyour.com: server misbehaving",
		"server misbehaving",
		"connect: host is unreachable",
		"net/http: TLS handshake timeout",
		"net/http: timeout awaiting response headers",
		"Post \"https://api.bringyour.com/auth/login\": EOF",
		"Post \"https://api.bringyour.com/auth/login\": unexpected EOF",
		"tls: failed to verify certificate: x509: certificate signed by unknown authority",
		"Could not resolve api.bringyour.com",
	} {
		if !IsCantReachError(message) {
			t.Errorf("not can't-reach: %q", message)
		}
	}
}

// The texts only some of the apps knew while each kept its own list. Every
// one of these raised the prompt on one platform and not on another.
func TestIsCantReachErrorKnowsTheTextsTheAppsDisagreedOn(t *testing.T) {
	for _, c := range []struct {
		message string
		// the apps whose lists lacked it
		lackedBy string
	}{
		{"Get \"https://api.bringyour.com/network/provider-locations\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)", "android, apple"},
		{"connect: host is down", "android, apple, windows"},
		{"Could not resolve host: api.bringyour.com", "android, apple, windows"},
		{"lookup api.bringyour.com on 10.0.0.1:53: no answer from DNS server", "android, windows, linux"},
		{"remote error: tls: handshake failure", "windows, linux"},
		{"x509: certificate is valid for www.example.com, not api.bringyour.com", "windows, linux"},
		{"net/http: timeout awaiting response headers", "windows"},
		{"Post \"https://api.bringyour.com/auth/login\": EOF", "windows, linux"},
		{"Post \"https://api.bringyour.com/auth/login\": unexpected EOF", "linux"},
		{"connect: host is unreachable", "linux"},
		{"http request failed: TypeError: Failed to fetch", "linux"},
		{"Unable to resolve host \"api.bringyour.com\": No address associated with hostname", "apple, windows, linux"},
		{"failed to connect to api.bringyour.com/1.2.3.4 (port 443) from /10.0.0.2 (port 50000) after 10000ms", "apple, windows, linux"},
	} {
		if !IsCantReachError(c.message) {
			t.Errorf("not can't-reach (lacked by %s): %q", c.lackedBy, c.message)
		}
	}
}

// An answer from the platform is never can't-reach, whatever its text holds:
// an http status (connect HttpStatusError, "<code> <status>: <body>"), an
// auth or validation message, an app's own refusal. The negative cases of all
// four apps' tests.
func TestIsCantReachErrorAnAnswerFromThePlatformIsNot(t *testing.T) {
	for _, message := range []string{
		"",
		"   ",
		"401 Unauthorized: invalid jwt",
		"401 Unauthorized: ",
		"401 Unauthorized: {\"message\":\"timeout awaiting response headers\"}",
		"403 Forbidden: ",
		"400 Bad Request: user auth is required",
		"400 Bad Request: invalid user auth",
		"400 Bad Request: bad user auth",
		"503 Service Unavailable: upstream i/o timeout",
		"503 Service Unavailable: dial tcp upstream refused",
		// a status answer about attempts is still the platform answering
		"429 Too Many Requests: http request attempts exhausted",
		"  429 Too Many Requests: http request attempts exhausted",
		"Invalid user auth",
		"Invalid password.",
		"Invalid user or password.",
		"verification_required",
		"verification required",
		"not_configured",
		"signed out while this sign-in was pending",
		"superseded by signing out",
		"no result",
		"No result found",
	} {
		if IsCantReachError(message) {
			t.Errorf("can't-reach, but URnetwork answered: %q", message)
		}
	}
}

// Case and the whitespace around a failure's text don't change the answer.
func TestIsCantReachErrorIgnoresCaseAndSurroundingSpace(t *testing.T) {
	for _, message := range []string{
		"HTTP REQUEST ATTEMPTS EXHAUSTED",
		"  http request attempts exhausted\n",
		"Dial TCP 1.2.3.4:443: I/O Timeout",
	} {
		if !IsCantReachError(message) {
			t.Errorf("not can't-reach: %q", message)
		}
	}
}

// Signed in, the prompt rises once as the window's diagnosis turns to the
// platform being unreachable (support inbox 7956, 7973): Windows did this
// (ConnectPage.cpp ShouldPromptOnStall) and android, apple and linux never
// did, so a signed-in connect from a network that blocks the platform showed
// "connecting" with no way to the connection region.
func TestCantReachOnStallRaisesOncePerTurn(t *testing.T) {
	unreachable := connect.WindowStallPlatformUnreachable
	if unreachable != "platform-unreachable" {
		t.Fatalf("connect's platform-unreachable diagnosis is %q, the apps read %q", unreachable, "platform-unreachable")
	}
	others := []string{
		"",
		connect.WindowStallEvaluating,
		connect.WindowStallProvidersUnresponsive,
		connect.WindowStallRateLimited,
		connect.WindowStallAuthFailing,
	}
	for _, previous := range others {
		if !CantReachOnStall(previous, unreachable) {
			t.Errorf("no prompt as the diagnosis turned from %q to %q", previous, unreachable)
		}
	}
	// not again on every report while it stays unreachable
	if CantReachOnStall(unreachable, unreachable) {
		t.Errorf("prompt repeated while the diagnosis stayed %q", unreachable)
	}
	// never for another diagnosis, nor as it leaves unreachable
	for _, previous := range append(others, unreachable) {
		for _, stallReason := range others {
			if CantReachOnStall(previous, stallReason) {
				t.Errorf("prompt as the diagnosis turned from %q to %q", previous, stallReason)
			}
		}
	}
}

// A window that left the platform-unreachable diagnosis and came back to it
// raises the prompt again: each turn is new news.
func TestCantReachOnStallRaisesAgainAfterTheWindowRecovered(t *testing.T) {
	reports := []string{
		connect.WindowStallEvaluating,
		connect.WindowStallPlatformUnreachable,
		connect.WindowStallPlatformUnreachable,
		connect.WindowStallProvidersUnresponsive,
		connect.WindowStallPlatformUnreachable,
		"",
		connect.WindowStallPlatformUnreachable,
	}
	raised := 0
	previous := ""
	for _, stallReason := range reports {
		if CantReachOnStall(previous, stallReason) {
			raised += 1
		}
		previous = stallReason
	}
	if raised != 3 {
		t.Errorf("raised %d times over %v, want 3", raised, reports)
	}
}
