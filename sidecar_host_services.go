//go:build !ios_extension && !js

// The Api session an sdk host gives the core as its TokenSource (§18.7's JWT row, CORE-M8-01): a session of the
// device's Api, sharing its transports and none of its credentials, that holds only the sidecar's child token,
// strict_exp for 7 days (owner decision 131). The trap is the order (§7.4): a strict_exp refresh with no signer is not
// sent at all, so the session starts its refresher only once the core has handed it the ClientKey's DPoP signer
// (SetDpopSigner), and until then the core's own refresher keeps the token (D-CORE-8). Each refresh goes to the core,
// which re-seals the token in the keychain and installs it (Store()'s re-seal); a confirmed rejection the session's
// Api found current (IsAuthLogoutCurrent at the transition) goes to the core as a logout of the token it held, which
// the core reads as session_ended for a revoked session and parent_lost otherwise, dropping one that names an older
// generation's client (D-SDK-14).
//
// The session holds a token only once EnrollWithDevice minted the child in this process. After a restart the child's
// token is sealed in the core's keychain and no host holds it, so the host offers no TokenSource and the core's own
// refresher refreshes with its own proof, the contract's fallback (api_token_source.go). The network credential and
// the device's token never enter the session (I-6): it is minted from the device's token by a request of the
// device's own Api, and only the child's answer is kept.
//
// Safe for concurrent use.

package sdk

import (
	"errors"
	"sync"

	"github.com/urnetwork/sidecar"
)

// The session holds no token: the core refreshes on its own.
var errSidecarSessionEmpty = errors.New("the sidecar's api session holds no token")

// The child token's session.
type sidecarApiSession struct {
	api *Api

	stateLock sync.Mutex
	// the child's token as this session last held it: installed at the mint, then each refresh's
	byJwt string
	// the refresher runs, since a signer is installed
	refreshing bool
	closed     bool
	// the session's own subscriptions, ended at close
	subs []Sub
}

// A session over api, a session of the device's Api with no credential of its own.
func newSidecarApiSession(api *Api) *sidecarApiSession {
	self := &sidecarApiSession{api: api}
	// each refreshed token is the one a later logout names
	self.subs = append(self.subs, api.AddJwtRefreshListener(sidecarJwtRefreshListener(func(byJwt string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = byJwt
	})))
	return self
}

// Holds the child's token, from the mint's answer: the only token this session ever presents.
func (self *sidecarApiSession) install(byJwt string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = byJwt
	}()
	self.api.SetByJwt(byJwt)
}

// Whether the session holds a token.
func (self *sidecarApiSession) holdsToken() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byJwt != "" && !self.closed
}

// The child's token now. See sidecar.SidecarTokenSource.
func (self *sidecarApiSession) ByJwt() (string, error) {
	byJwt := self.api.GetByJwt()
	if byJwt == "" {
		return "", errSidecarSessionEmpty
	}
	return byJwt, nil
}

// Calls callback with each refreshed token. See sidecar.SidecarTokenSource.
func (self *sidecarApiSession) AddByJwtCallback(callback func(byJwt string)) func() {
	sub := self.api.AddJwtRefreshListener(sidecarJwtRefreshListener(callback))
	return sub.Close
}

// Installs the core's DPoP signer, and only then starts the refresher, so no refresh of the strict_exp token is ever
// sent without its proof (§7.4). A session holding no token refuses it, which leaves the core refreshing. See
// sidecar.SidecarTokenSourceSigner.
func (self *sidecarApiSession) SetDpopSigner(signer sidecar.SidecarDpopSigner) error {
	if signer == nil {
		return errors.New("a sidecar's session refreshes only with a signer")
	}
	start := false
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.byJwt == "" {
			return errSidecarSessionEmpty
		}
		start = !self.refreshing
		self.refreshing = true
		return nil
	}()
	if err != nil {
		return err
	}
	self.api.SetDpopSigner(signer)
	if start {
		self.api.StartJwtRefresh()
	}
	return nil
}

// Calls callback with each confirmed rejection of the session's token that its Api found current at the transition,
// with the token it held and the rejection's cause. A rejection a later token retired is dropped here. See
// sidecar.SidecarTokenSourceLogout.
func (self *sidecarApiSession) AddLogoutCallback(callback func(byJwt string, cause string)) func() {
	sub := self.api.AddAuthLogoutEventListener(sidecarAuthLogoutEventListener(func(generation int64, cause string) {
		if !self.api.IsAuthLogoutCurrent(generation) {
			return
		}
		byJwt := func() string {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.byJwt
		}()
		if byJwt != "" {
			callback(byJwt, cause)
		}
	}))
	return sub.Close
}

// Ends the session: its refresher and its subscriptions. The token goes with it.
func (self *sidecarApiSession) close() {
	subs := func() []Sub {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		self.closed = true
		subs := self.subs
		self.subs = nil
		return subs
	}()
	for _, sub := range subs {
		sub.Close()
	}
	self.api.SetDpopSigner(nil)
	self.api.Close()
}

// A refresh listener of a function.
type sidecarJwtRefreshListener func(byJwt string)

func (self sidecarJwtRefreshListener) JwtRefreshed(byJwt string) {
	self(byJwt)
}

// A logout event listener of a function.
type sidecarAuthLogoutEventListener func(generation int64, cause string)

func (self sidecarAuthLogoutEventListener) AuthLogoutEvent(generation int64, cause string) {
	self(generation, cause)
}
