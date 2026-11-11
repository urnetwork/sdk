// Device-owned provider recovery remains a bounded sibling through role flips.
package sdk

import (
	"context"
	"testing"
)

// Shrinking permission preserves already-admitted ownership and closing the
// branch waits for its true release. All ceilings and the root stay unchanged.
func TestDeviceLocalProviderHeadRecoveryRoleResizeKeepsOwner(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		captured := testMobileProviderCapturedDefaults(t, target)
		owner, err := newTestMobileProviderFixture(context.Background(), captured, "", nil, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		head := owner.clientSettings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget
		rx := owner.clientSettings.ReceiveBufferSettings.ReceiveQueueBudget
		parent, root := owner.memory.provider, owner.memory.root
		rootTotal, parentTotal := root.TotalByteCount(), parent.TotalByteCount()
		claim := 1024 * 1024
		claimed := false
		t.Cleanup(func() {
			if claimed {
				head.Release(ByteCount(claim))
			}
			if err := owner.closeAndCheck(context.Background()); err != nil {
				t.Error(err)
			}
		})
		if head == nil || head.Parent() != parent || head == rx || head.TotalByteCount() != rx.TotalByteCount() || !head.TryReserve(ByteCount(claim)) {
			t.Fatal("actual provider child was not a funded sibling of RX")
		}
		claimed = true
		owner.device.applyProvideMemorySharesWithLock(false)
		if head != owner.memory.providerHeadRecovery || head.TotalByteCount() != 384*1024 ||
			head.UsedByteCount() != ByteCount(claim) || head.TryReserve(1) || parent.TryReserve(1) {
			t.Fatal("PROVIDER_HEAD_ROLE_RESIZE_LOST: role shrink replaced, revoked or over-admitted a live provider child")
		}
		head.Release(ByteCount(claim))
		claimed = false
		owner.device.applyProvideMemorySharesWithLock(true)
		if head != owner.clientSettings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget || head.TotalByteCount() != rx.TotalByteCount() ||
			root.TotalByteCount() != rootTotal || parent.TotalByteCount() != parentTotal || !head.TryReserve(ByteCount(claim)) {
			t.Fatal("provider role restoration changed the stable child/ancestors or lost capacity")
		}
		claimed = true
		head.Release(ByteCount(claim))
		claimed = false
		if err := owner.closeAndCheck(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// The shared-root memory snapshot includes the provider child exactly once,
// even though provider receive and parent/root usages all contain that owner.
func TestDeviceLocalProviderHeadRecoveryUsageIncludesChildOnce(t *testing.T) {
	captured := testMobileProviderCapturedDefaults(t, 64*1024*1024)
	owner, err := newTestMobileProviderFixture(context.Background(), captured, "", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	head := owner.clientSettings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget
	claimed := false
	t.Cleanup(func() {
		if claimed {
			head.Release(5848)
		}
		if err := owner.closeAndCheck(context.Background()); err != nil {
			t.Error(err)
		}
	})
	before := owner.snapshot()
	if !head.TryReserve(5848) {
		t.Fatal("funded real provider child did not admit the measured head cost")
	}
	claimed = true
	usage := DeviceLocalMemoryUsage{}
	applyDeviceLocalTransferMemoryUsage(&usage, owner.memory, owner.clientSettings.ReceiveBufferSettings.PackQueueBudget)
	if usage.ProviderReceiveHeadRecoveryUsedByteCount != 5848 || usage.ProviderReceiveByteCount != 5848 ||
		usage.ProviderTransferUsedByteCount-before.provider.UsedByteCount != 5848 ||
		usage.TransferRootUsedByteCount-before.root.UsedByteCount != 5848 || usage.NatUsedByteCount != before.nat.UsedByteCount {
		t.Fatal("PROVIDER_HEAD_USAGE_OMITTED: provider recovery usage omitted, duplicated or attributed the child to NAT")
	}
	// Applying a coherent read twice to the same value must not double it.
	applyDeviceLocalTransferMemoryUsage(&usage, owner.memory, owner.clientSettings.ReceiveBufferSettings.PackQueueBudget)
	if usage.ProviderReceiveByteCount != 5848 {
		t.Fatal("repeated usage projection double-counted the provider child")
	}
	head.Release(5848)
	claimed = false
	if err := owner.closeAndCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}
