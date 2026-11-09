//go:build !ios_extension

package sdk

// The plan decision of SubscriptionBalanceViewController, exported for the
// apps that read the subscription balance themselves (the desktop stores and
// the mobile view models): whether the network is Pro, and when to refresh a
// jwt whose `pro` claim disagrees with the server.

import (
	"sync"
)

// Decides Pro and the jwt refresh the same on every platform. The app hands
// it the jwt it reads its claims from, at sign in and after every jwt refresh
// (SetJwt), and the plan state of every balance it loads (BalanceLoaded),
// which answers whether to refresh the jwt now.
//
//   - Pro is the server's answer (current_subscription is set) once a balance
//     has loaded, and the jwt's `pro` claim before that. The claim is baked in
//     when the jwt is issued and goes stale on an upgrade and on a lapse
//     alike, so a jwt that arrives after a load never moves Pro back to its
//     claim.
//
//   - A claim that disagrees with the server asks for one refresh per jwt.
//     The jwt the apps read (LocalState's, the network's sign-in token) renews
//     on its own schedule, while the refresh renews the device's client token
//     and leaves it as it was. Asking again for the same jwt refreshed on
//     every balance read, and a store that read the balance again when Pro
//     changed looped as fast as the api answered (support inbox 8211). A new
//     jwt that still disagrees asks once more.
//
//   - No jwt (a sign out), or a jwt of another network, starts over: the
//     server's answer belonged to the session before.
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

	// a refresh was asked for byJwt
	refreshRequested bool
}

func NewSubscriptionProTracker() *SubscriptionProTracker {
	return &SubscriptionProTracker{}
}

// Takes the jwt the app reads its claims from, at sign in and after every jwt
// refresh, and reports whether GetIsPro changed. Once a balance has loaded,
// only no jwt or a jwt of another network changes it. The same jwt again
// changes nothing, and in particular does not ask for another refresh.
func (self *SubscriptionProTracker) SetJwt(byJwt string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if byJwt == self.byJwt {
		return false
	}
	previousPro := self.isProWithLock()
	jwtNetworkId := parseJwtNetworkId(byJwt)
	if byJwt == "" || jwtNetworkId != self.jwtNetworkId {
		self.loaded = false
		self.serverIsPro = false
	}
	self.byJwt = byJwt
	self.jwtPro, _ = parseJwtProGuestClaims(byJwt)
	self.jwtNetworkId = jwtNetworkId
	self.refreshRequested = false
	return previousPro != self.isProWithLock()
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
	if self.byJwt == "" || serverIsPro == self.jwtPro || self.refreshRequested {
		return false
	}
	self.refreshRequested = true
	return true
}

// The plan state to show: the server's answer once a balance has loaded, else
// the jwt's `pro` claim.
func (self *SubscriptionProTracker) GetIsPro() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.isProWithLock()
}

// caller holds stateLock
func (self *SubscriptionProTracker) isProWithLock() bool {
	if self.loaded {
		return self.serverIsPro
	}
	return self.jwtPro
}

// Forgets the jwt and the server's answer, for a sign out.
func (self *SubscriptionProTracker) Reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.byJwt = ""
	self.jwtPro = false
	self.jwtNetworkId = ""
	self.loaded = false
	self.serverIsPro = false
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
