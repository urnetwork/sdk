//go:build !ios_extension

// What the dns screens call for the third-party presets (dns_presets.go,
// connect ADBLOCK3RD A5, F1, F2): the picks in the screen's order, the
// personal id check, the helpers every save starts from, and the descriptions
// of a selection and of the resolver status. The rules live here so the apps
// only render. Copy is localization key ids (G1), with the provider's name as
// the argument where a key takes one; provider names, operators and
// jurisdictions are shown as they are (G2). Excluded from the ios packet
// tunnel extension, which only applies the stored settings.
package sdk

import (
	"slices"
	"strings"
)

// The kinds of pick and selection, in the screen's order (O9): Default, the
// preset groups, Custom, then Regional.
const (
	DnsPickKindDefault  = "default"
	DnsPickKindPreset   = "preset"
	DnsPickKindCustom   = "custom"
	DnsPickKindRegional = "regional"
)

// The localization key ids the sdk hands the screens, as the localization
// store has them (localizations keys/, ADBLOCK3RD G1). A key whose text names
// the provider takes it as {provider}, never inside the key. The ad-blocking
// group heading, the regional explanation and the pills are keys the apps
// already show.
const (
	dnsGroupKeyBlockAds            = "block_ads_and_trackers"
	dnsGroupKeyBlockMalware        = "dns_group_block_malware"
	dnsGroupKeyFamily              = "dns_group_family"
	dnsGroupKeyMyAccount           = "dns_group_my_account"
	dnsGroupKeyNoFiltering         = "dns_group_no_filtering"
	dnsProviderKeyDefault          = "dns_provider_default"
	dnsProviderKeyDefaultCopy      = "dns_provider_default_copy"
	dnsProviderKeyCustom           = "dns_provider_custom"
	dnsProviderKeyCustomCopy       = "dns_provider_custom_copy"
	dnsProviderKeyRegional         = "dns_provider_regional"
	dnsProviderKeyRegionalCopy     = "dns_recommendation_message"
	dnsSummaryKeyDefault           = "dns_summary_default"
	dnsSummaryKeyAdBlocking        = "dns_summary_ad_blocking"
	dnsSummaryKeyMalwareBlocking   = "dns_summary_malware_blocking"
	dnsSummaryKeyFamily            = "dns_summary_family"
	dnsSummaryKeyNoFiltering       = "dns_summary_no_filtering"
	dnsSummaryKeyPersonal          = "dns_summary_personal"
	dnsSummaryKeyCustom            = "dns_summary_custom"
	dnsSummaryKeyCustomUnencrypted = "dns_summary_custom_unencrypted"
	dnsSummaryKeyRegional          = "dns_summary_regional"
	dnsVerifyKeyUnconfirmed        = "dns_verify_unconfirmed"
	dnsVerifyKeyRejected           = "dns_verify_rejected_controld"
	dnsVerifyKeyMalformed          = "dns_verify_malformed_adguard"
	dnsVerifyKeyNotFiltering       = "dns_verify_not_filtering"
	dnsVerifyKeyDashboardLink      = "dns_personal_id_dashboard_link"
	dnsStatusKeyProvider           = "dns_status_provider"
	dnsStatusKeyUnreachable        = "dns_status_unreachable"
	dnsStatusKeyUnreachableDetail  = "dns_status_unreachable_detail"
	dnsStatusKeyPaused             = "dns_status_paused"
	dnsStatusKeyPausedDetail       = "dns_status_paused_detail"
	dnsStatusKeyFallback           = "dns_status_fallback"
	dnsPillKeyRecommended          = "dns_pill_recommended"
	dnsPillKeyDefaults             = "dns_pill_defaults"
)

// A provider of presets. Its name, operator and jurisdiction are shown as
// they are, never translated, and never with a logo (O10).
type DnsPresetProvider struct {
	ProviderId   string
	Name         string
	Operator     string
	Jurisdiction string
	HelpUrl      string
	PrivacyUrl   string
	// empty when the provider publishes no terms for its public resolver
	TermsUrl string
	// the provider's presets this catalog offers, in display order
	Presets *DnsPresetList
}

// One preset a user can pick.
type DnsPreset struct {
	PresetId   string
	ProviderId string
	// the provider's name, shown as it is: the argument of the keys that take
	// a provider
	ProviderName string
	Jurisdiction string
	PrivacyUrl   string
	HelpUrl      string
	// DnsPresetGroup*
	Group string
	// the group's heading key: block_ads_and_trackers,
	// dns_group_block_malware, dns_group_family, dns_group_my_account or
	// dns_group_no_filtering
	GroupKey string
	// the key of what it blocks, one of the dns_preset_copy_* keys;
	// dns_preset_copy_personal takes {provider}
	CopyKey   string
	BlocksAds bool
	// whether a failing preset falls back to URnetwork's dns through the
	// tunnel (true), or fails closed (false) (decision 3)
	Failover bool
	// whether it filters, so the screen shows the blocked-lookups count while
	// it is selected: every group but No filtering (I3)
	Filtering bool
	// a personal preset: the user's configuration id is required
	PersonalIdRequired bool
	// the provider's own example of an id, the field's placeholder
	PersonalIdExample string
	// the provider's dashboard page that shows the id, also linked when the
	// profile cannot be confirmed
	PersonalIdHelpUrl string
	// the key of where the provider shows the id
	// (dns_personal_id_where_<provider>), with {provider}
	PersonalIdWhereKey string
	// the key of the provider's id format message
	// (dns_personal_id_format_<provider>), with {provider} and {example}
	// (PersonalIdExample), shown with DnsPresetErrorPersonalIdInvalid
	PersonalIdFormatKey string
	// the key of the free-tier note under the field
	// (dns_personal_id_quota_<provider>), with {provider}, or empty
	QuotaNoteKey string
}

// One row of the dns screen's picker.
type DnsPick struct {
	// DnsPickKind*
	Kind string
	// what selecting the pick sets as DnsResolverSettings.PresetId:
	// DnsPresetIdDefault, a catalog preset id, "regional-<cc>", or empty for
	// Custom, which opens the editor (DnsResolverSettingsAsCustom)
	PresetId string
	// the group of a preset pick and its heading key, empty for the others
	Group    string
	GroupKey string
	// the label key of a default, custom or regional pick: dns_provider_default,
	// dns_provider_custom or dns_provider_regional (with {country}). Empty for
	// a preset pick, which shows its provider's name.
	LabelKey string
	// the key of the pick's line under its label: a preset's CopyKey,
	// dns_provider_default_copy, dns_provider_custom_copy, or for a regional
	// pick dns_recommendation_message (with {country})
	CopyKey string
	// the country of a regional pick
	CountryCode string
	// a regional pick resolves unencrypted, and says so
	Unencrypted bool
	// the preset of a preset pick, else nil
	Preset *DnsPreset
}

// What the summary row and the screen show about the settings in force, given
// the exit's country (A5, F1, F2).
type DnsSelectionSummary struct {
	// DnsPickKind*: what the settings select. A stored preset that cannot
	// apply reads as default, with Dropped set.
	Kind string
	// the pick to mark as selected: DnsPresetIdDefault, the preset id,
	// "regional-<cc>", or empty for Custom
	PresetId string
	// the selected preset's provider, group and copy keys, empty for the
	// other kinds
	ProviderId   string
	ProviderName string
	Group        string
	GroupKey     string
	CopyKey      string
	// the summary row's key: dns_summary_default, dns_summary_custom,
	// dns_summary_custom_unencrypted, dns_summary_regional, or for a preset
	// by its group dns_summary_ad_blocking, dns_summary_malware_blocking,
	// dns_summary_family, dns_summary_no_filtering or dns_summary_personal,
	// each with {provider}
	SummaryKey string
	// the label key of a default, custom or regional selection, and the
	// country of a regional one
	LabelKey    string
	CountryCode string
	// a personal preset: the summary shows "your profile", never the id (D5)
	Personal bool
	// the selection resolves with plain dns alone: a regional pick, or custom
	// settings with no DoH
	Unencrypted bool
	// whether the selected preset falls back to URnetwork's dns when it fails
	Failover bool
	// whether the screen shows the blocked-lookups count (I3)
	ShowBlockedCount bool
	// the stored selection cannot apply and URnetwork's dns is in force (A7):
	// the screen shows the dropped-preset notice naming DroppedProviderName,
	// with "Choose another preset" and "Enter it yourself under Custom".
	// DroppedProviderName is empty for an id this release never knew.
	// Dismissing the notice saves DnsPresetIdDefault.
	Dropped             bool
	DroppedPresetId     string
	DroppedProviderName string
	// the pill over the summary, or empty: dns_pill_recommended (with
	// PillCountryCode) while the exit's country has a recommendation the
	// settings do not apply, else dns_pill_defaults while they differ from the
	// defaults. Never while a preset is selected (F2).
	PillKey         string
	PillCountryCode string
}

// What the status line, the verification line and the blocked count show for
// the settings in force and the device's dns resolver status (C6, C8, I3).
type DnsStatusSummary struct {
	// the status line's key, with ProviderName as {provider}, or empty when
	// there is no status line (no preset selected): dns_status_provider while
	// the preset answers; dns_status_paused for an ad-blocking preset and
	// dns_status_fallback for a no-filtering one failed over to URnetwork's
	// dns; dns_status_unreachable for a failing preset
	StatusKey    string
	ProviderName string
	// the line under the status line, or empty: dns_status_paused_detail
	// under dns_status_paused, and dns_status_unreachable_detail under the
	// dns_status_unreachable of a preset that fails closed
	StatusDetailKey string
	// the verification line's key, with ProviderName as {provider}, or empty:
	// dns_verify_not_filtering, dns_verify_unconfirmed,
	// dns_verify_rejected_controld or dns_verify_malformed_adguard
	VerificationKey string
	// the page an unconfirmed profile links to, the provider's dashboard, and
	// the link's key (dns_personal_id_dashboard_link, with {provider}); both
	// empty for every other verification
	VerificationUrl     string
	VerificationLinkKey string
	// the actions under a failing line: "Use URnetwork DNS"
	// (dns_status_use_default, which saves DnsPresetIdDefault) and "Change
	// location" (dns_status_change_location)
	ShowUseDefault     bool
	ShowChangeLocation bool
	// whether the blocked-lookups count shows (dns_blocked_count with
	// BlockedCount and ProviderName, and dns_blocked_count_floor_note)
	ShowBlockedCount bool
	BlockedCount     int64
}

type DnsPresetProviderList struct {
	exportedList[*DnsPresetProvider]
}

func NewDnsPresetProviderList() *DnsPresetProviderList {
	return &DnsPresetProviderList{
		exportedList: *newExportedList[*DnsPresetProvider](),
	}
}

type DnsPresetList struct {
	exportedList[*DnsPreset]
}

func NewDnsPresetList() *DnsPresetList {
	return &DnsPresetList{
		exportedList: *newExportedList[*DnsPreset](),
	}
}

type DnsPickList struct {
	exportedList[*DnsPick]
}

func NewDnsPickList() *DnsPickList {
	return &DnsPickList{
		exportedList: *newExportedList[*DnsPick](),
	}
}

// The heading key of each group, in the screen's order (O9).
var dnsPresetGroupKeys = []struct {
	group string
	key   string
}{
	{group: DnsPresetGroupAdblock, key: dnsGroupKeyBlockAds},
	{group: DnsPresetGroupSecurity, key: dnsGroupKeyBlockMalware},
	{group: DnsPresetGroupFamily, key: dnsGroupKeyFamily},
	{group: DnsPresetGroupPersonal, key: dnsGroupKeyMyAccount},
	{group: DnsPresetGroupUnfiltered, key: dnsGroupKeyNoFiltering},
}

// The group's place in the screen's order and its heading key.
func dnsPresetGroupRank(group string) (int, string) {
	for i, groupKey := range dnsPresetGroupKeys {
		if groupKey.group == group {
			return i, groupKey.key
		}
	}
	return len(dnsPresetGroupKeys), ""
}

// The summary row's key of a preset, by its group.
func dnsPresetSummaryKey(group string) string {
	switch group {
	case DnsPresetGroupAdblock:
		return dnsSummaryKeyAdBlocking
	case DnsPresetGroupSecurity:
		return dnsSummaryKeyMalwareBlocking
	case DnsPresetGroupFamily:
		return dnsSummaryKeyFamily
	case DnsPresetGroupPersonal:
		return dnsSummaryKeyPersonal
	default:
		return dnsSummaryKeyNoFiltering
	}
}

// Whether a preset filters, so the blocked count shows (I3).
func dnsPresetFiltering(preset *dnsPreset) bool {
	return preset.group != DnsPresetGroupUnfiltered
}

// The catalog's presets this process offers, in display order (O9): by group
// in the screen's order, then by provider name, then by id. Hidden presets
// are left out.
func dnsPresetsInDisplayOrder(catalog *dnsPresetCatalog) []*dnsPreset {
	presets := []*dnsPreset{}
	for _, preset := range catalog.presets {
		if !dnsPresetHidden(preset) {
			presets = append(presets, preset)
		}
	}
	providerName := func(preset *dnsPreset) string {
		if provider := catalog.provider(preset.providerId); provider != nil {
			return strings.ToLower(provider.name)
		}
		return ""
	}
	slices.SortStableFunc(presets, func(a *dnsPreset, b *dnsPreset) int {
		aRank, _ := dnsPresetGroupRank(a.group)
		bRank, _ := dnsPresetGroupRank(b.group)
		if aRank != bRank {
			return aRank - bRank
		}
		if c := strings.Compare(providerName(a), providerName(b)); c != 0 {
			return c
		}
		return strings.Compare(a.presetId, b.presetId)
	})
	return presets
}

// The app form of a catalog preset.
func newDnsPreset(catalog *dnsPresetCatalog, preset *dnsPreset) *DnsPreset {
	_, groupKey := dnsPresetGroupRank(preset.group)
	exported := &DnsPreset{
		PresetId:   preset.presetId,
		ProviderId: preset.providerId,
		Group:      preset.group,
		GroupKey:   groupKey,
		CopyKey:    preset.copyKey,
		BlocksAds:  preset.blocksAds,
		Failover:   preset.failover,
		Filtering:  dnsPresetFiltering(preset),
	}
	if provider := catalog.provider(preset.providerId); provider != nil {
		exported.ProviderName = provider.name
		exported.Jurisdiction = provider.jurisdiction
		exported.PrivacyUrl = provider.privacyUrl
		exported.HelpUrl = provider.helpUrl
	}
	if personal := preset.personalId; personal != nil {
		exported.PersonalIdRequired = true
		exported.PersonalIdExample = personal.example
		exported.PersonalIdHelpUrl = personal.idHelpUrl
		exported.PersonalIdWhereKey = personal.whereKey
		exported.PersonalIdFormatKey = personal.formatKey
		exported.QuotaNoteKey = personal.quotaNoteKey
	}
	return exported
}

// The catalog preset a screen can offer, or nil for an unknown or hidden id.
func offeredDnsPreset(catalog *dnsPresetCatalog, presetId string) *dnsPreset {
	preset := catalog.preset(presetId)
	if preset == nil || dnsPresetHidden(preset) {
		return nil
	}
	return preset
}

// The providers by name, each with the presets this catalog offers in display
// order. A provider whose presets are all hidden is left out.
func GetDnsPresetProviders() *DnsPresetProviderList {
	catalog := currentDnsPresetCatalog()
	providers := slices.Clone(catalog.providers)
	slices.SortStableFunc(providers, func(a *dnsPresetProvider, b *dnsPresetProvider) int {
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	presets := dnsPresetsInDisplayOrder(catalog)
	exportedProviders := NewDnsPresetProviderList()
	for _, provider := range providers {
		providerPresets := NewDnsPresetList()
		for _, preset := range presets {
			if preset.providerId == provider.providerId {
				providerPresets.Add(newDnsPreset(catalog, preset))
			}
		}
		if providerPresets.Len() == 0 {
			continue
		}
		exportedProviders.Add(&DnsPresetProvider{
			ProviderId:   provider.providerId,
			Name:         provider.name,
			Operator:     provider.operator,
			Jurisdiction: provider.jurisdiction,
			HelpUrl:      provider.helpUrl,
			PrivacyUrl:   provider.privacyUrl,
			TermsUrl:     provider.termsUrl,
			Presets:      providerPresets,
		})
	}
	return exportedProviders
}

// The preset of an id, or nil for an unknown or hidden one.
func GetDnsPreset(presetId string) *DnsPreset {
	catalog := currentDnsPresetCatalog()
	preset := offeredDnsPreset(catalog, presetId)
	if preset == nil {
		return nil
	}
	return newDnsPreset(catalog, preset)
}

// The picker's rows in the screen's order (O9): URnetwork (default), each
// group's presets with providers alphabetical, Custom, then the regional pick
// of the exit's country when it has a recommendation, which is never selected
// automatically (O6). No preset is preselected or marked as recommended (H1).
func GetDnsPicks(exitCountryCode string) *DnsPickList {
	catalog := currentDnsPresetCatalog()
	picks := NewDnsPickList()
	picks.Add(&DnsPick{
		Kind:     DnsPickKindDefault,
		PresetId: DnsPresetIdDefault,
		LabelKey: dnsProviderKeyDefault,
		CopyKey:  dnsProviderKeyDefaultCopy,
	})
	for _, preset := range dnsPresetsInDisplayOrder(catalog) {
		exported := newDnsPreset(catalog, preset)
		picks.Add(&DnsPick{
			Kind:     DnsPickKindPreset,
			PresetId: preset.presetId,
			Group:    preset.group,
			GroupKey: exported.GroupKey,
			CopyKey:  exported.CopyKey,
			Preset:   exported,
		})
	}
	picks.Add(&DnsPick{
		Kind:     DnsPickKindCustom,
		LabelKey: dnsProviderKeyCustom,
		CopyKey:  dnsProviderKeyCustomCopy,
	})
	countryCode := strings.ToLower(strings.TrimSpace(exitCountryCode))
	if dnsRegionalCountryCodeValid(countryCode) && HasRegionalDnsRecommendation(countryCode) {
		picks.Add(&DnsPick{
			Kind:        DnsPickKindRegional,
			PresetId:    DnsPresetIdRegionalPrefix + countryCode,
			LabelKey:    dnsProviderKeyRegional,
			CopyKey:     dnsProviderKeyRegionalCopy,
			CountryCode: countryCode,
			Unencrypted: true,
		})
	}
	return picks
}

// The id in what a user typed or pasted for a personal preset (D3): trimmed,
// taken out of the provider's dashboard DoH url or DoT hostname with any
// device name dropped, and the provider's case rule applied. Input it cannot
// read is returned trimmed, for the validator to reject.
func NormalizeDnsPresetPersonalId(presetId string, input string) string {
	return normalizeDnsPresetPersonalId(offeredDnsPreset(currentDnsPresetCatalog(), presetId), input)
}

// The error id of a personal id for a selection (DnsPresetError*), or empty
// when it is valid (D4). The id may be what the user typed or pasted. A
// selection that takes no id accepts only an empty one.
func ValidateDnsPresetPersonalId(presetId string, personalId string) (errorId string) {
	if presetId == DnsPresetIdDefault || strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix) {
		if strings.TrimSpace(personalId) != "" {
			return DnsPresetErrorPersonalIdNotAccepted
		}
		return ""
	}
	catalog := currentDnsPresetCatalog()
	preset := offeredDnsPreset(catalog, presetId)
	if preset == nil {
		return DnsPresetErrorUnknown
	}
	return validateDnsPresetPersonalIdOf(catalog, preset, personalId)
}

// base with a pick selected: PresetId and PresetPersonalId set (the id
// normalized), the lists, toggles and mask those the pick resolves with, and
// EnableFallback kept. The pick is DnsPresetIdDefault, a preset the catalog
// offers, or "regional-<cc>" for a country with a recommendation. nil when
// the pick is none of those or the personal id does not validate. A nil base
// starts from the defaults.
func DnsResolverSettingsWithPreset(base *DnsResolverSettings, presetId string, personalId string) *DnsResolverSettings {
	switch {
	case presetId == DnsPresetIdDefault:
	case strings.HasPrefix(presetId, DnsPresetIdRegionalPrefix):
		countryCode := strings.TrimPrefix(presetId, DnsPresetIdRegionalPrefix)
		if !dnsRegionalCountryCodeValid(countryCode) || !HasRegionalDnsRecommendation(countryCode) {
			return nil
		}
	default:
		if offeredDnsPreset(currentDnsPresetCatalog(), presetId) == nil {
			return nil
		}
	}
	if ValidateDnsPresetPersonalId(presetId, personalId) != "" {
		return nil
	}
	if base == nil {
		base = GetDefaultDnsResolverSettings()
	}
	settings := copyDnsResolverSettings(base)
	settings.PresetId = presetId
	settings.PresetPersonalId = personalId
	return ownDnsResolverSettings(settings)
}

// base with Fast DNS on connect turned on or off; the selection and the lists
// are kept. A nil base starts from the defaults.
func DnsResolverSettingsWithFallback(base *DnsResolverSettings, enableFallback bool) *DnsResolverSettings {
	if base == nil {
		base = GetDefaultDnsResolverSettings()
	}
	settings := copyDnsResolverSettings(base)
	settings.EnableFallback = enableFallback
	return settings
}

// base as custom settings for the advanced editor to start from: the lists,
// toggles and mask in force for its selection, PresetId and PresetPersonalId
// cleared, and EnableFallback kept. A nil base starts from the defaults.
func DnsResolverSettingsAsCustom(base *DnsResolverSettings) *DnsResolverSettings {
	if base == nil {
		base = GetDefaultDnsResolverSettings()
	}
	settings := ownDnsResolverSettings(base)
	settings.PresetId = ""
	settings.PresetPersonalId = ""
	return settings
}

// A deep copy of every field.
func CopyDnsResolverSettings(settings *DnsResolverSettings) *DnsResolverSettings {
	return copyDnsResolverSettings(settings)
}

// What the summary row and the screen show for settings, given the exit's
// country (A5, F1, F2): the settings as a save would apply them. Nil settings
// describe the defaults.
func DescribeDnsResolverSettings(settings *DnsResolverSettings, exitCountryCode string) *DnsSelectionSummary {
	if settings == nil {
		settings = GetDefaultDnsResolverSettings()
	}
	settings = dnsResolverSettingsAsEdited(settings)
	catalog := currentDnsPresetCatalog()
	selection := resolveDnsResolverSelection(settings.PresetId, settings.PresetPersonalId)
	summary := &DnsSelectionSummary{}
	switch {
	case selection.custom():
		summary.Kind = DnsPickKindCustom
		summary.LabelKey = dnsProviderKeyCustom
		summary.Unencrypted = !settings.EnableRemoteDoh && !settings.EnableLocalDoh &&
			(settings.EnableRemoteDns || settings.EnableLocalDns)
		if summary.Unencrypted {
			summary.SummaryKey = dnsSummaryKeyCustomUnencrypted
		} else {
			summary.SummaryKey = dnsSummaryKeyCustom
		}
	case selection.preset != nil:
		preset := newDnsPreset(catalog, selection.preset)
		summary.Kind = DnsPickKindPreset
		summary.PresetId = preset.PresetId
		summary.ProviderId = preset.ProviderId
		summary.ProviderName = preset.ProviderName
		summary.Group = preset.Group
		summary.GroupKey = preset.GroupKey
		summary.CopyKey = preset.CopyKey
		summary.SummaryKey = dnsPresetSummaryKey(preset.Group)
		summary.Personal = preset.PersonalIdRequired
		summary.Failover = preset.Failover
		summary.ShowBlockedCount = preset.Filtering
	case selection.regional():
		summary.Kind = DnsPickKindRegional
		summary.PresetId = settings.PresetId
		summary.LabelKey = dnsProviderKeyRegional
		summary.SummaryKey = dnsSummaryKeyRegional
		summary.CountryCode = selection.countryCode
		summary.Unencrypted = true
	default:
		summary.Kind = DnsPickKindDefault
		summary.PresetId = DnsPresetIdDefault
		summary.LabelKey = dnsProviderKeyDefault
		summary.SummaryKey = dnsSummaryKeyDefault
		if selection.unavailable {
			summary.Dropped = true
			summary.DroppedPresetId = settings.PresetId
			summary.DroppedProviderName = selection.unavailableProviderName
		}
	}
	if summary.Kind != DnsPickKindPreset {
		// today's rules, on the settings as they apply
		applied := ownDnsResolverSettings(settings)
		countryCode := strings.ToLower(strings.TrimSpace(exitCountryCode))
		if recommended := GetRecommendedDnsResolverSettings(countryCode); recommended != nil {
			if !dnsResolverSettingsPillEqual(applied, recommended) {
				summary.PillKey = dnsPillKeyRecommended
				summary.PillCountryCode = countryCode
			}
		} else if defaults := GetDefaultDnsResolverSettings(); !dnsResolverSettingsPillEqual(applied, defaults) {
			summary.PillKey = dnsPillKeyDefaults
		}
	}
	return summary
}

// Whether two settings are the same for the pill, as the apps compared them
// before the presets: every resolver toggle, Fast DNS on connect and every
// list in order, an absent list the same as an empty one. The selection and
// the mask do not count.
func dnsResolverSettingsPillEqual(a *DnsResolverSettings, b *DnsResolverSettings) bool {
	return a.EnableFallback == b.EnableFallback && dnsResolverSettingsListsEqual(a, b)
}

// What the status line, the verification line and the blocked count show for
// the settings in force and the device's status (C6, C8, I3). Only a preset
// selection has a status line. A status that describes another selection
// than the settings, as during a change, reads as unknown.
func DescribeDnsResolverStatus(settings *DnsResolverSettings, status *DnsResolverStatus) *DnsStatusSummary {
	summary := &DnsStatusSummary{}
	if settings == nil {
		return summary
	}
	catalog := currentDnsPresetCatalog()
	selection := resolveDnsResolverSelection(settings.PresetId, settings.PresetPersonalId)
	preset := selection.preset
	if preset == nil {
		return summary
	}
	if provider := catalog.provider(preset.providerId); provider != nil {
		summary.ProviderName = provider.name
	}
	if status == nil || status.PresetId != settings.PresetId {
		status = unknownDnsResolverStatus()
	}
	switch status.State {
	case DnsResolverStateFailedOver:
		if preset.group == DnsPresetGroupUnfiltered {
			// a no-filtering preset pauses no ad blocking
			summary.StatusKey = dnsStatusKeyFallback
		} else {
			summary.StatusKey = dnsStatusKeyPaused
			summary.StatusDetailKey = dnsStatusKeyPausedDetail
		}
		summary.ShowUseDefault = true
		summary.ShowChangeLocation = true
	case DnsResolverStateFailing:
		summary.StatusKey = dnsStatusKeyUnreachable
		if !preset.failover {
			// a preset that fails over says nothing about staying closed
			// while its failover has not answered yet
			summary.StatusDetailKey = dnsStatusKeyUnreachableDetail
		}
		summary.ShowUseDefault = true
		summary.ShowChangeLocation = true
	default:
		summary.StatusKey = dnsStatusKeyProvider
	}
	switch status.Verification {
	case DnsResolverVerificationNotFiltering:
		summary.VerificationKey = dnsVerifyKeyNotFiltering
		summary.ShowChangeLocation = true
	case DnsResolverVerificationUnconfirmed:
		summary.VerificationKey = dnsVerifyKeyUnconfirmed
		if preset.personalId != nil {
			summary.VerificationUrl = preset.personalId.idHelpUrl
			summary.VerificationLinkKey = dnsVerifyKeyDashboardLink
		}
	case DnsResolverVerificationRejected:
		summary.VerificationKey = dnsVerifyKeyRejected
	case DnsResolverVerificationMalformed:
		summary.VerificationKey = dnsVerifyKeyMalformed
	}
	if dnsPresetFiltering(preset) {
		summary.ShowBlockedCount = true
		summary.BlockedCount = status.BlockedCount
	}
	return summary
}
