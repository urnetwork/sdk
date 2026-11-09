// The third-party DNS presets (connect ADBLOCK3RD.md): the catalog of
// resolvers built into the sdk, the selection a DnsResolverSettings carries
// (PresetId and PresetPersonalId), and the expansion of a selection into the
// connect resolver settings the device applies.
//
// The catalog changes only with an sdk release (ADBLOCK3RD O13): there is no
// server copy and no withdraw list. A preset's addresses are built in too
// (O5): a hostname DoH url is dialed at the catalog's pins
// (connect.DnsResolverSettings.DohServerHostAddrs) and its host is never
// looked up. The device derives a selection's lists from the catalog each time
// it applies the settings, so a release that changes a preset reaches every
// device that selects it, and a release that drops one switches its devices to
// URnetwork's default DNS with a notice the apps show (A7, O8).
//
// The selections (DnsResolverSettings.PresetId):
//   - "" is custom: the explicit lists of today's editor, applied as given,
//     except that a DoH url whose host the catalog knows is dialed at the
//     catalog's pins (A6, "pins follow the host").
//   - "default" is URnetwork's built-in resolver, derived from connect's
//     defaults, so a device on it follows later default changes (E4). It is
//     the default everywhere and no preset is ever preselected (N1, O15).
//   - "regional-<cc>" is the regional recommendation of a country as a pick:
//     unencrypted remote dns, never selected automatically (O6).
//   - a catalog preset id. One-tap presets and the personal presets ("Use my
//     account"), which take the user's configuration id in PresetPersonalId.
//
// A stored id the catalog dropped or never knew, a preset hidden by
// SetDnsPresetsHideAdBlocking, a personal id that no longer validates, or a
// country that no longer has a recommendation applies as "default" (A7). The
// device keeps the stored id until the user picks again, so the summary
// (DescribeDnsResolverSettings) carries the notice.
//
// "default", custom and regional selections keep today's behavior: no
// failover, and the host-network fallback (Fast DNS on connect) as the host
// projection of the lists. A preset selection resolves only through the
// tunnel, with DoH alone (A6); its failover and Fast DNS rules are in
// upgradeMuxSettingsWithDnsResolverSettings (decisions 3 and 5).
//
// A personal configuration id travels only in the DoH path (D1). No error or
// log line in this file carries one (D5).
//
// This file is built for every target, the ios packet tunnel extension
// included, because the device derives the lists when it applies them. What
// only the screens call is in dns_presets_ui.go.
package sdk

import (
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// The reserved selections that are not catalog presets.
const (
	// URnetwork's built-in resolver, the default everywhere.
	DnsPresetIdDefault = "default"
	// The prefix of a regional pick, followed by the lower case country code.
	DnsPresetIdRegionalPrefix = "regional-"
)

// The outcome groups of the presets. The screen orders the picks Default,
// these groups in this order, Custom, then Regional (O9).
const (
	DnsPresetGroupAdblock    = "adblock"
	DnsPresetGroupSecurity   = "security"
	DnsPresetGroupFamily     = "family"
	DnsPresetGroupPersonal   = "personal"
	DnsPresetGroupUnfiltered = "unfiltered"
)

// The error ids of a preset selection and its personal id. Each is a
// localization key id (ADBLOCK3RD G1), as the bootstrap DoH error ids are
// (control_doh.go). A function that answers one names its result `errorId`.
const (
	// The preset id names no selectable preset.
	DnsPresetErrorUnknown = "dns_preset_error_unknown"
	// The personal preset needs the user's configuration id.
	DnsPresetErrorPersonalIdRequired = "dns_preset_error_personal_id_required"
	// The id does not have the provider's form; the preset's
	// PersonalIdFormatKey is the provider's format message (D4).
	DnsPresetErrorPersonalIdInvalid = "dns_preset_error_personal_id_invalid"
	// A url or hostname of another catalog provider was pasted.
	DnsPresetErrorPersonalIdOtherProvider = "dns_preset_error_personal_id_other_provider"
	// The selection takes no personal id.
	DnsPresetErrorPersonalIdNotAccepted = "dns_preset_error_personal_id_not_accepted"
)

// How a preset answers a name it blocks (ADBLOCK3RD A3, B4).
const (
	dnsPresetBlockAnswerNone     = ""
	dnsPresetBlockAnswerNull     = "null"
	dnsPresetBlockAnswerNxdomain = "nxdomain"
)

// How a provider answers a personal id it does not know (A3, C6).
const (
	dnsPresetUnknownIdUnfiltered           = "unfiltered"
	dnsPresetUnknownIdRefused              = "refused"
	dnsPresetUnknownIdUnfilteredOrServfail = "unfiltered-or-servfail"
)

// The placeholder of the personal id in a personal preset's DoH url.
const dnsPresetPersonalIdPlaceholder = "{id}"

// The longest personal id: one DNS label, which every provider's id also is
// in its DoT hostname form (D4).
const dnsPresetPersonalIdMaxLength = 63

// A provider whose resolvers the catalog offers. Names, operators and
// jurisdictions are proper nouns and places, not localized (G2).
type dnsPresetProvider struct {
	// stable: "adguard"
	providerId string
	name       string
	operator   string
	// where the operator is established and whose law applies (H2)
	jurisdiction string
	helpUrl      string
	privacyUrl   string
	// empty when the provider publishes none for its public resolver (H3)
	termsUrl string
	// the registrable domains of the provider's DoH and DoT hosts, so a url
	// or hostname of this provider pasted for another is recognized (D4)
	domains []string
}

// One selectable resolver (ADBLOCK3RD A2).
type dnsPreset struct {
	// stable and stored on devices, never reused (A4): "adguard"
	presetId   string
	providerId string
	group      string
	// whether it blocks ads; false shows "not ads" in its copy and keeps it in
	// a Play-only catalog (O15)
	blocksAds bool
	// the localization key of what it blocks (G1): one key per distinct text,
	// shared by presets that block the same
	copyKey string
	// ip-literal presets: one url per address, the v4 and v6 lists in
	// operator pair order. hostname presets: the one url in both lists. A
	// personal preset's url holds the placeholder.
	dohUrlsIpv4 []string
	dohUrlsIpv6 []string
	// the addresses a hostname url's host is dialed at (B1); empty for
	// ip-literal presets
	dohPinIpv4s []string
	dohPinIpv6s []string
	// how a block is answered (dnsPresetBlockAnswer*)
	blockAnswer string
	// a name the preset is known to block, for verification (C6); empty for
	// none
	canaryName string
	// whether a failing preset falls back to URnetwork's default resolver
	// through the tunnel (C1, decision 3)
	failover bool
	// nil unless the preset takes a personal id (D)
	personalId *dnsPresetPersonalId
	// the date the endpoints were last checked live (F3, dnspresetcheck)
	checked string
}

// The personal configuration id a personal preset takes (ADBLOCK3RD D).
type dnsPresetPersonalId struct {
	// the DoH url with one placeholder path segment, the last one (D1)
	dohUrlTemplate string
	// whether the dashboard's DoH url can carry a device name segment after
	// the id, which a paste drops (D3, D6)
	dohUrlNameSegment bool
	// the DoT (Android Private DNS) hostname with one capture group for the
	// id, matched in lower case; a device name in it is dropped (D3, D6)
	dotHostnamePattern *regexp.Regexp
	// anchored patterns an id must match after normalization, one of them
	// (D4). Each admits only [A-Za-z0-9-].
	idPatterns []*regexp.Regexp
	// substrings an id must never contain (D4)
	forbiddenSubstrings []string
	// the provider's case rule (D4)
	normalizeCase func(id string) string
	// the provider's own example, the field's placeholder (D3)
	example string
	// how the provider answers an unknown or malformed id
	// (dnsPresetUnknownId*), which verification reads (C6)
	unknownIdAnswer string
	// the provider's dashboard page that shows the id, also linked when the
	// profile cannot be confirmed (D3, C6)
	idHelpUrl string
	// the localization key of the provider's id format message (D4, G1)
	formatKey string
	// the localization key of where the provider shows the id (D3, G1)
	whereKey string
	// the localization key of the free-tier note under the field, or empty
	// (D3, G1)
	quotaNoteKey string
}

// The catalog: the providers, their presets, and the ids a release dropped.
// Immutable once built.
type dnsPresetCatalog struct {
	providers []*dnsPresetProvider
	presets   []*dnsPreset
	// the ids of presets a release dropped, each with its provider's name:
	// kept so a device that stored one switches to "default" with a notice
	// that names the provider, and so no id is ever reused (A4, A7)
	reservedProviderNames map[string]string

	providerIdProviders map[string]*dnsPresetProvider
	presetIdPresets     map[string]*dnsPreset
	// the pins of every hostname DoH host of the catalog, by lower case host
	hostPins map[string][]string
	// the provider of every DoH host of the catalog, by lower case host
	hostProviderIds map[string]string
}

func newDnsPresetCatalog(
	providers []*dnsPresetProvider,
	presets []*dnsPreset,
	reservedProviderNames map[string]string,
) *dnsPresetCatalog {
	catalog := &dnsPresetCatalog{
		providers:             providers,
		presets:               presets,
		reservedProviderNames: reservedProviderNames,
		providerIdProviders:   map[string]*dnsPresetProvider{},
		presetIdPresets:       map[string]*dnsPreset{},
		hostPins:              map[string][]string{},
		hostProviderIds:       map[string]string{},
	}
	for _, provider := range providers {
		catalog.providerIdProviders[provider.providerId] = provider
	}
	for _, preset := range presets {
		catalog.presetIdPresets[preset.presetId] = preset
		pins := slices.Concat(preset.dohPinIpv4s, preset.dohPinIpv6s)
		for _, dohUrl := range slices.Concat(preset.dohUrlsIpv4, preset.dohUrlsIpv6) {
			host := dnsPresetUrlHost(dohUrl)
			if host == "" {
				continue
			}
			catalog.hostProviderIds[host] = preset.providerId
			if 0 < len(pins) {
				catalog.hostPins[host] = pins
			}
		}
	}
	return catalog
}

func (self *dnsPresetCatalog) provider(providerId string) *dnsPresetProvider {
	return self.providerIdProviders[providerId]
}

func (self *dnsPresetCatalog) preset(presetId string) *dnsPreset {
	return self.presetIdPresets[presetId]
}

// The provider a url or hostname names, by the catalog's DoH hosts and the
// providers' domains, or empty.
func (self *dnsPresetCatalog) providerIdOfHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return ""
	}
	if providerId, ok := self.hostProviderIds[host]; ok {
		return providerId
	}
	for _, provider := range self.providers {
		for _, domain := range provider.domains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				return provider.providerId
			}
		}
	}
	return ""
}

// Production never sets it; tests install a catalog of synthetic `.example`
// providers to exercise rules the shipped catalog has no case for, such as a
// dropped preset.
var dnsPresetCatalogOverride atomic.Pointer[dnsPresetCatalog]

// Whether the presets that block ads are hidden (SetDnsPresetsHideAdBlocking).
var dnsPresetsHideAdBlocking atomic.Bool

// The catalog in force.
func currentDnsPresetCatalog() *dnsPresetCatalog {
	if catalog := dnsPresetCatalogOverride.Load(); catalog != nil {
		return catalog
	}
	return builtInDnsPresetCatalog()
}

// Hides every preset that blocks ads, for a Play-only catalog if Play review
// objects to DNS ad blocking (ADBLOCK3RD O15, H4). The android play flavor
// calls it at startup, before it builds the device. A stored selection it
// hides applies as "default" with the dropped-preset notice (A7); malware,
// family, no-filtering, personal ids and Custom stay. Process-wide, like the
// catalog itself.
func SetDnsPresetsHideAdBlocking(hide bool) {
	dnsPresetsHideAdBlocking.Store(hide)
}

// Whether the preset is hidden from this process's catalog.
func dnsPresetHidden(preset *dnsPreset) bool {
	return preset.blocksAds && dnsPresetsHideAdBlocking.Load()
}

// The lower case host of a url, the ip without brackets, or empty.
func dnsPresetUrlHost(rawUrl string) string {
	u, err := url.Parse(strings.TrimSpace(rawUrl))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Whether the host is an ip literal.
func dnsPresetHostIsIp(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// The catalog of ADBLOCK3RD A3, as the owner decided it on 2026-11-09
// (decisions 1, 15 and 17): seven ad-blocking, malware and family one-tap
// presets from AdGuard, Control D, Quad9 and Cloudflare, the No filtering
// picks, DNS4EU's five variants, and the personal ids of NextDNS, Control D
// and AdGuard. RethinkDNS is left out: its resolver forwards to Google
// upstream, and support inbox 8236 asked to avoid Google. Values are verbatim
// from the providers' pages (A3), and each entry passed dnspresetcheck (F3)
// on its checked date: on 2026-11-09 every url and pin over IPv4 (h2, the
// certificate for the url's host or ip, a normal name, the canary as listed,
// no HTTPS address hints, and resolution through connect's pinned client),
// from a host with no IPv6 route, so the IPv6 urls and pins wait for a run
// from a dual-stack host (ADBLOCK3RD section 10).
var builtInDnsPresetCatalog = sync.OnceValue(func() *dnsPresetCatalog {
	lowerCase := strings.ToLower
	return newDnsPresetCatalog(
		[]*dnsPresetProvider{
			{
				providerId:   "adguard",
				name:         "AdGuard DNS",
				operator:     "AdGuard Software Ltd",
				jurisdiction: "Cyprus",
				helpUrl:      "https://adguard-dns.io/kb/public-dns/overview/",
				privacyUrl:   "https://adguard-dns.io/en/privacy.html",
				termsUrl:     "https://adguard-dns.io/en/eula.html",
				domains:      []string{"adguard-dns.com", "adguard.com"},
			},
			{
				providerId:   "cloudflare",
				name:         "Cloudflare",
				operator:     "Cloudflare, Inc.",
				jurisdiction: "United States",
				helpUrl:      "https://developers.cloudflare.com/1.1.1.1/setup/",
				privacyUrl:   "https://developers.cloudflare.com/1.1.1.1/privacy/public-dns-resolver/",
				domains:      []string{"cloudflare-dns.com"},
			},
			{
				providerId:   "controld",
				name:         "Control D",
				operator:     "ControlD Inc",
				jurisdiction: "Canada",
				helpUrl:      "https://docs.controld.com/docs/free-dns",
				privacyUrl:   "https://controld.com/privacy",
				termsUrl:     "https://controld.com/terms",
				domains:      []string{"controld.com"},
			},
			{
				providerId:   "dns4eu",
				name:         "DNS4EU",
				operator:     "Whalebone, s.r.o.",
				jurisdiction: "Czech Republic",
				helpUrl:      "https://www.joindns4.eu/for-public",
				privacyUrl:   "https://legal-documents-dns4eu.s3.fr-par.scw.cloud/DNS4EU-Public-DNS-Resolver-policy-2025.pdf",
				termsUrl:     "https://legal-documents-dns4eu.s3.fr-par.scw.cloud/DNS4EU-Public-Resolver-Terms-of-Use.pdf",
				domains:      []string{"joindns4.eu"},
			},
			{
				providerId:   "nextdns",
				name:         "NextDNS",
				operator:     "NextDNS Inc.",
				jurisdiction: "United States",
				helpUrl:      "https://help.nextdns.io/",
				privacyUrl:   "https://nextdns.io/privacy",
				termsUrl:     "https://nextdns.io/terms",
				domains:      []string{"nextdns.io"},
			},
			{
				providerId:   "quad9",
				name:         "Quad9",
				operator:     "Quad9 Foundation",
				jurisdiction: "Switzerland",
				helpUrl:      "https://quad9.net/service/service-addresses-and-features",
				privacyUrl:   "https://quad9.net/privacy/policy/",
				domains:      []string{"quad9.net"},
			},
		},
		[]*dnsPreset{
			// block ads and trackers
			{
				presetId:    "adguard",
				providerId:  "adguard",
				group:       DnsPresetGroupAdblock,
				copyKey:     "dns_preset_copy_ads_phishing",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://dns.adguard-dns.com/dns-query"},
				dohUrlsIpv6: []string{"https://dns.adguard-dns.com/dns-query"},
				dohPinIpv4s: []string{"94.140.14.14", "94.140.15.15"},
				dohPinIpv6s: []string{"2a10:50c0::ad1:ff", "2a10:50c0::ad2:ff"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				failover:    true,
				checked:     "2026-11-09",
			},
			{
				presetId:    "controld-ads",
				providerId:  "controld",
				group:       DnsPresetGroupAdblock,
				copyKey:     "dns_preset_copy_ads_malware",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://freedns.controld.com/p2"},
				dohUrlsIpv6: []string{"https://freedns.controld.com/p2"},
				dohPinIpv4s: []string{"76.76.2.11"},
				dohPinIpv6s: []string{"2606:1a40::11"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				failover:    true,
				checked:     "2026-11-09",
			},
			{
				presetId:    "dns4eu-noads",
				providerId:  "dns4eu",
				group:       DnsPresetGroupAdblock,
				copyKey:     "dns_preset_copy_ads_malware_phishing",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://noads.joindns4.eu/dns-query"},
				dohUrlsIpv6: []string{"https://noads.joindns4.eu/dns-query"},
				dohPinIpv4s: []string{"86.54.11.13", "86.54.11.213"},
				dohPinIpv6s: []string{"2a13:1001::86:54:11:13", "2a13:1001::86:54:11:213"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				failover:    true,
				checked:     "2026-11-09",
			},
			// block malware, not ads
			{
				presetId:    "cloudflare-security",
				providerId:  "cloudflare",
				group:       DnsPresetGroupSecurity,
				copyKey:     "dns_preset_copy_malware",
				dohUrlsIpv4: []string{"https://1.1.1.2/dns-query", "https://1.0.0.2/dns-query"},
				dohUrlsIpv6: []string{"https://[2606:4700:4700::1112]/dns-query", "https://[2606:4700:4700::1002]/dns-query"},
				blockAnswer: dnsPresetBlockAnswerNull,
				checked:     "2026-11-09",
			},
			{
				presetId:    "dns4eu-protective",
				providerId:  "dns4eu",
				group:       DnsPresetGroupSecurity,
				copyKey:     "dns_preset_copy_malware_phishing",
				dohUrlsIpv4: []string{"https://protective.joindns4.eu/dns-query"},
				dohUrlsIpv6: []string{"https://protective.joindns4.eu/dns-query"},
				dohPinIpv4s: []string{"86.54.11.1", "86.54.11.201"},
				dohPinIpv6s: []string{"2a13:1001::86:54:11:1", "2a13:1001::86:54:11:201"},
				blockAnswer: dnsPresetBlockAnswerNull,
				checked:     "2026-11-09",
			},
			{
				presetId:    "quad9",
				providerId:  "quad9",
				group:       DnsPresetGroupSecurity,
				copyKey:     "dns_preset_copy_malware_phishing",
				dohUrlsIpv4: []string{"https://9.9.9.9/dns-query", "https://149.112.112.112/dns-query"},
				dohUrlsIpv6: []string{"https://[2620:fe::fe]/dns-query", "https://[2620:fe::9]/dns-query"},
				blockAnswer: dnsPresetBlockAnswerNxdomain,
				canaryName:  "examplemalwaredomain.com",
				checked:     "2026-11-09",
			},
			// family
			{
				presetId:    "adguard-family",
				providerId:  "adguard",
				group:       DnsPresetGroupFamily,
				copyKey:     "dns_preset_copy_ads_adult_safe_search",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://family.adguard-dns.com/dns-query"},
				dohUrlsIpv6: []string{"https://family.adguard-dns.com/dns-query"},
				dohPinIpv4s: []string{"94.140.14.15", "94.140.15.16"},
				dohPinIpv6s: []string{"2a10:50c0::bad1:ff", "2a10:50c0::bad2:ff"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				checked:     "2026-11-09",
			},
			{
				presetId:    "cloudflare-family",
				providerId:  "cloudflare",
				group:       DnsPresetGroupFamily,
				copyKey:     "dns_preset_copy_malware_adult",
				dohUrlsIpv4: []string{"https://1.1.1.3/dns-query", "https://1.0.0.3/dns-query"},
				dohUrlsIpv6: []string{"https://[2606:4700:4700::1113]/dns-query", "https://[2606:4700:4700::1003]/dns-query"},
				blockAnswer: dnsPresetBlockAnswerNull,
				checked:     "2026-11-09",
			},
			{
				presetId:    "controld-family",
				providerId:  "controld",
				group:       DnsPresetGroupFamily,
				copyKey:     "dns_preset_copy_ads_malware_adult_drugs",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://freedns.controld.com/family"},
				dohUrlsIpv6: []string{"https://freedns.controld.com/family"},
				dohPinIpv4s: []string{"76.76.2.11"},
				dohPinIpv6s: []string{"2606:1a40::11"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				checked:     "2026-11-09",
			},
			{
				presetId:    "dns4eu-child",
				providerId:  "dns4eu",
				group:       DnsPresetGroupFamily,
				copyKey:     "dns_preset_copy_child",
				dohUrlsIpv4: []string{"https://child.joindns4.eu/dns-query"},
				dohUrlsIpv6: []string{"https://child.joindns4.eu/dns-query"},
				dohPinIpv4s: []string{"86.54.11.12", "86.54.11.212"},
				dohPinIpv6s: []string{"2a13:1001::86:54:11:12", "2a13:1001::86:54:11:212"},
				blockAnswer: dnsPresetBlockAnswerNull,
				checked:     "2026-11-09",
			},
			{
				presetId:    "dns4eu-child-noads",
				providerId:  "dns4eu",
				group:       DnsPresetGroupFamily,
				copyKey:     "dns_preset_copy_child_ads",
				blocksAds:   true,
				dohUrlsIpv4: []string{"https://child-noads.joindns4.eu/dns-query"},
				dohUrlsIpv6: []string{"https://child-noads.joindns4.eu/dns-query"},
				dohPinIpv4s: []string{"86.54.11.11", "86.54.11.211"},
				dohPinIpv6s: []string{"2a13:1001::86:54:11:11", "2a13:1001::86:54:11:211"},
				blockAnswer: dnsPresetBlockAnswerNull,
				canaryName:  "doubleclick.net",
				checked:     "2026-11-09",
			},
			// no filtering
			{
				presetId:    "adguard-unfiltered",
				providerId:  "adguard",
				group:       DnsPresetGroupUnfiltered,
				copyKey:     "dns_preset_copy_nothing",
				dohUrlsIpv4: []string{"https://unfiltered.adguard-dns.com/dns-query"},
				dohUrlsIpv6: []string{"https://unfiltered.adguard-dns.com/dns-query"},
				dohPinIpv4s: []string{"94.140.14.140", "94.140.14.141"},
				dohPinIpv6s: []string{"2a10:50c0::1:ff", "2a10:50c0::2:ff"},
				failover:    true,
				checked:     "2026-11-09",
			},
			{
				presetId:    "cloudflare",
				providerId:  "cloudflare",
				group:       DnsPresetGroupUnfiltered,
				copyKey:     "dns_preset_copy_nothing",
				dohUrlsIpv4: []string{"https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"},
				dohUrlsIpv6: []string{"https://[2606:4700:4700::1111]/dns-query", "https://[2606:4700:4700::1001]/dns-query"},
				failover:    true,
				checked:     "2026-11-09",
			},
			{
				presetId:    "controld-unfiltered",
				providerId:  "controld",
				group:       DnsPresetGroupUnfiltered,
				copyKey:     "dns_preset_copy_nothing",
				dohUrlsIpv4: []string{"https://freedns.controld.com/p0"},
				dohUrlsIpv6: []string{"https://freedns.controld.com/p0"},
				dohPinIpv4s: []string{"76.76.2.11"},
				dohPinIpv6s: []string{"2606:1a40::11"},
				failover:    true,
				checked:     "2026-11-09",
			},
			{
				// court-ordered blocks still apply to every DNS4EU variant (A3)
				presetId:    "dns4eu-unfiltered",
				providerId:  "dns4eu",
				group:       DnsPresetGroupUnfiltered,
				copyKey:     "dns_preset_copy_court_orders",
				dohUrlsIpv4: []string{"https://unfiltered.joindns4.eu/dns-query"},
				dohUrlsIpv6: []string{"https://unfiltered.joindns4.eu/dns-query"},
				dohPinIpv4s: []string{"86.54.11.100", "86.54.11.200"},
				dohPinIpv6s: []string{"2a13:1001::86:54:11:100", "2a13:1001::86:54:11:200"},
				failover:    true,
				checked:     "2026-11-09",
			},
			// use my account
			{
				presetId:    "adguard-personal",
				providerId:  "adguard",
				group:       DnsPresetGroupPersonal,
				copyKey:     "dns_preset_copy_personal",
				dohUrlsIpv4: []string{"https://d.adguard-dns.com/dns-query/{id}"},
				dohUrlsIpv6: []string{"https://d.adguard-dns.com/dns-query/{id}"},
				dohPinIpv4s: []string{"94.140.14.49"},
				dohPinIpv6s: []string{"2a10:50c0::ded:ff"},
				personalId: &dnsPresetPersonalId{
					dohUrlTemplate:     "https://d.adguard-dns.com/dns-query/{id}",
					dotHostnamePattern: regexp.MustCompile(`^([a-z0-9-]{1,63})\.d\.adguard-dns\.com\.?$`),
					idPatterns: []*regexp.Regexp{
						// a DeviceID
						regexp.MustCompile(`^[0-9a-f]{8}$`),
						// an automatic-device id: type, server id and device name
						regexp.MustCompile(`^(adr|gam|ios|lnx|mac|rtr|stv|win|otr)-[0-9a-f]{8}-[A-Za-z0-9](?:[A-Za-z0-9-]{0,48}[A-Za-z0-9])?$`),
					},
					forbiddenSubstrings: []string{"---"},
					// a DeviceID is lower case; an automatic-device id lowers its
					// type and server id and keeps its name part as typed
					normalizeCase: func(id string) string {
						parts := strings.SplitN(id, "-", 3)
						if len(parts) == 3 {
							return strings.ToLower(parts[0]) + "-" + strings.ToLower(parts[1]) + "-" + parts[2]
						}
						return strings.ToLower(id)
					},
					example:         "1a2b3c4d",
					unknownIdAnswer: dnsPresetUnknownIdUnfilteredOrServfail,
					idHelpUrl:       "https://adguard-dns.io/en/dashboard/",
					formatKey:       "dns_personal_id_format_adguard",
					whereKey:        "dns_personal_id_where_adguard",
					quotaNoteKey:    "dns_personal_id_quota_adguard",
				},
				checked: "2026-11-09",
			},
			{
				presetId:    "controld-personal",
				providerId:  "controld",
				group:       DnsPresetGroupPersonal,
				copyKey:     "dns_preset_copy_personal",
				dohUrlsIpv4: []string{"https://dns.controld.com/{id}"},
				dohUrlsIpv6: []string{"https://dns.controld.com/{id}"},
				dohPinIpv4s: []string{"76.76.2.22"},
				dohPinIpv6s: []string{"2606:1a40::22"},
				personalId: &dnsPresetPersonalId{
					dohUrlTemplate:     "https://dns.controld.com/{id}",
					dohUrlNameSegment:  true,
					dotHostnamePattern: regexp.MustCompile(`^([a-z0-9]+)(?:-[a-z0-9-]+)?\.dns\.controld\.com\.?$`),
					// provisional: Control D publishes neither the alphabet nor
					// the length, and the id works as a dns label (D4)
					idPatterns:      []*regexp.Regexp{regexp.MustCompile(`^[a-z0-9]{4,32}$`)},
					normalizeCase:   lowerCase,
					example:         "abcd1234",
					unknownIdAnswer: dnsPresetUnknownIdRefused,
					idHelpUrl:       "https://controld.com/dashboard",
					formatKey:       "dns_personal_id_format_controld",
					whereKey:        "dns_personal_id_where_controld",
				},
				checked: "2026-11-09",
			},
			{
				presetId:    "nextdns-personal",
				providerId:  "nextdns",
				group:       DnsPresetGroupPersonal,
				copyKey:     "dns_preset_copy_personal",
				dohUrlsIpv4: []string{"https://dns.nextdns.io/{id}"},
				dohUrlsIpv6: []string{"https://dns.nextdns.io/{id}"},
				dohPinIpv4s: []string{"45.90.28.0", "45.90.30.0"},
				dohPinIpv6s: []string{"2a07:a8c0::", "2a07:a8c1::"},
				personalId: &dnsPresetPersonalId{
					dohUrlTemplate:     "https://dns.nextdns.io/{id}",
					dohUrlNameSegment:  true,
					dotHostnamePattern: regexp.MustCompile(`^(?:.+-)?([0-9a-f]{6})\.dns\.nextdns\.io\.?$`),
					// the official installers' check
					idPatterns:      []*regexp.Regexp{regexp.MustCompile(`^[0-9a-f]{6}$`)},
					normalizeCase:   lowerCase,
					example:         "abc123",
					unknownIdAnswer: dnsPresetUnknownIdUnfiltered,
					idHelpUrl:       "https://my.nextdns.io/",
					formatKey:       "dns_personal_id_format_nextdns",
					whereKey:        "dns_personal_id_where_nextdns",
					quotaNoteKey:    "dns_personal_id_quota_nextdns",
				},
				checked: "2026-11-09",
			},
		},
		// no release has dropped a preset yet
		map[string]string{},
	)
})

// A selection resolved against the catalog in force: what the device applies.
type dnsResolverSelection struct {
	// the id as stored, empty for custom
	presetId string
	// the catalog preset the selection applies, nil unless it is an
	// available one-tap or personal preset
	preset *dnsPreset
	// the normalized personal id of a personal preset
	personalId string
	// the country of a regional pick
	countryCode string
	// set when the stored selection cannot apply and "default" applies in its
	// place (A7): a preset the catalog dropped, hid or never knew, a personal
	// id that no longer validates, or a country with no recommendation
	unavailable bool
	// the name of the provider of an unavailable preset, empty when the
	// catalog never knew it
	unavailableProviderName string
}

// Resolves a stored selection against the catalog in force.
func resolveDnsResolverSelection(presetId string, personalId string) dnsResolverSelection {
	selection := dnsResolverSelection{presetId: presetId}
	switch {
	case presetId == "" || presetId == DnsPresetIdDefault:
	case strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix):
		selection.countryCode = strings.TrimPrefix(presetId, DnsPresetIdRegionalPrefix)
		if !dnsRegionalCountryCodeValid(selection.countryCode) || !HasRegionalDnsRecommendation(selection.countryCode) {
			selection.unavailable = true
		}
	default:
		catalog := currentDnsPresetCatalog()
		if preset := catalog.preset(presetId); preset != nil {
			provider := catalog.provider(preset.providerId)
			switch {
			case dnsPresetHidden(preset):
				selection.unavailable = true
			case preset.personalId != nil:
				normalizedId := normalizeDnsPresetPersonalId(preset, personalId)
				if validateDnsPresetPersonalIdOf(catalog, preset, personalId) != "" {
					selection.unavailable = true
				} else {
					selection.preset = preset
					selection.personalId = normalizedId
				}
			default:
				selection.preset = preset
			}
			if selection.unavailable && provider != nil {
				selection.unavailableProviderName = provider.name
			}
		} else {
			selection.unavailable = true
			selection.unavailableProviderName = catalog.reservedProviderNames[presetId]
		}
	}
	return selection
}

// Whether the selection is custom: the explicit lists.
func (self dnsResolverSelection) custom() bool {
	return self.presetId == ""
}

// Whether the selection applies a regional recommendation.
func (self dnsResolverSelection) regional() bool {
	return self.countryCode != "" && !self.unavailable
}

// The key that changes exactly when the selection does: the id and the
// personal id as stored. A Fast DNS toggle keeps it.
func dnsResolverSelectionKey(presetId string, personalId string) string {
	return presetId + "\x00" + personalId
}

// A regional pick's country code is two lower case ascii letters.
func dnsRegionalCountryCodeValid(countryCode string) bool {
	if len(countryCode) != 2 {
		return false
	}
	for _, c := range countryCode {
		if c < 'a' || 'z' < c {
			return false
		}
	}
	return true
}

// URnetwork's built-in resolver, which "default" applies and which an
// ad-blocking or no-filtering preset fails over to: the mux's default
// resolver, a deep copy.
func defaultDnsResolver() *connect.DnsResolverSettings {
	return cloneConnectDnsResolverSettings(connect.DefaultUpgradeMuxSettings().Dns.Resolver)
}

// A deep copy of a connect resolver setting's lists and pins. TlsConfig is
// shared, as connect's own copies share it.
func cloneConnectDnsResolverSettings(resolver *connect.DnsResolverSettings) *connect.DnsResolverSettings {
	if resolver == nil {
		return nil
	}
	cloned := *resolver
	cloned.RemoteDohUrlsIpv4 = slices.Clone(resolver.RemoteDohUrlsIpv4)
	cloned.RemoteDohUrlsIpv6 = slices.Clone(resolver.RemoteDohUrlsIpv6)
	cloned.LocalDohUrlsIpv4 = slices.Clone(resolver.LocalDohUrlsIpv4)
	cloned.LocalDohUrlsIpv6 = slices.Clone(resolver.LocalDohUrlsIpv6)
	cloned.RemoteDnsIpv4 = slices.Clone(resolver.RemoteDnsIpv4)
	cloned.RemoteDnsIpv6 = slices.Clone(resolver.RemoteDnsIpv6)
	cloned.LocalDnsIpv4 = slices.Clone(resolver.LocalDnsIpv4)
	cloned.LocalDnsIpv6 = slices.Clone(resolver.LocalDnsIpv6)
	if resolver.DohServerHostAddrs != nil {
		cloned.DohServerHostAddrs = map[string][]string{}
		for host, addrs := range resolver.DohServerHostAddrs {
			cloned.DohServerHostAddrs[host] = slices.Clone(addrs)
		}
	}
	return &cloned
}

// The preset's resolver (ADBLOCK3RD A6): DoH alone, through the tunnel, the
// lists exclusive to the preset's urls with the personal id filled in, no
// remote plain dns, so a filter's NXDOMAIN is never answered by a later step
// and a missing pin fails closed. The pins are attached by the caller, as
// for every selection (dnsResolverPins).
func (self *dnsPreset) resolver(personalId string) *connect.DnsResolverSettings {
	fill := func(dohUrls []string) []string {
		filled := make([]string, 0, len(dohUrls))
		for _, dohUrl := range dohUrls {
			filled = append(filled, strings.Replace(dohUrl, dnsPresetPersonalIdPlaceholder, personalId, 1))
		}
		return filled
	}
	return &connect.DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: fill(self.dohUrlsIpv4),
		RemoteDohUrlsIpv6: fill(self.dohUrlsIpv6),
	}
}

// The pins of every DoH url of a resolver whose host the catalog knows: pins
// follow the host, not the selection (A6), so custom lists that name a
// catalog host, or a preset's lists saved as custom, dial the same addresses
// with no plaintext lookup. Nil when no url names a catalog host, which keeps
// every other resolver as it was.
func dnsResolverPins(resolver *connect.DnsResolverSettings) map[string][]string {
	hostPins := currentDnsPresetCatalog().hostPins
	var pins map[string][]string
	for _, dohUrl := range slices.Concat(
		resolver.RemoteDohUrlsIpv4,
		resolver.RemoteDohUrlsIpv6,
		resolver.LocalDohUrlsIpv4,
		resolver.LocalDohUrlsIpv6,
	) {
		host := dnsPresetUrlHost(dohUrl)
		if hostAddrs, ok := hostPins[host]; ok {
			if pins == nil {
				pins = map[string][]string{}
			}
			pins[host] = slices.Clone(hostAddrs)
		}
	}
	return pins
}

// Normalizes the personal id a user typed or pasted for a personal preset
// (D3): trimmed, taken out of the provider's dashboard DoH url or DoT
// hostname with any device name dropped (D6), and the provider's case rule
// applied. Input it cannot read is returned trimmed, for the validator to
// reject. Other presets return the input trimmed.
func normalizeDnsPresetPersonalId(preset *dnsPreset, input string) string {
	input = strings.TrimSpace(input)
	if preset == nil || preset.personalId == nil {
		return input
	}
	personal := preset.personalId
	if strings.Contains(input, "://") {
		if id, ok := personal.idFromUrl(input); ok {
			return personal.normalizeCase(id)
		}
		return input
	}
	if strings.Contains(input, ".") {
		if id, ok := personal.idFromDotHostname(input); ok {
			return personal.normalizeCase(id)
		}
		return input
	}
	return personal.normalizeCase(input)
}

// The id in the provider's dashboard DoH url: https, the template's host, no
// user info, the template's path with the id segment in place, and at most a
// device name segment after it where the provider has one, which is dropped
// with any query or fragment (D6). A DoT url (tls:// or quic://) is read as
// its hostname.
func (self *dnsPresetPersonalId) idFromUrl(input string) (string, bool) {
	u, err := url.Parse(input)
	if err != nil || u.User != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "tls", "quic":
		return self.idFromDotHostname(u.Hostname())
	case "https":
	default:
		return "", false
	}
	if u.Port() != "" && u.Port() != "443" {
		return "", false
	}
	template, err := url.Parse(self.dohUrlTemplate)
	if err != nil || !strings.EqualFold(u.Hostname(), template.Hostname()) {
		return "", false
	}
	templateSegments := strings.Split(strings.Trim(template.Path, "/"), "/")
	// the segments as pasted: an escaped slash or anything else escaped
	// stays inside its segment, where no id has it
	escapedPath := u.EscapedPath()
	if strings.Contains(escapedPath, "%") {
		return "", false
	}
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	idIndex := len(templateSegments) - 1
	if len(segments) < len(templateSegments) {
		return "", false
	}
	if len(templateSegments) < len(segments) {
		if !self.dohUrlNameSegment || len(templateSegments)+1 < len(segments) {
			return "", false
		}
	}
	for i := 0; i < idIndex; i += 1 {
		if segments[i] != templateSegments[i] {
			return "", false
		}
	}
	id := segments[idIndex]
	if id == "" {
		return "", false
	}
	return id, true
}

// The id in the provider's DoT hostname, any device name dropped (D6).
func (self *dnsPresetPersonalId) idFromDotHostname(input string) (string, bool) {
	if self.dotHostnamePattern == nil {
		return "", false
	}
	groups := self.dotHostnamePattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(input)))
	if len(groups) != 2 || groups[1] == "" {
		return "", false
	}
	return groups[1], true
}

// The error id of a personal id for a catalog preset, or empty when it is
// valid (D4). The input may be what the user typed or pasted: a url or
// hostname of another catalog provider is recognized, and the rest is
// normalized first.
func validateDnsPresetPersonalIdOf(catalog *dnsPresetCatalog, preset *dnsPreset, input string) (errorId string) {
	trimmed := strings.TrimSpace(input)
	if preset.personalId == nil {
		if trimmed != "" {
			return DnsPresetErrorPersonalIdNotAccepted
		}
		return ""
	}
	if trimmed == "" {
		return DnsPresetErrorPersonalIdRequired
	}
	if host := dnsPresetInputHost(trimmed); host != "" {
		if providerId := catalog.providerIdOfHost(host); providerId != "" && providerId != preset.providerId {
			return DnsPresetErrorPersonalIdOtherProvider
		}
	}
	id := normalizeDnsPresetPersonalId(preset, trimmed)
	if !preset.personalId.idValid(id) {
		return DnsPresetErrorPersonalIdInvalid
	}
	if _, ok := preset.personalId.dohUrl(id); !ok {
		return DnsPresetErrorPersonalIdInvalid
	}
	return ""
}

// The host of a pasted url or hostname, or empty for anything else (a bare
// id has neither a scheme nor a dot).
func dnsPresetInputHost(input string) string {
	if strings.Contains(input, "://") {
		if u, err := url.Parse(input); err == nil {
			return u.Hostname()
		}
		return ""
	}
	if strings.Contains(input, ".") && !strings.ContainsAny(input, " /\t") {
		return input
	}
	return ""
}

// Whether a normalized id has the provider's form (D4): ascii letters,
// digits and hyphens only, so it can never leave its path segment, at most
// one dns label long, matching one of the provider's patterns.
func (self *dnsPresetPersonalId) idValid(id string) bool {
	if id == "" || dnsPresetPersonalIdMaxLength < len(id) {
		return false
	}
	for _, c := range id {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	for _, forbidden := range self.forbiddenSubstrings {
		if strings.Contains(id, forbidden) {
			return false
		}
	}
	for _, pattern := range self.idPatterns {
		if pattern.MatchString(id) {
			return true
		}
	}
	return false
}

// The DoH url of a valid id, parsed again: it must have the template's
// scheme, host and path with the id in place as one whole segment, and
// nothing else (D4).
func (self *dnsPresetPersonalId) dohUrl(id string) (string, bool) {
	template, err := url.Parse(self.dohUrlTemplate)
	if err != nil || id == "" || id == "." || id == ".." {
		return "", false
	}
	built := strings.Replace(self.dohUrlTemplate, dnsPresetPersonalIdPlaceholder, id, 1)
	u, err := url.Parse(built)
	if err != nil {
		return "", false
	}
	wantPath := strings.Replace(template.Path, dnsPresetPersonalIdPlaceholder, id, 1)
	if strings.Count(wantPath, "/") != strings.Count(template.Path, "/") {
		// the id would span segments
		return "", false
	}
	if u.Scheme != template.Scheme || u.Host != template.Host || u.User != nil ||
		u.Path != wantPath || u.EscapedPath() != wantPath ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	return built, true
}

// Settings a caller hands a setter, as the device applies them. A caller that
// changed the lists or toggles of settings read back with URnetwork's default
// or a regional selection, as an editor that keeps the read-back struct as its
// draft does, chose custom settings: they apply as custom, the lists as given,
// so an editor that predates the presets keeps working. A catalog preset
// keeps its selection and the device derives its lists, ignoring the ones
// given (E1).
//
// Lists are not edited when they are a derivation of the selection, today's
// or an earlier release's: an app seeds a device from a copy it kept, whose
// lists are those of the release that wrote it. An earlier release's default
// lists and regional recommendations are the ones the default server list
// migration knows (migrateDefaultServerLists); a release that changes them
// adds the old ones there, as it must for custom records anyway. A bare
// selection, with no list and no toggle, is not an edit either.
func dnsResolverSettingsAsEdited(dnsResolverSettings *DnsResolverSettings) *DnsResolverSettings {
	if dnsResolverSettings == nil {
		return nil
	}
	presetId := dnsResolverSettings.PresetId
	if presetId != DnsPresetIdDefault && !strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix) {
		return dnsResolverSettings
	}
	if dnsResolverSettingsBare(dnsResolverSettings) {
		return dnsResolverSettings
	}
	derived := dnsResolverSettingsFromConnect(dnsResolverSettings.toConnect())
	if dnsResolverSettingsListsEqual(dnsResolverSettings, derived) {
		return dnsResolverSettings
	}
	// an earlier release's derivation: its lists migrate to today's
	for _, recordVersion := range []int{0, 1} {
		earlier := copyDnsResolverSettings(dnsResolverSettings)
		migrateDefaultServerLists(earlier, recordVersion)
		if dnsResolverSettingsListsEqual(earlier, derived) {
			return dnsResolverSettings
		}
	}
	edited := copyDnsResolverSettings(dnsResolverSettings)
	edited.PresetId = ""
	edited.PresetPersonalId = ""
	return edited
}

// Whether settings name no resolver toggle and no list: a selection alone.
func dnsResolverSettingsBare(dnsResolverSettings *DnsResolverSettings) bool {
	if dnsResolverSettings.EnableRemoteDoh || dnsResolverSettings.EnableLocalDoh ||
		dnsResolverSettings.EnableRemoteDns || dnsResolverSettings.EnableLocalDns {
		return false
	}
	for _, list := range []*StringList{
		dnsResolverSettings.RemoteDohUrlsIpv4,
		dnsResolverSettings.RemoteDohUrlsIpv6,
		dnsResolverSettings.LocalDohUrlsIpv4,
		dnsResolverSettings.LocalDohUrlsIpv6,
		dnsResolverSettings.RemoteDnsIpv4,
		dnsResolverSettings.RemoteDnsIpv6,
		dnsResolverSettings.LocalDnsIpv4,
		dnsResolverSettings.LocalDnsIpv6,
	} {
		if list != nil && 0 < list.Len() {
			return false
		}
	}
	return true
}

// Whether two settings resolve with the same toggles and lists, in order, an
// absent list the same as an empty one. Fast DNS on connect, the mask and the
// selection do not count.
func dnsResolverSettingsListsEqual(a *DnsResolverSettings, b *DnsResolverSettings) bool {
	if a.EnableRemoteDoh != b.EnableRemoteDoh || a.EnableLocalDoh != b.EnableLocalDoh ||
		a.EnableRemoteDns != b.EnableRemoteDns || a.EnableLocalDns != b.EnableLocalDns {
		return false
	}
	stringsOf := func(list *StringList) []string {
		if list == nil {
			return nil
		}
		return list.getAll()
	}
	for _, pair := range [][2]*StringList{
		{a.RemoteDohUrlsIpv4, b.RemoteDohUrlsIpv4},
		{a.RemoteDohUrlsIpv6, b.RemoteDohUrlsIpv6},
		{a.LocalDohUrlsIpv4, b.LocalDohUrlsIpv4},
		{a.LocalDohUrlsIpv6, b.LocalDohUrlsIpv6},
		{a.RemoteDnsIpv4, b.RemoteDnsIpv4},
		{a.RemoteDnsIpv6, b.RemoteDnsIpv6},
		{a.LocalDnsIpv4, b.LocalDnsIpv4},
		{a.LocalDnsIpv6, b.LocalDnsIpv6},
	} {
		if !slices.Equal(stringsOf(pair[0]), stringsOf(pair[1])) {
			return false
		}
	}
	return true
}

// The refusals of a stored or requested selection. Fixed texts: none carries
// the selection's ids, so a refusal can be logged (D5).
var (
	errDnsPresetUnknown           = errors.New("unknown dns preset")
	errDnsPresetPersonalIdInvalid = errors.New("invalid dns preset personal id")
)

// Refuses a selection no device can apply (E2): an id that is not custom,
// "default", a well formed regional pick or one the catalog knows, and a
// personal preset whose id does not validate. A preset the catalog dropped
// or hid is accepted and applies as "default", so the screens can keep the
// stored selection while its notice shows (A7). A personal id given with a
// selection that takes none is accepted and dropped.
func validateDnsResolverSettingsSelection(dnsResolverSettings *DnsResolverSettings) error {
	if dnsResolverSettings == nil {
		return nil
	}
	presetId := dnsResolverSettings.PresetId
	switch {
	case presetId == "" || presetId == DnsPresetIdDefault:
		return nil
	case strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix):
		if !dnsRegionalCountryCodeValid(strings.TrimPrefix(presetId, DnsPresetIdRegionalPrefix)) {
			return errDnsPresetUnknown
		}
		return nil
	}
	catalog := currentDnsPresetCatalog()
	preset := catalog.preset(presetId)
	if preset == nil {
		if _, reserved := catalog.reservedProviderNames[presetId]; reserved {
			return nil
		}
		return errDnsPresetUnknown
	}
	if preset.personalId != nil && validateDnsPresetPersonalIdOf(catalog, preset, dnsResolverSettings.PresetPersonalId) != "" {
		return errDnsPresetPersonalIdInvalid
	}
	return nil
}
