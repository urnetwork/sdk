package sdk

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func transferMemoryTestDevice(t *testing.T, target ByteCount) (*DeviceLocal, *connect.ClientSettings) {
	t.Helper()
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.AllowProvider = true
	memory := newDeviceLocalTransferMemoryForPlatform(settings, true)
	configureDeviceLocalClientMemoryForPlatform(settings, memory, true)
	_, _, _, providerShare := deviceMemoryShares(settings)
	providerSettings := newDeviceClientSettings(&settings.ClientSettings, "", nil)
	providerSend, providerReceive := configureDeviceLocalProviderMemory(providerSettings, providerShare, memory.provider)
	device := &DeviceLocal{
		settings:       settings,
		transferMemory: memory,
		provider: &deviceLocalProvider{
			resendQueueBudget:  providerSend,
			receiveQueueBudget: providerReceive,
		},
		dnsMemoryTarget:         connect.NewMemoryTarget(target / 10),
		platformTransportBudget: connect.NewPlatformTransportBudget(0, 0),
	}
	device.applyProvideMemorySharesWithLock(false)
	return device, providerSettings
}

func TestDeviceLocalTransferHierarchyProfilesAndOwnership(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, provider := transferMemoryTestDevice(t, targetMiB*1024*1024)
			memory := device.transferMemory
			_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
			connect.AssertEqual(t, memory.root.TotalByteCount(), clientShare+providerShare)
			connect.AssertEqual(t, memory.client.TotalByteCount(), clientShare+providerShare)
			connect.AssertEqual(t, memory.provider.TotalByteCount(), deviceLocalProviderIdleTransferByteCount)
			connect.AssertEqual(t, memory.nat.TotalByteCount(), providerShare/2)
			client := &device.settings.ClientSettings
			for name, budget := range map[string]*connect.TransferMemoryBudget{
				"client resend":  client.SendBufferSettings.ResendQueueBudget,
				"client receive": client.ReceiveBufferSettings.ReceiveQueueBudget,
				"shared Pack":    client.ReceiveBufferSettings.PackQueueBudget,
				"client P2P":     client.WebRtcSettings.MemoryBudget,
			} {
				if budget == nil || budget.Parent() != memory.client {
					t.Fatalf("%s escaped client parent", name)
				}
			}
			for name, budget := range map[string]*connect.TransferMemoryBudget{
				"provider resend":      provider.SendBufferSettings.ResendQueueBudget,
				"provider receive":     provider.ReceiveBufferSettings.ReceiveQueueBudget,
				"provider public P2P":  provider.WebRtcSettings.MemoryBudget,
				"provider network P2P": provider.WebRtcSettings.NetworkPeerMemoryBudget,
			} {
				if budget == nil || budget.Parent() != memory.provider {
					t.Fatalf("%s escaped provider parent", name)
				}
			}
			if provider.ReceiveBufferSettings.PackQueueBudget != client.ReceiveBufferSettings.PackQueueBudget {
				t.Fatal("shared Pack handoff was counted twice")
			}
			for _, settings := range []*connect.ClientSettings{client, provider} {
				if !settings.SendBufferSettings.ResendQueueRetainedByteAccounting || !settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting {
					t.Fatal("parented queues were left on legacy overdraft accounting")
				}
			}
			// A selected peer gets a distinct leaf per generation, never a new
			// independent root. Old and new generations therefore compose.
			_, oldPeer := deviceLocalDestinationWebRtcSettings(client.WebRtcSettings, true)
			_, newPeer := deviceLocalDestinationWebRtcSettings(client.WebRtcSettings, true)
			if oldPeer == newPeer || oldPeer.Parent() != memory.client || newPeer.Parent() != memory.client {
				t.Fatal("selected peer generations do not share client admission")
			}
			device.applyProvideMemorySharesWithLock(true)
			connect.AssertEqual(t, memory.client.TotalByteCount(), clientShare)
			connect.AssertEqual(t, memory.provider.TotalByteCount(), providerShare/2)
			connect.AssertEqual(t, memory.nat.TotalByteCount(), providerShare/2)
			// Existing target-scaled queue sizes remain independent leaves.
			connect.AssertEqual(t, client.SendBufferSettings.ResendQueueBudget.TotalByteCount(), max(clientShare*3/7, 1024*1024))
			connect.AssertEqual(t, client.ReceiveBufferSettings.ReceiveQueueBudget.TotalByteCount(), mobileReceiveQueueBudgetForPlatform(device.settings.MemoryTargetByteCount, clientShare, true))
			connect.AssertEqual(t, client.ReceiveBufferSettings.PackQueueBudget.TotalByteCount(), mobilePackQueueBudgetByteCountForTarget(clientShare, device.settings.MemoryTargetByteCount))
		})
	}
	settings := DefaultDeviceLocalSettings()
	if newDeviceLocalTransferMemoryForPlatform(settings, false) != nil {
		t.Fatal("mobile hierarchy changed desktop legacy policy")
	}
	settings.MemoryTargetByteCount = 0
	if newDeviceLocalTransferMemoryForPlatform(settings, true) != nil {
		t.Fatal("targetless client acquired a hard hierarchy")
	}
}

func TestDeviceLocalTransferHierarchyRoleOverlapBothDirections(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, _ := transferMemoryTestDevice(t, targetMiB*1024*1024)
			memory := device.transferMemory
			rootTotal := memory.root.TotalByteCount()
			// Model owners from retired window generations, not merely current
			// settings. They keep the same group root until their actual release.
			oldWindow := connect.NewTransferMemoryBudgetWithParent(rootTotal, memory.client)
			connect.AssertEqual(t, oldWindow.TryReserve(rootTotal), true)
			device.applyProvideMemorySharesWithLock(true)
			connect.AssertEqual(t, memory.provider.TryReserve(1), false)
			connect.AssertEqual(t, memory.nat.TryReserve(1), false)
			connect.AssertEqual(t, oldWindow.TryReserve(1), false)
			providerTotal, natTotal := memory.provider.TotalByteCount(), memory.nat.TotalByteCount()
			notify := memory.provider.CapacityNotify()
			oldWindow.Release(providerTotal + natTotal)
			select {
			case <-notify:
			case <-time.After(time.Second):
				t.Fatal("provider role did not wake when old clients drained")
			}
			connect.AssertEqual(t, memory.provider.TryReserve(providerTotal), true)
			connect.AssertEqual(t, memory.nat.TryReserve(natTotal), true)
			connect.AssertEqual(t, memory.root.UsedByteCount(), rootTotal)
			device.applyProvideMemorySharesWithLock(false)
			connect.AssertEqual(t, oldWindow.TryReserve(1), false)
			connect.AssertEqual(t, memory.provider.TryReserve(1), false)
			// NAT remains permitted in provide-off, but not in addition to a
			// fully used root. Existing fallback owners cannot disappear.
			connect.AssertEqual(t, memory.nat.TotalByteCount(), natTotal)
			memory.provider.Release(providerTotal)
			connect.AssertEqual(t, oldWindow.TryReserve(providerTotal), true)
			memory.nat.Release(natTotal)
			connect.AssertEqual(t, oldWindow.TryReserve(natTotal), true)
			oldWindow.Release(rootTotal)
			stats := memory.root.Stats()
			connect.AssertEqual(t, stats.UsedByteCount, ByteCount(0))
			connect.AssertEqual(t, stats.ReservedByteCount, stats.ReleasedByteCount)
		})
	}
}

func TestDeviceLocalTransferHierarchyRoleResizeFanout(t *testing.T) {
	for _, targetMiB := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(targetMiB), func(t *testing.T) {
			device, provider := transferMemoryTestDevice(t, targetMiB*1024*1024)
			memory := device.transferMemory
			client := &device.settings.ClientSettings
			_, selected := deviceLocalDestinationWebRtcSettings(client.WebRtcSettings, true)
			leaves := []*connect.TransferMemoryBudget{
				client.SendBufferSettings.ResendQueueBudget,
				client.ReceiveBufferSettings.ReceiveQueueBudget,
				client.ReceiveBufferSettings.PackQueueBudget,
				client.WebRtcSettings.MemoryBudget, selected,
				provider.SendBufferSettings.ResendQueueBudget,
				provider.ReceiveBufferSettings.ReceiveQueueBudget,
				provider.WebRtcSettings.MemoryBudget,
				provider.WebRtcSettings.NetworkPeerMemoryBudget,
				memory.nat,
			}
			var wg sync.WaitGroup
			var escaped atomic.Bool
			start := make(chan struct{})
			for _, leaf := range leaves {
				for range 4 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						for range 200 {
							const bytes = ByteCount(256 * 1024)
							if leaf.TryReserve(bytes) {
								stats := memory.root.Stats()
								if stats.UsedByteCount > stats.TotalByteCount || stats.UsedByteCount != stats.ReservedByteCount-stats.ReleasedByteCount {
									escaped.Store(true)
								}
								leaf.Release(bytes)
							}
						}
					}()
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range 200 {
					device.applyProvideMemorySharesWithLock(true)
					device.applyProvideMemorySharesWithLock(false)
				}
			}()
			close(start)
			wg.Wait()
			connect.AssertEqual(t, escaped.Load(), false)
			for _, budget := range append(leaves, memory.root, memory.client, memory.provider) {
				stats := budget.Stats()
				connect.AssertEqual(t, stats.UsedByteCount, ByteCount(0))
				connect.AssertEqual(t, stats.ReservedByteCount, stats.ReleasedByteCount)
			}
		})
	}
}

func TestDeviceLocalTransferHierarchyNatGenerationsAndTelemetry(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 20*1024*1024)
	memory := device.transferMemory
	settings := providerLocalUserNatSettings(memory.providerShare, connect.DefaultLogger())
	settings.MemoryBudget = memory.nat
	first, err := connect.TryNewLocalUserNat(context.Background(), "fallback", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.CloseAndWait(context.Background()) })
	second, err := connect.TryNewLocalUserNat(context.Background(), "remote", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.CloseAndWait(context.Background()) })
	natBytes := memory.nat.UsedByteCount()
	if natBytes <= 0 || natBytes > memory.nat.TotalByteCount() {
		t.Fatalf("both live NATs not charged within one child: %d", natBytes)
	}
	pack := device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget
	connect.AssertEqual(t, pack.TryReserve(1024), true)
	usage := device.MemoryUsed()
	connect.AssertEqual(t, usage.TransferRootBudgetByteCount, ByteCount(13*1024*1024))
	connect.AssertEqual(t, usage.TransferRootUsedByteCount, natBytes+1024)
	connect.AssertEqual(t, usage.NatUsedByteCount, natBytes)
	connect.AssertEqual(t, usage.PackQueueUsedByteCount, ByteCount(1024))
	connect.AssertEqual(t, usage.TotalByteCount, natBytes+1024)
	pack.Release(1024)
	device.applyProvideMemorySharesWithLock(true)
	device.applyProvideMemorySharesWithLock(false)
	connect.AssertEqual(t, memory.root.UsedByteCount(), natBytes)
	_ = first.CloseAndWait(context.Background())
	if memory.nat.UsedByteCount() <= 0 || memory.nat.UsedByteCount() >= natBytes {
		t.Fatal("one NAT teardown lost or retained its sibling's charge")
	}
	_ = second.CloseAndWait(context.Background())
	usage = device.MemoryUsed()
	connect.AssertEqual(t, usage.TransferRootUsedByteCount, ByteCount(0))
	connect.AssertEqual(t, usage.TransferRootReservedByteCount, usage.TransferRootReleasedByteCount)
	connect.AssertEqual(t, usage.NatReservedByteCount, usage.NatReleasedByteCount)
}

// The initial NAT retry has its own completion boundary for both successful
// admission and cancellation; cleanup alone joins the closed device lifecycle.
func TestDeviceLocalTransferHierarchyDeferredNatAdmissionAndCancel(t *testing.T) {
	for _, cancelBeforeRelease := range []bool{false, true} {
		device, _ := providerMemoryTestDevice(t, 20*1024*1024)
		memory := device.transferMemory
		held := memory.root.TotalByteCount()
		connect.AssertEqual(t, memory.client.TryReserve(held), true)
		defer func() { memory.client.Release(held) }()
		device.applyProvideMemorySharesWithLock(true)
		device.stateLock.Lock()
		device.ensureRemoteUserNatProviderWithLock()
		admissionDone := device.remoteUserNatProviderMemoryWaitDone
		refused := device.remoteUserNatProvider == nil && device.remoteUserNatProviderLocalUserNat == nil && admissionDone != nil
		device.stateLock.Unlock()
		if !refused {
			t.Fatal("refused NAT installed an owner or did not arrange retry")
		}
		connect.AssertEqual(t, memory.nat.UsedByteCount(), ByteCount(0))
		if cancelBeforeRelease {
			device.cancel()
		} else {
			// Wake on enough real capacity, not an enlarged nominal child.
			const constructorBytes = providerMemoryTestNatByteCount + providerMemoryTestProviderByteCount + providerMemoryTestStatsByteCount
			memory.client.Release(constructorBytes)
			held -= constructorBytes
		}
		waitProviderRotationBarrier(t, admissionDone, "NAT admission worker")
		device.stateLock.Lock()
		if device.remoteUserNatProviderMemoryWaitDone != nil {
			t.Error("admission worker left pending flag")
		}
		if cancelBeforeRelease {
			if device.remoteUserNatProvider != nil || device.remoteUserNatProviderLocalUserNat != nil {
				t.Error("canceled refusal constructed a NAT")
			}
		} else if device.remoteUserNatProvider == nil || device.remoteUserNatProviderLocalUserNat == nil || device.providerPacketStatsSub == nil {
			t.Error("sibling drain did not admit the complete provider graph")
		}
		device.closed = true
		device.provideMode = ProvideModeNone
		device.closeRemoteUserNatProviderWithLock()
		device.stateLock.Unlock()
		device.lifecycleWorkers.Wait()
		connect.AssertEqual(t, memory.nat.UsedByteCount(), ByteCount(0))
		connect.AssertEqual(t, memory.root.UsedByteCount(), held)
	}
}
