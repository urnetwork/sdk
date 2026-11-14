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
// The mint's token names no sidecar. POST /sidecar/bind answers it re-minted with the bound sidecar_id (QD-040,
// QD-797), and the core hands that re-mint to the session (AdoptByJwt, QD-805) at the child's enrollment, before the
// generation starts, and at a host's Reregister. From then on the session presents and refreshes the re-mint, and each
// refresh keeps the claim of the token it presents and adds none, so the claim sockets reads survives the session's
// refreshes. A session that kept the mint's token would hand the core a token naming no sidecar at every refresh, and
// sockets would deny the sidecar, the closed direction.
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

// The session has closed, so it takes no token.
var errSidecarSessionClosed = errors.New("the sidecar's api session has closed")

// A token offered as bind's re-mint is not a re-mint of the session's token.
var errSidecarRemintMismatch = errors.New("the re-minted token names another network, client or device than the session's, or no sidecar")

// The core finds a source's optional interfaces by assertion at run time, where a missing method only falls back, a
// missing AdoptByJwt to a token naming no sidecar, so the session's are each checked here, at compile time.
var (
	_ sidecar.SidecarTokenSource       = (*sidecarApiSession)(nil)
	_ sidecar.SidecarTokenSourceSigner = (*sidecarApiSession)(nil)
	_ sidecar.SidecarTokenSourceLogout = (*sidecarApiSession)(nil)
	_ sidecar.SidecarTokenSourceRemint = (*sidecarApiSession)(nil)
)

// The child token's session.
type sidecarApiSession struct {
	api *Api

	stateLock sync.Mutex
	// the child's token as this session last held it: installed at the mint, replaced by bind's re-mint, then each
	// refresh's
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

// Holds byJwt and then gives it to the session's Api, reporting whether it did: the only token this session presents,
// the mint's answer and then bind's re-mint (AdoptByJwt). A closed session takes none, so a close that came first
// leaves nothing held and its Api's token as it was.
func (self *sidecarApiSession) install(byJwt string) bool {
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.byJwt = byJwt
		return true
	}()
	if !held {
		return false
	}
	self.api.SetByJwt(byJwt)
	return true
}

// Takes bind's re-mint of the child's token (QD-797, QD-805) in place of the token the session holds: the same
// network, client and device, with the bound sidecar_id, which the session presents and refreshes from then on. With
// the refresher running (a host's Reregister), installing it refreshes it at once, as any new token is (Api.SetByJwt),
// and that refresh keeps the claim. Refuses, keeping the session's own token, when the session has closed, holds no
// token (none installed yet, or one a confirmed rejection cleared, which a re-mint must not bring back), or byJwt is
// not a re-mint of the token it holds. See sidecar.SidecarTokenSourceRemint.
func (self *sidecarApiSession) AdoptByJwt(byJwt string) error {
	heldByJwt, err := func() (string, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		switch {
		case self.closed:
			return "", errSidecarSessionClosed
		case self.byJwt == "":
			return "", errSidecarSessionEmpty
		}
		return self.byJwt, nil
	}()
	if err != nil {
		return err
	}
	// a re-mint of the held token names its network, client and device, and a sidecar in its sidecar_id claim. Both
	// are read unverified, as the core reads the re-mint and the sdk a refreshed token: api verifies the signature, and
	// this keeps the session from presenting another client's token
	isRemint := func() bool {
		heldIdentity, heldOk := parseClientJwtIdentity(heldByJwt)
		identity, ok := parseClientJwtIdentity(byJwt)
		if !heldOk || !ok || heldIdentity != identity {
			return false
		}
		heldNetworkId, heldOk := credentialNetworkId(heldByJwt)
		networkId, ok := credentialNetworkId(byJwt)
		if !heldOk || !ok || heldNetworkId != networkId {
			return false
		}
		claims, ok := unverifiedCredentialClaims(byJwt)
		if !ok {
			return false
		}
		claimed, _ := claims["sidecar_id"].(string)
		sidecarId, err := sidecar.ParseId(claimed)
		return err == nil && sidecarId != (sidecar.Id{})
	}
	if !isRemint() {
		return errSidecarRemintMismatch
	}
	if self.api.GetByJwt() == "" {
		return errSidecarSessionEmpty
	}
	if !self.install(byJwt) {
		return errSidecarSessionClosed
	}
	return nil
}

// Whether the session holds a token: one it was given, which its Api still presents, so a session whose token a
// confirmed rejection cleared is no source for a later generation.
func (self *sidecarApiSession) holdsToken() bool {
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.byJwt != "" && !self.closed
	}()
	return held && self.api.GetByJwt() != ""
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
	if self.api.GetByJwt() == "" {
		return errSidecarSessionEmpty
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
