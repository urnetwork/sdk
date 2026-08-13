package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// TestToWindowStatusCarriesStallDiagnosis pins the WindowStatus rpc surface:
// the stall reason and the failed latch cross from the connect monitor's
// WindowExpandEvent into the struct DeviceRemote reads (GetWindowStatus and
// the change listener both carry it, so the app sees the same diagnosis as
// DeviceLocal). This is the sdk half of the connect window honesty layer
// (urnetwork/connect#199) -- urnetwork/windows#1 reads WindowStatus.Failed
// and WindowStatus.StallReason directly.
func TestToWindowStatusCarriesStallDiagnosis(t *testing.T) {
	monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()

	windowStatus := toWindowStatus(monitor)
	connect.AssertEqual(t, windowStatus.StallReason, connect.WindowStallEvaluating)
	connect.AssertEqual(t, windowStatus.Failed, false)

	monitor.SetStallStatus(connect.WindowStallPlatformUnreachable, true)
	windowStatus = toWindowStatus(monitor)
	connect.AssertEqual(t, windowStatus.StallReason, connect.WindowStallPlatformUnreachable)
	connect.AssertEqual(t, windowStatus.Failed, true)
}
