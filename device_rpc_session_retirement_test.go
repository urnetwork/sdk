// Accepted RPC sessions must leave the API's listener registry at every exit.
package sdk

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect"
)

// Keeps the API alive after its manager, so abandoned registrations remain observable.
type testingRpcSessionRetirement struct {
	api             *Api
	manager         *deviceLocalRpcManager
	listener        *testingScriptedDeviceRpcListener
	cancel          context.CancelFunc
	externalUpdates atomic.Int64
}

// Uses the real API and manager with only transport admission supplied by the fixture.
func newTestingRpcSessionRetirement(t *testing.T) *testingRpcSessionRetirement {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	fixture := &testingRpcSessionRetirement{
		api: newApi(t.Context(), nil, "https://rpc.example"),
		listener: &testingScriptedDeviceRpcListener{
			entered: make(chan struct{}),
			results: make(chan testingDeviceRpcAcceptResult),
		},
		cancel: cancel,
	}
	independent := fixture.api.AddNetworkSessionsChangeListener(sessionHintTestListener(func(*NetworkSessionsRevision) {
		fixture.externalUpdates.Add(1)
	}))
	fixture.manager = newDeviceLocalRpcManager(ctx, &DeviceLocal{
		api: fixture.api,
		log: connect.NewNoopLogger(),
	}, defaultDeviceRpcSettings(), fixture.listener)
	t.Cleanup(func() {
		cancel()
		if err := fixture.manager.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join manager: %v", err)
		}
		independent.Close()
		if err := fixture.api.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join api: %v", err)
		}
	})
	fixture.waitForAccept(t)
	return fixture
}

// The next accept is a barrier after the preceding session was fully constructed.
func (self *testingRpcSessionRetirement) waitForAccept(t *testing.T) {
	t.Helper()
	select {
	case <-self.listener.entered:
	case <-self.manager.done:
		t.Fatal("manager exited before the next accept")
	}
}

// Returns the exact session found in the real registry, not a newest-entry guess.
func (self *testingRpcSessionRetirement) accept(t *testing.T) (*DeviceLocalRpc, net.Conn) {
	t.Helper()
	forward, forwardPeer := net.Pipe()
	reverse, reversePeer := net.Pipe()
	t.Cleanup(func() {
		forwardPeer.Close()
		reversePeer.Close()
	})
	self.listener.results <- testingDeviceRpcAcceptResult{forward: forward, reverse: reverse}
	self.waitForAccept(t)
	for _, listener := range self.api.networkSessionsListeners.Get() {
		if session, ok := listener.(*DeviceLocalRpc); ok && session.conn == forward {
			return session, forwardPeer
		}
	}
	t.Fatal("accepted session did not install its API subscription")
	return nil, nil
}

// A fresh API event after join must reach the independent owner, never a retired queue.
func (self *testingRpcSessionRetirement) assertRetired(t *testing.T, sessions ...*DeviceLocalRpc) {
	t.Helper()
	retained := 0
	for _, listener := range self.api.networkSessionsListeners.Get() {
		for _, session := range sessions {
			if listener == session {
				retained++
			}
		}
	}
	self.api.networkSessionsChanged(&NetworkSessionsRevision{Generation: "synthetic-generation", EventId: 1})
	pending := 0
	for _, session := range sessions {
		select {
		case <-session.done:
		default:
			t.Fatal("retirement observed before the session joined")
		}
		session.sendMu.Lock()
		pending += len(session.sendPending)
		session.sendMu.Unlock()
	}
	if retained != 0 || pending != 0 {
		t.Fatalf("joined sessions retained subscriptions=%d pending notifications=%d; want 0, 0", retained, pending)
	}
	if listeners := len(self.api.networkSessionsListeners.Get()); listeners != 1 || self.externalUpdates.Load() != 1 {
		t.Fatalf("independent owner listeners=%d updates=%d; want 1, 1", listeners, self.externalUpdates.Load())
	}
}

// Manager cancellation joins both accepted sessions without explicitly closing either RPC.
func TestDeviceLocalRpcManagerCancellationRetiresNetworkSessionSubscriptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newTestingRpcSessionRetirement(t)
		first, _ := fixture.accept(t)
		second, _ := fixture.accept(t)
		fixture.cancel()
		if err := fixture.manager.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.assertRetired(t, first, second)
	})
}

// Peer EOF retires a session while the manager and API remain available for another.
func TestDeviceLocalRpcPeerEofRetiresNetworkSessionSubscriptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newTestingRpcSessionRetirement(t)
		retired := make([]*DeviceLocalRpc, 0, 3)
		for range 3 {
			session, peer := fixture.accept(t)
			if err := peer.Close(); err != nil {
				t.Fatal(err)
			}
			<-session.done
			retired = append(retired, session)
		}
		select {
		case <-fixture.manager.done:
			t.Fatal("peer EOF terminated the shared manager")
		default:
		}
		fixture.assertRetired(t, retired...)
	})
}

// Existing explicit-close behavior is the control; repeated removal must remain harmless.
func TestDeviceLocalRpcExplicitCloseRetiresNetworkSessionSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newTestingRpcSessionRetirement(t)
		session, _ := fixture.accept(t)
		session.Close()
		if err := session.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.assertRetired(t, session)
	})
}
