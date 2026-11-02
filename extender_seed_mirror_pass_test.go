//go:build !ios && !android && !js

package sdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The mirror part of a pass through connect's own bootstrap pass, with the
// space's dialer and tls roots (extender_seed_mirror_pass.go; EXTENDER-UPDATE2.md
// §2.3, §2.4): a stalled mirror holds up nothing but itself, a redirect to a
// host nobody configured and an answer past the artifact cap are refused,
// every connection the pass opened is closed when it ends, and the pass keeps
// its deadline and connect's budget. Every mirror is reached at its configured
// address only, through the recorded dialer.

// A tcp listener that accepts and then says nothing, as a mirror whose path
// goes dark after the connect would.
type testSeedStalledMirror struct {
	listener net.Listener
	// connections the client has not closed yet
	openCount atomic.Int32
}

func newTestSeedStalledMirror(t *testing.T) *testSeedStalledMirror {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &testSeedStalledMirror{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			self.openCount.Add(1)
			go func() {
				defer self.openCount.Add(-1)
				defer conn.Close()
				// reads until the client closes, never answering
				io.Copy(io.Discard, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
	})
	return self
}

func (self *testSeedStalledMirror) address() string {
	return self.listener.Addr().String()
}

// An https server for `host` on loopback, its certificate in `roots`.
func newTestSeedHttpsServer(t *testing.T, host string, roots *x509.CertPool, handler http.HandlerFunc) string {
	t.Helper()
	certificate, _ := testProviderMoveCertificate(t, host)
	roots.AddCert(certificate.Leaf)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}

// Runs connect's mirror pass for the space a test builds, and keeps connect's
// account of each pass.
type testSeedConnectMirrorPasses struct {
	// set once the space is built, before any pass runs
	space *NetworkSpace

	stateLock sync.Mutex
	results   []*connect.ExtenderSeedBootstrapResult
}

func (self *testSeedConnectMirrorPasses) install(t *testing.T, clock *testSeedClock, passTimeout time.Duration, attemptTimeout time.Duration) {
	t.Helper()
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
		settings.PassTimeout = passTimeout
		settings.MirrorAttemptTimeout = attemptTimeout
		settings.MirrorPass = func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
			result := runExtenderSeedMirrorPass(ctx, self.space, settings, request)
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.results = append(self.results, result.pass)
			return result
		}
	})
}

func (self *testSeedConnectMirrorPasses) list() []*connect.ExtenderSeedBootstrapResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.results)
}

// The mirrors of the refusal cases, and what they serve.
type testSeedRefusedMirrors struct {
	stalled         *testSeedStalledMirror
	redirectAddress string
	largeAddress    string
	roots           *x509.CertPool
	dialer          *testSeedDialer
}

// The size of the oversized answer: past the artifact cap, which is the paste
// form of a full envelope.
const testSeedLargeAnswerByteCount = 256 * 1024

func newTestSeedRefusedMirrors(t *testing.T) *testSeedRefusedMirrors {
	t.Helper()
	self := &testSeedRefusedMirrors{
		stalled: newTestSeedStalledMirror(t),
		roots:   x509.NewCertPool(),
		dialer:  newTestSeedDialer(),
	}
	self.redirectAddress = newTestSeedHttpsServer(t, "redirect.seed.example", self.roots, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/v1/seed.bin", http.StatusFound)
	})
	self.largeAddress = newTestSeedHttpsServer(t, "large.seed.example", self.roots, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// streamed, so no length warns the reader in advance
		w.(http.Flusher).Flush()
		w.Write(bytes.Repeat([]byte{0x55}, testSeedLargeAnswerByteCount))
	})
	for _, address := range []string{self.stalled.address(), self.redirectAddress, self.largeAddress} {
		self.dialer.route(address, address)
	}
	return self
}

func (self *testSeedRefusedMirrors) values() []ExtenderSeedMirrorValue {
	return []ExtenderSeedMirrorValue{
		{Url: "https://stalled.seed.example", Address: self.stalled.address()},
		{Url: "https://redirect.seed.example", Address: self.redirectAddress},
		{Url: "https://large.seed.example", Address: self.largeAddress},
	}
}

// Every dial of the pass went to a configured address, never to a name and
// never to where the redirect pointed.
func (self *testSeedRefusedMirrors) requireConfiguredDialsOnly(t *testing.T, extra ...string) {
	t.Helper()
	configured := append([]string{self.stalled.address(), self.redirectAddress, self.largeAddress}, extra...)
	for _, address := range self.dialer.dialed() {
		if !slices.Contains(configured, address) {
			t.Errorf("the pass dialed %s, which no mirror configures", address)
		}
	}
}

// The pass's budget: what connect's defaults allow.
func requireTestSeedPassWithinBudget(t *testing.T, result *connect.ExtenderSeedBootstrapResult) {
	t.Helper()
	budget := connect.DefaultExtenderSeedBootstrapSettings()
	if budget.MaxAttemptCount < result.AttemptCount || budget.MaxDialCount < result.DialCount || budget.MaxByteCount < result.ByteCount {
		t.Errorf(
			"the pass took %d attempts, %d dials and %d bytes, past %d, %d and %d",
			result.AttemptCount,
			result.DialCount,
			result.ByteCount,
			budget.MaxAttemptCount,
			budget.MaxDialCount,
			budget.MaxByteCount,
		)
	}
	// the oversized answer was read only to the cap
	if testSeedLargeAnswerByteCount <= result.ByteCount {
		t.Errorf("the pass read %d bytes", result.ByteCount)
	}
}

// A valid mirror among a stalled one and two that are refused: the pass
// applies the valid mirror's bundle without waiting on the stalled one, and
// ends every connection it opened.
func TestExtenderSeedMirrorPassAmongStalledAndRefusedMirrors(t *testing.T) {
	clock := newTestSeedClock()
	envelope := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 17})
	refused := newTestSeedRefusedMirrors(t)
	good := newTestSeedMirror(t, testSeedBytes(t, envelope))
	refused.dialer.route(good.address(), good.address())
	for _, certificate := range good.server.TLS.Certificates {
		refused.roots.AddCert(certificate.Leaf)
	}
	testSeedInstallConnectSettings(t, refused.dialer, func(connectSettings *connect.ConnectSettings) {
		connectSettings.TlsConfig = &tls.Config{RootCAs: refused.roots}
	})
	testSeedInstallEmbedded(t, "")
	testSeedInstallMirrors(t, append(refused.values(), ExtenderSeedMirrorValue{
		Url:     "https://" + testSeedMirrorHost,
		Address: good.address(),
	})...)
	// an attempt at the stalled mirror would hold its connection this long
	attemptTimeout := 20 * time.Second
	passes := &testSeedConnectMirrorPasses{}
	passes.install(t, clock, 2*attemptTimeout, attemptTimeout)
	space := newTestSeedSpace(t, testSeedStoragePath(t))
	passes.space = space

	startTime := time.Now()
	result := space.getExtenderSeed().awaitPass(context.Background())
	if elapsed := time.Since(startTime); attemptTimeout <= elapsed {
		t.Errorf("the pass took %s: the stalled mirror held it up", elapsed)
	}
	if !result.usable || result.outcome != ExtenderSeedPassOutcomeApplied {
		t.Fatalf("the pass with a valid mirror: %+v", result)
	}
	if entry := testSeedDirectoryEntry(space, 0); entry == nil || entry.Source != connect.ExtenderSourceSeed {
		t.Errorf("the mirror's record landed as %+v", entry)
	}
	results := passes.list()
	if len(results) != 1 {
		t.Fatalf("%d connect passes", len(results))
	}
	outcomes := []string{}
	for _, mirrorResult := range results[0].Mirrors {
		outcomes = append(outcomes, mirrorResult.Outcome)
	}
	expected := [][]string{
		{connect.ExtenderSeedMirrorOutcomeCanceled, connect.ExtenderSeedMirrorOutcomeTimeout},
		{connect.ExtenderSeedMirrorOutcomeRedirect, connect.ExtenderSeedMirrorOutcomeCanceled},
		{connect.ExtenderSeedMirrorOutcomeTooLarge, connect.ExtenderSeedMirrorOutcomeCanceled},
		{connect.ExtenderSeedMirrorOutcomeAccepted},
	}
	if len(outcomes) != len(expected) {
		t.Fatalf("the mirror outcomes are %v", outcomes)
	}
	for i, outcome := range outcomes {
		if !slices.Contains(expected[i], outcome) {
			t.Errorf("mirror %d ended %s, not one of %v (all %v)", i, outcome, expected[i], outcomes)
		}
	}
	requireTestSeedPassWithinBudget(t, results[0])
	refused.requireConfiguredDialsOnly(t, good.address())
	testSeedWaitFor(t, func() bool {
		return refused.stalled.openCount.Load() == 0
	})
}

// With no valid mirror the pass ends by its deadline, within its budget, with
// nothing applied and every connection closed; the stalled mirror's attempts
// time out and are tried again, the refused ones are not; and the mirrors are
// then left in backoff.
func TestExtenderSeedMirrorPassWithoutAValidMirrorKeepsItsBounds(t *testing.T) {
	clock := newTestSeedClock()
	refused := newTestSeedRefusedMirrors(t)
	testSeedInstallConnectSettings(t, refused.dialer, func(connectSettings *connect.ConnectSettings) {
		connectSettings.TlsConfig = &tls.Config{RootCAs: refused.roots}
	})
	testSeedInstallEmbedded(t, "")
	testSeedInstallMirrors(t, refused.values()...)
	passTimeout := 5 * time.Second
	passes := &testSeedConnectMirrorPasses{}
	passes.install(t, clock, passTimeout, time.Second)
	space := newTestSeedSpace(t, testSeedStoragePath(t))
	passes.space = space
	seed := space.getExtenderSeed()

	startTime := time.Now()
	result := seed.awaitPass(context.Background())
	if elapsed := time.Since(startTime); passTimeout+5*time.Second < elapsed {
		t.Errorf("the pass took %s, past its %s deadline", elapsed, passTimeout)
	}
	if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeFetch {
		t.Fatalf("the pass without a valid mirror: %+v", result)
	}
	testSeedRequireNothingApplied(t, space)
	results := passes.list()
	if len(results) != 1 {
		t.Fatalf("%d connect passes", len(results))
	}
	mirrors := results[0].Mirrors
	if mirrors[0].Outcome != connect.ExtenderSeedMirrorOutcomeTimeout && mirrors[0].Outcome != connect.ExtenderSeedMirrorOutcomeCanceled {
		t.Errorf("the stalled mirror ended %s", mirrors[0].Outcome)
	}
	if mirrors[0].AttemptCount < 2 {
		t.Errorf("the stalled mirror was attempted %d times, not again after its timeout", mirrors[0].AttemptCount)
	}
	if mirrors[1].Outcome != connect.ExtenderSeedMirrorOutcomeRedirect || mirrors[1].AttemptCount != 1 {
		t.Errorf("the redirecting mirror ended %s after %d attempts", mirrors[1].Outcome, mirrors[1].AttemptCount)
	}
	if mirrors[2].Outcome != connect.ExtenderSeedMirrorOutcomeTooLarge || mirrors[2].AttemptCount != 1 {
		t.Errorf("the oversized mirror ended %s after %d attempts", mirrors[2].Outcome, mirrors[2].AttemptCount)
	}
	requireTestSeedPassWithinBudget(t, results[0])
	refused.requireConfiguredDialsOnly(t)
	testSeedWaitFor(t, func() bool {
		return refused.stalled.openCount.Load() == 0
	})

	// the next caller does not fetch again within the backoff
	if result := seed.awaitPass(context.Background()); result.usable {
		t.Errorf("the pass in backoff: %+v", result)
	}
	if results := passes.list(); len(results) != 1 {
		t.Errorf("the mirrors were fetched again in their backoff: %d passes", len(results))
	}
}

// A dual-stack mirror is two entries with one url, each dialed at its own
// address: the family this path cannot reach holds up nothing, and the bundle
// comes through the other (connect's ExtenderSeedMirror takes one address per
// entry).
func TestExtenderSeedMirrorPassDualStackMirrorIsTwoEntries(t *testing.T) {
	clock := newTestSeedClock()
	envelope := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 23})
	mirror := newTestSeedMirror(t, testSeedBytes(t, envelope))
	dialer := newTestSeedDialer()
	// the recorded dialer refuses the v6 address, which goes nowhere on this path
	unreachable := "[2001:db8::7]:443"
	dialer.route(mirror.address(), mirror.address())
	roots := x509.NewCertPool()
	for _, certificate := range mirror.server.TLS.Certificates {
		roots.AddCert(certificate.Leaf)
	}
	testSeedInstallConnectSettings(t, dialer, func(connectSettings *connect.ConnectSettings) {
		connectSettings.TlsConfig = &tls.Config{RootCAs: roots}
	})
	testSeedInstallEmbedded(t, "")
	testSeedInstallMirrors(
		t,
		ExtenderSeedMirrorValue{Url: "https://" + testSeedMirrorHost, Address: unreachable},
		ExtenderSeedMirrorValue{Url: "https://" + testSeedMirrorHost, Address: mirror.address()},
	)
	passes := &testSeedConnectMirrorPasses{}
	passes.install(t, clock, 30*time.Second, 15*time.Second)
	space := newTestSeedSpace(t, testSeedStoragePath(t))
	passes.space = space
	if status := space.GetExtenderSeedStatus(); status.MirrorCount != 2 || status.DnsIndependentMirrorCount != 2 {
		t.Fatalf("the status counts the two entries as %+v", status)
	}

	result := space.getExtenderSeed().awaitPass(context.Background())
	if !result.usable || result.outcome != ExtenderSeedPassOutcomeApplied {
		t.Fatalf("the pass over a dual-stack mirror: %+v", result)
	}
	results := passes.list()
	if len(results) != 1 || len(results[0].Mirrors) != 2 {
		t.Fatalf("the connect passes are %+v", results)
	}
	if outcome := results[0].Mirrors[1].Outcome; outcome != connect.ExtenderSeedMirrorOutcomeAccepted {
		t.Errorf("the reachable address ended %s", outcome)
	}
	// how it failed is the dialer's to say; it supplied nothing
	if outcome := results[0].Mirrors[0].Outcome; outcome == connect.ExtenderSeedMirrorOutcomeAccepted || outcome == connect.ExtenderSeedMirrorOutcomeIdentical {
		t.Errorf("the unreachable address ended %s", outcome)
	}
	if !dialer.dialedAddress(mirror.address()) {
		t.Errorf("the reachable address was not dialed: %v", dialer.dialed())
	}
	for _, address := range dialer.dialed() {
		if address != unreachable && address != mirror.address() {
			t.Errorf("the pass dialed %s, which neither entry configures", address)
		}
	}
}
