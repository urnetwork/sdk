// Complete boolean tokens distinguish corrupted required policy from an
// intentional false value before any saved connection is adopted.
package sdk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Both required boolean records share the decoder. A malformed file must stop
// all adoption, preserve the bytes for diagnosis, and preserve saved intent.
func TestDeviceLocalPreferenceBooleanMalformedReadPreservesConnectionIntent(t *testing.T) {
	for _, name := range []string{"route-local", "blocker-enabled"} {
		manager, fixture := testingPreferenceSpaceAt(t, t.TempDir())
		fixture.seedDistinctLogin(t)
		device := testingPreferenceDevice(t, fixture)
		device.SetBlockerEnabled(true)
		target := testingSpecificPreferenceLocation()
		if err := fixture.localState.SetConnectLocation(target); err != nil {
			t.Fatal("could not seed saved connection intent")
		}
		if err := fixture.localState.SetCanRefer(true); err != nil {
			t.Fatal("could not seed later preference adoption")
		}
		file, known := localPreferenceFile(name)
		if !known {
			t.Fatal("required boolean is not in the preference catalog")
		}
		path := filepath.Join(fixture.localState.localStorageDir, file)
		for _, invalid := range []string{"private-load-marker", "garbage", "2", "truejunk", "false trailing", "true\x00", "\xff", " \n\t"} {
			if err := os.WriteFile(path, []byte(invalid), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not seed malformed required boolean")
			}
			result, err := device.Load()
			if result != nil || err == nil || err.Error() != "load "+name ||
				device.GetCanRefer() || !device.GetBlockerEnabled() || device.GetRouteLocal() ||
				device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
				t.Fatalf("malformed boolean preference was adopted instead of stopping startup: %s", name)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, []byte(invalid)) {
				t.Fatal("failed required observation changed its stored evidence")
			}
			if saved, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(saved, target) {
				t.Fatal("failed required observation changed saved connection intent")
			}
		}
		testingJoinPreferenceDevice(t, device)
		manager.Close()
	}
}

// Existing writers use true/false; legacy numeric, single-letter and mixed-case
// tokens retain their meaning. Required validation is not a default-to-false.
func TestDeviceLocalPreferenceBooleanLegacyTokensRetainMeaning(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	for _, token := range []struct {
		text  string
		value bool
	}{
		{text: "true", value: true}, {text: "false", value: false},
		{text: "1", value: true}, {text: "0", value: false},
		{text: "t", value: true}, {text: "T", value: true},
		{text: "f", value: false}, {text: "F", value: false},
		{text: "TrUe", value: true}, {text: "FaLsE", value: false},
		{text: " \ttrue ", value: true}, {text: " \tfalse ", value: false},
	} {
		for _, file := range []string{".route_local-2", ".blocker_enabled"} {
			if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, file), []byte(token.text), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not seed existing boolean encoding")
			}
		}
		result, err := device.Load()
		if err != nil || result == nil || !result.GetHasPreference("route-local") || !result.GetHasPreference("blocker-enabled") ||
			device.GetRouteLocal() != token.value || device.GetBlockerEnabled() != token.value ||
			device.GetAutoSave() || device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
			t.Fatalf("valid boolean preference changed meaning or enabled a connection: %q", token.text)
		}
	}
}

// Repairing the actual failed record permits a subsequent explicit Load to
// construct the saved specific consumer without an app listener or new choice.
func TestDeviceLocalPreferenceBooleanRepairedReadRestoresSpecificConsumer(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal("could not seed specific connection")
	}
	device := testingPreferenceDevice(t, fixture)
	path := filepath.Join(fixture.localState.localStorageDir, ".blocker_enabled")
	if err := os.WriteFile(path, []byte("unavailable-policy"), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not seed failed policy observation")
	}
	if result, err := device.Load(); result != nil || err == nil || err.Error() != "load blocker-enabled" || testingPreferenceConsumer(device) != nil {
		t.Fatal("malformed policy did not leave startup pending without a consumer")
	}
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal("could not repair policy through its existing writer")
	}
	result, err := device.Load()
	if err != nil || result == nil || !result.GetHasConnectLocation() || !device.GetBlockerEnabled() ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) == nil || device.GetAutoSave() {
		t.Fatal("repaired required observation did not restore the exact saved consumer")
	}
	if saved, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(saved, target) {
		t.Fatal("explicit recovery rewrote the saved destination")
	}
}
