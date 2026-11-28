package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The hotspot proxy's choice and credentials are account state: a sign-out
// removes them, a late write while signed out is dropped, and the next account
// starts with the proxy off.
func TestLocalStateHotspotProxyPreferencesAreAccountState(t *testing.T) {
	space := testingAuthClientShapeSpace(t)
	space.seedDistinctLogin(t)
	localState := space.localState

	if preferences := localState.GetHotspotProxyPreferences(); preferences.Enabled || preferences.Username != "" || preferences.Password != "" {
		t.Fatalf("a new space's hotspot proxy = %+v, want off with no credentials", preferences)
	}
	on := &HotspotProxyPreferences{
		Enabled:  true,
		Username: "ur-synthetic",
		Password: "SyntheticPassword0001",
	}
	if err := localState.SetHotspotProxyPreferences(on); err != nil {
		t.Fatal(err)
	}
	if preferences := localState.GetHotspotProxyPreferences(); *preferences != *on {
		t.Fatalf("stored %+v, want %+v", preferences, on)
	}

	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if preferences := localState.GetHotspotProxyPreferences(); preferences.Enabled || preferences.Password != "" {
		t.Fatalf("after the sign-out the hotspot proxy = %+v, want off with no credentials", preferences)
	}

	// a proxy of the signed-out account writes late
	if err := localState.SetHotspotProxyPreferences(on); err != nil {
		t.Fatal(err)
	}
	space.seedDistinctLogin(t)
	if preferences := localState.GetHotspotProxyPreferences(); preferences.Enabled || preferences.Password != "" {
		t.Fatalf("a late write of the signed-out account reached the next sign-in: %+v", preferences)
	}

	if err := localState.SetHotspotProxyPreferences(on); err != nil {
		t.Fatal(err)
	}
	if preferences := localState.GetHotspotProxyPreferences(); *preferences != *on {
		t.Fatalf("the next sign-in stored %+v, want %+v", preferences, on)
	}
}

// A damaged file never turns the proxy on.
func TestLocalStateHotspotProxyDamagedPreferencesAreOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localState := newLocalState(ctx, t.TempDir())
	defer localState.Close()
	for _, contents := range []string{`{"enabled": true, "username": "ur-x", "password": `, "enabled", ""} {
		if err := os.WriteFile(filepath.Join(localState.localStorageDir, hotspotProxyFileName), []byte(contents), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		if preferences := localState.GetHotspotProxyPreferences(); preferences.Enabled || preferences.Username != "" {
			t.Errorf("file %q reads as %+v, want off with no credentials", contents, preferences)
		}
	}
	if err := localState.SetHotspotProxyPreferences(nil); err != nil {
		t.Fatal(err)
	}
	if preferences := localState.GetHotspotProxyPreferences(); preferences.Enabled {
		t.Fatalf("storing no preferences reads as %+v, want off", preferences)
	}
}
