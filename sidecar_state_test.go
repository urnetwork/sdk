//go:build !ios_extension && !js

package sdk

// §18.5's state calls on both backends (SDK-M8-01): GetState and GetStateReason name the core's state, Unlock("")
// takes the key store's wrap once the store answers and leaves a locked store Locked, and the remote backend answers
// its daemon's state and refuses to unlock.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A remote backend that answers a daemon's state.
type testSidecarRemote struct {
	daemonState  string
	daemonReason string
	closed       bool
}

func (self *testSidecarRemote) sidecarId() *Id {
	return nil
}

func (self *testSidecarRemote) state() (string, string) {
	return self.daemonState, self.daemonReason
}

func (self *testSidecarRemote) close() {
	self.closed = true
}

func (self *testSidecarRemote) generation() int64 {
	return 0
}

func (self *testSidecarRemote) refresh(ctx context.Context) error {
	return nil
}

func (self *testSidecarRemote) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errSidecarRemoteOffline
}

func (self *testSidecarRemote) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	return errSidecarRemoteOffline
}

// A local Sidecar's state is its core's: unenrolled before enrollment, and locked while the key store is.
func TestGetStateNamesTheCore(t *testing.T) {
	world := newTestSidecarWorld(t)
	first := world.open(t)
	awaitTestSidecarState(t, first, sidecar.SidecarStateUnenrolled, "")
	if state, reason := first.GetState(), first.GetStateReason(); state != "unenrolled" || reason != "" {
		t.Fatalf("before enrollment: %s %s", state, reason)
	}
	world.recordNetwork(t, first)
	world.close(t, first)
	if state := first.GetState(); state == "" {
		t.Fatal("a closed Sidecar named no state")
	}

	world.keyStore.setMode(testKeyStoreLocked)
	second := world.open(t)
	awaitTestSidecarState(t, second, sidecar.SidecarStateLocked, "")
	if state := second.GetState(); state != "locked" {
		t.Fatalf("with the store locked: %s", state)
	}
	// Unlock("") answers the locked store and creates nothing
	callback, results := testSidecarResultCallback()
	second.Unlock("", callback)
	if result := <-results; result.Ok {
		t.Fatalf("an unlock while the store is locked: %+v", result)
	}
	world.keyStore.setMode(testKeyStoreWorking)
	callback, results = testSidecarResultCallback()
	second.Unlock("", callback)
	if result := <-results; !result.Ok {
		t.Fatalf("an unlock once the store answers: %+v", result)
	}
	awaitTestSidecarStateMoved(t, second, sidecar.SidecarStateLocked)
	// the network's keychain was never made, so the open lands at keystore_lost, as a lost keychain.wrap does
	if state, reason := second.GetState(), second.GetStateReason(); state != "unenrolled" || reason != "keystore_lost" {
		t.Fatalf("after the unlock: %s %s", state, reason)
	}
}

// A remote backend answers its daemon's state and refuses to unlock: the daemon opens its own keychain.
func TestRemoteStateAndUnlock(t *testing.T) {
	remote := &testSidecarRemote{daemonState: "revoked", daemonReason: "signed"}
	local := &Sidecar{remote: remote}
	if state, reason := local.GetState(), local.GetStateReason(); state != "revoked" || reason != "signed" {
		t.Fatalf("the daemon's state: %s %s", state, reason)
	}
	callback, results := testSidecarResultCallback()
	local.Unlock("a pass", callback)
	if result := <-results; result.Ok {
		t.Fatalf("a remote unlock: %+v", result)
	}
}
