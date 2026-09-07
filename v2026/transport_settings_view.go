//go:build !ios_extension

package sdk

// The transport policy helpers the apps use to render and edit the policy:
// the stable stats order, the selectable modes in default preference order,
// the carriers a policy enables, and the Auto editing rules. They live in
// their own file, outside the network extension build, because the extension
// only applies a policy -- it never renders or edits one -- and the extension
// slice has a compiled-size budget. The rules are here rather than in each app
// so they are tested once (see transport_settings_view_test.go) and reused
// everywhere.

import (
	"slices"
)

// transportTypes is the stable public stats order, mirroring
// connect.TransportTypes: the selectable carriers in production mode order,
// then p2p, then unknown. Unknown is a real bucket: admitted traffic waiting
// for its first physical route write lives there until it is attributed.
func transportTypes() []TransportType {
	return []TransportType{
		TransportTypeH3,
		TransportTypeH1,
		TransportTypeDns,
		TransportTypeDnsPump,
		TransportTypeP2p,
		TransportTypeUnknown,
	}
}

// SelectableTransportModes returns the modes a policy can select (every mode
// but Auto) in the default H1-first preference order. AutoModes reports the
// separately sorted order of the policy it is called on.
func SelectableTransportModes() *StringList {
	modes := NewStringList()
	modes.addAll(selectableTransportModes()...)
	return modes
}

func selectableTransportModes() []TransportMode {
	return []TransportMode{
		TransportModeH1,
		TransportModeH3,
		TransportModeDns,
		TransportModeDnsPump,
	}
}

// DefaultTransportModePriority is the priority a mode has in the default Auto
// policy. A mode outside the default policy sorts after everything in it.
func DefaultTransportModePriority(mode TransportMode) int {
	maxPriority := 0
	for _, item := range DefaultTransportSettings().AutoModePriorities.getAll() {
		if item.Mode == mode {
			return item.Priority
		}
		maxPriority = max(maxPriority, item.Priority)
	}
	return maxPriority + 1
}

// transportTypeFromMode maps a selectable mode to the transport type that
// carries it. The strings are identical by design; this keeps the mapping in one
// place should they ever diverge
func transportTypeFromMode(mode TransportMode) TransportType {
	switch mode {
	case TransportModeH3:
		return TransportTypeH3
	case TransportModeH1:
		return TransportTypeH1
	case TransportModeDns:
		return TransportTypeDns
	case TransportModeDnsPump:
		return TransportTypeDnsPump
	default:
		return TransportTypeUnknown
	}
}

// Clone returns an independent copy, e.g. an editable draft
func (self *TransportSettings) Clone() *TransportSettings {
	return cloneTransportSettings(self)
}

// Equals compares the normalized policies
func (self *TransportSettings) Equals(other *TransportSettings) bool {
	return transportSettingsEqual(self, other, false)
}

// AutoModes returns the modes enabled under Auto in preference order: by
// priority, then the default order among equals. The Auto policy is retained
// while a single mode is selected, so this reads the same either way.
func (self *TransportSettings) AutoModes() *StringList {
	modes := NewStringList()
	modes.addAll(self.autoModes()...)
	return modes
}

func (self *TransportSettings) autoModes() []TransportMode {
	normalized := normalizeTransportSettings(self, false)
	modes := []TransportMode{}
	for _, item := range normalized.AutoModePriorities.getAll() {
		modes = append(modes, item.Mode)
	}
	return modes
}

// IsAutoModeEnabled reports whether a mode is enabled under Auto
func (self *TransportSettings) IsAutoModeEnabled(mode TransportMode) bool {
	return slices.Contains(self.autoModes(), mode)
}

// SetAutoModeEnabled enables or disables one mode under Auto, editing the policy
// in place. A newly enabled mode takes its default priority, so the default
// preference order is preserved without the caller managing priorities;
// re-enabling an enabled mode keeps its priority. Disabling the last enabled
// mode is refused: an empty Auto policy normalizes to the full default, which
// would silently re-enable everything. Returns whether the policy changed.
func (self *TransportSettings) SetAutoModeEnabled(mode TransportMode, enabled bool) bool {
	if !validAutoTransportMode(mode) {
		return false
	}
	normalized := normalizeTransportSettings(self, false)
	items := normalized.AutoModePriorities.getAll()
	if enabled {
		for _, item := range items {
			if item.Mode == mode {
				return false
			}
		}
		items = append(items, &TransportModePriority{
			Mode:     mode,
			Priority: DefaultTransportModePriority(mode),
		})
	} else {
		remaining := []*TransportModePriority{}
		for _, item := range items {
			if item.Mode != mode {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == len(items) || len(remaining) == 0 {
			return false
		}
		items = remaining
	}
	list := NewTransportModePriorityList()
	list.addAll(items...)
	self.AutoModePriorities = list
	// re-normalize so the retained order is canonical
	self.AutoModePriorities = normalizeTransportSettings(self, false).AutoModePriorities
	return true
}

// EnabledTransportTypes returns the transport types the policy enables, in
// preference order: the single mode's carrier, or the Auto modes' carriers.
// p2p and unknown are observable carriers only and are never enabled.
func (self *TransportSettings) EnabledTransportTypes() *StringList {
	types := NewStringList()
	types.addAll(self.enabledTransportTypes()...)
	return types
}

func (self *TransportSettings) enabledTransportTypes() []TransportType {
	normalized := normalizeTransportSettings(self, false)
	if normalized.Mode != TransportModeAuto {
		return []TransportType{transportTypeFromMode(normalized.Mode)}
	}
	types := []TransportType{}
	for _, mode := range normalized.autoModes() {
		types = append(types, transportTypeFromMode(mode))
	}
	return types
}

// The same rules as free functions over policies passed by value, for bindings
// that cross data structs as json (the desktop c abi) and so cannot call the
// methods above. The input policy is never modified.

// TransportSettingsAutoModes is `TransportSettings.AutoModes` for a policy
// passed by value
func TransportSettingsAutoModes(settings *TransportSettings) *StringList {
	return normalizeTransportSettings(settings, false).AutoModes()
}

// TransportSettingsEnabledTransportTypes is
// `TransportSettings.EnabledTransportTypes` for a policy passed by value
func TransportSettingsEnabledTransportTypes(settings *TransportSettings) *StringList {
	return normalizeTransportSettings(settings, false).EnabledTransportTypes()
}

// TransportSettingsWithAutoModeEnabled returns a copy of the policy with one
// Auto mode enabled or disabled per `TransportSettings.SetAutoModeEnabled`.
// The copy equals the input when the edit is refused (e.g. disabling the last
// enabled mode).
func TransportSettingsWithAutoModeEnabled(settings *TransportSettings, mode TransportMode, enabled bool) *TransportSettings {
	edited := normalizeTransportSettings(settings, false)
	edited.SetAutoModeEnabled(mode, enabled)
	return edited
}

// TransportSettingsWithMode returns a copy of the policy selecting one mode
// (Auto or a single carrier), retaining the Auto policy
func TransportSettingsWithMode(settings *TransportSettings, mode TransportMode) *TransportSettings {
	edited := normalizeTransportSettings(settings, false)
	if validTransportMode(mode) {
		edited.Mode = mode
	}
	return edited
}

// TransportSettingsEqual is `TransportSettings.Equals` for policies passed by
// value
func TransportSettingsEqual(a *TransportSettings, b *TransportSettings) bool {
	return transportSettingsEqual(a, b, false)
}
