package sdk

// The provider's part in the platform's connection moves
// (connect/resident_move.go, server/connect/MOVE.md §5.2): a move over real
// transports, the advisory failure and decline paths with their reports, the
// mobile background or metered decline, and the rule that a migration the
// platform asked for never breaks before it makes.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

const testProviderMoveHost = "connect.space.example"

// Documentation addresses (RFC 5737) for move targets.
var testProviderMoveTarget = netip.MustParseAddr("192.0.2.10")

// A certificate for `host` and the pool that trusts it.
func testProviderMoveCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}

// One upgrade a test edge accepted.
type testProviderMoveRequest struct {
	host        string
	serverName  string
	moveVersion string
}

// A local TLS WebSocket server standing in for one platform edge.
type testProviderMoveEdge struct {
	address  string
	requests chan testProviderMoveRequest
}

func newTestProviderMoveEdge(t *testing.T, certificate tls.Certificate) *testProviderMoveEdge {
	t.Helper()
	self := &testProviderMoveEdge{
		requests: make(chan testProviderMoveRequest, 16),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		serverName := ""
		if request.TLS != nil {
			serverName = request.TLS.ServerName
		}
		conn, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		self.requests <- testProviderMoveRequest{
			host:        request.Host,
			serverName:  serverName,
			moveVersion: request.Header.Get(connect.HeaderResidentMoveVersion),
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	self.address = server.Listener.Addr().String()
	return self
}

func receiveTestProviderMove[T any](t *testing.T, values <-chan T, message string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal(message)
		var empty T
		return empty
	}
}

// The reports a provider sent, in order.
type testProviderMoveReports struct {
	stateLock sync.Mutex
	results   []*protocol.ResidentMoveResult
}

func (self *testProviderMoveReports) observe(result *protocol.ResidentMoveResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.results = append(self.results, proto.Clone(result).(*protocol.ResidentMoveResult))
}

func (self *testProviderMoveReports) snapshot() []*protocol.ResidentMoveResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.results)
}

func requireTestProviderMoveReports(t *testing.T, reports *testProviderMoveReports, want ...*protocol.ResidentMoveResult) {
	t.Helper()
	got := reports.snapshot()
	if len(got) != len(want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Fatalf("report %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func testProviderMoveFrame(kind protocol.ResidentMoveKind, moveId connect.Id, targetIps ...netip.Addr) *protocol.Frame {
	targets := []*protocol.ResidentMoveTarget{}
	for _, targetIp := range targetIps {
		targets = append(targets, &protocol.ResidentMoveTarget{Ip: targetIp.AsSlice()})
	}
	message := &protocol.ResidentMigrate{
		MigrateTime:    uint64(time.Now().UnixMilli()),
		MoveId:         moveId.Bytes(),
		Kind:           kind,
		Targets:        targets,
		PreferDuration: uint64((6 * time.Hour).Milliseconds()),
	}
	if kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory {
		message.CloseTime = uint64(time.Now().Add(time.Minute).UnixMilli())
	}
	return connect.RequireToFrameWithDefaultProtocolVersion(message)
}

// Joins the provider's migration worker.
func waitTestProviderMigration(t *testing.T, provider *deviceLocalProvider) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		provider.migrationWorkers.Wait()
		close(done)
	}()
	receiveTestProviderMove(t, done, "the migration worker did not finish")
}

// A whole move over real transports: the provider's transport group reached
// by name takes an advisory move to a target address served by another edge.
// The replacement group dials the target with the platform hostname kept for
// TLS and HTTP, and the old group closes only after the replacement arrived.
func TestDeviceLocalProviderMoveSwapsToTheTargetMakeBeforeBreak(t *testing.T) {
	certificate, roots := testProviderMoveCertificate(t, testProviderMoveHost)
	edgeByName := newTestProviderMoveEdge(t, certificate)
	edgeAtTarget := newTestProviderMoveEdge(t, certificate)
	routes := map[string]string{
		net.JoinHostPort(testProviderMoveHost, "443"):            edgeByName.address,
		net.JoinHostPort(testProviderMoveTarget.String(), "443"): edgeAtTarget.address,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = 2 * time.Second
	strategySettings.TlsConfig = &tls.Config{RootCAs: roots}
	strategySettings.Resolver = testOfflineResolver
	strategySettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			if serverAddress, ok := routes[address]; ok {
				return (&net.Dialer{}).DialContext(ctx, "tcp", serverAddress)
			}
			return nil, errors.New("test: no route to this address")
		},
	}
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	clientSettings := connect.DefaultClientSettings()
	clientSettings.ControlPingTimeout = 0
	clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOff
	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
	defer client.Close()

	settings := connect.DefaultPlatformTransportSettings()
	settings.EnableH1Plus = false
	settings.ReconnectTimeout = 50 * time.Millisecond
	settings.ResidentMoveTracker = connect.NewResidentMoveTracker()
	reports := &testProviderMoveReports{}
	auth := &connect.ClientAuth{ByJwt: "test", InstanceId: connect.NewId(), AppVersion: "0.0.0"}
	provider := &deviceLocalProvider{
		ctx:                           ctx,
		client:                        client,
		clientStrategy:                strategy,
		clientStrategySettings:        strategySettings,
		platformUrl:                   "wss://" + testProviderMoveHost,
		platformTransportSettings:     settings,
		targetMode:                    connect.TransportModeH1,
		modePreferences:               connect.DefaultTransportModePreferences(),
		transportPolicyVersion:        1,
		migrateConnectTimeout:         10 * time.Second,
		migrateMaxScheduleDelay:       time.Second,
		auth:                          auth,
		platformTransportMonitor:      connect.NewMonitor(),
		sendResidentMoveResultForTest: reports.observe,
	}
	old := provider.newProviderPlatformTransport(auth, connect.TransportModeH1, settings)
	provider.platformTransport = old
	defer old.Close()
	receiveTestProviderMove(t, edgeByName.requests, "the provider did not reach the edge by name")
	if !waitForProviderCondition(10*time.Second, old.IsConnected) {
		t.Fatal("the provider did not connect")
	}
	oldGroup := old.(*connect.FamilyPlatformTransportGroup)

	provider.handleControlFrames(
		connect.SourceId(connect.ControlId),
		[]*protocol.Frame{testProviderMoveFrame(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, connect.NewId(), testProviderMoveTarget)},
		connect.Peer{},
	)
	request := receiveTestProviderMove(t, edgeAtTarget.requests, "the replacement did not reach the target edge")
	if request.host != testProviderMoveHost || request.serverName != testProviderMoveHost || request.moveVersion != "1" {
		t.Fatalf("target edge upgrade = %+v", request)
	}
	// make before break: the old group still carried when the replacement
	// reached the target edge
	select {
	case <-oldGroup.Done():
		t.Fatal("the old transports closed before the replacement reached the target")
	default:
	}
	receiveTestProviderMove(t, oldGroup.Done(), "the old transports did not close after the arrival")
	waitTestProviderMigration(t, provider)
	provider.stateLock.Lock()
	current := provider.platformTransport
	provider.stateLock.Unlock()
	if current == old || !current.IsConnected() {
		t.Fatal("the connected replacement is not the provider's transport")
	}
	defer current.Close()
	if !settings.ResidentMoveTracker.MovedWithin(time.Minute, time.Now()) {
		t.Fatal("the move does not count for the client interval")
	}
	requireTestProviderMoveReports(t, reports)
}

func waitForProviderCondition(timeout time.Duration, condition func() bool) bool {
	endTime := time.Now().Add(timeout)
	for {
		if condition() {
			return true
		}
		if !time.Now().Before(endTime) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A provider over fixture transports: `create` builds each replacement.
type testProviderMoveFixture struct {
	provider *deviceLocalProvider
	old      *fakeMigratablePlatformTransport
	reports  *testProviderMoveReports
	created  chan *connect.PlatformTransportSettings
}

func newTestProviderMoveFixture(
	t *testing.T,
	settings *connect.PlatformTransportSettings,
	create func() migratablePlatformTransport,
) *testProviderMoveFixture {
	t.Helper()
	auth := &connect.ClientAuth{InstanceId: connect.NewId()}
	self := &testProviderMoveFixture{
		old:     newFakeMigratablePlatformTransport(auth, true),
		reports: &testProviderMoveReports{},
		created: make(chan *connect.PlatformTransportSettings, 4),
	}
	self.provider = &deviceLocalProvider{
		ctx:                           t.Context(),
		auth:                          auth,
		platformTransport:             self.old,
		platformTransportSettings:     settings,
		targetMode:                    connect.TransportModeAuto,
		modePreferences:               connect.DefaultTransportModePreferences(),
		transportPolicyVersion:        1,
		migrateConnectTimeout:         5 * time.Second,
		migrateMaxScheduleDelay:       time.Second,
		sendResidentMoveResultForTest: self.reports.observe,
		newPlatformTransport: func(
			*connect.ClientAuth,
			connect.TransportMode,
			*connect.PlatformTransportSettings,
		) migratablePlatformTransport {
			return create()
		},
	}
	created := self.provider.newPlatformTransport
	self.provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		self.created <- settings
		return created(auth, targetMode, settings)
	}
	return self
}

func (self *testProviderMoveFixture) move(kind protocol.ResidentMoveKind, moveId connect.Id, targetIps ...netip.Addr) {
	self.provider.handleControlFrames(
		connect.SourceId(connect.ControlId),
		[]*protocol.Frame{testProviderMoveFrame(kind, moveId, targetIps...)},
		connect.Peer{},
	)
}

// An advisory move builds its replacement with a strict dial of the target
// sharing the provider's move memory. A replacement that never arrives keeps
// the old transport and reports a connect timeout.
func TestDeviceLocalProviderAdvisoryMoveTimeoutKeepsTheOldTransport(t *testing.T) {
	next := newFakeMigratablePlatformTransport(&connect.ClientAuth{}, false)
	settings := connect.DefaultPlatformTransportSettings()
	fixture := newTestProviderMoveFixture(t, settings, func() migratablePlatformTransport { return next })
	fixture.provider.migrateConnectTimeout = 50 * time.Millisecond
	moveId := connect.NewId()
	fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, moveId, testProviderMoveTarget)
	created := receiveTestProviderMove(t, fixture.created, "the move did not build a replacement")
	if created.ResidentMoveDial == nil || !created.ResidentMoveDial.Strict() || created.ResidentMoveDial.MoveId() != moveId {
		t.Fatal("the replacement does not carry the strict move dial")
	}
	if created.ResidentMoveTracker == nil || created.ResidentMoveTracker != fixture.provider.moveTracker() {
		t.Fatal("the replacement does not share the provider's move memory")
	}
	waitTestProviderMigration(t, fixture.provider)
	fixture.provider.stateLock.Lock()
	current := fixture.provider.platformTransport
	fixture.provider.stateLock.Unlock()
	if current != fixture.old {
		t.Fatal("a timed out move displaced the old transport")
	}
	next.mutex.Lock()
	nextClosed := next.closed
	next.mutex.Unlock()
	fixture.old.mutex.Lock()
	oldClosed := fixture.old.closed
	fixture.old.mutex.Unlock()
	if !nextClosed || oldClosed {
		t.Fatalf("replacement closed = %t, old closed = %t", nextClosed, oldClosed)
	}
	reports := fixture.reports.snapshot()
	if len(reports) != 1 ||
		reports[0].Outcome != protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed ||
		reports[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout ||
		!slices.Equal(reports[0].MoveId, moveId.Bytes()) || reports[0].Elapsed < 50 {
		t.Fatalf("reports = %v, want one connect timeout", reports)
	}
}

// The mobile decline (D11): while the app is in the background or the active
// network is metered, an advisory move is declined and reported, and a
// compulsory one still moves. With the policy off, the state declines nothing.
func TestDeviceLocalProviderDeclinesAdvisoryMovesInTheBackgroundOrMetered(t *testing.T) {
	for _, c := range []struct {
		name         string
		enabled      bool
		backgrounded bool
		metered      bool
		declined     bool
	}{
		{name: "foreground unmetered", enabled: true},
		{name: "backgrounded", enabled: true, backgrounded: true, declined: true},
		{name: "metered", enabled: true, metered: true, declined: true},
		{name: "policy off", enabled: false, backgrounded: true, metered: true},
	} {
		moveDecline := newDeviceLocalMoveDecline(c.enabled)
		device := &DeviceLocal{moveDecline: moveDecline}
		device.SetAppBackgrounded(c.backgrounded)
		device.SetNetworkMetered(c.metered)
		settings := newDeviceLocalPlatformTransportSettings(0, nil, nil, "", "")
		settings.MoveDecline = device.moveDeclineHook()
		if (settings.MoveDecline == nil) == c.enabled {
			t.Fatalf("%s: hook installed = %t", c.name, settings.MoveDecline != nil)
		}
		fixture := newTestProviderMoveFixture(t, settings, func() migratablePlatformTransport {
			return newFakeMigratablePlatformTransport(&connect.ClientAuth{}, false)
		})
		fixture.provider.migrateConnectTimeout = 10 * time.Millisecond
		moveId := connect.NewId()
		fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, moveId, testProviderMoveTarget)
		waitTestProviderMigration(t, fixture.provider)
		select {
		case <-fixture.created:
			if c.declined {
				t.Fatalf("%s: a declined move built a replacement", c.name)
			}
		default:
			if !c.declined {
				t.Fatalf("%s: an accepted move built no replacement", c.name)
			}
		}
		reports := fixture.reports.snapshot()
		if c.declined {
			requireTestProviderMoveReports(t, fixture.reports, connect.NewResidentMoveDeclined(
				moveId,
				protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered,
				0,
			))
		} else if len(reports) != 1 || reports[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout {
			t.Fatalf("%s: reports = %v, want the accepted move's timeout", c.name, reports)
		}

		// a compulsory move is never declined
		compulsoryId := connect.NewId()
		fixture.move(protocol.ResidentMoveKind_ResidentMoveKindCompulsory, compulsoryId, testProviderMoveTarget)
		created := receiveTestProviderMove(t, fixture.created, c.name+": the compulsory move built no replacement")
		if created.ResidentMoveDial == nil || created.ResidentMoveDial.Strict() {
			t.Fatalf("%s: the compulsory replacement does not try its target", c.name)
		}
		waitTestProviderMigration(t, fixture.provider)
	}
}

// A move while another migration is in flight is declined as in progress; a
// repeated move id is dropped without a report.
func TestDeviceLocalProviderMoveInProgressAndRepeats(t *testing.T) {
	settings := connect.DefaultPlatformTransportSettings()
	settings.MoveDecline = func() bool { return true }
	fixture := newTestProviderMoveFixture(t, settings, func() migratablePlatformTransport {
		return newFakeMigratablePlatformTransport(&connect.ClientAuth{}, false)
	})
	fixture.provider.migrating.Store(true)
	inProgressId := connect.NewId()
	fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, inProgressId, testProviderMoveTarget)
	fixture.provider.migrating.Store(false)
	declinedId := connect.NewId()
	fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, declinedId, testProviderMoveTarget)
	// both ids were seen, so their repeats are dropped
	fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, inProgressId, testProviderMoveTarget)
	fixture.move(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, declinedId, testProviderMoveTarget)
	waitTestProviderMigration(t, fixture.provider)
	select {
	case <-fixture.created:
		t.Fatal("a declined or repeated move built a replacement")
	default:
	}
	requireTestProviderMoveReports(t, fixture.reports,
		connect.NewResidentMoveDeclined(inProgressId, protocol.ResidentMoveReason_ResidentMoveReasonMoveInProgress, 0),
		connect.NewResidentMoveDeclined(declinedId, protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered, 0),
	)
}

// A drain frame still migrates by name, make-before-break, with no move dial
// and no report.
func TestDeviceLocalProviderDrainMigratesByNameAsBefore(t *testing.T) {
	next := newFakeMigratablePlatformTransport(&connect.ClientAuth{}, false)
	fixture := newTestProviderMoveFixture(t, connect.DefaultPlatformTransportSettings(), func() migratablePlatformTransport { return next })
	drain := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ResidentMigrate{
		MigrateTime: uint64(time.Now().UnixMilli()),
	})
	fixture.provider.handleControlFrames(connect.SourceId(connect.ControlId), []*protocol.Frame{drain}, connect.Peer{})
	created := receiveTestProviderMove(t, fixture.created, "the drain built no replacement")
	if created.ResidentMoveDial != nil {
		t.Fatal("a drain replacement dials a target")
	}
	fixture.old.mutex.Lock()
	oldClosed := fixture.old.closed
	fixture.old.mutex.Unlock()
	if oldClosed {
		t.Fatal("the drain closed the old transport before the replacement connected")
	}
	next.connect()
	waitTestProviderMigration(t, fixture.provider)
	fixture.old.mutex.Lock()
	oldClosed = fixture.old.closed
	fixture.old.mutex.Unlock()
	if !oldClosed {
		t.Fatal("the drain did not close the old transport after the replacement connected")
	}
	requireTestProviderMoveReports(t, fixture.reports)
}

// A local QUIC platform edge on loopback: it answers each auth frame with an
// echo, so an H3 carrier dialing it connects.
func newTestProviderMoveH3Edge(t *testing.T) (*net.UDPAddr, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "move-test"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	const nextProto = "urnetwork-provider-move-test"
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
			NextProtos:   []string{nextProto},
		},
		&quic.Config{MaxIdleTimeout: 10 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		serverCancel()
		listener.Close()
	})
	framerSettings := connect.DefaultFramerSettings(int(connect.DefaultClientSettings().MinimumMessageLenLimit()))
	go func() {
		for {
			connection, err := listener.Accept(serverCtx)
			if err != nil {
				return
			}
			go func() {
				stream, err := connection.AcceptStream(serverCtx)
				if err != nil {
					return
				}
				framer := connect.NewFramer(framerSettings)
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				err = framer.Write(stream, authBytes)
				connect.MessagePoolReturn(authBytes)
				if err != nil {
					return
				}
				<-connection.Context().Done()
			}()
		}
	}()
	return listener.Addr().(*net.UDPAddr), &tls.Config{
		RootCAs:    roots,
		NextProtos: []string{nextProto},
	}
}

// The make-before-break fix (MOVE.md §2.2): with the memory budget full of
// the old explicit H3 carrier, a migration the platform asked for -- a drain,
// a compulsory or an advisory move -- closes its replacement and keeps the old
// carrier, and only the advisory move reports. A carrier policy change, which
// nothing else would ever apply, still releases the old carrier first.
func TestDeviceLocalProviderPlatformMigrationNeverBreaksBeforeMake(t *testing.T) {
	edgeAddr, quicTlsConfig := newTestProviderMoveH3Edge(t)
	for _, c := range []struct {
		name         string
		migrate      func(provider *deviceLocalProvider)
		breaksBefore bool
		reported     bool
	}{
		{
			name: "drain",
			migrate: func(provider *deviceLocalProvider) {
				provider.handleControlFrames(connect.SourceId(connect.ControlId), []*protocol.Frame{
					connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ResidentMigrate{
						MigrateTime: uint64(time.Now().UnixMilli()),
					}),
				}, connect.Peer{})
			},
		},
		{
			name: "compulsory move",
			migrate: func(provider *deviceLocalProvider) {
				provider.handleControlFrames(connect.SourceId(connect.ControlId), []*protocol.Frame{
					testProviderMoveFrame(protocol.ResidentMoveKind_ResidentMoveKindCompulsory, connect.NewId(), testProviderMoveTarget),
				}, connect.Peer{})
			},
		},
		{
			name: "advisory move",
			migrate: func(provider *deviceLocalProvider) {
				provider.handleControlFrames(connect.SourceId(connect.ControlId), []*protocol.Frame{
					testProviderMoveFrame(protocol.ResidentMoveKind_ResidentMoveKindAdvisory, connect.NewId(), testProviderMoveTarget),
				}, connect.Peer{})
			},
			reported: true,
		},
		{
			name: "policy change",
			migrate: func(provider *deviceLocalProvider) {
				provider.requestPlatformTransportMigration(time.Now())
			},
			breaksBefore: true,
		},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		clientSettings := connect.DefaultClientSettings()
		clientSettings.ControlPingTimeout = 0
		clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOff
		client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
		strategy := connect.NewClientStrategy(ctx, testOfflineClientStrategySettings())
		budget := connect.NewPlatformTransportBudget(4, 1)
		settings := connect.DefaultPlatformTransportSettings()
		settings.PlatformTransportBudget = budget
		settings.H3BudgetByteCount = 4
		settings.H3Port = edgeAddr.Port
		settings.QuicTlsConfig = quicTlsConfig
		auth := &connect.ClientAuth{ByJwt: "test", InstanceId: connect.NewId(), AppVersion: "0.0.0"}
		newH3 := func(settings *connect.PlatformTransportSettings) *connect.PlatformTransport {
			return connect.NewPlatformTransportWithTargetMode(
				client.Ctx(),
				strategy,
				client.RouteManager(),
				"https://127.0.0.1",
				auth,
				connect.TransportModeH3,
				settings,
			)
		}
		old := newH3(settings)
		if !waitForProviderCondition(10*time.Second, func() bool {
			return old.IsConnected() && old.ActiveMode() == connect.TransportModeH3
		}) {
			t.Fatalf("%s: the old H3 carrier did not connect", c.name)
		}
		if stats := budget.Stats(); stats.UsedTransportCount != 1 {
			t.Fatalf("%s: the old H3 carrier does not hold the budget: %+v", c.name, stats)
		}
		created := make(chan *connect.PlatformTransport, 1)
		reports := &testProviderMoveReports{}
		provider := &deviceLocalProvider{
			ctx:                           ctx,
			client:                        client,
			auth:                          auth,
			platformTransport:             old,
			platformTransportSettings:     settings,
			targetMode:                    connect.TransportModeH3,
			transportPolicyVersion:        1,
			migrateConnectTimeout:         100 * time.Millisecond,
			migrateMaxScheduleDelay:       time.Second,
			sendResidentMoveResultForTest: reports.observe,
			newPlatformTransport: func(
				_ *connect.ClientAuth,
				_ connect.TransportMode,
				settings *connect.PlatformTransportSettings,
			) migratablePlatformTransport {
				next := newH3(settings)
				created <- next
				return next
			},
		}
		c.migrate(provider)
		next := receiveTestProviderMove(t, created, c.name+": no replacement")
		waitTestProviderMigration(t, provider)
		oldDone := false
		select {
		case <-old.Done():
			oldDone = true
		default:
		}
		if oldDone != c.breaksBefore {
			t.Fatalf("%s: old carrier released = %t, want %t", c.name, oldDone, c.breaksBefore)
		}
		if !c.breaksBefore {
			select {
			case <-next.Done():
			default:
				t.Fatalf("%s: the replacement the budget cannot hold was not closed", c.name)
			}
			provider.stateLock.Lock()
			current := provider.platformTransport
			provider.stateLock.Unlock()
			if current != old || !old.IsConnected() {
				t.Fatalf("%s: the old carrier does not carry the provider", c.name)
			}
		}
		got := reports.snapshot()
		if c.reported {
			if len(got) != 1 ||
				got[0].Outcome != protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined ||
				got[0].Reason != protocol.ResidentMoveReason_ResidentMoveReasonMemoryBudget {
				t.Fatalf("%s: reports = %v, want a memory budget decline", c.name, got)
			}
		} else if 0 < len(got) {
			t.Fatalf("%s: reports = %v, want none", c.name, got)
		}
		old.Close()
		next.Close()
		strategy.Close()
		client.Close()
		cancel()
	}
}

// A provider's transports as a move sees them: a group's every transport, a
// single platform transport, and a fixture as a connected agnostic carrier.
func TestProviderResidentMoveTransports(t *testing.T) {
	provider := newTestFamilyProvider(t, "wss://connect-v4.space.example", "wss://connect-v6.space.example")
	group := provider.newProviderPlatformTransport(provider.auth, connect.TransportModeH1, provider.platformTransportSettings)
	defer waitTransportDone(t, group)
	moveTransports := providerResidentMoveTransports(group)
	families := []int{}
	for _, moveTransport := range moveTransports {
		families = append(families, moveTransport.IpFamily())
	}
	if !slices.Equal(families, []int{4, 6, 0}) {
		t.Fatalf("group transports = %v, want v4, v6 and the standby", families)
	}
	fixture := newFakeMigratablePlatformTransport(&connect.ClientAuth{}, true)
	views := providerResidentMoveTransports(fixture)
	if len(views) != 1 || views[0].IpFamily() != 0 || !views[0].IsConnected() || views[0].RelayCarried() ||
		views[0].ActiveMode() != connect.TransportModeNone {
		t.Fatalf("fixture view = %+v", views)
	}
	if providerResidentMoveTransports(nil) != nil {
		t.Fatal("no transport has a view")
	}
}

// The device builds every platform transport with its move policy: the
// defaults keep moves on, the provider and each window decline as the app
// reports, and the policy off installs no hook.
func TestDeviceLocalPlatformTransportsCarryTheMovePolicy(t *testing.T) {
	if !DefaultDeviceLocalSettings().MoveDeclineBackgroundOrMetered {
		t.Fatal("the mobile move decline is off by default")
	}
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	if device.provider == nil {
		t.Fatal("the fixture has no provider")
	}
	device.provider.platformTransport.Close()
	providerSettings := device.provider.platformTransportSettings
	windowSettings := device.windowPlatformTransportSettings()
	for name, transportSettings := range map[string]*connect.PlatformTransportSettings{
		"provider": providerSettings,
		"window":   windowSettings,
	} {
		if !transportSettings.EnableMove || transportSettings.MoveDecline == nil {
			t.Fatalf("%s settings lost the move policy", name)
		}
		if transportSettings.MoveDecline() {
			t.Fatalf("%s declines before the app reported anything", name)
		}
	}
	if providerSettings.ResidentMoveTracker == nil {
		t.Fatal("the provider has no move memory shared by its generations")
	}
	device.SetNetworkMetered(true)
	if !providerSettings.MoveDecline() || !windowSettings.MoveDecline() {
		t.Fatal("a metered network does not decline")
	}
	device.SetNetworkMetered(false)
	device.SetAppBackgrounded(true)
	if !providerSettings.MoveDecline() || !windowSettings.MoveDecline() {
		t.Fatal("a backgrounded app does not decline")
	}
	device.SetAppBackgrounded(false)
	if providerSettings.MoveDecline() || windowSettings.MoveDecline() {
		t.Fatal("the foreground on an unmetered network declines")
	}

	offSettings := DefaultDeviceLocalSettings()
	offSettings.DisableLogging = true
	offSettings.MoveDeclineBackgroundOrMetered = false
	offDevice := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), offSettings, true)
	if offDevice.provider != nil {
		offDevice.provider.platformTransport.Close()
		if offDevice.provider.platformTransportSettings.MoveDecline != nil {
			t.Fatal("the policy off still installs a provider hook")
		}
	}
	if offDevice.windowPlatformTransportSettings().MoveDecline != nil {
		t.Fatal("the policy off still installs a window hook")
	}
}
