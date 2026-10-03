package sdk

// Actual remote teardown keeps a replacement's API resources intact.

import "testing"

// Identical credentials do not make two constructed remotes one owner.
func TestRetiredRemoteClosePreservesIdenticalTokenReplacement(t *testing.T) {
	old := testingAuthClientShapeSpace(t)
	old.seedDistinctLogin(t)
	old.startRemote(t)
	replacement := &testingAuthClientShape{
		networkSpace: old.networkSpace, localState: old.localState,
		initialJwt: old.initialJwt, adminJwt: old.adminJwt,
		instanceId: old.instanceId, api: old.api,
	}
	replacement.startRemote(t)
	old.closeDevice()
	if replacement.api.GetByJwt() != replacement.initialJwt {
		t.Error("old close cleared an identical-token replacement credential")
	}
	replacement.api.mutex.Lock()
	postOwner := replacement.api.httpPostRawOwner
	getOwner := replacement.api.httpGetRawOwner
	replacement.api.mutex.Unlock()
	if postOwner != replacement.remoteDevice.authPublication || getOwner != replacement.remoteDevice.authPublication {
		t.Error("old close replaced or cleared the new remote's HTTP owners")
	}
}

// A relogin admin token installed before a new remote exists belongs to the
// login, while the closing old remote should still release its own HTTP hooks.
func TestRemoteClosePreservesExplicitAdminLogin(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	fixture.closeDevice()
	if fixture.api.GetByJwt() != fixture.adminJwt {
		t.Error("old remote teardown cleared an in-progress admin login")
	}
	fixture.api.mutex.Lock()
	postInstalled := fixture.api.httpPostRaw != nil
	getInstalled := fixture.api.httpGetRaw != nil
	fixture.api.mutex.Unlock()
	if postInstalled || getInstalled {
		t.Error("old remote retained its own hooks after admin login")
	}
}

// Explicitly installing even identical API bytes revokes the former owner.
func TestRemoteClosePreservesEqualExplicitApiLogin(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	fixture.api.SetByJwt(fixture.initialJwt)
	fixture.closeDevice()
	if fixture.api.GetByJwt() != fixture.initialJwt {
		t.Error("old remote cleared an explicit equal-token API owner")
	}
}

// The same ownership fence protects local-to-local replacement teardown.
func TestRetiredLocalClosePreservesIdenticalTokenReplacement(t *testing.T) {
	old := testingAuthClientShapeSpace(t)
	old.seedDistinctLogin(t)
	old.startLocal(t)
	replacement := &testingAuthClientShape{
		networkSpace: old.networkSpace, localState: old.localState,
		initialJwt: old.initialJwt, adminJwt: old.adminJwt,
		instanceId: old.instanceId, api: old.api,
	}
	replacement.startLocal(t)
	old.closeDevice()
	if replacement.api.GetByJwt() != replacement.initialJwt {
		t.Error("old local teardown cleared an identical-token replacement")
	}
	replacement.requireCurrentRefresh(t)
}

func TestRetiredRemoteCloseCannotClearReplacementApiOwner(t *testing.T) {
	old := testingAuthClientShapeSpace(t)
	old.seedDistinctLogin(t)
	old.startRemote(t)
	replacement := &testingAuthClientShape{
		networkSpace: old.networkSpace,
		localState:   old.localState,
		initialJwt:   testingRefreshableJwtWithMarker(t, "replacement-api-owner"),
		adminJwt:     old.adminJwt,
		instanceId:   NewId(),
		api:          old.api,
	}
	replacement.startRemote(t)
	if replacement.api.GetByJwt() != replacement.initialJwt {
		t.Fatal("replacement constructor did not install its client credential")
	}
	replacement.api.mutex.Lock()
	beforePostInstalled := replacement.api.httpPostRaw != nil
	beforeGetInstalled := replacement.api.httpGetRaw != nil
	replacement.api.mutex.Unlock()
	if !beforePostInstalled || !beforeGetInstalled {
		t.Fatal("replacement constructor did not install its API transports")
	}

	old.closeDevice()

	if replacement.api.GetByJwt() != replacement.initialJwt {
		t.Error("retired remote close cleared the replacement API credential")
	}
	replacement.api.mutex.Lock()
	afterPostInstalled := replacement.api.httpPostRaw != nil
	afterGetInstalled := replacement.api.httpGetRaw != nil
	replacement.api.mutex.Unlock()
	if !afterPostInstalled || !afterGetInstalled {
		t.Error("retired remote close cleared the replacement API transports")
	}
}

func TestRemoteCloseClearsItsOwnApiOwner(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	fixture.closeDevice()
	if fixture.api.GetByJwt() != "" {
		t.Error("closed remote retained its own API credential")
	}
	fixture.api.mutex.Lock()
	postInstalled := fixture.api.httpPostRaw != nil
	getInstalled := fixture.api.httpGetRaw != nil
	fixture.api.mutex.Unlock()
	if postInstalled || getInstalled {
		t.Error("closed remote retained its own API transport hooks")
	}
}
