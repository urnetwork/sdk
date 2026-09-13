package sdk

import (
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
)

// The small app-facing rules of the extender work that the end-to-end tests
// exercise but never pin on their own (EXTENDER.md J4, K1, K3, K6).

// The two lists a grid point carries pair by index, so an app draws ring n in
// color n. An address that does not parse is dropped from BOTH, because a
// dropped color with a kept address is a ring drawn in the wrong extender's
// color (K1, K3).
func TestExtenderIpsAndColorHexesPairByIndex(t *testing.T) {
	cases := []struct {
		name               string
		extenderIps        []netip.Addr
		expectIps          string
		expectColorIndexes []string
	}{
		{name: "no extender", extenderIps: nil, expectIps: "", expectColorIndexes: nil},
		{
			name:               "one",
			extenderIps:        []netip.Addr{netip.MustParseAddr("192.0.2.1")},
			expectIps:          "192.0.2.1",
			expectColorIndexes: []string{"192.0.2.1"},
		},
		{
			// both families, which is what a transport migration looks like
			name: "two, in order",
			extenderIps: []netip.Addr{
				netip.MustParseAddr("192.0.2.1"),
				netip.MustParseAddr("2001:db8::1"),
			},
			expectIps:          "192.0.2.1,2001:db8::1",
			expectColorIndexes: []string{"192.0.2.1", "2001:db8::1"},
		},
		{
			// the canonical form is what the color is of, so an app never sees
			// two rings for one address
			name:               "v4 mapped",
			extenderIps:        []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.1")},
			expectIps:          "192.0.2.1",
			expectColorIndexes: []string{"192.0.2.1"},
		},
		{
			name: "an invalid address is dropped from both",
			extenderIps: []netip.Addr{
				{},
				netip.MustParseAddr("198.51.100.7"),
				{},
			},
			expectIps:          "198.51.100.7",
			expectColorIndexes: []string{"198.51.100.7"},
		},
		{
			name:               "every address invalid",
			extenderIps:        []netip.Addr{{}},
			expectIps:          "",
			expectColorIndexes: nil,
		},
	}
	for _, c := range cases {
		extenderIps, extenderColorHexes := extenderIpsAndColorHexes(c.extenderIps)
		if extenderIps != c.expectIps {
			t.Errorf("%s: ips = %q, expected %q", c.name, extenderIps, c.expectIps)
		}
		expectColorHexes := ""
		for i, ip := range c.expectColorIndexes {
			if 0 < i {
				expectColorHexes += ","
			}
			expectColorHexes += GetExtenderColorHex(ip)
		}
		if extenderColorHexes != expectColorHexes {
			t.Errorf(
				"%s: colors = %q, expected %q",
				c.name,
				extenderColorHexes,
				expectColorHexes,
			)
		}
	}
}

// A provider whose mode serves public peers dials the platform directly on
// every transport, so the platform observes the provider's own address (J4).
// The modes are ordered by openness, and the rule errs toward public: dialing
// direct where it was not needed costs nothing, while a public provider tagged
// with an extender's address is the failure this exists to prevent.
func TestProvideModeIncludesPublic(t *testing.T) {
	cases := []struct {
		provideMode ProvideMode
		expect      bool
	}{
		{provideMode: ProvideModeNone, expect: false},
		{provideMode: ProvideModeNetwork, expect: false},
		{provideMode: ProvideModeFriendsAndFamily, expect: false},
		{provideMode: ProvideModePublic, expect: true},
		{provideMode: ProvideModeStream, expect: true},
	}
	for _, c := range cases {
		if public := provideModeIncludesPublic(c.provideMode); public != c.expect {
			t.Errorf("mode %d: public = %v, expected %v", c.provideMode, public, c.expect)
		}
	}
}

// A field holding only whitespace is a blank field, so the settings screen
// shows the derived default as a placeholder rather than claiming an override
// the space does not have (K6).
func TestExtenderSettingsWhitespaceIsABlankField(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {
			values.ExtenderDnsName = "  "
			values.GossipUrl = "\t\n"
			values.ExtenderRootPublicKeys = []string{"   ", ""}
			values.ExtenderHosts = []string{" ", ""}
		},
	)

	settings := extenderSettings(networkSpace)
	connect.AssertEqual(t, settings.DnsName, "extender.space.example")
	connect.AssertEqual(t, settings.DnsNameDefault, true)
	connect.AssertEqual(t, settings.GossipUrl, "wss://gossip.space.example")
	connect.AssertEqual(t, settings.GossipUrlDefault, true)
	connect.AssertEqual(t, settings.RootPublicKeysDefault, true)
	connect.AssertEqual(t, settings.RootPublicKeys.Len(), 0)
	connect.AssertEqual(t, settings.Hosts.Len(), 0)
	connect.AssertEqual(t, settings.NetworkHost, "space.example")

	// and the empty settings of no space at all are the same shape, never nil
	empty := extenderSettings(nil)
	connect.AssertEqual(t, empty.DnsName, "")
	connect.AssertEqual(t, empty.DnsNameDefault, false)
	if empty.Hosts == nil || empty.RootPublicKeys == nil {
		t.Fatal("the empty settings carried no lists")
	}
}
