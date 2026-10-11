package sdk

// The host-network dns fallback ("fast DNS on connect") is opt-in: the default device resolves
// only through the tunnel, a record written by an older sdk (where the fallback was the default)
// loads with the fallback off, and an explicit opt-in written by this sdk survives a reload.
// The tests drive the real LocalState files and DeviceLocal load path, with no network.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDnsResolverSettingsResolveOnlyThroughTunnel(t *testing.T) {
	dnsResolverSettings := GetDefaultDnsResolverSettings()
	if dnsResolverSettings == nil || !dnsResolverSettings.EnableRemoteDoh {
		t.Fatalf("unexpected default dns resolver settings %+v", dnsResolverSettings)
	}
	if dnsResolverSettings.EnableFallback {
		t.Fatal("default dns resolver settings enable the host-network fallback; dns must resolve only through the tunnel")
	}
	if dnsResolverSettings.EnableLocalDoh || dnsResolverSettings.EnableLocalDns {
		t.Fatalf("default dns resolver settings dial the host network %+v", dnsResolverSettings)
	}
}

func TestDeviceLocalDefaultDnsHasNoHostFallback(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)

	if dnsResolverSettings := device.GetDnsResolverSettings(); dnsResolverSettings.EnableFallback {
		t.Fatalf("fresh device reports the host-network fallback on %+v", dnsResolverSettings)
	}
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		if fallback := device.upgradeMuxSettings.Dns.Fallback; fallback != nil {
			t.Fatalf("fresh device installs a host-network fallback resolver %+v", fallback)
		}
	}()
}

// A record exactly as an older sdk wrote it: the whole settings object, fallback on, no version.
func testingWriteLegacyDnsResolverSettings(t *testing.T, localState *LocalState) {
	t.Helper()
	legacy := GetDefaultDnsResolverSettings()
	legacy.EnableFallback = true
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localState.localStorageDir, ".dns_resolver_settings"), data, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyDnsResolverSettingsRecordLoadsWithFallbackOff(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	localState := fixture.networkSpace.GetAsyncLocalState().GetLocalState()
	testingWriteLegacyDnsResolverSettings(t, localState)

	if persisted := localState.GetDnsResolverSettings(); persisted == nil || persisted.EnableFallback {
		t.Fatalf("legacy record read with the host-network fallback on %+v", persisted)
	}

	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	if dnsResolverSettings := device.GetDnsResolverSettings(); dnsResolverSettings.EnableFallback {
		t.Fatalf("legacy record loaded with the host-network fallback on %+v", dnsResolverSettings)
	}
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		if fallback := device.upgradeMuxSettings.Dns.Fallback; fallback != nil {
			t.Fatalf("legacy record installed a host-network fallback resolver %+v", fallback)
		}
	}()
}

func TestExplicitDnsFallbackOptInSurvivesReload(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)

	optIn := GetDefaultDnsResolverSettings()
	optIn.EnableFallback = true
	device.SetDnsResolverSettings(optIn)
	testingJoinPreferenceDevice(t, device)

	device2 := testing_loadedBlockDevice(t, fixture.networkSpace)
	if dnsResolverSettings := device2.GetDnsResolverSettings(); !dnsResolverSettings.EnableFallback {
		t.Fatalf("explicit fallback opt-in was lost on reload %+v", dnsResolverSettings)
	}
	func() {
		device2.stateLock.Lock()
		defer device2.stateLock.Unlock()
		if device2.upgradeMuxSettings.Dns.Fallback == nil {
			t.Fatal("explicit fallback opt-in did not install the host-network fallback resolver")
		}
	}()
}

func TestLocalStateDnsResolverSettingsKeepExplicitFallback(t *testing.T) {
	localState := &LocalState{localStorageDir: t.TempDir()}
	for _, enableFallback := range []bool{true, false} {
		dnsResolverSettings := GetDefaultDnsResolverSettings()
		dnsResolverSettings.EnableFallback = enableFallback
		if err := localState.SetDnsResolverSettings(dnsResolverSettings); err != nil {
			t.Fatal(err)
		}
		if persisted := localState.GetDnsResolverSettings(); persisted == nil || persisted.EnableFallback != enableFallback {
			t.Fatalf("persisted fallback = %+v, want %t", persisted, enableFallback)
		}
	}
}
