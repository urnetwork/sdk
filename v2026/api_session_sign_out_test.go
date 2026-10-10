package sdk

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type sessionSignOutCallbackFunc func(*SessionSignOutResult, error)

func (self sessionSignOutCallbackFunc) Result(result *SessionSignOutResult, err error) {
	self(result, err)
}
func TestExplicitSessionSignOutBoundedOfflineAndGeneration(t *testing.T) {
	for _, newLogin := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "new-login"}[newLogin], func(t *testing.T) {
			api := newApi(t.Context(), nil, "https://api.test")
			defer api.Close()
			token := taggedSessionTestJwt(t, NewId().String(), "")
			api.SetByJwt(token)
			target := api.captureNetworkTarget()
			entered, release := make(chan struct{}), make(chan struct{})
			api.setHttpPostRaw(func(ctx context.Context, _ string, body []byte, _ string) ([]byte, error) {
				close(entered)
				<-release
				return nil, context.Canceled
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan *SessionSignOutResult, 1)
			go api.signOutCaptured(ctx, target, sessionSignOutCallbackFunc(func(result *SessionSignOutResult, err error) {
				if err != nil {
					t.Error(err)
				}
				done <- result
			}))
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("sign-out not submitted")
			}
			newer := taggedSessionTestJwt(t, NewId().String(), "")
			if newLogin {
				api.SetByJwt(newer)
			}
			cancel()
			select {
			case result := <-done:
				if result.RevocationConfirmed || result.CredentialCleared == newLogin {
					t.Fatal("offline/late sign-out misreported", result)
				}
			case <-time.After(time.Second):
				t.Fatal("offline sign-out waited on an uncooperative request")
			}
			close(release)
			if newLogin && api.GetByJwt() != newer || !newLogin && api.HasNetworkCredential() {
				t.Fatal("sign-out cleared wrong generation")
			}
		})
	}
}
func TestExplicitSessionSignOutConfirmsServerCutoff(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	api.SetByJwt(taggedSessionTestJwt(t, NewId().String(), ""))
	api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
		var args RevokeNetworkSessionArgs
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, State: "enforced", Status: "revoked"})
	})
	done := make(chan *SessionSignOutResult, 1)
	api.SignOut(sessionSignOutCallbackFunc(func(result *SessionSignOutResult, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- result
	}))
	select {
	case result := <-done:
		if !result.RevocationConfirmed || !result.CredentialCleared {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("sign-out not completed")
	}
}
