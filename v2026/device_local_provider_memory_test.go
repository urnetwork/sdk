package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

func providerMemoryTestDevice(t *testing.T, target ByteCount) (*DeviceLocal, *connect.Client) {
	t.Helper()
	device, _ := transferMemoryTestDevice(t, target)
	ctx, cancel := context.WithCancel(t.Context())
	settings := connect.DefaultClientSettings()
	settings.Log = connect.NewNoopLogger()
	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), settings)
	device.ctx, device.cancel = ctx, cancel
	device.clientId = client.ClientId()
	device.provider.client = client
	device.provideMode = ProvideModePublic
	device.log = connect.NewNoopLogger()
	device.providerPacketStatsChangeListeners = connect.NewCallbackList[PacketStatsChangeListener]()
	t.Cleanup(func() {
		device.stateLock.Lock()
		device.closed = true
		device.provideMode = ProvideModeNone
		device.closeRemoteUserNatProviderWithLock()
		device.stateLock.Unlock()
		cancel()
		device.lifecycleWorkers.Wait()
		_ = client.CloseAndWait(context.Background())
	})
	return device, client
}

func TestDeviceLocalProviderMemoryPermanentPolicyRefusal(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, _ := providerMemoryTestDevice(t, targetMiB*1024*1024)
			var factoryCalls atomic.Int64
			device.providerSecurityPolicyGenerator = func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy {
				factoryCalls.Add(1)
				return connect.DisableSecurityPolicy()
			}
			device.stateLock.Lock()
			err := device.ensureRemoteUserNatProviderWithLock()
			refused := device.remoteUserNatProvider == nil && device.remoteUserNatProviderLocalUserNat == nil && !device.remoteUserNatProviderMemoryWait
			device.stateLock.Unlock()
			var policyError *connect.NatMemoryPolicyError
			if !errors.As(err, &policyError) || !refused || factoryCalls.Load() != 0 {
				t.Fatalf("permanent refusal installed an owner/retry or invoked opaque factory: %v", err)
			}
			device.lifecycleWorkers.Wait()
			if stats := device.transferMemory.nat.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("permanent refusal stranded admitted NAT: %+v", stats)
			}
		})
	}
}

func TestDeviceLocalProviderMemoryRequiredStatsAdmissionRetries(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, _ := providerMemoryTestDevice(t, targetMiB*1024*1024)
			memory := device.transferMemory
			// Exactly enough for NAT+provider, but not the required subscription.
			held := memory.root.TotalByteCount() - (256+512)*1024
			if !memory.client.TryReserve(held) {
				t.Fatal("sibling fill failed")
			}
			defer func() { memory.client.Release(held) }()
			device.applyProvideMemorySharesWithLock(true)
			device.stateLock.Lock()
			err := device.ensureRemoteUserNatProviderWithLock()
			refused := device.remoteUserNatProvider == nil && device.remoteUserNatProviderLocalUserNat != nil && device.remoteUserNatProviderMemoryWait
			generation := device.remoteUserNatProviderGeneration
			device.stateLock.Unlock()
			if !errors.Is(err, connect.ErrNatMemoryBudget) || !refused || memory.nat.UsedByteCount() != 256*1024 {
				t.Fatalf("stats refusal installed incomplete generation: %v", err)
			}
			reserved := memory.nat.Stats().ReservedByteCount
			// No self-generated release notification or rebuild loop is possible:
			// atomic refusal has allocated neither provider nor registration.
			select {
			case <-time.After(20 * time.Millisecond):
			case <-device.ctx.Done():
				t.Fatal("unexpected cancellation")
			}
			device.stateLock.Lock()
			spun := device.remoteUserNatProviderGeneration != generation
			device.stateLock.Unlock()
			if spun || memory.nat.Stats().ReservedByteCount != reserved {
				t.Fatal("required stats refusal spun build/retire workers")
			}
			memory.client.Release(1024)
			held -= 1024
			joined := make(chan struct{})
			go func() { device.lifecycleWorkers.Wait(); close(joined) }()
			waitProviderRotationBarrier(t, joined, "required subscription retry")
			device.stateLock.Lock()
			installed := device.remoteUserNatProvider != nil && device.providerPacketStatsSub != nil && !device.remoteUserNatProviderMemoryWait
			device.stateLock.Unlock()
			if !installed || memory.nat.UsedByteCount() != (256+512+1)*1024 {
				t.Fatal("drained sibling did not admit complete provider graph")
			}
		})
	}
}

func TestDeviceLocalProviderMemoryCapturedStatsCloseAndFinalSnapshot(t *testing.T) {
	device, _ := providerMemoryTestDevice(t, 20*1024*1024)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	device.beforeRemoteUserNatProviderPacketStatsForTest = func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}
	device.newRemoteUserNatProviderForTest = func(client *connect.Client, nat *connect.LocalUserNat, settings *connect.RemoteUserNatProviderSettings) *connect.RemoteUserNatProvider {
		settings.EventEpoch = time.Millisecond
		return connect.NewRemoteUserNatProvider(client, nat, settings)
	}
	var finalCount atomic.Int64
	device.remoteUserNatProviderCloseFinalStatsForTest = func(*connect.RemoteUserNatProvider) *connect.PacketStats {
		return &connect.PacketStats{RemoteIngressPacketCount: finalCount.Load()}
	}
	device.stateLock.Lock()
	err := device.ensureRemoteUserNatProviderWithLock()
	device.stateLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitProviderRotationBarrier(t, entered, "captured stats callback")
	detached := make(chan struct{})
	go func() {
		device.stateLock.Lock()
		device.provideMode = ProvideModeNone
		device.closeRemoteUserNatProviderWithLock()
		device.stateLock.Unlock()
		close(detached)
	}()
	waitProviderRotationBarrier(t, detached, "detach without joining under stateLock")
	finalCount.Store(19) // A final admitted return completes while Close drains.
	unblock()
	joined := make(chan struct{})
	go func() { device.lifecycleWorkers.Wait(); close(joined) }()
	waitProviderRotationBarrier(t, joined, "captured callback close join")
	device.stateLock.Lock()
	final := device.providerPacketStatsBase.RemoteIngressPacketCount
	device.stateLock.Unlock()
	if final != 19 || device.transferMemory.nat.UsedByteCount() != 0 {
		t.Fatalf("close lost final snapshot or released before join: final=%d used=%d", final, device.transferMemory.nat.UsedByteCount())
	}
}

func TestDeviceLocalProviderMemoryWorstSupportedOverlapProfiles(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, client := providerMemoryTestDevice(t, targetMiB*1024*1024)
			memory := device.transferMemory
			device.applyProvideMemorySharesWithLock(true)
			var nats []*connect.LocalUserNat
			for _, name := range []string{"fallback", "retiring-provider-local", "replacement-provider-local"} {
				settings := connect.DefaultLocalUserNatSettings()
				settings.Log, settings.MemoryBudget = connect.NewNoopLogger(), memory.nat
				nat, err := connect.TryNewLocalUserNat(device.ctx, name, settings)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = nat.CloseAndWait(context.Background()) })
				nats = append(nats, nat)
			}
			var providers []*connect.RemoteUserNatProvider
			for _, nat := range nats[1:] {
				settings := connect.DefaultRemoteUserNatProviderSettings()
				settings.WriteTimeout = time.Millisecond
				provider, _, err := connect.TryNewRemoteUserNatProviderWithPacketStats(client, nat, settings, func(*connect.RemoteUserNatProvider, *connect.PacketStats) {})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(provider.Close)
				providers = append(providers, provider)
			}
			const exactOverlap = (3*256 + 2*(512+1)) * 1024
			if memory.nat.UsedByteCount() != exactOverlap || memory.root.UsedByteCount() != exactOverlap || memory.nat.Available() < 254*1024 {
				t.Fatal("fallback+old/new provider graph escaped shared profile ledger")
			}
			addr, stop := startUdpEchoServer(t)
			defer stop()
			peer, err := net.ResolveUDPAddr("udp4", addr)
			if err != nil {
				t.Fatal(err)
			}
			received := make(chan struct{}, 1)
			nats[2].AddReceivePacketsCallback(func(connect.TransferPath, protocol.ProvideMode, *connect.IpPath, [][]byte) {
				select {
				case received <- struct{}{}:
				default:
				}
			})
			packet := connect.MessagePoolCopy(craftIpv4Packet(connect.IpProtocolUdp, net.IPv4(10, 0, 0, 2), 42000, peer.IP, peer.Port, false, []byte("profile-overlap")))
			if !nats[2].SendPacket(connect.SourceId(connect.NewId()), protocol.ProvideMode_Network, packet, 0) {
				connect.MessagePoolReturn(packet)
				t.Fatal("worst overlap left no useful UDP admission")
			}
			waitProviderRotationBarrier(t, received, "overlap UDP echo")
			if memory.nat.UsedByteCount() > memory.nat.TotalByteCount() || memory.root.UsedByteCount() > memory.root.TotalByteCount() {
				t.Fatal("overlap traffic overdraw")
			}
			for _, provider := range providers {
				provider.Close()
			}
			for _, nat := range nats {
				_ = nat.CloseAndWait(context.Background())
			}
			if stats := memory.nat.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || memory.root.UsedByteCount() != 0 {
				t.Fatalf("profile overlap teardown imbalance: %+v", stats)
			}
		})
	}
}
