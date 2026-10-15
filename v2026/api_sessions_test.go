package sdk

import (
	"context"
	"errors"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/v2026"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func taggedSessionTestJwt(t *testing.T, session, client string) string {
	claims := gojwt.MapClaims{"network_id": credentialTestNetworkId, "user_id": credentialTestUserId, "session_id": session}
	if client != "" {
		claims["client_id"] = client
		claims["device_id"] = credentialTestDeviceId
	}
	return credentialTestJwt(t, claims)
}
func TestSessionRejectionMalformedIdentityNeverMatches(t *testing.T) {
	for _, value := range []any{[]any{"network"}, map[string]any{"user": "id"}, float64(1), nil} {
		token := credentialTestJwt(t, gojwt.MapClaims{"session_id": "sid", "network_id": value, "user_id": value})
		if sameTaggedSession(token, token) {
			t.Fatal("malformed unverified identity was positive evidence")
		}
	}
	if sameTaggedSession(credentialTestNetworkJwt(t, credentialTestNetworkId), credentialTestNetworkJwt(t, credentialTestNetworkId)) {
		t.Fatal("legacy identity counted as session proof")
	}
}
func TestSessionRejectionPersistenceAndGeneration(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "related"}[same], func(t *testing.T) {
			api := newApi(t.Context(), nil, "https://api.test")
			defer api.Close()
			network := taggedSessionTestJwt(t, "session-a", "")
			clientSession := "session-b"
			if same {
				clientSession = "session-a"
			}
			client := taggedSessionTestJwt(t, clientSession, credentialTestClientId)
			store := newRenewalTestLocalState(t, t.TempDir(), network, client)
			api.SetByJwt(network)
			api.SetNetworkCredentialStore(store)
			api.mutex.Lock()
			api.byJwt = client
			api.mutex.Unlock()
			var logout, account atomic.Int32
			api.AddAuthLogoutListener(authLogoutListenerFunc(func() { logout.Add(1) }))
			api.AddAccountSignInRequiredListener(accountSignInRequiredFunc(func() { account.Add(1) }))
			target := api.captureNetworkTarget()
			if !api.rejectNetworkCredential(target, "") || api.rejectNetworkCredential(target, "") {
				t.Fatal("rejection was not exactly once")
			}
			if store.GetByJwt() != "" || api.HasNetworkCredential() {
				t.Fatal("rejected network credential retained")
			}
			if same {
				if logout.Load() != 1 || api.GetByJwt() != "" || store.GetByClientJwt() != "" {
					t.Fatal("related session not cleared")
				}
			} else if account.Load() != 1 || logout.Load() != 0 || api.GetByJwt() != client || store.GetByClientJwt() != client {
				t.Fatal("unrelated device destroyed")
			}
			newer := taggedSessionTestJwt(t, "new-login", "")
			api.SetByJwt(newer)
			if api.rejectNetworkCredential(target, "") || api.GetByJwt() != newer {
				t.Fatal("old response rejected new login")
			}
		})
	}
}

type accountSignInRequiredFunc func()

func (self accountSignInRequiredFunc) AccountSignInRequired() { self() }
func TestSessionRejectionStorageFailureReportedAndFenced(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	jwt := taggedSessionTestJwt(t, "session", "")
	dir := t.TempDir()
	store := newRenewalTestLocalState(t, dir, jwt, "")
	api.SetByJwt(jwt)
	api.SetNetworkCredentialStore(store)
	// Keep reads valid, but make the atomic credential destination a directory.
	path := filepath.Join(store.localStorageDir, localAuthStateFileName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	target := api.captureNetworkTarget()
	if !api.rejectNetworkCredential(target, "") {
		t.Fatal("not rejected")
	}
	if !errors.Is(api.GetCredentialPersistenceError(), ErrCredentialPersistence) || api.HasNetworkCredential() {
		t.Fatal("persistence failure hidden")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	api.mutex.Lock()
	rejected := api.networkByJwtRejected
	api.mutex.Unlock()
	if rejected != jwt {
		t.Fatal("failed persistence erased re-adoption fence")
	}
}
func TestSessionActionNeverUsesNewAccountAfterAdmission(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	api.SetByJwt(taggedSessionTestJwt(t, "old-session", ""))
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	defer vc.Close()
	admitted := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	vc.testingBeforeSubmit = func() { close(admitted); <-release }
	vc.testingAfterAction = func() { close(completed) }
	var sends atomic.Int32
	api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		sends.Add(1)
		return nil, errors.New("unexpected request")
	})
	vc.RevokeOtherSessions()
	<-admitted
	api.SetByJwt(taggedSessionTestJwt(t, "new-session", ""))
	close(release)
	<-completed
	// Wait for the action goroutine to leave the admission hook, then directly
	// prove its captured old target fails and cannot select the new credential.
	vc.stateLock.Lock()
	old := vc.state.BulkAction.target
	vc.stateLock.Unlock()
	if _, err := api.sessionOperationFor(t.Context(), "/network/revoke-other-sessions", &RevokeOtherNetworkSessionsArgs{OperationId: newId(connect.NewId())}, old); !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatal(err)
	}
	if sends.Load() != 0 {
		t.Fatal("old operation sent under a new account")
	}
}
func TestSessionOperationAcceptsPendingAndKeepsStableRequestIdentity(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	api.SetByJwt(taggedSessionTestJwt(t, "session", ""))
	id := newId(connect.NewId())
	api.setHttpPostRaw(func(_ context.Context, _ string, _ []byte, byJwt string) ([]byte, error) {
		return nil, &connect.HttpStatusError{StatusCode: http.StatusAccepted, Body: []byte(`{"operation_id":"` + id.String() + `","status":"pending","state":"prepared"}`)}
	})
	result, err := api.sessionOperation(t.Context(), "/network/revoke-other-sessions", &RevokeOtherNetworkSessionsArgs{OperationId: id})
	if err != nil || result.Status != "pending" || result.OperationId.Cmp(id) != 0 {
		t.Fatal(result, err)
	}
}
func TestClientSessionControllerCloseCancelsAndSnapshotsOwnMetadata(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	api.SetByJwt(taggedSessionTestJwt(t, "session", ""))
	started := make(chan struct{})
	cancelled := make(chan struct{})
	api.setHttpGetRaw(func(ctx context.Context, _ string, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	vc.state.Sessions.Add(&NetworkSessionInfo{SessionId: newId(connect.NewId()), LastUsed: &SessionLastUsed{City: "Chicago"}})
	snapshot := vc.GetSnapshot()
	snapshot.Sessions.Get(0).LastUsed.City = "changed"
	if vc.GetSnapshot().Sessions.Get(0).LastUsed.City != "Chicago" {
		t.Fatal("snapshot aliases mutable metadata")
	}
	vc.Start()
	<-started
	vc.Close()
	vc.Start()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel request")
	}
	if len(api.networkSessionsListeners.Get()) != 0 {
		t.Fatal("closed controller leaked listener")
	}
}

func TestSessionMetadataAndHintsCrossDeviceRpc(t *testing.T) {
	local, remote := testing_newSyncedDeviceLocalRemote(t, t.Context())
	api := remote.GetApi()
	api.SetByJwt(taggedSessionTestJwt(t, "rpc-session", ""))
	api.SetClientInfo(NewClientInfo("android", "1.2.3"))
	observed := make(chan connect.ClientInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case observed <- connect.ClientInfoFromHeader(r.Header):
		default:
		}
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}))
	defer server.Close()
	if _, err := remote.httpGetRaw(api.clientInfoContext(t.Context()), server.URL, api.GetByJwt()); err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-observed:
		if info.DeviceType != "android" || info.AppVersion != "1.2.3" {
			t.Fatal(info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RPC request lost metadata")
	}
	hints := make(chan *NetworkSessionsRevision, 1)
	sub := api.AddNetworkSessionsChangeListener(sessionHintTestListener(func(revision *NetworkSessionsRevision) {
		select {
		case hints <- revision:
		default:
		}
	}))
	defer sub.Close()
	local.GetApi().networkSessionsChanged(&NetworkSessionsRevision{Generation: "new-incarnation", EventId: 17})
	select {
	case hint := <-hints:
		if hint.Generation != "new-incarnation" || hint.EventId != 17 {
			t.Fatal(hint)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RPC dropped typed session revision")
	}
	// Typed RPC request shape itself is copied rather than JSON text interpreted
	// by an embedding app. The API metadata remains an immutable snapshot.
	request := &DeviceRemoteHttpRequest{ClientInfo: api.connectClientInfo()}
	api.SetClientInfo(NewClientInfo("ios", "2.0.0"))
	if request.ClientInfo.DeviceType != "android" || request.ClientInfo.AppVersion != "1.2.3" {
		t.Fatal("queued metadata changed generation")
	}
}

type sessionHintTestListener func(*NetworkSessionsRevision)

func (self sessionHintTestListener) NetworkSessionsChanged(revision *NetworkSessionsRevision) {
	self(revision)
}
