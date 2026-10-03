package sdk

import (
	"runtime"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

func TestDeviceLocalTransferMemorySnapshotReplacesEarlierPackSample(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 20*1024*1024)
	pack := device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget
	connect.AssertEqual(t, pack.TryReserve(1024), true)
	usage := &DeviceLocalMemoryUsage{
		PackQueueUsedByteCount:     1024,
		PackQueueCapacityByteCount: 1,
		ClientReceiveByteCount:     2048,
	}
	// This is the exact interleaving that used to report Pack > root: Pack
	// was observed alive, then drained before the coherent root/group read.
	pack.Release(1024)
	applyDeviceLocalTransferMemoryUsage(usage, device.transferMemory, pack)
	connect.AssertEqual(t, usage.PackQueueUsedByteCount, ByteCount(0))
	connect.AssertEqual(t, usage.PackQueueCapacityByteCount, pack.TotalByteCount())
	connect.AssertEqual(t, usage.ClientReceiveByteCount, ByteCount(1024))
	connect.AssertEqual(t, usage.TransferRootUsedByteCount, ByteCount(0))
	connect.AssertEqual(t, usage.TransferRootReservedByteCount, usage.TransferRootReleasedByteCount)
	connect.AssertEqual(t, pack.TryReserve(2048), true)
	defer pack.Release(2048)
	applyDeviceLocalTransferMemoryUsage(usage, device.transferMemory, pack)
	connect.AssertEqual(t, usage.PackQueueUsedByteCount, ByteCount(2048))
	connect.AssertEqual(t, usage.ClientReceiveByteCount, ByteCount(3072))
	connect.AssertEqual(t, usage.ClientTransferUsedByteCount, usage.PackQueueUsedByteCount)
	connect.AssertEqual(t, usage.TransferRootUsedByteCount, usage.PackQueueUsedByteCount)
}

func TestDeviceLocalTransferMemorySnapshotRejectsUnrelatedPack(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 20*1024*1024)
	defer func() {
		if recover() == nil {
			t.Fatal("unrelated Pack budget was presented as a transfer-root subset")
		}
	}()
	applyDeviceLocalTransferMemoryUsage(&DeviceLocalMemoryUsage{}, device.transferMemory, connect.NewTransferMemoryBudget(1024))
}

func TestDeviceLocalTransferMemorySnapshotConcurrentPackDrain(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 20*1024*1024)
	pack := device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if pack.TryReserve(1024) {
					runtime.Gosched()
					pack.Release(1024)
				}
			}
		}()
	}
	defer func() {
		close(stop)
		workers.Wait()
		usage := device.MemoryUsed()
		if usage.TransferRootUsedByteCount != 0 || usage.PackQueueUsedByteCount != 0 ||
			usage.TransferRootReservedByteCount != usage.TransferRootReleasedByteCount {
			t.Errorf("concurrent Pack workers did not return every root charge: %+v", usage)
		}
	}()
	for range 3000 {
		usage := device.MemoryUsed()
		if usage.PackQueueUsedByteCount != usage.TransferRootUsedByteCount ||
			usage.PackQueueUsedByteCount != usage.ClientTransferUsedByteCount ||
			usage.TransferRootUsedByteCount != usage.TransferRootReservedByteCount-usage.TransferRootReleasedByteCount ||
			usage.PackQueueUsedByteCount > usage.PackQueueCapacityByteCount ||
			usage.ClientReceiveByteCount != usage.PackQueueUsedByteCount ||
			usage.TotalByteCount != usage.TransferRootUsedByteCount {
			t.Fatalf("Pack, its client group and its root were not one coherent sample: %+v", usage)
		}
	}
}
