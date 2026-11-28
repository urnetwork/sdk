// Real DeviceLocal close and existing accounting getter establish the Android
// publisher's sample boundary. No SDK production API or close policy changes.
package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A held lifecycle worker proves cancellation alone is not a zero-ownership
// boundary. Its actual receive/Pack reservations drain before the joined read.
func TestDeviceMemoryUsedAfterActualCloseWaitReportsReturnedCredits(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		fixture := testingAuthClientShapeSpace(t)
		device := newPeerPinTestDevice(t, fixture, peerPinDeviceSettings(target), "")
		receive := device.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
		pack := device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget
		if receive == nil || pack == nil || device.transferMemory == nil {
			t.Fatal("real mobile constructor did not install accounting")
		}
		if !receive.TryReserve(1024) {
			t.Fatal("synthetic receive reservation refused")
		}
		var receiveOnce sync.Once
		releaseReceive := func() { receiveOnce.Do(func() { receive.Release(1024) }) }
		t.Cleanup(releaseReceive)
		if !pack.TryReserve(2048) {
			t.Fatal("synthetic Pack reservation refused")
		}
		var packOnce sync.Once
		releasePack := func() { packOnce.Do(func() { pack.Release(2048) }) }
		t.Cleanup(releasePack)
		active := device.MemoryUsed()
		_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
		if active.TargetByteCount != target || active.TransferRootBudgetByteCount != clientShare+providerShare ||
			active.TransferRootUsedByteCount < 3072 || active.ClientReceiveByteCount < 3072 || active.PackQueueUsedByteCount != 2048 {
			t.Fatalf("actual constructor/getter lost live ownership for %d: %+v", target, active)
		}
		releaseWorker := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(releaseWorker) }) }
		t.Cleanup(unblock)
		device.lifecycleWorkers.Add(1)
		go func() {
			defer device.lifecycleWorkers.Done()
			<-releaseWorker
			releaseReceive()
			releasePack()
		}()
		device.Close()
		select {
		case <-device.ctx.Done():
		default:
			t.Fatal("actual Close did not cancel")
		}
		select {
		case <-device.lifecycleDone:
			t.Fatal("actual lifecycle joined with its receive owner deliberately held")
		default:
		}
		if held := device.MemoryUsed(); held.TransferRootUsedByteCount < 3072 || held.ClientReceiveByteCount < 3072 {
			t.Fatal("Close request fabricated released ownership before actual worker join")
		}
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := device.CloseAndWait(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		usage := device.MemoryUsed()
		if usage.TargetByteCount != target || usage.TransferRootBudgetByteCount != clientShare+providerShare {
			t.Fatal("closed getter lost its actual configured target/root")
		}
		for name, used := range map[string]int64{
			"root":             usage.TransferRootUsedByteCount,
			"receive-and-pack": usage.ClientReceiveByteCount, "pack": usage.PackQueueUsedByteCount,
			"client": usage.ClientTransferUsedByteCount, "provider": usage.ProviderTransferUsedByteCount,
			"nat": usage.NatUsedByteCount, "peer-pins": usage.PeerKeyPinUsedByteCount,
			"platform": usage.PlatformTransportUsedByteCount, "platform-count": int64(usage.PlatformTransportUsedCount),
			"pending-h1": int64(usage.PlatformTransportPendingH1Count), "pending-h1-bytes": usage.PlatformTransportPendingH1Bytes,
			"pending-handoff": int64(usage.PlatformTransportPendingHandoffCount),
			"active-handoff":  int64(usage.PlatformTransportActiveHandoffCount),
		} {
			if used != 0 {
				t.Fatalf("actual joined %s ownership remained: %d", name, used)
			}
		}
		for _, counts := range [][2]int64{
			{usage.TransferRootReservedByteCount, usage.TransferRootReleasedByteCount},
			{usage.NatReservedByteCount, usage.NatReleasedByteCount},
			{usage.PeerKeyPinReservedByteCount, usage.PeerKeyPinReleasedByteCount},
			{usage.PlatformTransportReservedBytes, usage.PlatformTransportReleasedBytes},
		} {
			if counts[0] != counts[1] {
				t.Fatalf("actual joined credits are unbalanced: %v", counts)
			}
		}
	}
}

// An externally retained real claim is not erased merely because lifecycleDone
// closed. This negative prevents the publisher/reader tests trusting fake zeros.
func TestDeviceMemoryUsedAfterActualCloseWaitRetainsOutstandingCredit(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		fixture := testingAuthClientShapeSpace(t)
		device := newPeerPinTestDevice(t, fixture, peerPinDeviceSettings(target), "")
		receive := device.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
		if receive == nil || !receive.TryReserve(1024) {
			t.Fatal("real receive budget unavailable")
		}
		var once sync.Once
		release := func() { once.Do(func() { receive.Release(1024) }) }
		t.Cleanup(release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := device.CloseAndWait(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		held := device.MemoryUsed()
		if held.TargetByteCount != target || held.TransferRootUsedByteCount != 1024 ||
			held.ClientReceiveByteCount != 1024 || held.TransferRootReservedByteCount-held.TransferRootReleasedByteCount != 1024 {
			t.Fatalf("actual post-join getter hid retained credit: %+v", held)
		}
		release()
		drained := device.MemoryUsed()
		if drained.TransferRootUsedByteCount != 0 || drained.ClientReceiveByteCount != 0 ||
			drained.TransferRootReservedByteCount != drained.TransferRootReleasedByteCount {
			t.Fatalf("actual released post-join getter did not drain: %+v", drained)
		}
	}
}
