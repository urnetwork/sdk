package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type providerRotationTestFixture struct {
	device  *DeviceLocal
	ctx     context.Context
	cancel  context.CancelFunc
	clients []*connect.Client
}

func newProviderRotationTestFixture(t *testing.T) *providerRotationTestFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := connect.NewClient(
		ctx,
		connect.NewId(),
		connect.NewNoContractClientOob(),
		connect.DefaultClientSettings(),
	)
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 0
	fixture := &providerRotationTestFixture{
		ctx:     ctx,
		cancel:  cancel,
		clients: []*connect.Client{client},
	}
	device := &DeviceLocal{
		ctx:                                ctx,
		cancel:                             cancel,
		settings:                           settings,
		clientId:                           connect.NewId(),
		provider:                           &deviceLocalProvider{client: client},
		provideMode:                        ProvideModePublic,
		lifecycleDone:                      make(chan struct{}),
		providerPacketStatsChangeListeners: connect.NewCallbackList[PacketStatsChangeListener](),
	}
	fixture.device = device
	device.stateLock.Lock()
	device.ensureRemoteUserNatProviderWithLock()
	device.stateLock.Unlock()
	if device.remoteUserNatProvider == nil || device.remoteUserNatProviderLocalUserNat == nil {
		t.Fatal("provider rotation fixture did not create its first generation")
	}
	t.Cleanup(func() {
		device.stateLock.Lock()
		device.closed = true
		device.provideMode = ProvideModeNone
		device.closeRemoteUserNatProviderWithLock()
		device.stateLock.Unlock()
		device.lifecycleWorkers.Wait()
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		for _, ownedClient := range fixture.clients {
			if err := ownedClient.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close provider client: %v", err)
			}
		}
	})
	return fixture
}

func (self *providerRotationTestFixture) addClient() *connect.Client {
	client := connect.NewClient(
		self.ctx,
		connect.NewId(),
		connect.NewNoContractClientOob(),
		connect.DefaultClientSettings(),
	)
	self.clients = append(self.clients, client)
	return client
}

func waitProviderRotationBarrier(t *testing.T, barrier <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// Saturation atomically detaches the terminal generation, returns without
// joining it, then recreates the same providing intent only after both old NAT
// layers have stopped. Duplicate and stale generation callbacks are no-ops.
func TestDeviceLocalProviderSaturationRotatesGenerationWithoutBlocking(t *testing.T) {
	fixture := newProviderRotationTestFixture(t)
	device := fixture.device
	device.stateLock.Lock()
	oldProvider := device.remoteUserNatProvider
	oldLocalUserNat := device.remoteUserNatProviderLocalUserNat
	oldGeneration := device.remoteUserNatProviderGeneration
	device.stateLock.Unlock()

	joinEntered := make(chan struct{})
	joinRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(joinRelease) }) })
	device.beforeRemoteUserNatProviderRotationJoinForTest = func() {
		close(joinEntered)
		<-joinRelease
	}
	callbackReturned := make(chan struct{})
	go func() {
		device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
		close(callbackReturned)
	}()
	waitProviderRotationBarrier(t, joinEntered, "rotation join hook")
	waitProviderRotationBarrier(t, callbackReturned, "nonblocking saturation callback")

	device.stateLock.Lock()
	if device.remoteUserNatProvider != nil ||
		device.remoteUserNatProviderLocalUserNat != nil ||
		device.retiringRemoteUserNatProvider != oldProvider ||
		!device.remoteUserNatProviderRotationPending {
		device.stateLock.Unlock()
		t.Fatal("saturation did not atomically detach the old provider generation")
	}
	device.stateLock.Unlock()
	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)

	releaseOnce.Do(func() { close(joinRelease) })
	device.lifecycleWorkers.Wait()
	device.stateLock.Lock()
	newProvider := device.remoteUserNatProvider
	newLocalUserNat := device.remoteUserNatProviderLocalUserNat
	newGeneration := device.remoteUserNatProviderGeneration
	pending := device.remoteUserNatProviderRotationPending
	mode := device.provideMode
	device.stateLock.Unlock()
	if newProvider == nil || newProvider == oldProvider ||
		newLocalUserNat == nil || newLocalUserNat == oldLocalUserNat ||
		newGeneration == oldGeneration || pending || mode != ProvideModePublic {
		t.Fatalf(
			"rotation did not recover: providerChanged=%t natChanged=%t generation=%d->%d pending=%t mode=%d",
			newProvider != nil && newProvider != oldProvider,
			newLocalUserNat != nil && newLocalUserNat != oldLocalUserNat,
			oldGeneration,
			newGeneration,
			pending,
			mode,
		)
	}

	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	if device.remoteUserNatProvider != newProvider ||
		device.remoteUserNatProviderGeneration != newGeneration ||
		device.remoteUserNatProviderRotationPending {
		t.Fatal("delayed old saturation callback rotated the replacement generation")
	}
}

// Replacement resolves the provider Client at completion. A legitimate
// carrier-owner change during the old join cannot strand DeviceLocal in the
// providerless interval or recreate against a retired Client.
func TestDeviceLocalProviderRotationUsesCurrentClient(t *testing.T) {
	fixture := newProviderRotationTestFixture(t)
	device := fixture.device
	newClient := fixture.addClient()
	newClient.ContractManager().SetProvidePaused(true)
	var constructedClients []*connect.Client
	device.newRemoteUserNatProviderForTest = func(
		client *connect.Client,
		localUserNat *connect.LocalUserNat,
		settings *connect.RemoteUserNatProviderSettings,
	) *connect.RemoteUserNatProvider {
		constructedClients = append(constructedClients, client)
		return connect.NewRemoteUserNatProvider(client, localUserNat, settings)
	}

	device.stateLock.Lock()
	oldGeneration := device.remoteUserNatProviderGeneration
	device.stateLock.Unlock()
	joinEntered := make(chan struct{})
	joinRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(joinRelease) }) })
	device.beforeRemoteUserNatProviderRotationJoinForTest = func() {
		close(joinEntered)
		<-joinRelease
	}
	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
	waitProviderRotationBarrier(t, joinEntered, "old provider join")
	device.stateLock.Lock()
	device.provider = &deviceLocalProvider{client: newClient}
	device.stateLock.Unlock()
	releaseOnce.Do(func() { close(joinRelease) })
	device.lifecycleWorkers.Wait()

	device.stateLock.Lock()
	activeProvider := device.remoteUserNatProvider
	device.stateLock.Unlock()
	if activeProvider == nil || len(constructedClients) != 1 || constructedClients[0] != newClient {
		t.Fatalf(
			"rotation did not use current Client: active=%t constructions=%d current=%t",
			activeProvider != nil,
			len(constructedClients),
			len(constructedClients) == 1 && constructedClients[0] == newClient,
		)
	}
	if !newClient.ContractManager().IsProvidePaused() || device.GetProvideMode() != ProvideModePublic {
		t.Fatal("rotation changed paused or provide-mode intent")
	}
}

// Turning providing off while the terminal generation joins is authoritative:
// completion clears the pending token but does not resurrect a provider.
func TestDeviceLocalProviderRotationPreservesDisabledIntent(t *testing.T) {
	fixture := newProviderRotationTestFixture(t)
	device := fixture.device
	device.stateLock.Lock()
	oldGeneration := device.remoteUserNatProviderGeneration
	device.stateLock.Unlock()
	joinEntered := make(chan struct{})
	joinRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(joinRelease) }) })
	device.beforeRemoteUserNatProviderRotationJoinForTest = func() {
		close(joinEntered)
		<-joinRelease
	}
	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
	waitProviderRotationBarrier(t, joinEntered, "disabled-intent join")
	device.stateLock.Lock()
	device.provideMode = ProvideModeNone
	device.stateLock.Unlock()
	releaseOnce.Do(func() { close(joinRelease) })
	device.lifecycleWorkers.Wait()

	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	if device.remoteUserNatProvider != nil ||
		device.remoteUserNatProviderLocalUserNat != nil ||
		device.remoteUserNatProviderRotationPending {
		t.Fatal("rotation completion resurrected explicitly disabled providing")
	}
}

// A delayed packet-stat callback may already have been captured when the old
// subscription is removed. Provider+generation identity makes it inert after
// the final old counters are folded, while current-generation callbacks still
// update the observable epoch.
func TestDeviceLocalProviderRotationIgnoresDelayedOldPacketStats(t *testing.T) {
	fixture := newProviderRotationTestFixture(t)
	device := fixture.device
	device.stateLock.Lock()
	oldProvider := device.remoteUserNatProvider
	oldGeneration := device.remoteUserNatProviderGeneration
	device.stateLock.Unlock()
	finalOldStats := &connect.PacketStats{
		RemoteIngressPacketCount: 3,
		RemoteIngressByteCount:   300,
	}
	device.remoteUserNatProviderFinalPacketStatsForTest = func(
		provider *connect.RemoteUserNatProvider,
	) *connect.PacketStats {
		if provider != oldProvider {
			t.Fatal("final stats seam received the wrong provider generation")
		}
		return finalOldStats
	}
	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
	device.lifecycleWorkers.Wait()

	device.stateLock.Lock()
	newProvider := device.remoteUserNatProvider
	newGeneration := device.remoteUserNatProviderGeneration
	baseBeforeDelayed := device.providerPacketStatsBase
	device.stateLock.Unlock()
	if baseBeforeDelayed.RemoteIngressPacketCount != 3 ||
		baseBeforeDelayed.RemoteIngressByteCount != 300 {
		t.Fatalf("old provider counters were not carried once: %+v", baseBeforeDelayed)
	}
	currentStats := &connect.PacketStats{
		RemoteIngressPacketCount: 2,
		RemoteIngressByteCount:   200,
	}
	device.updateProviderPacketStatsForGeneration(newProvider, newGeneration, currentStats)
	device.stateLock.Lock()
	currentTraffic := device.mobileMemoryProviderTrafficByteCount
	device.stateLock.Unlock()

	device.updateProviderPacketStatsForGeneration(oldProvider, oldGeneration, &connect.PacketStats{
		RemoteIngressPacketCount: 100,
		RemoteIngressByteCount:   10000,
	})
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	if device.providerPacketStatsBase.RemoteIngressPacketCount !=
		baseBeforeDelayed.RemoteIngressPacketCount ||
		device.providerPacketStatsBase.RemoteIngressByteCount !=
			baseBeforeDelayed.RemoteIngressByteCount ||
		device.mobileMemoryProviderTrafficByteCount != currentTraffic {
		t.Fatal("delayed old packet-stat callback mutated the replacement epoch")
	}
}

// Device closure can win while a saturation join is in flight. The admitted
// rotation worker still completes, but its generation token cannot recreate
// provider state after closed is published.
func TestDeviceLocalProviderRotationDoesNotRaceClose(t *testing.T) {
	fixture := newProviderRotationTestFixture(t)
	device := fixture.device
	device.stateLock.Lock()
	oldGeneration := device.remoteUserNatProviderGeneration
	device.stateLock.Unlock()
	joinEntered := make(chan struct{})
	joinRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(joinRelease) }) })
	device.beforeRemoteUserNatProviderRotationJoinForTest = func() {
		close(joinEntered)
		<-joinRelease
	}
	device.remoteUserNatProviderSourceLifecycleSaturated(oldGeneration)
	waitProviderRotationBarrier(t, joinEntered, "close-race join")
	device.stateLock.Lock()
	device.closed = true
	device.stateLock.Unlock()
	releaseOnce.Do(func() { close(joinRelease) })
	device.lifecycleWorkers.Wait()

	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	if device.remoteUserNatProvider != nil || device.remoteUserNatProviderRotationPending {
		t.Fatal("closed DeviceLocal recreated provider state after rotation")
	}
}
