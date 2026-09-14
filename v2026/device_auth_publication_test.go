package sdk

import (
	"context"
	"testing"
)

func TestDeviceAuthPublicationGateRejectsLateAndJoinsAdmittedCallback(t *testing.T) {
	gate := newDeviceAuthPublicationGate()
	release := gate.Begin()
	if release == nil {
		t.Fatal("live auth publication was rejected")
	}
	gate.Close()
	if lateRelease := gate.Begin(); lateRelease != nil {
		lateRelease()
		t.Fatal("retired auth publication admitted a late callback")
	}
	select {
	case <-gate.Done():
		t.Fatal("retired gate completed while its callback was still admitted")
	default:
	}

	release()
	select {
	case <-gate.Done():
	default:
		t.Fatal("retired gate did not complete after its callback released")
	}
}
func TestParkedAuthCallbackAfterLogoutCannotRecreateState(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	initialJwt := testingRefreshableJwtWithMarker(t, "parked-callback-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "parked-callback-refresh")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		t.Fatal("initial instance_id is nil")
	}

	gate := newDeviceAuthPublicationGate()
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	callbackDone := make(chan struct{})
	accepted := false
	var callbackErr error
	go func() {
		defer close(callbackDone)
		release := gate.Begin()
		if release == nil {
			return
		}
		defer release()
		close(callbackStarted)
		<-callbackRelease
		accepted, callbackErr = localState.replaceRefreshedByJwt(
			initialJwt,
			refreshedJwt,
			instanceId,
		)
	}()
	<-callbackStarted
	gate.Close()
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	close(callbackRelease)
	<-callbackDone
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	if accepted {
		t.Fatal("parked retired callback was accepted after logout")
	}
	select {
	case <-gate.Done():
	default:
		t.Fatal("auth publication join did not observe callback completion")
	}
	state, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != "" || state.ByClientJwt != "" || state.InstanceId != "" {
		t.Fatalf("parked callback recreated auth after logout: %+v", state)
	}
}
