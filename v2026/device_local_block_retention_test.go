//go:build !ios

package sdk

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	"github.com/urnetwork/connect/v2026"
)

// Exercise the production history/conversion path without a network, SDK
// worker tree, UI bridge or changing a security decision. Time advances inside
// a synctest bubble, not by relaxing the production five-minute window.
func newBlockRetentionDevice() *DeviceLocal {
	return &DeviceLocal{
		settings:                         DefaultDeviceLocalSettings(),
		blockActionWindowChangeListeners: connect.NewCallbackList[BlockActionWindowChangeListener](),
	}
}

func blockRetentionInput(count, ipCount int, now time.Time) []*connect.BlockAction {
	rows := make([]*connect.BlockAction, count)
	for i := range rows {
		ips := make([]netip.Addr, ipCount)
		for j := range ips {
			ips[j] = netip.AddrFrom4([4]byte{198, 18, byte(i), byte(j + 1)})
		}
		rows[i] = &connect.BlockAction{
			Time: now, Ips: ips, Hosts: []string{fmt.Sprintf("owner-%04d.example.test", i)},
			Block: i%2 == 0, Local: i%3 == 0, PacketCount: i + 1, ByteCount: connect.ByteCount((i + 1) * 1200),
		}
	}
	return rows
}

func weakBlockRetentionRows(device *DeviceLocal) []weak.Pointer[BlockAction] {
	out := make([]weak.Pointer[BlockAction], len(device.blockActions))
	for i, row := range device.blockActions {
		out[i] = weak.Make(row)
	}
	return out
}

func collectBlockRetentionRows() { runtime.GC(); runtime.GC() }

func liveBlockRetentionRows(rows []weak.Pointer[BlockAction]) int {
	count := 0
	for _, row := range rows {
		if row.Value() != nil {
			count++
		}
	}
	return count
}

func TestDeviceLocalBlockActionHistoryReleasesExpiredRows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device := newBlockRetentionDevice()
		device.updateBlockActions(blockRetentionInput(1024, 16, time.Now()))
		old := weakBlockRetentionRows(device)
		time.Sleep(120 * time.Second)
		device.updateBlockActions(blockRetentionInput(35, 16, time.Now()))
		if len(device.blockActions) != 1024 {
			t.Fatalf("history cap changed: %d", len(device.blockActions))
		}
		// The old epoch now expires, while all newer rows remain eligible.
		time.Sleep(181 * time.Second)
		window := device.GetBlockActions()
		if window.BlockActions.Len() != 35 || len(device.blockActions) != 35 || cap(device.blockActions) > 70 {
			t.Fatalf("history did not shrink: visible=%d owned=%d slots=%d", window.BlockActions.Len(), len(device.blockActions), cap(device.blockActions))
		}
		for i, row := range window.BlockActions.values {
			if row.Block != (i%2 == 0) || row.Local != (i%3 == 0) || row.PacketCount != i+1 || row.ByteCount != ByteCount((i+1)*1200) {
				t.Fatal("history eviction changed retained decision/stats semantics")
			}
		}
		collectBlockRetentionRows()
		if live := liveBlockRetentionRows(old); live != 0 {
			t.Fatalf("expired/dropped rows remain rooted: %d", live)
		}
		runtime.KeepAlive(window)
		time.Sleep(301 * time.Second)
		if got := device.GetBlockActions().BlockActions.Len(); got != 0 || len(device.blockActions) != 0 || cap(device.blockActions) != 0 {
			t.Fatalf("empty history retained backing slots: visible=%d owned=%d slots=%d", got, len(device.blockActions), cap(device.blockActions))
		}
		runtime.KeepAlive(device)
	})
}

func TestDeviceLocalBlockActionSnapshotOwnsRowsUntilReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device := newBlockRetentionDevice()
		device.updateBlockActions(blockRetentionInput(64, 8, time.Now()))
		old := weakBlockRetentionRows(device)
		func() {
			// Public snapshots are independent lists, with intentionally shared
			// event rows. The producer must not clear a held snapshot.
			window := device.GetBlockActions()
			time.Sleep(301 * time.Second)
			if device.GetBlockActions().BlockActions.Len() != 0 {
				t.Fatal("device did not expire history")
			}
			collectBlockRetentionRows()
			if live := liveBlockRetentionRows(old); live != 64 || window.BlockActions.Len() != 64 {
				t.Fatalf("held consumer snapshot lost rows: %d", live)
			}
			runtime.KeepAlive(window)
		}()
		collectBlockRetentionRows()
		if live := liveBlockRetentionRows(old); live != 0 {
			t.Fatalf("released consumer snapshot retained rows: %d", live)
		}
		runtime.KeepAlive(device)
	})
}

// A fresh subprocess gives each release experiment a separate allocator state.
// It reports primitive aggregates only; no endpoints or heap profile contents.
// Forced collection/scavenging distinguishes reachability from span retention,
// and is diagnostic/test-only, never a proposed production memory policy.
func TestDeviceLocalBlockActionRetentionMeasurement(t *testing.T) {
	if os.Getenv("URNETWORK_BLOCK_RETENTION_MEASURE_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestDeviceLocalBlockActionRetentionMeasurement$", "-test.v")
		command.Env = append(os.Environ(), "URNETWORK_BLOCK_RETENTION_MEASURE_CHILD=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated retention measurement: %v\n%s", err, output)
		}
		t.Logf("isolated primitive measurements:\n%s", output)
		return
	}
	synctest.Test(t, func(t *testing.T) {
		device := newBlockRetentionDevice()
		measure := func(phase string, scavenge bool) {
			collectBlockRetentionRows()
			if scavenge {
				debug.FreeOSMemory()
			}
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			t.Logf("block-retention phase=%s rows=%d slots=%d heap=%d inuse=%d slack=%d free=%d runtime=%d", phase,
				len(device.blockActions), cap(device.blockActions), stats.HeapAlloc, stats.HeapInuse,
				stats.HeapInuse-stats.HeapAlloc, stats.HeapIdle-stats.HeapReleased, stats.Sys-stats.HeapReleased)
		}
		measure("empty", true)
		device.updateBlockActions(blockRetentionInput(1024, 16, time.Now()))
		measure("full-1024", false)
		time.Sleep(120 * time.Second)
		device.updateBlockActions(blockRetentionInput(35, 16, time.Now()))
		time.Sleep(181 * time.Second)
		_ = device.GetBlockActions()
		measure("expired-to-35", false)
		measure("expired-to-35-scavenged", true)
		time.Sleep(301 * time.Second)
		_ = device.GetBlockActions()
		measure("expired-all", false)
		measure("expired-all-scavenged", true)
		runtime.KeepAlive(device)
	})
}

func BenchmarkDeviceLocalBlockActionHistoryEpoch(b *testing.B) {
	device := newBlockRetentionDevice()
	input := blockRetentionInput(64, 16, time.Now())
	device.updateBlockActions(blockRetentionInput(1024, 16, time.Now()))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		device.updateBlockActions(input)
	}
	runtime.KeepAlive(device)
}
