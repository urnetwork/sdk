// The optional consumer-head child shares the device lifetime root. It is
// permission rather than a standing reservation, and snapshots include its
// physical owners exactly once alongside ordinary receive and Pack ownership.
package sdk

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Both mobile profiles preserve logical head credit and independently funded
// physical RX backing under one root, including cloned clients and role changes.
func TestDeviceLocalConsumerHeadRecoveryProfilesAndGenerationOverlap(t *testing.T) {
	for _, row := range []struct {
		target, logicalOff, physicalOff, logicalOn, physicalOn ByteCount
	}{
		{target: 32 * 1024 * 1024, logicalOff: 2181038, physicalOff: 7633633, logicalOn: 2 * 1024 * 1024, physicalOn: 7 * 1024 * 1024},
		{target: 64 * 1024 * 1024, logicalOff: 4362076, physicalOff: 15267266, logicalOn: 4 * 1024 * 1024, physicalOn: 14 * 1024 * 1024},
	} {
		device, provider := transferMemoryTestDevice(t, row.target)
		memory := device.transferMemory
		receive := device.settings.ClientSettings.ReceiveBufferSettings
		recovery, rx := receive.ReceiveHeadRecoveryBudget, receive.ReceiveQueueBudget
		pack := receive.PackQueueBudget
		_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
		if recovery == nil || recovery != memory.headRecovery || recovery.Parent() != rx.Parent() || recovery.Parent() != memory.client ||
			recovery == rx || recovery.TotalByteCount() != row.logicalOff || rx.TotalByteCount() != row.physicalOff || memory.root.TotalByteCount() != clientShare+providerShare ||
			recovery.UsedByteCount() != 0 || memory.root.UsedByteCount() != 0 || provider.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != nil ||
			pack == nil || pack == rx || pack == recovery || pack.Parent() != memory.client || provider.ReceiveBufferSettings.PackQueueBudget != pack {
			t.Fatalf("%d profile did not preserve logical head credit and physical RX backing under the unchanged hierarchy", row.target)
		}
		cloneA := newDeviceClientSettings(&device.settings.ClientSettings, "", nil)
		cloneB := newDeviceClientSettings(&device.settings.ClientSettings, "", nil)
		if cloneA.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != recovery || cloneB.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != recovery {
			t.Fatal("client generations acquired independent recovery partitions")
		}
		beforeRoot, beforeRx, beforeHead := memory.root.TotalByteCount(), rx.TotalByteCount(), recovery.TotalByteCount()
		if !recovery.TryReserve(1024) {
			t.Fatal("head ownership setup")
		}
		device.applyProvideMemorySharesWithLock(true)
		if memory.root.TotalByteCount() != beforeRoot || recovery != receive.ReceiveHeadRecoveryBudget ||
			recovery.TotalByteCount() != row.logicalOn || rx.TotalByteCount() != row.physicalOn || recovery.UsedByteCount() != 1024 || memory.root.UsedByteCount() != 1024 {
			recovery.Release(1024)
			t.Fatal("role change replaced, lost or double-counted a live head owner")
		}
		recovery.Release(1024)
		device.applyProvideMemorySharesWithLock(false)
		if rx.TotalByteCount() != beforeRx || recovery.TotalByteCount() != beforeHead {
			t.Fatal("role round trip changed the ordinary or recovery ceiling")
		}
		competitor := connect.NewTransferMemoryBudgetWithParent(beforeRoot, memory.client)
		if !competitor.TryReserve(beforeRoot) {
			t.Fatal("root pressure setup")
		}
		accepted := recovery.TryReserve(1)
		if accepted {
			recovery.Release(1)
		}
		competitor.Release(beforeRoot)
		if accepted || memory.root.UsedByteCount() != 0 {
			t.Fatal("recovery partition bypassed the fixed global root")
		}
	}
}

// A separately read head counter may drain before the coherent hierarchy
// snapshot. Replace that sample instead of retaining or adding it twice.
func TestDeviceLocalConsumerHeadRecoverySnapshotReconcilesOwnership(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 32*1024*1024)
	memory := device.transferMemory
	recovery := device.settings.ClientSettings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget
	pack := device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget
	if !recovery.TryReserve(1024) {
		t.Fatal("snapshot ownership setup")
	}
	usage := device.MemoryUsed()
	if usage.ClientReceiveHeadRecoveryUsedByteCount != 1024 || usage.ClientReceiveByteCount != 1024 ||
		usage.TransferRootUsedByteCount != 1024 || usage.ClientTransferUsedByteCount != 1024 || usage.TotalByteCount != 1024 {
		recovery.Release(1024)
		t.Fatal("head owner is missing or counted twice in device usage")
	}
	diagnostic := transferDiagTransferBudget(usage, 1)
	if diagnostic.HeadRecoveryTotalByteCount != usage.ClientReceiveHeadRecoveryBudgetByteCount ||
		diagnostic.HeadRecoveryUsedByteCount != 1024 || diagnostic.HeadRecoveryReservedByteCount != 1024 ||
		diagnostic.HeadRecoveryReleasedByteCount != 0 || diagnostic.RootUsedByteCount != 1024 {
		recovery.Release(1024)
		t.Fatal("existing transfer diagnostic lost the attributed child snapshot")
	}
	encoded, err := json.Marshal(diagnostic)
	var fields map[string]json.RawMessage
	if err != nil || len(encoded) > 3000 || json.Unmarshal(encoded, &fields) != nil {
		recovery.Release(1024)
		t.Fatal("bounded transfer diagnostic could not serialize recovery counters")
	}
	for key, want := range map[string]int64{"consumer_head_recovery_total_bytes": usage.ClientReceiveHeadRecoveryBudgetByteCount,
		"consumer_head_recovery_used_bytes": 1024, "consumer_head_recovery_reserved_bytes": 1024, "consumer_head_recovery_released_bytes": 0} {
		var got int64
		if json.Unmarshal(fields[key], &got) != nil || got != want {
			recovery.Release(1024)
			t.Fatalf("transfer diagnostic omitted or changed %s", key)
		}
	}
	recovery.Release(1024)
	applyDeviceLocalTransferMemoryUsage(usage, memory, pack)
	if usage.ClientReceiveHeadRecoveryUsedByteCount != 0 || usage.ClientReceiveByteCount != 0 || usage.TransferRootUsedByteCount != 0 ||
		usage.ClientReceiveHeadRecoveryReservedByteCount != usage.ClientReceiveHeadRecoveryReleasedByteCount {
		t.Fatal("coherent snapshot retained a prior head sample")
	}
	if !recovery.TryReserve(2048) {
		t.Fatal("second snapshot ownership setup")
	}
	applyDeviceLocalTransferMemoryUsage(usage, memory, pack)
	if usage.ClientReceiveHeadRecoveryUsedByteCount != 2048 || usage.ClientReceiveByteCount != 2048 || usage.TransferRootUsedByteCount != 2048 {
		recovery.Release(2048)
		t.Fatal("coherent snapshot did not observe the new child owner once")
	}
	recovery.Release(2048)
	final := device.MemoryUsed()
	if final.TransferRootUsedByteCount != 0 || final.ClientReceiveByteCount != 0 || final.ClientReceiveHeadRecoveryUsedByteCount != 0 ||
		final.TransferRootReservedByteCount != final.TransferRootReleasedByteCount {
		t.Fatal("device retained consumer-head bytes after final return")
	}
	finalDiagnostic := transferDiagTransferBudget(final, 2)
	if finalDiagnostic.HeadRecoveryUsedByteCount != 0 || finalDiagnostic.HeadRecoveryReservedByteCount != 3072 ||
		finalDiagnostic.HeadRecoveryReleasedByteCount != 3072 || finalDiagnostic.RootUsedByteCount != 0 {
		t.Fatal("existing diagnostic lost the final child return")
	}
}

// The default destination factory starts from fresh Connect settings; cloning
// device settings alone does not exercise its budget forwarding. Real local
// destination generations must keep this one child and the SDK Pack topology.
// This fixture may attempt its configured loopback API, so it is not NoNetwork.
func TestDeviceLocalConsumerHeadRecoveryDefaultWindowGenerations(t *testing.T) {
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.KeepWindowClientKeysLocal = true
	device := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), settings, false)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	receive := device.settings.ReceiveBufferSettings
	recovery, rx, pack := receive.ReceiveHeadRecoveryBudget, receive.ReceiveQueueBudget, receive.PackQueueBudget
	if recovery == nil || recovery != device.transferMemory.headRecovery {
		t.Fatal("actual device did not configure its shared recovery child")
	}
	var previous *connect.ApiMultiClientGenerator
	for range 2 {
		device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
		device.stateLock.Lock()
		generator := device.apiMultiClientGenerator
		device.stateLock.Unlock()
		if generator == nil || generator == previous {
			t.Fatal("actual default destination factory did not replace its generation")
		}
		client := generator.NewClientSettings()
		if client.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != recovery || client.ReceiveBufferSettings.ReceiveQueueBudget != rx ||
			client.ReceiveBufferSettings.PackQueueBudget != pack || recovery.Parent() != device.transferMemory.client ||
			pack == rx || pack == recovery || pack.Parent() != recovery.Parent() || recovery.TotalByteCount() != 2181038 || rx.TotalByteCount() != 7633633 {
			t.Fatal("default window settings lost the shared consumer recovery or Pack partition")
		}
		previous = generator
		device.SetConnectLocation(nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := device.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	stats := recovery.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatal("joined default generations retained a recovery claim")
	}
	t.Log("[rx-consumer-default-window] generations=2 shared_child=true pack_sibling=true joined=true")
}
