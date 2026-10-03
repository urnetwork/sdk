package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The physical harness stages in filesDir/acceptance before application.logout.
// Exercise the actual storage-path and Logout implementations, not a simulated
// parent-directory wipe: LocalState owns only <storage home>/.by.
func TestLocalStateLogoutPreservesSiblingAcceptanceCredentials(t *testing.T) {
	for _, layout := range []string{"network-space", "direct-storage-home"} {
		t.Run(layout, func(t *testing.T) {
			filesDir := t.TempDir()
			acceptanceDir := filepath.Join(filesDir, "acceptance")
			if err := os.MkdirAll(acceptanceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(acceptanceDir, "credentials")
			input := []byte("fixture@example.invalid\nsynthetic-password")
			if err := os.WriteFile(credentials, input, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(credentials)
			if err != nil {
				t.Fatal(err)
			}

			storageHome := filesDir
			if layout == "network-space" {
				// This is the exact path builder called by the Android-created
				// NetworkSpaceManager. No network client/fixture is necessary.
				manager := &NetworkSpaceManager{storagePath: filesDir}
				storageHome = manager.envStoragePath(NewNetworkSpaceKey("ur.network", "main"))
				if storageHome != filepath.Join(filesDir, "network_spaces", "ur.network", "main") {
					t.Fatal("network-space storage escaped its expected scoped directory")
				}
			}
			async := NewAsyncLocalState(storageHome)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := async.CloseAndWait(ctx); err != nil {
					t.Error(err)
				}
			})
			localState := async.GetLocalState()
			if localState.localStorageDir != filepath.Join(storageHome, ".by") {
				t.Fatal("local state is not isolated beneath its storage home")
			}
			authMarker := filepath.Join(localState.localStorageDir, "auth-marker")
			if err := os.WriteFile(authMarker, []byte("synthetic-auth-state"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Same synchronous method used by MainApplication.logoutInternal.
			if err := localState.Logout(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(authMarker); !os.IsNotExist(err) {
				t.Fatal("logout did not wipe its owned auth state")
			}
			if info, err := os.Stat(localState.localStorageDir); err != nil || !info.IsDir() {
				t.Fatal("logout did not recreate its owned store")
			}
			output, err := os.ReadFile(credentials)
			if err != nil || string(output) != string(input) {
				t.Fatal("login cannot read the exact staged credentials after logout")
			}
			after, err := os.Stat(credentials)
			if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
				t.Fatal("logout replaced or exposed another owner's acceptance file")
			}
		})
	}
}
