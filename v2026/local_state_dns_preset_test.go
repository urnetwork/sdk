package sdk

// The dns selection in the saved record (connect ADBLOCK3RD E2, E3, E4, E5,
// E6, A7): PresetVersion and the migration of records from before the
// presets, on top of the DefaultServersVersion migrations; the selection's
// round trip, an older sdk's view of the record, a dropped preset, the
// refusals and the hosted device. The tests drive the real record files and
// the DeviceLocal load path, with no network.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A record exactly as an sdk before the presets wrote it at
// DefaultServersVersion 2: the whole settings object and both versions, with
// no selection.
func testingWriteVersion2DnsResolverSettings(t *testing.T, localStorageDir string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	legacy := CopyDnsResolverSettings(dnsResolverSettings)
	data, err := json.Marshal(&struct {
		EnableRemoteDoh       bool
		EnableLocalDoh        bool
		EnableRemoteDns       bool
		EnableLocalDns        bool
		EnableFallback        bool
		DnsUpgradeMaskAddress string
		RemoteDohUrlsIpv4     *StringList
		RemoteDohUrlsIpv6     *StringList
		LocalDohUrlsIpv4      *StringList
		LocalDohUrlsIpv6      *StringList
		RemoteDnsIpv4         *StringList
		RemoteDnsIpv6         *StringList
		LocalDnsIpv4          *StringList
		LocalDnsIpv6          *StringList
		FallbackOptInVersion  int
		DefaultServersVersion int
	}{
		EnableRemoteDoh:       legacy.EnableRemoteDoh,
		EnableLocalDoh:        legacy.EnableLocalDoh,
		EnableRemoteDns:       legacy.EnableRemoteDns,
		EnableLocalDns:        legacy.EnableLocalDns,
		EnableFallback:        legacy.EnableFallback,
		DnsUpgradeMaskAddress: legacy.DnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     legacy.RemoteDohUrlsIpv4,
		RemoteDohUrlsIpv6:     legacy.RemoteDohUrlsIpv6,
		LocalDohUrlsIpv4:      legacy.LocalDohUrlsIpv4,
		LocalDohUrlsIpv6:      legacy.LocalDohUrlsIpv6,
		RemoteDnsIpv4:         legacy.RemoteDnsIpv4,
		RemoteDnsIpv6:         legacy.RemoteDnsIpv6,
		LocalDnsIpv4:          legacy.LocalDnsIpv4,
		LocalDnsIpv6:          legacy.LocalDnsIpv6,
		FallbackOptInVersion:  1,
		DefaultServersVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// The settings as a comparable value, an absent list the same as an empty
// one, as the record and the apps read them.
type testingDnsResolverSettingsComparable struct {
	PresetId              string
	PresetPersonalId      string
	EnableRemoteDoh       bool
	EnableLocalDoh        bool
	EnableRemoteDns       bool
	EnableLocalDns        bool
	EnableFallback        bool
	DnsUpgradeMaskAddress string
	Lists                 map[string][]string
}

func testingDnsResolverSettingsValue(settings *DnsResolverSettings) *testingDnsResolverSettingsComparable {
	if settings == nil {
		return nil
	}
	lists := map[string][]string{}
	for name, list := range testingDnsResolverSettingsLists(settings) {
		lists[name] = append([]string{}, list...)
	}
	return &testingDnsResolverSettingsComparable{
		PresetId:              settings.PresetId,
		PresetPersonalId:      settings.PresetPersonalId,
		EnableRemoteDoh:       settings.EnableRemoteDoh,
		EnableLocalDoh:        settings.EnableLocalDoh,
		EnableRemoteDns:       settings.EnableRemoteDns,
		EnableLocalDns:        settings.EnableLocalDns,
		EnableFallback:        settings.EnableFallback,
		DnsUpgradeMaskAddress: settings.DnsUpgradeMaskAddress,
		Lists:                 lists,
	}
}

// The current default lists, as an editor before the presets saved them.
func testingCurrentDefaultsAsSaved(enableFallback bool) *DnsResolverSettings {
	settings := DnsResolverSettingsAsCustom(GetDefaultDnsResolverSettings())
	settings.EnableFallback = enableFallback
	return settings
}

// A record from before the presets that holds the default lists of its time,
// in any era, loads as URnetwork's default with exactly the lists it migrated
// to before, and the fallback choice it had (E4).
func TestPresetVersionMigrationLoadsDefaultListsAsDefault(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(t *testing.T, localStorageDir string)
	}{
		{name: "Google era, version 0", write: func(t *testing.T, localStorageDir string) {
			testingWriteGoogleEraDnsResolverSettings(t, localStorageDir, testingGoogleEraDefaultDnsResolverSettings())
		}},
		{name: "OpenDNS era, version 1", write: func(t *testing.T, localStorageDir string) {
			testingWriteOpenDnsEraDnsResolverSettings(t, localStorageDir, testingOpenDnsEraDefaultDnsResolverSettings())
		}},
		{name: "Quad9 unfiltered era, version 2", write: func(t *testing.T, localStorageDir string) {
			testingWriteVersion2DnsResolverSettings(t, localStorageDir, testingVersion2DefaultDnsResolverSettings(true))
		}},
	} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		c.write(t, localState.localStorageDir)
		loaded := localState.GetDnsResolverSettings()
		if loaded == nil {
			t.Fatalf("%s: the record did not load", c.name)
		}
		connect.AssertEqual(t, loaded.PresetId, DnsPresetIdDefault)
		connect.AssertEqual(t, loaded.PresetPersonalId, "")
		connect.AssertEqual(t, loaded.EnableFallback, true)
		testingErrorNotCurrentDefaults(t, c.name, loaded)
		// it resolves exactly as the lists it migrated to
		connect.AssertEqual(t, loaded.toConnect(), DnsResolverSettingsAsCustom(loaded).customToConnect())
	}
}

// Every other record from before the presets loads as custom, its lists as
// the DefaultServersVersion migrations left them: a near-default list, the
// defaults reordered, a regional recommendation and a user's own servers.
func TestPresetVersionMigrationKeepsCustomRecords(t *testing.T) {
	nearDefault := testingCurrentDefaultsAsSaved(false)
	nearDefault.RemoteDohUrlsIpv4 = testing_stringList(nearDefault.RemoteDohUrlsIpv4.getAll()[1:]...)
	reordered := testingCurrentDefaultsAsSaved(false)
	reorderedUrls := reordered.RemoteDohUrlsIpv4.getAll()
	slices.Reverse(reorderedUrls)
	reordered.RemoteDohUrlsIpv4 = testing_stringList(reorderedUrls...)
	withLocalDns := testingCurrentDefaultsAsSaved(false)
	withLocalDns.EnableLocalDns = true
	for _, settings := range []*DnsResolverSettings{
		nearDefault,
		reordered,
		withLocalDns,
		DnsResolverSettingsAsCustom(GetRecommendedDnsResolverSettings("cn")),
		{EnableRemoteDoh: true, RemoteDohUrlsIpv4: testing_stringList("https://192.0.2.80/dns-query")},
	} {
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, settings)
		loaded := localState.GetDnsResolverSettings()
		connect.AssertEqual(t, loaded.PresetId, "")
		connect.AssertEqual(t, testingDnsResolverSettingsValue(loaded).Lists, testingDnsResolverSettingsValue(settings).Lists)
		connect.AssertEqual(t, loaded.toConnect(), loaded.customToConnect())
	}
	// a version 1 kz recommendation still migrates to the current one, as custom
	kz := &DnsResolverSettings{
		EnableRemoteDns: true,
		RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9"),
		RemoteDnsIpv6:   testing_stringList("2606:4700:4700::1111", "2620:fe::fe"),
	}
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, kz)
	loaded := localState.GetDnsResolverSettings()
	connect.AssertEqual(t, loaded.PresetId, "")
	connect.AssertEqual(t, testingStringsOf(loaded.RemoteDnsIpv4), connect.RegionalDnsServerIps("kz"))
}

// A record this sdk wrote never migrates: custom settings that happen to
// equal the defaults stay custom, as chosen.
func TestPresetVersionRecordAtTheCurrentVersionNeverMigrates(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	custom := testingCurrentDefaultsAsSaved(false)
	if err := localState.SetDnsResolverSettings(custom); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(localState.localStorageDir, ".dns_resolver_settings"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"PresetVersion":1`) {
		t.Fatalf("the record carries no PresetVersion: %s", data)
	}
	connect.AssertEqual(t, localState.GetDnsResolverSettings().PresetId, "")
}

// The record keeps the selection and the derived lists; custom settings with
// every field filled come back field for field, and a selection comes back
// with its ids and fallback choice and the lists it derives today (E3).
func TestPresetRecordRoundTrip(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	localState := &LocalState{localStorageDir: t.TempDir()}
	selected := DnsResolverSettingsWithFallback(testingPresetSettings(t, "gamma-personal", "abc123"), true)
	if err := localState.SetDnsResolverSettings(selected); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, testingDnsResolverSettingsValue(localState.GetDnsResolverSettings()), testingDnsResolverSettingsValue(selected))

	codec := func(settings *DnsResolverSettings) *DnsResolverSettings {
		t.Helper()
		data, err := encodeDnsResolverSettingsRecord(settings)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeDnsResolverSettingsRecord(data)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	seed := 0
	filled := &DnsResolverSettings{}
	fillNonZero(t, reflect.ValueOf(filled), &seed)
	selection := CopyDnsResolverSettings(filled)
	filled.PresetId = ""
	connect.AssertEqual(t, testingDnsResolverSettingsValue(codec(filled)), testingDnsResolverSettingsValue(filled))
	// an id this catalog never knew loads as stored, applying the defaults
	want := ownDnsResolverSettings(selection)
	connect.AssertEqual(t, []string{want.PresetId, want.PresetPersonalId}, []string{selection.PresetId, selection.PresetPersonalId})
	connect.AssertEqual(t, testingDnsResolverSettingsValue(codec(selection)), testingDnsResolverSettingsValue(want))
}

// An older sdk ignores the selection and reads the lists, which are the
// derived ones (E5): DoH alone to the preset's url, with no plain dns, so a
// hostname preset, whose pins that sdk lacks, fails closed rather than look
// its host up in plaintext. An ip-literal preset keeps working.
func TestPresetRecordAsAnOlderSdkReadsIt(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	for _, c := range []struct {
		presetId string
		dohUrls  []string
	}{
		{presetId: "alpha-ads", dohUrls: []string{"https://dns.alpha.example/dns-query"}},
		{presetId: "beta-security", dohUrls: []string{"https://192.0.2.10/dns-query", "https://198.51.100.10/dns-query"}},
	} {
		data, err := encodeDnsResolverSettingsRecord(testingPresetSettings(t, c.presetId, ""))
		if err != nil {
			t.Fatal(err)
		}
		var older struct {
			EnableRemoteDoh   bool
			EnableLocalDoh    bool
			EnableRemoteDns   bool
			EnableLocalDns    bool
			RemoteDohUrlsIpv4 []string
			RemoteDnsIpv4     []string
			RemoteDnsIpv6     []string
		}
		if err := json.Unmarshal(data, &older); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, older.EnableRemoteDoh, true)
		connect.AssertEqual(t, older.EnableLocalDoh || older.EnableRemoteDns || older.EnableLocalDns, false)
		connect.AssertEqual(t, older.RemoteDohUrlsIpv4, c.dohUrls)
		connect.AssertEqual(t, len(older.RemoteDnsIpv4)+len(older.RemoteDnsIpv6), 0)
	}
}

// A device keeps its selection across a reload, deriving the lists again,
// with its preset's failure policy (E2, decision 3).
func TestDeviceLocalPresetSelectionSurvivesReload(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	device.SetDnsResolverSettings(testingPresetSettings(t, "gamma-personal", "https://id.gamma.example/ABC123"))
	testingJoinPreferenceDevice(t, device)

	reloaded := testing_loadedBlockDevice(t, fixture.networkSpace)
	settings := reloaded.GetDnsResolverSettings()
	connect.AssertEqual(t, []string{settings.PresetId, settings.PresetPersonalId}, []string{"gamma-personal", "abc123"})
	connect.AssertEqual(t, settings.RemoteDohUrlsIpv4.getAll(), []string{"https://id.gamma.example/abc123"})
	reloaded.stateLock.Lock()
	dns := reloaded.upgradeMuxSettings.Dns
	reloaded.stateLock.Unlock()
	connect.AssertEqual(t, dns.Resolver.DohServerHostAddrs, map[string][]string{"id.gamma.example": {"203.0.113.1", "2001:db8:2::1"}})
	if dns.Failover != nil || !dns.FallbackUntilTunnelReady {
		t.Fatalf("a personal preset reloaded with failover %+v, until ready %t", dns.Failover, dns.FallbackUntilTunnelReady)
	}
}

// A record whose preset a release dropped applies URnetwork's default, keeps
// the stored id for the notice that names the provider, and dismissing the
// notice saves "default" (A7, O8).
func TestDeviceLocalDroppedPresetAppliesDefaultUntilDismissed(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	stored := &DnsResolverSettings{
		PresetId:          "retired-preset",
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: testing_stringList("https://dns.retired.example/dns-query"),
		RemoteDohUrlsIpv6: testing_stringList("https://dns.retired.example/dns-query"),
	}
	if err := localState.SetDnsResolverSettings(stored); err != nil {
		t.Fatal(err)
	}
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	settings := device.GetDnsResolverSettings()
	connect.AssertEqual(t, settings.PresetId, "retired-preset")
	testingErrorNotCurrentDefaults(t, "the dropped preset's device", settings)
	device.stateLock.Lock()
	resolver := device.upgradeMuxSettings.Dns.Resolver
	device.stateLock.Unlock()
	connect.AssertEqual(t, resolver, (&DnsResolverSettings{PresetId: DnsPresetIdDefault}).toConnect())
	summary := DescribeDnsResolverSettings(settings, "")
	connect.AssertEqual(t,
		[]any{summary.Kind, summary.Dropped, summary.DroppedNameKey, summary.DroppedNameProvider, summary.DroppedProviderName},
		[]any{DnsPickKindDefault, true, "dns_preset_name_ad_blocking", "Retired", "Retired DNS"})

	// a Fast DNS toggle keeps the notice
	device.SetDnsResolverSettings(DnsResolverSettingsWithFallback(settings, true))
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetId, "retired-preset")
	// dismissing it saves default
	device.SetDnsResolverSettings(DnsResolverSettingsWithPreset(device.GetDnsResolverSettings(), DnsPresetIdDefault, ""))
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetId, DnsPresetIdDefault)
	testingJoinPreferenceDevice(t, device)
	reloaded := testing_loadedBlockDevice(t, fixture.networkSpace)
	connect.AssertEqual(t, DescribeDnsResolverSettings(reloaded.GetDnsResolverSettings(), "").Dropped, false)
	connect.AssertEqual(t, reloaded.GetDnsResolverSettings().EnableFallback, true)
}

// An unknown preset id or a personal id that does not validate is neither
// saved nor applied, and the previous resolver stays (E2).
func TestDeviceLocalRefusesUnknownPresetAndInvalidPersonalId(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	device.SetDnsResolverSettings(testingPresetSettings(t, "alpha-ads", ""))
	recordPath := filepath.Join(fixture.networkSpace.GetAsyncLocalState().GetLocalState().localStorageDir, ".dns_resolver_settings")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	applied := device.GetDnsResolverSettings()
	for _, refused := range []*DnsResolverSettings{
		{PresetId: "no-such-preset", EnableRemoteDoh: true},
		{PresetId: "gamma-personal"},
		{PresetId: "gamma-personal", PresetPersonalId: "abc12/"},
		{PresetId: "gamma-personal", PresetPersonalId: "https://dns.alpha.example/abc123"},
	} {
		device.SetDnsResolverSettings(refused)
		connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), testingDnsResolverSettingsValue(applied))
		after, err := os.ReadFile(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, string(after), string(before))
	}
}

// A hosted device takes no resolver settings, a preset like any other, and
// has no resolver status (E6).
func TestHostedDeviceRefusesPreset(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	hosted := &DeviceLocal{
		settings:           &DeviceLocalSettings{HostedIncompatible: true},
		log:                connect.NewNoopLogger(),
		upgradeMuxSettings: connect.DefaultUpgradeMuxSettings(),
		dnsResolver:        newDnsResolverStatusTracker(),
	}
	notify, err := hosted.applyDnsResolverSettingsWithLock(testingPresetSettings(t, "alpha-ads", ""))
	if notify != nil || err != nil {
		t.Fatalf("a hosted device applied a preset: %v", err)
	}
	connect.AssertEqual(t, hosted.GetDnsResolverSettings().PresetId, DnsPresetIdDefault)
	connect.AssertEqual(t, hosted.upgradeMuxSettings.Dns.Resolver, connect.DefaultUpgradeMuxSettings().Dns.Resolver)
	connect.AssertEqual(t, hosted.GetDnsResolverStatus().State, DnsResolverStateUnknown)
	connect.AssertEqual(t, hosted.GetDnsBlockedCount(), int64(0))
}

// Settings set on a device come back field for field: custom settings with
// every field filled, and a personal selection (E3).
func TestDeviceLocalDnsResolverSettingsRoundTrip(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	filled := testingFilledCustomDnsResolverSettings(t)
	device.SetDnsResolverSettings(filled)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), testingDnsResolverSettingsValue(filled))
	selected := DnsResolverSettingsWithFallback(testingPresetSettings(t, "gamma-personal", "abc123"), true)
	device.SetDnsResolverSettings(selected)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(device.GetDnsResolverSettings()), testingDnsResolverSettingsValue(selected))
	testingJoinPreferenceDevice(t, device)
	reloaded := testing_loadedBlockDevice(t, fixture.networkSpace)
	connect.AssertEqual(t, testingDnsResolverSettingsValue(reloaded.GetDnsResolverSettings()), testingDnsResolverSettingsValue(selected))
}

// The space's own names keep their ingress dns whatever the device selects:
// a preset never reaches the control-name resolver, so a filter can never
// affect the platform's names (N2, 6.3).
func TestDnsPresetLeavesIngressUntouched(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	strategy := fixture.networkSpace.clientStrategy
	before := strategy.DohSettings()
	beforeResolver := cloneConnectDnsResolverSettings(before.DnsResolverSettings)
	beforeIngress := before.IngressCountryCode
	for _, pick := range GetDnsPicks("cn").getAll() {
		var settings *DnsResolverSettings
		switch pick.Kind {
		case DnsPickKindCustom:
			settings = DnsResolverSettingsAsCustom(device.GetDnsResolverSettings())
		default:
			personalId := ""
			if pick.Preset != nil && pick.Preset.PersonalIdRequired {
				personalId = pick.Preset.PersonalIdExample
			}
			settings = DnsResolverSettingsWithPreset(device.GetDnsResolverSettings(), pick.PresetId, personalId)
		}
		device.SetDnsResolverSettings(settings)
		connect.AssertEqual(t, device.GetDnsResolverSettings().PresetId, pick.PresetId)
		after := strategy.DohSettings()
		if after != before || after.IngressCountryCode != beforeIngress {
			t.Fatalf("selecting %q replaced the ingress dns settings", pick.PresetId)
		}
		connect.AssertEqual(t, cloneConnectDnsResolverSettings(after.DnsResolverSettings), beforeResolver)
		if after.DnsResolverSettings.DohServerHostAddrs != nil {
			t.Fatalf("selecting %q pinned an ingress host", pick.PresetId)
		}
	}
}
