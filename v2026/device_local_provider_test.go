package sdk

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Resident migration may wait for a future schedule and a replacement
// transport, but its Client receive callback only admits one owned worker and
// returns. Running the migration inline would stall every Transfer sequence.
func TestDeviceLocalProviderMigrateReceiveCallbackDoesNotWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &deviceLocalProvider{
		ctx:                     ctx,
		migrateMaxScheduleDelay: time.Hour,
	}
	migrateFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.ResidentMigrate{
		MigrateTime: uint64(time.Now().Add(24 * time.Hour).UnixMilli()),
	})
	defer connect.MessagePoolReturn(migrateFrame.MessageBytes)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		provider.handleControlFrames(
			connect.SourceId(connect.ControlId),
			[]*protocol.Frame{migrateFrame},
			connect.Peer{},
		)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("resident migration blocked the Client receive callback")
	}
	if !provider.migrating.Load() {
		t.Fatal("resident migration callback did not admit its worker")
	}
}

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
	defer clientStrategy.Close()
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

	connected        bool
	waitingForBudget bool
	notify           chan struct{}
	auth             *connect.ClientAuth
	closed           bool
	waitStarted      chan struct{}
	waitStartedOnce  sync.Once
}

func newFakeMigratablePlatformTransport(auth *connect.ClientAuth, connected bool) *fakeMigratablePlatformTransport {
	return &fakeMigratablePlatformTransport{
		connected:   connected,
		notify:      make(chan struct{}),
		auth:        auth,
		waitStarted: make(chan struct{}),
	}
}

func (self *fakeMigratablePlatformTransport) ConnectedNotify() <-chan struct{} {
	self.waitStartedOnce.Do(func() {
		close(self.waitStarted)
	})
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.notify
}

func (self *fakeMigratablePlatformTransport) IsConnected() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.connected
}

func (self *fakeMigratablePlatformTransport) IsWaitingForBudget() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.waitingForBudget
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
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	defer clientStrategy.Close()
	oobApi := connect.NewBringYourApi(ctx, clientStrategy, "http://unused.invalid")
	oobApi.SetByJwt(oldAuth.ByJwt)
	nextCreated := make(chan *fakeMigratablePlatformTransport, 1)
	provider := &deviceLocalProvider{
		ctx:                     ctx,
		appVersion:              oldAuth.AppVersion,
		instanceId:              oldAuth.InstanceId,
		auth:                    oldAuth,
		clientOob:               connect.NewApiOutOfBandControlWithApi(oobApi),
		platformTransport:       oldTransport,
		migrateConnectTimeout:   2 * time.Second,
		migrateMaxScheduleDelay: 50 * time.Millisecond,
		newPlatformTransport: func(
			auth *connect.ClientAuth,
			_ connect.TransportMode,
			_ *connect.PlatformTransportSettings,
		) migratablePlatformTransport {
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
	if got := oobApi.ByJwt(); got != "new" {
		t.Fatalf("out-of-band auth = %q, want refreshed JWT", got)
	}
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

func TestDeviceLocalProviderTransportPolicyIsLiveAndMakeBeforeBreak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	auth := &connect.ClientAuth{
		ByJwt:      "test",
		InstanceId: connect.NewId(),
		AppVersion: "0.0.0",
	}
	oldTransport := newFakeMigratablePlatformTransport(auth, true)
	type creation struct {
		transport   *fakeMigratablePlatformTransport
		targetMode  connect.TransportMode
		preferences map[connect.TransportMode]int
	}
	created := make(chan creation, 2)
	provider := &deviceLocalProvider{
		ctx:                       ctx,
		auth:                      auth,
		platformTransport:         oldTransport,
		targetMode:                connect.TransportModeH1,
		transportPolicyVersion:    1,
		migrateConnectTimeout:     2 * time.Second,
		migrateMaxScheduleDelay:   time.Second,
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		newPlatformTransport: func(
			auth *connect.ClientAuth,
			targetMode connect.TransportMode,
			settings *connect.PlatformTransportSettings,
		) migratablePlatformTransport {
			next := newFakeMigratablePlatformTransport(auth, false)
			created <- creation{
				transport:   next,
				targetMode:  targetMode,
				preferences: maps.Clone(settings.ModePreferences),
			}
			return next
		},
	}

	preferences := map[connect.TransportMode]int{
		connect.TransportModeH3:        1,
		connect.TransportModeH1:        1,
		connect.TransportModeH3Dns:     2,
		connect.TransportModeH3DnsPump: 3,
	}
	provider.SetTransportPolicy(connect.TransportModeAuto, preferences)
	var next creation
	select {
	case next = <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("transport policy change did not construct a replacement")
	}
	connect.AssertEqual(t, next.targetMode, connect.TransportModeAuto)
	connect.AssertEqual(t, maps.Equal(next.preferences, preferences), true)

	oldTransport.mutex.Lock()
	oldClosedBeforeConnect := oldTransport.closed
	oldTransport.mutex.Unlock()
	if oldClosedBeforeConnect {
		t.Fatal("old provider transport closed before replacement connected")
	}

	// The setter owns its policy copy; caller mutation cannot alter either the
	// pending replacement or the policy used by a later migration.
	preferences[connect.TransportModeH3] = 99
	next.transport.connect()
	deadline := time.Now().Add(2 * time.Second)
	for provider.migrating.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if provider.migrating.Load() {
		t.Fatal("provider policy migration did not finish")
	}

	provider.stateLock.Lock()
	current := provider.platformTransport
	storedPreferences := maps.Clone(provider.modePreferences)
	provider.stateLock.Unlock()
	connect.AssertEqual(t, current == next.transport, true)
	connect.AssertEqual(t, storedPreferences[connect.TransportModeH3], 1)
	oldTransport.mutex.Lock()
	oldClosedAfterConnect := oldTransport.closed
	oldTransport.mutex.Unlock()
	if !oldClosedAfterConnect {
		t.Fatal("old provider transport remained open after replacement connected")
	}

	// Reapplying the canonical policy is a no-op, not another connection churn.
	provider.SetTransportPolicy(connect.TransportModeAuto, storedPreferences)
	if provider.migrating.Load() {
		t.Fatal("duplicate provider transport policy started a migration")
	}
	select {
	case <-created:
		t.Fatal("duplicate provider transport policy built a replacement")
	default:
	}
}

// The provider owns a separate top-level PlatformTransport, so pin the same
// 25 policy edges as outbound API windows. A changed policy (or resident
// migration on a diagonal entry) must construct the exact destination, keep
// the source installed until activation, then publish the destination and
// drain the source.
func TestDeviceLocalProviderTransportPolicyTransitionMatrix(t *testing.T) {
	modes := []connect.TransportMode{
		connect.TransportModeH1,
		connect.TransportModeH3,
		connect.TransportModeH3Dns,
		connect.TransportModeH3DnsPump,
		connect.TransportModeAuto,
	}
	preferencesFor := func(mode connect.TransportMode) map[connect.TransportMode]int {
		if mode == connect.TransportModeAuto {
			return connect.DefaultTransportModePreferences()
		}
		return nil
	}

	for _, sourceMode := range modes {
		for _, targetMode := range modes {
			sourceMode := sourceMode
			targetMode := targetMode
			t.Run(string(sourceMode)+"_to_"+string(targetMode), func(t *testing.T) {
				auth := &connect.ClientAuth{InstanceId: connect.NewId()}
				source := newFakeMigratablePlatformTransport(auth, true)
				t.Cleanup(source.Close)
				type creation struct {
					mode      connect.TransportMode
					transport *fakeMigratablePlatformTransport
				}
				created := make(chan creation, 1)
				provider := &deviceLocalProvider{
					ctx:                       t.Context(),
					auth:                      auth,
					platformTransport:         source,
					targetMode:                sourceMode,
					modePreferences:           preferencesFor(sourceMode),
					transportPolicyVersion:    1,
					migrateConnectTimeout:     time.Second,
					migrateMaxScheduleDelay:   time.Second,
					platformTransportSettings: connect.DefaultPlatformTransportSettings(),
					newPlatformTransport: func(
						auth *connect.ClientAuth,
						mode connect.TransportMode,
						_ *connect.PlatformTransportSettings,
					) migratablePlatformTransport {
						next := newFakeMigratablePlatformTransport(auth, false)
						created <- creation{mode: mode, transport: next}
						return next
					},
				}

				provider.SetTransportPolicy(targetMode, preferencesFor(targetMode))
				if sourceMode == targetMode {
					if provider.migrating.Load() {
						t.Fatal("identical provider policy started a migration")
					}
					select {
					case <-created:
						t.Fatal("identical provider policy constructed a replacement")
					default:
					}
					provider.requestPlatformTransportMigration(time.Now())
				}

				var replacement creation
				select {
				case replacement = <-created:
				case <-time.After(time.Second):
					t.Fatal("provider transition did not construct a replacement")
				}
				t.Cleanup(replacement.transport.Close)
				if replacement.mode != targetMode {
					t.Fatalf("constructed mode=%q want=%q", replacement.mode, targetMode)
				}
				select {
				case <-replacement.transport.waitStarted:
				case <-time.After(time.Second):
					t.Fatal("provider transition did not wait for destination activation")
				}
				provider.stateLock.Lock()
				currentBeforeActivation := provider.platformTransport
				provider.stateLock.Unlock()
				if currentBeforeActivation != source {
					t.Fatal("provider installed destination before activation")
				}
				source.mutex.Lock()
				sourceClosed := source.closed
				source.mutex.Unlock()
				if sourceClosed {
					t.Fatal("provider drained source before destination activation")
				}

				replacement.transport.connect()
				deadline := time.Now().Add(time.Second)
				for provider.migrating.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				provider.stateLock.Lock()
				current := provider.platformTransport
				provider.stateLock.Unlock()
				if provider.migrating.Load() || current != replacement.transport ||
					!replacement.transport.IsConnected() {
					t.Fatal("provider did not install the active destination")
				}
				source.mutex.Lock()
				sourceClosed = source.closed
				source.mutex.Unlock()
				if !sourceClosed {
					t.Fatal("provider did not drain source after destination activation")
				}
			})
		}
	}
}

func TestDeviceLocalProviderExplicitPolicyKeepsOldCarrierUntilH3ConnectsWhenBudgetBlocked(t *testing.T) {
	auth := &connect.ClientAuth{InstanceId: connect.NewId()}
	oldTransport := newFakeMigratablePlatformTransport(auth, true)
	nextTransport := newFakeMigratablePlatformTransport(auth, false)
	nextTransport.waitingForBudget = true
	created := make(chan struct{}, 1)
	provider := &deviceLocalProvider{
		ctx:                       t.Context(),
		auth:                      auth,
		platformTransport:         oldTransport,
		targetMode:                connect.TransportModeAuto,
		modePreferences:           connect.DefaultTransportModePreferences(),
		transportPolicyVersion:    1,
		migrateConnectTimeout:     time.Second,
		migrateMaxScheduleDelay:   time.Second,
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		newPlatformTransport: func(
			*connect.ClientAuth,
			connect.TransportMode,
			*connect.PlatformTransportSettings,
		) migratablePlatformTransport {
			created <- struct{}{}
			return nextTransport
		},
	}

	provider.SetTransportPolicy(connect.TransportModeH3, nil)
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("explicit H3 provider policy did not construct a replacement")
	}
	select {
	case <-nextTransport.waitStarted:
	case <-time.After(time.Second):
		t.Fatal("explicit provider H3 migration did not start waiting for its replacement")
	}
	oldTransport.mutex.Lock()
	oldClosed := oldTransport.closed
	oldTransport.mutex.Unlock()
	if oldClosed {
		t.Fatal("budget-blocked explicit H3 closed provider H1 before H3 connected")
	}

	nextTransport.connect()
	for deadline := time.Now().Add(time.Second); provider.migrating.Load() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	provider.stateLock.Lock()
	current := provider.platformTransport
	provider.stateLock.Unlock()
	if current != nextTransport {
		t.Fatal("explicit provider H3 replacement was not installed")
	}
	oldTransport.mutex.Lock()
	oldClosed = oldTransport.closed
	oldTransport.mutex.Unlock()
	if !oldClosed {
		t.Fatal("old provider H1 carrier did not drain after explicit H3 connected")
	}
}
