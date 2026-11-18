// Queued registration responses belong to the invoking sign-in, including
// when a replacement login installs the same credential before dispatch.
package sdk

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A synthetic network api key uses the normal ownership path without renewal.
const (
	registrationGenerationTestJwt           = "urn_synthetic_registration_generation"
	registrationGenerationTestRejectionBody = `{"error":"Synthetic registration refusal."}`
)

// The launcher holds the real public method before the real http transport;
// the local server returns the same typed status errors as production.
type registrationGenerationTest struct {
	held         *heldAuthNetworkClient
	localState   *LocalState
	requestCount atomic.Int32
	logoutCount  int
}

// Keeps each callback's terminal result separate from credential side effects.
type registrationGenerationReply struct {
	result        *AuthNetworkClientResult
	err           error
	callbackCount int
}

// Response statuses are immutable and keyed by synthetic request description.
func newRegistrationGenerationTest(t *testing.T, statuses map[string]int) *registrationGenerationTest {
	t.Helper()
	fixture := &registrationGenerationTest{}
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requestCount.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/network/auth-client" {
			t.Error("registration used an unexpected method or route")
		}
		if r.Header.Get("Authorization") != "Bearer "+registrationGenerationTestJwt {
			t.Error("registration did not send the invocation credential")
		}
		var args AuthNetworkClientArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Error("registration arguments were not JSON")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status, ok := statuses[args.DeviceDescription]
		if !ok {
			t.Error("registration sent an unexpected request description")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"by_client_jwt":"synthetic-registration-client"}`))
		} else {
			_, _ = w.Write([]byte(registrationGenerationTestRejectionBody))
		}
	}))
	fixture.held = &heldAuthNetworkClient{api: api}
	api.authNetworkClientLaunch = func(request func()) {
		fixture.held.requests = append(fixture.held.requests, request)
	}
	fixture.localState = newRenewalTestLocalState(t, t.TempDir(), registrationGenerationTestJwt, "")
	api.SetByJwt(registrationGenerationTestJwt)
	api.SetNetworkCredentialStore(fixture.localState)
	api.AddAuthLogoutListener(authLogoutListenerFunc(func() { fixture.logoutCount += 1 }))
	return fixture
}

// The synchronous launcher barrier proves transport and callback have not run.
func (self *registrationGenerationTest) queue(t *testing.T, description string) *registrationGenerationReply {
	t.Helper()
	reply := &registrationGenerationReply{}
	queuedCount := len(self.held.requests)
	requestCount := self.requestCount.Load()
	self.held.api.AuthNetworkClient(&AuthNetworkClientArgs{DeviceDescription: description}, connect.NewApiCallback(func(result *AuthNetworkClientResult, err error) {
		reply.callbackCount += 1
		reply.result, reply.err = result, err
	}))
	if len(self.held.requests) != queuedCount+1 || self.requestCount.Load() != requestCount || reply.callbackCount != 0 {
		t.Fatal("registration did not remain held at its invocation boundary")
	}
	return reply
}

// Reinstalls the same token through explicit login and reconnects persistence.
func (self *registrationGenerationTest) loginAgain(t *testing.T, logout bool) {
	t.Helper()
	api := self.held.api
	previous, tracked := api.networkRequestTarget(registrationGenerationTestJwt)
	if !tracked {
		t.Fatal("the original network credential was not tracked")
	}
	if logout {
		api.SetByJwt("")
		if err := self.localState.SetByJwt(""); err != nil {
			t.Fatal(err)
		}
	}
	if err := self.localState.SetByJwt(registrationGenerationTestJwt); err != nil {
		t.Fatal(err)
	}
	api.SetByJwt(registrationGenerationTestJwt)
	api.SetNetworkCredentialStore(self.localState)
	current, tracked := api.networkRequestTarget(registrationGenerationTestJwt)
	if !tracked || previous.generation == current.generation {
		t.Fatal("explicit login did not replace the network credential generation")
	}
}

// A typed 401 must still reach the old caller exactly once after its owner left.
func (self *registrationGenerationTest) releaseError(t *testing.T, index int, reply *registrationGenerationReply, status int) {
	t.Helper()
	requestCount := self.requestCount.Load()
	self.held.release(t, index)
	if self.requestCount.Load() != requestCount+1 || reply.callbackCount != 1 {
		t.Fatal("registration did not make one request and deliver one callback")
	}
	var statusErr *connect.HttpStatusError
	if reply.result != nil || !errors.As(reply.err, &statusErr) || statusErr.StatusCode != status {
		t.Fatalf("registration did not deliver its typed HTTP status %d", status)
	}
	if ConfirmedClientRefreshRejection(reply.err) != (status == http.StatusUnauthorized) {
		t.Fatal("registration changed the confirmed rejection classification")
	}
}

// Successful replies still reach their invoking caller exactly once.
func (self *registrationGenerationTest) releaseSuccess(t *testing.T, index int, reply *registrationGenerationReply) {
	t.Helper()
	requestCount := self.requestCount.Load()
	self.held.release(t, index)
	if self.requestCount.Load() != requestCount+1 || reply.callbackCount != 1 || reply.err != nil || reply.result == nil || reply.result.ByClientJwt != "synthetic-registration-client" {
		t.Error("registration did not deliver its successful result once")
	}
}

// The replacement's api and durable state must remain available to new work.
func (self *registrationGenerationTest) requireSignedIn(t *testing.T) {
	t.Helper()
	api := self.held.api
	if api.GetByJwt() != registrationGenerationTestJwt || !api.HasNetworkCredential() {
		t.Error("queued registration cleared the replacement API credential")
	}
	if self.localState.GetByJwt() != registrationGenerationTestJwt {
		t.Error("queued registration cleared the replacement stored credential")
	}
	if rejected, cause := api.sessionSignInRejection(); rejected || cause != "" || api.GetAuthLogoutCause() != "" {
		t.Error("queued registration marked the replacement sign-in as rejected")
	}
	if self.logoutCount != 0 {
		t.Error("queued registration notified logout for the replacement sign-in")
	}
}

// A current owner's confirmed refusal keeps the normal persistent logout path.
func (self *registrationGenerationTest) requireSignedOut(t *testing.T) {
	t.Helper()
	api := self.held.api
	if api.GetByJwt() != "" || api.HasNetworkCredential() || self.localState.GetByJwt() != "" {
		t.Error("current registration rejection did not clear API and stored credentials")
	}
	if rejected, cause := api.sessionSignInRejection(); !rejected || cause != "" || api.GetAuthLogoutCause() != "" {
		t.Error("current registration rejection did not preserve its rejection state")
	}
	if self.logoutCount != 1 {
		t.Error("current registration rejection did not notify logout exactly once")
	}
}

// The replacement is already installed when the old request first dispatches.
func TestAuthNetworkClientQueuedRejectionCannotCrossEqualLogin(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"old": http.StatusUnauthorized})
	reply := fixture.queue(t, "old")
	fixture.loginAgain(t, false)
	fixture.releaseError(t, 0, reply, http.StatusUnauthorized)
	fixture.requireSignedIn(t)
}

// A full local logout followed by equal-byte login is a new owner as well.
func TestAuthNetworkClientQueuedRejectionCannotCrossLogoutAndEqualLogin(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"old": http.StatusUnauthorized})
	reply := fixture.queue(t, "old")
	fixture.loginAgain(t, true)
	fixture.releaseError(t, 0, reply, http.StatusUnauthorized)
	fixture.requireSignedIn(t)
}

// Capturing generation must not disable rejection for an unchanged owner.
func TestAuthNetworkClientQueuedRejectionKeepsCurrentOwner(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"current": http.StatusUnauthorized})
	reply := fixture.queue(t, "current")
	fixture.releaseError(t, 0, reply, http.StatusUnauthorized)
	fixture.requireSignedOut(t)
}

// An old rejection is ignored while the replacement's own 401 still logs out.
func TestAuthNetworkClientQueuedReplacementOwnRejectionStillApplies(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"old": http.StatusUnauthorized, "current": http.StatusUnauthorized})
	oldReply := fixture.queue(t, "old")
	fixture.loginAgain(t, true)
	currentReply := fixture.queue(t, "current")
	fixture.releaseError(t, 0, oldReply, http.StatusUnauthorized)
	fixture.requireSignedIn(t)
	fixture.releaseError(t, 1, currentReply, http.StatusUnauthorized)
	fixture.requireSignedOut(t)
}

// New work after equal-byte login keeps its successful callback and credential.
func TestAuthNetworkClientQueuedReplacementCanRegister(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"old": http.StatusUnauthorized, "current": http.StatusOK})
	oldReply := fixture.queue(t, "old")
	fixture.loginAgain(t, true)
	currentReply := fixture.queue(t, "current")
	fixture.releaseError(t, 0, oldReply, http.StatusUnauthorized)
	fixture.requireSignedIn(t)
	fixture.releaseSuccess(t, 1, currentReply)
	fixture.requireSignedIn(t)
}

// A successful old reply is delivered without mutating the replacement login.
func TestAuthNetworkClientQueuedSuccessKeepsEqualLogin(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"old": http.StatusOK})
	reply := fixture.queue(t, "old")
	fixture.loginAgain(t, true)
	fixture.releaseSuccess(t, 0, reply)
	fixture.requireSignedIn(t)
}

// A transport status other than confirmed rejection never removes this owner.
func TestAuthNetworkClientQueuedTransientFailureKeepsCurrentOwner(t *testing.T) {
	fixture := newRegistrationGenerationTest(t, map[string]int{"current": http.StatusServiceUnavailable})
	reply := fixture.queue(t, "current")
	fixture.releaseError(t, 0, reply, http.StatusServiceUnavailable)
	fixture.requireSignedIn(t)
}
