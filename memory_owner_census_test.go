//go:build !ios

package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

func TestMemoryOwnerCensusCoreAllocatesNothing(t *testing.T) {
	device := &DeviceLocal{}
	_ = device.memoryOwnerCensus() // initialize the existing process budget
	if got := testing.AllocsPerRun(100, func() { _ = device.memoryOwnerCensus() }); got != 0 {
		t.Fatalf("owner read allocations=%g", got)
	}
	var nilDevice *DeviceLocal
	if got := nilDevice.memoryOwnerCensus(); got.BlockActions != 0 || got.Client.Flows != 0 || got.Api.Dialers != 0 {
		t.Fatalf("nil device census invents owners: %+v", got)
	}
}

func TestMemoryOwnerCensusPrivateExclusiveAndSanitized(t *testing.T) {
	const secret = "private-diagnostic-sentinel-not-for-json"
	device := &DeviceLocal{byJwt: secret, blockActions: make([]*BlockAction, 1, 8)}
	device.blockActions[0] = &BlockAction{Hosts: NewStringList()}
	device.blockActions[0].Hosts.Add(secret)
	path := filepath.Join(t.TempDir(), "owners.json")
	if err := device.WriteMemoryOwnerCensus(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "byJwt") {
		t.Fatal("owner census exposed identity content")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private file=%v err=%v", info, err)
	}
	var report memoryOwnerCensusReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != 1 || report.UnixMillis <= 0 || report.Before.RuntimeBytes == 0 ||
		report.Owners.BlockActions != 1 || report.Owners.BlockActionSlots != 8 {
		t.Fatalf("incomplete report: %+v", report)
	}
	if err := device.WriteMemoryOwnerCensus(path); !os.IsExist(err) {
		t.Fatalf("existing artifact overwrite was not refused: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("refused write changed prior evidence")
	}
}

func TestMemoryOwnerCensusDoesNotCollectShedOrChangeRuntimePolicy(t *testing.T) {
	// Remove natural GC as a test variable; restore the process policy. The
	// diagnostic method must not force a collection or invoke any shed callback.
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	sheds := 0
	unregister := connect.AddMemoryShedder(func() { sheds++ })
	defer unregister()
	profileRate := runtime.MemProfileRate
	limit := debug.SetMemoryLimit(-1)
	device := &DeviceLocal{}
	_ = device.memoryOwnerCensus()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	report := device.memoryOwnerCensusReport()
	runtime.ReadMemStats(&after)
	if before.NumForcedGC != after.NumForcedGC || report.Before.ForcedGcCycles != report.After.ForcedGcCycles || sheds != 0 {
		t.Fatalf("diagnostic changed lifecycle/GC: before=%d after=%d sheds=%d", before.NumForcedGC, after.NumForcedGC, sheds)
	}
	if runtime.MemProfileRate != profileRate || debug.SetMemoryLimit(-1) != limit || debug.SetGCPercent(-1) != -1 {
		t.Fatal("diagnostic changed runtime policy")
	}
	if len(report.SizeClasses) != len(before.BySize) {
		t.Fatal("runtime size-class schema changed")
	}
	for i, class := range report.SizeClasses {
		if class.SizeBytes != before.BySize[i].Size {
			t.Fatalf("class %d size=%d want=%d", i, class.SizeBytes, before.BySize[i].Size)
		}
	}
}

func TestMemoryOwnerCensusConcurrentPrimitiveRead(t *testing.T) {
	device := &DeviceLocal{}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				_ = device.memoryOwnerCensus()
			}
		})
	}
	for range 100 {
		device.stateLock.Lock()
		device.blockActions = append(device.blockActions[:0], &BlockAction{})
		device.stateLock.Unlock()
	}
	workers.Wait()
}

func BenchmarkMemoryOwnerCensus(b *testing.B) {
	device := &DeviceLocal{}
	_ = device.memoryOwnerCensus()
	b.ReportAllocs()
	for b.Loop() {
		_ = device.memoryOwnerCensus()
	}
}

func BenchmarkMemoryOwnerCensusRuntimeReport(b *testing.B) {
	device := &DeviceLocal{}
	_ = device.memoryOwnerCensusReport()
	b.ReportAllocs()
	for b.Loop() {
		_ = device.memoryOwnerCensusReport()
	}
}
