// Product events belong to the network signed in when they were added (owner
// decision 2026-10-05: logout must not cross contaminate other networks): what
// a signed-out network left unsent never goes out under the next network's
// credential, and no session id is shared by two networks.
package sdk

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The sign-in a queue sees, switched by the test as an app switches its Api's
// credential.
type testingEventCredential struct {
	signedIn  bool
	networkId string
}

// The queue's signed-in seam.
func (self *testingEventCredential) hasJwt() bool {
	return self.signedIn
}

// The queue's network seam: the signed-in network, empty when signed out.
func (self *testingEventCredential) currentNetworkId() string {
	if !self.signedIn {
		return ""
	}
	return self.networkId
}

// Installs the credential of `networkId`.
func (self *testingEventCredential) signIn(networkId string) {
	self.signedIn = true
	self.networkId = networkId
}

// Clears the credential.
func (self *testingEventCredential) signOut() {
	self.signedIn = false
	self.networkId = ""
}

// A queue on the credential seams above, closed with the test.
func testingNetworkEventQueue(t *testing.T, sender *fakeEventSender, credential *testingEventCredential, statePath string) *ClientEventQueue {
	t.Helper()
	q := newClientEventQueue(context.Background(), sender.send, credential.hasJwt, credential.currentNetworkId, statePath, EventPlatformAndroid, "1", "en", time.Hour)
	t.Cleanup(q.Close)
	return q
}

// Network A's events that could not be sent before it signed out are dropped
// once network B signs in, in the same process and after a restart that
// reloads them from disk; B's own events go out.
func TestClientEventQueueNeverSendsAnotherNetworksEvents(t *testing.T) {
	for _, restart := range []bool{false, true} {
		statePath := filepath.Join(t.TempDir(), clientEventQueueFile)
		sender := &fakeEventSender{failAll: true}
		credential := &testingEventCredential{}
		credential.signIn(testingIdentityNetworkA)
		q := testingNetworkEventQueue(t, sender, credential, statePath)
		q.Add(NewOfferCardTappedEvent(PlanYearly))
		q.Add(NewConnectFirstEvent())
		// offline at the sign-out: the drain fails and the events stay
		q.flushOnce()
		if q.PendingCount() != 2 {
			t.Fatal("the failed send did not keep network A's events")
		}
		credential.signOut()
		if restart {
			q.Close()
			q = testingNetworkEventQueue(t, sender, credential, statePath)
			if q.PendingCount() != 2 {
				t.Fatal("network A's events were not persisted")
			}
		}

		credential.signIn(testingIdentityNetworkB)
		sender.mutex.Lock()
		sender.failAll = false
		sender.batches = nil
		sender.mutex.Unlock()
		q.Add(NewWidgetAddedEvent("globe"))
		q.FlushAndWait(5000)

		if q.PendingCount() != 0 {
			t.Fatalf("restart=%t: events are still pending", restart)
		}
		var sent []*ClientEvent
		for _, batch := range sender.batches {
			sent = append(sent, batch...)
		}
		if len(sent) != 1 || sent[0].Name != EventWidgetAdded {
			names := []string{}
			for _, event := range sent {
				names = append(names, event.Name)
			}
			t.Fatalf("restart=%t: network B's credential sent %v, want only its own widget.added", restart, names)
		}
	}
}

// The same network signing back in sends what it left unsent.
func TestClientEventQueueKeepsTheSameNetworksEvents(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), clientEventQueueFile)
	sender := &fakeEventSender{failAll: true}
	credential := &testingEventCredential{}
	credential.signIn(testingIdentityNetworkA)
	q := testingNetworkEventQueue(t, sender, credential, statePath)
	q.Add(NewConnectFirstEvent())
	q.flushOnce()
	credential.signOut()
	credential.signIn(testingIdentityNetworkA)
	sender.mutex.Lock()
	sender.failAll = false
	sender.batches = nil
	sender.mutex.Unlock()
	q.FlushAndWait(5000)
	if q.PendingCount() != 0 || len(sender.batches) != 1 || sender.batches[0][0].Name != EventConnectFirst {
		t.Fatal("network A's own event was not sent when it signed back in")
	}
}

// A session never spans two networks. A signed-out stretch starts a new
// session, which the next sign-in continues (the sign-up funnel).
func TestClientEventSessionDoesNotSpanNetworks(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), clientEventQueueFile)
	sender := &fakeEventSender{failAll: true}
	credential := &testingEventCredential{}
	q := testingNetworkEventQueue(t, sender, credential, statePath)
	sessionOf := func() string {
		q.mutex.Lock()
		defer q.mutex.Unlock()
		return q.pending[len(q.pending)-1].Event.Session
	}

	credential.signIn(testingIdentityNetworkA)
	q.Add(NewConnectFirstEvent())
	sessionA := sessionOf()
	q.Add(NewWidgetAddedEvent("globe"))
	if sessionOf() != sessionA {
		t.Fatal("one network's events did not share their session")
	}

	// straight to another network
	credential.signIn(testingIdentityNetworkB)
	q.Add(NewConnectFirstEvent())
	sessionB := sessionOf()
	if sessionB == sessionA {
		t.Fatal("network B's events carry network A's session")
	}

	// signed out, then network A again
	credential.signOut()
	q.Add(NewOnboardingStepShownEvent("welcome", 0, 0))
	sessionSignedOut := sessionOf()
	if sessionSignedOut == sessionB {
		t.Fatal("the signed-out events carry network B's session")
	}
	credential.signIn(testingIdentityNetworkA)
	q.Add(NewConnectFirstEvent())
	if sessionOf() != sessionSignedOut {
		t.Fatal("the sign-in did not continue the signed-out session")
	}
	if sessionOf() == sessionA {
		t.Fatal("network A's new sign-in reused its earlier session")
	}
}
