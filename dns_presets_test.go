package sdk

// The preset mechanisms (connect ADBLOCK3RD A6, A7, B3, C1, C7, E2), on a
// catalog of synthetic `.example` providers and documentation addresses
// installed for each test, so no fixture names a real resolver. With no
// network: the tests read the resolver settings the device would apply.

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The synthetic catalog: one provider of each shape the shipped catalog has.
func testingDnsPresetCatalog() *dnsPresetCatalog {
	lowerCase := strings.ToLower
	return newDnsPresetCatalog(
		[]*dnsPresetProvider{
			{
				providerId: "alpha", name: "Alpha DNS", operator: "Alpha Operator", jurisdiction: "Exampleland",
				helpUrl: "https://help.alpha.example/", privacyUrl: "https://alpha.example/privacy",
				termsUrl: "https://alpha.example/terms", domains: []string{"alpha.example"},
			},
			{
				providerId: "beta", name: "Beta Resolver", operator: "Beta Operator", jurisdiction: "Sampleland",
				helpUrl: "https://beta.example/help", privacyUrl: "https://beta.example/privacy",
				domains: []string{"beta.example"},
			},
			{
				providerId: "gamma", name: "Gamma", operator: "Gamma Operator", jurisdiction: "Testland",
				helpUrl: "https://gamma.example/help", privacyUrl: "https://gamma.example/privacy",
				domains: []string{"gamma.example"},
			},
		},
		[]*dnsPreset{
			{
				presetId: "alpha-ads", providerId: "alpha", group: DnsPresetGroupAdblock, blocksAds: true,
				copyKey:     "dns_preset_copy_ads_phishing",
				dohUrlsIpv4: []string{"https://dns.alpha.example/dns-query"},
				dohUrlsIpv6: []string{"https://dns.alpha.example/dns-query"},
				dohPinIpv4s: []string{"192.0.2.1", "192.0.2.2"}, dohPinIpv6s: []string{"2001:db8::1", "2001:db8::2"},
				blockAnswer: dnsPresetBlockAnswerNull, canaryName: "ads.canary.example", failover: true, checked: "2026-11-09",
			},
			{
				presetId: "alpha-family", providerId: "alpha", group: DnsPresetGroupFamily, blocksAds: true,
				copyKey:     "dns_preset_copy_ads_adult_safe_search",
				dohUrlsIpv4: []string{"https://family.alpha.example/dns-query"},
				dohUrlsIpv6: []string{"https://family.alpha.example/dns-query"},
				dohPinIpv4s: []string{"192.0.2.3"}, dohPinIpv6s: []string{"2001:db8::3"},
				blockAnswer: dnsPresetBlockAnswerNull, canaryName: "adult.canary.example", checked: "2026-11-09",
			},
			{
				presetId: "beta-security", providerId: "beta", group: DnsPresetGroupSecurity,
				copyKey:     "dns_preset_copy_malware_phishing",
				dohUrlsIpv4: []string{"https://192.0.2.10/dns-query", "https://198.51.100.10/dns-query"},
				dohUrlsIpv6: []string{"https://[2001:db8::10]/dns-query", "https://[2001:db8:1::10]/dns-query"},
				blockAnswer: dnsPresetBlockAnswerNxdomain, canaryName: "malware.canary.example", checked: "2026-11-09",
			},
			{
				presetId: "beta-plain", providerId: "beta", group: DnsPresetGroupSecurity,
				copyKey:     "dns_preset_copy_malware",
				dohUrlsIpv4: []string{"https://192.0.2.20/dns-query"},
				dohUrlsIpv6: []string{"https://[2001:db8::20]/dns-query"},
				blockAnswer: dnsPresetBlockAnswerNull, checked: "2026-11-09",
			},
			{
				presetId: "beta-unfiltered", providerId: "beta", group: DnsPresetGroupUnfiltered,
				copyKey:     "dns_preset_copy_nothing",
				dohUrlsIpv4: []string{"https://192.0.2.30/dns-query", "https://198.51.100.30/dns-query"},
				dohUrlsIpv6: []string{"https://[2001:db8::30]/dns-query", "https://[2001:db8:1::30]/dns-query"},
				failover:    true, checked: "2026-11-09",
			},
			{
				presetId: "gamma-personal", providerId: "gamma", group: DnsPresetGroupPersonal,
				copyKey:     "dns_preset_copy_personal",
				dohUrlsIpv4: []string{"https://id.gamma.example/{id}"},
				dohUrlsIpv6: []string{"https://id.gamma.example/{id}"},
				dohPinIpv4s: []string{"203.0.113.1"}, dohPinIpv6s: []string{"2001:db8:2::1"},
				personalId: &dnsPresetPersonalId{
					dohUrlTemplate:     "https://id.gamma.example/{id}",
					dohUrlNameSegment:  true,
					dotHostnamePattern: regexp.MustCompile(`^(?:.+-)?([0-9a-f]{6})\.dot\.gamma\.example\.?$`),
					idPatterns:         []*regexp.Regexp{regexp.MustCompile(`^[0-9a-f]{6}$`)},
					normalizeCase:      lowerCase,
					example:            "abc123",
					unknownIdAnswer:    dnsPresetUnknownIdRefused,
					idHelpUrl:          "https://gamma.example/dashboard",
					formatKey:          "dns_personal_id_format_nextdns",
					whereKey:           "dns_personal_id_where_nextdns",
					quotaNoteKey:       "dns_personal_id_quota_nextdns",
				},
				checked: "2026-11-09",
			},
			{
				presetId: "alpha-personal", providerId: "alpha", group: DnsPresetGroupPersonal,
				copyKey:     "dns_preset_copy_personal",
				dohUrlsIpv4: []string{"https://d.alpha.example/dns-query/{id}"},
				dohUrlsIpv6: []string{"https://d.alpha.example/dns-query/{id}"},
				dohPinIpv4s: []string{"203.0.113.2"}, dohPinIpv6s: []string{"2001:db8:2::2"},
				personalId: &dnsPresetPersonalId{
					dohUrlTemplate:     "https://d.alpha.example/dns-query/{id}",
					dotHostnamePattern: regexp.MustCompile(`^([a-z0-9-]{1,63})\.d\.alpha\.example\.?$`),
					idPatterns:         []*regexp.Regexp{regexp.MustCompile(`^[0-9a-f]{8}$`)},
					normalizeCase:      lowerCase,
					example:            "1a2b3c4d",
					unknownIdAnswer:    dnsPresetUnknownIdUnfilteredOrServfail,
					idHelpUrl:          "https://alpha.example/dashboard",
					formatKey:          "dns_personal_id_format_adguard",
					whereKey:           "dns_personal_id_where_adguard",
				},
				checked: "2026-11-09",
			},
		},
		map[string]string{"retired-preset": "Retired DNS"},
	)
}

// Installs the synthetic catalog for the test, with ad-blocking presets
// shown, and restores the shipped one after.
func testingWithDnsPresetCatalog(t *testing.T) *dnsPresetCatalog {
	t.Helper()
	catalog := testingDnsPresetCatalog()
	dnsPresetCatalogOverride.Store(catalog)
	SetDnsPresetsHideAdBlocking(false)
	t.Cleanup(func() {
		dnsPresetCatalogOverride.Store(nil)
		SetDnsPresetsHideAdBlocking(false)
	})
	return catalog
}

// A selection's settings as an editor with the helpers saves them.
func testingPresetSettings(t *testing.T, presetId string, personalId string) *DnsResolverSettings {
	t.Helper()
	settings := DnsResolverSettingsWithPreset(GetDefaultDnsResolverSettings(), presetId, personalId)
	if settings == nil {
		t.Fatalf("%s cannot be selected", presetId)
	}
	return settings
}

// A hostname preset resolves through the tunnel with DoH alone: exactly its
// url, in both lists, dialed at its pins, and no plain dns to fall to (A6, B3).
func TestDnsPresetExpansionHostnamePreset(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	settings := testingPresetSettings(t, "alpha-ads", "")
	settings.DnsUpgradeMaskAddress = "192.0.2.53"
	resolver := settings.toConnect()
	connect.AssertEqual(t, resolver, &connect.DnsResolverSettings{
		EnableRemoteDoh:       true,
		DnsUpgradeMaskAddress: "192.0.2.53",
		RemoteDohUrlsIpv4:     []string{"https://dns.alpha.example/dns-query"},
		RemoteDohUrlsIpv6:     []string{"https://dns.alpha.example/dns-query"},
		DohServerHostAddrs: map[string][]string{
			"dns.alpha.example": {"192.0.2.1", "192.0.2.2", "2001:db8::1", "2001:db8::2"},
		},
	})
}

// An ip-literal preset lists its v4 and v6 urls in operator pair order, so the
// families pair by index, and needs no pins.
func TestDnsPresetExpansionIpLiteralPreset(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	resolver := testingPresetSettings(t, "beta-security", "").toConnect()
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv4, []string{"https://192.0.2.10/dns-query", "https://198.51.100.10/dns-query"})
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv6, []string{"https://[2001:db8::10]/dns-query", "https://[2001:db8:1::10]/dns-query"})
	if resolver.DohServerHostAddrs != nil || resolver.EnableLocalDoh || resolver.EnableRemoteDns || resolver.EnableLocalDns ||
		0 < len(resolver.RemoteDnsIpv4)+len(resolver.RemoteDnsIpv6)+len(resolver.LocalDohUrlsIpv4)+len(resolver.LocalDnsIpv4) {
		t.Fatalf("ip-literal preset resolver = %+v", resolver)
	}
}

// A personal preset's url carries the normalized id in its path, and only
// there; the selection keeps the normalized id (D1, D3).
func TestDnsPresetExpansionPersonalPreset(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	settings := testingPresetSettings(t, "gamma-personal", " https://id.gamma.example/ABC123/My-Laptop ")
	connect.AssertEqual(t, settings.PresetPersonalId, "abc123")
	resolver := settings.toConnect()
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv4, []string{"https://id.gamma.example/abc123"})
	connect.AssertEqual(t, resolver.RemoteDohUrlsIpv6, []string{"https://id.gamma.example/abc123"})
	connect.AssertEqual(t, resolver.DohServerHostAddrs, map[string][]string{"id.gamma.example": {"203.0.113.1", "2001:db8:2::1"}})
	// the saved lists are the derived ones, for an older sdk (E5)
	connect.AssertEqual(t, settings.RemoteDohUrlsIpv4.getAll(), []string{"https://id.gamma.example/abc123"})
	connect.AssertEqual(t, settings.RemoteDnsIpv4.Len()+settings.RemoteDnsIpv6.Len(), 0)
}

// Ad-blocking and no-filtering presets fail over to URnetwork's default
// resolver; family, malware and personal presets, and every selection that is
// not a preset, have no failover and fail closed (decision 3).
func TestDnsPresetMuxSettingsFailoverByGroup(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	base := connect.DefaultUpgradeMuxSettings()
	for _, c := range []struct {
		settings *DnsResolverSettings
		failover bool
	}{
		{settings: testingPresetSettings(t, "alpha-ads", ""), failover: true},
		{settings: testingPresetSettings(t, "beta-unfiltered", ""), failover: true},
		{settings: testingPresetSettings(t, "alpha-family", ""), failover: false},
		{settings: testingPresetSettings(t, "beta-security", ""), failover: false},
		{settings: testingPresetSettings(t, "gamma-personal", "abc123"), failover: false},
		{settings: GetDefaultDnsResolverSettings(), failover: false},
		{settings: GetRecommendedDnsResolverSettings("cn"), failover: false},
		{settings: DnsResolverSettingsAsCustom(GetDefaultDnsResolverSettings()), failover: false},
	} {
		dns := upgradeMuxSettingsWithDnsResolverSettings(base, c.settings).Dns
		if c.failover {
			connect.AssertEqual(t, dns.Failover, defaultDnsResolver())
		} else if dns.Failover != nil {
			t.Errorf("%q fails over to %+v; it must fail closed", c.settings.PresetId, dns.Failover)
		}
		// the thresholds stay the mux's own settings
		connect.AssertEqual(t, dns.ResolverFailingMinCount, base.Dns.ResolverFailingMinCount)
		connect.AssertEqual(t, dns.ResolverFailingMinDuration, base.Dns.ResolverFailingMinDuration)
		connect.AssertEqual(t, dns.FailoverProbeInterval, base.Dns.FailoverProbeInterval)
	}
}

// Fast DNS on connect with a preset races URnetwork's default resolver over
// the host network, never the preset or its pins, and only until the tunnel
// is ready. Without a preset the fallback is the host projection of the lists,
// racing as before (decision 5, C7).
func TestDnsPresetMuxSettingsFastDns(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	base := connect.DefaultUpgradeMuxSettings()
	for _, presetId := range []string{"alpha-ads", "alpha-family", "beta-security", "beta-unfiltered"} {
		settings := DnsResolverSettingsWithFallback(testingPresetSettings(t, presetId, ""), true)
		dns := upgradeMuxSettingsWithDnsResolverSettings(base, settings).Dns
		connect.AssertEqual(t, dns.Fallback, connect.DefaultDnsUpgradeFallbackSettings())
		connect.AssertEqual(t, dns.FallbackUntilTunnelReady, true)
		if dns.Fallback.DohServerHostAddrs != nil {
			t.Errorf("%s: the fallback carries pins %v", presetId, dns.Fallback.DohServerHostAddrs)
		}
		for _, dohUrl := range slices.Concat(dns.Fallback.LocalDohUrlsIpv4, dns.Fallback.LocalDohUrlsIpv6, dns.Fallback.RemoteDohUrlsIpv4) {
			if slices.Contains(slices.Concat(dns.Resolver.RemoteDohUrlsIpv4, dns.Resolver.RemoteDohUrlsIpv6), dohUrl) {
				t.Errorf("%s: the host fallback queries the preset at %s", presetId, connect.DohUrlForLog(dohUrl))
			}
		}
		// Fast DNS off: no fallback, the flag set with the preset
		off := upgradeMuxSettingsWithDnsResolverSettings(base, testingPresetSettings(t, presetId, "")).Dns
		if off.Fallback != nil || !off.FallbackUntilTunnelReady {
			t.Errorf("%s with Fast DNS off: fallback %+v, until ready %t", presetId, off.Fallback, off.FallbackUntilTunnelReady)
		}
	}
	for _, settings := range []*DnsResolverSettings{
		GetDefaultDnsResolverSettings(),
		GetRecommendedDnsResolverSettings("cn"),
		DnsResolverSettingsAsCustom(GetDefaultDnsResolverSettings()),
	} {
		settings = DnsResolverSettingsWithFallback(settings, true)
		dns := upgradeMuxSettingsWithDnsResolverSettings(base, settings).Dns
		connect.AssertEqual(t, dns.Fallback, hostFallbackDnsResolverSettings(settings.toConnect()))
		connect.AssertEqual(t, dns.FallbackUntilTunnelReady, false)
	}
}

// Pins follow the host (A6): custom lists that name a catalog host dial its
// pins, a preset's settings saved as custom with a fresh struct, as editors
// before the presets build them, still look nothing up, and the host
// projection of such custom settings carries the pins. A host the catalog does
// not know keeps no pins.
func TestDnsPresetPinsFollowTheHost(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	preset := testingPresetSettings(t, "alpha-ads", "")
	fresh := &DnsResolverSettings{
		EnableRemoteDoh:   preset.EnableRemoteDoh,
		RemoteDohUrlsIpv4: testing_stringList(preset.RemoteDohUrlsIpv4.getAll()...),
		RemoteDohUrlsIpv6: testing_stringList(preset.RemoteDohUrlsIpv6.getAll()...),
	}
	resolver := fresh.toConnect()
	connect.AssertEqual(t, resolver.DohServerHostAddrs, map[string][]string{
		"dns.alpha.example": {"192.0.2.1", "192.0.2.2", "2001:db8::1", "2001:db8::2"},
	})
	fallback := hostFallbackDnsResolverSettings(resolver)
	connect.AssertEqual(t, fallback.DohServerHostAddrs, resolver.DohServerHostAddrs)
	connect.AssertEqual(t, fallback.LocalDohUrlsIpv4, []string{"https://dns.alpha.example/dns-query"})

	mixed := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		EnableLocalDoh:    true,
		RemoteDohUrlsIpv4: testing_stringList("https://doh.unlisted.example/dns-query", "https://family.alpha.example/dns-query"),
		LocalDohUrlsIpv4:  testing_stringList("https://id.gamma.example/abc123"),
	}
	connect.AssertEqual(t, mixed.toConnect().DohServerHostAddrs, map[string][]string{
		"family.alpha.example": {"192.0.2.3", "2001:db8::3"},
		"id.gamma.example":     {"203.0.113.1", "2001:db8:2::1"},
	})
	unlisted := &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: testing_stringList("https://doh.unlisted.example/dns-query")}
	if pins := unlisted.toConnect().DohServerHostAddrs; pins != nil {
		t.Fatalf("an unlisted host got pins %v", pins)
	}
}

// The resolver endpoints, the pins a dial goes to included, skip block
// actions and the built-in blocker.
func TestDnsPresetIgnoreHostsCarryPins(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	values := dnsIgnoreHostValues(testingPresetSettings(t, "alpha-ads", ""))
	for _, want := range []string{"dns.alpha.example", "192.0.2.1", "192.0.2.2", "2001:db8::1", "2001:db8::2"} {
		if !slices.Contains(values, want) {
			t.Errorf("the ignore list %v lacks %s", values, want)
		}
	}
}

// A regional pick expands to exactly the country's recommendation, with the
// user's mask (6.3), and a country without one applies "default".
func TestDnsPresetRegionalPickExpandsToTheRecommendation(t *testing.T) {
	settings := &DnsResolverSettings{PresetId: "regional-cn", DnsUpgradeMaskAddress: connect.DefaultDnsUpgradeMaskAddress}
	connect.AssertEqual(t, settings.toConnect(), connect.RegionalDnsResolverSettings("cn"))
	nowhere := &DnsResolverSettings{PresetId: "regional-zz"}
	connect.AssertEqual(t, nowhere.toConnect(), (&DnsResolverSettings{PresetId: DnsPresetIdDefault}).toConnect())
}

// A stored selection that cannot apply resolves as URnetwork's default, with
// no failover and no preset rules: a preset a release dropped, an id the
// catalog never knew, a preset hidden from a Play-only catalog, and a
// personal id that no longer validates (A7, O8).
func TestDnsPresetUnavailableSelectionsApplyDefault(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	defaults := (&DnsResolverSettings{PresetId: DnsPresetIdDefault}).toConnect()
	base := connect.DefaultUpgradeMuxSettings()
	check := func(presetId string, personalId string, providerName string) {
		t.Helper()
		settings := &DnsResolverSettings{PresetId: presetId, PresetPersonalId: personalId, EnableFallback: true}
		connect.AssertEqual(t, settings.toConnect(), defaults)
		selection := resolveDnsResolverSelection(presetId, personalId)
		if !selection.unavailable || selection.preset != nil {
			t.Errorf("%q applies as %+v", presetId, selection)
		}
		connect.AssertEqual(t, selection.unavailableProviderName, providerName)
		dns := upgradeMuxSettingsWithDnsResolverSettings(base, settings).Dns
		if dns.Failover != nil || dns.FallbackUntilTunnelReady {
			t.Errorf("%q applies preset rules", presetId)
		}
		connect.AssertEqual(t, dns.Fallback, hostFallbackDnsResolverSettings(defaults))
	}
	check("retired-preset", "", "Retired DNS")
	check("never-in-the-catalog", "", "")
	check("gamma-personal", "not-an-id", "Gamma")
	SetDnsPresetsHideAdBlocking(true)
	check("alpha-ads", "", "Alpha DNS")
	check("alpha-family", "", "Alpha DNS")
	SetDnsPresetsHideAdBlocking(false)
	if selection := resolveDnsResolverSelection("alpha-ads", ""); selection.unavailable {
		t.Fatal("a shown preset stays unavailable")
	}
}

// With no preset, nothing changes: default and custom settings, those an
// editor saved before the presets among them, resolve with exactly the lists
// they converted to before, with no pins, no failover, the fallback as the
// host projection and no connect window limit.
func TestDnsPresetNoPresetNoChange(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	base := connect.DefaultUpgradeMuxSettings()
	settingsList := []*DnsResolverSettings{
		GetDefaultDnsResolverSettings(),
		testingOpenDnsEraDefaultDnsResolverSettings(),
		{
			EnableRemoteDoh:       true,
			EnableLocalDns:        true,
			EnableFallback:        true,
			DnsUpgradeMaskAddress: "192.0.2.53",
			RemoteDohUrlsIpv4:     testing_stringList("https://192.0.2.80/dns-query", "https://doh.unlisted.example/dns-query"),
			RemoteDohUrlsIpv6:     testing_stringList("https://[2001:db8::80]/dns-query"),
			LocalDnsIpv4:          testing_stringList("192.0.2.81"),
			RemoteDnsIpv4:         testing_stringList("192.0.2.82"),
		},
		{EnableRemoteDns: true, RemoteDnsIpv4: testing_stringList("198.51.100.53")},
	}
	for _, settings := range settingsList {
		for _, enableFallback := range []bool{false, true} {
			settings = DnsResolverSettingsWithFallback(settings, enableFallback)
			before := settings.customToConnect()
			if settings.PresetId == DnsPresetIdDefault {
				before = defaultDnsResolver()
				before.DnsUpgradeMaskAddress = dnsUpgradeMaskAddress(settings.DnsUpgradeMaskAddress)
			}
			resolver := settings.toConnect()
			connect.AssertEqual(t, resolver, before)
			dns := upgradeMuxSettingsWithDnsResolverSettings(base, settings).Dns
			if dns.Failover != nil || dns.FallbackUntilTunnelReady {
				t.Errorf("%+v applies preset rules", settings)
			}
			if enableFallback {
				connect.AssertEqual(t, dns.Fallback, hostFallbackDnsResolverSettings(before))
			} else if dns.Fallback != nil {
				t.Errorf("Fast DNS off installed a fallback")
			}
			// the record keeps every list as given
			owned := ownDnsResolverSettings(settings)
			if settings.PresetId == "" {
				connect.AssertEqual(t, testingDnsResolverSettingsLists(owned), testingDnsResolverSettingsLists(dnsResolverSettingsFromConnect(settings.customToConnect())))
			}
		}
	}
}

// A setter refuses a selection no device can apply (E2), with a refusal that
// names no id; a dropped or hidden preset is accepted and applies as default.
func TestDnsPresetValidateSelection(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	for _, c := range []struct {
		presetId   string
		personalId string
		refused    bool
	}{
		{presetId: ""},
		{presetId: DnsPresetIdDefault},
		{presetId: "regional-cn"},
		{presetId: "regional-zz"},
		{presetId: "alpha-ads"},
		{presetId: "alpha-ads", personalId: "abc123"},
		{presetId: "retired-preset"},
		{presetId: "gamma-personal", personalId: "abc123"},
		{presetId: "gamma-personal", personalId: "https://id.gamma.example/ABC123"},
		{presetId: "gamma-personal", refused: true},
		{presetId: "gamma-personal", personalId: "abc12/", refused: true},
		{presetId: "gamma-personal", personalId: "https://dns.alpha.example/abc123", refused: true},
		{presetId: "no-such-preset", refused: true},
		{presetId: "regional-CN", refused: true},
		{presetId: "Default", refused: true},
	} {
		err := validateDnsResolverSettingsSelection(&DnsResolverSettings{PresetId: c.presetId, PresetPersonalId: c.personalId})
		if (err != nil) != c.refused {
			t.Errorf("%q %q: refused %v, want %t", c.presetId, c.personalId, err, c.refused)
		}
		if err != nil && c.personalId != "" && strings.Contains(err.Error(), c.personalId) {
			t.Errorf("the refusal names the personal id: %v", err)
		}
	}
	SetDnsPresetsHideAdBlocking(true)
	if err := validateDnsResolverSettingsSelection(&DnsResolverSettings{PresetId: "alpha-ads"}); err != nil {
		t.Errorf("a hidden preset kept by a Fast DNS toggle is refused: %v", err)
	}
}

// Lists edited under the default or a regional selection, as an editor that
// keeps the read-back struct as its draft saves them, apply as custom; an
// untouched read-back, a Fast DNS toggle and a preset with stale lists keep
// their selection.
func TestDnsResolverSettingsAsEditedAppliesEditsAsCustom(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	untouched := GetDefaultDnsResolverSettings()
	connect.AssertEqual(t, dnsResolverSettingsAsEdited(untouched).PresetId, DnsPresetIdDefault)
	toggled := DnsResolverSettingsWithFallback(untouched, true)
	connect.AssertEqual(t, dnsResolverSettingsAsEdited(toggled).PresetId, DnsPresetIdDefault)
	masked := GetDefaultDnsResolverSettings()
	masked.DnsUpgradeMaskAddress = "192.0.2.53"
	connect.AssertEqual(t, dnsResolverSettingsAsEdited(masked).PresetId, DnsPresetIdDefault)

	edited := GetDefaultDnsResolverSettings()
	edited.EnableLocalDns = true
	edited.LocalDnsIpv4 = testing_stringList("192.0.2.81")
	asEdited := dnsResolverSettingsAsEdited(edited)
	connect.AssertEqual(t, asEdited.PresetId, "")
	connect.AssertEqual(t, asEdited.LocalDnsIpv4.getAll(), []string{"192.0.2.81"})
	connect.AssertEqual(t, edited.PresetId, DnsPresetIdDefault)

	regional := GetRecommendedDnsResolverSettings("cn")
	regional.RemoteDnsIpv4 = testing_stringList("192.0.2.82")
	connect.AssertEqual(t, dnsResolverSettingsAsEdited(regional).PresetId, "")

	stale := testingPresetSettings(t, "alpha-ads", "")
	stale.RemoteDohUrlsIpv4 = testing_stringList("https://doh.unlisted.example/dns-query")
	kept := dnsResolverSettingsAsEdited(stale)
	connect.AssertEqual(t, kept.PresetId, "alpha-ads")
	connect.AssertEqual(t, ownDnsResolverSettings(kept).RemoteDohUrlsIpv4.getAll(), []string{"https://dns.alpha.example/dns-query"})
}

// The paste forms of a personal id (D3, D4, D6), on a synthetic provider:
// its dashboard url with or without a device name, its DoT hostname, a DoT
// url, upper case and whitespace; and what never reads as an id.
func TestDnsPresetPersonalIdPasteForms(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	testingErrorPersonalIdCases(t, "gamma-personal", []testingPersonalIdCase{
		{input: "abc123", normalized: "abc123"},
		{input: "\tABC123 ", normalized: "abc123"},
		{input: "https://id.gamma.example/abc123", normalized: "abc123"},
		{input: "https://id.gamma.example/abc123/", normalized: "abc123"},
		{input: "https://id.gamma.example/abc123/Kitchen", normalized: "abc123"},
		{input: "https://id.gamma.example:443/abc123?device=Kitchen#x", normalized: "abc123"},
		{input: "abc123.dot.gamma.example", normalized: "abc123"},
		{input: "Kitchen-abc123.dot.gamma.example.", normalized: "abc123"},
		{input: "tls://abc123.dot.gamma.example", normalized: "abc123"},
		{input: "https://id.gamma.example:8443/abc123", normalized: "https://id.gamma.example:8443/abc123", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://user:pw@id.gamma.example/abc123", normalized: "https://user:pw@id.gamma.example/abc123", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://id.gamma.example/abc123/Kitchen/extra", normalized: "https://id.gamma.example/abc123/Kitchen/extra", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://id.gamma.example/", normalized: "https://id.gamma.example/", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://id.gamma.example/abc%2F23", normalized: "https://id.gamma.example/abc%2F23", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://other.gamma.example/abc123", normalized: "https://other.gamma.example/abc123", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "sdns://AgcAAAAAAAAAAAAWcHJvdGVjdGl2ZQ", normalized: "sdns://AgcAAAAAAAAAAAAWcHJvdGVjdGl2ZQ", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "..", normalized: "..", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "%2e%2e", normalized: "%2e%2e", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abç123", normalized: "abç123", errorId: DnsPresetErrorPersonalIdInvalid},
		// another catalog provider's url or hostname, an ip-literal one included
		{input: "https://dns.alpha.example/dns-query", normalized: "https://dns.alpha.example/dns-query", errorId: DnsPresetErrorPersonalIdOtherProvider},
		{input: "1a2b3c4d.d.alpha.example", normalized: "1a2b3c4d.d.alpha.example", errorId: DnsPresetErrorPersonalIdOtherProvider},
		{input: "https://192.0.2.10/dns-query", normalized: "https://192.0.2.10/dns-query", errorId: DnsPresetErrorPersonalIdOtherProvider},
	})
	// the template's path segments before the id must match
	testingErrorPersonalIdCases(t, "alpha-personal", []testingPersonalIdCase{
		{input: "https://d.alpha.example/dns-query/1A2B3C4D", normalized: "1a2b3c4d"},
		{input: "https://d.alpha.example/other/1a2b3c4d", normalized: "https://d.alpha.example/other/1a2b3c4d", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://d.alpha.example/dns-query/1a2b3c4d/Kitchen", normalized: "https://d.alpha.example/dns-query/1a2b3c4d/Kitchen", errorId: DnsPresetErrorPersonalIdInvalid},
	})
}

// The id a template takes is parsed back into exactly its url (D4).
func TestDnsPresetPersonalIdUrlRebuild(t *testing.T) {
	personal := testingDnsPresetCatalog().preset("gamma-personal").personalId
	if built, ok := personal.dohUrl("abc123"); !ok || built != "https://id.gamma.example/abc123" {
		t.Fatalf("built %q, %t", built, ok)
	}
	for _, id := range []string{"abc/123", "abc?123", "abc#123", "abc%20", "..", "abc 123"} {
		if built, ok := personal.dohUrl(id); ok {
			t.Errorf("%q built %q", id, built)
		}
	}
}
