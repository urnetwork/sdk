// Post-start destination changes must wait for the real device publication
// event without disabling autosave or treating a rejected read as disconnect.
// These are SDK boundary controls, not a native wake scheduler simulation.
package sdk

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Holds only the constructor-installed refresh callback after the API update;
// destination mutations take their own live leases and can reject promptly.
func testingPostStartupPreferenceSettlement(t *testing.T, reconnect bool) {
	t.Helper()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal("settled device could not enable explicit autosave")
	}
	original := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(original); err != nil {
		t.Fatal("initial current destination could not be committed")
	}
	originalConsumer := testingPreferenceConsumer(device)
	if originalConsumer == nil || !device.GetConnectEnabled() {
		t.Fatal("initial destination did not create a real consumer")
	}
	locationPath := filepath.Join(fixture.localState.localStorageDir, localConnectLocationFileName)
	originalBytes, err := os.ReadFile(locationPath)
	if err != nil {
		t.Fatal("initial current destination was not durable")
	}
	target := testingSpecificPreferenceLocation()
	mutate := device.SetConnectLocationChecked
	if reconnect {
		target = cloneConnectLocation(original)
		mutate = device.ReconnectChecked
	}

	refreshed := testingRefreshableJwtWithMarker(t, "poststart-preference-settlement")
	settled := make(chan string, 1)
	sub := device.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		settled <- jwt
	}))
	t.Cleanup(sub.Close)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var admissions atomic.Int64
	device.authPublication.testingAfterAdmission = func() {
		if admissions.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	refreshDone := make(chan bool, 1)
	go func() {
		refreshDone <- fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshed)
	}()
	testingAwaitAuthBoundary(t, entered)
	if fixture.api.GetByJwt() != refreshed || fixture.localState.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("actual callback was not held between API and durable publication")
	}

	// Exhaust the old immediate-recapture count while publication is explicitly
	// held. No sleep, negative deadline or fabricated error determines ordering.
	for attempt := 0; attempt < 3; attempt += 1 {
		if err := mutate(target); err == nil || err.Error() != localAuthSnapshotSupersededMessage {
			t.Fatal("unsettled destination mutation lost its retry discriminator")
		}
		result := device.GetLastLocalStateSaveResult()
		if result == nil || result.GetSaved() || !result.GetAutoSaveEnabled() ||
			result.GetError() != localAuthSnapshotSupersededMessage {
			t.Fatal("rejected mutation changed or misreported active persistence")
		}
		if !device.GetAutoSave() || !connectLocationValuesEqual(device.GetConnectLocation(), original) ||
			testingPreferenceConsumer(device) != originalConsumer {
			t.Fatal("unsettled mutation changed the current route or consumer")
		}
		if data, err := os.ReadFile(locationPath); err != nil || !bytes.Equal(data, originalBytes) {
			t.Fatal("unsettled mutation changed the last committed destination")
		}
	}
	select {
	case <-settled:
		t.Fatal("device advertised settlement before its durable publication")
	default:
	}
	resume()
	select {
	case jwt := <-settled:
		if jwt != refreshed || fixture.localState.GetByClientJwt() != refreshed || device.GetClientJwt() != refreshed {
			t.Fatal("actual event preceded the accepted durable client publication")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("successful publication did not emit the subscribed progress event")
	}
	// This call consumes the real publication event, without another app/RPC or
	// path event. Native queue ownership and retry scheduling need separate tests.
	if err := mutate(target); err != nil {
		t.Fatal("settled post-start destination mutation did not recover")
	}
	if result := device.GetLastLocalStateSaveResult(); result == nil || !result.GetSaved() || result.GetError() != "" {
		t.Fatal("resumed destination mutation did not report its committed state")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || !device.GetConnectEnabled() {
		t.Fatal("publication did not admit the exact current destination and consumer")
	}
	consumer := testingPreferenceConsumer(device)
	if consumer == nil || consumer == originalConsumer {
		t.Fatal("accepted new destination or explicit reconnect did not replace the real consumer")
	}
	if err := device.SetConnectLocationChecked(cloneConnectLocation(target)); err != nil ||
		testingPreferenceConsumer(device) != consumer {
		t.Fatal("healthy equal replay rebuilt the recovered consumer")
	}
	if fixture.localState.GetByJwt() != fixture.adminJwt || !device.GetAutoSave() {
		t.Fatal("post-start recovery changed admin authority or disabled autosave")
	}
	select {
	case accepted := <-refreshDone:
		if !accepted {
			t.Fatal("the actual current-owner publication was rejected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the accepted publication callback did not finish")
	}
}

// A new intended destination can be retried on settlement after normal startup.
func TestDeviceLocalPostStartSetResumesAfterActualAuthPublication(t *testing.T) {
	testingPostStartupPreferenceSettlement(t, false)
}

// Explicit reconnect keeps its rebuild contract without losing saved intent.
func TestDeviceLocalPostStartReconnectResumesAfterActualAuthPublication(t *testing.T) {
	testingPostStartupPreferenceSettlement(t, true)
}
