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
//
// Both signals wait for evidence that lasts (owner, 2026-11-13: the prompt came
// up again and again on a working iOS tunnel). A signed-in window publishes
// platform-unreachable only once its diagnosis has held for
// connect.MultiClientSettings.WindowPlatformUnreachableHold, so the
// transport reconnect after a connect server restart never turns it. An api
// failure raises the prompt only when can't-reach failures have kept coming
// for cantReachSettings.ApiFailureHold with no answer from URnetwork between
// them (cantReachRule), unless its text alone settles it: a name URnetwork's
// resolver says does not exist, or a certificate that is not URnetwork's.
// One timeout, or a burst of calls cut by the same blip, raises nothing.
//
// The api rule's state is the process's, as the prompt is: every app asks
// through these package functions, and an app process shows one prompt. The
// sdk's api request seams report each answer (noteCantReachApiResult).

import (
	"strings"
	"sync"
	"time"

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

// The texts that settle it at once: URnetwork's own names do not exist for the
// resolver the app asked, or a certificate that is not URnetwork's was
// presented (an interception). Neither is a blip that a retry outlasts.
var cantReachImmediateMarkers = []string{
	"no such host",
	"x509: ",
}

// IsCantReachError is whether an api failure should raise the prompt: its
// text means that the app could not reach URnetwork at all (name resolution,
// dial, tls, a timeout, or no route answering), as against URnetwork
// answering with a refusal, and the failure is not a blip (cantReachRule). An
// http status answer ("<code> <status>: <body>", connect HttpStatusError) is
// URnetwork answering, whatever its body says; a validation or auth message
// is too. An empty text is no failure.
//
// Apps call it once per failed call, and a failure it does not raise the
// prompt for still counts toward the next one.
func IsCantReachError(errorMessage string) bool {
	return processCantReach.apiFailure(errorMessage, time.Now())
}

// The text half of IsCantReachError: whether the failure never got an answer
// from URnetwork, and whether its text alone settles it.
func classifyCantReachText(errorMessage string) (cantReach bool, immediate bool) {
	message := strings.ToLower(strings.TrimSpace(errorMessage))
	if message == "" || isHttpStatusAnswer(message) {
		return false, false
	}
	for _, marker := range cantReachMarkers {
		if strings.Contains(message, marker) {
			cantReach = true
			break
		}
	}
	if !cantReach {
		return false, false
	}
	for _, marker := range cantReachImmediateMarkers {
		if strings.Contains(message, marker) {
			return true, true
		}
	}
	return true, false
}

// Tunes when api failures raise the prompt.
type cantReachSettings struct {
	// How long can't-reach api failures must keep coming, the first and the
	// latest of them at least this far apart with no answer from URnetwork
	// between them, before one raises the prompt. 20s is past the calls one
	// blip cuts at once (a tunnel restart, a server restart, a network change
	// all fail every call in flight within a second or two) and within the
	// prompt's use, a sign-in from a network that blocks URnetwork, where a
	// second try fails as the first did. Non-positive raises the prompt on
	// the first failure.
	ApiFailureHold time.Duration
	// How long a failure counts toward the hold: older ones are forgotten, so
	// two blips minutes apart are not one outage. 60s holds about three of
	// the hold's spans and the app's regular refreshes. Non-positive forgets
	// every failure but the latest, so only a text that settles it raises the
	// prompt while the hold is positive.
	ApiFailureWindow time.Duration
}

func defaultCantReachSettings() *cantReachSettings {
	return &cantReachSettings{
		ApiFailureHold:   20 * time.Second,
		ApiFailureWindow: 60 * time.Second,
	}
}

// Keeps the recent can't-reach api failures that no answer has ended. Safe
// for concurrent use.
type cantReachRule struct {
	settings *cantReachSettings

	stateLock sync.Mutex
	// the times of the failures since the last answer, oldest first, within
	// ApiFailureWindow of the latest
	failureTimes []time.Time
}

func newCantReachRule(settings *cantReachSettings) *cantReachRule {
	return &cantReachRule{
		settings: settings,
	}
}

// the app process's rule, which IsCantReachError and the api request seams
// share
var processCantReach = newCantReachRule(defaultCantReachSettings())

// Records a failed api call and returns whether it raises the prompt.
func (self *cantReachRule) apiFailure(errorMessage string, now time.Time) bool {
	cantReach, immediate := classifyCantReachText(errorMessage)
	if !cantReach {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	windowStart := now.Add(-self.settings.ApiFailureWindow)
	i := 0
	for ; i < len(self.failureTimes) && self.failureTimes[i].Before(windowStart); i += 1 {
	}
	failureTimes := append(self.failureTimes[i:], now)
	if self.settings.ApiFailureWindow <= 0 {
		failureTimes = failureTimes[len(failureTimes)-1:]
	}
	self.failureTimes = failureTimes
	if immediate || self.settings.ApiFailureHold <= 0 {
		return true
	}
	return self.settings.ApiFailureHold <= now.Sub(failureTimes[0])
}

// Ends the failures so far: URnetwork answered a call.
func (self *cantReachRule) apiAnswer() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failureTimes = nil
}

// Reports one api request's result to the process's rule: a result or an
// http status answer means URnetwork answered. A failure is left for the
// app's IsCantReachError, which sees it with its text.
func noteCantReachApiResult(requestErr error) {
	if requestErr == nil || isHttpStatusAnswer(strings.ToLower(strings.TrimSpace(requestErr.Error()))) {
		processCantReach.apiAnswer()
	}
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
