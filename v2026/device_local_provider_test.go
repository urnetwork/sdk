package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A `ResidentMigrate` control frame triggers make-before-break: the provider
// builds a replacement platform transport and only swaps once it connects.
// When the replacement cannot connect (unreachable platform here), the old
// transport is kept so traffic continues until the server evicts and the
// drain excuse path covers the reconnect (CONNECTDRAIN2.md §3.3). A migrate
// frame from a non-control source is ignored.
func TestDeviceLocalProviderMigrateKeepsOldOnTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), connect.DefaultClientSettings())
	defer client.Close()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	auth := &connect.ClientAuth{
		ByJwt:      "test",
		InstanceId: connect.NewId(),
		AppVersion: "0.0.0",
	}
	platformTransportSettings := connect.DefaultPlatformTransportSettings()
	// unreachable platform: neither the old nor the replacement can connect
	platformUrl := "ws://127.0.0.1:1"

	oldTransport := connect.NewPlatformTransport(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		platformUrl,
		auth,
		platformTransportSettings,
	)

	provider := &deviceLocalProvider{
		ctx:                       ctx,
		client:                    client,
		appVersion:                "0.0.0",
		instanceId:                connect.NewId(),
		clientStrategy:            clientStrategy,
		platformUrl:               platformUrl,
		platformTransportSettings: platformTransportSettings,
		migrateConnectTimeout:     300 * time.Millisecond,
		auth:                      auth,
		platformTransport:         oldTransport,
	}
	// the bare provider has no local user nat; the transports close with the
	// client ctx
	defer oldTransport.Close()

	migrateFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ResidentMigrate{
		MigrateTime: uint64(time.Now().UnixMilli()),
	})

	// a migrate frame from a non-control source is ignored
	provider.handleControlFrames(connect.SourceId(connect.NewId()), []*protocol.Frame{migrateFrame}, connect.Peer{})
	connect.AssertEqual(t, false, provider.migrating.Load())

	// a control migrate starts a single in-flight migration; with the
	// replacement unable to connect, the old transport is kept
	provider.handleControlFrames(connect.SourceId(connect.ControlId), []*protocol.Frame{migrateFrame}, connect.Peer{})

	migrated := func() bool {
		return !provider.migrating.Load()
	}
	endTime := time.Now().Add(10 * time.Second)
	started := false
	for time.Now().Before(endTime) {
		if provider.migrating.Load() {
			started = true
		}
		if started && migrated() {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	connect.AssertEqual(t, true, started)
	connect.AssertEqual(t, true, migrated())

	func() {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		connect.AssertEqual(t, true, oldTransport == provider.platformTransport)
	}()
}

type fakeMigratablePlatformTransport struct {
	mutex sync.Mutex

	connected bool
	notify    chan struct{}
	auth      *connect.ClientAuth
	closed    bool
}

func newFakeMigratablePlatformTransport(auth *connect.ClientAuth, connected bool) *fakeMigratablePlatformTransport {
	return &fakeMigratablePlatformTransport{
		connected: connected,
		notify:    make(chan struct{}),
		auth:      auth,
	}
}

func (self *fakeMigratablePlatformTransport) ConnectedNotify() <-chan struct{} {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.notify
}

func (self *fakeMigratablePlatformTransport) IsConnected() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.connected
}

func (self *fakeMigratablePlatformTransport) SetAuth(auth *connect.ClientAuth) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.auth = auth
}

func (self *fakeMigratablePlatformTransport) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.closed = true
}

func (self *fakeMigratablePlatformTransport) connect() {
	self.mutex.Lock()
	if self.connected {
		self.mutex.Unlock()
		return
	}
	self.connected = true
	close(self.notify)
	self.notify = make(chan struct{})
	self.mutex.Unlock()
}

func TestDeviceLocalProviderMigrationReappliesRacingAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldAuth := &connect.ClientAuth{
		ByJwt:      "old",
		InstanceId: connect.NewId(),
		AppVersion: "0.0.0",
	}
	oldTransport := newFakeMigratablePlatformTransport(oldAuth, true)
	nextCreated := make(chan *fakeMigratablePlatformTransport, 1)
	provider := &deviceLocalProvider{
		ctx:                     ctx,
		appVersion:              oldAuth.AppVersion,
		instanceId:              oldAuth.InstanceId,
		auth:                    oldAuth,
		platformTransport:       oldTransport,
		migrateConnectTimeout:   2 * time.Second,
		migrateMaxScheduleDelay: 50 * time.Millisecond,
		newPlatformTransport: func(auth *connect.ClientAuth) migratablePlatformTransport {
			next := newFakeMigratablePlatformTransport(auth, false)
			nextCreated <- next
			return next
		},
	}

	done := make(chan struct{})
	go func() {
		// A wildly future server timestamp is clamped to the configured
		// schedule bound rather than pinning migration forever.
		provider.migratePlatformTransport(time.Now().Add(24 * time.Hour))
		close(done)
	}()

	var next *fakeMigratablePlatformTransport
	select {
	case next = <-nextCreated:
	case <-time.After(2 * time.Second):
		t.Fatal("clamped migration did not construct replacement")
	}

	// Rotate auth after the replacement captured the old token but before it
	// connects and swaps.
	provider.SetByJwt("new")
	next.connect()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("migration did not finish")
	}

	next.mutex.Lock()
	nextJwt := next.auth.ByJwt
	next.mutex.Unlock()
	if nextJwt != "new" {
		t.Fatalf("replacement auth = %q, want racing refresh %q", nextJwt, "new")
	}
	oldTransport.mutex.Lock()
	oldClosed := oldTransport.closed
	oldTransport.mutex.Unlock()
	if !oldClosed {
		t.Fatal("old transport was not closed after successful replacement")
	}
}
