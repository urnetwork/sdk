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

// A real pin owner and ordinary SCTP claims can survive the provider-on role
// switch. Its incoming ACK must fit before any old resend ownership can drain.
func TestDeviceLocalIngressProtectionRoleShrinkProgress(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		device, _ := transferMemoryTestDevice(t, target)
		memory := device.transferMemory
		pack := device.settings.ReceiveBufferSettings.PackQueueBudget
		reorder := device.settings.ReceiveBufferSettings.ReceiveQueueBudget
		resend := device.settings.SendBufferSettings.ResendQueueBudget
		p2p := device.settings.WebRtcSettings.MemoryBudget
		store, err := newBoundedPeerClientKeyPinStore(&LocalState{localStorageDir: t.TempDir()}, nil, memory.peerKeyPins)
		if err != nil {
			t.Fatal(err)
		}
		owned := map[*connect.TransferMemoryBudget]ByteCount{}
		reserve := func(budget *connect.TransferMemoryBudget, bytes ByteCount) bool {
			if !budget.TryReserve(bytes) {
				return false
			}
			owned[budget] += bytes
			return true
		}
		release := func(budget *connect.TransferMemoryBudget, bytes ByteCount) {
			if bytes < 0 || owned[budget] < bytes {
				t.Fatal("fixture released ownership it did not admit")
			}
			owned[budget] -= bytes
			budget.Release(bytes)
		}
		t.Cleanup(func() {
			for budget, bytes := range owned {
				if bytes > 0 {
					budget.Release(bytes)
				}
			}
			store.Close()
			stats := memory.root.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Error("role-shrink fixture cleanup did not reconcile actual owners")
			}
		})
		resendBytes := resend.TotalByteCount()
		_, clientShare, _, _ := deviceMemoryShares(device.settings)
		onProtected := mobilePackQueueBudgetByteCountForTarget(clientShare, target) + mobileReceiveQueueBudgetForPlatform(target, clientShare, true)
		onOrdinary := clientShare - onProtected
		p2pBytes := ((onOrdinary-resendBytes-peerPinStoreMemoryByteCount)/deviceLocalP2pReceiveBufferByteCount + 1) * deviceLocalP2pReceiveBufferByteCount
		if p2pBytes <= 0 || !reserve(resend, resendBytes) || !reserve(p2p, p2pBytes) {
			t.Fatalf("target=%d: ordinary pre-switch owners did not fit", target)
		}
		oldOrdinary := memory.client.UsedByteCount()
		rootTotal := memory.root.TotalByteCount()
		device.applyProvideMemorySharesWithLock(true)
		if oldOrdinary <= onOrdinary || memory.client.UsedByteCount() != oldOrdinary || memory.root.TotalByteCount() != rootTotal || reserve(resend, 1) {
			t.Fatalf("target=%d: fixture missed retained ordinary overhang or changed the root", target)
		}
		// Fill every ordinary byte at the shared root; ingress cannot rely on
		// accidental spare provider/NAT capacity or on retiring a healthy peer.
		sibling := connect.NewTransferMemoryBudgetWithParent(rootTotal, memory.root)
		siblingBytes := rootTotal - oldOrdinary - onProtected
		if !reserve(sibling, siblingBytes) || reserve(sibling, 1) {
			t.Fatalf("target=%d: root sibling stole the promised ingress window", target)
		}
		packBytes, reorderBytes := pack.TotalByteCount(), reorder.TotalByteCount()
		if pack.Available() != packBytes || reorder.Available() != reorderBytes || !reserve(pack, packBytes) || !reserve(reorder, reorderBytes) {
			t.Fatalf("target=%d: role shrink blocked ACK admission behind its own retained sender", target)
		}
		if memory.root.UsedByteCount() != rootTotal || memory.client.UsedByteCount() != oldOrdinary+onProtected {
			t.Fatal("draining role duplicated or lost ancestor ownership")
		}
		// Only after ingress succeeds may the acknowledged send owner retire.
		release(resend, resendBytes)
		release(pack, packBytes)
		release(reorder, reorderBytes)
		release(p2p, p2pBytes)
		release(sibling, siblingBytes)
		store.Close()
		stats := memory.root.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatal("role-shrink owner release did not reconcile")
		}
	}
}
