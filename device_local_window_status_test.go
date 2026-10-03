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

// the Added providers split by category and sum to ProviderStateAdded; a legacy
// event counts as v4-only and non-added states are not counted (IPV6.md D1)
func TestToWindowStatusCountsIpFamilies(t *testing.T) {
	monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()

	add := func(state connect.ProviderState, ipFamily connect.IpFamily) {
		clientId := connect.NewId()
		monitor.AddProviderEvent(clientId, state, clientId, nil, ipFamily)
	}
	add(connect.ProviderStateAdded, connect.IpFamilyDualstack)
	add(connect.ProviderStateAdded, connect.IpFamilyDualstack)
	add(connect.ProviderStateAdded, connect.IpFamilyV4Only)
	add(connect.ProviderStateAdded, connect.IpFamilyV6Only)
	add(connect.ProviderStateAdded, connect.IpFamilyLegacy)
	add(connect.ProviderStateInEvaluation, connect.IpFamilyDualstack)

	windowStatus := toWindowStatus(monitor)
	connect.AssertEqual(t, windowStatus.ProviderStateAdded, 5)
	connect.AssertEqual(t, windowStatus.ProviderStateInEvaluation, 1)
	connect.AssertEqual(t, windowStatus.ProviderDualstackCount, 2)
	connect.AssertEqual(t, windowStatus.ProviderV4OnlyCount, 2)
	connect.AssertEqual(t, windowStatus.ProviderV6OnlyCount, 1)
	connect.AssertEqual(t,
		windowStatus.ProviderDualstackCount+windowStatus.ProviderV4OnlyCount+windowStatus.ProviderV6OnlyCount,
		windowStatus.ProviderStateAdded,
	)
}

// Ipv6Available mirrors the merged window expand event exactly: false before
// any expand event, and whatever the event says afterwards.
func TestToWindowStatusIpv6Available(t *testing.T) {
	monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
	connect.AssertEqual(t, toWindowStatus(monitor).Ipv6Available, false)
	for _, ipv6Available := range []bool{true, false, true} {
		monitor.AddWindowExpandEvent(true, 1, ipv6Available)
		connect.AssertEqual(t, toWindowStatus(monitor).Ipv6Available, ipv6Available)
	}
}
