package sdk

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/connect"
)

// Explicit sign-out always clears the captured local login. Confirmation means
// the server cutoff was observed; an offline result never claims revocation.
// The signed-out device's own client is removed first, with the network
// credential that the cutoff then refuses: the server does not deactivate a
// client when its session is revoked, so an abandoned client would count
// toward the network's cap of active top-level clients until the idle reap.
type SessionSignOutResult struct {
	OperationId         *Id  `json:"operation_id"`
	RevocationConfirmed bool `json:"revocation_confirmed"`
	CredentialCleared   bool `json:"credential_cleared"`
	ClientRemoved       bool `json:"client_removed"`
}

type SessionSignOutCallback interface {
	Result(*SessionSignOutResult, error)
}

// The budgets of an explicit sign-out. Each step has its own, so a removal
// that takes its whole budget still leaves the cutoff its own: with one
// budget for both, a slow removal left the session unrevoked, still listed
// and still accepted on the server, after a sign-out that looked complete.
type apiSignOutSettings struct {
	// The removal of the device's own client. 1 s: an unanswered removal only
	// delays the cutoff; the idle reap removes the client in the end.
	// Non-positive skips the removal.
	removeClientTimeout time.Duration
	// The revocation of the session, the server cutoff. 2 s, after which the
	// local login is cleared without a confirmed cutoff. Non-positive skips
	// the revocation.
	revokeTimeout time.Duration
}

func defaultApiSignOutSettings() *apiSignOutSettings {
	return &apiSignOutSettings{
		removeClientTimeout: time.Second,
		revokeTimeout:       2 * time.Second,
	}
}

func (self *Api) SignOut(callback SessionSignOutCallback) {
	target := self.captureNetworkTarget()
	go connect.HandleError(func() { self.signOutCaptured(self.ctx, target, callback) })
}

// ctx bounds the whole sign-out (the API's lifetime); each step adds its own
// budget from the API's settings.
func (self *Api) signOutCaptured(ctx context.Context, target networkRenewalTarget, callback SessionSignOutCallback) {
	settings := self.signOutSettings
	operationId := NewId()
	result := &SessionSignOutResult{OperationId: operationId}
	// The device's client: the client token this API serves, else the one the
	// LocalState backing the network credential pairs with it. Only a client
	// of the network being signed out, by its unverified claims.
	deviceClientId := func() *Id {
		self.mutex.Lock()
		byJwt := self.byJwt
		self.mutex.Unlock()
		deviceByJwts := []string{byJwt}
		if target.store != nil {
			deviceByJwts = append(deviceByJwts, target.store.GetByClientJwt())
		}
		networkId, ok := credentialNetworkId(target.byJwt)
		if !ok {
			return nil
		}
		for _, deviceByJwt := range deviceByJwts {
			if !credentialNamesClient(deviceByJwt) {
				continue
			}
			if deviceNetworkId, ok := credentialNetworkId(deviceByJwt); !ok || deviceNetworkId != networkId {
				continue
			}
			claims, err := connect.ParseByJwtUnverified(deviceByJwt)
			if err != nil || claims.ClientId == (connect.Id{}) {
				continue
			}
			return newId(claims.ClientId)
		}
		return nil
	}
	if clientId := deviceClientId(); clientId != nil && 0 < settings.removeClientTimeout {
		ctx, cancel := context.WithTimeout(ctx, settings.removeClientTimeout)
		defer cancel()
		removed := make(chan bool, 1)
		go connect.HandleError(func() {
			current, ok := self.currentNetworkTarget(target)
			if !ok {
				removed <- false
				return
			}
			body, err := json.Marshal(&RemoveNetworkClientArgs{ClientId: clientId})
			if err != nil {
				removed <- false
				return
			}
			raw, err := self.getHttpPostRaw()(ctx, self.apiUrl+"/network/remove-client", body, current.byJwt)
			if err != nil {
				removed <- false
				return
			}
			var removeResult RemoveNetworkClientResult
			removed <- json.Unmarshal(raw, &removeResult) == nil && removeResult.Error == nil
		})
		// bounded by its own budget: an unanswered removal leaves the cutoff
		// below its whole budget
		select {
		case result.ClientRemoved = <-removed:
		case <-ctx.Done():
		}
	}
	if claims, err := connect.ParseByJwtUnverified(target.byJwt); err == nil && claims.SessionId != nil && 0 < settings.revokeTimeout {
		ctx, cancel := context.WithTimeout(ctx, settings.revokeTimeout)
		defer cancel()
		sid := newId(*claims.SessionId)
		replies := make(chan *SessionOperationResult, 1)
		go connect.HandleError(func() {
			operation, err := self.sessionOperationFor(ctx, "/network/revoke-session", &RevokeNetworkSessionArgs{SessionId: sid, OperationId: operationId}, target)
			if err != nil {
				operation = nil
			}
			replies <- operation
		})
		select {
		case operation := <-replies:
			result.RevocationConfirmed = operation != nil && (operation.State == "enforced" || operation.State == "complete" || operation.Status == "revoked" || operation.Status == "already_revoked")
		case <-ctx.Done():
		}
	}
	if current, ok := self.currentNetworkTarget(target); ok {
		// the user signed this session out: no cause
		result.CredentialCleared = self.rejectNetworkCredential(current, "")
	}
	if !result.CredentialCleared && target.byJwt != "" {
		self.mutex.Lock()
		result.CredentialCleared = self.networkByJwt == "" && self.networkByJwtRejected == target.byJwt
		self.mutex.Unlock()
	}
	callback.Result(result, self.GetCredentialPersistenceError())
}
