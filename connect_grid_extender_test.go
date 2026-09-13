package sdk

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
)

// The sdk half of EXTENDER.md K1 and K3: the extender addresses of a provider
// event reach the grid point with their colors, over a local device and over
// the device rpc, and the color itself is pinned so every app can test its own
// binding against the same values.

// The pinned colors. An app's own test asserts these exact strings against its
// binding, so a change here is a change every app has to see. FNV-1a 32 over
// the canonical address, hue the hash modulo 360, saturation 70 percent,
// lightness 55 percent. Documentation addresses only (CODESTYLE).
var testExtenderColorHexes = map[string]string{
	"192.0.2.1":    "3cdd67",
	"192.0.2.2":    "dd3cba",
	"198.51.100.7": "3cdd49",
	"203.0.113.42": "3c4fdd",
	"2001:db8::1":  "dd4f3c",
}

func TestGetExtenderColorHex(t *testing.T) {
	for ip, colorHex := range testExtenderColorHexes {
		if got := GetExtenderColorHex(ip); got != colorHex {
			t.Errorf("%s = %q, expected %q", ip, got, colorHex)
		}
	}
	// the color is of the CANONICAL address, so the same address written two
	// ways is one ring rather than two
	for _, c := range []struct {
		written   string
		canonical string
	}{
		{written: "2001:DB8::1", canonical: "2001:db8::1"},
		{written: "2001:db8:0:0:0:0:0:1", canonical: "2001:db8::1"},
		{written: "::ffff:192.0.2.1", canonical: "192.0.2.1"},
		{written: "  192.0.2.1  ", canonical: "192.0.2.1"},
	} {
		if got := GetExtenderColorHex(c.written); got != testExtenderColorHexes[c.canonical] {
			t.Errorf("%s = %q, expected the color of %s", c.written, got, c.canonical)
		}
	}
	// every color is six hex digits with no leading marker, whatever the input
	for _, ip := range []string{"192.0.2.1", "2001:db8::1", "not-an-address", ""} {
		colorHex := GetExtenderColorHex(ip)
		if len(colorHex) != 6 {
			t.Errorf("%q = %q, expected six hex digits", ip, colorHex)
		}
		for _, c := range colorHex {
			switch {
			case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
			default:
				t.Errorf("%q = %q, expected lower case hex", ip, colorHex)
			}
		}
	}
}

// A provider reached through an extender carries that address and its color on
// its grid point, in the same order, and a migration that changes the set
// updates the live point rather than leaving a stale ring (K1).
func TestConnectGridPointsCarryExtenderIps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation

	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	throughExtender := connect.NewId()
	migrating := connect.NewId()
	direct := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		throughExtender: {
			ClientId:    throughExtender,
			State:       connect.ProviderStateAdded,
			ExtenderIps: []netip.Addr{netip.MustParseAddr("192.0.2.1")},
		},
		migrating: {
			ClientId: migrating,
			State:    connect.ProviderStateAdded,
			ExtenderIps: []netip.Addr{
				netip.MustParseAddr("192.0.2.1"),
				netip.MustParseAddr("2001:db8::1"),
			},
		},
		direct: {ClientId: direct, State: connect.ProviderStateAdded},
	})

	point := func(clientId connect.Id) *ProviderGridPoint {
		p := grid.GetProviderGridPointByClientId(newId(clientId))
		if p == nil {
			t.Fatalf("missing grid point for %s", clientId)
		}
		return p
	}
	connect.AssertEqual(t, point(throughExtender).ExtenderIps, "192.0.2.1")
	connect.AssertEqual(t, point(throughExtender).ExtenderColorHexes, testExtenderColorHexes["192.0.2.1"])
	// two addresses across a migration, paired by index
	connect.AssertEqual(t, point(migrating).ExtenderIps, "192.0.2.1,2001:db8::1")
	connect.AssertEqual(
		t,
		point(migrating).ExtenderColorHexes,
		testExtenderColorHexes["192.0.2.1"]+","+testExtenderColorHexes["2001:db8::1"],
	)
	// a direct or p2p route carries none, which is the common case
	connect.AssertEqual(t, point(direct).ExtenderIps, "")
	connect.AssertEqual(t, point(direct).ExtenderColorHexes, "")

	// the list copies carry the fields too
	list := grid.GetProviderGridPointList()
	connect.AssertEqual(t, list.Len(), 3)
	for i := range list.Len() {
		p := list.Get(i)
		if p.ClientId.Cmp(newId(migrating)) == 0 {
			connect.AssertEqual(t, p.ExtenderIps, "192.0.2.1,2001:db8::1")
		}
	}

	// the migration completes on the second address, and the dot follows
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		migrating: {
			ClientId:    migrating,
			State:       connect.ProviderStateAdded,
			ExtenderIps: []netip.Addr{netip.MustParseAddr("2001:db8::1")},
		},
	})
	connect.AssertEqual(t, point(migrating).ExtenderIps, "2001:db8::1")
	connect.AssertEqual(t, point(migrating).ExtenderColorHexes, testExtenderColorHexes["2001:db8::1"])

	// the route drops back to direct and the rings go with it
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		migrating: {ClientId: migrating, State: connect.ProviderStateAdded},
	})
	connect.AssertEqual(t, point(migrating).ExtenderIps, "")
	connect.AssertEqual(t, point(migrating).ExtenderColorHexes, "")
}

// The same fields over a DeviceRemote. Provider events cross the device rpc
// as `connect.ProviderEvent` itself, so the guard that matters is that the gob
// wire carries `ExtenderIps` (netip.Addr has no exported fields) and that the
// grid a remote device drives reads them identically (K1).
func TestConnectGridExtenderIpsCrossTheDeviceRpc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientId := connect.NewId()
	sent := &DeviceRemoteWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{connect.NewId(): true},
		WindowExpandEvent: &connect.WindowExpandEvent{TargetSize: 1, MinSatisfied: true},
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			clientId: {
				ClientId: clientId,
				State:    connect.ProviderStateAdded,
				IpFamily: connect.IpFamilyDualstack,
				ExtenderIps: []netip.Addr{
					netip.MustParseAddr("192.0.2.1"),
					netip.MustParseAddr("2001:db8::1"),
				},
			},
		},
		Reset: true,
	}
	received := gobRoundTrip(t, sent)
	wired := received.ProviderEvents[clientId]
	if wired == nil {
		t.Fatal("the provider event did not cross the rpc")
	}
	if len(wired.ExtenderIps) != 2 {
		t.Fatalf("extender ips = %v, expected two", wired.ExtenderIps)
	}

	// the remote's grid is the same grid; feeding it the decoded events is
	// what the device rpc's window monitor bridge does
	vc := newTestingConnectViewController(ctx)
	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation

	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)
	monitor.emit(received.ProviderEvents)

	point := grid.GetProviderGridPointByClientId(newId(clientId))
	if point == nil {
		t.Fatal("missing grid point after the rpc mirror")
	}
	connect.AssertEqual(t, point.ExtenderIps, "192.0.2.1,2001:db8::1")
	connect.AssertEqual(
		t,
		point.ExtenderColorHexes,
		testExtenderColorHexes["192.0.2.1"]+","+testExtenderColorHexes["2001:db8::1"],
	)
	// the family still crosses beside it
	connect.AssertEqual(t, point.IpFamily, IpFamilyDualstack)
}

// gob must be able to compile the whole mirror type, not only the fields a
// fixture happened to fill: a nil `ExtenderIps` and an empty one both have to
// encode, because most provider events carry no extender at all.
func TestProviderEventGobCarriesEmptyExtenderIps(t *testing.T) {
	clientId := connect.NewId()
	for _, extenderIps := range [][]netip.Addr{nil, {}} {
		event := &connect.ProviderEvent{
			ClientId:    clientId,
			State:       connect.ProviderStateAdded,
			ExtenderIps: extenderIps,
		}
		wired := gobRoundTrip(t, event)
		if 0 < len(wired.ExtenderIps) {
			t.Fatalf("extender ips = %v, expected none", wired.ExtenderIps)
		}
	}
}
