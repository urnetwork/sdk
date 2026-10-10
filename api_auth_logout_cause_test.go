//go:build !ios_extension

// The trusted cause of a logout (api_auth_logout_cause.go): only the server's
// structured session_revoked 401 carries it, on every path that rejects a
// credential, read inside the logout listeners, never for an older login,
// across the device rpc, and in the session controller's error.
package sdk

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// The server's answer to a request whose session is revoked, as
// router.RaiseHttpError writes session.SessionError{Code: "session_revoked",
// Status: 401}: its HttpErrorResultBody marshaled, then a newline.
const testingSessionRevokedBody = `{"code":"session_revoked","error":"Session request could not be completed."}` + "\n"

const testingLogoutCauseStepLimit = 15 * time.Second

func testingWriteSessionRevoked(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(testingSessionRevokedBody))
}

// Records each AuthLogout with the cause read inside it, as an app reads it.
type testingLogoutRecorder struct {
	stateLock sync.Mutex
	causes    []string
	fired     chan struct{}
}

func newTestingLogoutRecorder() *testingLogoutRecorder {
	return &testingLogoutRecorder{fired: make(chan struct{}, 16)}
}

func (self *testingLogoutRecorder) listener(cause func() string) AuthLogoutListener {
	return authLogoutListenerFunc(func() {
		value := cause()
		self.stateLock.Lock()
		self.causes = append(self.causes, value)
		self.stateLock.Unlock()
		self.fired <- struct{}{}
	})
}

func (self *testingLogoutRecorder) observed() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.causes...)
}

func (self *testingLogoutRecorder) await(t *testing.T) {
	t.Helper()
	select {
	case <-self.fired:
	case <-time.After(testingLogoutCauseStepLimit):
		t.Fatal("no logout was published")
	}
}

func (self *testingLogoutRecorder) requireCauses(t *testing.T, want ...string) {
	t.Helper()
	got := self.observed()
	if len(got) != len(want) {
		t.Fatalf("logouts read causes %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("logouts read causes %q, want %q", got, want)
		}
	}
}

// A sign-in session id the unverified parser reads, as the server mints them.
func testingSessionId() string {
	return NewId().String()
}

// Every path that sends the API's network credential rejects it on the
// structured 401 with the cause set before the one logout.
func TestSessionRevokedRejectionSetsTheCauseBeforeOneLogout(t *testing.T) {
	operationId := NewId()
	for _, c := range []struct {
		name    string
		request func(api *Api) error
	}{
		{name: "get", request: func(api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkSessionsResult]) { api.GetNetworkSessions(cb) })
		}},
		{name: "post", request: func(api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SessionOperationResult]) {
				api.RevokeOtherNetworkSessions(&RevokeOtherNetworkSessionsArgs{}, cb)
			})
		}},
		{name: "operation status", request: func(api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SessionOperationResult]) {
				api.GetNetworkSessionOperation(operationId, cb)
			})
		}},
		{name: "stream post", request: func(api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*UploadLogsResult]) {
				api.postLogsZip("feedback", strings.NewReader("zip"), cb)
			})
		}},
	} {
		var requests atomic.Int32
		_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			testingWriteSessionRevoked(w)
		}))
		api.SetByJwt(taggedSessionTestJwt(t, testingSessionId(), ""))
		recorder := newTestingLogoutRecorder()
		api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))

		err := c.request(api)
		if !ConfirmedClientRefreshRejection(err) {
			t.Fatalf("%s: err = %v, want the server's 401", c.name, err)
		}
		recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
		if api.GetAuthLogoutCause() != AuthLogoutCauseSessionRevoked || api.GetByJwt() != "" || api.HasNetworkCredential() {
			t.Fatalf("%s: cause %q, credential %t after the rejection", c.name, api.GetAuthLogoutCause(), api.HasNetworkCredential())
		}
		// nothing is left to reject
		_ = c.request(api)
		recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
		if requests.Load() == 0 {
			t.Fatalf("%s: the request never reached the server", c.name)
		}
	}
}

// The client token refresh that a revoked transport triggers.
func TestClientRefreshSessionRevokedSetsTheCause(t *testing.T) {
	clientJwt := taggedSessionTestJwt(t, testingSessionId(), credentialTestClientId)
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" || r.Header.Get("Authorization") != "Bearer "+clientJwt {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		testingWriteSessionRevoked(w)
	}))
	api.SetByJwt(clientJwt)
	recorder := newTestingLogoutRecorder()
	api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))
	api.StartJwtRefresh()
	api.RequestJwtRefresh()

	recorder.await(t)
	recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
	if api.GetByJwt() != "" || api.GetAuthLogoutCause() != AuthLogoutCauseSessionRevoked {
		t.Fatal("the rejected client token or its cause was not kept")
	}
}

// The renewal of the network credential that a device keeps beside its
// client token, of the same sign-in session.
func TestNetworkRenewalSessionRevokedSetsTheCause(t *testing.T) {
	r := newRenewalTestApi(t)
	day := 24 * time.Hour
	session := testingSessionId()
	networkJwt := renewalTestJwt(t, gojwt.MapClaims{
		"network_id": credentialTestNetworkId, "user_id": credentialTestUserId, "session_id": session,
		"iat": renewalTestBaseTime.Add(-40 * day).Unix(), "exp": renewalTestBaseTime.Add(-10 * day).Unix(),
	})
	clientJwt := taggedSessionTestJwt(t, session, credentialTestClientId)
	localState := newRenewalTestLocalState(t, t.TempDir(), networkJwt, clientJwt)
	recorder := newTestingLogoutRecorder()
	r.api.AddAuthLogoutListener(recorder.listener(r.api.GetAuthLogoutCause))
	r.startDevice(t, localState, clientJwt)

	request := r.transport.takeRenewal(t, networkJwt)
	request.fail(&connect.HttpStatusError{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
		Body:       []byte(testingSessionRevokedBody),
	})
	r.requireRenewed(t)

	recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
	if r.api.HasNetworkCredential() || r.api.GetByJwt() != "" || localState.GetByJwt() != "" {
		t.Fatal("the revoked session kept a credential")
	}
}

// A generic 401, and any body that is not exactly the structured code, signs
// out with no cause. Another status with the code body signs nothing out.
func TestGenericRejectionHasNoCause(t *testing.T) {
	for _, c := range []struct {
		name        string
		status      int
		contentType string
		body        string
		logout      bool
	}{
		{name: "generic 401", status: http.StatusUnauthorized, contentType: "text/plain; charset=utf-8", body: "Not authorized.\n", logout: true},
		{name: "another code", status: http.StatusUnauthorized, contentType: "application/json", body: `{"code":"credential_rejected","error":"Session request could not be completed."}`, logout: true},
		{name: "duplicated code", status: http.StatusUnauthorized, contentType: "application/json", body: `{"code":"credential_rejected","code":"session_revoked"}`, logout: true},
		{name: "case-folded code", status: http.StatusUnauthorized, contentType: "application/json", body: `{"Code":"session_revoked"}`, logout: true},
		{name: "code not in an object", status: http.StatusUnauthorized, contentType: "application/json", body: `"session_revoked"`, logout: true},
		{name: "trailing json", status: http.StatusUnauthorized, contentType: "application/json", body: `{"code":"session_revoked"}{}`, logout: true},
		{name: "empty body", status: http.StatusUnauthorized, contentType: "application/json", body: "", logout: true},
		{name: "forbidden", status: http.StatusForbidden, contentType: "application/json", body: testingSessionRevokedBody, logout: false},
		{name: "unavailable", status: http.StatusServiceUnavailable, contentType: "application/json", body: testingSessionRevokedBody, logout: false},
	} {
		_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.contentType)
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		login := taggedSessionTestJwt(t, testingSessionId(), "")
		api.SetByJwt(login)
		recorder := newTestingLogoutRecorder()
		api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))

		_, err := api.networkSessions(t.Context())
		var status *connect.HttpStatusError
		if !errors.As(err, &status) || status.StatusCode != c.status {
			t.Fatalf("%s: err = %v, want status %d", c.name, err, c.status)
		}
		if c.logout {
			recorder.requireCauses(t, "")
		} else {
			recorder.requireCauses(t)
			if api.GetByJwt() != login {
				t.Fatalf("%s: a %d signed the account out", c.name, c.status)
			}
		}
		if api.GetAuthLogoutCause() != "" {
			t.Fatalf("%s: cause %q, want none", c.name, api.GetAuthLogoutCause())
		}
	}
}

// Every leaf of a joined verdict must be the structured code.
func TestRejectionCauseRequiresEveryLeaf(t *testing.T) {
	revoked := &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte(testingSessionRevokedBody)}
	generic := &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte("Not authorized.\n")}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{name: "revoked", err: revoked, want: AuthLogoutCauseSessionRevoked},
		{name: "wrapped", err: fmt.Errorf("request: %w", revoked), want: AuthLogoutCauseSessionRevoked},
		{name: "joined revoked", err: errors.Join(revoked, revoked), want: AuthLogoutCauseSessionRevoked},
		{name: "joined generic", err: errors.Join(revoked, generic), want: ""},
		{name: "joined timeout", err: errors.Join(revoked, context.DeadlineExceeded), want: ""},
		{name: "untyped text", err: errors.New(revoked.Error()), want: ""},
		{name: "nil", err: nil, want: ""},
	} {
		if got := confirmedRejectionCause(c.err); got != c.want {
			t.Fatalf("%s: cause %q, want %q", c.name, got, c.want)
		}
	}
}

// A 401 answered for a credential after a new login replaced it neither
// clears the new login nor sets its cause, for the network credential's
// request seam and for the client token refresh.
func TestDelayedSessionRevokedOfAnOlderLoginChangesNothing(t *testing.T) {
	for _, sameBytes := range []bool{false, true} {
		older := taggedSessionTestJwt(t, testingSessionId(), "")
		newer := taggedSessionTestJwt(t, testingSessionId(), "")
		if sameBytes {
			// an explicit login of the same bytes is still a newer login
			newer = older
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
		var enteredOnce sync.Once
		_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enteredOnce.Do(func() { close(entered) })
			<-release
			testingWriteSessionRevoked(w)
		}))
		t.Cleanup(releaseRequest)
		api.SetByJwt(older)
		recorder := newTestingLogoutRecorder()
		api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))

		done := make(chan error, 1)
		go func() {
			_, err := api.networkSessions(t.Context())
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(testingLogoutCauseStepLimit):
			t.Fatal("the older login's request was not sent")
		}
		api.SetByJwt(newer)
		releaseRequest()
		select {
		case err := <-done:
			if !ConfirmedClientRefreshRejection(err) {
				t.Fatalf("err = %v, want the delayed 401", err)
			}
		case <-time.After(testingLogoutCauseStepLimit):
			t.Fatal("the delayed request did not return")
		}
		recorder.requireCauses(t)
		rejected, cause := api.sessionSignInRejection()
		if api.GetByJwt() != newer || !api.HasNetworkCredential() || api.GetAuthLogoutCause() != "" || rejected || cause != "" {
			t.Fatalf("same bytes %t: the delayed 401 changed the newer login", sameBytes)
		}
	}

	olderClient := taggedSessionTestJwt(t, testingSessionId(), credentialTestClientId)
	newerClient := taggedSessionTestJwt(t, testingSessionId(), credentialTestClientId)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	var enteredOnce sync.Once
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		testingWriteSessionRevoked(w)
	}))
	t.Cleanup(releaseRequest)
	api.SetByJwt(olderClient)
	recorder := newTestingLogoutRecorder()
	api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))
	outcomes := make(chan apiTokenRefreshOutcome, 1)
	go func() { outcomes <- api.tokenManager.refreshTokenWithContext(t.Context(), olderClient) }()
	select {
	case <-entered:
	case <-time.After(testingLogoutCauseStepLimit):
		t.Fatal("the older client token's refresh was not sent")
	}
	api.SetByJwt(newerClient)
	releaseRequest()
	select {
	case outcome := <-outcomes:
		if outcome.loggedOut || !outcome.stale {
			t.Fatalf("the delayed refresh 401 outcome = %+v, want stale", outcome)
		}
	case <-time.After(testingLogoutCauseStepLimit):
		t.Fatal("the delayed refresh did not return")
	}
	recorder.requireCauses(t)
	if api.GetByJwt() != newerClient || api.GetAuthLogoutCause() != "" {
		t.Fatal("the delayed refresh 401 changed the newer login")
	}
}

// The cause outlives the app's own cleanup and ends with a new login,
// explicit or a device's.
func TestNewLoginClearsTheAuthLogoutCause(t *testing.T) {
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testingWriteSessionRevoked(w)
	}))
	revoke := func(login string) {
		t.Helper()
		api.SetByJwt(login)
		if _, err := api.networkSessions(t.Context()); !ConfirmedClientRefreshRejection(err) {
			t.Fatalf("err = %v, want the server's 401", err)
		}
		if api.GetAuthLogoutCause() != AuthLogoutCauseSessionRevoked {
			t.Fatalf("cause %q after the revoked session's 401", api.GetAuthLogoutCause())
		}
	}

	revoke(taggedSessionTestJwt(t, testingSessionId(), ""))
	api.SetByJwt("")
	if api.GetAuthLogoutCause() != AuthLogoutCauseSessionRevoked {
		t.Fatal("clearing the credential is not a new login")
	}
	if rejected, cause := api.sessionSignInRejection(); !rejected || cause != AuthLogoutCauseSessionRevoked {
		t.Fatal("clearing the credential forgot the rejection")
	}
	api.SetByJwt(taggedSessionTestJwt(t, testingSessionId(), ""))
	if api.GetAuthLogoutCause() != "" {
		t.Fatal("a new login kept the earlier sign-in's cause")
	}
	if rejected, cause := api.sessionSignInRejection(); rejected || cause != "" {
		t.Fatal("a new login kept the earlier sign-in's rejection")
	}

	revoke(taggedSessionTestJwt(t, testingSessionId(), ""))
	session := testingSessionId()
	clientJwt := taggedSessionTestJwt(t, session, credentialTestClientId)
	localState := newRenewalTestLocalState(t, t.TempDir(), taggedSessionTestJwt(t, session, ""), clientJwt)
	installTestDeviceByJwt(t, api, localState, clientJwt, localState.GetInstanceId())
	if api.GetAuthLogoutCause() != "" {
		t.Fatal("a device's login kept the earlier sign-in's cause")
	}
}

// A sign-out of this API's own session carries no cause, even when another
// request observes the revocation before the sign-out completes.
func TestSelfSignOutCarriesNoCause(t *testing.T) {
	revoking := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	var revoked atomic.Bool
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/network/revoke-session" {
			// the server enforces the cutoff, then the answer is slow
			revoked.Store(true)
			close(revoking)
			<-release
			var args RevokeNetworkSessionArgs
			_ = json.NewDecoder(r.Body).Decode(&args)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&SessionOperationResult{OperationId: args.OperationId, SessionId: args.SessionId, Status: "revoked", State: "enforced"})
			return
		}
		if revoked.Load() {
			testingWriteSessionRevoked(w)
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	t.Cleanup(releaseRequest)
	api.SetByJwt(taggedSessionTestJwt(t, testingSessionId(), ""))
	recorder := newTestingLogoutRecorder()
	api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))

	results := make(chan *SessionSignOutResult, 1)
	api.SignOut(sessionSignOutCallbackFunc(func(result *SessionSignOutResult, err error) {
		results <- result
	}))
	select {
	case <-revoking:
	case <-time.After(testingLogoutCauseStepLimit):
		t.Fatal("the sign-out did not reach the server")
	}
	// another request of this API observes the cutoff first
	if _, err := api.networkSessions(t.Context()); !ConfirmedClientRefreshRejection(err) {
		t.Fatalf("err = %v, want the server's 401", err)
	}
	releaseRequest()
	select {
	case <-results:
	case <-time.After(testingLogoutCauseStepLimit):
		t.Fatal("the sign-out did not complete")
	}
	recorder.requireCauses(t, "")
	if api.GetAuthLogoutCause() != "" {
		t.Fatalf("a sign-out made here reported %q", api.GetAuthLogoutCause())
	}
}

// A network space with storage whose API makes no background refresh: each
// request in these tests is one the test makes.
func testingLogoutCauseSpace(t *testing.T, apiUrl string) (*NetworkSpace, *LocalState) {
	t.Helper()
	manager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(manager.Close)
	key := NewNetworkSpaceKey("logout-cause.test", "test")
	space := manager.UpdateNetworkSpaceValues(key, &NetworkSpaceValues{
		ApiUrl:                   apiUrl,
		PlatformUrl:              "ws://127.0.0.1:1",
		NetExposeServerIps:       true,
		NetExposeServerHostNames: true,
	})
	api := space.GetApi()
	api.tokenManager.Close()
	testingAwaitAuthBoundary(t, api.tokenManager.done)
	return space, space.asyncLocalState.localState
}

// A sign-in of one session: its network token and a device's client token.
func testingLogoutCauseLogin(t *testing.T, localState *LocalState, instanceId *Id) (networkJwt string, clientJwt string) {
	t.Helper()
	session := testingSessionId()
	networkJwt = taggedSessionTestJwt(t, session, "")
	clientJwt = taggedSessionTestJwt(t, session, credentialTestClientId)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwtForInstance(clientJwt, instanceId); err != nil {
		t.Fatal(err)
	}
	return networkJwt, clientJwt
}

func testingLogoutCauseDeviceLocal(t *testing.T, space *NetworkSpace, clientJwt string, instanceId *Id, clientId connect.Id) *DeviceLocal {
	t.Helper()
	settings := testDeviceLocalSettingsRpc()
	settings.EnableRpc = false
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(space, clientJwt, "logout-cause-test", "test", "0.0.0", instanceId, settings, clientId)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(device.Close)
	return device
}

// Answers the server's revoked-session 401 to the credentials a test revokes
// and a 400, which rejects nothing, to every other request.
type testingRevokedSessionServer struct {
	server       *httptest.Server
	stateLock    sync.Mutex
	revokedJwts  map[string]bool
	revokedPaths map[string]int
}

func newTestingRevokedSessionServer(t *testing.T) *testingRevokedSessionServer {
	t.Helper()
	revokedServer := &testingRevokedSessionServer{revokedJwts: map[string]bool{}, revokedPaths: map[string]int{}}
	revokedServer.server = httptest.NewServer(testApiHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revokedServer.stateLock.Lock()
		revoked := revokedServer.revokedJwts[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if revoked {
			revokedServer.revokedPaths[r.URL.Path] += 1
		}
		revokedServer.stateLock.Unlock()
		if !revoked {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		testingWriteSessionRevoked(w)
	})))
	t.Cleanup(revokedServer.server.Close)
	return revokedServer
}

func (self *testingRevokedSessionServer) revoke(byJwts ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, byJwt := range byJwts {
		self.revokedJwts[byJwt] = true
	}
}

func (self *testingRevokedSessionServer) revokedRequests() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requests := map[string]int{}
	for path, count := range self.revokedPaths {
		requests[path] = count
	}
	return requests
}

// The two ways a device's sign-in is rejected: an account request, which
// sends the network credential of the device's session, and the refresh of
// the device's client token, which a revoked transport also asks for.
var testingDeviceRejections = []struct {
	name   string
	path   string
	reject func(t *testing.T, device Device, clientJwt string)
}{
	{name: "account request", path: "/network/sessions", reject: func(t *testing.T, device Device, clientJwt string) {
		t.Helper()
		_, err := device.GetApi().networkSessions(t.Context())
		var status *connect.HttpStatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized {
			t.Fatalf("account request err = %v, want the server's typed 401", err)
		}
	}},
	{name: "client refresh", path: "/auth/refresh", reject: func(t *testing.T, device Device, clientJwt string) {
		t.Helper()
		if outcome := device.GetApi().tokenManager.refreshTokenWithContext(t.Context(), clientJwt); !outcome.loggedOut {
			t.Fatalf("client refresh outcome = %+v, want the logout", outcome)
		}
	}},
}

// The device's own logout listeners run, once, with the cause readable on the
// device, and the device's stored sign-in is gone.
func testingRequireDeviceLogout(t *testing.T, name string, device Device, recorder *testingLogoutRecorder, localState *LocalState) {
	t.Helper()
	recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
	if device.GetAuthLogoutCause() != AuthLogoutCauseSessionRevoked || device.GetApi().GetByJwt() != "" {
		t.Fatalf("%s: the device lost the cause or kept its credential", name)
	}
	if localState.GetByJwt() != "" || localState.GetByClientJwt() != "" || localState.GetInstanceId() != nil {
		t.Fatalf("%s: the device's stored sign-in survived the rejection", name)
	}
}

// DeviceLocal publishes its API's cause to its own logout listeners, for a
// rejection of its client token and for one of its session's network
// credential, which also signs the device out.
func TestDeviceLocalAuthLogoutCause(t *testing.T) {
	for _, c := range testingDeviceRejections {
		revokedServer := newTestingRevokedSessionServer(t)
		space, localState := testingLogoutCauseSpace(t, revokedServer.server.URL)
		instanceId := NewId()
		networkJwt, clientJwt := testingLogoutCauseLogin(t, localState, instanceId)
		revokedServer.revoke(networkJwt, clientJwt)
		device := testingLogoutCauseDeviceLocal(t, space, clientJwt, instanceId, connect.NewId())
		recorder := newTestingLogoutRecorder()
		sub := device.AddAuthLogoutListener(recorder.listener(device.GetAuthLogoutCause))
		t.Cleanup(sub.Close)
		if device.GetAuthLogoutCause() != "" {
			t.Fatalf("%s: a signed-in device reported a logout cause", c.name)
		}

		c.reject(t, device, clientJwt)
		testingRequireDeviceLogout(t, c.name, device, recorder, localState)
		// a GET may race several dials; only this path met the revocation
		if requests := revokedServer.revokedRequests(); len(requests) != 1 || requests[c.path] < 1 {
			t.Fatalf("%s: the server answered %v, want only %s", c.name, requests, c.path)
		}
	}
}

// A DeviceRemote's API reaches the server only through the device process,
// which forwards the server's 401 with its status and body. The remote's API
// rejects its own credential with the cause, and the remote's logout
// listeners read it; the device process's own sign-in is untouched.
func TestDeviceRemoteReceivesTheCauseOverRpc(t *testing.T) {
	for _, c := range testingDeviceRejections {
		revokedServer := newTestingRevokedSessionServer(t)
		localSpace, localState := testingLogoutCauseSpace(t, revokedServer.server.URL)
		instanceId := NewId()
		_, localClientJwt := testingLogoutCauseLogin(t, localState, instanceId)
		clientId := connect.NewId()
		local := testingLogoutCauseDeviceLocal(t, localSpace, localClientJwt, instanceId, clientId)
		settings := defaultDeviceRpcSettings()
		settings.DisableLogging = true
		// each pair on its own port, so a pair never syncs with another
		address, err := parseDeviceRemoteAddress(testing_freeHostPort())
		if err != nil {
			t.Fatal(err)
		}
		settings.Address = address
		// the remote's API has no path to the server but the device process
		settings.RequireRemoteApi = true
		if err := local.SetRpcServer("", "", settings.Address.HostPort()); err != nil {
			t.Fatal(err)
		}
		localRecorder := newTestingLogoutRecorder()
		localSub := local.AddAuthLogoutListener(localRecorder.listener(local.GetAuthLogoutCause))
		t.Cleanup(localSub.Close)

		remoteSpace, remoteState := testingLogoutCauseSpace(t, revokedServer.server.URL)
		remoteNetworkJwt, remoteClientJwt := testingLogoutCauseLogin(t, remoteState, instanceId)
		revokedServer.revoke(remoteNetworkJwt, remoteClientJwt)
		remote, err := newDeviceRemoteWithOverrides(remoteSpace, remoteClientJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(remote.Close)
		remote.Sync()
		if !remote.waitForSync(testingLogoutCauseStepLimit) || !remote.GetRemoteConnected() {
			t.Fatalf("%s: the remote did not sync with the device", c.name)
		}
		recorder := newTestingLogoutRecorder()
		sub := remote.AddAuthLogoutListener(recorder.listener(remote.GetAuthLogoutCause))
		t.Cleanup(sub.Close)

		c.reject(t, remote, remoteClientJwt)
		testingRequireDeviceLogout(t, c.name, remote, recorder, remoteState)
		if requests := revokedServer.revokedRequests(); len(requests) != 1 || requests[c.path] < 1 {
			t.Fatalf("%s: the device process forwarded %v, want only %s", c.name, requests, c.path)
		}
		localRecorder.requireCauses(t)
		if local.GetApi().GetByJwt() != localClientJwt || local.GetAuthLogoutCause() != "" {
			t.Fatalf("%s: the remote's rejection signed the device process out", c.name)
		}
	}
}

type testingClientSessionSnapshots chan *ClientSessionSnapshot

func (self testingClientSessionSnapshots) ClientSessionsChanged(snapshot *ClientSessionSnapshot) {
	select {
	case self <- snapshot:
	default:
	}
}

// The first published snapshot with an error that is not still loading.
func testingAwaitClientSessionError(t *testing.T, snapshots testingClientSessionSnapshots) *ClientSessionError {
	t.Helper()
	deadline := time.After(testingLogoutCauseStepLimit)
	for {
		select {
		case snapshot := <-snapshots:
			if snapshot.Error != nil && !snapshot.Loading {
				return snapshot.Error
			}
		case <-deadline:
			t.Fatal("the controller published no error")
			return nil
		}
	}
}

// The controller reports SessionRevoked only for the server's structured code,
// never for a generic 401, another status, or a revocation of this session
// that the controller itself made.
func TestClientSessionErrorReportsSessionRevokedOnlyForTheTrustedCode(t *testing.T) {
	for _, revokedSession := range []bool{true, false} {
		_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if revokedSession {
				testingWriteSessionRevoked(w)
				return
			}
			http.Error(w, "Not authorized.", http.StatusUnauthorized)
		}))
		api.SetByJwt(taggedSessionTestJwt(t, testingSessionId(), ""))
		recorder := newTestingLogoutRecorder()
		api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))
		vc := NewClientSessionViewControllerWithApi(t.Context(), api)
		t.Cleanup(vc.Close)
		snapshots := make(testingClientSessionSnapshots, 64)
		sub := vc.AddClientSessionListener(snapshots)
		t.Cleanup(sub.Close)
		vc.Start()

		sessionError := testingAwaitClientSessionError(t, snapshots)
		if !sessionError.SignInRequired || sessionError.Retryable || sessionError.SessionRevoked != revokedSession {
			t.Fatalf("revoked session %t: error %+v", revokedSession, sessionError)
		}
		if revokedSession {
			recorder.requireCauses(t, AuthLogoutCauseSessionRevoked)
		} else {
			recorder.requireCauses(t, "")
		}
	}

	// statuses other than 401 never report a revoked session
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	defer vc.Close()
	sent := taggedSessionTestJwt(t, testingSessionId(), "")
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable} {
		sessionError := vc.clientSessionError(&connect.HttpStatusError{StatusCode: status, Body: []byte(testingSessionRevokedBody)}, true, sent)
		if sessionError.SessionRevoked || sessionError.SignInRequired {
			t.Fatalf("status %d: error %+v", status, sessionError)
		}
	}
	sessionError := vc.clientSessionError(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Body: []byte(testingSessionRevokedBody)}, true, sent)
	if !sessionError.SessionRevoked || !sessionError.SignInRequired {
		t.Fatalf("the structured 401: error %+v", sessionError)
	}
}

// Revoking the current session from the controller signs this app out. The
// server answers 202 while the cutoff is enforced, so the controller's next
// requests meet session_revoked: that is this sign-out, reported without the
// cause.
func TestClientSessionSelfRevokeIsNotReportedAsRevokedElsewhere(t *testing.T) {
	session := testingSessionId()
	sessionId, err := ParseId(session)
	if err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Bool
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case revoked.Load():
			testingWriteSessionRevoked(w)
		case r.URL.Path == "/network/sessions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"sessions":[{"session_id":%q,"current":true,"kind":"password"}],"generation":"g","event_id":1,"current_session_id":%q,"legacy_coverage":"complete"}`, session, session)
		case r.URL.Path == "/network/revoke-session":
			var args RevokeNetworkSessionArgs
			_ = json.NewDecoder(r.Body).Decode(&args)
			revoked.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(&SessionOperationResult{OperationId: args.OperationId, SessionId: args.SessionId, Status: "pending", State: "prepared"})
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	api.SetByJwt(taggedSessionTestJwt(t, session, ""))
	recorder := newTestingLogoutRecorder()
	api.AddAuthLogoutListener(recorder.listener(api.GetAuthLogoutCause))
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	t.Cleanup(vc.Close)
	snapshots := make(testingClientSessionSnapshots, 64)
	sub := vc.AddClientSessionListener(snapshots)
	t.Cleanup(sub.Close)
	vc.Start()
	deadline := time.After(testingLogoutCauseStepLimit)
	for loaded := false; !loaded; {
		select {
		case snapshot := <-snapshots:
			loaded = snapshot.Loaded && snapshot.CurrentSessionId != nil && snapshot.CurrentSessionId.Cmp(sessionId) == 0
		case <-deadline:
			t.Fatal("the controller did not load the current session")
		}
	}

	vc.RevokeSession(sessionId)
	sessionError := testingAwaitClientSessionError(t, snapshots)
	if !sessionError.SignInRequired || sessionError.SessionRevoked {
		t.Fatalf("this session's own sign-out: error %+v", sessionError)
	}
	recorder.requireCauses(t, "")
	if api.GetAuthLogoutCause() != "" || api.GetByJwt() != "" {
		t.Fatal("the self sign-out was reported as revoked elsewhere")
	}
}

// The device process's answer crosses the rpc as gob. A typed answer within
// the body limit arrives as the same connect.HttpStatusError; a peer from
// before the status fields, either side, keeps the untyped message.
func TestDeviceRemoteHttpStatusCrossesTheRpcWire(t *testing.T) {
	revoked := &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte(testingSessionRevokedBody)}
	response := gobRoundTrip(t, newDeviceRemoteHttpResponseWithLimit(connect.NewId(), nil, revoked, 1024))
	err := response.toError()
	var status *connect.HttpStatusError
	if !errors.As(err, &status) || status.StatusCode != revoked.StatusCode || status.Status != revoked.Status || string(status.Body) != testingSessionRevokedBody {
		t.Fatalf("the answer arrived as %#v", err)
	}
	if err.Error() != revoked.Error() || confirmedRejectionCause(err) != AuthLogoutCauseSessionRevoked {
		t.Fatalf("the typed answer reads %q with cause %q", err.Error(), confirmedRejectionCause(err))
	}

	// a body over the limit crosses as text only, which confirms nothing
	oversized := newDeviceRemoteHttpResponseWithLimit(connect.NewId(), nil, revoked, len(testingSessionRevokedBody)-1)
	if err := oversized.toError(); errors.As(err, &status) || ConfirmedClientRefreshRejection(err) {
		t.Fatalf("an oversized answer arrived typed: %#v", err)
	}
	// a request that got no answer stays untyped
	if err := newDeviceRemoteHttpResponseWithLimit(connect.NewId(), nil, context.DeadlineExceeded, 1024).toError(); errors.As(err, &status) {
		t.Fatalf("a failed request arrived typed: %#v", err)
	}

	type legacyHttpResponseError struct{ Error string }
	type legacyHttpResponse struct {
		RequestId connect.Id
		BodyBytes []byte
		Error     *legacyHttpResponseError
	}
	// an older remote reads the message it always read
	legacy := gobRoundTripAs[legacyHttpResponse](t, newDeviceRemoteHttpResponseWithLimit(connect.NewId(), nil, revoked, 1024))
	if legacy.Error == nil || legacy.Error.Error != revoked.Error() {
		t.Fatalf("an older remote read %#v", legacy.Error)
	}
	// an older device process sends no status: untyped, as before
	current := gobRoundTripAs[DeviceRemoteHttpResponse](t, &legacyHttpResponse{RequestId: connect.NewId(), Error: &legacyHttpResponseError{Error: revoked.Error()}})
	if err := current.toError(); err == nil || errors.As(err, &status) || err.Error() != revoked.Error() {
		t.Fatalf("an older device process's answer arrived as %#v", err)
	}
}

// Encodes one type and decodes it as another, as two rpc peers of different
// versions do.
func gobRoundTripAs[T any](t *testing.T, value any) T {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(value); err != nil {
		t.Fatal(err)
	}
	var out T
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
