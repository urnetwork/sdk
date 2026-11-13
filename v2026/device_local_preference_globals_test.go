// Process-global preference effects need their own final owner admission.
// These controls hold the real apply seam and observe the actual shared value.
package sdk

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testingGlobalPreference struct {
	name     string
	oldValue int
	newValue int
	set      func(*DeviceLocal, int)
	get      func() int
	seed     func(*LocalState, int) error
	stored   func(*LocalState) int
}

func testingGlobalPreferences() []testingGlobalPreference {
	return []testingGlobalPreference{
		{name: "control-ip-family-policy", oldValue: IpFamilyPolicyForce4, newValue: IpFamilyPolicyForce6,
			set: func(d *DeviceLocal, value int) { d.SetControlIpFamilyPolicy(value) }, get: GetControlIpFamilyPolicy,
			seed: func(s *LocalState, value int) error { return s.SetControlIpFamilyPolicy(value) }, stored: func(s *LocalState) int { return s.GetControlIpFamilyPolicy() }},
		{name: "log-verbosity", oldValue: LogVerbosityTrace, newValue: LogVerbosityDefault,
			set: func(d *DeviceLocal, value int) { d.SetLogVerbosity(value) }, get: GetLogVerbosity,
			seed: func(s *LocalState, value int) error { return s.SetLogVerbosity(value) }, stored: func(s *LocalState) int { return s.GetLogVerbosity() }},
	}
}

// Both callers exercise actual SDK entry points; the assertion is the process
// global after reset/new-owner publication, never a helper's ownership boolean.
func testingRetiredGlobalPreference(t *testing.T, preference testingGlobalPreference, load bool) {
	t.Helper()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	if load {
		if err := preference.seed(fixture.localState, preference.oldValue); err != nil {
			t.Fatal(err)
		}
	}
	oldDevice := testingPreferenceDevice(t, fixture)
	if !load {
		if err := oldDevice.SetAutoSave(true); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	oldDevice.testingBeforeGlobalPreferenceApply = func(name string) {
		if name == preference.name {
			close(entered)
			<-release
		}
	}
	completed := make(chan error, 1)
	go func() {
		if load {
			result, err := oldDevice.Load()
			if result != nil {
				completed <- errors.New("retired Load reported success")
			} else {
				completed <- err
			}
		} else {
			completed <- oldDevice.setLocalCatalogPreference(preference.name, preference.oldValue)
		}
	}()
	testingAwaitAuthBoundary(t, entered)
	snapshot, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || reset == nil || !reset.GetReset() {
		t.Fatal("real reset could not pass the held old scalar application")
	}
	fixture.seedDistinctLogin(t)
	current := testingPreferenceDevice(t, fixture)
	if err := current.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	preference.set(current, preference.newValue)
	if preference.get() != preference.newValue {
		t.Fatal("new owner did not publish the actual process preference")
	}
	resume()
	var completedErr error
	select {
	case err := <-completed:
		completedErr = err
	case <-time.After(5 * time.Second):
		t.Fatal("held old scalar application did not finish")
	}
	if preference.get() != preference.newValue || preference.stored(fixture.localState) != preference.newValue {
		t.Fatal("retired application overwrote the newer process-global preference")
	}
	if completedErr == nil {
		t.Fatal("retired operation completed shared preference publication")
	}
	if load && completedErr.Error() != localAuthSnapshotSupersededMessage {
		t.Fatal("retired Load lost its exact supersession discriminator")
	}
	if !load {
		result := oldDevice.GetLastLocalStateSaveResult()
		if result == nil || !result.GetSaved() || result.GetError() != "apply "+preference.name {
			t.Fatal("retired partial operation lost its prior commit versus failed apply result")
		}
	}
}

func TestDeviceLocalPreferenceCatalogRetiredLoadCannotChangeProcessGlobals(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	for _, preference := range testingGlobalPreferences() {
		testingRetiredGlobalPreference(t, preference, true)
	}
}

func TestDeviceLocalPreferenceCatalogRetiredMutationCannotChangeProcessGlobals(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	for _, preference := range testingGlobalPreferences() {
		testingRetiredGlobalPreference(t, preference, false)
	}
}

func TestDeviceLocalPreferenceCatalogGlobalModeOffAndSameOwnerRenewal(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	for _, preference := range testingGlobalPreferences() {
		preference.set(device, preference.oldValue)
		if preference.get() != preference.oldValue {
			t.Fatal("mode-off explicit global mutation stopped applying")
		}
		file, _ := localPreferenceFile(preference.name)
		if _, err := os.Stat(filepath.Join(fixture.localState.localStorageDir, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("mode-off global mutation implicitly persisted")
		}
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	refreshed := testingRefreshableJwtWithMarker(t, "global-renewal")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshed) {
		t.Fatal("same-owner actual refresh failed")
	}
	for _, preference := range testingGlobalPreferences() {
		preference.set(device, preference.newValue)
		result := device.GetLastLocalStateSaveResult()
		if result == nil || !result.GetSaved() || result.GetError() != "" || preference.get() != preference.newValue || preference.stored(fixture.localState) != preference.newValue {
			t.Fatal("same-owner renewal blocked a healthy owned global mutation")
		}
	}
	if fixture.localState.GetByJwt() != fixture.adminJwt {
		t.Fatal("global preference mutation replaced separate admin auth")
	}
}
