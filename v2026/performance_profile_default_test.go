package sdk

// Post-quantum encryption is on by default (owner, 2026-11-24). These pin it
// for a new install (the app's saved profile and the device), the one-time
// migration of a profile an older sdk saved, a choice saved after the update,
// the default a caller gets by setting no profile, a hosted device that never
// runs it, and the location list a post-quantum device asks for.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// What an sdk before the default wrote after the user changed some other
// connect option, the post-quantum toggle untouched: Fixed IP in Web mode,
// with direct mode on.
const testingLegacyPerformanceProfileRecord = `{"window_type":"quality","window_size":{"window_size_min":1,"window_size_min_p2p_only":0,"window_size_max":1,"window_size_hard_max":0,"window_size_reconnect_scale":0,"keep_healthiest_count":0,"ulimit":0},"allow_direct":true,"post_quantum_encryption":false}`

// What an sdk before the post-quantum field wrote.
const testingPreFieldPerformanceProfileRecord = `{"window_type":"speed","window_size":null,"allow_direct":false}`

// Fails unless the profile is set and its post-quantum setting is want.
func assertPostQuantumEncryption(t *testing.T, performanceProfile *PerformanceProfile, want bool, description string) {
	t.Helper()
	if performanceProfile == nil {
		t.Fatalf("%s: no profile, want post-quantum encryption %t", description, want)
	}
	if performanceProfile.PostQuantumEncryption != want {
		t.Fatalf("%s: post-quantum encryption %t, want %t", description, performanceProfile.PostQuantumEncryption, want)
	}
}

// Fails unless the record at path is stamped with the post-quantum default
// version, so the choice in it is kept. Read as the bytes on disk, the format
// an older or a newer sdk reads.
func assertVersionedPerformanceProfileRecord(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if version := string(fields["PostQuantumDefaultVersion"]); version != "1" {
		t.Fatalf("the saved record has post-quantum default version %q, want 1: %s", version, data)
	}
}

// The default profile: auto, post-quantum on.
func testingDefaultPerformanceProfile() *PerformanceProfile {
	return &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
}

// A new install reads the default profile from the app's saved state: auto,
// with post-quantum encryption on. Before, it read no profile, which every app
// showed and applied as post-quantum off.
func TestNewInstallLocalStateDefaultsPostQuantumOn(t *testing.T) {
	localState := newLocalState(t.Context(), t.TempDir())
	performanceProfile := localState.GetPerformanceProfile()
	assertPostQuantumEncryption(t, performanceProfile, true, "a new install's saved profile")
	if performanceProfile.WindowType != WindowTypeAuto || performanceProfile.WindowSize != nil || performanceProfile.AllowDirect {
		t.Fatalf("a new install's profile is not auto: %+v", performanceProfile)
	}
}

// A new device runs post-quantum encryption before anything is loaded or set,
// as an extension or a service started before its app does, and a load with
// nothing saved keeps it. The multi client it builds takes it.
func TestNewInstallDeviceDefaultsPostQuantumOn(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), true, "a new device")
	if _, err := device.Load(); err != nil {
		t.Fatal(err)
	}
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), true, "a new device after a load with nothing saved")
	if connectProfile := toConnectPerformanceProfile(device.GetPerformanceProfile()); connectProfile == nil || !connectProfile.PostQuantumEncryption {
		t.Fatal("the multi client of a new device does not run post-quantum encryption")
	}
}

// A user who turns post-quantum encryption off after the update keeps it off,
// in the app's saved profile and in the device's, which autosave writes and
// the next load reads.
func TestExplicitPostQuantumOffIsKept(t *testing.T) {
	off := &PerformanceProfile{WindowType: WindowTypeAuto}

	directory := t.TempDir()
	localState := newLocalState(t.Context(), directory)
	assertPostQuantumEncryption(t, localState.GetPerformanceProfile(), true, "a new install")
	if err := localState.SetPerformanceProfile(off); err != nil {
		t.Fatal(err)
	}
	assertVersionedPerformanceProfileRecord(t, filepath.Join(localState.localStorageDir, ".performance_profile"))
	for range 2 {
		reopened := newLocalState(t.Context(), directory)
		assertPostQuantumEncryption(t, reopened.GetPerformanceProfile(), false, "the app's saved profile after the user turned it off")
	}

	deviceDirectory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, deviceDirectory)
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), true, "a new device")
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetPerformanceProfile(off)
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), false, "the device after the user turned it off")
	assertVersionedPerformanceProfileRecord(t, filepath.Join(fixture.localState.localStorageDir, ".performance_profile"))
	testingJoinPreferenceDevice(t, device)
	manager.Close()

	_, fresh := testingPreferenceSpaceAt(t, deviceDirectory)
	restored := testingPreferenceDevice(t, fresh)
	if _, err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	assertPostQuantumEncryption(t, restored.GetPerformanceProfile(), false, "the device's saved profile after the user turned it off")
}

// A profile an older sdk saved, with post-quantum off because that was the
// default, or with no post-quantum field at all, loads with it on and keeps
// every other setting. Reading changes nothing on disk. Once a save stamps
// the version the migration is over: a later choice of off reads back off,
// every time.
func TestLegacyPerformanceProfileRecordMigratesPostQuantumOnOnce(t *testing.T) {
	for _, legacy := range []string{testingLegacyPerformanceProfileRecord, testingPreFieldPerformanceProfileRecord} {
		directory := t.TempDir()
		path := filepath.Join(newLocalState(t.Context(), directory).localStorageDir, ".performance_profile")
		if err := os.WriteFile(path, []byte(legacy), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		var saved PerformanceProfile
		if err := json.Unmarshal([]byte(legacy), &saved); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			migrated := newLocalState(t.Context(), directory).GetPerformanceProfile()
			assertPostQuantumEncryption(t, migrated, true, "an older sdk's saved profile")
			saved.PostQuantumEncryption = true
			if !performanceProfileValuesEqual(migrated, &saved) {
				t.Fatalf("the migration changed another setting: %+v, want %+v", migrated, &saved)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, []byte(legacy)) {
				t.Fatal("reading an older sdk's saved profile rewrote it")
			}
		}

		localState := newLocalState(t.Context(), directory)
		saved.PostQuantumEncryption = false
		if err := localState.SetPerformanceProfile(&saved); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			assertPostQuantumEncryption(t, newLocalState(t.Context(), directory).GetPerformanceProfile(), false, "a choice of off saved after the migration")
		}
	}

	// the device's saved profile migrates the same way on load
	deviceDirectory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, deviceDirectory)
	fixture.seedDistinctLogin(t)
	devicePath := filepath.Join(fixture.localState.localStorageDir, ".performance_profile")
	if err := os.WriteFile(devicePath, []byte(testingLegacyPerformanceProfileRecord), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	device := testingPreferenceDevice(t, fixture)
	if _, err := device.Load(); err != nil {
		t.Fatal(err)
	}
	migrated := device.GetPerformanceProfile()
	assertPostQuantumEncryption(t, migrated, true, "the device's profile from an older sdk")
	if migrated.WindowType != WindowTypeQuality || !migrated.AllowDirect {
		t.Fatalf("the device's migration changed another setting: %+v", migrated)
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	off := clonePerformanceProfile(migrated)
	off.PostQuantumEncryption = false
	device.SetPerformanceProfile(off)
	testingJoinPreferenceDevice(t, device)
	manager.Close()

	_, fresh := testingPreferenceSpaceAt(t, deviceDirectory)
	restored := testingPreferenceDevice(t, fresh)
	if _, err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	assertPostQuantumEncryption(t, restored.GetPerformanceProfile(), false, "the device's choice of off saved after the migration")
}

// An app that sets no profile, meaning the default, gets the default profile,
// post-quantum on, instead of a device without it. Before, no profile ran
// auto without post-quantum encryption.
func TestDeviceSetNoPerformanceProfileRestoresDefault(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeSpeed})
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), false, "an explicit profile without post-quantum encryption")
	device.SetPerformanceProfile(nil)
	performanceProfile := device.GetPerformanceProfile()
	assertPostQuantumEncryption(t, performanceProfile, true, "no profile")
	if !performanceProfileValuesEqual(performanceProfile, testingDefaultPerformanceProfile()) {
		t.Fatalf("no profile set %+v, want the default profile", performanceProfile)
	}
}

// A hosted device (the proxy's, which serves the web app and the extension)
// never runs post-quantum encryption: it starts with no profile, setting no
// profile keeps none, and a post-quantum profile is limited on the way in
// (owner, 2026-11-19).
func TestHostedDeviceKeepsPostQuantumOff(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = 32 * 1024 * 1024
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, fixture, settings, false)
	if performanceProfile := device.GetPerformanceProfile(); performanceProfile != nil && performanceProfile.PostQuantumEncryption {
		t.Fatal("a new hosted device runs post-quantum encryption")
	}
	device.SetPerformanceProfile(nil)
	if performanceProfile := device.GetPerformanceProfile(); performanceProfile != nil && performanceProfile.PostQuantumEncryption {
		t.Fatal("no profile on a hosted device ran post-quantum encryption")
	}
	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeQuality, AllowDirect: true, PostQuantumEncryption: true})
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), false, "a hosted device given a post-quantum profile")
}

// The requests a locations view controller makes, read as the platform
// receives them, with an empty answer.
type testingLocationsRequests struct {
	stateLock sync.Mutex
	requests  []string
	request   chan struct{}
}

func (self *testingLocationsRequests) record(description string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, description)
	}()
	select {
	case self.request <- struct{}{}:
	default:
	}
}

// Waits for the next request and returns it.
func (self *testingLocationsRequests) next(t *testing.T, ctx context.Context) string {
	t.Helper()
	for {
		self.stateLock.Lock()
		if 0 < len(self.requests) {
			request := self.requests[0]
			self.requests = self.requests[1:]
			self.stateLock.Unlock()
			return request
		}
		self.stateLock.Unlock()
		select {
		case <-self.request:
		case <-ctx.Done():
			t.Fatal("the locations view controller made no request")
		}
	}
}

// A device with post-quantum encryption on, the default, lists only the
// locations with post-quantum supply: its initial browse and its searches go
// to find-provider-locations with require_modern_provider, as the platform
// receives them. Turning post-quantum off asks again with the latest filter,
// for every location, and turning it on asks again for the post-quantum list.
// Before, a post-quantum device listed every location, the ones it could
// never connect to included.
func TestLocationsViewControllerListsPostQuantumLocations(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	requests := &testingLocationsRequests{request: make(chan struct{}, 1)}
	empty := []byte(`{"groups":[],"locations":[],"devices":[]}`)
	api := device.GetApi()
	api.setHttpGetRaw(func(_ context.Context, requestUrl string, _ string) ([]byte, error) {
		requests.record("GET " + filepath.Base(requestUrl))
		return empty, nil
	})
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, body []byte, _ string) ([]byte, error) {
		fields := map[string]any{}
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		requireModernProvider, _ := fields["require_modern_provider"].(bool)
		query, _ := fields["query"].(string)
		if requireModernProvider {
			requests.record("POST " + filepath.Base(requestUrl) + " post-quantum " + query)
		} else {
			requests.record("POST " + filepath.Base(requestUrl) + " " + query)
		}
		return empty, nil
	})

	vc := device.OpenLocationsViewController()
	defer device.CloseLocationsViewController(vc)
	vc.Start()
	want := func(description string) {
		t.Helper()
		if got := requests.next(t, ctx); got != description {
			t.Fatalf("the controller asked %q, want %q", got, description)
		}
	}
	want("POST find-provider-locations post-quantum ")
	vc.FilterLocations("sample")
	want("POST find-provider-locations post-quantum sample")

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	want("POST find-provider-locations sample")
	vc.FilterLocations("")
	want("GET provider-locations")

	device.SetPerformanceProfile(testingDefaultPerformanceProfile())
	want("POST find-provider-locations post-quantum ")
}
