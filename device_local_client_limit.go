package sdk

// device_local_client_limit.go -- the client limit status of a local device
// (client_limit_status.go): the getter, the listener, the watch that turns
// the provider's client limit hold into callbacks, and the retry an app asks
// for once its user has acted on the prompt.
//
// The device's platform connection is its provider's (deviceLocalProvider),
// which every app build runs whether or not it provides, so the hold of the
// provider's transports is the device's hold. A device built without a
// provider has no platform connection to hold and always reads
// ClientLimitStatusNone.
//
// The prompt follows the plan of the device's client, the pro and
// client_limit_upgrade claims of its client jwt (clientLimitPlanValue). The
// claims are as old as the jwt, which renews only at a fraction of its
// lifetime, so a new hold asks for a refresh, and refreshed claims that change
// the prompt of the hold in force are emitted again.

import (
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// GetClientLimitStatus is the client limit status of the device's platform
// connection. Never nil.
func (self *DeviceLocal) GetClientLimitStatus() *ClientLimitStatus {
	provider, closed := func() (*deviceLocalProvider, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.provider, self.closed
	}()
	if closed || provider == nil {
		return noneClientLimitStatus()
	}
	status, _ := provider.clientLimitStatus()
	return newClientLimitStatus(status, self.clientLimitPlanValue.Value())
}

// AddClientLimitStatusChangeListener subscribes to every change of the client
// limit status: a hold starting, a hold ending or being reset, and the prompt
// of the hold in force changing with the plan.
func (self *DeviceLocal) AddClientLimitStatusChangeListener(listener ClientLimitStatusChangeListener) Sub {
	callbackId := self.clientLimitStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.clientLimitStatusChangeListeners.Remove(callbackId)
	})
}

// RetryClientLimit ends the device's client limit hold at once, so its
// platform connection dials again now instead of at the hold's retry time.
// An app calls it when its user has acted on the prompt: an upgrade whose
// purchase is confirmed, or a slot freed on another device. It also refreshes
// the client jwt, so a refusal of the new dial, which starts a new hold of at
// least 15 minutes, prompts for the plan the network has now. Returns whether a
// hold was ended.
//
// It does nothing with no hold in force, since ending one also supersedes every
// connection dialed before it (connect/transport_client_limit.go). It ends at
// most one hold per ClientLimitRetryMinInterval, and the floor doubles with
// each retry in a row whose redial the platform refused again, up to the
// platform's shortest hold (connect.ClientLimitBackoffTimeout, 15 minutes), so
// an app that calls it in a loop cannot turn the hold into a retry storm
// against the limit. Quiet for twice that long, the floor starts over.
func (self *DeviceLocal) RetryClientLimit() bool {
	provider, closed := func() (*deviceLocalProvider, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.provider, self.closed
	}()
	if closed || provider == nil {
		return false
	}
	if !provider.retryClientLimit(
		time.Now(),
		self.settings.ClientLimitRetryMinInterval,
		connect.ClientLimitBackoffTimeout,
	) {
		return false
	}
	self.GetApi().RequestJwtRefresh()
	return true
}

func (self *DeviceLocal) clientLimitStatusChanged(status *ClientLimitStatus) {
	for _, listener := range self.clientLimitStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ClientLimitStatusChanged(cloneClientLimitStatus(status))
		})
	}
}

// watchClientLimitStatus emits each change of the client limit status: the
// provider's hold, and the plan the prompt follows. Both notify only on an
// actual change, and an unchanged readout is not emitted again, so a plan
// change with no hold in force emits nothing. update and planUpdate are armed
// by the constructor before this goroutine starts, so a hold that starts the
// instant the constructor returns still wakes it.
func (self *DeviceLocal) watchClientLimitStatus(
	provider *deviceLocalProvider,
	update chan struct{},
	planUpdate chan struct{},
) {
	emitted := noneClientLimitStatus()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-update:
		case <-planUpdate:
		}
		var hold connect.ClientLimitStatus
		hold, update = provider.clientLimitStatus()
		var plan clientLimitPlan
		plan, planUpdate = self.clientLimitPlanValue.Get()
		status := newClientLimitStatus(hold, plan)
		if *status == *emitted {
			continue
		}
		if status.Status == ClientLimitStatusExceeded && emitted.Status != ClientLimitStatusExceeded {
			// a new hold: refresh the plan its prompt follows, once per hold
			self.GetApi().RequestJwtRefresh()
		}
		emitted = status
		// contain a panic to the emit: a failed emit must never end the watch
		connect.HandleError(func() {
			self.clientLimitStatusChanged(status)
		})
	}
}

// clientLimitProClaim is the pro claim of a client jwt, false when it has none
// or does not parse.
func clientLimitProClaim(byJwt string) bool {
	return clientLimitPlanClaims(byJwt).pro
}

// clientLimitPlanClaims is the plan a client jwt's claims give the client
// limit prompt: its pro claim, false when it has none, and its
// client_limit_upgrade claim, unknown when it has none. A jwt that does not
// parse has neither.
func clientLimitPlanClaims(byJwt string) clientLimitPlan {
	if byJwt == "" {
		return clientLimitPlan{}
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return clientLimitPlan{}
	}
	plan := clientLimitPlan{}
	if pro, ok := claims["pro"].(bool); ok {
		plan.pro = pro
	}
	if upgrade, ok := claims["client_limit_upgrade"].(bool); ok {
		if upgrade {
			plan.upgrade = clientLimitUpgradeRaises
		} else {
			plan.upgrade = clientLimitUpgradeNone
		}
	}
	return plan
}
