package sdk

// DefaultServersVersion 2: OpenDNS left the default server lists, Cloudflare's
// second address joined them, and Quad9's unfiltered 9.9.9.10 took the place of
// its filtering 9.9.9.9. A record written at version 1 (or older) that holds
// exactly an old default list loads with the current lists, keeping the saved
// order of the servers that remain with 9.9.9.10 where 9.9.9.9 was; every list
// a user chose loads as saved, 9.9.9.9 included; a version 2 record never
// migrates; and an sdk that knows only version 1 loads a version 2 record as
// saved. The tests drive the real record files and the DeviceLocal load path,
// with no network.

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The default lists exactly as the sdk shipped them at version 1 (2026-11-08,
// after Google Public DNS left them), and so as an editor saved them.
var (
	testingOpenDnsEraRemoteDohUrlsIpv4 = []string{
		"https://1.1.1.1/dns-query",
		"https://9.9.9.9/dns-query",
		"https://208.67.222.222/dns-query",
	}
	testingOpenDnsEraRemoteDohUrlsIpv6 = []string{
		"https://[2606:4700:4700::1111]/dns-query",
		"https://[2620:fe::fe]/dns-query",
		"https://[2620:119:35::35]/dns-query",
	}
	testingOpenDnsEraRemoteDnsIpv4 = []string{"1.1.1.1", "9.9.9.9", "208.67.222.222"}
	testingOpenDnsEraRemoteDnsIpv6 = []string{"2606:4700:4700::1111", "2620:fe::fe", "2620:119:35::35"}
)

// OpenDNS as a list names it: an address in its v4 /21 or v6 prefixes, a DoH
// url on one, or one of its host names.
func testingIsOpenDns(entry string) bool {
	host := strings.TrimSpace(entry)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		return netip.MustParsePrefix("208.67.216.0/21").Contains(addr) ||
			netip.MustParsePrefix("2620:119::/32").Contains(addr)
	}
	return host == "opendns.com" || strings.HasSuffix(host, ".opendns.com")
}

// A record exactly as an sdk at version 1 wrote it: the whole settings
// object, the fallback opt-in version and DefaultServersVersion 1.
func testingWriteOpenDnsEraDnsResolverSettings(t *testing.T, localStorageDir string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	data, err := json.Marshal(&struct {
		DnsResolverSettings
		FallbackOptInVersion  int `json:",omitempty"`
		DefaultServersVersion int `json:",omitempty"`
	}{
		DnsResolverSettings:   *dnsResolverSettings,
		FallbackOptInVersion:  1,
		DefaultServersVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// What an editor at version 1 saved from the defaults, with the user's
// fallback opt-in.
func testingOpenDnsEraDefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh:       true,
		EnableFallback:        true,
		DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     testing_stringList(testingOpenDnsEraRemoteDohUrlsIpv4...),
		RemoteDohUrlsIpv6:     testing_stringList(testingOpenDnsEraRemoteDohUrlsIpv6...),
		LocalDohUrlsIpv4:      testing_stringList(),
		LocalDohUrlsIpv6:      testing_stringList(),
		RemoteDnsIpv4:         testing_stringList(testingOpenDnsEraRemoteDnsIpv4...),
		RemoteDnsIpv6:         testing_stringList(testingOpenDnsEraRemoteDnsIpv6...),
		LocalDnsIpv4:          testing_stringList(),
		LocalDnsIpv6:          testing_stringList(),
	}
}

func testingErrorOpenDns(t *testing.T, what string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	for name, list := range testingDnsResolverSettingsLists(dnsResolverSettings) {
		for _, entry := range list {
			if testingIsOpenDns(entry) {
				t.Errorf("%s %s names OpenDNS: %s", what, name, entry)
			}
		}
	}
}

// The four remote lists equal the current defaults exactly, in their order.
func testingErrorNotCurrentDefaults(t *testing.T, what string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	defaults := connect.DefaultDnsResolverSettings()
	for name, pair := range map[string][2][]string{
		"RemoteDohUrlsIpv4": {testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv4), defaults.RemoteDohUrlsIpv4},
		"RemoteDohUrlsIpv6": {testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv6), defaults.RemoteDohUrlsIpv6},
		"RemoteDnsIpv4":     {testingStringsOf(dnsResolverSettings.RemoteDnsIpv4), defaults.RemoteDnsIpv4},
		"RemoteDnsIpv6":     {testingStringsOf(dnsResolverSettings.RemoteDnsIpv6), defaults.RemoteDnsIpv6},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s %s loaded as %v, want the current default %v", what, name, pair[0], pair[1])
		}
	}
}

// v1 to v2: a version 1 record holding the version 1 default lists loads with
// the current lists, Cloudflare's second address in and OpenDNS out, and the
// rest of the record as saved.
func TestOpenDnsEraDefaultDnsListsLoadAsCurrentDefaults(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, testingOpenDnsEraDefaultDnsResolverSettings())
	persisted := localState.GetDnsResolverSettings()
	if persisted == nil {
		t.Fatal("version 1 record did not load")
	}
	testingErrorOpenDns(t, "version 1 default record", persisted)
	testingErrorNotCurrentDefaults(t, "version 1 default record", persisted)
	if !persisted.EnableRemoteDoh || !persisted.EnableFallback {
		t.Errorf("version 1 record toggles changed: %+v", persisted)
	}
}

// v0 to v2: a record from before version 1 holding the Google-era default
// lists migrates straight to the current lists, with neither Google nor
// OpenDNS.
func TestGoogleEraDefaultDnsListsLoadAsVersion2Defaults(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, testingGoogleEraDefaultDnsResolverSettings())
	persisted := localState.GetDnsResolverSettings()
	if persisted == nil {
		t.Fatal("version 0 record did not load")
	}
	testingErrorGoogleDns(t, "version 0 default record", persisted)
	testingErrorOpenDns(t, "version 0 default record", persisted)
	testingErrorNotCurrentDefaults(t, "version 0 default record", persisted)
}

// The composed path for v1 to v2: the device loads the version 1 record
// through the preference catalog and installs its resolver; nothing the
// tunnel or the host-side fallback queries names OpenDNS, and both of
// Cloudflare's addresses are there.
func TestOpenDnsEraDefaultDnsListsLoadWithoutOpenDnsOnDevice(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, testingOpenDnsEraDefaultDnsResolverSettings())

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || !dnsResolverSettings.EnableRemoteDoh || !dnsResolverSettings.EnableFallback {
		t.Fatalf("version 1 record did not load on the device: %+v", dnsResolverSettings)
	}
	testingErrorOpenDns(t, "device resolver", dnsResolverSettings)
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
	for what, settings := range map[string]*connect.DnsResolverSettings{"mux resolver": resolver, "mux fallback": fallback} {
		entries := slices.Concat(
			settings.RemoteDohUrlsIpv4, settings.RemoteDohUrlsIpv6,
			settings.LocalDohUrlsIpv4, settings.LocalDohUrlsIpv6,
			settings.RemoteDnsIpv4, settings.RemoteDnsIpv6,
			settings.LocalDnsIpv4, settings.LocalDnsIpv6,
		)
		for _, entry := range entries {
			if testingIsOpenDns(entry) {
				t.Errorf("%s names OpenDNS: %s", what, entry)
			}
		}
		for _, cloudflare := range []string{"https://1.0.0.1/dns-query", "https://[2606:4700:4700::1001]/dns-query"} {
			if !slices.Contains(entries, cloudflare) {
				t.Errorf("%s lacks Cloudflare's second address %s", what, cloudflare)
			}
		}
		for _, filteringQuad9 := range []string{"https://9.9.9.9/dns-query", "https://[2620:fe::fe]/dns-query", "9.9.9.9", "2620:fe::fe"} {
			if slices.Contains(entries, filteringQuad9) {
				t.Errorf("%s names Quad9's filtering %s", what, filteringQuad9)
			}
		}
		if !slices.Contains(entries, "https://9.9.9.10/dns-query") {
			t.Errorf("%s lacks Quad9's unfiltered https://9.9.9.10/dns-query", what)
		}
	}
}

// A version 1 default list the user reordered keeps its order for the servers
// that remain, Quad9's 9.9.9.10 where its 9.9.9.9 was, and the current list's
// new servers follow: Cloudflare's second address, then DNS.SB's two.
func TestOpenDnsEraDefaultDnsListKeepsSavedOrder(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, &DnsResolverSettings{
		EnableRemoteDoh: true,
		RemoteDnsIpv4:   testing_stringList("9.9.9.9", "208.67.222.222", "1.1.1.1"),
	})
	persisted := localState.GetDnsResolverSettings()
	if got, want := testingStringsOf(persisted.RemoteDnsIpv4), []string{"9.9.9.10", "1.1.1.1", "1.0.0.1", "185.222.222.222", "45.11.45.11"}; !slices.Equal(got, want) {
		t.Errorf("reordered version 1 default = %v, want %v", got, want)
	}
}

// Every old default list, in the default order and reordered, from a version 0
// and a version 1 record, migrates with Quad9's unfiltered address in the place
// of its filtering one and the other servers in their saved order, then the
// current list's new servers: Cloudflare's second address and DNS.SB's two.
func TestOldDefaultDnsListsPutUnfilteredQuad9InItsPlace(t *testing.T) {
	cases := []struct {
		name    string
		version int
		list    string
		saved   []string
		want    []string
	}{
		{name: "version 1 doh v4", version: 1, list: "RemoteDohUrlsIpv4",
			saved: []string{"https://1.1.1.1/dns-query", "https://9.9.9.9/dns-query", "https://208.67.222.222/dns-query"},
			want: []string{"https://1.1.1.1/dns-query", "https://9.9.9.10/dns-query", "https://1.0.0.1/dns-query",
				"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query"}},
		{name: "version 1 doh v6 reordered", version: 1, list: "RemoteDohUrlsIpv6",
			saved: []string{"https://[2620:fe::fe]/dns-query", "https://[2620:119:35::35]/dns-query", "https://[2606:4700:4700::1111]/dns-query"},
			want: []string{"https://[2620:fe::10]/dns-query", "https://[2606:4700:4700::1111]/dns-query", "https://[2606:4700:4700::1001]/dns-query",
				"https://[2a09::]/dns-query", "https://[2a11::]/dns-query"}},
		{name: "version 1 plain v6", version: 1, list: "RemoteDnsIpv6",
			saved: []string{"2606:4700:4700::1111", "2620:fe::fe", "2620:119:35::35"},
			want:  []string{"2606:4700:4700::1111", "2620:fe::10", "2606:4700:4700::1001", "2a09::", "2a11::"}},
		{name: "version 0 doh v4 reordered", version: 0, list: "RemoteDohUrlsIpv4",
			saved: []string{"https://9.9.9.9/dns-query", "https://8.8.8.8/dns-query", "https://208.67.222.222/dns-query", "https://1.1.1.1/dns-query"},
			want: []string{"https://9.9.9.10/dns-query", "https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query",
				"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query"}},
		{name: "version 0 plain v4", version: 0, list: "RemoteDnsIpv4",
			saved: []string{"1.1.1.1", "9.9.9.9", "8.8.8.8", "208.67.222.222"},
			want:  []string{"1.1.1.1", "9.9.9.10", "1.0.0.1", "185.222.222.222", "45.11.45.11"}},
		{name: "version 0 plain v6 reordered", version: 0, list: "RemoteDnsIpv6",
			saved: []string{"2620:119:35::35", "2001:4860:4860::8888", "2620:fe::fe", "2606:4700:4700::1111"},
			want:  []string{"2620:fe::10", "2606:4700:4700::1111", "2606:4700:4700::1001", "2a09::", "2a11::"}},
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
		}
		localState := &LocalState{localStorageDir: t.TempDir()}
		switch c.version {
		case 0:
			testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, settings)
		case 1:
			testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, settings)
		}
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

// What a user chose loads as saved: OpenDNS alone, OpenDNS among other
// servers, the version 1 defaults plus a server, Quad9's filtering 9.9.9.9
// alone or with other servers outside an exact old default list, a Google-era
// default list or the old tm recommendation in a version 1 record (a version 1
// sdk had already migrated those, so they are choices), and the version 1
// defaults in a version 2 record.
func TestChosenDnsListsLoadAsSavedAfterVersion2(t *testing.T) {
	cases := []struct {
		name     string
		settings *DnsResolverSettings
		version  int
	}{
		{name: "opendns alone", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList("https://208.67.222.222/dns-query"),
			RemoteDnsIpv4:     testing_stringList("208.67.222.222"),
		}},
		{name: "opendns among others", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList("https://192.0.2.53/dns-query", "https://208.67.220.220/dns-query"),
		}},
		{name: "version 1 defaults plus a server", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList(append(slices.Clone(testingOpenDnsEraRemoteDohUrlsIpv4), "https://192.0.2.53/dns-query")...),
		}},
		{name: "quad9's filtering 9.9.9.9 alone", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList("https://9.9.9.9/dns-query"),
			RemoteDohUrlsIpv6: testing_stringList("https://[2620:fe::fe]/dns-query"),
			RemoteDnsIpv4:     testing_stringList("9.9.9.9"),
		}},
		{name: "quad9's filtering 9.9.9.9 with both cloudflare addresses", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList("https://1.1.1.1/dns-query", "https://9.9.9.9/dns-query", "https://1.0.0.1/dns-query"),
			RemoteDnsIpv4:     testing_stringList("9.9.9.9", "1.1.1.1"),
		}},
		{name: "google-era defaults in a version 1 record", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: testing_stringList(testingGoogleEraRemoteDohUrlsIpv4...),
			RemoteDnsIpv4:     testing_stringList(testingGoogleEraRemoteDnsIpv4...),
		}},
		{name: "old tm recommendation in a version 1 record", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("217.174.238.141", "8.8.4.4"),
			RemoteDnsIpv6:   testing_stringList("2001:4860:4860::8844"),
		}},
		{name: "version 1 defaults in a version 2 record", version: 2, settings: testingOpenDnsEraDefaultDnsResolverSettings()},
	}
	for _, c := range cases {
		localState := &LocalState{localStorageDir: t.TempDir()}
		switch c.version {
		case 1:
			testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
		case 2:
			if err := localState.SetDnsResolverSettings(c.settings); err != nil {
				t.Fatal(err)
			}
		}
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil {
			t.Errorf("%s: record did not load", c.name)
			continue
		}
		saved := testingDnsResolverSettingsLists(c.settings)
		for name, list := range testingDnsResolverSettingsLists(persisted) {
			if !slices.Equal(list, saved[name]) {
				t.Errorf("%s: %s loaded as %v, want %v as saved", c.name, name, list, saved[name])
			}
		}
	}
}

// An sdk that knows only version 1 migrates a record only below
// DefaultServersVersion 1 and resets the fallback only below
// FallbackOptInVersion 1, and reads unknown fields as absent. A record this
// sdk writes carries version 4 of the first (since the ir, tr and tm regional
// recommendations changed) and version 1 of the second, so that sdk (shipped
// at sdk 92553ef0) loads it as saved.
func TestVersion2RecordLoadsAsSavedForAVersion1Sdk(t *testing.T) {
	settings := testingOpenDnsEraDefaultDnsResolverSettings()
	data, err := encodeDnsResolverSettingsRecord(settings)
	if err != nil {
		t.Fatal(err)
	}
	var version1Record struct {
		DnsResolverSettings
		FallbackOptInVersion  int `json:",omitempty"`
		DefaultServersVersion int `json:",omitempty"`
	}
	if err := json.Unmarshal(data, &version1Record); err != nil {
		t.Fatal(err)
	}
	if version1Record.DefaultServersVersion != 4 || version1Record.FallbackOptInVersion != 1 {
		t.Fatalf("record versions = default servers %d, fallback opt-in %d; want 4 and 1",
			version1Record.DefaultServersVersion, version1Record.FallbackOptInVersion)
	}
	saved := testingDnsResolverSettingsLists(settings)
	for name, list := range testingDnsResolverSettingsLists(&version1Record.DnsResolverSettings) {
		if !slices.Equal(list, saved[name]) {
			t.Errorf("%s reads as %v, want %v", name, list, saved[name])
		}
	}
	if !version1Record.EnableFallback || !version1Record.EnableRemoteDoh {
		t.Errorf("toggles read as %+v", version1Record.DnsResolverSettings)
	}
}

// The kz recommendation as the version 1 sdk applied it: Cloudflare and
// Quad9's filtering 9.9.9.9 in both remote plain lists. In a version 1 record
// it loads as the current kz recommendation, Quad9's unfiltered 9.9.9.10 in
// its place and the saved order kept, in either order.
func TestVersion1KzRecommendationLoadsAsCurrentRecommendation(t *testing.T) {
	cases := []struct {
		name     string
		ipv4     []string
		ipv6     []string
		wantIpv4 []string
		wantIpv6 []string
	}{
		{name: "version 1",
			ipv4: []string{"1.1.1.1", "9.9.9.9"}, ipv6: []string{"2606:4700:4700::1111", "2620:fe::fe"},
			wantIpv4: []string{"1.1.1.1", "9.9.9.10"}, wantIpv6: []string{"2606:4700:4700::1111", "2620:fe::10"}},
		{name: "version 1 reordered",
			ipv4: []string{"9.9.9.9", "1.1.1.1"}, ipv6: []string{"2620:fe::fe", "2606:4700:4700::1111"},
			wantIpv4: []string{"9.9.9.10", "1.1.1.1"}, wantIpv6: []string{"2620:fe::10", "2606:4700:4700::1111"}},
	}
	if kzIpv4 := connect.RegionalDnsServerIps("kz"); !slices.Equal(kzIpv4, cases[0].wantIpv4) {
		t.Fatalf("connect's kz recommendation = %v, want %v", kzIpv4, cases[0].wantIpv4)
	}
	for _, c := range cases {
		recommended := &DnsResolverSettings{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
			RemoteDohUrlsIpv4:     testing_stringList(),
			RemoteDohUrlsIpv6:     testing_stringList(),
			RemoteDnsIpv4:         testing_stringList(c.ipv4...),
			RemoteDnsIpv6:         testing_stringList(c.ipv6...),
		}
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, recommended)
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil || !persisted.EnableRemoteDns || persisted.EnableRemoteDoh {
			t.Errorf("%s: kz recommendation did not load with its toggles: %+v", c.name, persisted)
			continue
		}
		if got := testingStringsOf(persisted.RemoteDnsIpv4); !slices.Equal(got, c.wantIpv4) {
			t.Errorf("%s: kz v4 = %v, want %v", c.name, got, c.wantIpv4)
		}
		if got := testingStringsOf(persisted.RemoteDnsIpv6); !slices.Equal(got, c.wantIpv6) {
			t.Errorf("%s: kz v6 = %v, want %v", c.name, got, c.wantIpv6)
		}
	}
}

// A version 0 record cannot hold the version 1 kz recommendation: before
// version 1, kz recommended Google alone. Remote plain lists of exactly
// Cloudflare and Quad9's filtering 9.9.9.9 in both families there are the
// user's own choice (the Google-era defaults with Google and OpenDNS deleted,
// or servers typed over the old kz recommendation) and load as saved in either
// order, 9.9.9.9 included. Google-era default DoH lists the editor carried
// along in the same record still load as the current defaults.
func TestVersion0RecordWithTheVersion1KzListsLoadsAsSaved(t *testing.T) {
	defaults := connect.DefaultDnsResolverSettings()
	cases := []struct {
		name        string
		ipv4        []string
		ipv6        []string
		dohIpv4     []string
		dohIpv6     []string
		wantDohIpv4 []string
		wantDohIpv6 []string
	}{
		{name: "google and opendns deleted from the defaults",
			ipv4: []string{"1.1.1.1", "9.9.9.9"}, ipv6: []string{"2606:4700:4700::1111", "2620:fe::fe"},
			dohIpv4: testingGoogleEraRemoteDohUrlsIpv4, dohIpv6: testingGoogleEraRemoteDohUrlsIpv6,
			wantDohIpv4: defaults.RemoteDohUrlsIpv4, wantDohIpv6: defaults.RemoteDohUrlsIpv6},
		{name: "typed over the old kz recommendation, reordered",
			ipv4: []string{"9.9.9.9", "1.1.1.1"}, ipv6: []string{"2620:fe::fe", "2606:4700:4700::1111"}},
	}
	for _, c := range cases {
		chosen := &DnsResolverSettings{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
			RemoteDohUrlsIpv4:     testing_stringList(c.dohIpv4...),
			RemoteDohUrlsIpv6:     testing_stringList(c.dohIpv6...),
			RemoteDnsIpv4:         testing_stringList(c.ipv4...),
			RemoteDnsIpv6:         testing_stringList(c.ipv6...),
		}
		localState := &LocalState{localStorageDir: t.TempDir()}
		testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, chosen)
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil || !persisted.EnableRemoteDns || persisted.EnableRemoteDoh {
			t.Errorf("%s: record did not load with its toggles: %+v", c.name, persisted)
			continue
		}
		if got := testingStringsOf(persisted.RemoteDnsIpv4); !slices.Equal(got, c.ipv4) {
			t.Errorf("%s: v4 = %v, want %v as saved", c.name, got, c.ipv4)
		}
		if got := testingStringsOf(persisted.RemoteDnsIpv6); !slices.Equal(got, c.ipv6) {
			t.Errorf("%s: v6 = %v, want %v as saved", c.name, got, c.ipv6)
		}
		if got := testingStringsOf(persisted.RemoteDohUrlsIpv4); !slices.Equal(got, c.wantDohIpv4) {
			t.Errorf("%s: doh v4 = %v, want %v", c.name, got, c.wantDohIpv4)
		}
		if got := testingStringsOf(persisted.RemoteDohUrlsIpv6); !slices.Equal(got, c.wantDohIpv6) {
			t.Errorf("%s: doh v6 = %v, want %v", c.name, got, c.wantDohIpv6)
		}
	}
}

// The kz recommendation before version 1, Google alone, no longer loads as
// saved: the owner moved it to today's kz pick (DefaultServersVersion 3,
// local_state_dns_default_servers_v3_test.go).

// The version 1 kz lists with a server the user added, with a v6 list the user
// changed or emptied, or in a version 2 record, are choices and load as saved.
func TestChangedKzRecommendationLoadsAsSaved(t *testing.T) {
	cases := []struct {
		name     string
		settings *DnsResolverSettings
		version  int
	}{
		{name: "added v4 server", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9", "192.0.2.53"),
			RemoteDnsIpv6:   testing_stringList("2606:4700:4700::1111", "2620:fe::fe"),
		}},
		{name: "changed v6", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9"),
			RemoteDnsIpv6:   testing_stringList("2606:4700:4700::1111"),
		}},
		{name: "emptied v6", version: 1, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9"),
			RemoteDnsIpv6:   testing_stringList(),
		}},
		{name: "written by this sdk", version: 2, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("1.1.1.1", "9.9.9.9"),
			RemoteDnsIpv6:   testing_stringList("2606:4700:4700::1111", "2620:fe::fe"),
		}},
	}
	for _, c := range cases {
		localState := &LocalState{localStorageDir: t.TempDir()}
		switch c.version {
		case 1:
			testingWriteOpenDnsEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
		case 2:
			if err := localState.SetDnsResolverSettings(c.settings); err != nil {
				t.Fatal(err)
			}
		}
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil {
			t.Errorf("%s: record did not load", c.name)
			continue
		}
		saved := testingDnsResolverSettingsLists(c.settings)
		for name, list := range testingDnsResolverSettingsLists(persisted) {
			if !slices.Equal(list, saved[name]) {
				t.Errorf("%s: %s loaded as %v, want %v as saved", c.name, name, list, saved[name])
			}
		}
	}
}
