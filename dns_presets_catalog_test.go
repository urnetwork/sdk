package sdk

// The built-in dns preset catalog (connect ADBLOCK3RD A3, A4, D4, O9, O15 and
// the owner's decisions of 2026-11-09). These are the only tests that read
// the shipped catalog; every other preset test installs a catalog of
// synthetic `.example` providers. The catalog's rules run here as a test
// (F3's offline part), with no network: dnspresetcheck is what checks the
// endpoints live.

import (
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Every entry keeps the catalog's rules (A4).
func TestDnsPresetCatalogIntegrity(t *testing.T) {
	catalog := builtInDnsPresetCatalog()
	if len(catalog.presets) == 0 || len(catalog.providers) == 0 {
		t.Fatal("the catalog is empty")
	}
	idPattern := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	datePattern := regexp.MustCompile(`^20[0-9]{2}-[01][0-9]-[0-3][0-9]$`)
	providerIds := map[string]bool{}
	for _, provider := range catalog.providers {
		if providerIds[provider.providerId] {
			t.Errorf("provider %s appears twice", provider.providerId)
		}
		providerIds[provider.providerId] = true
		if !idPattern.MatchString(provider.providerId) {
			t.Errorf("provider id %q is not lower case letters, digits and hyphens", provider.providerId)
		}
		if provider.name == "" || provider.operator == "" || provider.jurisdiction == "" {
			t.Errorf("provider %s lacks a name, operator or jurisdiction", provider.providerId)
		}
		for _, link := range []string{provider.privacyUrl, provider.helpUrl} {
			if u, err := url.Parse(link); err != nil || u.Scheme != "https" || u.Host == "" {
				t.Errorf("provider %s link %q is not https", provider.providerId, link)
			}
		}
		if provider.termsUrl != "" {
			if u, err := url.Parse(provider.termsUrl); err != nil || u.Scheme != "https" || u.Host == "" {
				t.Errorf("provider %s terms %q is not https", provider.providerId, provider.termsUrl)
			}
		}
		if len(provider.domains) == 0 {
			t.Errorf("provider %s names no domain to recognize its pasted urls by", provider.providerId)
		}
	}
	presetIds := map[string]bool{}
	hostPins := map[string][]string{}
	providerPresetCounts := map[string]int{}
	for _, preset := range catalog.presets {
		id := preset.presetId
		if presetIds[id] {
			t.Errorf("preset %s appears twice", id)
		}
		presetIds[id] = true
		if !idPattern.MatchString(id) {
			t.Errorf("preset id %q is not lower case letters, digits and hyphens", id)
		}
		if id == DnsPresetIdDefault || strings.HasPrefix(id, DnsPresetIdRegionalPrefix) {
			t.Errorf("preset id %q is a reserved selection", id)
		}
		if _, reserved := catalog.reservedPresets[id]; reserved {
			t.Errorf("preset id %q reuses a dropped preset's id", id)
		}
		if !providerIds[preset.providerId] {
			t.Errorf("preset %s names an unknown provider %s", id, preset.providerId)
		}
		providerPresetCounts[preset.providerId] += 1
		if !datePattern.MatchString(preset.checked) {
			t.Errorf("preset %s has no checked date: %q", id, preset.checked)
		}
		// groups are consistent with the failure policy (decision 3)
		switch preset.group {
		case DnsPresetGroupAdblock, DnsPresetGroupUnfiltered:
			if !preset.failover {
				t.Errorf("preset %s in %s does not fail over", id, preset.group)
			}
		case DnsPresetGroupSecurity, DnsPresetGroupFamily, DnsPresetGroupPersonal:
			if preset.failover {
				t.Errorf("preset %s in %s fails over; it must fail closed", id, preset.group)
			}
		default:
			t.Errorf("preset %s has an unknown group %q", id, preset.group)
		}
		if preset.group == DnsPresetGroupAdblock && !preset.blocksAds {
			t.Errorf("preset %s sits in adblock but does not block ads", id)
		}
		if preset.group == DnsPresetGroupUnfiltered && (preset.blocksAds || preset.canaryName != "" || preset.blockAnswer != dnsPresetBlockAnswerNone) {
			t.Errorf("no-filtering preset %s claims to block", id)
		}
		if (preset.group == DnsPresetGroupPersonal) != (preset.personalId != nil) {
			t.Errorf("preset %s: the personal group and a personal id go together", id)
		}
		if preset.canaryName != "" && preset.blockAnswer == dnsPresetBlockAnswerNone {
			t.Errorf("preset %s has a canary but no block answer", id)
		}
		// urls: https, a path, no user info, query or fragment
		if len(preset.dohUrlsIpv4) == 0 || len(preset.dohUrlsIpv4) != len(preset.dohUrlsIpv6) {
			t.Errorf("preset %s lists %d v4 and %d v6 urls, want pairs", id, len(preset.dohUrlsIpv4), len(preset.dohUrlsIpv6))
			continue
		}
		hostnameUrls := 0
		for i, dohUrl := range slices.Concat(preset.dohUrlsIpv4, preset.dohUrlsIpv6) {
			u, err := url.Parse(dohUrl)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
				u.RawQuery != "" || u.Fragment != "" || u.Path == "" || u.Path == "/" || u.Port() != "" {
				t.Errorf("preset %s url %q is not an https url with a path and nothing else", id, dohUrl)
				continue
			}
			host := u.Hostname()
			addr, err := netip.ParseAddr(host)
			if err != nil {
				hostnameUrls += 1
				continue
			}
			// ip-literal urls: each list holds its family, in operator pair order
			if v4List := i < len(preset.dohUrlsIpv4); v4List != addr.Is4() {
				t.Errorf("preset %s url %q sits in the other family's list", id, dohUrl)
			}
		}
		pins := slices.Concat(preset.dohPinIpv4s, preset.dohPinIpv6s)
		switch {
		case hostnameUrls == 0:
			if len(pins) != 0 {
				t.Errorf("ip-literal preset %s carries pins", id)
			}
		case hostnameUrls == len(preset.dohUrlsIpv4)+len(preset.dohUrlsIpv6):
			// a hostname preset: the one url in both lists, pins of both families
			if len(preset.dohUrlsIpv4) != 1 || preset.dohUrlsIpv4[0] != preset.dohUrlsIpv6[0] {
				t.Errorf("hostname preset %s does not list its one url in both lists", id)
			}
			if len(preset.dohPinIpv4s) == 0 || len(preset.dohPinIpv6s) == 0 {
				t.Errorf("hostname preset %s lacks pins of both families", id)
			}
		default:
			t.Errorf("preset %s mixes ip-literal and hostname urls", id)
		}
		for i, pin := range pins {
			addr, err := netip.ParseAddr(pin)
			v4Pin := i < len(preset.dohPinIpv4s)
			if err != nil || addr.Is4() != v4Pin || addr.String() != pin {
				t.Errorf("preset %s pin %q is not a canonical address of its family", id, pin)
			}
		}
		if hostnameUrls != 0 {
			host := dnsPresetUrlHost(preset.dohUrlsIpv4[0])
			if previous, ok := hostPins[host]; ok && !slices.Equal(previous, pins) {
				t.Errorf("host %s has different pins in two presets", host)
			}
			hostPins[host] = pins
		}
		// a personal template has one placeholder, filling the last whole
		// path segment
		if personal := preset.personalId; personal != nil {
			if strings.Count(personal.dohUrlTemplate, dnsPresetPersonalIdPlaceholder) != 1 ||
				!strings.HasSuffix(personal.dohUrlTemplate, "/"+dnsPresetPersonalIdPlaceholder) {
				t.Errorf("preset %s template %q does not end in one placeholder segment", id, personal.dohUrlTemplate)
			}
			if preset.dohUrlsIpv4[0] != personal.dohUrlTemplate {
				t.Errorf("preset %s lists %q, not its template", id, preset.dohUrlsIpv4[0])
			}
			for _, pattern := range personal.idPatterns {
				source := pattern.String()
				if !strings.HasPrefix(source, "^") || !strings.HasSuffix(source, "$") {
					t.Errorf("preset %s id pattern %q is not anchored", id, source)
				}
			}
			if personal.example == "" || personal.formatKey == "" || personal.idHelpUrl == "" || personal.unknownIdAnswer == "" {
				t.Errorf("preset %s lacks its example, format key, help url or unknown-id answer", id)
			}
			if !personal.idValid(personal.normalizeCase(personal.example)) {
				t.Errorf("preset %s refuses its own example %q", id, personal.example)
			}
		}
	}
	for providerId := range providerIds {
		if providerPresetCounts[providerId] == 0 {
			t.Errorf("provider %s has no preset", providerId)
		}
	}
	for id, reserved := range catalog.reservedPresets {
		if presetIds[id] {
			t.Errorf("reserved id %s is in the catalog", id)
		}
		if reserved.name.key == "" || reserved.name.provider == "" || reserved.providerName == "" {
			t.Errorf("reserved id %s keeps no name for its notice", id)
		}
	}
	// every preset has a short, descriptive name of its own (owner,
	// 2026-11-09): its provider's short name and a name key of its group's
	// purpose. No two presets of the catalog show the same name, so a row, a
	// summary or a status line says which one is meant; and DNS4EU's two
	// family names (decision 20) say whether ads are blocked, as their presets
	// do.
	groupNameKeys := map[string][]string{
		DnsPresetGroupAdblock:    {"dns_preset_name_ad_blocking"},
		DnsPresetGroupSecurity:   {"dns_preset_name_malware_blocking"},
		DnsPresetGroupFamily:     {"dns_preset_name_family", "dns_preset_name_child", "dns_preset_name_child_ads"},
		DnsPresetGroupPersonal:   {"dns_preset_name_personal"},
		DnsPresetGroupUnfiltered: {"dns_preset_name_no_filtering"},
	}
	namePresetIds := map[dnsPresetName]string{}
	for _, preset := range catalog.presets {
		name := catalog.presetName(preset)
		if !slices.Contains(groupNameKeys[preset.group], name.key) {
			t.Errorf("preset %s in %s is named by %q", preset.presetId, preset.group, name.key)
		}
		if name.provider == "" {
			t.Errorf("preset %s names no provider", preset.presetId)
		}
		if other, ok := namePresetIds[name]; ok {
			t.Errorf("presets %s and %s both show %+v", other, preset.presetId, name)
		}
		namePresetIds[name] = preset.presetId
		if (name.key == "dns_preset_name_child_ads" && !preset.blocksAds) || (name.key == "dns_preset_name_child" && preset.blocksAds) {
			t.Errorf("preset %s is named %q, which says otherwise about ads", preset.presetId, name.key)
		}
	}
	// a short name shortens its provider's name, so it still says whose
	// resolver a preset is and keeps each group alphabetical by provider (O9)
	for _, provider := range catalog.providers {
		if provider.shortName != "" && (provider.shortName == provider.name || !strings.HasPrefix(provider.name, provider.shortName+" ")) {
			t.Errorf("provider %s short name %q does not shorten %q", provider.providerId, provider.shortName, provider.name)
		}
	}
}

// The id patterns admit only letters, digits and hyphens, so an id can never
// leave its path segment (A4, D4).
func TestDnsPresetPersonalIdPatternsAdmitOnlyLabelCharacters(t *testing.T) {
	for _, preset := range builtInDnsPresetCatalog().presets {
		personal := preset.personalId
		if personal == nil {
			continue
		}
		example := personal.normalizeCase(personal.example)
		for _, inserted := range []string{".", "/", "%", "?", "#", " ", "\t", "_", "+", "@", ":", "é", "\x00"} {
			for _, candidate := range []string{inserted + example, example[:3] + inserted + example[3:], example + inserted} {
				if personal.idValid(candidate) {
					t.Errorf("preset %s accepts %q", preset.presetId, candidate)
				}
			}
		}
	}
}

// The catalog as it ships: every id, name key, group, url, pin and canary of
// A3, as the owner decided it on 2026-11-09. RethinkDNS is not in it
// (decision 17), and every preset has a name of its own (decision 20, then
// per-preset names, owner, 2026-11-09).
func TestDnsPresetCatalogAsShipped(t *testing.T) {
	type expected struct {
		providerId  string
		nameKey     string
		group       string
		blocksAds   bool
		failover    bool
		dohUrlsIpv4 []string
		dohUrlsIpv6 []string
		dohPinIpv4s []string
		dohPinIpv6s []string
		canaryName  string
		blockAnswer string
	}
	hostname := func(dohUrl string) []string { return []string{dohUrl} }
	shipped := map[string]expected{
		"adguard": {
			providerId: "adguard", nameKey: "dns_preset_name_ad_blocking", group: DnsPresetGroupAdblock, blocksAds: true, failover: true,
			dohUrlsIpv4: hostname("https://dns.adguard-dns.com/dns-query"), dohUrlsIpv6: hostname("https://dns.adguard-dns.com/dns-query"),
			dohPinIpv4s: []string{"94.140.14.14", "94.140.15.15"}, dohPinIpv6s: []string{"2a10:50c0::ad1:ff", "2a10:50c0::ad2:ff"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"controld-ads": {
			providerId: "controld", nameKey: "dns_preset_name_ad_blocking", group: DnsPresetGroupAdblock, blocksAds: true, failover: true,
			dohUrlsIpv4: hostname("https://freedns.controld.com/p2"), dohUrlsIpv6: hostname("https://freedns.controld.com/p2"),
			dohPinIpv4s: []string{"76.76.2.11"}, dohPinIpv6s: []string{"2606:1a40::11"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"dns4eu-noads": {
			providerId: "dns4eu", nameKey: "dns_preset_name_ad_blocking", group: DnsPresetGroupAdblock, blocksAds: true, failover: true,
			dohUrlsIpv4: hostname("https://noads.joindns4.eu/dns-query"), dohUrlsIpv6: hostname("https://noads.joindns4.eu/dns-query"),
			dohPinIpv4s: []string{"86.54.11.13", "86.54.11.213"}, dohPinIpv6s: []string{"2a13:1001::86:54:11:13", "2a13:1001::86:54:11:213"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"cloudflare-security": {
			providerId: "cloudflare", nameKey: "dns_preset_name_malware_blocking", group: DnsPresetGroupSecurity,
			dohUrlsIpv4: []string{"https://1.1.1.2/dns-query", "https://1.0.0.2/dns-query"},
			dohUrlsIpv6: []string{"https://[2606:4700:4700::1112]/dns-query", "https://[2606:4700:4700::1002]/dns-query"},
			blockAnswer: dnsPresetBlockAnswerNull,
		},
		"dns4eu-protective": {
			providerId: "dns4eu", nameKey: "dns_preset_name_malware_blocking", group: DnsPresetGroupSecurity,
			dohUrlsIpv4: hostname("https://protective.joindns4.eu/dns-query"), dohUrlsIpv6: hostname("https://protective.joindns4.eu/dns-query"),
			dohPinIpv4s: []string{"86.54.11.1", "86.54.11.201"}, dohPinIpv6s: []string{"2a13:1001::86:54:11:1", "2a13:1001::86:54:11:201"},
			blockAnswer: dnsPresetBlockAnswerNull,
		},
		"quad9": {
			providerId: "quad9", nameKey: "dns_preset_name_malware_blocking", group: DnsPresetGroupSecurity,
			dohUrlsIpv4: []string{"https://9.9.9.9/dns-query", "https://149.112.112.112/dns-query"},
			dohUrlsIpv6: []string{"https://[2620:fe::fe]/dns-query", "https://[2620:fe::9]/dns-query"},
			canaryName:  "examplemalwaredomain.com", blockAnswer: dnsPresetBlockAnswerNxdomain,
		},
		"adguard-family": {
			providerId: "adguard", nameKey: "dns_preset_name_family", group: DnsPresetGroupFamily, blocksAds: true,
			dohUrlsIpv4: hostname("https://family.adguard-dns.com/dns-query"), dohUrlsIpv6: hostname("https://family.adguard-dns.com/dns-query"),
			dohPinIpv4s: []string{"94.140.14.15", "94.140.15.16"}, dohPinIpv6s: []string{"2a10:50c0::bad1:ff", "2a10:50c0::bad2:ff"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"cloudflare-family": {
			providerId: "cloudflare", nameKey: "dns_preset_name_family", group: DnsPresetGroupFamily,
			dohUrlsIpv4: []string{"https://1.1.1.3/dns-query", "https://1.0.0.3/dns-query"},
			dohUrlsIpv6: []string{"https://[2606:4700:4700::1113]/dns-query", "https://[2606:4700:4700::1003]/dns-query"},
			blockAnswer: dnsPresetBlockAnswerNull,
		},
		"controld-family": {
			providerId: "controld", nameKey: "dns_preset_name_family", group: DnsPresetGroupFamily, blocksAds: true,
			dohUrlsIpv4: hostname("https://freedns.controld.com/family"), dohUrlsIpv6: hostname("https://freedns.controld.com/family"),
			dohPinIpv4s: []string{"76.76.2.11"}, dohPinIpv6s: []string{"2606:1a40::11"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"dns4eu-child": {
			providerId: "dns4eu", nameKey: "dns_preset_name_child", group: DnsPresetGroupFamily,
			dohUrlsIpv4: hostname("https://child.joindns4.eu/dns-query"), dohUrlsIpv6: hostname("https://child.joindns4.eu/dns-query"),
			dohPinIpv4s: []string{"86.54.11.12", "86.54.11.212"}, dohPinIpv6s: []string{"2a13:1001::86:54:11:12", "2a13:1001::86:54:11:212"},
			blockAnswer: dnsPresetBlockAnswerNull,
		},
		"dns4eu-child-noads": {
			providerId: "dns4eu", nameKey: "dns_preset_name_child_ads", group: DnsPresetGroupFamily, blocksAds: true,
			dohUrlsIpv4: hostname("https://child-noads.joindns4.eu/dns-query"), dohUrlsIpv6: hostname("https://child-noads.joindns4.eu/dns-query"),
			dohPinIpv4s: []string{"86.54.11.11", "86.54.11.211"}, dohPinIpv6s: []string{"2a13:1001::86:54:11:11", "2a13:1001::86:54:11:211"},
			canaryName: "doubleclick.net", blockAnswer: dnsPresetBlockAnswerNull,
		},
		"adguard-unfiltered": {
			providerId: "adguard", nameKey: "dns_preset_name_no_filtering", group: DnsPresetGroupUnfiltered, failover: true,
			dohUrlsIpv4: hostname("https://unfiltered.adguard-dns.com/dns-query"), dohUrlsIpv6: hostname("https://unfiltered.adguard-dns.com/dns-query"),
			dohPinIpv4s: []string{"94.140.14.140", "94.140.14.141"}, dohPinIpv6s: []string{"2a10:50c0::1:ff", "2a10:50c0::2:ff"},
		},
		"cloudflare": {
			providerId: "cloudflare", nameKey: "dns_preset_name_no_filtering", group: DnsPresetGroupUnfiltered, failover: true,
			dohUrlsIpv4: []string{"https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"},
			dohUrlsIpv6: []string{"https://[2606:4700:4700::1111]/dns-query", "https://[2606:4700:4700::1001]/dns-query"},
		},
		"controld-unfiltered": {
			providerId: "controld", nameKey: "dns_preset_name_no_filtering", group: DnsPresetGroupUnfiltered, failover: true,
			dohUrlsIpv4: hostname("https://freedns.controld.com/p0"), dohUrlsIpv6: hostname("https://freedns.controld.com/p0"),
			dohPinIpv4s: []string{"76.76.2.11"}, dohPinIpv6s: []string{"2606:1a40::11"},
		},
		"dns4eu-unfiltered": {
			providerId: "dns4eu", nameKey: "dns_preset_name_no_filtering", group: DnsPresetGroupUnfiltered, failover: true,
			dohUrlsIpv4: hostname("https://unfiltered.joindns4.eu/dns-query"), dohUrlsIpv6: hostname("https://unfiltered.joindns4.eu/dns-query"),
			dohPinIpv4s: []string{"86.54.11.100", "86.54.11.200"}, dohPinIpv6s: []string{"2a13:1001::86:54:11:100", "2a13:1001::86:54:11:200"},
		},
		"adguard-personal": {
			providerId: "adguard", nameKey: "dns_preset_name_personal", group: DnsPresetGroupPersonal,
			dohUrlsIpv4: hostname("https://d.adguard-dns.com/dns-query/{id}"), dohUrlsIpv6: hostname("https://d.adguard-dns.com/dns-query/{id}"),
			dohPinIpv4s: []string{"94.140.14.49"}, dohPinIpv6s: []string{"2a10:50c0::ded:ff"},
		},
		"controld-personal": {
			providerId: "controld", nameKey: "dns_preset_name_personal", group: DnsPresetGroupPersonal,
			dohUrlsIpv4: hostname("https://dns.controld.com/{id}"), dohUrlsIpv6: hostname("https://dns.controld.com/{id}"),
			dohPinIpv4s: []string{"76.76.2.22"}, dohPinIpv6s: []string{"2606:1a40::22"},
		},
		"nextdns-personal": {
			providerId: "nextdns", nameKey: "dns_preset_name_personal", group: DnsPresetGroupPersonal,
			dohUrlsIpv4: hostname("https://dns.nextdns.io/{id}"), dohUrlsIpv6: hostname("https://dns.nextdns.io/{id}"),
			dohPinIpv4s: []string{"45.90.28.0", "45.90.30.0"}, dohPinIpv6s: []string{"2a07:a8c0::", "2a07:a8c1::"},
		},
	}
	catalog := builtInDnsPresetCatalog()
	if len(catalog.presets) != len(shipped) {
		t.Errorf("the catalog has %d presets, want %d", len(catalog.presets), len(shipped))
	}
	for _, preset := range catalog.presets {
		want, ok := shipped[preset.presetId]
		if !ok {
			t.Errorf("preset %s is not in the decided catalog", preset.presetId)
			continue
		}
		got := expected{
			providerId:  preset.providerId,
			nameKey:     preset.nameKey,
			group:       preset.group,
			blocksAds:   preset.blocksAds,
			failover:    preset.failover,
			dohUrlsIpv4: preset.dohUrlsIpv4,
			dohUrlsIpv6: preset.dohUrlsIpv6,
			dohPinIpv4s: preset.dohPinIpv4s,
			dohPinIpv6s: preset.dohPinIpv6s,
			canaryName:  preset.canaryName,
			blockAnswer: preset.blockAnswer,
		}
		connect.AssertEqual(t, got, want)
	}
	// the personal presets' providers' id rules (D4)
	connect.AssertEqual(t, catalog.preset("controld-personal").personalId.unknownIdAnswer, dnsPresetUnknownIdRefused)
	connect.AssertEqual(t, catalog.preset("adguard-personal").personalId.unknownIdAnswer, dnsPresetUnknownIdUnfilteredOrServfail)
	connect.AssertEqual(t, catalog.preset("nextdns-personal").personalId.unknownIdAnswer, dnsPresetUnknownIdUnfiltered)
	// the short names the presets' names begin with: AdGuard's alone differs
	// from its name
	shortNames := map[string]string{}
	for _, provider := range catalog.providers {
		if provider.shortName != "" {
			shortNames[provider.providerId] = provider.shortName
		}
	}
	connect.AssertEqual(t, shortNames, map[string]string{"adguard": "AdGuard"})
}

// The English of the presets' name keys, as the localization store has them
// (localizations keys/dns_preset_name_*.yaml, which
// TestDnsPresetNameKeysMatchTheStore checks when the store is at hand). Each
// key's {provider} is the provider's short name.
var testingDnsPresetNamesEnglish = map[string]string{
	"dns_preset_name_ad_blocking":      "{provider} Ad Blocking",
	"dns_preset_name_malware_blocking": "{provider} Malware Blocking",
	"dns_preset_name_family":           "{provider} Family",
	"dns_preset_name_child":            "{provider} Child",
	"dns_preset_name_child_ads":        "{provider} Child, no ads",
	"dns_preset_name_no_filtering":     "{provider} No Filtering",
	"dns_preset_name_personal":         "{provider} Personal",
}

// A preset's name as the apps render it in English.
func testingDnsPresetNameInEnglish(t *testing.T, nameKey string, nameProvider string) string {
	t.Helper()
	english, ok := testingDnsPresetNamesEnglish[nameKey]
	if !ok {
		t.Fatalf("%q is not a preset name key", nameKey)
	}
	return strings.Replace(english, "{provider}", nameProvider, 1)
}

// Every preset has a short, descriptive name of its own (owner, 2026-11-09),
// unique in the catalog, on every line that names the preset: the picker
// row, the provider list, the summary row, the status line and, under the
// Play-only catalog, the dropped notice of a hidden preset. DNS4EU's two
// family presets keep the names of decision 20. The lines that name the
// adblock provider keep its name, "AdGuard DNS" and "DNS4EU" included.
func TestDnsPresetNamesShowOnEveryLine(t *testing.T) {
	t.Cleanup(func() { SetDnsPresetsHideAdBlocking(false) })
	names := map[string]string{
		"adguard":             "AdGuard Ad Blocking",
		"controld-ads":        "Control D Ad Blocking",
		"dns4eu-noads":        "DNS4EU Ad Blocking",
		"cloudflare-security": "Cloudflare Malware Blocking",
		"dns4eu-protective":   "DNS4EU Malware Blocking",
		"quad9":               "Quad9 Malware Blocking",
		"adguard-family":      "AdGuard Family",
		"cloudflare-family":   "Cloudflare Family",
		"controld-family":     "Control D Family",
		"dns4eu-child":        "DNS4EU Child",
		"dns4eu-child-noads":  "DNS4EU Child, no ads",
		"adguard-personal":    "AdGuard Personal",
		"controld-personal":   "Control D Personal",
		"nextdns-personal":    "NextDNS Personal",
		"adguard-unfiltered":  "AdGuard No Filtering",
		"cloudflare":          "Cloudflare No Filtering",
		"controld-unfiltered": "Control D No Filtering",
		"dns4eu-unfiltered":   "DNS4EU No Filtering",
	}
	providerNames := map[string]string{
		"adguard":    "AdGuard DNS",
		"cloudflare": "Cloudflare",
		"controld":   "Control D",
		"dns4eu":     "DNS4EU",
		"nextdns":    "NextDNS",
		"quad9":      "Quad9",
	}
	// no two presets read alike, in a group or across groups
	namePresetIds := map[string]string{}
	for presetId, name := range names {
		if other, ok := namePresetIds[name]; ok {
			t.Errorf("%s and %s are both named %q", other, presetId, name)
		}
		namePresetIds[name] = presetId
	}

	// the picker rows and the provider list
	pickNames := map[string]string{}
	for _, pick := range GetDnsPicks("de").getAll() {
		if preset := pick.Preset; preset != nil {
			pickNames[preset.PresetId] = testingDnsPresetNameInEnglish(t, preset.NameKey, preset.NameProvider)
			connect.AssertEqual(t, preset.ProviderName, providerNames[preset.ProviderId])
			if pick.LabelKey != "" {
				t.Errorf("the preset pick %s has a label key %q", preset.PresetId, pick.LabelKey)
			}
		}
	}
	connect.AssertEqual(t, pickNames, names)
	listNames := map[string]string{}
	for _, provider := range GetDnsPresetProviders().getAll() {
		connect.AssertEqual(t, provider.Name, providerNames[provider.ProviderId])
		for _, preset := range provider.Presets.getAll() {
			listNames[preset.PresetId] = testingDnsPresetNameInEnglish(t, preset.NameKey, preset.NameProvider)
		}
	}
	connect.AssertEqual(t, listNames, names)

	// the summary row ("DNS: <name>") and the status line of each one
	// selected; the presets that block ads are kept for the Play-only notice
	adBlockingSettings := map[string]*DnsResolverSettings{}
	for presetId, name := range names {
		preset := GetDnsPreset(presetId)
		personalId := ""
		if preset.PersonalIdRequired {
			personalId = preset.PersonalIdExample
		}
		settings := DnsResolverSettingsWithPreset(nil, presetId, personalId)
		if settings == nil {
			t.Fatalf("%s cannot be selected", presetId)
		}
		summary := DescribeDnsResolverSettings(settings, "de")
		connect.AssertEqual(t,
			[]any{summary.SummaryKey, testingDnsPresetNameInEnglish(t, summary.NameKey, summary.NameProvider), summary.ProviderName},
			[]any{"dns_status_provider", name, providerNames[preset.ProviderId]})
		status := DescribeDnsResolverStatus(settings, nil)
		connect.AssertEqual(t,
			[]any{status.StatusKey, testingDnsPresetNameInEnglish(t, status.NameKey, status.NameProvider), status.ProviderName},
			[]any{"dns_status_provider", name, providerNames[preset.ProviderId]})
		if preset.BlocksAds {
			adBlockingSettings[presetId] = settings
		}
	}

	// the Play-only catalog hides the presets that block ads: each one's
	// notice names it as it showed
	SetDnsPresetsHideAdBlocking(true)
	for presetId, settings := range adBlockingSettings {
		dropped := DescribeDnsResolverSettings(settings, "de")
		connect.AssertEqual(t,
			[]any{dropped.Dropped, dropped.DroppedPresetId, testingDnsPresetNameInEnglish(t, dropped.DroppedNameKey, dropped.DroppedNameProvider)},
			[]any{true, presetId, names[presetId]})
	}
	connect.AssertEqual(t, len(adBlockingSettings), 6)
}

// RethinkDNS left the catalog (decision 17): no provider, preset, host or
// pin names it. A user can still enter it under Custom.
func TestDnsPresetCatalogLeavesOutRethinkDns(t *testing.T) {
	catalog := builtInDnsPresetCatalog()
	for _, provider := range catalog.providers {
		if strings.Contains(strings.ToLower(provider.providerId+provider.name+strings.Join(provider.domains, " ")), "rethink") {
			t.Errorf("provider %s names RethinkDNS", provider.providerId)
		}
	}
	for _, preset := range catalog.presets {
		if strings.Contains(strings.ToLower(preset.presetId+strings.Join(preset.dohUrlsIpv4, " ")), "rethink") {
			t.Errorf("preset %s names RethinkDNS", preset.presetId)
		}
	}
	for host := range catalog.hostPins {
		if strings.Contains(host, "rethink") {
			t.Errorf("host %s names RethinkDNS", host)
		}
	}
	if ValidateDnsPresetPersonalId("rethinkdns-unfiltered", "") != DnsPresetErrorUnknown {
		t.Error("a RethinkDNS preset id is selectable")
	}
}

// The picks in the screen's order (O9, F1): Default, Block ads and trackers,
// Block malware, Family, Use my account, No filtering, Custom, then Regional,
// providers alphabetical within each group; Regional only for a country with
// a recommendation.
func TestDnsPresetCatalogDisplayOrder(t *testing.T) {
	picks := func(exitCountryCode string) []string {
		ids := []string{}
		for _, pick := range GetDnsPicks(exitCountryCode).getAll() {
			ids = append(ids, pick.Kind+":"+pick.PresetId)
		}
		return ids
	}
	presetOrder := []string{
		"preset:adguard", "preset:controld-ads", "preset:dns4eu-noads",
		"preset:cloudflare-security", "preset:dns4eu-protective", "preset:quad9",
		"preset:adguard-family", "preset:cloudflare-family", "preset:controld-family", "preset:dns4eu-child", "preset:dns4eu-child-noads",
		"preset:adguard-personal", "preset:controld-personal", "preset:nextdns-personal",
		"preset:adguard-unfiltered", "preset:cloudflare", "preset:controld-unfiltered", "preset:dns4eu-unfiltered",
	}
	want := slices.Concat([]string{"default:default"}, presetOrder, []string{"custom:"})
	connect.AssertEqual(t, picks(""), want)
	connect.AssertEqual(t, picks("de"), want)
	connect.AssertEqual(t, picks("CN"), slices.Concat(want, []string{"regional:regional-cn"}))
}

// A Play-only catalog (O15, H4) hides exactly the presets that block ads:
// the four AdGuard and Control D presets and DNS4EU's two ad-blocking
// variants. Malware, family, no-filtering and personal presets stay.
func TestDnsPresetCatalogPlayOnlyHidesAdBlocking(t *testing.T) {
	t.Cleanup(func() { SetDnsPresetsHideAdBlocking(false) })
	all := map[string]bool{}
	for _, pick := range GetDnsPicks("").getAll() {
		if pick.Kind == DnsPickKindPreset {
			all[pick.PresetId] = true
		}
	}
	SetDnsPresetsHideAdBlocking(true)
	visible := map[string]bool{}
	for _, pick := range GetDnsPicks("").getAll() {
		if pick.Kind == DnsPickKindPreset {
			visible[pick.PresetId] = true
			if pick.Preset.BlocksAds {
				t.Errorf("the Play-only catalog shows %s", pick.PresetId)
			}
		}
	}
	for _, provider := range GetDnsPresetProviders().getAll() {
		for _, preset := range provider.Presets.getAll() {
			if preset.BlocksAds {
				t.Errorf("the Play-only providers list %s", preset.PresetId)
			}
		}
	}
	hidden := []string{}
	for id := range all {
		if !visible[id] {
			hidden = append(hidden, id)
		}
	}
	slices.Sort(hidden)
	connect.AssertEqual(t, hidden, []string{
		"adguard", "adguard-family", "controld-ads", "controld-family", "dns4eu-child-noads", "dns4eu-noads",
	})
	if GetDnsPreset("adguard") != nil || DnsResolverSettingsWithPreset(nil, "adguard", "") != nil {
		t.Error("a hidden preset can still be picked")
	}
	if GetDnsPreset("quad9") == nil || DnsResolverSettingsWithPreset(nil, "nextdns-personal", "abc123") == nil {
		t.Error("the Play-only catalog dropped a preset that blocks no ads")
	}
}

// One provider's personal id: what a user may type or paste, normalized, and
// the error id of each (D3, D4, D6).
type testingPersonalIdCase struct {
	input      string
	normalized string
	errorId    string
}

func testingErrorPersonalIdCases(t *testing.T, presetId string, cases []testingPersonalIdCase) {
	t.Helper()
	for _, c := range cases {
		if normalized := NormalizeDnsPresetPersonalId(presetId, c.input); normalized != c.normalized {
			t.Errorf("%s: %q normalized to %q, want %q", presetId, c.input, normalized, c.normalized)
		}
		if errorId := ValidateDnsPresetPersonalId(presetId, c.input); errorId != c.errorId {
			t.Errorf("%s: %q validated as %q, want %q", presetId, c.input, errorId, c.errorId)
		}
	}
}

func TestDnsPresetPersonalIdNextDns(t *testing.T) {
	testingErrorPersonalIdCases(t, "nextdns-personal", []testingPersonalIdCase{
		{input: "abc123", normalized: "abc123"},
		{input: "  ABC123\n", normalized: "abc123"},
		{input: "https://dns.nextdns.io/abc123", normalized: "abc123"},
		// a device name in the dashboard url is dropped (D6)
		{input: "https://dns.nextdns.io/abc123/Living-Room", normalized: "abc123"},
		{input: "https://DNS.NextDNS.io/ABC123/", normalized: "abc123"},
		{input: "abc123.dns.nextdns.io", normalized: "abc123"},
		{input: "Living-Room-abc123.dns.nextdns.io.", normalized: "abc123"},
		{input: "tls://abc123.dns.nextdns.io", normalized: "abc123"},
		{input: "", normalized: "", errorId: DnsPresetErrorPersonalIdRequired},
		{input: "abc12", normalized: "abc12", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc1234", normalized: "abc1234", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "zzzzzz", normalized: "zzzzzz", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc/23", normalized: "abc/23", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "../c123", normalized: "../c123", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc%2F", normalized: "abc%2f", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc?23", normalized: "abc?23", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc#23", normalized: "abc#23", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc12é", normalized: "abc12é", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc 23", normalized: "abc 23", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: strings.Repeat("a", 70), normalized: strings.Repeat("a", 70), errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://dns.nextdns.io/abc123/name/extra", normalized: "https://dns.nextdns.io/abc123/name/extra", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://user@dns.nextdns.io/abc123", normalized: "https://user@dns.nextdns.io/abc123", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "http://dns.nextdns.io/abc123", normalized: "http://dns.nextdns.io/abc123", errorId: DnsPresetErrorPersonalIdInvalid},
		// another catalog provider's url or hostname
		{input: "https://dns.controld.com/abcd1234", normalized: "https://dns.controld.com/abcd1234", errorId: DnsPresetErrorPersonalIdOtherProvider},
		{input: "1a2b3c4d.d.adguard-dns.com", normalized: "1a2b3c4d.d.adguard-dns.com", errorId: DnsPresetErrorPersonalIdOtherProvider},
	})
}

func TestDnsPresetPersonalIdControlD(t *testing.T) {
	testingErrorPersonalIdCases(t, "controld-personal", []testingPersonalIdCase{
		{input: "abcd1234", normalized: "abcd1234"},
		{input: " ABCD1234 ", normalized: "abcd1234"},
		{input: "https://dns.controld.com/abcd1234", normalized: "abcd1234"},
		{input: "https://dns.controld.com/abcd1234/kitchen-tablet", normalized: "abcd1234"},
		{input: "abcd1234.dns.controld.com", normalized: "abcd1234"},
		{input: "abcd1234-kitchen-tablet.dns.controld.com", normalized: "abcd1234"},
		{input: "abc", normalized: "abc", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: strings.Repeat("a", 33), normalized: strings.Repeat("a", 33), errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abcd-1234", normalized: "abcd-1234", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "", normalized: "", errorId: DnsPresetErrorPersonalIdRequired},
		{input: "https://freedns.controld.com/p2", normalized: "https://freedns.controld.com/p2", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://dns.nextdns.io/abc123", normalized: "https://dns.nextdns.io/abc123", errorId: DnsPresetErrorPersonalIdOtherProvider},
	})
}

func TestDnsPresetPersonalIdAdGuard(t *testing.T) {
	testingErrorPersonalIdCases(t, "adguard-personal", []testingPersonalIdCase{
		{input: "1a2b3c4d", normalized: "1a2b3c4d"},
		{input: "1A2B3C4D", normalized: "1a2b3c4d"},
		{input: "https://d.adguard-dns.com/dns-query/1a2b3c4d", normalized: "1a2b3c4d"},
		{input: "1a2b3c4d.d.adguard-dns.com", normalized: "1a2b3c4d"},
		{input: "quic://1a2b3c4d.d.adguard-dns.com", normalized: "1a2b3c4d"},
		// an automatic-device id keeps its name part as typed
		{input: "IOS-1A2B3C4D-My-Phone", normalized: "ios-1a2b3c4d-My-Phone"},
		{input: "ios-1a2b3c4d-My---Phone", normalized: "ios-1a2b3c4d-My---Phone", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "abc-1a2b3c4d-phone", normalized: "abc-1a2b3c4d-phone", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "ios-1a2b3c4d-" + strings.Repeat("a", 51), normalized: "ios-1a2b3c4d-" + strings.Repeat("a", 51), errorId: DnsPresetErrorPersonalIdInvalid},
		// the server would take seven hex digits; a typo answers unfiltered
		{input: "1a2b3c4", normalized: "1a2b3c4", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "1a2b3c4g", normalized: "1a2b3c4g", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://dns.adguard-dns.com/dns-query", normalized: "https://dns.adguard-dns.com/dns-query", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "https://d.adguard-dns.com/dns-query/1a2b3c4d/extra", normalized: "https://d.adguard-dns.com/dns-query/1a2b3c4d/extra", errorId: DnsPresetErrorPersonalIdInvalid},
		{input: "", normalized: "", errorId: DnsPresetErrorPersonalIdRequired},
		{input: "abcd1234.dns.controld.com", normalized: "abcd1234.dns.controld.com", errorId: DnsPresetErrorPersonalIdOtherProvider},
	})
}

// A selection that takes no id refuses one; an unknown selection is unknown.
func TestDnsPresetPersonalIdNotAccepted(t *testing.T) {
	for _, presetId := range []string{"adguard", "quad9", DnsPresetIdDefault, "regional-cn"} {
		connect.AssertEqual(t, ValidateDnsPresetPersonalId(presetId, ""), "")
		connect.AssertEqual(t, ValidateDnsPresetPersonalId(presetId, "abc123"), DnsPresetErrorPersonalIdNotAccepted)
		if presetId != DnsPresetIdDefault && presetId != "regional-cn" {
			connect.AssertEqual(t, NormalizeDnsPresetPersonalId(presetId, " abc123 "), "abc123")
		}
	}
	connect.AssertEqual(t, ValidateDnsPresetPersonalId("no-such-preset", ""), DnsPresetErrorUnknown)
	connect.AssertEqual(t, ValidateDnsPresetPersonalId("", ""), DnsPresetErrorUnknown)
}

// The provider pins of the shipped catalog follow each host (A6): a custom
// list that names a catalog host, or a personal preset's url with an id,
// dials the host's built-in addresses.
func TestDnsPresetCatalogPinsFollowTheHost(t *testing.T) {
	custom := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: testing_stringList("https://dns.nextdns.io/abc123", "https://dns.adguard-dns.com/dns-query"),
		RemoteDohUrlsIpv6: testing_stringList("https://dns.nextdns.io/abc123"),
	}
	connect.AssertEqual(t, custom.toConnect().DohServerHostAddrs, map[string][]string{
		"dns.nextdns.io":      {"45.90.28.0", "45.90.30.0", "2a07:a8c0::", "2a07:a8c1::"},
		"dns.adguard-dns.com": {"94.140.14.14", "94.140.15.15", "2a10:50c0::ad1:ff", "2a10:50c0::ad2:ff"},
	})
}
