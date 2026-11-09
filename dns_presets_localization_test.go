package sdk

// The localization key ids the dns preset surface hands the screens must be
// keys of the localization store (localizations keys/<id>.yaml), which is the
// single source of every interface string: the apps map each id to their own
// string, and a missing key would show nothing.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every key id the sdk returns for the dns screens, checked against the
// localization store at localizations feature/adblock3rd 62e782f1, and the
// presets' names and the summary rows at localizations
// feature/dns-preset-names. A key the sdk starts returning joins this list
// once the store has it.
var testingDnsPresetLocalizationKeys = []string{
	// error ids
	"dns_preset_error_unknown",
	"dns_preset_error_personal_id_required",
	"dns_preset_error_personal_id_invalid",
	"dns_preset_error_personal_id_other_provider",
	"dns_preset_error_personal_id_not_accepted",
	// group headings
	"block_ads_and_trackers",
	"dns_group_block_malware",
	"dns_group_family",
	"dns_group_my_account",
	"dns_group_no_filtering",
	// picks
	"dns_provider_default",
	"dns_provider_default_copy",
	"dns_provider_custom",
	"dns_provider_custom_copy",
	"dns_provider_regional",
	"dns_recommendation_message",
	// preset names
	"dns_preset_name_ad_blocking",
	"dns_preset_name_malware_blocking",
	"dns_preset_name_family",
	"dns_preset_name_child",
	"dns_preset_name_child_ads",
	"dns_preset_name_no_filtering",
	"dns_preset_name_personal",
	// preset copy
	"dns_preset_copy_ads_phishing",
	"dns_preset_copy_ads_malware",
	"dns_preset_copy_ads_malware_phishing",
	"dns_preset_copy_malware",
	"dns_preset_copy_malware_phishing",
	"dns_preset_copy_ads_adult_safe_search",
	"dns_preset_copy_malware_adult",
	"dns_preset_copy_ads_malware_adult_drugs",
	"dns_preset_copy_child",
	"dns_preset_copy_child_ads",
	"dns_preset_copy_nothing",
	"dns_preset_copy_court_orders",
	"dns_preset_copy_personal",
	// personal ids
	"dns_personal_id_where_nextdns",
	"dns_personal_id_where_controld",
	"dns_personal_id_where_adguard",
	"dns_personal_id_format_nextdns",
	"dns_personal_id_format_controld",
	"dns_personal_id_format_adguard",
	"dns_personal_id_quota_nextdns",
	"dns_personal_id_quota_adguard",
	"dns_personal_id_dashboard_link",
	// summary rows; a preset's is dns_status_provider, with its name
	"dns_summary_default",
	"dns_summary_custom",
	"dns_summary_custom_unencrypted",
	"dns_summary_regional",
	// pills
	"dns_pill_recommended",
	"dns_pill_defaults",
	// status
	"dns_status_provider",
	"dns_status_paused",
	"dns_status_paused_detail",
	"dns_status_fallback",
	"dns_status_unreachable",
	"dns_status_unreachable_detail",
	// verification
	"dns_verify_not_filtering",
	"dns_verify_unconfirmed",
	"dns_verify_rejected_controld",
	"dns_verify_malformed_adguard",
}

// Every key the surface can return, over the whole catalog, every pick,
// selection kind, status state and verification.
func testingDnsPresetReturnedKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	add := func(key string) {
		if key != "" {
			keys[key] = true
		}
	}
	for _, errorId := range []string{
		DnsPresetErrorUnknown,
		DnsPresetErrorPersonalIdRequired,
		DnsPresetErrorPersonalIdInvalid,
		DnsPresetErrorPersonalIdOtherProvider,
		DnsPresetErrorPersonalIdNotAccepted,
	} {
		add(errorId)
	}
	settingsList := []*DnsResolverSettings{
		GetDefaultDnsResolverSettings(),
		GetRecommendedDnsResolverSettings("cn"),
		{EnableRemoteDoh: true, RemoteDohUrlsIpv4: testing_stringList("https://doh.custom.example/dns-query")},
		{EnableRemoteDns: true, RemoteDnsIpv4: testing_stringList("192.0.2.53")},
		{PresetId: "no-such-preset"},
	}
	for _, countryCode := range []string{"", "cn", "de"} {
		for _, pick := range GetDnsPicks(countryCode).getAll() {
			add(pick.GroupKey)
			add(pick.LabelKey)
			add(pick.CopyKey)
			if preset := pick.Preset; preset != nil {
				add(preset.NameKey)
				add(preset.GroupKey)
				add(preset.CopyKey)
				add(preset.PersonalIdWhereKey)
				add(preset.PersonalIdFormatKey)
				add(preset.QuotaNoteKey)
				personalId := ""
				if preset.PersonalIdRequired {
					personalId = preset.PersonalIdExample
				}
				if settings := DnsResolverSettingsWithPreset(nil, preset.PresetId, personalId); settings != nil {
					settingsList = append(settingsList, settings)
				} else {
					t.Fatalf("the pick %s cannot be selected", preset.PresetId)
				}
			}
		}
	}
	for _, provider := range GetDnsPresetProviders().getAll() {
		for _, preset := range provider.Presets.getAll() {
			add(preset.NameKey)
			add(preset.GroupKey)
			add(preset.CopyKey)
			add(preset.PersonalIdWhereKey)
			add(preset.PersonalIdFormatKey)
			add(preset.QuotaNoteKey)
		}
	}
	for _, settings := range settingsList {
		for _, countryCode := range []string{"", "cn", "de"} {
			summary := DescribeDnsResolverSettings(settings, countryCode)
			add(summary.NameKey)
			add(summary.DroppedNameKey)
			add(summary.GroupKey)
			add(summary.CopyKey)
			add(summary.SummaryKey)
			add(summary.LabelKey)
			add(summary.PillKey)
		}
		for _, state := range []string{DnsResolverStateUnknown, DnsResolverStateOk, DnsResolverStateFailing, DnsResolverStateFailedOver} {
			for _, verification := range []string{
				DnsResolverVerificationNone,
				DnsResolverVerificationConfirmed,
				DnsResolverVerificationNotFiltering,
				DnsResolverVerificationRejected,
				DnsResolverVerificationMalformed,
				DnsResolverVerificationUnconfirmed,
			} {
				status := DescribeDnsResolverStatus(settings, &DnsResolverStatus{
					State:        state,
					Verification: verification,
					PresetId:     settings.PresetId,
				})
				add(status.NameKey)
				add(status.StatusKey)
				add(status.StatusDetailKey)
				add(status.VerificationKey)
				add(status.VerificationLinkKey)
			}
		}
	}
	return keys
}

// Every key the surface returns is in the checked list, and every listed key
// is returned, so the list is exactly the surface's.
func TestDnsPresetLocalizationKeysAreListed(t *testing.T) {
	returned := testingDnsPresetReturnedKeys(t)
	for key := range returned {
		if !slices.Contains(testingDnsPresetLocalizationKeys, key) {
			t.Errorf("the sdk returns %q, which is not in the checked list of store keys", key)
		}
	}
	for _, key := range testingDnsPresetLocalizationKeys {
		if !returned[key] {
			t.Errorf("the checked list names %q, which the sdk never returns", key)
		}
	}
}

// With URNETWORK_LOCALIZATIONS_KEYS_DIR naming a store's keys/ directory,
// every listed key has its file there. The list was checked this way against
// localizations feature/adblock3rd 62e782f1.
func TestDnsPresetLocalizationKeysExistInTheStore(t *testing.T) {
	keysDir := os.Getenv("URNETWORK_LOCALIZATIONS_KEYS_DIR")
	if keysDir == "" {
		t.Skip("URNETWORK_LOCALIZATIONS_KEYS_DIR is not set; the checked list stands in for the store")
	}
	for _, key := range testingDnsPresetLocalizationKeys {
		if _, err := os.Stat(filepath.Join(keysDir, key+".yaml")); err != nil {
			t.Errorf("the store has no key %q", key)
		}
	}
}

// With URNETWORK_LOCALIZATIONS_KEYS_DIR naming a store's keys/ directory,
// each preset name key's English is the one the name tests render
// (testingDnsPresetNamesEnglish), and its one placeholder is the provider's
// short name, so the names the tests pin are the names the apps show.
func TestDnsPresetNameKeysMatchTheStore(t *testing.T) {
	keysDir := os.Getenv("URNETWORK_LOCALIZATIONS_KEYS_DIR")
	if keysDir == "" {
		t.Skip("URNETWORK_LOCALIZATIONS_KEYS_DIR is not set; testingDnsPresetNamesEnglish stands in for the store")
	}
	type storePlaceholder struct {
		Name string `yaml:"name"`
		Type string `yaml:"type"`
	}
	type storeKey struct {
		Placeholders  []storePlaceholder `yaml:"placeholders"`
		Localizations map[string]any     `yaml:"localizations"`
	}
	for nameKey, english := range testingDnsPresetNamesEnglish {
		data, err := os.ReadFile(filepath.Join(keysDir, nameKey+".yaml"))
		if err != nil {
			t.Errorf("the store has no key %q", nameKey)
			continue
		}
		var key storeKey
		if err := yaml.Unmarshal(data, &key); err != nil {
			t.Fatalf("%s: %v", nameKey, err)
		}
		if storeEnglish := key.Localizations["en"]; storeEnglish != english {
			t.Errorf("%s: the store's English is %q, the tests render %q", nameKey, storeEnglish, english)
		}
		if !slices.Equal(key.Placeholders, []storePlaceholder{{Name: "provider", Type: "string"}}) {
			t.Errorf("%s: placeholders %+v, want the provider's name alone", nameKey, key.Placeholders)
		}
	}
}
