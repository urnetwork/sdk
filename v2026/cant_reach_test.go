package sdk

import (
	"context"
	"errors"
	"testing"
	"time"

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
		if cantReach, _ := classifyCantReachText(message); !cantReach {
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
		if cantReach, _ := classifyCantReachText(c.message); !cantReach {
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
		if cantReach, _ := classifyCantReachText(message); cantReach {
			t.Errorf("can't-reach, but URnetwork answered: %q", message)
		}
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
		if cantReach, _ := classifyCantReachText(message); !cantReach {
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

// One blip is not an outage (owner, 2026-11-13): on a working iOS tunnel the
// prompt came up again and again, each time for api calls that a stalled
// tunnel or a server restart had cut at once. Before the hold the first
// can't-reach failure raised it.
func TestIsCantReachErrorOneBlipIsNotAnOutage(t *testing.T) {
	processCantReach.apiAnswer()
	defer processCantReach.apiAnswer()
	for _, message := range []string{
		"Post \"https://api.example/network/clients\": dial tcp 192.0.2.1:443: i/o timeout",
		"http request attempts exhausted",
		"read tcp 198.51.100.2:5000->192.0.2.1:443: read: connection reset by peer",
	} {
		if IsCantReachError(message) {
			t.Errorf("one blip raised the prompt: %q", message)
		}
	}
}

// the ambiguous failure the rule tests repeat
const cantReachTestTimeout = "dial tcp 192.0.2.1:443: i/o timeout"

// Failures that keep coming for the hold raise the prompt, and it stays raised
// while they do: an app may ask twice for one failure (apple
// reportUrnetworkReachability, then the prompt's report).
func TestCantReachRuleRaisesWhenFailuresLast(t *testing.T) {
	rule := newCantReachRule(defaultCantReachSettings())
	t0 := time.Unix(1_000_000, 0)
	// a burst: every call in flight fails within a second
	for i := range 5 {
		if rule.apiFailure(cantReachTestTimeout, t0.Add(time.Duration(i)*100*time.Millisecond)) {
			t.Fatalf("burst failure %d raised the prompt", i)
		}
	}
	if rule.apiFailure(cantReachTestTimeout, t0.Add(19*time.Second)) {
		t.Fatal("raised before the hold")
	}
	if !rule.apiFailure(cantReachTestTimeout, t0.Add(20*time.Second)) {
		t.Fatal("not raised once failures had lasted the hold")
	}
	if !rule.apiFailure(cantReachTestTimeout, t0.Add(20*time.Second)) {
		t.Fatal("the same failure asked again did not raise")
	}
}

// An answer from URnetwork between failures ends the run: the failures after
// it start their own.
func TestCantReachRuleAnswerEndsTheRun(t *testing.T) {
	rule := newCantReachRule(defaultCantReachSettings())
	t0 := time.Unix(1_000_000, 0)
	rule.apiFailure(cantReachTestTimeout, t0)
	rule.apiAnswer()
	if rule.apiFailure(cantReachTestTimeout, t0.Add(25*time.Second)) {
		t.Fatal("a run an answer ended raised the prompt")
	}
	if !rule.apiFailure(cantReachTestTimeout, t0.Add(45*time.Second)) {
		t.Fatal("the new run did not raise once it lasted the hold")
	}
}

// Failures further apart than the window are separate blips.
func TestCantReachRuleForgetsFailuresPastTheWindow(t *testing.T) {
	rule := newCantReachRule(defaultCantReachSettings())
	t0 := time.Unix(1_000_000, 0)
	rule.apiFailure(cantReachTestTimeout, t0)
	if rule.apiFailure(cantReachTestTimeout, t0.Add(61*time.Second)) {
		t.Fatal("two failures a minute apart raised the prompt")
	}
	if !rule.apiFailure(cantReachTestTimeout, t0.Add(81*time.Second)) {
		t.Fatal("failures that lasted the hold inside the window did not raise")
	}
}

// A name that does not exist for the resolver, or a certificate that is not
// URnetwork's, settles it at once, as on a sign-in from a network that
// blocks or intercepts URnetwork.
func TestCantReachRuleTextsThatSettleItRaiseAtOnce(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for _, message := range []string{
		"Post \"https://api.example/auth/login\": dial tcp: lookup api.example: no such host",
		"tls: failed to verify certificate: x509: certificate signed by unknown authority",
		"x509: certificate is valid for www.example.com, not api.example",
	} {
		rule := newCantReachRule(defaultCantReachSettings())
		if !rule.apiFailure(message, t0) {
			t.Errorf("not raised at once: %q", message)
		}
	}
}

// An answer from the platform is not a failure for the rule either: it is
// not counted, so it cannot start a run.
func TestCantReachRuleAnswersAreNotCounted(t *testing.T) {
	rule := newCantReachRule(defaultCantReachSettings())
	t0 := time.Unix(1_000_000, 0)
	if rule.apiFailure("503 Service Unavailable: upstream i/o timeout", t0) {
		t.Fatal("a status answer raised the prompt")
	}
	if rule.apiFailure(cantReachTestTimeout, t0.Add(25*time.Second)) {
		t.Fatal("a status answer started a run")
	}
}

// A non-positive hold raises the prompt on the first failure, as before the
// hold.
func TestCantReachRuleHoldDisabled(t *testing.T) {
	settings := defaultCantReachSettings()
	settings.ApiFailureHold = 0
	rule := newCantReachRule(settings)
	if !rule.apiFailure(cantReachTestTimeout, time.Unix(1_000_000, 0)) {
		t.Fatal("a disabled hold did not raise at once")
	}
}

// The api request seams report answers to the process's rule: a request that
// got a result or an http status ends the run; one that got nothing does not.
func TestApiRequestSeamsReportAnswersToCantReach(t *testing.T) {
	processCantReach.apiAnswer()
	defer processCantReach.apiAnswer()
	api := newApi(t.Context(), nil, "https://cant-reach.example")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.CloseAndWait(ctx); err != nil {
			t.Errorf("close cant reach API: %v", err)
		}
	})
	var answerErr error
	api.setHttpGetRaw(func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("{}"), answerErr
	})

	for _, c := range []struct {
		answerErr error
		ends      bool
	}{
		{answerErr: nil, ends: true},
		{answerErr: errors.New("503 Service Unavailable: busy"), ends: true},
		{answerErr: errors.New(cantReachTestTimeout), ends: false},
	} {
		processCantReach.apiAnswer()
		now := time.Now()
		processCantReach.apiFailure(cantReachTestTimeout, now.Add(-30*time.Second))
		answerErr = c.answerErr
		api.getHttpGetRaw()(t.Context(), "https://elsewhere.example/status", "")
		raised := processCantReach.apiFailure(cantReachTestTimeout, now)
		if raised == c.ends {
			t.Errorf("request error %v: raised=%v after it, want %v", c.answerErr, raised, !c.ends)
		}
	}
}

// The apps' own transport failures, as each app passes them to
// IsCantReachError: an Apple URL loading error by domain and code, an Android
// exception by its text (Throwable.toString). The apps raised the prompt at
// once on any of these, on their own, because the sdk's rule could not read
// them: before this, the sustained failures below never raised it through the
// sdk.
var cantReachTestAppTransportFailures = []string{
	"NSURLErrorDomain -1001",
	"NSURLErrorDomain -1004",
	"NSURLErrorDomain -1005",
	"NSURLErrorDomain -1006",
	"NSURLErrorDomain -1009",
	"NSURLErrorDomain -1200",
	"NSURLErrorDomain -2000",
	"Error Domain=NSURLErrorDomain Code=-1001 \"The request timed out.\" UserInfo={NSErrorFailingURLStringKey=https://api.example/network/clients}",
	"java.net.SocketTimeoutException: timeout",
	"java.net.SocketTimeoutException: failed to connect to api.example/192.0.2.1 (port 443) from /198.51.100.2 (port 50000) after 10000ms",
	"java.net.UnknownHostException: Unable to resolve host \"api.example\": No address associated with hostname",
	"java.net.ConnectException: Failed to connect to api.example/192.0.2.1:443",
	"java.net.NoRouteToHostException: No route to host",
}

// One app transport failure raises nothing; failures that last the hold
// raise the prompt, as any other can't-reach failure does.
func TestAppTransportFailureRaisesOnlyWhenFailuresLast(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for _, message := range cantReachTestAppTransportFailures {
		rule := newCantReachRule(defaultCantReachSettings())
		if rule.apiFailure(message, t0) {
			t.Errorf("one app transport failure raised the prompt at once: %q", message)
		}
		if !rule.apiFailure(message, t0.Add(20*time.Second)) {
			t.Errorf("app transport failures lasting the hold did not raise the prompt: %q", message)
		}
	}
}

// An Apple URL error that settles it alone raises at once, as "no such host"
// and "x509: " do: a host name that does not exist, and a certificate that is
// not URnetwork's.
func TestAppleUrlErrorsThatSettleItRaiseAtOnce(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for _, message := range []string{
		"NSURLErrorDomain -1003",
		"NSURLErrorDomain -1201",
		"NSURLErrorDomain -1202",
		"Error Domain=NSURLErrorDomain Code=-1203 \"The certificate for this server was signed by an unknown certifying authority.\"",
	} {
		rule := newCantReachRule(defaultCantReachSettings())
		if !rule.apiFailure(message, t0) {
			t.Errorf("not raised at once: %q", message)
		}
	}
}

// Any other Apple URL error, a cancelled request among them, is not a
// can't-reach failure, however long it lasts.
func TestOtherAppleUrlErrorsAreNotCantReach(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for _, message := range []string{
		"NSURLErrorDomain -999",
		"NSURLErrorDomain -1011",
		"NSURLErrorDomain",
	} {
		rule := newCantReachRule(defaultCantReachSettings())
		rule.apiFailure(message, t0)
		if rule.apiFailure(message, t0.Add(30*time.Second)) {
			t.Errorf("raised the prompt: %q", message)
		}
	}
}
