package sdk

// DefaultServersVersion 3: DNS.SB joined the default server lists after
// Cloudflare and Quad9 (owner decision 2026-11-09), and the old kz
// recommendation, Google alone, loads as today's kz pick (owner decision
// 2026-11-09). A record written before version 3 that holds exactly an old
// default list loads with today's lists, chained from every earlier version;
// an edited list loads as saved and a reordered one keeps its order, both as
// Custom; an app's copy of "default" with the version 2 lists seeds a device as
// "default"; and a record that holds exactly an old Google-only kz
// recommendation, from any earlier version, selects "regional-kz", while every
// other list loads as saved. The tests drive the real record files, the
// DeviceLocal load path and the device rpc, with no network.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The default lists exactly as the sdk shipped them at version 2 (2026-11-08,
// after OpenDNS left them, until DNS.SB joined them), and so as an editor
// saved them.
var (
	testingVersion2RemoteDohUrlsIpv4 = []string{
		"https://1.1.1.1/dns-query",
		"https://9.9.9.10/dns-query",
		"https://1.0.0.1/dns-query",
	}
	testingVersion2RemoteDohUrlsIpv6 = []string{
		"https://[2606:4700:4700::1111]/dns-query",
		"https://[2620:fe::10]/dns-query",
		"https://[2606:4700:4700::1001]/dns-query",
	}
	testingVersion2RemoteDnsIpv4 = []string{"1.1.1.1", "9.9.9.10", "1.0.0.1"}
	testingVersion2RemoteDnsIpv6 = []string{"2606:4700:4700::1111", "2620:fe::10", "2606:4700:4700::1001"}
)

// Today's default lists, version 3: the version 2 lists, then DNS.SB's two
// servers.
var (
	testingVersion3RemoteDohUrlsIpv4 = []string{
		"https://1.1.1.1/dns-query",
		"https://9.9.9.10/dns-query",
		"https://1.0.0.1/dns-query",
		"https://185.222.222.222/dns-query",
		"https://45.11.45.11/dns-query",
	}
	testingVersion3RemoteDohUrlsIpv6 = []string{
		"https://[2606:4700:4700::1111]/dns-query",
		"https://[2620:fe::10]/dns-query",
		"https://[2606:4700:4700::1001]/dns-query",
		"https://[2a09::]/dns-query",
		"https://[2a11::]/dns-query",
	}
	testingVersion3RemoteDnsIpv4 = []string{"1.1.1.1", "9.9.9.10", "1.0.0.1", "185.222.222.222", "45.11.45.11"}
	testingVersion3RemoteDnsIpv6 = []string{"2606:4700:4700::1111", "2620:fe::10", "2606:4700:4700::1001", "2a09::", "2a11::"}
)

// What an editor at version 2 saved from the defaults, with the user's
// fallback choice.
func testingVersion2DefaultDnsResolverSettings(enableFallback bool) *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh:       true,
		EnableFallback:        enableFallback,
		DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     testing_stringList(testingVersion2RemoteDohUrlsIpv4...),
		RemoteDohUrlsIpv6:     testing_stringList(testingVersion2RemoteDohUrlsIpv6...),
		LocalDohUrlsIpv4:      testing_stringList(),
		LocalDohUrlsIpv6:      testing_stringList(),
		RemoteDnsIpv4:         testing_stringList(testingVersion2RemoteDnsIpv4...),
		RemoteDnsIpv6:         testing_stringList(testingVersion2RemoteDnsIpv6...),
		LocalDnsIpv4:          testing_stringList(),
		LocalDnsIpv6:          testing_stringList(),
	}
}

// A record exactly as the preset-aware sdk at version 2 wrote it (sdk
// 8604543d): the whole settings object with the selection, the fallback
// opt-in version, DefaultServersVersion 2 and PresetVersion 1.
func testingWritePresetEraVersion2DnsResolverSettings(t *testing.T, localStorageDir string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	data, err := json.Marshal(&struct {
		DnsResolverSettings
		FallbackOptInVersion  int `json:",omitempty"`
		DefaultServersVersion int `json:",omitempty"`
		PresetVersion         int `json:",omitempty"`
	}{
		DnsResolverSettings:   *dnsResolverSettings,
		FallbackOptInVersion:  1,
		DefaultServersVersion: 2,
		PresetVersion:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// The four remote lists equal today's version 3 lists exactly, in order.
func testingErrorNotVersion3Defaults(t *testing.T, what string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	for name, pair := range map[string][2][]string{
		"RemoteDohUrlsIpv4": {testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv4), testingVersion3RemoteDohUrlsIpv4},
		"RemoteDohUrlsIpv6": {testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv6), testingVersion3RemoteDohUrlsIpv6},
		"RemoteDnsIpv4":     {testingStringsOf(dnsResolverSettings.RemoteDnsIpv4), testingVersion3RemoteDnsIpv4},
		"RemoteDnsIpv6":     {testingStringsOf(dnsResolverSettings.RemoteDnsIpv6), testingVersion3RemoteDnsIpv6},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s %s loaded as %v, want %v", what, name, pair[0], pair[1])
		}
	}
}

// The default lists of every earlier version, in a record of that version,
// load as today's lists with DNS.SB, chained through every migration: the
// Google-era lists (version 0), the OpenDNS-era lists (version 1) and the
// version 2 lists. A record from before the presets loads as URnetwork's
// default with the fallback choice it had. A preset-aware version 2 record
// that chose Custom with the default lists keeps Custom, with today's lists.
func TestEveryOldDefaultListLoadsAsVersion3Defaults(t *testing.T) {
	for _, c := range []struct {
		name       string
		write      func(t *testing.T, localStorageDir string)
		wantPreset string
	}{
		{name: "Google era, version 0", wantPreset: DnsPresetIdDefault, write: func(t *testing.T, localStorageDir string) {
			testingWriteGoogleEraDnsResolverSettings(t, localStorageDir, testingGoogleEraDefaultDnsResolverSettings())
		}},
		{name: "OpenDNS era, version 1", wantPreset: DnsPresetIdDefault, write: func(t *testing.T, localStorageDir string) {
			testingWriteOpenDnsEraDnsResolverSettings(t, localStorageDir, testingOpenDnsEraDefaultDnsResolverSettings())
		}},
		{name: "version 2, before the presets", wantPreset: DnsPresetIdDefault, write: func(t *testing.T, localStorageDir string) {
			testingWriteVersion2DnsResolverSettings(t, localStorageDir, testingVersion2DefaultDnsResolverSettings(true))
		}},
		{name: "version 2, Custom with the default lists", wantPreset: "", write: func(t *testing.T, localStorageDir string) {
			testingWritePresetEraVersion2DnsResolverSettings(t, localStorageDir, testingVersion2DefaultDnsResolverSettings(true))
		}},
	} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		c.write(t, localState.localStorageDir)
		loaded := localState.GetDnsResolverSettings()
		if loaded == nil {
			t.Errorf("%s: the record did not load", c.name)
			continue
		}
		testingErrorNotVersion3Defaults(t, c.name, loaded)
		connect.AssertEqual(t, loaded.PresetId, c.wantPreset)
		connect.AssertEqual(t, loaded.EnableFallback, true)
		connect.AssertEqual(t, []bool{loaded.EnableRemoteDoh, loaded.EnableLocalDoh, loaded.EnableRemoteDns, loaded.EnableLocalDns}, []bool{true, false, false, false})
	}
}

// The composed path for v2 to v3: the device loads the version 2 record
// through the preference catalog and installs its resolver; the tunnel
// resolver and the host-side fallback both carry DNS.SB.
func TestVersion2DefaultDnsListsLoadWithDnsSbOnDevice(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, testingVersion2DefaultDnsResolverSettings(true))

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || dnsResolverSettings.PresetId != DnsPresetIdDefault || !dnsResolverSettings.EnableFallback {
		t.Fatalf("version 2 record did not load as the default on the device: %+v", dnsResolverSettings)
	}
	testingErrorNotVersion3Defaults(t, "device resolver", dnsResolverSettings)
	var resolver *connect.DnsResolverSettings
	var fallback *connect.DnsResolverSettings
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		resolver = device.upgradeMuxSettings.Dns.Resolver
		fallback = device.upgradeMuxSettings.Dns.Fallback
	}()
	if fallback == nil {
		t.Fatal("the user's fallback opt-in was lost")
	}
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv4, testingVersion3RemoteDohUrlsIpv4)
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv6, testingVersion3RemoteDohUrlsIpv6)
	for _, dnsSb := range []string{"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query", "https://[2a09::]/dns-query", "https://[2a11::]/dns-query"} {
		if !slices.Contains(slices.Concat(fallback.LocalDohUrlsIpv4, fallback.LocalDohUrlsIpv6), dnsSb) {
			t.Errorf("the host-side fallback lacks DNS.SB's %s", dnsSb)
		}
	}
}

// Every version 2 default list, in the default order and reordered, from a
// version 2 record, takes today's list: the saved order of its servers, then
// DNS.SB's two.
func TestVersion2DefaultListsGainDnsSbInTheirSavedOrder(t *testing.T) {
	cases := []struct {
		name  string
		list  string
		saved []string
		want  []string
	}{
		{name: "doh v4", list: "RemoteDohUrlsIpv4", saved: testingVersion2RemoteDohUrlsIpv4, want: testingVersion3RemoteDohUrlsIpv4},
		{name: "doh v6", list: "RemoteDohUrlsIpv6", saved: testingVersion2RemoteDohUrlsIpv6, want: testingVersion3RemoteDohUrlsIpv6},
		{name: "plain v4", list: "RemoteDnsIpv4", saved: testingVersion2RemoteDnsIpv4, want: testingVersion3RemoteDnsIpv4},
		{name: "plain v6", list: "RemoteDnsIpv6", saved: testingVersion2RemoteDnsIpv6, want: testingVersion3RemoteDnsIpv6},
		{name: "doh v4 reordered", list: "RemoteDohUrlsIpv4",
			saved: []string{"https://1.0.0.1/dns-query", "https://9.9.9.10/dns-query", "https://1.1.1.1/dns-query"},
			want: []string{"https://1.0.0.1/dns-query", "https://9.9.9.10/dns-query", "https://1.1.1.1/dns-query",
				"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query"}},
		{name: "plain v6 reordered", list: "RemoteDnsIpv6",
			saved: []string{"2620:fe::10", "2606:4700:4700::1001", "2606:4700:4700::1111"},
			want:  []string{"2620:fe::10", "2606:4700:4700::1001", "2606:4700:4700::1111", "2a09::", "2a11::"}},
		// the local plain lists hold the same servers in another order, and
		// take today's in theirs
		{name: "local plain v4", list: "LocalDnsIpv4",
			saved: []string{"9.9.9.10", "1.1.1.1", "1.0.0.1"},
			want:  []string{"9.9.9.10", "1.1.1.1", "1.0.0.1", "185.222.222.222", "45.11.45.11"}},
	}
	for _, c := range cases {
		settings := &DnsResolverSettings{EnableRemoteDoh: true}
		switch c.list {
		case "RemoteDohUrlsIpv4":
			settings.RemoteDohUrlsIpv4 = testing_stringList(c.saved...)
		case "RemoteDohUrlsIpv6":
			settings.RemoteDohUrlsIpv6 = testing_stringList(c.saved...)
		case "RemoteDnsIpv4":
			settings.RemoteDnsIpv4 = testing_stringList(c.saved...)
		case "RemoteDnsIpv6":
			settings.RemoteDnsIpv6 = testing_stringList(c.saved...)
		case "LocalDnsIpv4":
			settings.LocalDnsIpv4 = testing_stringList(c.saved...)
		}
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, settings)
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil {
			t.Errorf("%s: record did not load", c.name)
			continue
		}
		if got := testingDnsResolverSettingsLists(persisted)[c.list]; !slices.Equal(got, c.want) {
			t.Errorf("%s: %s loaded as %v, want %v", c.name, c.list, got, c.want)
		}
	}
}

// Edited version 2 default lists are a choice: a server added or removed loads
// as saved, and the record is Custom. Reordered default lists keep their order,
// with DNS.SB after them, and stay Custom too, as does the default with local
// plain dns turned on. A record this sdk writes never migrates: the version 2
// lists saved as Custom at version 3 load as saved.
func TestEditedOrReorderedVersion2DefaultListsStayCustom(t *testing.T) {
	// the edited list loads as saved; the record's other lists, still the
	// version 2 defaults, take today's
	added := testingVersion2DefaultDnsResolverSettings(false)
	added.RemoteDohUrlsIpv4 = testing_stringList(append(slices.Clone(testingVersion2RemoteDohUrlsIpv4), "https://192.0.2.53/dns-query")...)
	removed := testingVersion2DefaultDnsResolverSettings(false)
	removed.RemoteDnsIpv4 = testing_stringList("1.1.1.1", "1.0.0.1")
	for _, c := range []struct {
		name     string
		settings *DnsResolverSettings
		edited   string
	}{
		{name: "a server added", settings: added, edited: "RemoteDohUrlsIpv4"},
		{name: "a server removed", settings: removed, edited: "RemoteDnsIpv4"},
	} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, c.settings)
		loaded := localState.GetDnsResolverSettings()
		connect.AssertEqual(t, loaded.PresetId, "")
		lists := testingDnsResolverSettingsLists(loaded)
		connect.AssertEqual(t, lists[c.edited], testingDnsResolverSettingsLists(c.settings)[c.edited])
		today := map[string][]string{
			"RemoteDohUrlsIpv4": testingVersion3RemoteDohUrlsIpv4,
			"RemoteDohUrlsIpv6": testingVersion3RemoteDohUrlsIpv6,
			"RemoteDnsIpv4":     testingVersion3RemoteDnsIpv4,
			"RemoteDnsIpv6":     testingVersion3RemoteDnsIpv6,
		}
		for name, want := range today {
			if name != c.edited && !slices.Equal(lists[name], want) {
				t.Errorf("%s: the unedited %s loaded as %v, want %v", c.name, name, lists[name], want)
			}
		}
	}

	reordered := testingVersion2DefaultDnsResolverSettings(false)
	reordered.RemoteDohUrlsIpv4 = testing_stringList("https://1.0.0.1/dns-query", "https://9.9.9.10/dns-query", "https://1.1.1.1/dns-query")
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, reordered)
	loaded := localState.GetDnsResolverSettings()
	connect.AssertEqual(t, loaded.PresetId, "")
	connect.AssertEqual(t, testingStringsOf(loaded.RemoteDohUrlsIpv4), []string{
		"https://1.0.0.1/dns-query", "https://9.9.9.10/dns-query", "https://1.1.1.1/dns-query",
		"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query",
	})
	connect.AssertEqual(t, testingStringsOf(loaded.RemoteDnsIpv4), testingVersion3RemoteDnsIpv4)

	withLocalDns := testingVersion2DefaultDnsResolverSettings(false)
	withLocalDns.EnableLocalDns = true
	localState = &LocalState{localStorageDir: t.TempDir()}
	testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, withLocalDns)
	loaded = localState.GetDnsResolverSettings()
	connect.AssertEqual(t, loaded.PresetId, "")
	testingErrorNotVersion3Defaults(t, "local dns on", loaded)

	custom := testingVersion2DefaultDnsResolverSettings(false)
	localState = &LocalState{localStorageDir: t.TempDir()}
	if err := localState.SetDnsResolverSettings(custom); err != nil {
		t.Fatal(err)
	}
	loaded = localState.GetDnsResolverSettings()
	connect.AssertEqual(t, loaded.PresetId, "")
	connect.AssertEqual(t, testingDnsResolverSettingsValue(loaded).Lists, testingDnsResolverSettingsValue(custom).Lists)
}

// An app's copy of "default" from the version 2 release, its lists the
// version 2 defaults, is that release's derivation, not an edit: it seeds the
// app's own LocalState copy, a device and the rpc as "default" with today's
// lists, and its summary reads as URnetwork's default. A preset-aware version
// 2 record of the selection loads the same way. Before version 3 the edit rule
// knew no version 2 lists, so the copy dropped out of "default" into Custom.
func TestVersion2DefaultSelectionSeedsAsTheSelection(t *testing.T) {
	copyOfDefault := testingVersion2DefaultDnsResolverSettings(false)
	copyOfDefault.PresetId = DnsPresetIdDefault
	today := testingDnsResolverSettingsValue(testingTodaysDefaultSelection(false))

	summary := DescribeDnsResolverSettings(copyOfDefault, "de")
	connect.AssertEqual(t, []string{summary.Kind, summary.PresetId}, []string{DnsPickKindDefault, DnsPresetIdDefault})

	mirror := &LocalState{localStorageDir: t.TempDir()}
	if err := mirror.SetDnsResolverSettings(copyOfDefault); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, testingDnsResolverSettingsValue(mirror.GetDnsResolverSettings()), today)

	record := &LocalState{localStorageDir: t.TempDir()}
	testingWritePresetEraVersion2DnsResolverSettings(t, record.localStorageDir, copyOfDefault)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(record.GetDnsResolverSettings()), today)

	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	device.SetDnsResolverSettings(copyOfDefault)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), today)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	deviceRemote.SetDnsResolverSettings(copyOfDefault)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceLocal.GetDnsResolverSettings()), today)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceRemote.GetDnsResolverSettings()), today)
}

// The kz recommendation while it named Google alone, as an sdk applied it:
// remote plain dns alone, 8.8.8.8, and from 2026-09-11 Google's v6 address.
func testingGoogleKzRecommendation(ipv6 bool, enableFallback bool, mask string) *DnsResolverSettings {
	settings := &DnsResolverSettings{
		EnableRemoteDns:       true,
		EnableFallback:        enableFallback,
		DnsUpgradeMaskAddress: mask,
		RemoteDohUrlsIpv4:     testing_stringList(),
		RemoteDohUrlsIpv6:     testing_stringList(),
		LocalDohUrlsIpv4:      testing_stringList(),
		LocalDohUrlsIpv6:      testing_stringList(),
		RemoteDnsIpv4:         testing_stringList("8.8.8.8"),
		RemoteDnsIpv6:         testing_stringList(),
		LocalDnsIpv4:          testing_stringList(),
		LocalDnsIpv6:          testing_stringList(),
	}
	if ipv6 {
		settings.RemoteDnsIpv6 = testing_stringList("2001:4860:4860::8888")
	}
	return settings
}

// Each Google-only kz recommendation, v4 alone (2026-07-10 to 2026-09-11) and
// with its v6 address (2026-09-11 to 2026-11-08), in a record of any earlier
// version, loads as today's kz pick: "regional-kz", with the lists today's kz
// recommendation derives and no Google, and the fallback choice and mask it
// had. Version 0 is the Google era, version 1 the record a version 1 sdk wrote
// back as saved, and version 2 that of an sdk before the presets and of a
// preset-aware sdk, as Custom.
func TestGoogleKzRecommendationLoadsAsTodaysKzPick(t *testing.T) {
	writers := []struct {
		name  string
		write func(t *testing.T, localStorageDir string, settings *DnsResolverSettings)
	}{
		{name: "version 0", write: testingWriteGoogleEraDnsResolverSettings},
		{name: "version 1", write: testingWriteOpenDnsEraDnsResolverSettings},
		{name: "version 2", write: testingWriteVersion2DnsResolverSettings},
		{name: "version 2, preset-aware", write: testingWritePresetEraVersion2DnsResolverSettings},
	}
	variants := []struct {
		name           string
		ipv6           bool
		enableFallback bool
		mask           string
	}{
		{name: "v4 alone, no mask", ipv6: false, enableFallback: false, mask: ""},
		{name: "with v6", ipv6: true, enableFallback: true, mask: connect.DefaultDnsUpgradeMaskAddress},
	}
	for _, writer := range writers {
		for _, variant := range variants {
			name := writer.name + ", " + variant.name
			localState := &LocalState{localStorageDir: t.TempDir()}
			writer.write(t, localState.localStorageDir, testingGoogleKzRecommendation(variant.ipv6, variant.enableFallback, variant.mask))
			loaded := localState.GetDnsResolverSettings()
			if loaded == nil {
				t.Errorf("%s: the record did not load", name)
				continue
			}
			testingErrorGoogleDns(t, name, loaded)
			connect.AssertEqual(t, []string{loaded.PresetId, loaded.PresetPersonalId}, []string{"regional-kz", ""})
			connect.AssertEqual(t, []bool{loaded.EnableRemoteDoh, loaded.EnableLocalDoh, loaded.EnableRemoteDns, loaded.EnableLocalDns}, []bool{false, false, true, false})
			connect.AssertEqual(t, testingStringsOf(loaded.RemoteDnsIpv4), []string{"1.1.1.1", "9.9.9.10"})
			connect.AssertEqual(t, testingStringsOf(loaded.RemoteDnsIpv6), []string{"2606:4700:4700::1111", "2620:fe::10"})
			connect.AssertEqual(t, loaded.EnableFallback, variant.enableFallback)
			connect.AssertEqual(t, loaded.DnsUpgradeMaskAddress, connect.DefaultDnsUpgradeMaskAddress)
			summary := DescribeDnsResolverSettings(loaded, "kz")
			connect.AssertEqual(t, []string{summary.Kind, summary.PresetId}, []string{DnsPickKindRegional, "regional-kz"})
		}
	}
}

// The composed path: the device loads a Google era record holding the kz
// recommendation and installs today's kz pick, with no Google in the tunnel
// resolver.
func TestGoogleKzRecommendationLoadsAsTheKzPickOnDevice(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, testingGoogleKzRecommendation(true, false, connect.DefaultDnsUpgradeMaskAddress))

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || dnsResolverSettings.PresetId != "regional-kz" {
		t.Fatalf("the kz recommendation did not load as the kz pick on the device: %+v", dnsResolverSettings)
	}
	var resolver *connect.DnsResolverSettings
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		resolver = device.upgradeMuxSettings.Dns.Resolver
	}()
	connect.AssertEqual(t, resolver.EnableRemoteDns, true)
	connect.AssertEqual(t, resolver.RemoteDnsIpv4, connect.RegionalDnsServerIps("kz"))
	connect.AssertEqual(t, resolver.RemoteDnsIpv6, connect.RegionalDnsServerIpv6s("kz"))
	for _, entry := range slices.Concat(resolver.RemoteDohUrlsIpv4, resolver.RemoteDohUrlsIpv6, resolver.RemoteDnsIpv4, resolver.RemoteDnsIpv6, resolver.LocalDnsIpv4, resolver.LocalDnsIpv6) {
		if testingIsGoogleDns(entry) {
			t.Errorf("the device resolver names Google Public DNS: %s", entry)
		}
	}
}

// Anything but an exact Google-only kz recommendation loads as saved, Google
// included: another toggle on, the recommendation of February 2026 (local
// plain dns with Cloudflare, which no sdk saved), another server beside
// Google, Google's other address, a v6 list the user changed or alone, a DoH
// server of the user's, and the Google-only lists saved by this sdk, a choice
// made after the migration.
func TestGoogleKzListsOtherwiseLoadAsSaved(t *testing.T) {
	withRemoteDoh := testingGoogleKzRecommendation(true, false, "")
	withRemoteDoh.EnableRemoteDoh = true
	withLocalDns := testingGoogleKzRecommendation(true, false, "")
	withLocalDns.EnableLocalDns = true
	february := testingGoogleKzRecommendation(false, false, "")
	february.EnableLocalDns = true
	february.LocalDnsIpv4 = testing_stringList("1.1.1.1")
	besideAnother := testingGoogleKzRecommendation(true, false, "")
	besideAnother.RemoteDnsIpv4 = testing_stringList("8.8.8.8", "192.0.2.53")
	otherGoogle := testingGoogleKzRecommendation(false, false, "")
	otherGoogle.RemoteDnsIpv4 = testing_stringList("8.8.4.4")
	changedIpv6 := testingGoogleKzRecommendation(true, false, "")
	changedIpv6.RemoteDnsIpv6 = testing_stringList("2001:4860:4860::8844")
	ipv6Alone := testingGoogleKzRecommendation(true, false, "")
	ipv6Alone.RemoteDnsIpv4 = testing_stringList()
	withDohServer := testingGoogleKzRecommendation(true, false, "")
	withDohServer.RemoteDohUrlsIpv4 = testing_stringList("https://192.0.2.80/dns-query")
	for _, c := range []struct {
		name     string
		settings *DnsResolverSettings
		current  bool
	}{
		{name: "remote doh on too", settings: withRemoteDoh},
		{name: "local plain dns on too", settings: withLocalDns},
		{name: "the february recommendation", settings: february},
		{name: "beside another server", settings: besideAnother},
		{name: "google's other address", settings: otherGoogle},
		{name: "a changed v6 list", settings: changedIpv6},
		{name: "v6 alone", settings: ipv6Alone},
		{name: "beside a doh server", settings: withDohServer},
		{name: "saved by this sdk", settings: testingGoogleKzRecommendation(true, false, ""), current: true},
	} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		if c.current {
			if err := localState.SetDnsResolverSettings(c.settings); err != nil {
				t.Fatal(err)
			}
		} else {
			testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
		}
		loaded := localState.GetDnsResolverSettings()
		if loaded == nil {
			t.Errorf("%s: the record did not load", c.name)
			continue
		}
		if loaded.PresetId != "" {
			t.Errorf("%s: loaded as %q, want Custom", c.name, loaded.PresetId)
		}
		saved := testingDnsResolverSettingsLists(c.settings)
		for name, list := range testingDnsResolverSettingsLists(loaded) {
			if !slices.Equal(list, saved[name]) {
				t.Errorf("%s: %s loaded as %v, want %v as saved", c.name, name, list, saved[name])
			}
		}
	}
}
