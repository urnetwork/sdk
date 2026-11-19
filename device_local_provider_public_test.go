package sdk

// The provider's platform dials follow its provide mode (connect/EXTENDER.md
// J4). A provider whose mode includes public reaches the platform on its own
// egress on every transport, the standby included -- through no extender, and
// through its space's proxy when it has one -- so the platform observes the
// address the provider's exit leaves from; a network or friends-and-family
// provider keeps the device's shared strategy and its extender dialers. A
// mode change that flips that rebuilds the provider transports.
//
// The fixtures here run the group without family urls, so the standby is the
// only transport and every recorded dial is the standby's.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Labels every dial the strategy it is installed on makes and refuses it, so
// a test reads which strategy a transport dialed with and never touches the
// network.
type providerDialRecorder struct {
	dials chan string
}

func newProviderDialRecorder() *providerDialRecorder {
	return &providerDialRecorder{dials: make(chan string, 64)}
}

func (self *providerDialRecorder) dialContextSettings(label string) *connect.DialContextSettings {
	return &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			select {
			case self.dials <- label:
			default:
			}
			return nil, fmt.Errorf("dial %s %s refused by the test", network, addr)
		},
	}
}

// The label of the next dial, or "" when none arrives in time.
func (self *providerDialRecorder) nextDial(timeout time.Duration) string {
	select {
	case label := <-self.dials:
		return label
	case <-time.After(timeout):
		return ""
	}
}

// A standby-only provider (no family urls) in a provide mode, whose shared
// strategy and whose own egress strategy dial through distinguishable
// recorders. A plain ws url keeps the dial on the injected dial context, which
// is where gorilla hands an untunneled websocket dial.
func newTestProvideModeProvider(
	t *testing.T,
	provideMode ProvideMode,
) (*deviceLocalProvider, *providerDialRecorder) {
	t.Helper()
	client, _ := newTestProviderClient(t)
	recorder := newProviderDialRecorder()

	// the device's shared strategy, which a non-public provider keeps
	sharedStrategySettings := connect.DefaultClientStrategySettings()
	sharedStrategySettings.ConnectSettings.DialContextSettings = recorder.dialContextSettings("shared")
	sharedStrategy := connect.NewClientStrategy(client.Ctx(), sharedStrategySettings)
	t.Cleanup(sharedStrategy.Close)
	// the settings a public provider's egress strategy is built from
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ConnectSettings.DialContextSettings = recorder.dialContextSettings("egress")

	provider := &deviceLocalProvider{
		ctx:                       client.Ctx(),
		client:                    client,
		clientStrategy:            sharedStrategy,
		clientStrategySettings:    clientStrategySettings,
		platformUrl:               "ws://127.0.0.1:1",
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		targetMode:                connect.TransportModeH1,
		modePreferences:           connect.DefaultTransportModePreferences(),
		transportPolicyVersion:    1,
		migrateConnectTimeout:     connect.DefaultApiMultiClientGeneratorSettings().MigrateConnectTimeout,
		migrateMaxScheduleDelay:   connect.DefaultApiMultiClientGeneratorSettings().MigrateMaxScheduleDelay,
		provideMode:               provideMode,
		auth: &connect.ClientAuth{
			ByJwt:      "test",
			InstanceId: connect.NewId(),
			AppVersion: "0.0.0",
		},
	}
	t.Cleanup(func() {
		provider.stateLock.Lock()
		egressStandbyStrategy := provider.egressStandbyStrategy
		provider.stateLock.Unlock()
		if egressStandbyStrategy != nil {
			egressStandbyStrategy.Close()
		}
	})
	return provider, recorder
}

func waitProviderCondition(timeout time.Duration, condition func() bool) bool {
	for deadline := time.Now().Add(timeout); ; {
		if condition() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func receiveStandbyStrategy(t *testing.T, strategies chan *connect.ClientStrategy) *connect.ClientStrategy {
	t.Helper()
	select {
	case strategy := <-strategies:
		return strategy
	case <-time.After(15 * time.Second):
		t.Fatal("the provide mode change did not rebuild the provider transports")
		return nil
	}
}

// A public provider's standby dials with the provider's own egress strategy,
// which carries no extender dialer and keeps the space's dial (J4). connect's
// TestNewEgressClientStrategyKeepsTheSpaceProxy pins what a strategy from
// NewEgressClientStrategy keeps and drops, and
// TestDeviceLocalProviderPublicStandbyDialsThroughTheSpaceProxy the proxy.
func TestDeviceLocalProviderPublicStandbyDialsOnItsEgress(t *testing.T) {
	provider, recorder := newTestProvideModeProvider(t, ProvideModePublic)
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)

	standbyStrategy := provider.standbyClientStrategy(provider.clientStrategySettings)
	if standbyStrategy == nil || standbyStrategy == provider.clientStrategy {
		t.Fatal("a public provider's standby kept the device's shared strategy")
	}
	label := recorder.nextDial(30 * time.Second)
	if label == "" {
		t.Fatal("the standby never dialed")
	}
	if label != "egress" {
		t.Fatalf("the standby dialed with the %s strategy, want the egress strategy", label)
	}
}

// A network provider keeps the shared strategy, extender dialers included:
// network peers carry no location metadata.
func TestDeviceLocalProviderNetworkStandbyKeepsSharedStrategy(t *testing.T) {
	provider, recorder := newTestProvideModeProvider(t, ProvideModeNetwork)
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)

	if standbyStrategy := provider.standbyClientStrategy(provider.clientStrategySettings); standbyStrategy != provider.clientStrategy {
		t.Fatal("a network provider's standby did not keep the device's shared strategy")
	}
	provider.stateLock.Lock()
	egressStandbyStrategy := provider.egressStandbyStrategy
	provider.stateLock.Unlock()
	if egressStandbyStrategy != nil {
		t.Fatal("a network provider built an egress strategy it does not use")
	}
	label := recorder.nextDial(30 * time.Second)
	if label == "" {
		t.Fatal("the standby never dialed")
	}
	if label != "shared" {
		t.Fatalf("the standby dialed with the %s strategy, want the shared strategy", label)
	}
}

// Flipping the public flag while providing rebuilds the transports, and the
// new generation's standby carries the strategy the new mode calls for, in
// both directions.
func TestDeviceLocalProviderProvideModeFlipRebuildsTransports(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModeNetwork)
	current := newFakeMigratablePlatformTransport(provider.auth, true)
	provider.platformTransport = current

	standbyStrategies := make(chan *connect.ClientStrategy, 4)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		// the strategy this generation's standby dials with, read where the
		// production builder reads it. A connected replacement completes the
		// make-before-break at once.
		standbyStrategies <- provider.standbyClientStrategy(provider.clientStrategySettings)
		return newFakeMigratablePlatformTransport(auth, true)
	}

	provider.setProvideMode(ProvideModePublic)
	egressStandbyStrategy := receiveStandbyStrategy(t, standbyStrategies)
	if egressStandbyStrategy == nil || egressStandbyStrategy == provider.clientStrategy {
		t.Fatal("the rebuilt standby kept the shared strategy after the flip to public")
	}
	if !waitProviderCondition(15*time.Second, func() bool {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		return provider.platformTransport != current && !provider.migrating.Load()
	}) {
		t.Fatal("the rebuilt transport was not installed")
	}
	provider.stateLock.Lock()
	publicTransport := provider.platformTransport
	policyVersion := provider.transportPolicyVersion
	provider.stateLock.Unlock()
	if policyVersion != 2 {
		t.Fatalf("policy version = %d after the flip to public, want 2", policyVersion)
	}

	provider.setProvideMode(ProvideModeNetwork)
	sharedStandbyStrategy := receiveStandbyStrategy(t, standbyStrategies)
	if sharedStandbyStrategy != provider.clientStrategy {
		t.Fatal("the rebuilt standby did not return to the shared strategy after the flip back")
	}
	if !waitProviderCondition(15*time.Second, func() bool {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		return provider.platformTransport != publicTransport && !provider.migrating.Load()
	}) {
		t.Fatal("the flip back did not install a rebuilt transport")
	}
	provider.stateLock.Lock()
	policyVersion = provider.transportPolicyVersion
	provider.stateLock.Unlock()
	if policyVersion != 3 {
		t.Fatalf("policy version = %d after the flip back, want 3", policyVersion)
	}
}

// A provide mode change that leaves the public flag alone only records the
// mode: no policy version bump, so no rebuild is ever requested.
func TestDeviceLocalProviderProvideModeWithoutFlipDoesNotRebuild(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModeNetwork)
	current := newFakeMigratablePlatformTransport(provider.auth, true)
	provider.platformTransport = current

	builds := make(chan struct{}, 4)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		builds <- struct{}{}
		return newFakeMigratablePlatformTransport(auth, true)
	}

	// network -> friends and family, and network -> stream: neither crosses
	// the public flag, since the stream mode provides nothing publicly
	for _, c := range []struct {
		from ProvideMode
		to   ProvideMode
	}{
		{ProvideModeNetwork, ProvideModeFriendsAndFamily},
		{ProvideModeFriendsAndFamily, ProvideModeFriendsAndFamily},
		{ProvideModeNetwork, ProvideModeStream},
	} {
		provider.stateLock.Lock()
		provider.provideMode = c.from
		provider.transportPolicyVersion = 1
		provider.stateLock.Unlock()

		provider.setProvideMode(c.to)

		provider.stateLock.Lock()
		provideMode := provider.provideMode
		policyVersion := provider.transportPolicyVersion
		platformTransport := provider.platformTransport
		provider.stateLock.Unlock()
		if provideMode != c.to {
			t.Errorf("provide mode %d -> %d recorded %d", c.from, c.to, provideMode)
		}
		if policyVersion != 1 {
			t.Errorf("provide mode %d -> %d bumped the policy version to %d", c.from, c.to, policyVersion)
		}
		if platformTransport != current {
			t.Errorf("provide mode %d -> %d replaced the transport", c.from, c.to)
		}
		select {
		case <-builds:
			t.Errorf("provide mode %d -> %d rebuilt the transports", c.from, c.to)
		default:
		}
	}
}

// Public -> stream crosses the public flag: a stream mode provides nothing
// publicly, so the provider rebuilds on the shared strategy and stops declaring
// provide intent, which also lets its client connections offer the transport
// delay header (connect DELAY.md §6).
func TestDeviceLocalProviderPublicToStreamRebuildsWithoutProvideIntent(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModePublic)
	provider.stateLock.Lock()
	publicAuth := *provider.auth
	publicAuth.ProvideIntent = true
	provider.auth = &publicAuth
	current := newFakeMigratablePlatformTransport(provider.auth, true)
	provider.platformTransport = current
	provider.stateLock.Unlock()

	type build struct {
		provideIntent   bool
		standbyStrategy *connect.ClientStrategy
	}
	builds := make(chan build, 4)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		builds <- build{
			provideIntent:   auth.ProvideIntent,
			standbyStrategy: provider.standbyClientStrategy(provider.clientStrategySettings),
		}
		return newFakeMigratablePlatformTransport(auth, true)
	}

	provider.setProvideMode(ProvideModeStream)

	provider.stateLock.Lock()
	provideIntent := provider.auth.ProvideIntent
	policyVersion := provider.transportPolicyVersion
	provider.stateLock.Unlock()
	if provideIntent {
		t.Fatal("the stream mode still declares provide intent")
	}
	if policyVersion != 2 {
		t.Fatalf("policy version = %d after public -> stream, want 2", policyVersion)
	}
	current.mutex.Lock()
	currentProvideIntent := current.auth.ProvideIntent
	current.mutex.Unlock()
	if currentProvideIntent {
		t.Fatal("the running transport kept the public declaration")
	}

	select {
	case b := <-builds:
		if b.provideIntent {
			t.Fatal("the rebuilt transport declares provide intent")
		}
		if b.standbyStrategy != provider.clientStrategy {
			t.Fatal("the rebuilt standby did not return to the shared strategy")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("public -> stream did not rebuild the provider transports")
	}
	if !waitProviderCondition(15*time.Second, func() bool {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		return provider.platformTransport != current && !provider.migrating.Load()
	}) {
		t.Fatal("the rebuilt transport was not installed")
	}
}

// A closed provider records nothing and requests no rebuild.
func TestDeviceLocalProviderProvideModeAfterClose(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModeNetwork)
	provider.stateLock.Lock()
	provider.closed = true
	provider.stateLock.Unlock()

	provider.setProvideMode(ProvideModePublic)

	provider.stateLock.Lock()
	provideMode := provider.provideMode
	policyVersion := provider.transportPolicyVersion
	provider.stateLock.Unlock()
	if provideMode != ProvideModeNetwork || policyVersion != 1 {
		t.Fatalf("closed provider took mode %d at policy version %d", provideMode, policyVersion)
	}
}

// The device hands every provide mode change to its provider, so the rule
// holds for the mode the user actually set.
func TestDeviceLocalProvideModeReachesProvider(t *testing.T) {
	provider, _ := newTestProvideModeProvider(t, ProvideModeNone)
	provider.platformTransport = newFakeMigratablePlatformTransport(provider.auth, true)
	provider.newPlatformTransport = func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport {
		return newFakeMigratablePlatformTransport(auth, true)
	}
	device := &DeviceLocal{
		provider:    provider,
		provideMode: ProvideModePublic,
	}
	device.updateProviderProvideMode()

	provider.stateLock.Lock()
	provideMode := provider.provideMode
	provider.stateLock.Unlock()
	if provideMode != ProvideModePublic {
		t.Fatalf("provider provide mode = %d, want public", provideMode)
	}
	if standbyStrategy := provider.standbyClientStrategy(provider.clientStrategySettings); standbyStrategy == provider.clientStrategy {
		t.Fatal("the provider kept the shared strategy after the device went public")
	}
}
