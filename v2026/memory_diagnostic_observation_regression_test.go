package sdk

// The existing diagnostic batch is a separate, retained runtime observation;
// it must carry the live rate read with its bytes rather than a guessed zero.
import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// This test uses only pre-existing APIs and deterministically fails before
// the additive diagnostic rate field exists. It claims no host runtime cap.
func TestTransferDiagnosticRuntimeRetainsActualProfileRate(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 32*1024*1024)
	device.transferDiagStats = &connect.P2pDataPlaneStats{}
	device.memorySampler = &mobileMemorySampler{}
	previousRate := runtime.MemProfileRate
	runtime.MemProfileRate = 65536
	defer func() { runtime.MemProfileRate = previousRate }()
	batch, err := device.TransferDiagnosticSnapshotJson()
	if err != nil {
		t.Fatal(err)
	}
	memoryCount := 0
	for _, line := range strings.Split(strings.TrimSpace(batch), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["part"] == "memory" {
			memoryCount++
			if row["memory_profile_rate_bytes"] != float64(65536) || row["device_memory_target_bytes"] != float64(32*1024*1024) {
				t.Fatalf("diagnostic runtime policy was absent or relabeled: %+v", row)
			}
		}
	}
	if memoryCount != 1 {
		t.Fatalf("wanted exactly one real runtime observation, got %d", memoryCount)
	}
}
