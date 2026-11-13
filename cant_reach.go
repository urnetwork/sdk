package sdk

// Can't reach URnetwork: the one rule every app uses to decide that URnetwork
// can't be reached, so it can offer the connection region picker ("Can't
// reach URnetwork — choose where you're connecting from"). Two signals raise
// the prompt:
//
//   - an api call failed at the network level: IsCantReachError on the
//     failure's text (sign-in, and the calls a signed-in app makes);
//   - a signed-in connect's window turned to the platform-unreachable
//     diagnosis: CantReachOnStall on the window's stall reason
//     (WindowStatus.StallReason).
//
// Each app used to keep its own list of failure texts, and the four lists
// drifted, so the same failure raised the prompt on one platform and not on
// another; and only Windows raised it for a signed-in stall (support inbox
// 7956, 7973: mainland China, where the platform's own names are blocked).
// The apps now ask the sdk, and keep only what is truly theirs: their own
// transport exceptions (an Android UnknownHostException, an Apple
// NSURLErrorDomain code) and the screens.
//
// Neither changes the connection region: the prompt only opens the picker.

import (
	"strings"

	"github.com/urnetwork/connect"
)

// The texts of a failure that never got an answer from URnetwork, lower case
// (the failure is matched lower cased). An answer from the platform (an http
// status, an auth or validation message) is never one of these.
var cantReachMarkers = []string{
	// an api request that no route answered (connect HttpRequestExhaustedError,
	// net_http_exhaustion.go): what a sign-in from a network that blocks
	// URnetwork's names fails with, and its text names none of its causes
	"http request attempts exhausted",
	// a js/wasm fetch that failed (connect net_http.go)
	"http request failed",
	// name resolution: Go's net.DNSError ("lookup <name>: ...") and resolver
	// answers, and the platforms' own resolver texts
	"lookup ",
	"no such host",
	"server misbehaving",
	"could not resolve",
	"unable to resolve host",
	// the dial and the connection
	"dial tcp",
	"dial udp",
	"connection refused",
	"connection reset",
	"network is unreachable",
	"host is unreachable",
	"no route to host",
	"host is down",
	"failed to connect to",
	// timeouts
	"i/o timeout",
	"context deadline exceeded",
	"client.timeout exceeded",
	"timeout awaiting response headers",
	// tls: a handshake that failed or timed out, or a certificate that isn't
	// URnetwork's (a substitute an interception presented)
	"tls handshake",
	"tls: ",
	"x509: ",
	// a connection cut before the answer ended
	"unexpected eof",
	": eof",
}

// IsCantReachError is whether an api failure's text means that the app could
// not reach URnetwork at all (name resolution, dial, tls, a timeout, or no
// route answering), as against URnetwork answering with a refusal. An http
// status answer ("<code> <status>: <body>", connect HttpStatusError) is
// URnetwork answering, whatever its body says; a validation or auth message
// is too. An empty text is no failure.
func IsCantReachError(errorMessage string) bool {
	message := strings.ToLower(strings.TrimSpace(errorMessage))
	if message == "" || isHttpStatusAnswer(message) {
		return false
	}
	for _, marker := range cantReachMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// Whether a failure's text is an http status answer: three digits and a space
// at the start, as connect.HttpStatusError renders its status line.
func isHttpStatusAnswer(message string) bool {
	if len(message) < 4 || message[3] != ' ' {
		return false
	}
	for i := 0; i < 3; i += 1 {
		if message[i] < '0' || '9' < message[i] {
			return false
		}
	}
	return true
}

// CantReachOnStall is whether a signed-in connect has just found the platform
// unreachable: the window's stall reason (WindowStatus.StallReason, the
// connect WindowStall* diagnosis while the window is forming) turned to
// platform-unreachable from anything else. An app passes the reason it saw
// last and the one it sees now on each window report, and raises the prompt
// when this is true: once each time the diagnosis turns, not on every report.
// It is a signed-in signal: a window forms only for a device signed in with a
// connect asked for.
func CantReachOnStall(previousStallReason string, stallReason string) bool {
	return stallReason == connect.WindowStallPlatformUnreachable &&
		previousStallReason != connect.WindowStallPlatformUnreachable
}
