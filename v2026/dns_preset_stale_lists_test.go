package sdk

// A selection's lists in a copy an older release wrote, its record or an
// app's own copy that seeds the next device (apple DeviceManager.initDevice,
// and the other apps' LocalState mirrors), are that release's derivation:
// after a release changes the defaults, a preset or a regional
// recommendation, they load and seed a device as the selection with today's
// lists, never as a custom edit. Only lists a user edited are custom. Before,
// the record kept the old lists, and the edit rule read them as an edit, so
// the user silently dropped out of "default" into Custom.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// What a release with other default lists saved for "default": its default
// lists (here the OpenDNS-era lists the defaults had before
// DefaultServersVersion 2) under the selection.
func testingOlderReleaseDefaultSelection() *DnsResolverSettings {
	settings := testingOpenDnsEraDefaultDnsResolverSettings()
	settings.PresetId = DnsPresetIdDefault
	return settings
}

// Today's default lists, with the fallback choice and selection given.
func testingTodaysDefaultSelection(enableFallback bool) *DnsResolverSettings {
	settings := DnsResolverSettingsWithFallback(GetDefaultDnsResolverSettings(), enableFallback)
	return ownDnsResolverSettings(settings)
}

// A record exactly as a preset-aware release wrote it, lists and all.
func testingWriteSelectionRecord(t *testing.T, localState *LocalState, settings *DnsResolverSettings) {
	t.Helper()
	data, err := encodeDnsResolverSettingsRecord(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localState.localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// A record whose selection a release with other lists wrote loads as the
// selection with today's lists: lists another release derived, or lists no
// table of this one knows.
func TestStaleSelectionRecordLoadsWithTodaysLists(t *testing.T) {
	stale := testingOlderReleaseDefaultSelection()
	unknownToThisRelease := CopyDnsResolverSettings(stale)
	unknownToThisRelease.RemoteDohUrlsIpv4 = testing_stringList("https://192.0.2.1/dns-query", "https://192.0.2.2/dns-query")
	unknownToThisRelease.RemoteDnsIpv4 = testing_stringList("192.0.2.1")
	for _, settings := range []*DnsResolverSettings{stale, unknownToThisRelease} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteSelectionRecord(t, localState, settings)
		connect.AssertEqual(t, testingDnsResolverSettingsValue(localState.GetDnsResolverSettings()), testingDnsResolverSettingsValue(testingTodaysDefaultSelection(true)))
	}
}

// An app seeds a new device from the copy an older release left it: the
// device keeps "default" with today's lists, from the copy as the app holds
// it, from the app's own LocalState copy, and from the record the device
// loads.
func TestStaleSelectionSeedsDeviceLocalAsTheSelection(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteSelectionRecord(t, localState, testingOlderReleaseDefaultSelection())
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	today := testingDnsResolverSettingsValue(testingTodaysDefaultSelection(true))
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), today)

	device.SetDnsResolverSettings(testingOlderReleaseDefaultSelection())
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), today)

	mirror := &LocalState{localStorageDir: t.TempDir()}
	if err := mirror.SetDnsResolverSettings(testingOlderReleaseDefaultSelection()); err != nil {
		t.Fatal(err)
	}
	device.SetDnsResolverSettings(mirror.GetDnsResolverSettings())
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), today)

	// a regional pick an older release derived keeps its pick
	staleKz := &DnsResolverSettings{
		PresetId:        "regional-kz",
		EnableRemoteDns: true,
		RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9"),
		RemoteDnsIpv6:   testing_stringList("2606:4700:4700::1111", "2620:fe::fe"),
	}
	device.SetDnsResolverSettings(staleKz)
	settings := device.GetDnsResolverSettings()
	connect.AssertEqual(t, settings.PresetId, "regional-kz")
	connect.AssertEqual(t, testingStringsOf(settings.RemoteDnsIpv4), connect.RegionalDnsServerIps("kz"))

	// a selection alone selects
	device.SetDnsResolverSettings(&DnsResolverSettings{PresetId: DnsPresetIdDefault})
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), testingDnsResolverSettingsValue(testingTodaysDefaultSelection(false)))
}

// The same over the device rpc: a remote seeded from the old copy leaves the
// device on "default" with today's lists.
func TestStaleSelectionSeedsDeviceRemoteAsTheSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	deviceRemote.SetDnsResolverSettings(testingOlderReleaseDefaultSelection())
	today := testingDnsResolverSettingsValue(testingTodaysDefaultSelection(true))
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceLocal.GetDnsResolverSettings()), today)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceRemote.GetDnsResolverSettings()), today)
}

// Lists a user edited under "default" are custom, the edit as typed, through
// the app's copy, the device and the rpc.
func TestEditedDefaultListsStillLoadAsCustom(t *testing.T) {
	edited := testingTodaysDefaultSelection(false)
	edited.RemoteDohUrlsIpv4 = testing_stringList("https://192.0.2.80/dns-query", "https://1.1.1.1/dns-query")
	wantLists := testingDnsResolverSettingsValue(edited).Lists

	mirror := &LocalState{localStorageDir: t.TempDir()}
	if err := mirror.SetDnsResolverSettings(edited); err != nil {
		t.Fatal(err)
	}
	loaded := mirror.GetDnsResolverSettings()
	connect.AssertEqual(t, loaded.PresetId, "")
	connect.AssertEqual(t, testingDnsResolverSettingsValue(loaded).Lists, wantLists)

	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	device.SetDnsResolverSettings(edited)
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetId, "")
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()).Lists, wantLists)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	deviceRemote.SetDnsResolverSettings(edited)
	connect.AssertEqual(t, deviceLocal.GetDnsResolverSettings().PresetId, "")
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceLocal.GetDnsResolverSettings()).Lists, wantLists)
}
