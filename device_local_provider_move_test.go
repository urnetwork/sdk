package sdk

// The rule that a migration the platform asked for never breaks before it
// makes (server/connect/MOVE.md §2.2).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

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

// The make-before-break fix (server/connect/MOVE.md §2.2): with the memory
// budget full of the old explicit H3 carrier, a migration the platform asked
// for closes its replacement and keeps the old carrier. A carrier policy
// change, which nothing else would ever apply, still releases the old carrier
// first.
func TestDeviceLocalProviderPlatformMigrationNeverBreaksBeforeMake(t *testing.T) {
	edgeAddr, quicTlsConfig := newTestProviderMoveH3Edge(t)
	for _, c := range []struct {
		name         string
		migrate      func(provider *deviceLocalProvider)
		breaksBefore bool
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
		provider := &deviceLocalProvider{
			ctx:                       ctx,
			client:                    client,
			auth:                      auth,
			platformTransport:         old,
			platformTransportSettings: settings,
			targetMode:                connect.TransportModeH3,
			transportPolicyVersion:    1,
			migrateConnectTimeout:     100 * time.Millisecond,
			migrateMaxScheduleDelay:   time.Second,
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
		old.Close()
		next.Close()
		strategy.Close()
		client.Close()
		cancel()
	}
}
