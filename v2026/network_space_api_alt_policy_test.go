package sdk

// The explicit manager policy is applied before every strategy construction;
// neither persisted values nor another owner's api, dns or peer policy changes.

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// Enabled and disabled managers coexist, and the opt-out affects api alt only.
func TestNetworkSpaceApiAltPolicyIsOwnerScoped(t *testing.T) {
	off := NewNetworkSpaceManagerWithApiAlt("", false)
	defer off.Close()
	on := NewNetworkSpaceManagerNoStorage()
	defer on.Close()
	key := NewNetworkSpaceKey("space.example", "main")
	values := &NetworkSpaceValues{AltUrl: "https://alt.other.example:1443"}
	beforeTransport := DefaultTransportSettings()
	offSpace := off.UpdateNetworkSpaceValues(key, values)
	onSpace := on.UpdateNetworkSpaceValues(key, values)
	if offSpace.GetApiAltEnabled() || offSpace.clientStrategySettings.AltUrl != "" {
		t.Fatal("disabled manager installed api alt dialers")
	}
	if !onSpace.GetApiAltEnabled() || onSpace.clientStrategySettings.AltUrl != values.AltUrl {
		t.Fatal("one manager changed an independent owner's enabled default")
	}
	if offSpace.GetAltUrl() != values.AltUrl || offSpace.GetPlatformUrl() != onSpace.GetPlatformUrl() ||
		offSpace.GetApiUrl() != onSpace.GetApiUrl() || !reflect.DeepEqual(offSpace.valuesCopy(), *values) {
		t.Fatal("api policy changed stored values or platform endpoints")
	}
	if offSpace.clientStrategy.ConnectSettings().Resolver != testOfflineResolver ||
		!slices.Equal(offSpace.clientStrategySettings.InternalDohDomains, onSpace.clientStrategySettings.InternalDohDomains) ||
		!slices.Equal(offSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4,
			onSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4) ||
		!slices.Equal(offSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv6,
			onSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv6) {
		t.Fatal("api-alt opt-out changed ordinary or protected-domain dns")
	}
	if !reflect.DeepEqual(beforeTransport, DefaultTransportSettings()) {
		t.Fatal("api-alt opt-out changed default peer or platform transport policy")
	}
}

// Loading constructs the first persisted strategy under the explicit choice;
// updating and a later default launch cannot inherit a persisted exclusion.
func TestNetworkSpaceApiAltPolicyRestoresBeforeFirstSpace(t *testing.T) {
	storagePath := t.TempDir()
	key := NewNetworkSpaceKey("restored.example", "main")
	seed := NewNetworkSpaceManager(storagePath)
	space := seed.UpdateNetworkSpaceValues(key, &NetworkSpaceValues{AltUrl: "https://alt.restored.example"})
	seed.SetActiveNetworkSpace(space)
	seed.Close()
	before, err := os.ReadFile(filepath.Join(storagePath, ".network_spaces"))
	if err != nil {
		t.Fatal(err)
	}
	off := NewNetworkSpaceManagerWithApiAlt(storagePath, false)
	defer off.Close()
	restored := off.GetActiveNetworkSpace()
	if restored == nil || restored.GetApiAltEnabled() || restored.clientStrategySettings.AltUrl != "" {
		t.Fatal("the first restored strategy preceded its manager policy")
	}
	after, err := os.ReadFile(filepath.Join(storagePath, ".network_spaces"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("constructor persisted its launch-only choice: read error=%v", err)
	}
	replacement := off.UpdateNetworkSpaceValues(key, nil)
	if replacement == restored || restored.ctx.Err() == nil || replacement.GetApiAltEnabled() {
		t.Fatal("replacement did not retain the manager's immutable policy")
	}
	off.Close()
	on := NewNetworkSpaceManager(storagePath)
	defer on.Close()
	if current := on.GetActiveNetworkSpace(); current == nil || !current.GetApiAltEnabled() || current.GetAltUrl() != "https://alt.restored.example" {
		t.Fatal("a later ordinary launch inherited a diagnostic exclusion")
	}
}

// A real manager migration closes the old strategy and applies the same policy
// before publishing its replacement under a different key.
func TestNetworkSpaceApiAltPolicySurvivesMigration(t *testing.T) {
	manager := NewNetworkSpaceManagerWithApiAlt(t.TempDir(), false)
	defer manager.Close()
	fromKey := NewNetworkSpaceKey("old-space.example", "main")
	toKey := NewNetworkSpaceKey("new-space.example", "main")
	previous := manager.UpdateNetworkSpaceValues(fromKey, &NetworkSpaceValues{MigrationHostName: toKey.HostName})
	manager.SetActiveNetworkSpace(previous)
	if !manager.MigrateNetworkSpace(fromKey, toKey) {
		t.Fatal("real storage migration failed")
	}
	current := manager.GetActiveNetworkSpace()
	if current == nil || current == previous || previous.ctx.Err() == nil ||
		manager.GetNetworkSpace(fromKey) != nil || current != manager.GetNetworkSpace(toKey) ||
		current.GetApiAltEnabled() || current.GetAltUrl() != "wss://alt.new-space.example" {
		t.Fatal("migration lost source ownership or api-alt policy")
	}
}

// Exercise the exact reopen boundary used after a failed migration, without
// relying on a filesystem race or permission failure to reach that boundary.
func TestNetworkSpaceApiAltPolicySurvivesFailedMigrationReopen(t *testing.T) {
	manager := NewNetworkSpaceManagerWithApiAlt("", false)
	defer manager.Close()
	key := NewNetworkSpaceKey("reopen.example", "main")
	previous := manager.UpdateNetworkSpaceValues(key, &NetworkSpaceValues{})
	manager.SetActiveNetworkSpace(previous)
	previous.close()
	manager.restoreNetworkSpace(key, previous, true)
	current := manager.GetActiveNetworkSpace()
	if current == nil || current == previous || previous.ctx.Err() == nil ||
		current != manager.GetNetworkSpace(key) || current.GetApiAltEnabled() ||
		current.GetAltUrl() != "wss://alt.reopen.example" {
		t.Fatal("failed-migration reopen lost its manager policy")
	}
}
