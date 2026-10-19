// Paired reset must retire the admin credential at the same boundary as the
// primary API token. Public requests and real session actions prove the effect.
package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// The request seam records authorization without any socket or credential log.
// A checked reset's contract includes a live API holding its saved admin token.
func TestPairedResetClearsNetworkCredentialBeforeAdminRequest(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	var sends atomic.Int32
	fixture.api.setHttpGetRaw(func(_ context.Context, _ string, byJwt string) ([]byte, error) {
		sends.Add(1)
		if byJwt != fixture.adminJwt {
			t.Error("admin request selected an unexpected credential")
		}
		return []byte(`{}`), nil
	})
	request := func() error {
		return awaitApiCall(func(callback connect.ApiCallback[*NetworkClientsResult]) {
			fixture.api.GetNetworkClients(callback)
		})
	}
	if err := request(); err != nil || sends.Load() != 1 {
		t.Fatal("healthy admin request did not reach the isolated transport")
	}
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("current paired auth did not reset")
	}
	if err := request(); !errors.Is(err, ErrNetworkCredentialRequired) || sends.Load() != 1 {
		t.Error("accepted reset still sent an admin request with the old sign-in")
	}
	if fixture.api.GetByJwt() != "" || fixture.api.HasNetworkCredential() ||
		!testingPairedAuthSnapshot(t, fixture).GetEmpty() {
		t.Error("accepted reset retained part of the current sign-in")
	}
}

// Hold an actual controller action before submission. Reset must invalidate
// its captured network generation even though no replacement login follows.
func TestPairedResetSupersedesCapturedSessionAction(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	var sends atomic.Int32
	fixture.api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		sends.Add(1)
		return nil, errors.New("unexpected retired session operation")
	})
	fixture.api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		return []byte(`{"generation":"synthetic-generation","event_id":1}`), nil
	})
	controller := NewClientSessionViewControllerWithApi(t.Context(), fixture.api)
	t.Cleanup(controller.Close)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	controller.testingBeforeSubmit = func() { close(entered); <-release }
	controller.testingAfterAction = func() { close(done) }
	controller.RevokeOtherSessions()
	testingAwaitAuthBoundary(t, entered)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("current paired auth did not reset while the action was held")
	}
	resume()
	testingAwaitAuthBoundary(t, done)
	if sends.Load() != 0 {
		t.Error("session action admitted before reset used the retired admin credential")
	}
	controller.stateLock.Lock()
	target := controller.state.BulkAction.target
	controller.stateLock.Unlock()
	if _, current := fixture.api.currentNetworkTarget(target); current {
		t.Error("accepted reset retained the captured network generation")
	}
}

// A failure before destructive work remains an operational error. The reset
// fix must not retire the account merely because its preserved keys are unreadable.
func TestPairedResetReadFailureKeepsNetworkCredential(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	if err := os.Mkdir(filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material"), LocalStorageDirectoryPermissions); err != nil {
		t.Fatal("could not prepare the synthetic key-read failure")
	}
	var sends atomic.Int32
	fixture.api.setHttpGetRaw(func(_ context.Context, _ string, byJwt string) ([]byte, error) {
		sends.Add(1)
		if byJwt != fixture.adminJwt {
			t.Error("operational failure changed the selected admin credential")
		}
		return []byte(`{}`), nil
	})
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err == nil || result != nil {
		t.Fatal("unreadable preserved keys authorized destructive cleanup")
	}
	requestErr := awaitApiCall(func(callback connect.ApiCallback[*NetworkClientsResult]) {
		fixture.api.GetNetworkClients(callback)
	})
	if requestErr != nil || sends.Load() != 1 || !fixture.api.HasNetworkCredential() ||
		fixture.api.GetByJwt() != fixture.adminJwt {
		t.Error("failed reset retired the still-current sign-in")
	}
}
