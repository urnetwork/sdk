package sdk

// DefaultServersVersion 4: connect changed the ir, tr and tm regional
// recommendations on 2026-11-11. ir names Shecan and Electro instead of
// Shecan and 403.online's private 10.202.10.10; tr, whose TTNET and Turkcell
// Superonline resolvers answer only their own customers and where DoH works,
// has none; and tm has none. A record below version 4 that holds an
// earlier recommendation exactly as Custom, which an sdk before the presets
// applied and a preset-aware sdk kept, loads as its country's pick; an app's
// copy of a regional pick with an earlier derivation seeds the app's own
// LocalState, a device and the rpc as the pick; and every other list loads as
// saved. The tests drive the real record files, the DeviceLocal load path and
// the device rpc, with no network.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

// An earlier regional recommendation as an sdk applied it: remote plain dns
// alone with these servers, every other list empty.
func testingEarlierRegionalRecommendation(ipv4 []string, ipv6 []string, enableFallback bool, mask string) *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDns:       true,
		EnableFallback:        enableFallback,
		DnsUpgradeMaskAddress: mask,
		RemoteDohUrlsIpv4:     testing_stringList(),
		RemoteDohUrlsIpv6:     testing_stringList(),
		LocalDohUrlsIpv4:      testing_stringList(),
		LocalDohUrlsIpv6:      testing_stringList(),
		RemoteDnsIpv4:         testing_stringList(ipv4...),
		RemoteDnsIpv6:         testing_stringList(ipv6...),
		LocalDnsIpv4:          testing_stringList(),
		LocalDnsIpv6:          testing_stringList(),
	}
}

// A record exactly as the preset-aware sdk at version 3 wrote it: the whole
// settings object with the selection, the fallback opt-in version,
// DefaultServersVersion 3 and PresetVersion 1.
func testingWritePresetEraVersion3DnsResolverSettings(t *testing.T, localStorageDir string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	data, err := json.Marshal(&struct {
		DnsResolverSettings
		FallbackOptInVersion  int `json:",omitempty"`
		DefaultServersVersion int `json:",omitempty"`
		PresetVersion         int `json:",omitempty"`
	}{
		DnsResolverSettings:   *dnsResolverSettings,
		FallbackOptInVersion:  1,
		DefaultServersVersion: 3,
		PresetVersion:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// The earlier recommendations by country, each with the record versions that
// can hold it as the recommendation, as an sdk applied it.
var testingEarlierRegionalRecommendations = []struct {
	name        string
	countryCode string
	ipv4        []string
	ipv6        []string
	versions    []int
}{
	{name: "ir with 403.online", countryCode: "ir", ipv4: []string{"178.22.122.100", "10.202.10.10"}, versions: []int{0, 1, 2, 3}},
	{name: "ir reordered", countryCode: "ir", ipv4: []string{"10.202.10.10", "178.22.122.100"}, versions: []int{0, 1, 2, 3}},
	{name: "tr isp resolvers", countryCode: "tr", ipv4: []string{"195.175.39.39", "212.252.114.8"}, versions: []int{0, 1, 2, 3}},
	{name: "tm with google", countryCode: "tm", ipv4: []string{"217.174.238.141", "8.8.4.4"}, versions: []int{0}},
	{name: "tm with google, dual stack", countryCode: "tm", ipv4: []string{"217.174.238.141", "8.8.4.4"}, ipv6: []string{"2001:4860:4860::8844"}, versions: []int{0}},
	{name: "tm alone", countryCode: "tm", ipv4: []string{"217.174.238.141"}, versions: []int{1, 2, 3}},
}

// The record writers of each earlier version: version 0 is the Google era, 1
// the OpenDNS era, 2 an sdk before the presets and a preset-aware one, and 3
// a preset-aware one, the last two as Custom.
func testingEarlierRecordWriters(version int) []func(t *testing.T, localStorageDir string, settings *DnsResolverSettings) {
	switch version {
	case 0:
		return []func(t *testing.T, localStorageDir string, settings *DnsResolverSettings){testingWriteGoogleEraDnsResolverSettings}
	case 1:
		return []func(t *testing.T, localStorageDir string, settings *DnsResolverSettings){testingWriteOpenDnsEraDnsResolverSettings}
	case 2:
		return []func(t *testing.T, localStorageDir string, settings *DnsResolverSettings){testingWriteVersion2DnsResolverSettings, testingWritePresetEraVersion2DnsResolverSettings}
	default:
		return []func(t *testing.T, localStorageDir string, settings *DnsResolverSettings){testingWritePresetEraVersion3DnsResolverSettings}
	}
}

// Whether two settings values are equal in every field and list.
func testingDnsResolverSettingsComparableEqual(a *testingDnsResolverSettingsComparable, b *testingDnsResolverSettingsComparable) bool {
	return reflect.DeepEqual(a, b)
}

// What today's pick of a country derives: the country's recommendation, or
// URnetwork's default for a country that has none, with the mask.
func testingTodaysRegionalPick(countryCode string, enableFallback bool) *testingDnsResolverSettingsComparable {
	pick := &DnsResolverSettings{PresetId: DnsPresetIdRegionalPrefix + countryCode, EnableFallback: enableFallback}
	return testingDnsResolverSettingsValue(ownDnsResolverSettings(pick))
}

// Each earlier ir, tr and tm recommendation, in either order, in a record of
// every version that can hold it as the recommendation, loads as today's pick
// of its country with the fallback choice and mask it had: ir with today's
// list, and tr and tm, which have no recommendation, with URnetwork's default
// and the summary saying the pick dropped out, never with remote plain dns on
// and no server. Before version 4 they loaded as Custom with the servers that
// cannot answer an exit.
func TestEarlierRegionalRecommendationsLoadAsTodaysPick(t *testing.T) {
	if got := connect.RegionalDnsServerIps("ir"); !slices.Equal(got, []string{"178.22.122.100", "78.157.42.100"}) {
		t.Fatalf("connect's ir recommendation = %v", got)
	}
	for _, countryCode := range []string{"tr", "tm"} {
		if got := connect.RegionalDnsResolverSettings(countryCode); got != nil {
			t.Fatalf("connect recommends %+v for %s, want none", got, countryCode)
		}
	}
	for _, recommendation := range testingEarlierRegionalRecommendations {
		for _, version := range recommendation.versions {
			for i, write := range testingEarlierRecordWriters(version) {
				for _, enableFallback := range []bool{false, true} {
					name := recommendation.name
					localState := &LocalState{localStorageDir: t.TempDir()}
					write(t, localState.localStorageDir, testingEarlierRegionalRecommendation(recommendation.ipv4, recommendation.ipv6, enableFallback, connect.DefaultDnsUpgradeMaskAddress))
					loaded := localState.GetDnsResolverSettings()
					if loaded == nil {
						t.Errorf("%s, version %d writer %d: the record did not load", name, version, i)
						continue
					}
					if got, want := testingDnsResolverSettingsValue(loaded), testingTodaysRegionalPick(recommendation.countryCode, enableFallback); !testingDnsResolverSettingsComparableEqual(got, want) {
						t.Errorf("%s, version %d writer %d, fallback %t: loaded as %+v, want today's pick %+v", name, version, i, enableFallback, got, want)
					}
					testingErrorGoogleDns(t, name, loaded)
					if loaded.EnableRemoteDns && len(testingStringsOf(loaded.RemoteDnsIpv4)) == 0 && len(testingStringsOf(loaded.RemoteDnsIpv6)) == 0 {
						t.Errorf("%s, version %d: loaded with remote plain dns on and no server", name, version)
					}
					summary := DescribeDnsResolverSettings(loaded, recommendation.countryCode)
					if connect.RegionalDnsResolverSettings(recommendation.countryCode) == nil {
						if summary.Kind != DnsPickKindDefault || !summary.Dropped || summary.DroppedPresetId != DnsPresetIdRegionalPrefix+recommendation.countryCode {
							t.Errorf("%s, version %d: summary = %+v, want the default with the %s pick dropped", name, version, summary, recommendation.countryCode)
						}
					} else if summary.Kind != DnsPickKindRegional || summary.PresetId != DnsPresetIdRegionalPrefix+recommendation.countryCode {
						t.Errorf("%s, version %d: summary = %+v, want the %s pick", name, version, summary, recommendation.countryCode)
					}
				}
			}
		}
	}
}

// The composed path: the device loads a record that holds tr's earlier
// recommendation as the tr pick, which applies URnetwork's default, as tr has
// no recommendation: the tunnel resolver is the default one, with remote DoH
// on and neither TTNET's nor Superonline's resolver, not remote plain dns
// with no server.
func TestEarlierTrRecommendationLoadsAsTheDefaultOnDevice(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, testingEarlierRegionalRecommendation([]string{"195.175.39.39", "212.252.114.8"}, nil, false, connect.DefaultDnsUpgradeMaskAddress))

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || dnsResolverSettings.PresetId != "regional-tr" {
		t.Fatalf("the earlier tr recommendation did not load as the tr pick on the device: %+v", dnsResolverSettings)
	}
	var resolver *connect.DnsResolverSettings
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		resolver = device.upgradeMuxSettings.Dns.Resolver
	}()
	defaults := defaultDnsResolver()
	connect.AssertEqual(t, resolver.EnableRemoteDoh, defaults.EnableRemoteDoh)
	connect.AssertEqual(t, resolver.EnableRemoteDns, defaults.EnableRemoteDns)
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv4, defaults.RemoteDohUrlsIpv4)
	connect.AssertEqual(t, resolver.RemoteDnsIpv4, defaults.RemoteDnsIpv4)
	connect.AssertEqual(t, resolver.RemoteDnsIpv6, defaults.RemoteDnsIpv6)
	for _, entry := range slices.Concat(resolver.RemoteDnsIpv4, resolver.RemoteDnsIpv6, resolver.LocalDnsIpv4, resolver.LocalDnsIpv6) {
		if entry == "195.175.39.39" || entry == "212.252.114.8" {
			t.Errorf("the device resolver names an isp resolver: %s", entry)
		}
	}
}

// An app's copy of a regional pick whose lists an earlier release derived is
// that release's derivation, not an edit: it seeds the app's own LocalState,
// a device and the rpc as the pick with today's lists (tm: URnetwork's
// default, the pick kept). Before version 4 the edit rule knew none of these
// lists, so the copy dropped out of the pick into Custom with the servers
// that cannot answer an exit.
func TestEarlierRegionalDerivationSeedsAsThePick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	for _, recommendation := range testingEarlierRegionalRecommendations {
		copyOfPick := testingEarlierRegionalRecommendation(recommendation.ipv4, recommendation.ipv6, false, connect.DefaultDnsUpgradeMaskAddress)
		copyOfPick.PresetId = DnsPresetIdRegionalPrefix + recommendation.countryCode
		today := testingTodaysRegionalPick(recommendation.countryCode, false)

		if edited := dnsResolverSettingsAsEdited(copyOfPick); edited.PresetId != copyOfPick.PresetId {
			t.Errorf("%s: the copy reads as an edit: %+v", recommendation.name, edited)
		}

		mirror := &LocalState{localStorageDir: t.TempDir()}
		if err := mirror.SetDnsResolverSettings(copyOfPick); err != nil {
			t.Fatal(err)
		}
		if got := testingDnsResolverSettingsValue(mirror.GetDnsResolverSettings()); !testingDnsResolverSettingsComparableEqual(got, today) {
			t.Errorf("%s: the app's LocalState holds %+v, want %+v", recommendation.name, got, today)
		}

		device.SetDnsResolverSettings(copyOfPick)
		if got := testingDnsResolverSettingsValue(device.GetDnsResolverSettings()); !testingDnsResolverSettingsComparableEqual(got, today) {
			t.Errorf("%s: the device holds %+v, want %+v", recommendation.name, got, today)
		}

		deviceRemote.SetDnsResolverSettings(copyOfPick)
		if got := testingDnsResolverSettingsValue(deviceLocal.GetDnsResolverSettings()); !testingDnsResolverSettingsComparableEqual(got, today) {
			t.Errorf("%s: the rpc's device holds %+v, want %+v", recommendation.name, got, today)
		}
	}
}

// Anything but an exact earlier recommendation in a record that can hold it
// loads as saved, as Custom: another toggle on, a server added or missing, a
// v6 list the user added, Turkmentelecom alone in a version 0 record (tm then
// recommended Turkmentelecom with Google), the Google-era tm pair in a version
// 1 record (that sdk had already moved it), and an earlier recommendation
// saved by this sdk, a choice made with today's recommendations in view. And
// an app's copy of a pick with another country's earlier recommendation, or
// with the earlier list edited, is an edit: Custom with the lists as given.
func TestEarlierRegionalListsOtherwiseLoadAsSaved(t *testing.T) {
	withRemoteDoh := testingEarlierRegionalRecommendation([]string{"195.175.39.39", "212.252.114.8"}, nil, false, "")
	withRemoteDoh.EnableRemoteDoh = true
	withLocalDns := testingEarlierRegionalRecommendation([]string{"178.22.122.100", "10.202.10.10"}, nil, false, "")
	withLocalDns.EnableLocalDns = true
	withLocalDns.LocalDnsIpv4 = testing_stringList("192.0.2.53")
	cases := []struct {
		name     string
		version  int
		settings *DnsResolverSettings
	}{
		{name: "tr with remote doh", version: 2, settings: withRemoteDoh},
		{name: "ir with local dns", version: 2, settings: withLocalDns},
		{name: "ir plus a server", version: 2, settings: testingEarlierRegionalRecommendation([]string{"178.22.122.100", "10.202.10.10", "192.0.2.53"}, nil, false, "")},
		{name: "tr without superonline", version: 1, settings: testingEarlierRegionalRecommendation([]string{"195.175.39.39"}, nil, false, "")},
		{name: "ir with a v6 server", version: 1, settings: testingEarlierRegionalRecommendation([]string{"178.22.122.100", "10.202.10.10"}, []string{"2001:db8::53"}, false, "")},
		{name: "turkmentelecom alone in a version 0 record", version: 0, settings: testingEarlierRegionalRecommendation([]string{"217.174.238.141"}, nil, false, "")},
		{name: "google-era tm in a version 1 record", version: 1, settings: testingEarlierRegionalRecommendation([]string{"217.174.238.141", "8.8.4.4"}, []string{"2001:4860:4860::8844"}, false, "")},
		{name: "tr saved by this sdk", version: 4, settings: testingEarlierRegionalRecommendation([]string{"195.175.39.39", "212.252.114.8"}, nil, false, "")},
	}
	for _, c := range cases {
		localState := &LocalState{localStorageDir: t.TempDir()}
		switch c.version {
		case 0:
			testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
		case 1:
			testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
		case 2:
			testingWriteVersion2DnsResolverSettings(t, localState.localStorageDir, c.settings)
		default:
			if err := localState.SetDnsResolverSettings(c.settings); err != nil {
				t.Fatal(err)
			}
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

	for _, c := range []struct {
		name     string
		presetId string
		settings *DnsResolverSettings
	}{
		{name: "tr's earlier list under ir's pick", presetId: "regional-ir", settings: testingEarlierRegionalRecommendation([]string{"195.175.39.39", "212.252.114.8"}, nil, false, "")},
		{name: "ir's earlier list with a server added", presetId: "regional-ir", settings: testingEarlierRegionalRecommendation([]string{"178.22.122.100", "10.202.10.10", "192.0.2.53"}, nil, false, "")},
	} {
		copyOfPick := c.settings
		copyOfPick.PresetId = c.presetId
		edited := dnsResolverSettingsAsEdited(copyOfPick)
		if edited.PresetId != "" {
			t.Errorf("%s: reads as the pick %q, want an edit", c.name, edited.PresetId)
		}
		if got, want := testingStringsOf(edited.RemoteDnsIpv4), testingStringsOf(c.settings.RemoteDnsIpv4); !slices.Equal(got, want) {
			t.Errorf("%s: edited lists = %v, want %v as given", c.name, got, want)
		}
	}
}

// What the sdk shows the apps follows connect's recommendations of
// 2026-11-11: the regional server list, the recommendation and its pick, and
// the connection regions with their plain fallback servers name ir's Shecan
// and Electro, tr and tm have none and are no connection region, and no view
// names a server that cannot answer an exit.
func TestRegionalDnsViewsShowTodaysRecommendations(t *testing.T) {
	unable := []string{"10.202.10.10", "195.175.39.39", "195.175.39.40", "212.252.114.8", "217.174.238.141"}
	servers := GetRegionalDnsServers()
	for i := range servers.Len() {
		server := servers.Get(i)
		if server.CountryCode == "tr" || server.CountryCode == "tm" {
			t.Errorf("the regional servers name %s's %s %s", server.CountryCode, server.Name, server.Ipv4)
		}
		if slices.Contains(unable, server.Ipv4) {
			t.Errorf("the regional servers name %s %s %s, which cannot answer an exit", server.CountryCode, server.Name, server.Ipv4)
		}
	}
	for countryCode, want := range map[string]bool{"ir": true, "tr": false, "tm": false} {
		if got := HasRegionalDnsRecommendation(countryCode); got != want {
			t.Errorf("%s has a recommendation: %t, want %t", countryCode, got, want)
		}
	}
	for _, countryCode := range []string{"tr", "tm"} {
		if recommended := GetRecommendedDnsResolverSettings(countryCode); recommended != nil {
			t.Errorf("%s recommendation = %+v, want none", countryCode, recommended)
		}
		if slices.Contains(IngressCountryCodes().getAll(), countryCode) {
			t.Errorf("%s is a connection region: %v", countryCode, IngressCountryCodes().getAll())
		}
	}
	ir := GetRecommendedDnsResolverSettings("ir")
	if ir == nil || ir.PresetId != "regional-ir" || !ir.EnableRemoteDns || ir.EnableRemoteDoh {
		t.Fatalf("ir recommendation = %+v, want the ir pick with remote plain dns", ir)
	}
	connect.AssertEqual(t, testingStringsOf(ir.RemoteDnsIpv4), []string{"178.22.122.100", "78.157.42.100"})
	for countryCode, want := range map[string][]string{
		"ir": {"178.22.122.100", "78.157.42.100"},
		"tr": {},
		"tm": {},
	} {
		if got := IngressPlainDnsServers(countryCode).getAll(); !slices.Equal(got, want) {
			t.Errorf("%s connection region plain servers = %v, want %v", countryCode, got, want)
		}
	}
}
