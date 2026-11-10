//go:build !ios_extension && !js

package sdk

// §22.5's §18 row at the sdk (SDK-M8-01): the child token's Api session refreshes only once it holds the core's
// signer and sends no refresh without one, a logout event released after a newer token is dropped, the session holds
// no network credential and never the device's token, and the host classes' budgets and background rule. A local
// server stands in for api; every wait is on a channel.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"
)

// A sidecar child's token: a strict_exp client token, with marks that tell two of them apart.
func testSidecarChildToken(t *testing.T, networkId string, clientId string, refresh int) string {
	t.Helper()
	return credentialTestJwt(t, gojwt.MapClaims{
		"network_id": networkId,
		"user_id":    credentialTestUserId,
		"client_id":  clientId,
		"device_id":  credentialTestDeviceId,
		"strict_exp": true,
		"refresh":    refresh,
	})
}

// One request api took, with its body.
type testSessionRequest struct {
	method        string
	path          string
	authorization string
	proof         string
	body          []byte
}

// api for the session and the mint: each request recorded and handed to arrived, answered by answer.
type testSessionServer struct {
	arrived chan testSessionRequest
	answer  func(request testSessionRequest) (int, any)

	stateLock sync.Mutex
	requests  []testSessionRequest
}

func newTestSessionServer(answer func(request testSessionRequest) (int, any)) *testSessionServer {
	return &testSessionServer{arrived: make(chan testSessionRequest, 64), answer: answer}
}

func (self *testSessionServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := testSessionRequest{
		method:        r.Method,
		path:          r.URL.Path,
		authorization: r.Header.Get("Authorization"),
		proof:         r.Header.Get("DPoP"),
		body:          body,
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, request)
	}()
	status, reply := self.answer(request)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(reply)
	select {
	case self.arrived <- request:
	default:
	}
}

func (self *testSessionServer) seen() []testSessionRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testSessionRequest{}, self.requests...)
}

// The next request to path, or a failure after the test's timeout.
func (self *testSessionServer) await(t *testing.T, path string) testSessionRequest {
	t.Helper()
	deadline := time.After(testSidecarTimeout)
	for {
		select {
		case request := <-self.arrived:
			if request.path == path {
				return request
			}
		case <-deadline:
			t.Fatalf("no request to %s; saw %d", path, len(self.seen()))
		}
	}
}

// The session's refresher starts only once the core's signer is installed: before it, a refresh asked for is not
// sent, and the first refresh after it carries the signer's proof over the child token.
func TestSignerInstalledBeforeRefresh(t *testing.T) {
	child := testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 0)
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]string{"by_jwt": testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 1)}
	})
	ctx, deviceApi := newTestApi(t, server)
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	session.install(child)
	session.api.RequestJwtRefresh()
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("a refresh went before the signer: %v", seen)
	}
	signer, publicKey := dpopTestSigner(t)
	if err := session.SetDpopSigner(sidecar.SidecarDpopSigner(signer)); err != nil {
		t.Fatal(err)
	}
	request := server.await(t, "/auth/refresh")
	if request.authorization != "Bearer "+child {
		t.Fatal("the refresh did not present the child token")
	}
	requireDpopTestProof(t, request.proof, publicKey, http.MethodGet, session.api.apiUrl+"/auth/refresh", child)
	// a second signer starts nothing twice
	if err := session.SetDpopSigner(sidecar.SidecarDpopSigner(signer)); err != nil {
		t.Fatal(err)
	}
}

// Without the core's signer no refresh of the child token is sent at all, and a session holding no token refuses the
// signer, so the core's own refresher keeps the token (D-CORE-8).
func TestNoRefreshSentWithoutSigner(t *testing.T) {
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]string{"by_jwt": testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 1)}
	})
	ctx, deviceApi := newTestApi(t, server)
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	signer, _ := dpopTestSigner(t)
	if err := session.SetDpopSigner(sidecar.SidecarDpopSigner(signer)); !errors.Is(err, errSidecarSessionEmpty) {
		t.Fatalf("an empty session took the signer: %v", err)
	}
	if _, err := session.ByJwt(); err == nil {
		t.Fatal("an empty session answered a token")
	}
	session.install(testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 0))
	if _, err := session.api.RefreshJwtSyncWithContext(ctx); !errors.Is(err, ErrDpopSignerRequired) {
		t.Fatalf("a refresh without the signer: %v", err)
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("sent %v without the signer", seen)
	}
}

// A confirmed rejection the session's Api found current reaches the core as a logout of the token the session held;
// the same rejection released after a newer token was installed is dropped (D-SDK-14).
func TestStaleLogoutEventDropped(t *testing.T) {
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]any{}
	})
	ctx, deviceApi := newTestApi(t, server)
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	first := testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 0)
	session.install(first)
	var logoutLock sync.Mutex
	logouts := []string{}
	remove := session.AddLogoutCallback(func(byJwt string, cause string) {
		logoutLock.Lock()
		defer logoutLock.Unlock()
		logouts = append(logouts, byJwt)
	})
	defer remove()
	if !session.api.rejectByJwt(first, "") {
		t.Fatal("the rejection did not commit")
	}
	generation := func() uint64 {
		session.api.mutex.Lock()
		defer session.api.mutex.Unlock()
		return session.api.authLogoutGeneration
	}()
	if len(logouts) != 1 || logouts[0] != first {
		t.Fatalf("the current rejection: %d logouts", len(logouts))
	}
	// a re-pair installs the next child; the first rejection's event, released late, names nothing current
	session.install(testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 1))
	session.api.authLogoutEvent(generation, "")
	if len(logouts) != 1 {
		t.Fatalf("a stale logout event reached the core: %d logouts", len(logouts))
	}
}

// The mint goes on the device's token with both marks and no egress, and the session that holds the child's answer
// holds no network credential and never the device's token, while the device's Api keeps its own.
func TestApiSessionHoldsNoNetworkCredential(t *testing.T) {
	child := testSidecarChildToken(t, credentialTestNetworkId, credentialTestKeyClientId, 0)
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": credentialTestKeyClientId, "admin_refused": true, "strict_exp": true}
	})
	ctx, deviceApi := newTestApi(t, server)
	deviceToken := dpopTestClientToken(t)
	deviceApi.SetByJwt(deviceToken)
	result, err := mintSidecarChild(ctx, deviceApi)
	if err != nil {
		t.Fatal(err)
	}
	if result.ByClientJwt != child || !result.AdminRefused || !result.StrictExp || result.ClientId.String() != credentialTestKeyClientId {
		t.Fatalf("the mint: %v", result)
	}
	request := server.await(t, "/network/auth-client")
	body := &sidecarChildMintArgs{}
	if err := json.Unmarshal(request.body, body); err != nil || !body.AdminRefused || !body.StrictExp || !body.DeclaresNoEgress {
		t.Fatalf("the mint's body %s: %v", request.body, err)
	}
	if request.authorization != "Bearer "+deviceToken || request.proof != "" {
		t.Fatal("the mint did not go on the device's own token")
	}
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	if session.api.GetByJwt() != "" || session.api.networkByJwt != "" {
		t.Fatal("a new session holds a credential")
	}
	session.install(result.ByClientJwt)
	if session.api.GetByJwt() != child || session.api.networkByJwt != "" {
		t.Fatal("the session holds more than the child's token")
	}
	if deviceApi.GetByJwt() != deviceToken {
		t.Fatal("the device's Api lost its token")
	}

	// a refusal, and an answer with no token, keep nothing
	for _, answer := range []any{
		map[string]any{"error": map[string]any{"client_limit_exceeded": true, "message": "full"}},
		map[string]any{"client_id": credentialTestKeyClientId},
	} {
		refusing := newTestSessionServer(func(request testSessionRequest) (int, any) {
			return http.StatusOK, answer
		})
		refusingCtx, refusingApi := newTestApi(t, refusing)
		refusingApi.SetByJwt(deviceToken)
		if result, err := mintSidecarChild(refusingCtx, refusingApi); err == nil {
			t.Fatalf("a refused mint answered %v", result)
		}
	}
}

// §18.4's budgets by host class, an app's own budget in their place where it sets one, and an unknown class refused.
func TestHostClassBudgets(t *testing.T) {
	for _, c := range []struct {
		class  string
		budget int64
	}{
		{class: SidecarHostClassIos, budget: 32 * 1024 * 1024},
		{class: SidecarHostClassAndroid, budget: 16 * 1024 * 1024},
		{class: SidecarHostClassDesktop, budget: 128 * 1024 * 1024},
	} {
		coreSettings := sidecar.DefaultSidecarSettings()
		hostClass, err := applySidecarHostClass(&SidecarSettings{HostClass: c.class}, coreSettings)
		if err != nil || hostClass.name != c.class || coreSettings.MemoryBudgetByteCount != c.budget {
			t.Errorf("%s: %v, budget %d", c.class, err, coreSettings.MemoryBudgetByteCount)
		}
		coreSettings = sidecar.DefaultSidecarSettings()
		if _, err := applySidecarHostClass(&SidecarSettings{HostClass: c.class, MemoryBudgetByteCount: 7 * 1024 * 1024}, coreSettings); err != nil || coreSettings.MemoryBudgetByteCount != 7*1024*1024 {
			t.Errorf("%s with the app's budget: %d", c.class, coreSettings.MemoryBudgetByteCount)
		}
	}
	if _, err := applySidecarHostClass(&SidecarSettings{HostClass: "tv"}, sidecar.DefaultSidecarSettings()); err == nil {
		t.Error("an unknown host class was taken")
	}
	// this build's own class
	if hostClass, err := sidecarHostClassOf(""); err != nil || hostClass.name != SidecarHostClassDesktop {
		t.Errorf("the platform's class: %v %v", hostClass, err)
	}
}

// An Android host class starts no background sidecar while the VPN runs: the VPN service shares the app's process,
// yet the sidecar role runs only while the app is open, in an OS window or for a wake, as iOS's (owner decision 197);
// a desktop class runs whenever the app runs it.
func TestAndroidHostClassStartsNoBackgroundSidecar(t *testing.T) {
	for _, name := range []string{SidecarHostClassAndroid, SidecarHostClassIos} {
		hostClass, err := sidecarHostClassOf(name)
		if err != nil {
			t.Fatal(err)
		}
		if hostClass.mayRun(false, false, false, true) {
			t.Errorf("%s: the VPN alone keeps a sidecar running", name)
		}
		if !hostClass.mayRun(true, false, false, true) || !hostClass.mayRun(false, true, false, false) || !hostClass.mayRun(false, false, true, true) {
			t.Errorf("%s: the app open, an OS window or a wake does not let the sidecar run", name)
		}
	}
	desktop, _ := sidecarHostClassOf(SidecarHostClassDesktop)
	if !desktop.mayRun(false, false, false, false) {
		t.Error("a desktop host's sidecar does not run")
	}
	// and NewSidecarLocal takes the class from the settings
	world := newTestSidecarWorld(t)
	local := world.openWith(t, &SidecarSettings{Instance: "default", Components: NewStringList(), HostClass: SidecarHostClassAndroid}, &sidecarLocalOptions{})
	if local.hostClass.name != SidecarHostClassAndroid || !local.hostClass.foregroundOnly {
		t.Fatalf("the Sidecar's class: %+v", local.hostClass)
	}
	if _, err := checkedSidecarSettings(&SidecarSettings{HostClass: "tv"}, false); err == nil {
		t.Fatal("NewSidecarLocal took an unknown class")
	}
}
