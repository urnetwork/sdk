package sdk

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// This is an experiment, not a mobile policy change. Resize the existing
// aggregate receive leaf and its advertised ceiling together, without adding
// a new pool, per-flow allowance, standing reserve, or process soft limit.
func h1ReceiveBudgetExperiment(t *testing.T, target, receiveBytes ByteCount) *DeviceLocal {
	t.Helper()
	device, _ := transferMemoryTestDevice(t, target)
	settings := &device.settings.ClientSettings
	applyMobileH1PerformanceClientSettingsForPlatform(settings, target, true, true)
	if receiveBytes > 0 {
		settings.ReceiveBufferSettings.ReceiveQueueBudget.SetTotalByteCount(receiveBytes)
		settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = max(
			settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount, receiveBytes,
		)
	}
	return device
}

func TestH1ReceiveBudgetExperimentSettings(t *testing.T) {
	previousBudget, previousSizing := connect.MemoryBudget(), connect.DefaultWindowSizing()
	t.Cleanup(func() {
		connect.SetMemoryBudget(previousBudget)
		connect.SetWindowSizing(previousSizing)
	})
	connect.SetMemoryBudget(32 * 1024 * 1024)
	connect.SetWindowSizing(connect.WindowSizingFromDelivery)
	for _, arm := range []struct {
		name                   string
		target, override, want ByteCount
	}{
		{"ios-20-control", 20 << 20, 0, 1536 << 10},
		{"ios-24-control", 24 << 20, 0, 1635778},
		{"ios-24-receive-2", 24 << 20, 2 << 20, 2 << 20},
		{"ios-24-receive-2.5", 24 << 20, 2560 << 10, 2560 << 10},
	} {
		t.Run(arm.name, func(t *testing.T) {
			device := h1ReceiveBudgetExperiment(t, arm.target, arm.override)
			settings, memory := &device.settings.ClientSettings, device.transferMemory
			send, receive := settings.SendBufferSettings, settings.ReceiveBufferSettings
			_, client, _, provider := deviceMemoryShares(device.settings)
			if receive.ReceiveQueueBudget.TotalByteCount() != arm.want ||
				min(receive.ReceiveQueueMaxByteCount, receive.ReceiveQueueBudget.TotalByteCount()) != arm.want {
				t.Fatal("receive permission did not follow the one aggregate experimental leaf")
			}
			if memory.root.TotalByteCount() != client+provider || memory.client.TotalByteCount() != client+provider ||
				receive.ReceiveQueueBudget.Parent() != memory.client {
				t.Fatal("receive experiment changed or escaped the shared hierarchy")
			}
			if receive.PackQueueBudget.TotalByteCount() != mobilePackQueueBudgetByteCountForTarget(client+provider, arm.target) ||
				receive.H1SequenceBufferSize != 64 || receive.H1SequenceBufferByteCount != 128<<10 ||
				send.LogicalDataLaneCount != 8 || send.ResendQueueMaxByteCount != 512<<10 ||
				!send.ResendQueueRetainedByteAccounting || !receive.ReceiveQueueRetainedByteAccounting {
				t.Fatal("receive experiment changed an adjacent mobile ownership limit")
			}
			data, err := json.Marshal(map[string]any{
				"arm": arm.name, "scope": "constructor-and-admission-only-not-runtime-memory",
				"device_target_bytes": arm.target, "process_sizing_bytes": connect.MemoryBudget(),
				"transfer_root_bytes": memory.root.TotalByteCount(), "send_pool_bytes": send.ResendQueueBudget.TotalByteCount(),
				"receive_pool_bytes": arm.want, "advertised_receive_bytes": arm.want,
				"pack_pool_bytes": receive.PackQueueBudget.TotalByteCount(), "send_opening_bytes": send.ResendQueueMaxByteCount,
				"h1_handoff_count": receive.H1SequenceBufferSize, "h1_handoff_bytes": receive.H1SequenceBufferByteCount,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("H1_RECEIVE_BUDGET_SETTINGS %s", data)
		})
	}
}

func TestH1ReceiveBudgetExperimentRootAndRoleBounds(t *testing.T) {
	for _, capacity := range []ByteCount{2 << 20, 2560 << 10} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			device := h1ReceiveBudgetExperiment(t, 24<<20, capacity)
			memory := device.transferMemory
			leaf := device.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
			rootTotal := memory.root.TotalByteCount()
			sibling := connect.NewTransferMemoryBudgetWithParent(rootTotal, memory.client)
			if !leaf.TryReserve(capacity) || leaf.TryReserve(1) || !sibling.TryReserve(rootTotal-capacity) || sibling.TryReserve(1) {
				t.Fatal("experimental leaf or shared parent did not enforce exact admission")
			}
			// Existing owners may drain across the role change, but the normal
			// provider-on resize must revoke new use of the experimental spend.
			device.applyProvideMemorySharesWithLock(true)
			if leaf.TotalByteCount() != 1536<<10 || leaf.UsedByteCount() != capacity || leaf.TryReserve(1) ||
				memory.provider.TryReserve(1) || memory.root.TotalByteCount() != rootTotal {
				t.Fatal("role change escaped ownership or retained experimental admission")
			}
			leaf.Release(capacity)
			sibling.Release(rootTotal - capacity)
			if memory.root.UsedByteCount() != 0 || memory.client.UsedByteCount() != 0 || leaf.UsedByteCount() != 0 {
				t.Fatal("experimental ownership did not drain")
			}
			device.applyProvideMemorySharesWithLock(false)
			if leaf.TotalByteCount() != 1635778 {
				t.Fatal("test-only spend leaked into the next normal provider-off state")
			}
		})
	}
}
