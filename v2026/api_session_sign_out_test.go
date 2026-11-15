package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
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

// An explicit sign-out removes the device's own client before the session
// cutoff, with the network credential the cutoff then refuses. Sign-out used
// to revoke the session only, which left the client active: with
// enforce_concurrent_clients on, each sign-in's abandoned client counts
// toward the network's cap of active top-level clients (server
// LimitTopLevelClientIdsPerNetwork) until the idle reap, about 30 days later.
// A client of another network is never removed, a failed removal does not
// hold up the cutoff, and an API that serves no client token removes nothing.
func TestExplicitSessionSignOutRemovesDeviceClientFirst(t *testing.T) {
	type post struct {
		path    string
		byJwt   string
		removed *Id
	}
	signOut := func(t *testing.T, deviceByJwt string, removeFails bool) (*SessionSignOutResult, []post, string) {
		api := newApi(t.Context(), nil, "https://api.test")
		defer api.Close()
		networkByJwt := taggedSessionTestJwt(t, NewId().String(), "")
		api.SetByJwt(networkByJwt)
		if deviceByJwt != "" {
			// a device installs its client token beside the kept network credential
			// (keepNetworkByJwtForDeviceWithLock)
			func() {
				api.authMutationLock.Lock()
				defer api.authMutationLock.Unlock()
				api.mutex.Lock()
				defer api.mutex.Unlock()
				api.byJwt = deviceByJwt
			}()
		}
		var postsLock sync.Mutex
		posts := []post{}
		api.setHttpPostRaw(func(_ context.Context, url string, body []byte, byJwt string) ([]byte, error) {
			path := strings.TrimPrefix(url, "https://api.test")
			p := post{path: path, byJwt: byJwt}
			if path == "/network/remove-client" {
				var args RemoveNetworkClientArgs
				if err := json.Unmarshal(body, &args); err != nil {
					return nil, err
				}
				p.removed = args.ClientId
			}
			func() {
				postsLock.Lock()
				defer postsLock.Unlock()
				posts = append(posts, p)
			}()
			switch path {
			case "/network/remove-client":
				if removeFails {
					return nil, errors.New("remove-client unavailable")
				}
				return json.Marshal(&RemoveNetworkClientResult{})
			case "/network/revoke-session":
				var args RevokeNetworkSessionArgs
				if err := json.Unmarshal(body, &args); err != nil {
					return nil, err
				}
				return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, State: "enforced", Status: "revoked"})
			}
			return nil, errors.New("unexpected post " + path)
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
			postsLock.Lock()
			defer postsLock.Unlock()
			return result, slices.Clone(posts), networkByJwt
		case <-time.After(time.Second):
			t.Fatal("sign-out not completed")
			return nil, nil, ""
		}
	}

	t.Run("device client", func(t *testing.T) {
		clientId := NewId()
		result, posts, networkByJwt := signOut(t, taggedSessionTestJwt(t, NewId().String(), clientId.String()), false)
		if len(posts) != 2 || posts[0].path != "/network/remove-client" || posts[1].path != "/network/revoke-session" {
			t.Fatal("sign-out did not remove the device's client before the cutoff", posts)
		}
		if posts[0].removed == nil || *posts[0].removed != *clientId || posts[0].byJwt != networkByJwt {
			t.Fatal("sign-out removed the wrong client, or with the wrong credential", posts[0])
		}
		if !result.ClientRemoved || !result.RevocationConfirmed || !result.CredentialCleared {
			t.Fatal(result)
		}
	})
	t.Run("removal fails", func(t *testing.T) {
		result, posts, _ := signOut(t, taggedSessionTestJwt(t, NewId().String(), NewId().String()), true)
		if len(posts) != 2 || posts[1].path != "/network/revoke-session" {
			t.Fatal("a failed removal held up the cutoff", posts)
		}
		if result.ClientRemoved || !result.RevocationConfirmed || !result.CredentialCleared {
			t.Fatal(result)
		}
	})
	t.Run("client of another network", func(t *testing.T) {
		otherByJwt := credentialTestJwt(t, gojwt.MapClaims{
			"network_id": NewId().String(),
			"user_id":    credentialTestUserId,
			"session_id": NewId().String(),
			"client_id":  NewId().String(),
			"device_id":  credentialTestDeviceId,
		})
		result, posts, _ := signOut(t, otherByJwt, false)
		if len(posts) != 1 || posts[0].path != "/network/revoke-session" || result.ClientRemoved {
			t.Fatal("sign-out removed a client of another network", posts, result)
		}
	})
	t.Run("no client token", func(t *testing.T) {
		result, posts, _ := signOut(t, "", false)
		if len(posts) != 1 || posts[0].path != "/network/revoke-session" || result.ClientRemoved {
			t.Fatal("sign-out removed a client the API does not serve", posts, result)
		}
	})
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
