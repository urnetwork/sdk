//go:build !js

// The forwarding roles' clock (sidecar_clock_check.go's comment; QD-1422, owner decision 789's seam): each role's
// Forwarder and its refresh of F wait for wall-clock times on runtime timers, which stop while the device sleeps.
// DeviceLocal.CheckClock hands every role a check, which has its Forwarder read the clock again (Forwarder.CheckClock)
// and its refresh loop arm its wait for F's refresh point anew from now, refreshing at once past it. A refresh's retry
// is a relative wait and keeps its own time. On a desktop, where no platform call reaches the device, each role
// checks itself each of the bridges' ClockCheckInterval while its Forwarder runs (runClockCap), so one interval caps
// the bridges and the roles; a phone's bridges take none, and its roles run no such loop.

package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar"
)

// Hands every forwarding role a check of its clock, waiting on none.
func (self *deviceLocalForwardingHost) checkClock() {
	roles := func() []*deviceLocalForwarding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		roles := make([]*deviceLocalForwarding, 0, len(self.roles))
		for _, role := range self.roles {
			roles = append(roles, role)
		}
		return roles
	}()
	for _, role := range roles {
		role.checkClock()
	}
}

// Has the role's Forwarder and refresh loop read the clock again at once (the file's comment). A role with no Forwarder
// running does nothing; a check that comes while one waits for the refresh loop is one. Never blocks.
func (self *deviceLocalForwarding) checkClock() {
	forwarder, checked := func() (*sidecar.Forwarder, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.forwarder, self.clockChecked
	}()
	if forwarder == nil {
		return
	}
	forwarder.CheckClock()
	select {
	case checked <- struct{}{}:
	default:
	}
}

// The desktop's cap (the file's comment): a check of the role's clock each interval on its clock, until ctx ends.
func (self *deviceLocalForwarding) runClockCap(ctx context.Context, interval time.Duration) {
	defer self.loops.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-self.currentClock().After(interval):
		}
		self.checkClock()
	}
}
