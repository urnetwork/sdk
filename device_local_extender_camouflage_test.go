//go:build !ios && !android && !js

package sdk

// The tcp carrier camouflage of the provider extender role (EXTENDER.md P7, phase
// 14c; EXTENDER-UPDATE2 §3): off unless the role's settings ask, and then the same
// settings `connectctl extender` takes. Every list is synthetic and the verification
// is under the test's hand, so nothing leaves the machine.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// The synthetic borrowed name of these cases.
const testProvideCamouflageBorrowName = "borrow.example"

// Installs one measured list as the only bundled one for the rest of the test.
func installTestProvideBorrowList(t *testing.T, names ...string) {
	t.Helper()
	borrowList := &connect.ExtenderBorrowList{
		Version:           1,
		Measured:          true,
		MeasuredAt:        time.Now(),
		MaxMeasurementAge: 365 * 24 * time.Hour,
	}
	for _, name := range names {
		borrowList.Names = append(borrowList.Names, connect.ExtenderBorrowName{Name: name})
	}
	t.Cleanup(connect.Testing_SetExtenderCamouflageBorrowLists([]*connect.ExtenderBorrowList{borrowList}))
}

// Adjusts the camouflage role of every role this test builds.
func configureTestProvideCamouflage(t *testing.T, configure func(settings *extender.ExtenderCamouflageRoleSettings)) {
	t.Helper()
	configureExtenderCamouflageRole = configure
	t.Cleanup(func() {
		configureExtenderCamouflageRole = nil
	})
}

// The verification of a test role: passes unless told otherwise.
type testProvideVerifier struct {
	failing   atomic.Bool
	callCount atomic.Int64
}

func (self *testProvideVerifier) verify(ctx context.Context, network string, borrowName string) bool {
	self.callCount.Add(1)
	return !self.failing.Load()
}

// Off unless the role's settings ask: what the provider fills asks for nothing,
// the server is passed none of it, no key is posted and nothing is verified,
// whatever list is bundled.
func TestDeviceLocalProviderExtenderCamouflageIsOffUnlessItsSettingsAsk(t *testing.T) {
	installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
	verifier := &testProvideVerifier{}
	configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
		settings.VerifyBorrowName = verifier.verify
	})
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	if post := fixture.waitPost(); post.args.RealityPublicKeyHex != "" {
		t.Fatalf("the activation posted the key %q", post.args.RealityPublicKeyHex)
	}
	role := fixture.extender()
	if role.settings.CamouflageEnabled || role.settings.CamouflageSplice || role.settings.CamouflageRevalidationInterval != 0 {
		t.Fatalf("the provider asked for camouflage: %t, %t, %s",
			role.settings.CamouflageEnabled, role.settings.CamouflageSplice, role.settings.CamouflageRevalidationInterval)
	}
	if role.camouflage != nil {
		t.Fatal("the role built a camouflage")
	}
	settings := role.serverSettings(false)
	if settings.CamouflageEnabled || settings.ExtenderCamouflageSplice || settings.CamouflageSpliceGate != nil {
		t.Fatalf("the server is passed camouflage %t, splice %t, gate %t",
			settings.CamouflageEnabled, settings.ExtenderCamouflageSplice, settings.CamouflageSpliceGate != nil)
	}
	server := role.currentServer()
	if server.RealityPublicKey() != nil {
		t.Fatal("the server publishes a key")
	}
	if status := server.CamouflageStatus(); status.Phase != extender.ExtenderCamouflagePhaseOff || status.Covered {
		t.Fatalf("status = %+v, expected off", status)
	}
	if count := verifier.callCount.Load(); count != 0 {
		t.Fatalf("a role with no camouflage verified %d names", count)
	}
}

// What the role passes the server: the camouflage on, the posture it was given and
// the gate, on the role's own identity.
func TestDeviceLocalProviderExtenderCamouflagePassesTheServerItsSettings(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	for _, splice := range []bool{false, true} {
		roleSettings := &deviceLocalExtenderSettings{
			NetworkSpace:      &NetworkSpace{},
			IdentityKeySeed:   seed,
			CamouflageEnabled: true,
			CamouflageSplice:  splice,
		}
		camouflage, err := newExtenderProvideCamouflage(roleSettings)
		if err != nil {
			t.Fatal(err)
		}
		role := &deviceLocalExtender{settings: roleSettings, camouflage: camouflage}
		settings := role.serverSettings(false)
		if !settings.CamouflageEnabled {
			t.Fatal("the server is not passed the camouflage")
		}
		if settings.ExtenderCamouflageSplice != splice {
			t.Fatalf("posture = %t, expected %t", settings.ExtenderCamouflageSplice, splice)
		}
		if settings.CamouflageSpliceGate == nil {
			t.Fatal("the server is not passed the gate")
		}
		if settings.CamouflageSpliceGate() {
			t.Fatal("the gate opened before any record")
		}
		if !bytes.Equal(settings.IdentityKeySeed, seed) {
			t.Fatal("the server is not on the role's identity")
		}
	}
}

// The settings reach the camouflage, and a seed it cannot derive from is the error
// the role reports as its start failing. With nothing asked there is none to fail.
func TestDeviceLocalProviderExtenderCamouflageBuildsFromItsSettings(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	var revalidationInterval atomic.Int64
	var splice atomic.Bool
	configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
		revalidationInterval.Store(int64(settings.RevalidationInterval))
		splice.Store(settings.Splice)
	})
	camouflage, err := newExtenderProvideCamouflage(&deviceLocalExtenderSettings{
		IdentityKeySeed:                seed,
		CamouflageEnabled:              true,
		CamouflageSplice:               true,
		CamouflageRevalidationInterval: 2 * time.Hour,
	})
	if err != nil || camouflage == nil {
		t.Fatalf("camouflage = %v, err = %v", camouflage, err)
	}
	if time.Duration(revalidationInterval.Load()) != 2*time.Hour || !splice.Load() {
		t.Fatalf("settings reached it as %s, %t", time.Duration(revalidationInterval.Load()), splice.Load())
	}

	camouflage, err = newExtenderProvideCamouflage(&deviceLocalExtenderSettings{IdentityKeySeed: []byte("short")})
	if err != nil || camouflage != nil {
		t.Fatalf("nothing asked: camouflage = %v, err = %v", camouflage, err)
	}
	camouflage, err = newExtenderProvideCamouflage(&deviceLocalExtenderSettings{
		IdentityKeySeed:   []byte("short"),
		CamouflageEnabled: true,
	})
	if err == nil || camouflage != nil {
		t.Fatalf("a short seed: camouflage = %v, err = %v", camouflage, err)
	}
	if !strings.Contains(err.Error(), "identity key") {
		t.Fatalf("error = %q, which does not name the identity key", err)
	}
}

// The role end to end with the operator: the post carries the key the server opens
// tags under, the gate reads each activation, it opens a lifetime and the margin
// after the first qualifying record, and it closes at once when the requirement
// leaves the record.
func TestDeviceLocalProviderExtenderCamouflageSettlesFromItsActivations(t *testing.T) {
	installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
	// holds the first post until the operator below is set
	release := make(chan struct{})
	configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
		settings.FirstVerificationTimeout = time.Hour
		settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return true
		}
	})
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.CamouflageSplice = true
	})
	// longer than the role's activation tick, so no record lapses between two
	const recordLifetime = 36 * time.Hour
	func() {
		fixture.operator.stateLock.Lock()
		defer fixture.operator.stateLock.Unlock()
		fixture.operator.signCamouflageKey = true
		fixture.operator.requireCamouflage = true
		fixture.operator.recordLifetime = recordLifetime
	}()
	close(release)

	// every step's caller address check is the barrier: the loop is single
	// threaded, so its hello proves the pass before it, and its gate reads, ran
	barrier := func() {
		t.Helper()
		fixture.step(time.Minute)
	}
	seed := fixture.networkSpace.extenderIdentityKeySeed()
	realityPublicKey, err := connect.ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.waitPass()
	for range 2 {
		if post := fixture.waitPost(); post.args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
			t.Fatalf("the v%d post carried %q", post.ipVersion, post.args.RealityPublicKeyHex)
		}
	}
	barrier()
	server := fixture.extender().currentServer()
	if !bytes.Equal(server.RealityPublicKey(), realityPublicKey) {
		t.Fatal("the server does not open tags under the key the role posts")
	}
	if !server.CamouflageSplice() {
		t.Fatal("the server is not in the posture the role was given")
	}
	status := server.CamouflageStatus()
	if status.Covered || status.DegradedReason != extender.ExtenderCamouflageDegradedGateClosed ||
		status.VerifiedTargetCount != 1 {
		t.Fatalf("status before the records settled = %+v", status)
	}

	// the tick renews the record, which restarts nothing
	fixture.step(24 * time.Hour)
	for range 2 {
		fixture.waitPost()
	}
	barrier()
	if status := server.CamouflageStatus(); status.SpliceGateOpen {
		t.Fatalf("status a tick into the wait = %+v", status)
	}

	// a lifetime and the margin after the first record, inside the second's life
	margin := extender.DefaultExtenderCamouflageSpliceSettleSettings().Margin
	fixture.clock.advance(recordLifetime + margin - 24*time.Hour)
	status = server.CamouflageStatus()
	if !status.Covered || !status.SpliceGateOpen || status.Phase != extender.ExtenderCamouflagePhaseSplicePilot {
		t.Fatalf("status after the records settled = %+v", status)
	}

	func() {
		fixture.operator.stateLock.Lock()
		defer fixture.operator.stateLock.Unlock()
		fixture.operator.requireCamouflage = false
	}()
	fixture.step(24*time.Hour - (recordLifetime + margin - 24*time.Hour))
	for range 2 {
		fixture.waitPost()
	}
	barrier()
	if status := server.CamouflageStatus(); status.SpliceGateOpen || status.Covered {
		t.Fatalf("status after the requirement left the record = %+v", status)
	}
}

// An empty list is no coverage and no error: the role activates with no key, dials
// nothing, and the camouflage says no_verified_target.
func TestDeviceLocalProviderExtenderCamouflageEmptyListIsNoCoverageNotAnError(t *testing.T) {
	installTestProvideBorrowList(t)
	verifier := &testProvideVerifier{}
	configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
		settings.VerifyBorrowName = verifier.verify
	})
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.CamouflageEnabled = true
	})
	fixture.waitPass()
	if post := fixture.waitPost(); post.args.RealityPublicKeyHex != "" {
		t.Fatalf("the activation posted a key with no verified name: %q", post.args.RealityPublicKeyHex)
	}
	provideStatus := fixture.waitStatus("a family activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 || status.ActivatedV6
	})
	if provideStatus.State != ExtenderProvideStateActive {
		t.Fatalf("status = %+v, expected the role active", provideStatus)
	}
	status := fixture.extender().currentServer().CamouflageStatus()
	if !status.Enabled || status.Covered || status.DegradedReason != extender.ExtenderCamouflageDegradedNoVerifiedTarget {
		t.Fatalf("status = %+v, expected no_verified_target", status)
	}
	if count := verifier.callCount.Load(); count != 0 {
		t.Fatalf("an empty list verified %d names", count)
	}
}

// A name that verifies after the first post went out with no key is posted at once,
// so a provider that started before its network was up recovers without a restart.
func TestDeviceLocalProviderExtenderCamouflagePostsTheKeyWhenANameVerifiesLater(t *testing.T) {
	installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
	verifier := &testProvideVerifier{}
	verifier.failing.Store(true)
	retries := make(chan time.Time)
	configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
		settings.VerifyBorrowName = verifier.verify
		settings.After = func(wait time.Duration) <-chan time.Time {
			return retries
		}
	})
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.CamouflageEnabled = true
	})
	fixture.waitPass()
	for range 2 {
		if post := fixture.waitPost(); post.args.RealityPublicKeyHex != "" {
			t.Fatalf("the first post carried %q with no verified name", post.args.RealityPublicKeyHex)
		}
	}
	if stats := fixture.extender().currentServer().CamouflageStats(); stats.BorrowVerifyFailCount == 0 {
		t.Fatal("the failed verification was not counted")
	}

	verifier.failing.Store(false)
	select {
	case retries <- time.Now():
	case <-time.After(60 * time.Second):
		t.Fatal("the role is not waiting to retry")
	}
	seed := fixture.networkSpace.extenderIdentityKeySeed()
	realityPublicKey, err := connect.ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if post := fixture.waitPost(); post.args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
		t.Fatalf("the post after the name verified carried %q", post.args.RealityPublicKeyHex)
	}
}

// Records what a role logs of its camouflage.
type testProvideCamouflageLogger struct {
	connect.Logger
	stateLock sync.Mutex
	lines     []string
}

func (self *testProvideCamouflageLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(line, "[extender]provide camouflage: ") {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testProvideCamouflageLogger) takeLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := self.lines
	self.lines = nil
	return lines
}

// The role logs its camouflage's standing when the standing changes, in the words
// that never call a degraded state coverage. What a client or a prober does moves
// the counts in the line and logs nothing of its own, and a role with no camouflage
// logs nothing at all.
func TestDeviceLocalProviderExtenderCamouflageLogsItsStandingWhenItChanges(t *testing.T) {
	installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	build := func(camouflageEnabled bool) (*deviceLocalExtender, *testProvideCamouflageLogger) {
		logger := &testProvideCamouflageLogger{Logger: connect.NewNoopLogger()}
		roleSettings := &deviceLocalExtenderSettings{
			NetworkSpace:      &NetworkSpace{},
			IdentityKeySeed:   seed,
			CamouflageEnabled: camouflageEnabled,
		}
		camouflage, err := newExtenderProvideCamouflage(roleSettings)
		if err != nil {
			t.Fatal(err)
		}
		role := &deviceLocalExtender{log: logger, settings: roleSettings, camouflage: camouflage}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		role.server = extender.NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, role.serverSettings(false))
		t.Cleanup(role.server.CloseAndWait)
		return role, logger
	}

	role, logger := build(true)
	role.logCamouflageStatus()
	expected := "[extender]provide camouflage: authenticated_rollout, not covered: no_verified_target; " +
		"0 verified names, 0 failed verifications; 0 authenticated, 0 spliced, 0 terminated, 0 fallback terminated\n"
	if lines := logger.takeLines(); !slices.Equal(lines, []string{expected}) {
		t.Fatalf("lines = %q, expected %q", lines, expected)
	}
	role.logCamouflageStatus()
	if lines := logger.takeLines(); 0 < len(lines) {
		t.Fatalf("an unchanged standing logged %q", lines)
	}

	role, logger = build(false)
	role.logCamouflageStatus()
	if lines := logger.takeLines(); 0 < len(lines) {
		t.Fatalf("a role with no camouflage logged %q", lines)
	}
}

// A role off for want of tcp 443 verifies nothing and closes clean; closing a role
// that runs ends a verification in flight.
func TestDeviceLocalProviderExtenderCamouflageFollowsTheRole(t *testing.T) {
	t.Run("off for want of tcp", func(t *testing.T) {
		installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
		verifier := &testProvideVerifier{}
		configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
			settings.VerifyBorrowName = verifier.verify
		})
		tcpPort := testFreeTcpPort(t)
		testHoldTcpPort(t, tcpPort)
		fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
			settings.TcpPort = tcpPort
			settings.CamouflageEnabled = true
			testTcpRetry(settings, make(chan time.Duration, 16), make(chan time.Time))
		})
		fixture.waitStatus("off for want of tcp", func(status *ExtenderProvideStatus) bool {
			return status.ErrorCase == ExtenderProvideErrorTcpUnavailable
		})
		if count := verifier.callCount.Load(); count != 0 {
			t.Fatalf("a role with no server verified %d names", count)
		}
		fixture.extender().Close()
	})

	t.Run("closed with a verification in flight", func(t *testing.T) {
		installTestProvideBorrowList(t, testProvideCamouflageBorrowName)
		entered := make(chan struct{})
		ended := make(chan struct{})
		var once sync.Once
		configureTestProvideCamouflage(t, func(settings *extender.ExtenderCamouflageRoleSettings) {
			settings.FirstVerificationTimeout = time.Millisecond
			settings.VerifyBorrowName = func(ctx context.Context, network string, borrowName string) bool {
				once.Do(func() {
					close(entered)
					<-ctx.Done()
					close(ended)
				})
				return false
			}
		})
		fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
			settings.CamouflageEnabled = true
		})
		select {
		case <-entered:
		case <-time.After(60 * time.Second):
			t.Fatal("the verification never ran")
		}
		fixture.extender().Close()
		select {
		case <-ended:
		default:
			t.Fatal("the role closed with the verification in flight")
		}
	})
}

// A space reads the camouflage clock from its own api and the family hosts under
// it, whichever constructor built it, and leaves the caller's settings as they were.
func TestNetworkSpaceNamesItsApiHostsForTheCamouflageClock(t *testing.T) {
	expected := []string{
		"api." + testProvideExtenderHost,
		"api-v4." + testProvideExtenderHost,
		"api-v6." + testProvideExtenderHost,
	}

	storagePath := t.TempDir()
	manager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(manager.Close)
	stored := manager.updateNetworkSpace(
		NewNetworkSpaceKey(testProvideExtenderHost, "main"),
		func(values *NetworkSpaceValues) {},
	)
	if hosts := stored.clientStrategy.PlatformApiHosts(); !slices.Equal(hosts, expected) {
		t.Fatalf("a stored space's hosts = %v, expected %v", hosts, expected)
	}
	if hosts := stored.derivedClientStrategySettings().PlatformApiHosts; !slices.Equal(hosts, expected) {
		t.Fatalf("a derived strategy's hosts = %v, expected %v", hosts, expected)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := testOfflineClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	urlSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://api."+testProvideExtenderHost,
		"wss://connect."+testProvideExtenderHost,
		strategySettings,
	)
	t.Cleanup(urlSpace.Close)
	if hosts := urlSpace.clientStrategy.PlatformApiHosts(); !slices.Equal(hosts, expected) {
		t.Fatalf("a url space's hosts = %v, expected %v", hosts, expected)
	}
	if 0 < len(strategySettings.PlatformApiHosts) {
		t.Fatalf("the caller's settings were written: %v", strategySettings.PlatformApiHosts)
	}
}

// An api call of a space corrects the camouflage clock when its chain verified, and
// not when it did not, through the transport every api call of the sdk takes.
func TestNetworkSpaceApiCallCorrectsTheCamouflageClock(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{}"))
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	const skew = 10 * time.Minute

	apiCall := func(t *testing.T, tlsConfig *tls.Config) time.Duration {
		t.Helper()
		t.Cleanup(connect.Testing_SetExtenderCamouflageLocalClock(func() time.Time {
			return time.Now().Add(-skew)
		}))
		settings := testOfflineClientStrategySettings()
		settings.EnableResilient = false
		settings.Log = connect.NewNoopLogger()
		settings.ConnectSettings.TlsConfig = tlsConfig
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		networkSpace := NewNetworkSpaceWithUrls(ctx, server.URL, "wss"+strings.TrimPrefix(server.URL, "https"), settings)
		defer networkSpace.close()
		if _, err := networkSpace.api.transportHttpGetRaw()(ctx, server.URL+"/hello", ""); err != nil {
			t.Fatal(err)
		}
		return connect.Testing_ExtenderCamouflageClockOffset(make([]byte, ed25519.PublicKeySize))
	}

	t.Run("a verified chain", func(t *testing.T) {
		offset := apiCall(t, &tls.Config{RootCAs: roots})
		if offset < skew-time.Minute || skew+time.Minute < offset {
			t.Fatalf("offset = %s, expected about +%s to cancel the skew", offset, skew)
		}
	})
	t.Run("no verified chain", func(t *testing.T) {
		if offset := apiCall(t, &tls.Config{InsecureSkipVerify: true}); offset != 0 {
			t.Fatalf("offset = %s, expected the clock not to move", offset)
		}
	})
}
