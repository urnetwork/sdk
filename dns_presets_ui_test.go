package sdk

// What the dns screens call (connect ADBLOCK3RD A5, C6, C8, F1, F2), on the
// synthetic catalog of dns_presets_test.go.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Custom settings with every field distinct and filled, no selection.
func testingFilledCustomDnsResolverSettings(t *testing.T) *DnsResolverSettings {
	t.Helper()
	seed := 0
	settings := &DnsResolverSettings{}
	fillNonZero(t, reflect.ValueOf(settings), &seed)
	settings.PresetId = ""
	settings.PresetPersonalId = ""
	return settings
}

// Each helper keeps every field it does not set, and copies deeply (A5, E3).
func TestDnsPresetHelpersKeepEveryField(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	filled := testingFilledCustomDnsResolverSettings(t)
	filled.PresetId = "v-preset"
	filled.PresetPersonalId = "v-personal"

	copied := CopyDnsResolverSettings(filled)
	connect.AssertEqual(t, copied, filled)
	filled.RemoteDohUrlsIpv4.Add("https://doh.later.example/dns-query")
	if copied.RemoteDohUrlsIpv4.Len() == filled.RemoteDohUrlsIpv4.Len() {
		t.Fatal("the copy shares a list with its original")
	}
	if CopyDnsResolverSettings(nil) != nil {
		t.Fatal("a copy of nothing is something")
	}

	custom := testingFilledCustomDnsResolverSettings(t)
	toggled := DnsResolverSettingsWithFallback(custom, !custom.EnableFallback)
	want := CopyDnsResolverSettings(custom)
	want.EnableFallback = !custom.EnableFallback
	connect.AssertEqual(t, toggled, want)

	asCustom := DnsResolverSettingsAsCustom(custom)
	connect.AssertEqual(t, asCustom, custom)

	selected := DnsResolverSettingsWithPreset(custom, "alpha-ads", "")
	connect.AssertEqual(t, selected.PresetId, "alpha-ads")
	connect.AssertEqual(t, selected.EnableFallback, custom.EnableFallback)
	connect.AssertEqual(t, selected.DnsUpgradeMaskAddress, custom.DnsUpgradeMaskAddress)
	connect.AssertEqual(t, selected.RemoteDohUrlsIpv4.getAll(), []string{"https://dns.alpha.example/dns-query"})

	// back to custom: the lists the preset applied, the selection cleared
	back := DnsResolverSettingsAsCustom(DnsResolverSettingsWithPreset(custom, "gamma-personal", "abc123"))
	connect.AssertEqual(t, back.PresetId, "")
	connect.AssertEqual(t, back.PresetPersonalId, "")
	connect.AssertEqual(t, back.RemoteDohUrlsIpv4.getAll(), []string{"https://id.gamma.example/abc123"})
	connect.AssertEqual(t, back.EnableFallback, custom.EnableFallback)
}

// A pick the catalog cannot offer, or an id that does not validate, selects
// nothing.
func TestDnsPresetWithPresetRefuses(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	base := GetDefaultDnsResolverSettings()
	for _, c := range [][2]string{
		{"", ""},
		{"no-such-preset", ""},
		{"retired-preset", ""},
		{"regional-zz", ""},
		{"regional-", ""},
		{"gamma-personal", ""},
		{"gamma-personal", "zzzzzz"},
		{"alpha-ads", "abc123"},
		{DnsPresetIdDefault, "abc123"},
	} {
		if settings := DnsResolverSettingsWithPreset(base, c[0], c[1]); settings != nil {
			t.Errorf("%q %q selected %+v", c[0], c[1], settings)
		}
	}
	SetDnsPresetsHideAdBlocking(true)
	if DnsResolverSettingsWithPreset(base, "alpha-family", "") != nil {
		t.Error("a hidden preset was selected")
	}
	for _, c := range [][2]string{{DnsPresetIdDefault, ""}, {"regional-cn", ""}, {"beta-security", ""}, {"gamma-personal", "ABC123"}} {
		if DnsResolverSettingsWithPreset(base, c[0], c[1]) == nil {
			t.Errorf("%q could not be selected", c[0])
		}
	}
}

// The summary of each kind of selection: its keys, the preset's name and its
// provider's, one summary row for every preset ("DNS: <name>", owner,
// 2026-11-09), the blocked count only for a preset that filters, and the
// notice of a stored preset that cannot apply, naming it as it showed (A7,
// F1, I3).
func TestDescribeDnsResolverSettingsKinds(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	describe := func(settings *DnsResolverSettings) *DnsSelectionSummary {
		return DescribeDnsResolverSettings(settings, "")
	}
	connect.AssertEqual(t, describe(GetDefaultDnsResolverSettings()), &DnsSelectionSummary{
		Kind: DnsPickKindDefault, PresetId: DnsPresetIdDefault,
		SummaryKey: "dns_summary_default", LabelKey: "dns_provider_default",
	})
	connect.AssertEqual(t, describe(nil).Kind, DnsPickKindDefault)
	custom := describe(&DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: testing_stringList("https://192.0.2.80/dns-query")})
	connect.AssertEqual(t, custom, &DnsSelectionSummary{
		Kind: DnsPickKindCustom, SummaryKey: "dns_summary_custom", LabelKey: "dns_provider_custom",
		PillKey: "dns_pill_defaults",
	})
	plain := describe(&DnsResolverSettings{EnableRemoteDns: true, RemoteDnsIpv4: testing_stringList("192.0.2.53")})
	connect.AssertEqual(t, plain.SummaryKey, "dns_summary_custom_unencrypted")
	connect.AssertEqual(t, plain.Unencrypted, true)
	regional := DescribeDnsResolverSettings(GetRecommendedDnsResolverSettings("cn"), "cn")
	connect.AssertEqual(t, regional, &DnsSelectionSummary{
		Kind: DnsPickKindRegional, PresetId: "regional-cn", SummaryKey: "dns_summary_regional",
		LabelKey: "dns_provider_regional", CountryCode: "cn", Unencrypted: true,
	})
	connect.AssertEqual(t, describe(testingPresetSettings(t, "alpha-ads", "")), &DnsSelectionSummary{
		Kind: DnsPickKindPreset, PresetId: "alpha-ads", ProviderId: "alpha", ProviderName: "Alpha DNS",
		NameKey: "dns_preset_name_ad_blocking", NameProvider: "Alpha",
		Group: DnsPresetGroupAdblock, GroupKey: "block_ads_and_trackers", CopyKey: "dns_preset_copy_ads_phishing",
		SummaryKey: "dns_status_provider", Failover: true, ShowBlockedCount: true,
	})
	for _, c := range []struct {
		presetId     string
		personalId   string
		nameKey      string
		nameProvider string
		providerName string
		count        bool
	}{
		{presetId: "beta-security", nameKey: "dns_preset_name_malware_blocking", nameProvider: "Beta Resolver", providerName: "Beta Resolver", count: true},
		{presetId: "alpha-family", nameKey: "dns_preset_name_family", nameProvider: "Alpha", providerName: "Alpha DNS", count: true},
		{presetId: "beta-unfiltered", nameKey: "dns_preset_name_no_filtering", nameProvider: "Beta Resolver", providerName: "Beta Resolver", count: false},
		{presetId: "gamma-personal", personalId: "abc123", nameKey: "dns_preset_name_personal", nameProvider: "Gamma", providerName: "Gamma", count: true},
	} {
		summary := describe(testingPresetSettings(t, c.presetId, c.personalId))
		connect.AssertEqual(t,
			[]any{summary.SummaryKey, summary.NameKey, summary.NameProvider, summary.ProviderName},
			[]any{"dns_status_provider", c.nameKey, c.nameProvider, c.providerName})
		connect.AssertEqual(t, summary.ShowBlockedCount, c.count)
		connect.AssertEqual(t, summary.Personal, c.personalId != "")
	}
	for _, settings := range []*DnsResolverSettings{GetDefaultDnsResolverSettings(), GetRecommendedDnsResolverSettings("cn")} {
		if describe(settings).ShowBlockedCount {
			t.Errorf("%q shows the blocked count", settings.PresetId)
		}
	}

	// a dropped, hidden or unknown preset reads as default with the notice,
	// until the user picks again
	dropped := describe(&DnsResolverSettings{PresetId: "retired-preset"})
	connect.AssertEqual(t,
		[]any{dropped.Kind, dropped.PresetId, dropped.Dropped, dropped.DroppedPresetId, dropped.DroppedNameKey, dropped.DroppedNameProvider, dropped.DroppedProviderName},
		[]any{DnsPickKindDefault, DnsPresetIdDefault, true, "retired-preset", "dns_preset_name_ad_blocking", "Retired", "Retired DNS"})
	unknown := describe(&DnsResolverSettings{PresetId: "from-a-later-release"})
	connect.AssertEqual(t,
		[]any{unknown.Dropped, unknown.DroppedNameKey, unknown.DroppedNameProvider, unknown.DroppedProviderName},
		[]any{true, "", "", ""})
	SetDnsPresetsHideAdBlocking(true)
	hidden := describe(&DnsResolverSettings{PresetId: "alpha-ads"})
	connect.AssertEqual(t,
		[]any{hidden.Dropped, hidden.DroppedNameKey, hidden.DroppedNameProvider, hidden.DroppedProviderName},
		[]any{true, "dns_preset_name_ad_blocking", "Alpha", "Alpha DNS"})
	dismissed := describe(DnsResolverSettingsWithPreset(&DnsResolverSettings{PresetId: "alpha-ads"}, DnsPresetIdDefault, ""))
	connect.AssertEqual(t, dismissed.Dropped, false)
}

// The pill (F2): never while a preset is selected; the recommendation pill
// while the exit's country has a recommendation the settings do not apply;
// else the defaults pill while they differ from the defaults, Fast DNS on
// connect included, as the apps compared them before.
func TestDescribeDnsResolverSettingsPill(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	pill := func(settings *DnsResolverSettings, exitCountryCode string) []string {
		summary := DescribeDnsResolverSettings(settings, exitCountryCode)
		return []string{summary.PillKey, summary.PillCountryCode}
	}
	none := []string{"", ""}
	recommended := func(countryCode string) []string { return []string{"dns_pill_recommended", countryCode} }
	defaults := []string{"dns_pill_defaults", ""}

	for _, presetId := range []string{"alpha-ads", "beta-unfiltered", "beta-security"} {
		connect.AssertEqual(t, pill(testingPresetSettings(t, presetId, ""), "cn"), none)
		connect.AssertEqual(t, pill(DnsResolverSettingsWithFallback(testingPresetSettings(t, presetId, ""), true), "de"), none)
	}
	connect.AssertEqual(t, pill(GetDefaultDnsResolverSettings(), "CN"), recommended("cn"))
	connect.AssertEqual(t, pill(GetDefaultDnsResolverSettings(), "de"), none)
	connect.AssertEqual(t, pill(GetDefaultDnsResolverSettings(), ""), none)
	connect.AssertEqual(t, pill(DnsResolverSettingsWithFallback(GetDefaultDnsResolverSettings(), true), "de"), defaults)
	custom := &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: testing_stringList("https://192.0.2.80/dns-query")}
	connect.AssertEqual(t, pill(custom, "de"), defaults)
	connect.AssertEqual(t, pill(custom, "cn"), recommended("cn"))
	connect.AssertEqual(t, pill(DnsResolverSettingsAsCustom(GetDefaultDnsResolverSettings()), "de"), none)
	connect.AssertEqual(t, pill(DnsResolverSettingsAsCustom(GetRecommendedDnsResolverSettings("cn")), "cn"), none)
	connect.AssertEqual(t, pill(GetRecommendedDnsResolverSettings("cn"), "cn"), none)
	connect.AssertEqual(t, pill(GetRecommendedDnsResolverSettings("cn"), "ru"), recommended("ru"))
	connect.AssertEqual(t, pill(GetRecommendedDnsResolverSettings("cn"), "de"), defaults)
}

// The status line, its detail and actions, the verification line and the
// blocked count for each preset group and state (C6, C8, I3). The status line
// and the count name the preset, the verification line its provider.
func TestDescribeDnsResolverStatusLines(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	describe := func(presetId string, personalId string, state string, verification string) *DnsStatusSummary {
		settings := testingPresetSettings(t, presetId, personalId)
		return DescribeDnsResolverStatus(settings, &DnsResolverStatus{
			State: state, Verification: verification, PresetId: presetId, BlockedCount: 7,
		})
	}
	connect.AssertEqual(t, describe("alpha-ads", "", DnsResolverStateOk, DnsResolverVerificationConfirmed), &DnsStatusSummary{
		StatusKey: "dns_status_provider", NameKey: "dns_preset_name_ad_blocking", NameProvider: "Alpha", ProviderName: "Alpha DNS",
		ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("alpha-ads", "", DnsResolverStateFailedOver, DnsResolverVerificationNone), &DnsStatusSummary{
		StatusKey: "dns_status_paused", NameKey: "dns_preset_name_ad_blocking", NameProvider: "Alpha", ProviderName: "Alpha DNS",
		StatusDetailKey: "dns_status_paused_detail",
		ShowUseDefault:  true, ShowChangeLocation: true, ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("alpha-ads", "", DnsResolverStateFailing, DnsResolverVerificationNone), &DnsStatusSummary{
		StatusKey: "dns_status_unreachable", NameKey: "dns_preset_name_ad_blocking", NameProvider: "Alpha", ProviderName: "Alpha DNS",
		ShowUseDefault: true, ShowChangeLocation: true, ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("beta-unfiltered", "", DnsResolverStateFailedOver, DnsResolverVerificationNone), &DnsStatusSummary{
		StatusKey: "dns_status_fallback", NameKey: "dns_preset_name_no_filtering", NameProvider: "Beta Resolver", ProviderName: "Beta Resolver",
		ShowUseDefault: true, ShowChangeLocation: true,
	})
	connect.AssertEqual(t, describe("alpha-family", "", DnsResolverStateFailing, DnsResolverVerificationNone), &DnsStatusSummary{
		StatusKey: "dns_status_unreachable", NameKey: "dns_preset_name_family", NameProvider: "Alpha", ProviderName: "Alpha DNS",
		StatusDetailKey: "dns_status_unreachable_detail",
		ShowUseDefault:  true, ShowChangeLocation: true, ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("beta-security", "", DnsResolverStateOk, DnsResolverVerificationNotFiltering), &DnsStatusSummary{
		StatusKey: "dns_status_provider", NameKey: "dns_preset_name_malware_blocking", NameProvider: "Beta Resolver", ProviderName: "Beta Resolver",
		VerificationKey: "dns_verify_not_filtering", ShowChangeLocation: true, ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("gamma-personal", "abc123", DnsResolverStateUnknown, DnsResolverVerificationUnconfirmed), &DnsStatusSummary{
		StatusKey: "dns_status_provider", NameKey: "dns_preset_name_personal", NameProvider: "Gamma", ProviderName: "Gamma",
		VerificationKey: "dns_verify_unconfirmed",
		VerificationUrl: "https://gamma.example/dashboard", VerificationLinkKey: "dns_personal_id_dashboard_link",
		ShowBlockedCount: true, BlockedCount: 7,
	})
	connect.AssertEqual(t, describe("gamma-personal", "abc123", DnsResolverStateFailing, DnsResolverVerificationRejected).VerificationKey, "dns_verify_rejected_controld")
	connect.AssertEqual(t, describe("alpha-personal", "1a2b3c4d", DnsResolverStateFailing, DnsResolverVerificationMalformed).VerificationKey, "dns_verify_malformed_adguard")

	// no preset, no status line; a status of another selection reads unknown
	connect.AssertEqual(t, DescribeDnsResolverStatus(GetDefaultDnsResolverSettings(), &DnsResolverStatus{State: DnsResolverStateFailing}), &DnsStatusSummary{})
	connect.AssertEqual(t, DescribeDnsResolverStatus(nil, nil), &DnsStatusSummary{})
	stale := DescribeDnsResolverStatus(testingPresetSettings(t, "alpha-family", ""), &DnsResolverStatus{
		State: DnsResolverStateFailing, Verification: DnsResolverVerificationNotFiltering, PresetId: "alpha-ads", BlockedCount: 3,
	})
	connect.AssertEqual(t, stale, &DnsStatusSummary{
		StatusKey: "dns_status_provider", NameKey: "dns_preset_name_family", NameProvider: "Alpha", ProviderName: "Alpha DNS",
		ShowBlockedCount: true,
	})
}

// The picks in the screen's order on the synthetic catalog, with each pick's
// keys, hidden presets left out, and Regional only for a country with a
// recommendation, never selected for the user (O6, O9, O15).
func TestGetDnsPicksOrderAndRegional(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	picks := func(exitCountryCode string) []string {
		ids := []string{}
		for _, pick := range GetDnsPicks(exitCountryCode).getAll() {
			ids = append(ids, pick.Kind+":"+pick.PresetId+":"+pick.GroupKey+":"+pick.LabelKey+":"+pick.CopyKey)
		}
		return ids
	}
	shown := []string{
		"default:default::dns_provider_default:dns_provider_default_copy",
		"preset:alpha-ads:block_ads_and_trackers::dns_preset_copy_ads_phishing",
		"preset:beta-plain:dns_group_block_malware::dns_preset_copy_malware",
		"preset:beta-security:dns_group_block_malware::dns_preset_copy_malware_phishing",
		"preset:alpha-family:dns_group_family::dns_preset_copy_ads_adult_safe_search",
		"preset:alpha-personal:dns_group_my_account::dns_preset_copy_personal",
		"preset:gamma-personal:dns_group_my_account::dns_preset_copy_personal",
		"preset:beta-unfiltered:dns_group_no_filtering::dns_preset_copy_nothing",
		"custom:::dns_provider_custom:dns_provider_custom_copy",
	}
	connect.AssertEqual(t, picks(""), shown)
	connect.AssertEqual(t, picks("zz"), shown)
	connect.AssertEqual(t, picks(" CN "), slices.Concat(shown, []string{"regional:regional-cn::dns_provider_regional:dns_recommendation_message"}))
	regional := GetDnsPicks("cn").getAll()
	if last := regional[len(regional)-1]; last.CountryCode != "cn" || !last.Unencrypted {
		t.Fatalf("the regional pick = %+v", last)
	}
	SetDnsPresetsHideAdBlocking(true)
	for _, pick := range picks("") {
		if pick == shown[1] || pick == shown[4] {
			t.Errorf("a hidden preset is a pick: %s", pick)
		}
	}
}

// The providers by name, each with its presets in display order, a preset
// carrying its provider's facts and its personal id fields.
func TestGetDnsPresetProvidersGroupsPresets(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	providers := GetDnsPresetProviders().getAll()
	names := []string{}
	for _, provider := range providers {
		ids := []string{}
		for _, preset := range provider.Presets.getAll() {
			ids = append(ids, preset.PresetId)
		}
		names = append(names, provider.Name+":"+slices.Concat(ids, nil)[0])
	}
	connect.AssertEqual(t, names, []string{"Alpha DNS:alpha-ads", "Beta Resolver:beta-plain", "Gamma:gamma-personal"})
	connect.AssertEqual(t, GetDnsPreset("gamma-personal"), &DnsPreset{
		PresetId: "gamma-personal", ProviderId: "gamma", NameKey: "dns_preset_name_personal", NameProvider: "Gamma",
		ProviderName: "Gamma", Jurisdiction: "Testland",
		PrivacyUrl: "https://gamma.example/privacy", HelpUrl: "https://gamma.example/help",
		Group: DnsPresetGroupPersonal, GroupKey: "dns_group_my_account", CopyKey: "dns_preset_copy_personal",
		Filtering: true, PersonalIdRequired: true, PersonalIdExample: "abc123",
		PersonalIdHelpUrl: "https://gamma.example/dashboard", PersonalIdWhereKey: "dns_personal_id_where_nextdns",
		PersonalIdFormatKey: "dns_personal_id_format_nextdns", QuotaNoteKey: "dns_personal_id_quota_nextdns",
	})
	if GetDnsPreset("retired-preset") != nil || GetDnsPreset(DnsPresetIdDefault) != nil {
		t.Fatal("a reserved id reads as a preset")
	}
	SetDnsPresetsHideAdBlocking(true)
	for _, provider := range GetDnsPresetProviders().getAll() {
		for _, preset := range provider.Presets.getAll() {
			if preset.BlocksAds {
				t.Errorf("a hidden preset is listed: %s", preset.PresetId)
			}
		}
	}
}

// The saved id a personal preset's field starts on (decision 21): the id in
// force, normalized as the device applies it, for that preset's field alone;
// empty for any other pick, a preset that takes no id, and a stored selection
// that cannot apply (A7).
func TestGetDnsPresetPersonalIdForEdit(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	// saved as a screen saves a pasted dashboard url or DoT hostname
	gamma := testingPresetSettings(t, "gamma-personal", "https://id.gamma.example/C0FFEE/Kitchen")
	alpha := testingPresetSettings(t, "alpha-personal", "C0FFEE42.d.alpha.example")
	for _, c := range []struct {
		settings   *DnsResolverSettings
		presetId   string
		personalId string
	}{
		{settings: gamma, presetId: "gamma-personal", personalId: "c0ffee"},
		{settings: alpha, presetId: "alpha-personal", personalId: "c0ffee42"},
		// another personal preset's field starts empty
		{settings: gamma, presetId: "alpha-personal"},
		{settings: alpha, presetId: "gamma-personal"},
		// picks that take no id, and no pick
		{settings: gamma, presetId: "alpha-ads"},
		{settings: gamma, presetId: DnsPresetIdDefault},
		{settings: gamma, presetId: ""},
		{settings: nil, presetId: "gamma-personal"},
		// an id stored as typed starts as the device applies it
		{settings: &DnsResolverSettings{PresetId: "gamma-personal", PresetPersonalId: " C0FFEE "}, presetId: "gamma-personal", personalId: "c0ffee"},
		// a stray id beside a preset that takes none
		{settings: &DnsResolverSettings{PresetId: "alpha-ads", PresetPersonalId: "c0ffee"}, presetId: "alpha-ads"},
		// a stored id that no longer validates, and a preset this release
		// never knew, apply as default (A7)
		{settings: &DnsResolverSettings{PresetId: "gamma-personal", PresetPersonalId: "c0ffee/../x"}, presetId: "gamma-personal"},
		{settings: &DnsResolverSettings{PresetId: "from-a-later-release", PresetPersonalId: "c0ffee"}, presetId: "from-a-later-release"},
		// default, custom and regional settings
		{settings: GetDefaultDnsResolverSettings(), presetId: DnsPresetIdDefault},
		{settings: testingFilledCustomDnsResolverSettings(t), presetId: ""},
		{settings: GetRecommendedDnsResolverSettings("cn"), presetId: "regional-cn"},
	} {
		if personalId := GetDnsPresetPersonalIdForEdit(c.settings, c.presetId); personalId != c.personalId {
			presetId := ""
			if c.settings != nil {
				presetId = c.settings.PresetId
			}
			t.Errorf("the %q field on settings of %q starts on %q, want %q", c.presetId, presetId, personalId, c.personalId)
		}
	}
	// reading it leaves the settings as they were
	connect.AssertEqual(t, gamma, testingPresetSettings(t, "gamma-personal", "c0ffee"))
}

// A saved personal id shows in its own field alone (decision 21, D5): the
// summary row, the status line, the verification line and its link, and the
// picks the screen lists carry no part of it, for every state and
// verification, while the field's getter gives it back.
func TestDnsPersonalIdShowsInItsFieldAlone(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	states := []string{
		DnsResolverStateUnknown, DnsResolverStateOk, DnsResolverStateFailing, DnsResolverStateFailedOver,
	}
	verifications := []string{
		DnsResolverVerificationNone, DnsResolverVerificationConfirmed, DnsResolverVerificationNotFiltering,
		DnsResolverVerificationRejected, DnsResolverVerificationMalformed, DnsResolverVerificationUnconfirmed,
	}
	for _, c := range []struct {
		presetId   string
		pasted     string
		personalId string
	}{
		{presetId: "gamma-personal", pasted: "https://id.gamma.example/C0FFEE/Kitchen", personalId: "c0ffee"},
		{presetId: "alpha-personal", pasted: "C0FFEE42.d.alpha.example", personalId: "c0ffee42"},
	} {
		settings := testingPresetSettings(t, c.presetId, c.pasted)
		connect.AssertEqual(t, GetDnsPresetPersonalIdForEdit(settings, c.presetId), c.personalId)
		// the settings select the personal preset, and the summary says so
		summary := DescribeDnsResolverSettings(settings, "")
		connect.AssertEqual(t,
			[]any{summary.PresetId, summary.Personal, summary.SummaryKey, summary.NameKey},
			[]any{c.presetId, true, "dns_status_provider", "dns_preset_name_personal"})

		shown := []any{GetDnsPicks(""), GetDnsPicks("cn"), GetDnsPreset(c.presetId), GetDnsPresetProviders()}
		for _, exitCountryCode := range []string{"", "cn"} {
			shown = append(shown, DescribeDnsResolverSettings(settings, exitCountryCode))
		}
		linked := false
		for _, state := range states {
			for _, verification := range verifications {
				// the status of the selection, and of another one
				for _, statusPresetId := range []string{c.presetId, "alpha-ads"} {
					status := DescribeDnsResolverStatus(settings, &DnsResolverStatus{
						State: state, Verification: verification, PresetId: statusPresetId, BlockedCount: 7,
					})
					linked = linked || status.VerificationUrl != ""
					shown = append(shown, status)
				}
			}
		}
		if !linked {
			t.Fatalf("%s: no status links the dashboard", c.presetId)
		}
		for _, value := range shown {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(encoded)), c.personalId) {
				t.Errorf("%s: a screen's description carries the id: %s", c.presetId, encoded)
			}
		}
	}
}

// The catalog and the screens' surface handle the personal id where no
// device logger reaches, and log nothing: no logger import, no print to
// stdout or stderr (which mobile builds route to the system log), and no log
// field. A line they need goes through a device's logger, which
// TestDeviceLocalDnsLogsNoPersonalIdOrCount reads (D5, decision 21).
func TestDnsPresetFilesLogNothing(t *testing.T) {
	fileSet := token.NewFileSet()
	for _, path := range []string{"dns_presets.go", "dns_presets_ui.go"} {
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			switch importPath {
			case "log", "log/slog", "github.com/urnetwork/glog":
				t.Errorf("%s imports %s", path, importPath)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if name, ok := node.Fun.(*ast.Ident); ok && (name.Name == "print" || name.Name == "println") {
					t.Errorf("%s: %s", fileSet.Position(node.Pos()), name.Name)
				}
			case *ast.SelectorExpr:
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "fmt" &&
					(strings.HasPrefix(node.Sel.Name, "Print") || strings.HasPrefix(node.Sel.Name, "Fprint")) {
					t.Errorf("%s: fmt.%s", fileSet.Position(node.Pos()), node.Sel.Name)
				}
				if node.Sel.Name == "log" {
					t.Errorf("%s: a log field", fileSet.Position(node.Pos()))
				}
			}
			return true
		})
	}
}
