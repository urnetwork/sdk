package sdk

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The network credential renews on the client token's refresh window
// (apiTokenRefreshSettings): each token's point from 40% to 50% of its
// lifetime, a hash of its id, never later than its half-life, never sooner
// than the floor after the previous renewal (owner decision, 2026-11-01).
// Tests install their own points through the renewer's draw.

// installs the window draws a test's renewals take, in order
func (self *renewalTestApi) drawWindow(t *testing.T, draws ...float64) func() int {
	var drawLock sync.Mutex
	count := 0
	self.api.networkRenewer.draw = func(string) float64 {
		drawLock.Lock()
		defer drawLock.Unlock()
		if len(draws) <= count {
			t.Errorf("window draw %d, but the test has %d", count+1, len(draws))
			return 0
		}
		count += 1
		return draws[count-1]
	}
	return func() int {
		drawLock.Lock()
		defer drawLock.Unlock()
		return count
	}
}

// Every renewal draws its token's point again: 30-day tokens wait 12, 15 and
// 13.5 days for draws 0, 1 and 0.5. A wake that changes nothing keeps the
// point it has.
func TestNetworkCredentialRenewalRedrawsEachRenewal(t *testing.T) {
	r := newRenewalTestApi(t)
	drawCount := r.drawWindow(t, 0, 1, 0.5)
	day := 24 * time.Hour
	networkJwt := renewalTestRenewedJwt(t, renewalTestBaseTime, "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)
	r.clock.requireArmed(t, 12*day)
	connect.AssertEqual(t, drawCount(), 1)

	// a wake that changes nothing
	r.clock.Advance(day)
	r.api.networkRenewer.credentialChanged()
	r.clock.requireArmed(t, 11*day)
	connect.AssertEqual(t, drawCount(), 1)
	r.transport.requireNone(t)

	byJwt := networkJwt
	for _, wait := range []time.Duration{11 * day, 15 * day} {
		r.clock.Advance(wait)
		request := r.transport.takeRenewal(t, byJwt)
		byJwt = renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
		request.answer(renewalTestReplyJwt(byJwt))
		r.requireRenewed(t)
		if wait == 11*day {
			r.clock.requireArmed(t, 15*day)
		} else {
			r.clock.requireArmed(t, 13*day+12*time.Hour)
		}
	}
	connect.AssertEqual(t, drawCount(), 3)
	connect.AssertEqual(t, localState.GetByJwt(), byJwt)
}

// A token loaded past its drawn point renews at once; one loaded inside the
// window before its point renews at the point, never after the half-life.
func TestNetworkCredentialLoadedPastItsPointRenewsPromptly(t *testing.T) {
	day := 24 * time.Hour
	// 13.5 of 30 days old: 45% of its lifetime
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-13*day-12*time.Hour), renewalTestBaseTime.Add(16*day+12*time.Hour), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")

	past := newRenewalTestApi(t)
	past.drawWindow(t, 0, 0)
	pastState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	past.startDevice(t, pastState, clientJwt)
	request := past.transport.takeRenewal(t, networkJwt)
	past.clock.requireNotArmed(t)
	request.answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, past.clock.Now(), "renewed")))
	past.requireRenewed(t)
	past.clock.requireArmed(t, 12*day)

	ahead := newRenewalTestApi(t)
	ahead.drawWindow(t, 1)
	aheadState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	ahead.startDevice(t, aheadState, clientJwt)
	ahead.clock.requireArmed(t, day+12*time.Hour)
	ahead.transport.requireNone(t)
}

// A renewal that answers a token whose point is already due -- a lifetime
// shorter than the floor allows, or a clock far ahead of the server's -- does
// not renew again at once: renewals are spaced by MinRefreshTimeout.
func TestNetworkCredentialRenewalsOfShortLifetimesAreSpaced(t *testing.T) {
	day := 24 * time.Hour
	settings := defaultApiTokenRefreshSettings()
	for _, c := range []struct {
		lifetime time.Duration
		// how long before the clock's now each renewal was issued
		age time.Duration
	}{
		{lifetime: 2 * time.Minute},
		{lifetime: time.Second},
		{lifetime: 30 * day, age: 16 * day},
	} {
		r := newRenewalTestApi(t)
		r.drawWindow(t, 0, 0.5, 1, 0)
		clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
		// expired: renewed at once
		byJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
		localState := newRenewalTestLocalState(t, t.TempDir(), byJwt, clientJwt)
		r.startDevice(t, localState, clientJwt)
		for i := 0; i < 3; i += 1 {
			request := r.transport.takeRenewal(t, byJwt)
			now := r.clock.Now()
			byJwt = renewalTestNetworkJwt(t, now.Add(-c.age), now.Add(-c.age+c.lifetime), "renewed")
			request.answer(renewalTestReplyJwt(byJwt))
			r.requireRenewed(t)
			r.clock.requireArmed(t, settings.MinRefreshTimeout)
			r.clock.Advance(settings.MinRefreshTimeout)
		}
		r.transport.takeRenewal(t, byJwt)
	}
}

// The draw's upper end is the half-life, and a draw out of range does not pass
// it: a fresh 30-day token never waits more than 15 days.
func TestNetworkCredentialRenewalNeverSchedulesPastHalfLife(t *testing.T) {
	day := 24 * time.Hour
	r := newRenewalTestApi(t)
	r.drawWindow(t, 1, 2, math.Inf(1), 1.5)
	byJwt := renewalTestRenewedJwt(t, renewalTestBaseTime, "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), byJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)
	for i := 0; i < 3; i += 1 {
		r.clock.requireArmed(t, 15*day)
		r.clock.Advance(15 * day)
		request := r.transport.takeRenewal(t, byJwt)
		byJwt = renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
		request.answer(renewalTestReplyJwt(byJwt))
		r.requireRenewed(t)
	}
	r.clock.requireArmed(t, 15*day)
}

// A relaunch takes the same point for the token it loads, with the production
// draw: two processes that load one token 45% into its life agree on whether
// it is past its point, and if not, on the wait. A point drawn afresh by each
// process instead made a mass restart renew the tokens whose new point had
// passed in one burst, and frequent restarts renew early.
func TestNetworkCredentialRelaunchKeepsTheTokensPoint(t *testing.T) {
	day := 24 * time.Hour
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	settings := defaultApiTokenRefreshSettings()
	// the draw a renewer is built with, which the harness replaces
	productionDraw := newApiNetworkCredentialRenewer(context.Background(), nil, settings).draw
	pastCount := 0
	for i := 0; i < 8; i += 1 {
		// 13.5 of 30 days old: 45% of its lifetime
		networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-13*day-12*time.Hour), renewalTestBaseTime.Add(16*day+12*time.Hour), fmt.Sprintf("sign-in-%d", i))
		point := networkCredentialRenewalTime(networkJwt, renewalTestBaseTime, jwtRefreshDraw(networkJwt), settings)
		for _, process := range []string{"first", "relaunched"} {
			r := newRenewalTestApi(t)
			r.api.networkRenewer.draw = productionDraw
			r.startDevice(t, newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt), clientJwt)
			if point.After(r.clock.Now()) {
				r.clock.requireArmed(t, point.Sub(r.clock.Now()))
				r.transport.requireNone(t)
			} else {
				r.transport.takeRenewal(t, networkJwt)
				r.clock.requireNotArmed(t)
				if process == "first" {
					pastCount += 1
				}
			}
		}
	}
	// half of the window is behind a token 45% into its life
	if pastCount == 0 || pastCount == 8 {
		t.Fatalf("%d of 8 tokens past their point; the points do not spread", pastCount)
	}
}

// A renewer with nothing to renew keeps no copy of the token it scheduled
// last: after a sign-out it holds no credential.
func TestNetworkCredentialRenewerDropsTheTokenOnSignOut(t *testing.T) {
	day := 24 * time.Hour
	r := newRenewalTestApi(t)
	parked := make(chan struct{}, 16)
	r.api.networkRenewer.testingParked = func() {
		parked <- struct{}{}
	}
	networkJwt := renewalTestRenewedJwt(t, renewalTestBaseTime, "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	r.startDevice(t, localState, clientJwt)
	r.clock.requireArmed(t, 15*day)

	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	r.api.SetByJwt("")
	select {
	case <-parked:
	case <-time.After(renewalTestStepLimit):
		t.Fatal("the signed-out renewer did not park")
	}
	// parked in its select, the renewer writes nothing until the next wake
	connect.AssertEqual(t, r.api.networkRenewer.scheduledByJwt, "")
	connect.AssertEqual(t, r.api.networkRenewer.scheduledTime.IsZero(), true)
	r.transport.requireNone(t)
}

// The floor belongs to the token, not to the credential generation. A
// renewal that answers a token already past its point (a device clock 16 days
// ahead of the server's) waits out the floor when a device starts from another
// LocalState that keeps the same pair, instead of renewing again at once. A
// new sign-in starts the floor over (TestRejectedNetworkCredentialIsDroppedOnly).
func TestNetworkCredentialRenewalFloorOutlivesTheGeneration(t *testing.T) {
	day := 24 * time.Hour
	r := newRenewalTestApi(t)
	r.drawWindow(t, 0, 0, 0)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	expired := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-40*day), renewalTestBaseTime.Add(-10*day), "sign-in")
	r.startDevice(t, newRenewalTestLocalState(t, t.TempDir(), expired, clientJwt), clientJwt)
	request := r.transport.takeRenewal(t, expired)
	now := r.clock.Now()
	renewed := renewalTestNetworkJwt(t, now.Add(-16*day), now.Add(14*day), "renewed")
	request.answer(renewalTestReplyJwt(renewed))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 5*time.Minute)
	r.transport.requireNone(t)

	// the same token, a new generation, a minute later
	r.clock.Advance(time.Minute)
	r.startDevice(t, newRenewalTestLocalState(t, t.TempDir(), renewed, clientJwt), clientJwt)
	r.clock.requireArmed(t, 4*time.Minute)
	r.transport.requireNone(t)

	r.clock.Advance(4 * time.Minute)
	r.transport.takeRenewal(t, renewed).answer(renewalTestReplyJwt(renewalTestRenewedJwt(t, r.clock.Now(), "renewed-again")))
	r.requireRenewed(t)
}
