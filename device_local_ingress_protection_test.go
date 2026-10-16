// Exercises the actual SDK ingress partition and full-root fixture ownership.
package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// A synthetic full root contains actual protected ingress owners as well as
// ordinary traffic. Release drains ordinary ownership first, like a finished send.
type transferMemoryTestClaim struct {
	budgets [3]*connect.TransferMemoryBudget
	bytes   [3]ByteCount
}

// Shares the factory's real ingress leaves; no test-only budget bypass is used.
func newTransferMemoryTestClaim(device *DeviceLocal, ordinary *connect.TransferMemoryBudget) *transferMemoryTestClaim {
	return &transferMemoryTestClaim{budgets: [3]*connect.TransferMemoryBudget{
		device.settings.ReceiveBufferSettings.PackQueueBudget,
		device.settings.ReceiveBufferSettings.ReceiveQueueBudget,
		ordinary,
	}}
}

// Admits the requested aggregate or restores every partial claim on refusal.
func (self *transferMemoryTestClaim) TryReserve(bytes ByteCount) bool {
	remaining := bytes
	var added [3]ByteCount
	for index, budget := range self.budgets {
		count := min(remaining, budget.Available())
		if count > 0 && budget.TryReserve(count) {
			added[index] = count
			remaining -= count
		}
	}
	if remaining != 0 {
		for index, count := range added {
			if count > 0 {
				self.budgets[index].Release(count)
			}
		}
		return false
	}
	for index, count := range added {
		self.bytes[index] += count
	}
	return true
}

// Returns only this fixture's admitted claims, including across role changes.
func (self *transferMemoryTestClaim) Release(bytes ByteCount) {
	for index := len(self.budgets) - 1; index >= 0; index-- {
		count := min(bytes, self.bytes[index])
		if count > 0 {
			self.budgets[index].Release(count)
			self.bytes[index] -= count
			bytes -= count
		}
	}
	if bytes != 0 {
		panic("unowned synthetic transfer memory release")
	}
}

// Factory-created siblings cannot consume either protected window at 32/64 MiB.
func TestDeviceLocalIngressProtectionActualFactory(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		for _, active := range []bool{false, true} {
			device, _ := transferMemoryTestDevice(t, target)
			device.applyProvideMemorySharesWithLock(active)
			memory := device.transferMemory
			pack := device.settings.ReceiveBufferSettings.PackQueueBudget
			reorder := device.settings.ReceiveBufferSettings.ReceiveQueueBudget
			protected := pack.TotalByteCount() + reorder.TotalByteCount()
			ordinary := memory.root.TotalByteCount() - protected
			// A direct root sibling models all unrelated provider/NAT ownership.
			sibling := connect.NewTransferMemoryBudgetWithParent(memory.root.TotalByteCount(), memory.root)
			if !sibling.TryReserve(ordinary) || sibling.TryReserve(1) || memory.root.UsedByteCount() != ordinary {
				t.Fatalf("target=%d active=%t: ordinary sibling stole ingress permission", target, active)
			}
			if !pack.TryReserve(pack.TotalByteCount()) || !reorder.TryReserve(reorder.TotalByteCount()) || memory.root.UsedByteCount() != memory.root.TotalByteCount() {
				t.Fatalf("target=%d active=%t: incoming windows could not fill at the root boundary", target, active)
			}
			pack.Release(pack.TotalByteCount())
			reorder.Release(reorder.TotalByteCount())
			sibling.Release(ordinary)
			stats := memory.root.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatal("factory protected ownership did not reconcile")
			}
		}
	}
}
