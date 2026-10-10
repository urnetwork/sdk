//go:build !ios_extension

package sdk

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/v2026"
)

func TestClientSessionActionTerminalFailureIsVisible(t *testing.T) {
	for _, state := range []string{"cancelled", "failed"} {
		t.Run(state, func(t *testing.T) {
			api := newApi(t.Context(), nil, "https://api.test")
			defer api.Close()
			api.SetByJwt(taggedSessionTestJwt(t, "session", ""))
			vc := NewClientSessionViewControllerWithApi(t.Context(), api)
			defer vc.Close()
			completed := make(chan struct{})
			vc.testingAfterAction = func() { close(completed) }
			api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
				var args RevokeOtherNetworkSessionsArgs
				if err := json.Unmarshal(body, &args); err != nil {
					return nil, err
				}
				return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: state, State: state})
			})
			vc.RevokeOtherSessions()
			select {
			case <-completed:
			case <-time.After(5 * time.Second):
				t.Fatal("action did not finish")
			}
			action := vc.GetSnapshot().BulkAction
			if action == nil || action.Status != state || action.State != state || action.Loading || action.Pending || action.Error == nil || action.Error.Retryable {
				t.Fatalf("terminal failure presented as success or pending: %+v", action)
			}
		})
	}
}

func TestClientSessionPendingRecoveryUsesSameSessionRenewal(t *testing.T) {
	for _, state := range []string{"enforced", "cancelled", "failed"} {
		t.Run(state, func(t *testing.T) {
			api := newApi(t.Context(), nil, "https://api.test")
			defer api.Close()
			previous := credentialTestJwt(t, gojwt.MapClaims{"network_id": credentialTestNetworkId, "user_id": credentialTestUserId, "session_id": "session", "iat": 1, "exp": 100})
			renewed := credentialTestJwt(t, gojwt.MapClaims{"network_id": credentialTestNetworkId, "user_id": credentialTestUserId, "session_id": "session", "iat": 2, "exp": 200})
			store := newRenewalTestLocalState(t, t.TempDir(), previous, "")
			api.SetByJwt(previous)
			api.SetNetworkCredentialStore(store)
			vc := NewClientSessionViewControllerWithApi(t.Context(), api)
			defer vc.Close()
			target := api.captureNetworkTarget()
			operationId := newId(connect.NewId())
			vc.stateLock.Lock()
			vc.state.BulkAction = &ClientSessionAction{OperationId: operationId, Pending: true, target: target}
			vc.stateLock.Unlock()
			kept, committed, err := api.commitNetworkRenewal(target, renewed)
			if err != nil || !committed || kept != renewed || store.GetByJwt() != renewed {
				t.Fatalf("renewal did not commit: %q %v %v", kept, committed, err)
			}
			var sends atomic.Int32
			api.setHttpGetRaw(func(_ context.Context, url string, token string) ([]byte, error) {
				sends.Add(1)
				if token != renewed || !strings.HasSuffix(url, "/network/session-operations/"+operationId.String()) {
					t.Errorf("recovery used wrong credential or operation: %q %q", token, url)
				}
				return json.Marshal(&SessionOperationResult{OperationId: operationId, Status: state, State: state})
			})
			vc.recoverPending()
			action := vc.GetSnapshot().BulkAction
			if sends.Load() != 1 || action.Pending || action.Status != state || action.State != state {
				t.Fatalf("renewed session did not recover pending operation: %+v, requests=%d", action, sends.Load())
			}
			if state == "enforced" {
				if action.Error != nil {
					t.Fatal(action.Error)
				}
			} else if action.Error == nil || action.Error.Retryable {
				t.Fatalf("recovery hid terminal failure: %+v", action)
			}
		})
	}
}

func TestClientSessionPendingRecoveryCannotAdoptNewLogin(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	// Even a new login with identical unverified session claims changes the
	// credential generation; it cannot resume operations admitted before it.
	token := taggedSessionTestJwt(t, "session", "")
	api.SetByJwt(token)
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	defer vc.Close()
	target := api.captureNetworkTarget()
	vc.stateLock.Lock()
	vc.state.BulkAction = &ClientSessionAction{OperationId: newId(connect.NewId()), Pending: true, target: target}
	vc.stateLock.Unlock()
	api.SetByJwt("")
	api.SetByJwt(token)
	var sends atomic.Int32
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		sends.Add(1)
		return nil, nil
	})
	vc.recoverPending()
	if sends.Load() != 0 {
		t.Fatal("old pending operation sent under a new login")
	}
	if _, current := api.currentNetworkTarget(target); current {
		t.Fatal("new login adopted an old pending operation target")
	}
}
