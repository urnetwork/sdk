package sdk

// This file owns the gomobile-safe Device transport policy. The Connect
// carrier names remain internal; apps use the stable h3, h1, dns, dns-pump,
// and auto vocabulary below.

import (
	"cmp"
	"slices"

	"github.com/urnetwork/connect"
)

type TransportMode = string

type TransportType = string

const (
	// Keep these constants untyped: gomobile exports bare string constants to
	// Swift/Objective-C and Java, but silently skips constants explicitly typed
	// through a string alias.
	TransportModeAuto    = "auto"
	TransportModeH3      = "h3"
	TransportModeH1      = "h1"
	TransportModeDns     = "dns"
	TransportModeDnsPump = "dnspump"
)

const (
	TransportTypeH3      = "h3"
	TransportTypeH1      = "h1"
	TransportTypeDns     = "dns"
	TransportTypeDnsPump = "dnspump"
	TransportTypeP2p     = "p2p"
	TransportTypeUnknown = "unknown"
)

// TransportModePriority enables one mode in Auto. Lower priorities are tried
// first; every healthy mode with the same best live priority remains active in
// parallel.
type TransportModePriority struct {
	Mode     TransportMode `json:"mode"`
	Priority int           `json:"priority"`
}

type TransportModePriorityList struct {
	exportedList[*TransportModePriority]
}

func NewTransportModePriorityList() *TransportModePriorityList {
	return &TransportModePriorityList{
		exportedList: *newExportedList[*TransportModePriority](),
	}
}

// TransportSettings selects one carrier or an Auto policy. AutoModePriorities
// is retained for every mode so callers can edit and switch back to Auto
// without reconstructing the policy.
type TransportSettings struct {
	Mode               TransportMode              `json:"mode"`
	AutoModePriorities *TransportModePriorityList `json:"auto_mode_priorities"`
}

// DefaultTransportSettings makes H1 the primary Auto carrier. Direct H3
// remains available when H1 is unavailable, followed by DNS and DNS pump as
// progressively lower availability fallbacks. Explicit H3 selection bypasses
// this Auto ordering.
func DefaultTransportSettings() *TransportSettings {
	priorities := NewTransportModePriorityList()
	priorities.Add(&TransportModePriority{Mode: TransportModeH1, Priority: 1})
	priorities.Add(&TransportModePriority{Mode: TransportModeH3, Priority: 2})
	priorities.Add(&TransportModePriority{Mode: TransportModeDns, Priority: 3})
	priorities.Add(&TransportModePriority{Mode: TransportModeDnsPump, Priority: 4})
	return &TransportSettings{
		Mode:               TransportModeAuto,
		AutoModePriorities: priorities,
	}
}

// DefaultProviderTransportSettings currently matches the client policy but is
// a distinct constructor so provider defaults can evolve independently.
func DefaultProviderTransportSettings() *TransportSettings {
	return cloneTransportSettings(DefaultTransportSettings())
}

// hostedTransportSettings is the immutable server/proxy policy. Hosted
// devices cannot safely expose UDP/H3 or DNS carrier selection to a remote app.
func hostedTransportSettings() *TransportSettings {
	settings := DefaultTransportSettings()
	settings.Mode = TransportModeH1
	return settings
}

func validTransportMode(mode TransportMode) bool {
	switch mode {
	case TransportModeAuto, TransportModeH3, TransportModeH1, TransportModeDns, TransportModeDnsPump:
		return true
	default:
		return false
	}
}

func validAutoTransportMode(mode TransportMode) bool {
	return validTransportMode(mode) && mode != TransportModeAuto
}

func autoTransportModeOrder(mode TransportMode) int {
	switch mode {
	case TransportModeH1:
		return 0
	case TransportModeH3:
		return 1
	case TransportModeDns:
		return 2
	case TransportModeDnsPump:
		return 3
	default:
		return 4
	}
}

// migrateLegacyDefaultTransportPriorities recognizes complete or partial Auto
// policies composed entirely of the defaults shipped before H1 became the
// strict primary. TransportSettings retains this policy while an explicit mode
// is selected, so without this migration an installed client could later switch
// back to Auto—or re-enable H1—and silently restore the obsolete H1/H3 tie.
// Any genuinely custom priority leaves the complete policy untouched.
func migrateLegacyDefaultTransportPriorities(prioritiesByMode map[TransportMode]int) {
	legacy := map[TransportMode]int{
		TransportModeH3:      1,
		TransportModeH1:      1,
		TransportModeDns:     2,
		TransportModeDnsPump: 3,
	}
	for mode, priority := range prioritiesByMode {
		if legacy[mode] != priority {
			return
		}
	}
	for _, item := range DefaultTransportSettings().AutoModePriorities.getAll() {
		if _, ok := prioritiesByMode[item.Mode]; ok {
			prioritiesByMode[item.Mode] = item.Priority
		}
	}
}

// normalizeTransportSettings creates an immutable canonical copy. A nil,
// malformed, or empty Auto policy safely resolves to the production default;
// duplicate mode rows use the final value and output is stable by priority and
// production mode order (H1, H3, DNS, DNS pump).
func normalizeTransportSettings(settings *TransportSettings, provider bool) *TransportSettings {
	defaultSettings := DefaultTransportSettings()
	if provider {
		defaultSettings = DefaultProviderTransportSettings()
	}
	if settings == nil {
		return defaultSettings
	}

	mode := settings.Mode
	if !validTransportMode(mode) {
		mode = TransportModeAuto
	}
	prioritiesByMode := map[TransportMode]int{}
	if settings.AutoModePriorities != nil {
		for _, item := range settings.AutoModePriorities.getAll() {
			if item != nil && validAutoTransportMode(item.Mode) && 0 < item.Priority {
				prioritiesByMode[item.Mode] = item.Priority
			}
		}
	}
	if len(prioritiesByMode) == 0 {
		return &TransportSettings{
			Mode:               mode,
			AutoModePriorities: cloneTransportSettings(defaultSettings).AutoModePriorities,
		}
	}
	migrateLegacyDefaultTransportPriorities(prioritiesByMode)

	priorities := make([]*TransportModePriority, 0, len(prioritiesByMode))
	for autoMode, priority := range prioritiesByMode {
		priorities = append(priorities, &TransportModePriority{
			Mode:     autoMode,
			Priority: priority,
		})
	}
	slices.SortFunc(priorities, func(a *TransportModePriority, b *TransportModePriority) int {
		if order := cmp.Compare(a.Priority, b.Priority); order != 0 {
			return order
		}
		return cmp.Compare(autoTransportModeOrder(a.Mode), autoTransportModeOrder(b.Mode))
	})
	list := NewTransportModePriorityList()
	list.addAll(priorities...)
	return &TransportSettings{
		Mode:               mode,
		AutoModePriorities: list,
	}
}

func cloneTransportSettings(settings *TransportSettings) *TransportSettings {
	if settings == nil {
		return nil
	}
	clone := &TransportSettings{Mode: settings.Mode, AutoModePriorities: NewTransportModePriorityList()}
	if settings.AutoModePriorities != nil {
		for _, item := range settings.AutoModePriorities.getAll() {
			if item != nil {
				itemClone := *item
				clone.AutoModePriorities.Add(&itemClone)
			}
		}
	}
	return clone
}

func transportSettingsEqual(a *TransportSettings, b *TransportSettings, provider bool) bool {
	a = normalizeTransportSettings(a, provider)
	b = normalizeTransportSettings(b, provider)
	if a.Mode != b.Mode || a.AutoModePriorities.Len() != b.AutoModePriorities.Len() {
		return false
	}
	for index := 0; index < a.AutoModePriorities.Len(); index += 1 {
		left := a.AutoModePriorities.Get(index)
		right := b.AutoModePriorities.Get(index)
		if left == nil || right == nil || *left != *right {
			return false
		}
	}
	return true
}

func toConnectTransportMode(mode TransportMode) connect.TransportMode {
	switch mode {
	case TransportModeH3:
		return connect.TransportModeH3
	case TransportModeH1:
		return connect.TransportModeH1
	case TransportModeDns:
		return connect.TransportModeH3Dns
	case TransportModeDnsPump:
		return connect.TransportModeH3DnsPump
	default:
		return connect.TransportModeAuto
	}
}

func toConnectTransportPolicy(settings *TransportSettings, provider bool) (
	connect.TransportMode,
	map[connect.TransportMode]int,
) {
	settings = normalizeTransportSettings(settings, provider)
	mode := toConnectTransportMode(settings.Mode)
	if mode != connect.TransportModeAuto {
		return mode, nil
	}
	return mode, toConnectAutoModePreferences(settings, provider)
}

func toConnectAutoModePreferences(
	settings *TransportSettings,
	provider bool,
) map[connect.TransportMode]int {
	settings = normalizeTransportSettings(settings, provider)
	preferences := map[connect.TransportMode]int{}
	for _, item := range settings.AutoModePriorities.getAll() {
		preferences[toConnectTransportMode(item.Mode)] = item.Priority
	}
	return preferences
}
