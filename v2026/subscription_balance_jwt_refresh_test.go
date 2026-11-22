package sdk

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// The Pro/jwt ping-pong (support inbox 8211). The jwt the apps read Pro from
// is the stored network token, which renews on its own schedule; the refresh
// an app asks for when the claim disagrees with the server renews the device's
// client token and leaves the network token as it was. The linux store then
// re-read the unchanged claim, flipped Pro back, and read the balance again at
// once, which asked for another refresh: a loop as fast as the api answered
// while connected with the window visible, and on windows one flip and one
// refresh per background poll.
//
// The controller's rule is the one the apps now use (SubscriptionProTracker):
// Pro is the server's answer once a balance loaded, and a disagreement asks for
// at most one refresh until the jwt actually changes. These tests drive step()
// by hand against a synchronous fetch, so the platform's loop runs in full
// inside each step: no polling goroutine, no sleeps, no network.

// testSubscriptionJwtIssued is a fake, unsigned jwt with an issue time, so a
// renewal of the same claims is another jwt.
func testSubscriptionJwtIssued(t *testing.T, pro bool, issuedAt int64) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000011",
		"pro":        pro,
		"guest_mode": false,
		"iat":        issuedAt,
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// refreshingPlatform answers every out-of-sync signal the way the desktop
// stores did: it refreshes the jwt, reports the refresh (JwtRefreshed) and
// reads the balance again at once (Refresh). The refresh renews another token,
// so the jwt the controller tracks stays as it was unless the test installs
// a new one.
type refreshingPlatform struct {
	vc      *SubscriptionBalanceViewController
	signals []bool
}

func (self *refreshingPlatform) SubscriptionJwtOutOfSync(serverIsPro bool) {
	self.signals = append(self.signals, serverIsPro)
	self.vc.JwtRefreshed()
	self.vc.Refresh()
}

// installJwt is a refresh that did change the tracked jwt.
func (self *refreshingPlatform) installJwt(byJwt string) {
	self.vc.api.SetByJwt(byJwt)
	self.vc.JwtRefreshed()
	self.vc.Refresh()
}

// stepWithServer runs the given number of steps and fails on a step that
// shows a Pro other than the server's.
func stepWithServer(t *testing.T, vc *SubscriptionBalanceViewController, clock *stepTestClock, steps int, advance time.Duration, serverIsPro bool) {
	t.Helper()
	for i := 0; i < steps; i += 1 {
		clock.advance(advance)
		vc.step()
		if vc.GetIsPro() != serverIsPro {
			t.Fatalf("step %d: Pro = %t, the server says %t: Pro flipped back to the jwt's stale claim", i, vc.GetIsPro(), serverIsPro)
		}
	}
}

func TestJwtOutOfSyncAsksOnceUntilTheJwtChanges(t *testing.T) {
	t.Run("upgrade", func(t *testing.T) {
		// the server says Pro with a balance (the background poll stops for
		// a supporter with a balance); the stored jwt still says free
		stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, "stripe"))
		vc, clock := newStepTestSubscriptionBalanceVc(t, stub)
		vc.api.SetByJwt(testSubscriptionJwtIssued(t, false, 1))
		platform := &refreshingPlatform{vc: vc}
		sub := vc.AddSubscriptionJwtOutOfSyncListener(platform)
		defer sub.Close()

		vc.Start()
		stepWithServer(t, vc, clock, 10, 0, true)
		if len(platform.signals) != 1 {
			t.Fatalf("%d refreshes asked for one jwt, want 1: each refresh that left the jwt as it was asked again, and the platform's read at once looped", len(platform.signals))
		}
		if !platform.signals[0] {
			t.Fatal("the upgrade must report serverIsPro = true")
		}
		// the first read and the platform's one read after its refresh
		if got := stub.fetchCount(); got != 2 {
			t.Fatalf("%d balance reads, want 2", got)
		}

		// a renewed jwt that still carries the stale claim asks once more
		platform.installJwt(testSubscriptionJwtIssued(t, false, 2))
		stepWithServer(t, vc, clock, 10, 0, true)
		if len(platform.signals) != 2 {
			t.Fatalf("%d refreshes after a renewed jwt that still disagrees, want 2", len(platform.signals))
		}

		// a jwt whose claim agrees asks for nothing
		platform.installJwt(testSubscriptionJwtIssued(t, true, 3))
		stepWithServer(t, vc, clock, 10, 0, true)
		if len(platform.signals) != 2 {
			t.Fatalf("%d refreshes after the claim agreed, want 2", len(platform.signals))
		}
	})

	t.Run("lapse", func(t *testing.T) {
		// the server says free (the background poll keeps running); the
		// stored jwt still says Pro
		stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
		vc, clock := newStepTestSubscriptionBalanceVc(t, stub)
		vc.api.SetByJwt(testSubscriptionJwtIssued(t, true, 1))
		platform := &refreshingPlatform{vc: vc}
		sub := vc.AddSubscriptionJwtOutOfSyncListener(platform)
		defer sub.Close()

		vc.Start()
		if !vc.GetIsPro() {
			t.Fatal("before a balance loads, Pro is the jwt's claim")
		}
		stepWithServer(t, vc, clock, 10, 0, false)
		if len(platform.signals) != 1 || platform.signals[0] {
			t.Fatalf("signals %v, want one with serverIsPro = false", platform.signals)
		}

		// the background poll reads the balance every interval and never asks
		// again for the same jwt
		fetches := stub.fetchCount()
		stepWithServer(t, vc, clock, 10, 30*time.Second, false)
		if got := stub.fetchCount() - fetches; got != 10 {
			t.Fatalf("%d background reads in 10 intervals, want 10", got)
		}
		if len(platform.signals) != 1 {
			t.Fatalf("%d refreshes across the background polls of one jwt, want 1", len(platform.signals))
		}

		// a renewed jwt that still disagrees asks once more
		platform.installJwt(testSubscriptionJwtIssued(t, true, 2))
		stepWithServer(t, vc, clock, 10, 30*time.Second, false)
		if len(platform.signals) != 2 {
			t.Fatalf("%d refreshes after a renewed jwt that still disagrees, want 2", len(platform.signals))
		}
	})
}
