package sdk

// Google Public DNS left the default server lists (owner decision 2026-11-08,
// support inbox 8236). Every app's dns editor saves all eight lists, so a
// record an older sdk wrote can hold the old default lists, Google included,
// carried along rather than chosen. These tests drive the real record files
// and DeviceLocal load path, with no network: an old default list loads as
// the current default, the old tm recommendation as the current one, and
// every list a user chose loads as saved.

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

// The default lists exactly as the sdk shipped them from 2026-07-02 to
// 2026-11-08, and so as an editor saved them.
var (
	testingGoogleEraRemoteDohUrlsIpv4 = []string{
		"https://1.1.1.1/dns-query",
		"https://8.8.8.8/dns-query",
		"https://9.9.9.9/dns-query",
		"https://208.67.222.222/dns-query",
	}
	testingGoogleEraRemoteDohUrlsIpv6 = []string{
		"https://[2606:4700:4700::1111]/dns-query",
		"https://[2001:4860:4860::8888]/dns-query",
		"https://[2620:fe::fe]/dns-query",
		"https://[2620:119:35::35]/dns-query",
	}
	testingGoogleEraRemoteDnsIpv4 = []string{"1.1.1.1", "9.9.9.9", "8.8.8.8", "208.67.222.222"}
	testingGoogleEraRemoteDnsIpv6 = []string{"2606:4700:4700::1111", "2620:fe::fe", "2001:4860:4860::8888", "2620:119:35::35"}
)

// Google Public DNS as a list names it: an anycast address (v4, or in its v6
// /48), a DoH url on one, or one of its host names.
func testingIsGoogleDns(entry string) bool {
	host := strings.TrimSpace(entry)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		return addr == netip.MustParseAddr("8.8.8.8") ||
			addr == netip.MustParseAddr("8.8.4.4") ||
			netip.MustParsePrefix("2001:4860:4860::/48").Contains(addr)
	}
	return host == "dns.google" || strings.HasSuffix(host, ".dns.google") || host == "dns.google.com"
}

func testingStringsOf(list *StringList) []string {
	if list == nil {
		return nil
	}
	return list.getAll()
}

func testingDnsResolverSettingsLists(dnsResolverSettings *DnsResolverSettings) map[string][]string {
	return map[string][]string{
		"RemoteDohUrlsIpv4": testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv4),
		"RemoteDohUrlsIpv6": testingStringsOf(dnsResolverSettings.RemoteDohUrlsIpv6),
		"LocalDohUrlsIpv4":  testingStringsOf(dnsResolverSettings.LocalDohUrlsIpv4),
		"LocalDohUrlsIpv6":  testingStringsOf(dnsResolverSettings.LocalDohUrlsIpv6),
		"RemoteDnsIpv4":     testingStringsOf(dnsResolverSettings.RemoteDnsIpv4),
		"RemoteDnsIpv6":     testingStringsOf(dnsResolverSettings.RemoteDnsIpv6),
		"LocalDnsIpv4":      testingStringsOf(dnsResolverSettings.LocalDnsIpv4),
		"LocalDnsIpv6":      testingStringsOf(dnsResolverSettings.LocalDnsIpv6),
	}
}

func testingErrorGoogleDns(t *testing.T, what string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	for name, list := range testingDnsResolverSettingsLists(dnsResolverSettings) {
		for _, entry := range list {
			if testingIsGoogleDns(entry) {
				t.Errorf("%s %s names Google Public DNS: %s", what, name, entry)
			}
		}
	}
}

// A record exactly as the sdk before the migration wrote it: the whole
// settings object and the fallback opt-in version, no default servers version.
func testingWriteGoogleEraDnsResolverSettings(t *testing.T, localStorageDir string, dnsResolverSettings *DnsResolverSettings) {
	t.Helper()
	data, err := json.Marshal(&struct {
		DnsResolverSettings
		FallbackOptInVersion int `json:",omitempty"`
	}{
		DnsResolverSettings:  *dnsResolverSettings,
		FallbackOptInVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

// What an editor saved from the old defaults, with the user's fallback opt-in.
func testingGoogleEraDefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh:       true,
		EnableFallback:        true,
		DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     testing_stringList(testingGoogleEraRemoteDohUrlsIpv4...),
		RemoteDohUrlsIpv6:     testing_stringList(testingGoogleEraRemoteDohUrlsIpv6...),
		LocalDohUrlsIpv4:      testing_stringList(),
		LocalDohUrlsIpv6:      testing_stringList(),
		RemoteDnsIpv4:         testing_stringList(testingGoogleEraRemoteDnsIpv4...),
		RemoteDnsIpv6:         testing_stringList(testingGoogleEraRemoteDnsIpv6...),
		LocalDnsIpv4:          testing_stringList(),
		LocalDnsIpv6:          testing_stringList(),
	}
}

func TestGoogleEraDefaultDnsListsLoadAsCurrentDefaults(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, testingGoogleEraDefaultDnsResolverSettings())

	persisted := localState.GetDnsResolverSettings()
	if persisted == nil {
		t.Fatal("old record did not load")
	}
	testingErrorGoogleDns(t, "old default record", persisted)
	defaults := connect.DefaultDnsResolverSettings()
	for name, pair := range map[string][2][]string{
		"RemoteDohUrlsIpv4": {testingStringsOf(persisted.RemoteDohUrlsIpv4), defaults.RemoteDohUrlsIpv4},
		"RemoteDohUrlsIpv6": {testingStringsOf(persisted.RemoteDohUrlsIpv6), defaults.RemoteDohUrlsIpv6},
		"RemoteDnsIpv4":     {testingStringsOf(persisted.RemoteDnsIpv4), defaults.RemoteDnsIpv4},
		"RemoteDnsIpv6":     {testingStringsOf(persisted.RemoteDnsIpv6), defaults.RemoteDnsIpv6},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("old default %s loaded as %v, want the current default %v", name, pair[0], pair[1])
		}
	}
	// the rest of the record is the user's and loads as saved
	if !persisted.EnableRemoteDoh || !persisted.EnableFallback {
		t.Errorf("old record toggles changed: %+v", persisted)
	}
	if 0 < len(testingStringsOf(persisted.LocalDohUrlsIpv4)) || 0 < len(testingStringsOf(persisted.LocalDnsIpv4)) {
		t.Errorf("old record gained local servers: %+v", persisted)
	}
}

// The composed path: the device loads the old record through the preference
// catalog, installs its resolver in the mux, and reports it back. Nothing the
// tunnel or the host-side fallback queries names Google.
func TestGoogleEraDefaultDnsListsLoadWithoutGoogleOnDevice(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, testingGoogleEraDefaultDnsResolverSettings())

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || !dnsResolverSettings.EnableRemoteDoh || !dnsResolverSettings.EnableFallback {
		t.Fatalf("old record did not load on the device: %+v", dnsResolverSettings)
	}
	testingErrorGoogleDns(t, "device resolver", dnsResolverSettings)
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
		for _, entry := range slices.Concat(
			settings.RemoteDohUrlsIpv4, settings.RemoteDohUrlsIpv6,
			settings.LocalDohUrlsIpv4, settings.LocalDohUrlsIpv6,
			settings.RemoteDnsIpv4, settings.RemoteDnsIpv6,
			settings.LocalDnsIpv4, settings.LocalDnsIpv6,
		) {
			if testingIsGoogleDns(entry) {
				t.Errorf("%s names Google Public DNS: %s", what, entry)
			}
		}
	}
}

// An old default list the user reordered, or one carried into another list
// (the host-side projection puts the remote DoH servers in a local list),
// keeps its order for the servers that remain.
func TestGoogleEraDefaultDnsListKeepsSavedOrder(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	reordered := []string{
		"https://9.9.9.9/dns-query",
		"https://8.8.8.8/dns-query",
		"https://208.67.222.222/dns-query",
		"https://1.1.1.1/dns-query",
	}
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: testing_stringList(reordered...),
		LocalDohUrlsIpv4:  testing_stringList(testingGoogleEraRemoteDohUrlsIpv4...),
	})
	persisted := localState.GetDnsResolverSettings()
	want := []string{"https://9.9.9.9/dns-query", "https://208.67.222.222/dns-query", "https://1.1.1.1/dns-query"}
	if got := testingStringsOf(persisted.RemoteDohUrlsIpv4); !slices.Equal(got, want) {
		t.Errorf("reordered old default = %v, want %v", got, want)
	}
	if got := testingStringsOf(persisted.LocalDohUrlsIpv4); !slices.Equal(got, connect.DefaultDnsResolverSettings().RemoteDohUrlsIpv4) {
		t.Errorf("old default in a local list = %v, want the current default", got)
	}
}

// A user's own lists load as saved, Google included: one Google server alone
// (which is also what an old kz or tm recommendation looks like), Google among
// other servers, part of the old defaults, and the old defaults plus a server.
func TestUserChosenGoogleDnsServersLoadAsSaved(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	chosen := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		EnableRemoteDns:   true,
		RemoteDohUrlsIpv4: testing_stringList("https://8.8.8.8/dns-query"),
		RemoteDohUrlsIpv6: testing_stringList("https://[2606:4700:4700::1111]/dns-query", "https://[2001:4860:4860::8888]/dns-query"),
		LocalDohUrlsIpv6:  testing_stringList(append(slices.Clone(testingGoogleEraRemoteDohUrlsIpv6), "https://[2001:db8::53]/dns-query")...),
		RemoteDnsIpv4:     testing_stringList("8.8.8.8"),
		RemoteDnsIpv6:     testing_stringList("2001:4860:4860::8888"),
		LocalDnsIpv4:      testing_stringList("192.0.2.53", "8.8.4.4"),
	}
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, chosen)
	persisted := localState.GetDnsResolverSettings()
	if persisted == nil {
		t.Fatal("record did not load")
	}
	saved := testingDnsResolverSettingsLists(chosen)
	for name, list := range testingDnsResolverSettingsLists(persisted) {
		if !slices.Equal(list, saved[name]) {
			t.Errorf("chosen %s loaded as %v, want %v as saved", name, list, saved[name])
		}
	}
}

// A record this sdk wrote is a choice made with the current defaults in view,
// so even the old default servers load as saved.
func TestCurrentDnsRecordKeepsTheOldDefaultServers(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	if err := localState.SetDnsResolverSettings(testingGoogleEraDefaultDnsResolverSettings()); err != nil {
		t.Fatal(err)
	}
	persisted := localState.GetDnsResolverSettings()
	if persisted == nil {
		t.Fatal("record did not load")
	}
	saved := testingDnsResolverSettingsLists(testingGoogleEraDefaultDnsResolverSettings())
	for name, list := range testingDnsResolverSettingsLists(persisted) {
		if !slices.Equal(list, saved[name]) {
			t.Errorf("%s written by this sdk loaded as %v, want %v as saved", name, list, saved[name])
		}
	}
}

// A record from before the v6 defaults (2026-09-11) holds the old v4 lists and
// no v6 ones: the v4 lists take the current defaults, and the v6 lists stay
// empty, as the user had them.
func TestV4OnlyGoogleEraRecordKeepsEmptyV6Lists(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: testing_stringList(testingGoogleEraRemoteDohUrlsIpv4...),
		RemoteDnsIpv4:     testing_stringList(testingGoogleEraRemoteDnsIpv4...),
	})
	persisted := localState.GetDnsResolverSettings()
	defaults := connect.DefaultDnsResolverSettings()
	if got := testingStringsOf(persisted.RemoteDohUrlsIpv4); !slices.Equal(got, defaults.RemoteDohUrlsIpv4) {
		t.Errorf("v4 doh = %v, want %v", got, defaults.RemoteDohUrlsIpv4)
	}
	if got := testingStringsOf(persisted.RemoteDnsIpv4); !slices.Equal(got, defaults.RemoteDnsIpv4) {
		t.Errorf("v4 dns = %v, want %v", got, defaults.RemoteDnsIpv4)
	}
	if 0 < len(testingStringsOf(persisted.RemoteDohUrlsIpv6)) || 0 < len(testingStringsOf(persisted.RemoteDnsIpv6)) {
		t.Errorf("v6 lists gained servers: %v %v", testingStringsOf(persisted.RemoteDohUrlsIpv6), testingStringsOf(persisted.RemoteDnsIpv6))
	}
}

// The tm recommendation as an older sdk applied it: Turkmentelecom and Google
// in the remote plain v4 list, and Google's tm v6 address (none before
// 2026-09-11). Turkmentelecom's address marks it as the recommendation, so it
// loads as the current tm recommendation, in either order and either era.
func TestGoogleEraTmRecommendationLoadsAsCurrentRecommendation(t *testing.T) {
	tmIpv4 := connect.RegionalDnsServerIps("tm")
	tmIpv6 := connect.RegionalDnsServerIpv6s("tm")
	cases := []struct {
		name  string
		ipv4  []string
		ipv6  []string
		empty bool
	}{
		{name: "dual stack", ipv4: []string{"217.174.238.141", "8.8.4.4"}, ipv6: []string{"2001:4860:4860::8844"}},
		{name: "v4 only era", ipv4: []string{"217.174.238.141", "8.8.4.4"}, empty: true},
		{name: "reordered", ipv4: []string{"8.8.4.4", "217.174.238.141"}, ipv6: []string{"2001:4860:4860::8844"}},
	}
	for _, c := range cases {
		localState := &LocalState{localStorageDir: t.TempDir()}
		recommended := &DnsResolverSettings{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress,
			RemoteDnsIpv4:         testing_stringList(c.ipv4...),
		}
		if !c.empty {
			recommended.RemoteDnsIpv6 = testing_stringList(c.ipv6...)
		}
		testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, recommended)
		persisted := localState.GetDnsResolverSettings()
		if persisted == nil || !persisted.EnableRemoteDns {
			t.Errorf("%s: old tm recommendation did not load: %+v", c.name, persisted)
			continue
		}
		testingErrorGoogleDns(t, "old tm recommendation "+c.name, persisted)
		if got := testingStringsOf(persisted.RemoteDnsIpv4); !slices.Equal(got, tmIpv4) {
			t.Errorf("%s: tm v4 = %v, want the current recommendation %v", c.name, got, tmIpv4)
		}
		if got := testingStringsOf(persisted.RemoteDnsIpv6); !slices.Equal(got, tmIpv6) && !(len(got) == 0 && len(tmIpv6) == 0) {
			t.Errorf("%s: tm v6 = %v, want the current recommendation %v", c.name, got, tmIpv6)
		}
	}
}

// The tm pair with a server the user added, or with a v6 list the user
// changed, is a choice and loads as saved, Google included. So does the tm
// pair in a record this sdk wrote.
func TestChangedTmRecommendationLoadsAsSaved(t *testing.T) {
	cases := []struct {
		name     string
		settings *DnsResolverSettings
		current  bool
	}{
		{name: "added v4 server", settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("217.174.238.141", "8.8.4.4", "192.0.2.53"),
			RemoteDnsIpv6:   testing_stringList("2001:4860:4860::8844"),
		}},
		{name: "changed v6", settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("217.174.238.141", "8.8.4.4"),
			RemoteDnsIpv6:   testing_stringList("2001:4860:4860::8844", "2001:db8::53"),
		}},
		{name: "written by this sdk", current: true, settings: &DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   testing_stringList("217.174.238.141", "8.8.4.4"),
			RemoteDnsIpv6:   testing_stringList("2001:4860:4860::8844"),
		}},
	}
	for _, c := range cases {
		localState := &LocalState{localStorageDir: t.TempDir()}
		if c.current {
			if err := localState.SetDnsResolverSettings(c.settings); err != nil {
				t.Fatal(err)
			}
		} else {
			testingWriteGoogleEraDnsResolverSettings(t, localState.localStorageDir, c.settings)
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
