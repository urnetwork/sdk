package sdk

import (
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The share and settings controller (EXTENDER.md K6, K7). One implementation
// for every app, so these pin the payload, the foreign host rule and the
// settings write rather than each app doing it again.

// A controller over a space with a real extender network host, and the space
// and manager behind it.
func testExtenderViewController(t *testing.T) (
	*ExtenderViewController,
	*NetworkSpace,
	*NetworkSpaceManager,
) {
	t.Helper()
	networkSpaceManager, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	vc := newExtenderViewController(deviceLocal.ctx, deviceLocal)
	t.Cleanup(vc.Close)
	return vc, networkSpace, networkSpaceManager
}

// The effective settings, the default flags, and a save that restarts the
// space's extender network in place (K6).
func TestExtenderViewControllerSettings(t *testing.T) {
	vc, networkSpace, networkSpaceManager := testExtenderViewController(t)

	settings := vc.GetSettings()
	connect.AssertEqual(t, settings.DnsName, "extender.space.example")
	connect.AssertEqual(t, settings.DnsNameDefault, true)
	connect.AssertEqual(t, settings.GossipUrl, "wss://gossip.space.example")
	connect.AssertEqual(t, settings.GossipUrlDefault, true)
	connect.AssertEqual(t, settings.NetworkHost, "space.example")
	connect.AssertEqual(t, settings.Hosts.Len(), 0)
	// the bundled table names no key for this host, so there is none in force
	connect.AssertEqual(t, settings.RootPublicKeysDefault, true)
	connect.AssertEqual(t, settings.RootPublicKeys.Len(), 0)

	hosts := NewStringList()
	hosts.Add("192.0.2.1")
	hosts.Add(" bootstrap.example ")
	saved := vc.SetSettings("x.example", "wss://g.example/", hosts)
	connect.AssertEqual(t, saved.DnsName, "x.example")
	connect.AssertEqual(t, saved.DnsNameDefault, false)
	// the trailing slash is trimmed, as it is for a configured value anywhere
	connect.AssertEqual(t, saved.GossipUrl, "wss://g.example")
	connect.AssertEqual(t, saved.GossipUrlDefault, false)
	connect.AssertEqual(t, saved.Hosts.Len(), 2)
	connect.AssertEqual(t, saved.Hosts.Get(0), "192.0.2.1")
	connect.AssertEqual(t, saved.Hosts.Get(1), "bootstrap.example")

	// the space the controller and the device hold is still the live one
	if networkSpaceManager.GetNetworkSpace(networkSpace.GetKey()) != networkSpace {
		t.Fatal("saving the settings replaced the space the controller holds")
	}
	connect.AssertEqual(t, networkSpace.GetExtenderDnsName(), "x.example")
	connect.AssertEqual(t, networkSpace.GetGossipUrl(), "wss://g.example")

	// a field holding only whitespace is a blank field, not an override
	blank := vc.SetSettings("   ", "  ", NewStringList())
	connect.AssertEqual(t, blank.DnsName, "extender.space.example")
	connect.AssertEqual(t, blank.DnsNameDefault, true)
	connect.AssertEqual(t, blank.GossipUrl, "wss://gossip.space.example")
	connect.AssertEqual(t, blank.GossipUrlDefault, true)

	// clearing a field goes back to the derived default
	vc.SetSettings("x.example", "wss://g.example", hosts)
	cleared := vc.SetSettings("", "", NewStringList())
	connect.AssertEqual(t, cleared.DnsName, "extender.space.example")
	connect.AssertEqual(t, cleared.DnsNameDefault, true)
	connect.AssertEqual(t, cleared.GossipUrl, "wss://gossip.space.example")
	connect.AssertEqual(t, cleared.GossipUrlDefault, true)
	connect.AssertEqual(t, cleared.Hosts.Len(), 0)

	// and saving an unchanged form is a no-op, not a teardown
	vc.SetSettings("", "", NewStringList())
	if networkSpaceManager.GetNetworkSpace(networkSpace.GetKey()) != networkSpace {
		t.Fatal("saving an unchanged settings form replaced the space")
	}
}

// A share round-trips with and without the settings block, and the addresses
// it carries are this space's (K7).
func TestExtenderViewControllerShareRoundTrip(t *testing.T) {
	vc, networkSpace, _ := testExtenderViewController(t)
	directory := networkSpace.extenderDirectory
	directory.AddBootstrap(netip.MustParseAddr("192.0.2.1"), connect.ExtenderSourceDns)
	directory.AddBootstrap(netip.MustParseAddr("2001:db8::1"), connect.ExtenderSourceDns)
	directory.SetInUse(netip.MustParseAddr("2001:db8::1"), 1)

	plain := vc.BuildShare(false)
	if !strings.HasPrefix(plain.Text, connect.ExtenderSharePrefix) {
		t.Fatalf("share = %q, expected the %s prefix", plain.Text, connect.ExtenderSharePrefix)
	}
	connect.AssertEqual(t, plain.Count, 2)
	connect.AssertEqual(t, plain.IncludesSettings, false)

	decoded := vc.DecodeShare(plain.Text)
	connect.AssertEqual(t, decoded.Ok, true)
	connect.AssertEqual(t, decoded.Error, "")
	connect.AssertEqual(t, decoded.NetworkHost, "space.example")
	connect.AssertEqual(t, decoded.ForeignHost, false)
	connect.AssertEqual(t, decoded.Count, 2)
	connect.AssertEqual(t, decoded.HasSettings, false)
	connect.AssertEqual(t, decoded.SettingsHost, "")
	// the payload survives the whitespace a scan or a paste adds around it
	connect.AssertEqual(t, vc.DecodeShare("  \n"+plain.Text+"\n ").Count, 2)
	// the address carrying a connection is shared first (K7)
	share, err := connect.DecodeExtenderShare(plain.Text)
	if err != nil {
		t.Fatal(err)
	}
	addresses := connect.ExtenderShareAddresses(share)
	if len(addresses) != 2 || addresses[0].String() != "2001:db8::1" {
		t.Fatalf("addresses = %v, expected the in-use one first", addresses)
	}

	// with the settings block
	vc.SetSettings("x.example", "wss://g.example", NewStringList())
	networkSpace.extenderDirectory.SetRootKeys(testExtenderRootKeySet(t))
	withSettings := vc.BuildShare(true)
	connect.AssertEqual(t, withSettings.IncludesSettings, true)
	connect.AssertEqual(t, withSettings.Count, 2)

	decoded = vc.DecodeShare(withSettings.Text)
	connect.AssertEqual(t, decoded.Ok, true)
	connect.AssertEqual(t, decoded.HasSettings, true)
	connect.AssertEqual(t, decoded.SettingsHost, "x.example")
	connect.AssertEqual(t, decoded.ForeignHost, false)
	settingsShare, err := connect.DecodeExtenderShare(withSettings.Text)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, settingsShare.Settings.GossipUrl, "wss://g.example")
	if keyHexes := connect.ExtenderShareRootKeyHexes(settingsShare); len(keyHexes) != 1 ||
		keyHexes[0] != testExtenderRootPublicKeyHex {
		t.Fatalf("share root keys = %v", connect.ExtenderShareRootKeyHexes(settingsShare))
	}
}

// A payload from another operator is decodable but marked foreign, which is
// what the ui asks about before anything is applied (K7).
func TestExtenderViewControllerDecodesAForeignHost(t *testing.T) {
	vc, _, _ := testExtenderViewController(t)

	text := testForeignExtenderShare(t)
	decoded := vc.DecodeShare(text)
	connect.AssertEqual(t, decoded.Ok, true)
	connect.AssertEqual(t, decoded.NetworkHost, "other.example")
	connect.AssertEqual(t, decoded.ForeignHost, true)
	connect.AssertEqual(t, decoded.Count, 1)
	connect.AssertEqual(t, decoded.HasSettings, true)
	connect.AssertEqual(t, decoded.SettingsHost, "extender.other.example")

	// anything that is not a payload is one error id, whatever is wrong with it
	for _, invalid := range []string{
		"",
		"hello",
		"ur-ext:1:not-base64!!",
		"ur-ext:2:AAAA",
		connect.ExtenderSharePrefix,
	} {
		result := vc.DecodeShare(invalid)
		connect.AssertEqual(t, result.Ok, false)
		connect.AssertEqual(t, result.Error, ExtenderImportErrorInvalid)
	}
}

// An import of this space's own payload applies its addresses as unverified
// bootstrap entries sourced `import` (K7).
func TestExtenderViewControllerImportsOwnShare(t *testing.T) {
	vc, networkSpace, _ := testExtenderViewController(t)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)
	text := vc.BuildShare(false).Text

	// a second space imports it
	otherVc, otherSpace, _ := testExtenderViewController(t)
	result := otherVc.ImportShare(text, false)
	connect.AssertEqual(t, result.Ok, true)
	connect.AssertEqual(t, result.Error, "")
	connect.AssertEqual(t, result.ImportedCount, 1)

	entries := otherSpace.extenderDirectory.Snapshot().Entries
	if len(entries) != 1 {
		t.Fatalf("entries = %d, expected the imported address", len(entries))
	}
	connect.AssertEqual(t, entries[0].Ip.String(), "192.0.2.1")
	connect.AssertEqual(t, entries[0].Source, connect.ExtenderSourceImport)
	connect.AssertEqual(t, entries[0].State, connect.ExtenderStateUnverified)
	// an address already known counts zero rather than resetting the set
	connect.AssertEqual(t, otherVc.ImportShare(text, false).ImportedCount, 0)

	// the event rate is a measure of the network, and an import is not one
	connect.AssertEqual(t, otherSpace.GetExtenderStatus().EventCountLastMinute, 0)

	// an invalid payload never reaches the directory
	invalid := otherVc.ImportShare("ur-ext:1:!!", false)
	connect.AssertEqual(t, invalid.Ok, false)
	connect.AssertEqual(t, invalid.Error, ExtenderImportErrorInvalid)
	connect.AssertEqual(t, invalid.ImportedCount, 0)
	connect.AssertEqual(t, len(otherSpace.extenderDirectory.Snapshot().Entries), 1)
}

// A foreign payload is refused outright unless the settings are taken with it,
// and taking them rewrites the dns name, the gossip url and the trust anchor
// through the same path the settings screen saves by (K7).
func TestExtenderViewControllerImportsAForeignHostOnlyWithSettings(t *testing.T) {
	vc, networkSpace, networkSpaceManager := testExtenderViewController(t)
	text := testForeignExtenderShare(t)

	refused := vc.ImportShare(text, false)
	connect.AssertEqual(t, refused.Ok, false)
	connect.AssertEqual(t, refused.Error, ExtenderImportErrorForeignHost)
	connect.AssertEqual(t, refused.ImportedCount, 0)
	if entries := networkSpace.extenderDirectory.Snapshot().Entries; len(entries) != 0 {
		t.Fatalf("a refused import applied %d addresses", len(entries))
	}
	// nothing of the settings was taken either
	connect.AssertEqual(t, networkSpace.GetExtenderDnsName(), "extender.space.example")

	// a foreign payload with no settings block is refused even when the
	// settings are asked for: there are none to replace, and its addresses
	// could never verify against this space's network host
	noSettings := vc.ImportShare(testForeignExtenderShareWithoutSettings(t), true)
	connect.AssertEqual(t, noSettings.Ok, false)
	connect.AssertEqual(t, noSettings.Error, ExtenderImportErrorForeignHost)
	connect.AssertEqual(t, len(networkSpace.extenderDirectory.Snapshot().Entries), 0)

	applied := vc.ImportShare(text, true)
	connect.AssertEqual(t, applied.Ok, true)
	connect.AssertEqual(t, applied.Error, "")
	connect.AssertEqual(t, applied.ImportedCount, 1)

	entries := networkSpace.extenderDirectory.Snapshot().Entries
	if len(entries) != 1 || entries[0].Ip.String() != "192.0.2.50" {
		t.Fatalf("entries = %v", entries)
	}
	connect.AssertEqual(t, entries[0].Source, connect.ExtenderSourceImport)

	settings := vc.GetSettings()
	connect.AssertEqual(t, settings.DnsName, "extender.other.example")
	connect.AssertEqual(t, settings.DnsNameDefault, false)
	connect.AssertEqual(t, settings.GossipUrl, "wss://gossip.other.example")
	connect.AssertEqual(t, settings.GossipUrlDefault, false)
	connect.AssertEqual(t, settings.RootPublicKeysDefault, false)
	if settings.RootPublicKeys.Len() != 1 ||
		settings.RootPublicKeys.Get(0) != testExtenderRootPublicKeyHex {
		t.Fatalf("root keys = %v", settings.RootPublicKeys.getAll())
	}
	// the anchor in force followed, and the space survived the write
	if rootKeys := networkSpace.extenderDirectory.RootKeys(); rootKeys.Len() != 1 {
		t.Fatalf("directory root keys = %v", rootKeys)
	}
	if networkSpaceManager.GetNetworkSpace(networkSpace.GetKey()) != networkSpace {
		t.Fatal("an import with settings replaced the space")
	}
	// the network host is still this space's, so the imported addresses stay
	// unverified until a record this space accepts names them
	connect.AssertEqual(t, vc.GetSettings().NetworkHost, "space.example")
	connect.AssertEqual(t, entries[0].State, connect.ExtenderStateUnverified)
}

// A payload from another operator: one address and a settings block, encoded
// the way that operator's own app would.
func testForeignExtenderShare(t *testing.T) string {
	t.Helper()
	publicKey, err := hex.DecodeString(testExtenderRootPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	text, err := connect.EncodeExtenderShare(&protocol.ExtenderShare{
		Version:     connect.ExtenderShareVersion,
		NetworkHost: "other.example",
		Addresses:   [][]byte{{192, 0, 2, 50}},
		Settings: &protocol.ExtenderShareSettings{
			DnsName:        "extender.other.example",
			GossipUrl:      "wss://gossip.other.example",
			RootPublicKeys: [][]byte{publicKey},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// The same operator's payload with no settings block, which an importer has
// nothing to confirm for.
func testForeignExtenderShareWithoutSettings(t *testing.T) string {
	t.Helper()
	text, err := connect.EncodeExtenderShare(&protocol.ExtenderShare{
		Version:     connect.ExtenderShareVersion,
		NetworkHost: "other.example",
		Addresses:   [][]byte{{192, 0, 2, 51}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func testExtenderRootKeySet(t *testing.T) *connect.ExtenderRootKeySet {
	t.Helper()
	keySet, err := connect.NewExtenderRootKeySetFromHex(testExtenderRootPublicKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if keySet.Len() != 1 {
		t.Fatalf("key set = %v, expected one key", keySet)
	}
	return keySet
}
