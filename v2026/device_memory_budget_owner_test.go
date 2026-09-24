package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Process runtime telemetry is shared, but transport admission telemetry
// belongs to the queried device. The package getter has no imaginary root.
func TestDeviceMemoryStatsReportOnlyOwnedCarrierClaims(t *testing.T) {
	firstBudget := connect.NewPlatformTransportBudget(1024, 2)
	secondBudget := connect.NewPlatformTransportBudget(1024, 2)
	first := &DeviceLocal{platformTransportBudget: firstBudget}
	second := &DeviceLocal{platformTransportBudget: secondBudget}
	firstStats := first.GetMemoryStats()
	secondStats := second.GetMemoryStats()
	processStats := GetMemoryStats()
	if firstStats.PlatformTransportBudgetTotalByteCount != 1024 || secondStats.PlatformTransportBudgetTotalByteCount != 1024 {
		t.Fatal("device transport budget limits were not reported")
	}
	if processStats.PlatformTransportBudgetTotalByteCount != 0 || processStats.PlatformTransportBudgetUsedCount != 0 {
		t.Fatal("process stats reported an unowned transport admission budget")
	}
}
