//go:build !ios_extension && !js

package sdk

// §22.5's §18 row at the sdk (SDK-M8-01): the child token's Api session refreshes only once it holds the core's
// signer and sends no refresh without one, a logout event released after a newer token is dropped, the session holds
// no network credential and never the device's token, and the host classes' budgets and background rule. The session
// takes bind's re-mint of its own token alone, and no closed or emptied session takes one (QD-805). A local server
// stands in for api; every wait is on a channel.

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

// byJwt's claims, read unverified, edited by edit and signed again with no signature, as every stand-in token here
// is. An error for a byJwt that is not a token, so a stand-in server can answer it.
func testEditedSidecarToken(byJwt string, edit func(claims gojwt.MapClaims)) (string, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return "", err
	}
	edit(claims)
	return gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
}

// presented re-minted with sidecarId as its sidecar_id claim, as server's RemintSidecarCredential re-mints the bearer
// at POST /sidecar/bind (QD-040, QD-797): every other claim and its lifetime as presented, and a new jti.
func testRemintedSidecarToken(presented string, sidecarId sidecar.Id) (string, error) {
	return testEditedSidecarToken(presented, func(claims gojwt.MapClaims) {
		claims["sidecar_id"] = sidecarId.String()
		claims["jti"] = sidecar.NewId().String()
	})
}

// presented refreshed as server's GET /auth/refresh answers it: every claim kept, sidecar_id among them when presented
// carries one, and none added, with a new jti and a new lifetime from now.
func testRefreshedSidecarToken(presented string) (string, error) {
	return testEditedSidecarToken(presented, func(claims gojwt.MapClaims) {
		now := time.Now()
		claims["iat"] = now.Unix()
		claims["exp"] = now.Add(30 * 24 * time.Hour).Unix()
		claims["jti"] = sidecar.NewId().String()
	})
}

// testRemintedSidecarToken's re-mint, on the test's goroutine.
func testSidecarRemint(t *testing.T, presented string, sidecarId sidecar.Id) string {
	t.Helper()
	reminted, err := testRemintedSidecarToken(presented, sidecarId)
	if err != nil {
		t.Fatal(err)
	}
	return reminted
}

// testEditedSidecarToken's token, on the test's goroutine.
func testSidecarTokenEdited(t *testing.T, byJwt string, edit func(claims gojwt.MapClaims)) string {
	t.Helper()
	edited, err := testEditedSidecarToken(byJwt, edit)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

// Fails unless byJwt names clientId and carries sidecarId as its sidecar_id claim.
func requireTestSidecarClaim(t *testing.T, byJwt string, clientId string, sidecarId sidecar.Id) {
	t.Helper()
	claims, ok := unverifiedCredentialClaims(byJwt)
	if !ok {
		t.Fatal("not a token")
	}
	if claims["client_id"] != clientId || claims["sidecar_id"] != sidecarId.String() {
		t.Fatalf("the token names client %v and sidecar %v, not %s and %s", claims["client_id"], claims["sidecar_id"], clientId, sidecarId)
	}
}

// The token the session's state holds, beside the one its Api presents.
func testSessionHeldByJwt(session *sidecarApiSession) string {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	return session.byJwt
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
	// a session whose token a rejection cleared is no source for a later generation, and takes no signer
	signer, _ := dpopTestSigner(t)
	if session.holdsToken() || session.SetDpopSigner(sidecar.SidecarDpopSigner(signer)) == nil {
		t.Fatal("a session whose token was rejected still offers it")
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

// A closed session takes no re-mint (QD-805): AdoptByJwt refuses, and the session and its Api keep the token they held,
// as install does on a closed session.
func TestAdoptByJwtRefusedOnAClosedSession(t *testing.T) {
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]any{}
	})
	ctx, deviceApi := newTestApi(t, server)
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	child := testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 0)
	if !session.install(child) {
		t.Fatal("an open session did not take the child's token")
	}
	reminted := testSidecarRemint(t, child, sidecar.NewId())
	session.close()
	if err := session.AdoptByJwt(reminted); !errors.Is(err, errSidecarSessionClosed) {
		t.Fatalf("a closed session's AdoptByJwt: %v", err)
	}
	if session.api.GetByJwt() != child || testSessionHeldByJwt(session) != child {
		t.Fatal("a closed session took the re-mint")
	}
	if session.install(reminted) || session.api.GetByJwt() != child || testSessionHeldByJwt(session) != child {
		t.Fatal("a closed session installed a token")
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("a closed session sent %v", seen)
	}
}

// AdoptByJwt takes only a re-mint of the token the session holds, read as the core reads one (QD-805): the same
// network, client and device, naming a sidecar. A session that holds no token refuses, and so does one a confirmed
// rejection emptied, so no re-mint brings a rejected client back; anything else is refused with the session's own token
// kept, presented and held. The re-mint taken is what the session presents and holds.
func TestAdoptByJwtTakesOnlyTheSessionsRemint(t *testing.T) {
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]any{}
	})
	ctx, deviceApi := newTestApi(t, server)
	session := newSidecarApiSession(deviceApi.newSession(ctx))
	t.Cleanup(session.close)
	child := testSidecarChildToken(t, credentialTestNetworkId, credentialTestClientId, 0)
	sidecarId := sidecar.NewId()
	reminted := testSidecarRemint(t, child, sidecarId)
	if err := session.AdoptByJwt(reminted); !errors.Is(err, errSidecarSessionEmpty) || session.api.GetByJwt() != "" {
		t.Fatalf("a session holding no token: %v", err)
	}

	session.install(child)
	otherId := sidecar.NewId().String()
	for _, c := range []struct {
		name  string
		byJwt string
	}{
		{name: "another client's re-mint", byJwt: testSidecarRemint(t, testSidecarChildToken(t, credentialTestNetworkId, credentialTestKeyClientId, 0), sidecarId)},
		{name: "another device's re-mint", byJwt: testSidecarTokenEdited(t, reminted, func(claims gojwt.MapClaims) { claims["device_id"] = otherId })},
		{name: "another network's re-mint", byJwt: testSidecarTokenEdited(t, reminted, func(claims gojwt.MapClaims) { claims["network_id"] = otherId })},
		{name: "a token naming no sidecar", byJwt: testSidecarTokenEdited(t, child, func(claims gojwt.MapClaims) { claims["jti"] = otherId })},
		{name: "a token naming the zero sidecar", byJwt: testSidecarRemint(t, child, sidecar.Id{})},
		{name: "a token naming a malformed sidecar", byJwt: testSidecarTokenEdited(t, reminted, func(claims gojwt.MapClaims) { claims["sidecar_id"] = "sidecar" })},
		{name: "not a token", byJwt: "not-a-token"},
		{name: "no token", byJwt: ""},
	} {
		if err := session.AdoptByJwt(c.byJwt); !errors.Is(err, errSidecarRemintMismatch) {
			t.Errorf("%s: %v", c.name, err)
		}
		if session.api.GetByJwt() != child || testSessionHeldByJwt(session) != child {
			t.Fatalf("%s replaced the session's token", c.name)
		}
	}

	if err := session.AdoptByJwt(reminted); err != nil {
		t.Fatal(err)
	}
	if byJwt, err := session.ByJwt(); err != nil || byJwt != reminted || testSessionHeldByJwt(session) != reminted || !session.holdsToken() {
		t.Fatal("the session does not present and hold the re-mint")
	}
	// a confirmed rejection empties it, and a later re-mint does not fill it again
	if !session.api.rejectByJwt(reminted, "") {
		t.Fatal("the rejection did not commit")
	}
	if err := session.AdoptByJwt(testSidecarRemint(t, reminted, sidecarId)); !errors.Is(err, errSidecarSessionEmpty) {
		t.Fatalf("a rejected session's AdoptByJwt: %v", err)
	}
	if session.api.GetByJwt() != "" || session.holdsToken() {
		t.Fatal("a re-mint brought a rejected session's token back")
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("the session sent %v", seen)
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
