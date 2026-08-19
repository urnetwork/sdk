//go:build !ios_extension

package sdk

import (
	"slices"
	"testing"
)

// TestTransportSettingsEditingHelpers asserts the shared editing rules every
// app relies on: the selectable modes in default order, the enabled carriers
// of a policy, enabling under Auto at the default priority (order preserved),
// refusing to disable the last Auto mode, and clone/equals for drafts
func TestTransportSettingsEditingHelpers(t *testing.T) {
	modes := SelectableTransportModes()
	if modes.Len() != 4 || modes.Get(0) != TransportModeH3 || modes.Get(1) != TransportModeH1 || modes.Get(2) != TransportModeDns || modes.Get(3) != TransportModeDnsPump {
		t.Fatalf("unexpected selectable modes: %v", modes.getAll())
	}
	if DefaultTransportModePriority(TransportModeH3) != 1 || DefaultTransportModePriority(TransportModeH1) != 1 ||
		DefaultTransportModePriority(TransportModeDns) != 2 || DefaultTransportModePriority(TransportModeDnsPump) != 3 {
		t.Fatalf("unexpected default priorities")
	}
	if DefaultTransportModePriority("bogus") != 4 {
		t.Fatalf("expected an unknown mode to sort after the defaults, got %d", DefaultTransportModePriority("bogus"))
	}

	// the default policy enables every carrier in default order
	settings := DefaultTransportSettings()
	if got := settings.EnabledTransportTypes().getAll(); !slices.Equal(got, []string{TransportTypeH3, TransportTypeH1, TransportTypeDns, TransportTypeDnsPump}) {
		t.Fatalf("unexpected default enabled types: %v", got)
	}
	// a single mode enables its carrier only, the auto policy is retained
	settings.Mode = TransportModeDns
	if got := settings.EnabledTransportTypes().getAll(); !slices.Equal(got, []string{TransportTypeDns}) {
		t.Fatalf("unexpected single-mode enabled types: %v", got)
	}
	if got := settings.AutoModes().getAll(); !slices.Equal(got, []string{TransportModeH3, TransportModeH1, TransportModeDns, TransportModeDnsPump}) {
		t.Fatalf("expected the auto policy retained under a single mode, got %v", got)
	}
	settings.Mode = TransportModeAuto

	// disable down to one; the last refuses
	if !settings.SetAutoModeEnabled(TransportModeH3, false) || !settings.SetAutoModeEnabled(TransportModeDns, false) || !settings.SetAutoModeEnabled(TransportModeDnsPump, false) {
		t.Fatalf("expected disables to apply")
	}
	if got := settings.AutoModes().getAll(); !slices.Equal(got, []string{TransportModeH1}) {
		t.Fatalf("expected only h1, got %v", got)
	}
	if settings.SetAutoModeEnabled(TransportModeH1, false) {
		t.Fatalf("expected the last auto mode to refuse disabling")
	}
	if !settings.IsAutoModeEnabled(TransportModeH1) || settings.IsAutoModeEnabled(TransportModeH3) {
		t.Fatalf("unexpected auto enabled state")
	}
	// disabling a mode that is not enabled, or an invalid mode, is a no-op
	if settings.SetAutoModeEnabled(TransportModeH3, false) || settings.SetAutoModeEnabled(TransportModeAuto, true) || settings.SetAutoModeEnabled("p2p", true) {
		t.Fatalf("expected no-op edits to report no change")
	}

	// re-enable at the default priorities: the default order is preserved
	// regardless of the enable order
	if !settings.SetAutoModeEnabled(TransportModeDnsPump, true) || !settings.SetAutoModeEnabled(TransportModeH3, true) {
		t.Fatalf("expected enables to apply")
	}
	if got := settings.AutoModes().getAll(); !slices.Equal(got, []string{TransportModeH3, TransportModeH1, TransportModeDnsPump}) {
		t.Fatalf("expected default order h3, h1, dnspump, got %v", got)
	}
	// enabling an enabled mode keeps its (custom) priority
	settings.AutoModePriorities.Get(1).Priority = 7
	if settings.SetAutoModeEnabled(TransportModeH1, true) {
		t.Fatalf("expected re-enable to be a no-op")
	}
	if got := settings.AutoModes().getAll(); !slices.Equal(got, []string{TransportModeH3, TransportModeDnsPump, TransportModeH1}) {
		t.Fatalf("expected the custom priority to sort h1 last, got %v", got)
	}

	// clone is independent; equals compares the normalized policy
	clone := settings.Clone()
	if !clone.Equals(settings) {
		t.Fatalf("expected the clone to equal the original")
	}
	clone.SetAutoModeEnabled(TransportModeDns, true)
	if clone.Equals(settings) || settings.IsAutoModeEnabled(TransportModeDns) {
		t.Fatalf("expected the clone to edit independently")
	}
	// an empty auto list is the default policy
	empty := &TransportSettings{Mode: TransportModeAuto}
	if !empty.Equals(DefaultTransportSettings()) {
		t.Fatalf("expected an empty auto policy to equal the default")
	}
	if got := empty.EnabledTransportTypes().getAll(); len(got) != 4 {
		t.Fatalf("expected the empty auto policy to enable the defaults, got %v", got)
	}
}

// TestTransportSettingsValueHelpers asserts the by-value variants (the desktop
// c abi crosses policies as json) match the methods and never modify the input
func TestTransportSettingsValueHelpers(t *testing.T) {
	settings := DefaultTransportSettings()
	original := settings.Clone()

	edited := TransportSettingsWithAutoModeEnabled(settings, TransportModeH3, false)
	if !settings.Equals(original) {
		t.Fatalf("expected the input policy untouched")
	}
	if got := TransportSettingsAutoModes(edited).getAll(); !slices.Equal(got, []string{TransportModeH1, TransportModeDns, TransportModeDnsPump}) {
		t.Fatalf("expected h3 disabled in the copy, got %v", got)
	}
	// a refused edit returns an equal copy
	single := &TransportSettings{Mode: TransportModeAuto}
	single = TransportSettingsWithAutoModeEnabled(single, TransportModeH3, false)
	single = TransportSettingsWithAutoModeEnabled(single, TransportModeH1, false)
	single = TransportSettingsWithAutoModeEnabled(single, TransportModeDns, false)
	refused := TransportSettingsWithAutoModeEnabled(single, TransportModeDnsPump, false)
	if !TransportSettingsEqual(single, refused) {
		t.Fatalf("expected the refused edit to return an equal policy")
	}
	if got := TransportSettingsEnabledTransportTypes(refused).getAll(); !slices.Equal(got, []string{TransportTypeDnsPump}) {
		t.Fatalf("expected dnspump only, got %v", got)
	}

	// mode selection retains the auto policy; an invalid mode is ignored
	h1 := TransportSettingsWithMode(edited, TransportModeH1)
	if h1.Mode != TransportModeH1 || !slices.Equal(TransportSettingsAutoModes(h1).getAll(), TransportSettingsAutoModes(edited).getAll()) {
		t.Fatalf("expected a single mode with the auto policy retained, got %+v", h1)
	}
	if got := TransportSettingsEnabledTransportTypes(h1).getAll(); !slices.Equal(got, []string{TransportTypeH1}) {
		t.Fatalf("expected h1 enabled only, got %v", got)
	}
	if bogus := TransportSettingsWithMode(h1, "bogus"); bogus.Mode != TransportModeH1 {
		t.Fatalf("expected an invalid mode to be ignored, got %s", bogus.Mode)
	}
	if !TransportSettingsEqual(TransportSettingsWithMode(h1, TransportModeAuto), edited) {
		t.Fatalf("expected switching back to auto to restore the edited policy")
	}
	// a nil policy is the default
	if !TransportSettingsEqual(TransportSettingsWithMode(nil, TransportModeAuto), DefaultTransportSettings()) {
		t.Fatalf("expected a nil policy to read as the default")
	}
}
