package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Explicit autosave commits the toggle before returning; constructor replay
// remains off and the replacement restores only through checked Load.
func TestDeviceLocalBlockerEnabledPersistRestore(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.localState
	device := testingPreferenceDevice(t, fixture)
	connect.AssertEqual(t, false, device.GetBlockerEnabled())
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}

	device.SetBlockerEnabled(true)
	connect.AssertEqual(t, true, device.GetBlockerEnabled())
	connect.AssertEqual(t, true, localState.GetBlockerEnabled())
	testingJoinPreferenceDevice(t, device)

	restored := testingPreferenceDevice(t, fixture)
	connect.AssertEqual(t, false, restored.GetBlockerEnabled())
	if result, err := restored.Load(); err != nil || result == nil || result.GetPreferenceError("blocker-enabled") != "" {
		t.Fatal("checked blocker restoration failed")
	}
	connect.AssertEqual(t, true, restored.GetBlockerEnabled())
}
