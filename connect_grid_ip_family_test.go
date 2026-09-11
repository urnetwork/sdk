package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
)

// every provider dot carries its address-family category and label from the
// connect provider event, a legacy event reads as v4-only, and a category
// change on a live dot (the local v6 downgrade) updates the dot in place
func TestConnectGridPointsCarryIpFamily(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation

	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	dualstack := connect.NewId()
	v4Only := connect.NewId()
	v6Only := connect.NewId()
	legacy := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		dualstack: {ClientId: dualstack, State: connect.ProviderStateAdded, IpFamily: connect.IpFamilyDualstack},
		v4Only:    {ClientId: v4Only, State: connect.ProviderStateAdded, IpFamily: connect.IpFamilyV4Only},
		v6Only:    {ClientId: v6Only, State: connect.ProviderStateInEvaluation, IpFamily: connect.IpFamilyV6Only},
		legacy:    {ClientId: legacy, State: connect.ProviderStateAdded},
	})

	point := func(clientId connect.Id) *ProviderGridPoint {
		p := grid.GetProviderGridPointByClientId(newId(clientId))
		if p == nil {
			t.Fatalf("missing grid point for %s", clientId)
		}
		return p
	}
	connect.AssertEqual(t, point(dualstack).IpFamily, IpFamilyDualstack)
	connect.AssertEqual(t, point(dualstack).IpFamilyLabel, IpFamilyLabelBoth)
	connect.AssertEqual(t, point(v4Only).IpFamily, IpFamilyV4Only)
	connect.AssertEqual(t, point(v4Only).IpFamilyLabel, IpFamilyLabelV4)
	connect.AssertEqual(t, point(v6Only).IpFamily, IpFamilyV6Only)
	connect.AssertEqual(t, point(v6Only).IpFamilyLabel, IpFamilyLabelV6)
	connect.AssertEqual(t, point(legacy).IpFamily, IpFamilyV4Only)
	connect.AssertEqual(t, point(legacy).IpFamilyLabel, IpFamilyLabelV4)

	// the list copies carry the fields too
	list := grid.GetProviderGridPointList()
	connect.AssertEqual(t, list.Len(), 4)
	for i := range list.Len() {
		p := list.Get(i)
		if p.IpFamily == "" || p.IpFamilyLabel == "" {
			t.Fatalf("grid point list entry lost its family: %+v", p)
		}
	}

	// a local downgrade arrives as the same state with a new category
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		dualstack: {ClientId: dualstack, State: connect.ProviderStateAdded, IpFamily: connect.IpFamilyV4Only},
	})
	connect.AssertEqual(t, point(dualstack).IpFamily, IpFamilyV4Only)
	connect.AssertEqual(t, point(dualstack).IpFamilyLabel, IpFamilyLabelV4)
	connect.AssertEqual(t, point(dualstack).State, ProviderStateAdded)
}
