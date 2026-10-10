package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
)

// Explicit sign-out always clears the captured local login. Confirmation means
// the server cutoff was observed; an offline result never claims revocation.
type SessionSignOutResult struct {
	OperationId         *Id  `json:"operation_id"`
	RevocationConfirmed bool `json:"revocation_confirmed"`
	CredentialCleared   bool `json:"credential_cleared"`
}

type SessionSignOutCallback interface {
	Result(*SessionSignOutResult, error)
}

func (self *Api) SignOut(callback SessionSignOutCallback) {
	target := self.captureNetworkTarget()
	ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
	go connect.HandleError(func() { defer cancel(); self.signOutCaptured(ctx, target, callback) })
}
func (self *Api) signOutCaptured(ctx context.Context, target networkRenewalTarget, callback SessionSignOutCallback) {
	operationId := NewId()
	result := &SessionSignOutResult{OperationId: operationId}
	if claims, err := connect.ParseByJwtUnverified(target.byJwt); err == nil && claims.SessionId != nil {
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
		result.CredentialCleared = self.rejectNetworkCredential(current)
	}
	if !result.CredentialCleared && target.byJwt != "" {
		self.mutex.Lock()
		result.CredentialCleared = self.networkByJwt == "" && self.networkByJwtRejected == target.byJwt
		self.mutex.Unlock()
	}
	callback.Result(result, self.GetCredentialPersistenceError())
}
