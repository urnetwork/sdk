//go:build !ios_extension

package sdk

// The plan decision of SubscriptionBalanceViewController, exported for the
// apps that read the subscription balance themselves (the desktop stores and
// the mobile view models): the plan to show (Pro, Free or not known yet),
// and when to refresh a jwt whose `pro` claim disagrees with the server.

import (
	"sync"
)

// The plan an app shows, from GetShownPlan (SubscriptionProTracker and
// SubscriptionBalanceViewController).
const (
	// the server's answer is Pro, or before the server has answered, the
	// jwt's `pro` claim is
	ShownPlanPro = "pro"
	// the server's answer is Free. Only the server says Free.
	ShownPlanFree = "free"
	// the server has not answered this session and the jwt's claim is not
	// Pro: the first balance read is on its way, or failed
	// (GetLastReadFailed). The app shows neither Free nor an upgrade prompt:
	// a loading placeholder, or a retry once a read has failed.
	ShownPlanUnknown = "unknown"
)

// Decides the plan and the jwt refresh the same on every platform. The app
// hands it the jwt it reads its claims from, at sign in and after every jwt
// refresh (SetJwt), the plan state of every balance it loads (BalanceLoaded),
// which answers whether to refresh the jwt now, and every balance read that
// failed (BalanceFailed).
//
//   - The plan (GetShownPlan) is the server's answer (current_subscription
//     is set: Pro, else Free) once a balance has loaded. A read that fails
//     after that keeps the answer, as the snapshot does: the poll retries.
//
//   - Before the server has answered, the plan is Pro when the jwt's `pro`
//     claim is, and unknown otherwise, while the first read is on its way and
//     after it failed: never Free. The claim is baked in when the jwt is
//     issued and stays Free after a purchase until the network jwt renews on
//     its own schedule, so a Free claim cannot say the network is Free. A Pro
//     claim was the server's answer when the jwt was issued; it can only be
//     stale after a lapse, which shows a lapsed network Pro until the server
//     answers, never a paying one Free. In December 2025 the balance read
//     failed under database load and the apps of the time showed a failed
//     read as Free; nine paying networks wrote in (support inbox 4074).
//
//   - GetIsPro is the plan being Pro. A jwt that arrives after a load never
//     moves the plan back to its claim.
//
//   - A claim that disagrees with the server asks for one refresh per jwt.
//     The jwt the apps read (LocalState's, the network's sign-in token) renews
//     on its own schedule, while the refresh renews the device's client token
//     and leaves it as it was. Asking again for the same jwt refreshed on
//     every balance read, and a store that read the balance again when Pro
//     changed looped as fast as the api answered (support inbox 8211). A new
//     jwt that still disagrees asks once more. A failed read asks for nothing.
//
//   - A jwt of another network starts over, as Reset does at a sign out: the
//     server's answer, and a failed read, belonged to the session before. An
//     empty jwt (a read that found none) changes nothing.
//
// Safe for concurrent use; an app normally calls it from its main thread.
type SubscriptionProTracker struct {
	stateLock sync.Mutex

	// the jwt the claim was read from, "" when none
	byJwt        string
	jwtPro       bool
	jwtNetworkId string

	// a balance loaded for this session
	loaded      bool
	serverIsPro bool
	// the last balance read of this session failed
	readFailed bool

	// a refresh was asked for byJwt
	refreshRequested bool
}

// A tracker with no jwt and no balance: the plan is unknown until a jwt or a
// balance arrives.
func NewSubscriptionProTracker() *SubscriptionProTracker {
	return &SubscriptionProTracker{}
}

// Takes the jwt the app reads its claims from, at sign in and after every jwt
// refresh, and reports whether GetShownPlan (and with it GetIsPro) changed.
// Once a balance has loaded, only a jwt of another network changes it. The
// same jwt again, or an empty one, changes nothing, and in particular does not
// ask for another refresh.
func (self *SubscriptionProTracker) SetJwt(byJwt string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if byJwt == "" || byJwt == self.byJwt {
		return false
	}
	previousPlan := self.planWithLock()
	jwtNetworkId := parseJwtNetworkId(byJwt)
	if jwtNetworkId != self.jwtNetworkId {
		self.loaded = false
		self.serverIsPro = false
		self.readFailed = false
	}
	self.byJwt = byJwt
	self.jwtPro, _ = parseJwtProGuestClaims(byJwt)
	self.jwtNetworkId = jwtNetworkId
	self.refreshRequested = false
	return previousPlan != self.planWithLock()
}

// Takes the plan state of a loaded balance (serverIsPro: current_subscription
// is set) and reports whether the app should refresh the jwt now: the claim
// disagrees, and no refresh was asked for this jwt yet. Without a jwt there is
// nothing to refresh.
func (self *SubscriptionProTracker) BalanceLoaded(serverIsPro bool) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.loaded = true
	self.serverIsPro = serverIsPro
	self.readFailed = false
	if self.byJwt == "" || serverIsPro == self.jwtPro || self.refreshRequested {
		return false
	}
	self.refreshRequested = true
	return true
}

// Takes a balance read that failed (an error, or no result), and reports
// whether GetLastReadFailed changed. The plan does not change: the server's
// last answer stands once a balance has loaded, and before that a failed read
// leaves the plan Pro by the claim or unknown, never Free. Asks for no refresh.
func (self *SubscriptionProTracker) BalanceFailed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	changed := !self.readFailed
	self.readFailed = true
	return changed
}

// The plan to show, one of the ShownPlan* values: the server's answer
// once a balance has loaded; before that Pro when the jwt's claim is, else
// unknown.
func (self *SubscriptionProTracker) GetShownPlan() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.planWithLock()
}

// The last balance read of this session failed, and none has loaded since.
// While the plan is unknown the app offers a retry when this is set, and a
// loading placeholder when it is not (the first read is on its way).
func (self *SubscriptionProTracker) GetLastReadFailed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readFailed
}

// The plan is Pro: the server's answer once a balance has loaded, else the
// jwt's `pro` claim.
func (self *SubscriptionProTracker) GetIsPro() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.planWithLock() == ShownPlanPro
}

// The plan to show, by the rule of GetShownPlan. The caller holds stateLock.
func (self *SubscriptionProTracker) planWithLock() string {
	switch {
	case self.loaded && self.serverIsPro:
		return ShownPlanPro
	case self.loaded:
		return ShownPlanFree
	case self.jwtPro:
		return ShownPlanPro
	default:
		return ShownPlanUnknown
	}
}

// Forgets the jwt, the server's answer and a failed read, for a sign out.
func (self *SubscriptionProTracker) Reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.byJwt = ""
	self.jwtPro = false
	self.jwtNetworkId = ""
	self.loaded = false
	self.serverIsPro = false
	self.readFailed = false
	self.refreshRequested = false
}

// The jwt's network_id claim, read without verifying the signature; "" when
// there is none.
func parseJwtNetworkId(byJwt string) string {
	claims, ok := unverifiedCredentialClaims(byJwt)
	if !ok {
		return ""
	}
	networkId, _ := claims["network_id"].(string)
	return networkId
}
