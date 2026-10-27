// The real device refresh callback supplies native startup's settlement event.
// A held publication proves immediate retries alone cannot create progress;
// no clock delay or fabricated auth snapshot is used as the discriminator.
package sdk

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceLocalPreferenceRefreshEventAdmitsPreviouslyUnsettledLoad(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal("could not seed the owned specific destination")
	}
	device := testingPreferenceDevice(t, fixture)
	refreshed := testingRefreshableJwtWithMarker(t, "preference-settlement")
	settled := make(chan string, 1)
	sub := device.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		settled <- jwt
	}))
	defer sub.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	// Only the first admission is held: it is the actual refresh callback.
	// Load has its own leases, which must remain free to reject unsettled auth.
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
		t.Fatal("the real refresh was not held between API and durable publication")
	}
	select {
	case <-settled:
		t.Fatal("device advertised settlement before its held durable publication")
	default:
	}

	// This is the native helper's old three-immediate-attempt failure shape.
	// All observations occur before the explicitly held publisher can proceed.
	for attempt := 0; attempt < 3; attempt += 1 {
		result, err := device.Load()
		if result != nil || err == nil || err.Error() != localAuthSnapshotSupersededMessage {
			t.Fatal("an unsettled owner admitted preference restoration")
		}
		if device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
			t.Fatal("an unsettled Load partially installed a consumer")
		}
	}
	if err := device.SetAutoSave(true); err == nil || err.Error() != localAuthSnapshotSupersededMessage || device.GetAutoSave() {
		t.Fatal("unsettled startup enabled preference persistence")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) {
		t.Fatal("rejected startup discarded the saved destination")
	}
	if fixture.localState.GetByJwt() != fixture.adminJwt {
		t.Fatal("rejected startup replaced the separate admin credential")
	}

	resume()
	select {
	case jwt := <-settled:
		if jwt != refreshed || fixture.localState.GetByClientJwt() != refreshed || device.GetClientJwt() != refreshed {
			t.Fatal("refresh event preceded accepted durable client publication")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed refresh did not wake its subscribed startup owner")
	}
	select {
	case accepted := <-refreshDone:
		if !accepted {
			t.Fatal("the held current-owner refresh was rejected")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted refresh callback did not finish")
	}
	result, err := device.Load()
	if err != nil || result == nil || !result.GetHasConnectLocation() ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) == nil {
		t.Fatal("settlement did not admit exact saved consumer restoration without app RPC")
	}
	if err := device.SetAutoSave(true); err != nil || !device.GetAutoSave() {
		t.Fatal("settled startup could not enable explicit autosave")
	}
	if fixture.localState.GetByJwt() != fixture.adminJwt {
		t.Fatal("same-owner settlement changed the separate admin credential")
	}
}
