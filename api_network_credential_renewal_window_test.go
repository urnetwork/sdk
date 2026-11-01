package sdk

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The network credential renews on the client token's refresh window
// (apiTokenRefreshSettings): a drawn point from 40% to 50% of each token's
// lifetime, never later than its half-life, never sooner than the floor after
// the previous renewal (owner decision, 2026-11-01).

// installs the window draws a test's renewals take, in order
func (self *renewalTestApi) drawWindow(t *testing.T, draws ...float64) func() int {
	var drawLock sync.Mutex
	count := 0
	self.api.networkRenewer.draw = func() float64 {
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
