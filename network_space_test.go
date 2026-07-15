package sdk

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

func TestNetworkSpaceManager(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_network_space_manager")
	connect.AssertEqual(t, err, nil)

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 0)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace(), nil)

	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("ur.network", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 1)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Get(0) == networkSpace, true)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace(), nil)

	networkSpaceManager.SetActiveNetworkSpace(networkSpace)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == networkSpace, true)

	networkSpaceManager.RemoveNetworkSpace(networkSpace)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 1)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Get(0) == networkSpace, true)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == networkSpace, true)

	networkSpaceManager.Close()

	networkSpaceManager2 := NewNetworkSpaceManager(storagePath)
	connect.AssertEqual(t, networkSpaceManager2.GetNetworkSpaces().Len(), 1)
	networkSpace = networkSpaceManager2.GetNetworkSpaces().Get(0)
	connect.AssertEqual(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace, true)

	networkSpace2 := networkSpaceManager2.updateNetworkSpace(
		NewNetworkSpaceKey("bringyour.com", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	m1 := map[*NetworkSpace]bool{}
	m1List := networkSpaceManager2.GetNetworkSpaces()
	for i := 0; i < m1List.Len(); i += 1 {
		m1[m1List.Get(i)] = true
	}
	m2 := map[*NetworkSpace]bool{
		networkSpaceManager2.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")):    true,
		networkSpaceManager2.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")): true,
	}
	connect.AssertEqual(t, m1, m2)
	connect.AssertEqual(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace, true)
	networkSpaceManager2.SetActiveNetworkSpace(networkSpace2)
	connect.AssertEqual(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace2, true)
	networkSpaceManager2.Close()

	networkSpaceManager3 := NewNetworkSpaceManager(storagePath)
	connect.AssertEqual(t, networkSpaceManager3.GetNetworkSpaces().Len(), 2)
	networkSpaces3 := []*NetworkSpace{}
	networkSpaces3List := networkSpaceManager3.GetNetworkSpaces()
	for i := 0; i < networkSpaces3List.Len(); i += 1 {
		networkSpaces3 = append(networkSpaces3, networkSpaces3List.Get(i))
	}
	connect.AssertEqual(t, slices.Contains(networkSpaces3, networkSpaceManager3.GetActiveNetworkSpace()), true)

	networkSpace3 := networkSpaceManager3.updateNetworkSpace(
		NewNetworkSpaceKey("ur.io", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	m1 = map[*NetworkSpace]bool{}
	m1List = networkSpaceManager3.GetNetworkSpaces()
	for i := 0; i < m1List.Len(); i += 1 {
		m1[m1List.Get(i)] = true
	}
	m2 = map[*NetworkSpace]bool{
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")):    true,
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")): true,
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")):         true,
	}
	connect.AssertEqual(t, m1, m2)
	networkSpaceManager3.SetActiveNetworkSpace(networkSpace3)
	connect.AssertEqual(t, networkSpaceManager3.GetActiveNetworkSpace() == networkSpace3, true)

	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")))
	connect.AssertEqual(t, networkSpaceManager3.GetNetworkSpaces().Len(), 2)
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")))
	connect.AssertEqual(t, networkSpaceManager3.GetNetworkSpaces().Len(), 1)
	// cannot remove active network space
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")))
	connect.AssertEqual(t, networkSpaceManager3.GetNetworkSpaces().Len(), 1)

	networkSpaceManager3.SetActiveNetworkSpace(nil)
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")))
	connect.AssertEqual(t, networkSpaceManager3.GetNetworkSpaces().Len(), 0)

	networkSpaceManager3.Close()
}

func TestNetworkSpaceUrlResolution(t *testing.T) {
	ctx := context.Background()

	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("ur.network", "main"),
		NetworkSpaceValues{},
		"",
	)
	connect.AssertEqual(t, networkSpace.GetApiUrl(), "https://api.ur.network")
	connect.AssertEqual(t, networkSpace.GetPlatformUrl(), "wss://connect.ur.network")
	networkSpace.close()

	secretNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("example.com", "beta"),
		NetworkSpaceValues{EnvSecret: "secret"},
		"",
	)
	connect.AssertEqual(t, secretNetworkSpace.GetApiUrl(), "https://beta-api.example.com/secret")
	connect.AssertEqual(t, secretNetworkSpace.GetPlatformUrl(), "wss://beta-connect.example.com/secret")
	secretNetworkSpace.close()

	migratedNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("ur.network", "main"),
		NetworkSpaceValues{MigrationHostName: "bringyour.com"},
		"",
	)
	connect.AssertEqual(t, migratedNetworkSpace.GetApiUrl(), "https://api.bringyour.com")
	connect.AssertEqual(t, migratedNetworkSpace.GetPlatformUrl(), "wss://connect.bringyour.com")
	migratedNetworkSpace.close()

	overrideNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("custom.local", "main"),
		NetworkSpaceValues{
			ApiUrl:      "http://api.custom.test:8080/",
			PlatformUrl: "ws://connect.custom.test:5080/",
		},
		"",
	)
	connect.AssertEqual(t, overrideNetworkSpace.GetApiUrl(), "http://api.custom.test:8080")
	connect.AssertEqual(t, overrideNetworkSpace.GetPlatformUrl(), "ws://connect.custom.test:5080")
	connect.AssertEqual(t, overrideNetworkSpace.GetConfiguredApiUrl(), "http://api.custom.test:8080/")
	connect.AssertEqual(t, overrideNetworkSpace.GetConfiguredPlatformUrl(), "ws://connect.custom.test:5080/")
	overrideNetworkSpace.close()
}

func TestNetworkSpaceManagerHostSpecificStoragePath(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_network_space_manager_host_storage")
	connect.AssertEqual(t, err, nil)

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	defer networkSpaceManager.Close()

	firstNetworkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("ur.network", "main"),
		func(values *NetworkSpaceValues) {},
	)
	secondNetworkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("custom.example.com:8443", "main"),
		func(values *NetworkSpaceValues) {},
	)

	connect.AssertEqual(t, firstNetworkSpace.storagePath, filepath.Join(storagePath, "network_spaces", "ur.network", "main"))
	connect.AssertEqual(t, secondNetworkSpace.storagePath, filepath.Join(storagePath, "network_spaces", "custom.example.com_8443", "main"))
	if firstNetworkSpace.storagePath == secondNetworkSpace.storagePath {
		t.Fatalf("expected network spaces with different hosts to use different storage paths")
	}
}

func TestNetworkSpaceManagerMigratesLegacyEnvOnlyStoragePath(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_network_space_manager_legacy_migration")
	connect.AssertEqual(t, err, nil)

	// simulate a pre-existing install that predates host-scoped storage:
	// `network_spaces/<env>` with some local state file already in it.
	legacyEnvStoragePath := filepath.Join(storagePath, "network_spaces", "main")
	connect.AssertEqual(t, os.MkdirAll(legacyEnvStoragePath, LocalStorageFilePermissions), nil)
	legacyMarkerPath := filepath.Join(legacyEnvStoragePath, "legacy_marker")
	connect.AssertEqual(t, os.WriteFile(legacyMarkerPath, []byte("legacy state"), LocalStorageFilePermissions), nil)

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	defer networkSpaceManager.Close()

	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("ur.network", "main"),
		func(values *NetworkSpaceValues) {},
	)

	expectedStoragePath := filepath.Join(storagePath, "network_spaces", "ur.network", "main")
	connect.AssertEqual(t, networkSpace.storagePath, expectedStoragePath)

	migratedMarkerPath := filepath.Join(expectedStoragePath, "legacy_marker")
	migratedContents, err := os.ReadFile(migratedMarkerPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(migratedContents), "legacy state")

	if _, err := os.Stat(legacyEnvStoragePath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy env-only storage path to be moved, not left behind")
	}
}
