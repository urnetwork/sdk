//go:build dnspresetcheck

// The catalog's endpoints for dnspresetcheck (connect ADBLOCK3RD F3), the
// program that checks every preset live before a catalog change merges. Built
// only with the dnspresetcheck tag, with the program, so neither the app
// bindings nor the c abi ever carry it: `go run -tags dnspresetcheck
// ./dnspresetcheck`.
package sdk

import (
	"slices"
)

// One preset as the checker reads it. A personal preset's urls hold the
// provider's example id in place of the user's.
type DnsPresetCheckTarget struct {
	PresetId    string
	ProviderId  string
	Group       string
	DohUrlsIpv4 []string
	DohUrlsIpv6 []string
	PinIpv4s    []string
	PinIpv6s    []string
	// "null" (0.0.0.0 or ::), "nxdomain" (NXDOMAIN with a filtering extended
	// dns error), or empty for a preset that does not filter
	BlockAnswer string
	// a name the preset blocks, or empty
	CanaryName string
	// for a personal preset, how the provider answers the example id, which
	// it does not know: "unfiltered", "refused" or "unfiltered-or-servfail"
	UnknownIdAnswer string
	Checked         string
}

// Every preset of the built-in catalog, hidden or not.
func DnsPresetCheckTargets() []*DnsPresetCheckTarget {
	catalog := builtInDnsPresetCatalog()
	targets := []*DnsPresetCheckTarget{}
	for _, preset := range catalog.presets {
		target := &DnsPresetCheckTarget{
			PresetId:    preset.presetId,
			ProviderId:  preset.providerId,
			Group:       preset.group,
			DohUrlsIpv4: slices.Clone(preset.dohUrlsIpv4),
			DohUrlsIpv6: slices.Clone(preset.dohUrlsIpv6),
			PinIpv4s:    slices.Clone(preset.dohPinIpv4s),
			PinIpv6s:    slices.Clone(preset.dohPinIpv6s),
			BlockAnswer: preset.blockAnswer,
			CanaryName:  preset.canaryName,
			Checked:     preset.checked,
		}
		if personal := preset.personalId; personal != nil {
			example := personal.normalizeCase(personal.example)
			resolver := preset.resolver(example)
			target.DohUrlsIpv4 = resolver.RemoteDohUrlsIpv4
			target.DohUrlsIpv6 = resolver.RemoteDohUrlsIpv6
			target.UnknownIdAnswer = personal.unknownIdAnswer
		}
		targets = append(targets, target)
	}
	return targets
}
