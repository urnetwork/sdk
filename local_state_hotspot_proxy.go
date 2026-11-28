package sdk

// local_state_hotspot_proxy.go — what the hotspot proxy (sdk/hotspot)
// keeps of a network space: whether its owner turned the proxy on, and the
// credentials the shared devices sign in with. It is account state, so a
// sign-out removes it with the rest (keptAcrossSignOut does not keep it) and
// the proxy is off for the next account; a write while the space is signed out
// is dropped by the sign-in fence (local_state_sign_in.go).

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const hotspotProxyFileName = ".hotspot_proxy"

// The hotspot proxy choice of a network space and its credentials. The proxy
// listens only while Enabled; the credentials are kept while it is off so the
// shared devices keep their settings.
type HotspotProxyPreferences struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// The stored preferences. A space that stores none, or whose file cannot be
// read or parsed, is off with no credentials: a damaged file never turns the
// proxy on.
func (self *LocalState) GetHotspotProxyPreferences() *HotspotProxyPreferences {
	preferences := &HotspotProxyPreferences{}
	stateBytes, err := os.ReadFile(filepath.Join(self.localStorageDir, hotspotProxyFileName))
	if err != nil {
		return preferences
	}
	if err := json.Unmarshal(stateBytes, preferences); err != nil {
		return &HotspotProxyPreferences{}
	}
	return preferences
}

// Stores the preferences whole. The document is written to a file beside the
// stored one and renamed over it, so a reader never sees half of it. Dropped
// while the space is signed out.
func (self *LocalState) SetHotspotProxyPreferences(preferences *HotspotProxyPreferences) error {
	if preferences == nil {
		preferences = &HotspotProxyPreferences{}
	}
	stateBytes, err := json.Marshal(preferences)
	if err != nil {
		return err
	}
	return self.writeSignedIn(func() error {
		path := filepath.Join(self.localStorageDir, hotspotProxyFileName)
		tempPath := path + ".tmp"
		if err := os.WriteFile(tempPath, stateBytes, LocalStorageFilePermissions); err != nil {
			return err
		}
		return os.Rename(tempPath, path)
	})
}
