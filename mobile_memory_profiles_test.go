package sdk

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
)

// Exercise the real constructor defaults: a synthetic all-large settings
// object would miss a default that still throttles the larger mobile target.
func TestMobileProfilesSpendTargetInSharedOwners(t *testing.T) {
	previous := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previous) })
	for _, mib := range []ByteCount{32, 64} {
		t.Run(fmt.Sprint(mib), func(t *testing.T) {
			target := mib << 20
			connect.SetMemoryBudget(target)
			device, _ := transferMemoryTestDevice(t, target)
			dns, client, carrier, provider := deviceMemoryShares(device.settings)
			if dns != target/10 || carrier != target/4 || client != target*9/20 || provider != target/5 {
				t.Fatalf("wrong profile split: dns=%d client=%d carrier=%d provider=%d", dns, client, carrier, provider)
			}
			root := device.transferMemory.root
			if total := dns + root.TotalByteCount() + carrier; total > target || target-total > 3 {
				t.Fatalf("admission leaves unallocated capacity: total=%d target=%d", total, target)
			}
			if !device.dnsMemoryTarget.TryReserve(dns) || !device.transferMemory.client.TryReserve(root.TotalByteCount()) {
				t.Fatal("full DNS/transfer allocation was refused")
			}
			if device.dnsMemoryTarget.TryReserve(1) || device.transferMemory.client.TryReserve(1) || device.transferMemory.nat.TryReserve(1) {
				t.Fatal("shared admission overran its target")
			}
			usage := device.MemoryUsed()
			if usage.TotalByteCount != dns+root.TotalByteCount() || usage.TransferRootUsedByteCount != root.TotalByteCount() {
				t.Fatalf("overlapping owner telemetry counted bytes twice: %+v", usage)
			}
			device.transferMemory.client.Release(root.TotalByteCount())
			device.dnsMemoryTarget.Release(dns)
			if got := device.MemoryUsed().TotalByteCount; got != 0 {
				t.Fatalf("ownership survived release: %d", got)
			}
			for _, active := range []bool{false, true} {
				device.applyProvideMemorySharesWithLock(active)
				r := device.settings.ReceiveBufferSettings
				share := client + provider
				if active {
					share = client
				}
				want := max(target/16, share/10) // target-scaled floor or available client share
				if r.ReceiveQueueBudget.TotalByteCount() != want || r.PackQueueBudget.TotalByteCount() != want {
					t.Fatalf("active=%t target=%d receive/Pack did not scale: %d/%d, want %d", active, target,
						r.ReceiveQueueBudget.TotalByteCount(), r.PackQueueBudget.TotalByteCount(), want)
				}
				if r.ReceiveQueueMaxByteCount < want {
					t.Fatalf("sequence advertisement undercuts shared receive budget: %d < %d", r.ReceiveQueueMaxByteCount, want)
				}
			}
			packet, large := messagePoolMemoryTargetsForPlatform(target, true)
			if packet != 256<<10 || large != 512<<10 {
				t.Fatalf("larger target retained idle burst memory: packet=%d large=%d", packet, large)
			}
			t.Logf("target=%d DNS=%d transfer=%d carrier=%d returned_pool=%d", target, dns, root.TotalByteCount(), carrier, packet+large)
		})
	}
}

func TestMobileProfilesKeepComposedCarrierAccountingAtLargerTargets(t *testing.T) {
	previous := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previous) })
	for _, mib := range []ByteCount{32, 64, 128} {
		t.Run(fmt.Sprint(mib), func(t *testing.T) {
			target := mib << 20
			connect.SetMemoryBudget(target)
			settings := connect.DefaultPlatformTransportSettingsWithMemoryTarget(target)
			window := settings.H3MaxConnectionReceiveWindowByteCount
			applyMobileLowMemoryPlatformTransportSettingsForPlatform(settings, target, true)
			if settings.H3BudgetByteCount < settings.H3MaxConnectionReceiveWindowByteCount+1600<<10 {
				t.Fatalf("mobile target %d lost composed H3 accounting: claim=%d window=%d", target,
					settings.H3BudgetByteCount, settings.H3MaxConnectionReceiveWindowByteCount)
			}
			if settings.H3MaxConnectionReceiveWindowByteCount != window {
				t.Fatal("mobile retained accounting shrank the target-scaled receive window")
			}
			if settings.H3SocketReadBufferByteCount != 64<<10 || settings.H3SocketWriteBufferByteCount != 64<<10 ||
				settings.H3InitialStreamReceiveWindowByteCount != 128<<10 || settings.H3InitialConnectionReceiveWindowByteCount != 256<<10 {
				t.Fatal("mobile speculative-dial ownership escaped its accounting")
			}
		})
	}
}
