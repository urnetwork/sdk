package sdk

import (
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
)

// SubscriptionProTracker is the decision the apps that read the balance
// themselves use (support inbox 8211): Pro is the server's once a balance has
// loaded, and a claim that disagrees asks for one refresh per jwt.

// testNetworkJwt is a fake, unsigned network jwt of the given network.
func testNetworkJwt(t *testing.T, networkId string, pro bool, issuedAt int64) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"network_id": networkId,
		"pro":        pro,
		"iat":        issuedAt,
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

const (
	testTrackerNetworkA = "00000000-0000-0000-0000-00000000000a"
	testTrackerNetworkB = "00000000-0000-0000-0000-00000000000b"
)

func TestSubscriptionProTrackerProIsTheServersOnceLoaded(t *testing.T) {
	tracker := NewSubscriptionProTracker()
	connect.AssertEqual(t, tracker.GetIsPro(), false)

	// before a load, the claim
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 1)), true)
	connect.AssertEqual(t, tracker.GetIsPro(), true)
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 2)), true)
	connect.AssertEqual(t, tracker.GetIsPro(), false)

	// a load: the server's answer
	tracker.BalanceLoaded(true)
	connect.AssertEqual(t, tracker.GetIsPro(), true)

	// a jwt that arrives after the load never moves Pro back to its claim
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 3)), false)
	connect.AssertEqual(t, tracker.GetIsPro(), true)

	// the next load moves it
	tracker.BalanceLoaded(false)
	connect.AssertEqual(t, tracker.GetIsPro(), false)
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 4)), false)
	connect.AssertEqual(t, tracker.GetIsPro(), false)
}

func TestSubscriptionProTrackerAsksOncePerJwt(t *testing.T) {
	for _, serverIsPro := range []bool{true, false} {
		tracker := NewSubscriptionProTracker()
		staleJwt := testNetworkJwt(t, testTrackerNetworkA, !serverIsPro, 1)
		tracker.SetJwt(staleJwt)

		if !tracker.BalanceLoaded(serverIsPro) {
			t.Fatalf("server %t: a disagreeing claim asked for no refresh", serverIsPro)
		}
		for i := 0; i < 5; i += 1 {
			// the refresh renewed another token: the same jwt comes back
			connect.AssertEqual(t, tracker.SetJwt(staleJwt), false)
			if tracker.BalanceLoaded(serverIsPro) {
				t.Fatalf("server %t, read %d: asked again for the jwt it already asked about", serverIsPro, i)
			}
			connect.AssertEqual(t, tracker.GetIsPro(), serverIsPro)
		}

		// a renewed jwt that still disagrees asks once more
		tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, !serverIsPro, 2))
		connect.AssertEqual(t, tracker.BalanceLoaded(serverIsPro), true)
		connect.AssertEqual(t, tracker.BalanceLoaded(serverIsPro), false)

		// a jwt that agrees asks for nothing
		tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, serverIsPro, 3))
		connect.AssertEqual(t, tracker.BalanceLoaded(serverIsPro), false)
		connect.AssertEqual(t, tracker.GetIsPro(), serverIsPro)
	}

	// no jwt: nothing to refresh
	tracker := NewSubscriptionProTracker()
	connect.AssertEqual(t, tracker.BalanceLoaded(true), false)
	connect.AssertEqual(t, tracker.GetIsPro(), true)
}

func TestSubscriptionProTrackerStartsOverForAnotherSession(t *testing.T) {
	tracker := NewSubscriptionProTracker()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 1))
	tracker.BalanceLoaded(true)
	connect.AssertEqual(t, tracker.GetIsPro(), true)

	// another network's jwt: its claim until its own balance loads
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkB, false, 1)), true)
	connect.AssertEqual(t, tracker.GetIsPro(), false)
	connect.AssertEqual(t, tracker.BalanceLoaded(false), false)

	// a read that found no jwt changes nothing
	tracker.BalanceLoaded(true)
	connect.AssertEqual(t, tracker.SetJwt(""), false)
	connect.AssertEqual(t, tracker.GetIsPro(), true)

	// a sign out, then a sign in to the first network again
	tracker.Reset()
	connect.AssertEqual(t, tracker.GetIsPro(), false)
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 1)), false)
	connect.AssertEqual(t, tracker.GetIsPro(), false)
	// the jwt asked about in the session before asks again in this one
	connect.AssertEqual(t, tracker.BalanceLoaded(true), true)

	tracker.Reset()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 1))
	connect.AssertEqual(t, tracker.GetIsPro(), true)
}

// pingPongStore has the shape of the desktop balance stores: each read
// reports the server's Pro and asks for a refresh when told to; the refresh
// renews the device's client token, whose listener makes the store read the
// stored network jwt again, which the refresh left as it was; and a change of
// the Pro shown switches the poll mode, which read the balance again at once
// (linux StartBackgroundPolling).
type pingPongStore struct {
	// reports the server's Pro on a read and whether to refresh
	balanceLoaded func(serverIsPro bool) (refresh bool)
	// re-reads the stored jwt after a refresh
	jwtRefreshed func()
	isPro        func() bool

	serverIsPro bool
	shownPro    bool
	queue       []func()

	reads     int
	refreshes int
	// every Pro shown after the first
	shownPros []bool
}

func (self *pingPongStore) run(maxEvents int) {
	self.shownPro = self.isPro()
	self.queue = append(self.queue, self.read)
	for events := 0; 0 < len(self.queue) && events < maxEvents; events += 1 {
		event := self.queue[0]
		self.queue = self.queue[1:]
		event()
	}
}

func (self *pingPongStore) read() {
	self.reads += 1
	if self.balanceLoaded(self.serverIsPro) {
		self.refreshes += 1
		self.queue = append(self.queue, func() {
			self.jwtRefreshed()
			self.show()
		})
	}
	self.show()
}

func (self *pingPongStore) show() {
	if pro := self.isPro(); pro != self.shownPro {
		self.shownPro = pro
		self.shownPros = append(self.shownPros, pro)
		self.queue = append(self.queue, self.read)
	}
}

func TestSubscriptionProTrackerStopsTheDesktopPingPong(t *testing.T) {
	const maxEvents = 1000
	for _, serverIsPro := range []bool{true, false} {
		// the stored network jwt still carries the claim from before the
		// upgrade (or the lapse); no refresh in this test renews it
		storedJwt := testNetworkJwt(t, testTrackerNetworkA, !serverIsPro, 1)

		// control: the stores' own rule before this change, which
		// refreshed whenever the stored claim disagreed and took Pro from
		// the claim again on each refresh, loops until the cap
		claimPro := !serverIsPro
		shownPro := claimPro
		control := &pingPongStore{
			serverIsPro: serverIsPro,
			balanceLoaded: func(answer bool) bool {
				shownPro = answer
				return answer != claimPro
			},
			jwtRefreshed: func() { shownPro = claimPro },
			isPro:        func() bool { return shownPro },
		}
		control.run(maxEvents)
		if control.reads < maxEvents/10 {
			t.Fatalf("server %t: the control read the balance %d times; the harness no longer reproduces the ping-pong", serverIsPro, control.reads)
		}

		tracker := NewSubscriptionProTracker()
		tracker.SetJwt(storedJwt)
		store := &pingPongStore{
			serverIsPro:   serverIsPro,
			balanceLoaded: tracker.BalanceLoaded,
			jwtRefreshed:  func() { tracker.SetJwt(storedJwt) },
			isPro:         tracker.GetIsPro,
		}
		store.run(maxEvents)
		if 0 < len(store.queue) {
			t.Fatalf("server %t: still reading after %d events (%d reads, %d refreshes)", serverIsPro, maxEvents, store.reads, store.refreshes)
		}
		connect.AssertEqual(t, store.refreshes, 1)
		// the first read, and the read the one change of plan starts
		connect.AssertEqual(t, store.reads, 2)
		// Pro moved once, to the server's, and never back
		connect.AssertEqual(t, store.shownPros, []bool{serverIsPro})
	}
}
