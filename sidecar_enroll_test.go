//go:build !ios_extension && !js

package sdk

// EnrollWithDevice end to end on a local Sidecar (SDK-M8-01): the device's Api mints the child on its own token, the
// core binds it, and the generation that starts hands its ClientKey's signer to the child token's session, whose
// first refresh carries the proof; then GetSidecarId, GetState and SetPushToken read and post through the core. A
// refused mint keeps nothing and leaves the host's session as it was. Bind re-mints the child's token with the
// sidecar_id, and the session, the keychain and the generation all end holding the re-mint, whose refreshes keep the
// claim, as do those of the re-mint a host's Reregister hands the session (QD-797, QD-805). Two local servers stand in
// for api: the device's and the core's; the platform is a closed loopback port, so nothing leaves the machine.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The core's api: POST /sidecar/bind re-mints the bearer with the binding's sidecar as server does (QD-040, QD-797),
// at the binding's generation; the push token and the removal are answered, and every other route is a 404.
type testCoreServer struct {
	*testSessionServer
	// each token bind re-minted, in order, put here before bind answers it
	reminted chan string
}

func newTestCoreServer(t *testing.T) (*testCoreServer, string) {
	t.Helper()
	reminted := make(chan string, 16)
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/sidecar/bind":
			wire := struct {
				Binding []byte `json:"binding"`
			}{}
			if err := json.Unmarshal(request.body, &wire); err != nil {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			binding, err := sidecar.VerifySidecarIdentityBinding(wire.Binding)
			if err != nil {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			presented, _ := strings.CutPrefix(request.authorization, "Bearer ")
			// server binds only the bearer's own client
			if clientId, err := parseByJwtClientId(presented); err != nil || clientId.String() != binding.ClientId.String() {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			byJwt, err := testRemintedSidecarToken(presented, binding.SidecarId)
			if err != nil {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			select {
			case reminted <- byJwt:
			default:
			}
			return http.StatusOK, map[string]any{"generation": binding.Generation, "by_client_jwt": byJwt}
		case "/sidecar/push-token", "/network/remove-client":
			return http.StatusOK, map[string]any{}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return &testCoreServer{testSessionServer: server, reminted: reminted}, httpServer.URL
}

// The next token bind re-minted. Bind puts it here before it answers, so an enrollment or a Reregister that returned
// finds it.
func (self *testCoreServer) takeReminted(t *testing.T) string {
	t.Helper()
	select {
	case byJwt := <-self.reminted:
		return byJwt
	default:
		t.Fatal("bind re-minted no token")
		return ""
	}
}

// A local Sidecar whose core reaches coreUrl, whose platform is a closed port, and whose host finds no readable
// network credential, on world.
func openTestCoreSidecar(t *testing.T, world *testSidecarWorld, coreUrl string) *Sidecar {
	t.Helper()
	return world.openWith(t, DefaultSidecarSettings(), &sidecarLocalOptions{
		adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
			settings.ApiUrl = coreUrl
			settings.PlatformUrl = "wss://127.0.0.1:1"
		},
		hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
			return nil
		},
	})
}

// openTestCoreSidecar's Sidecar on a world nothing enrolled: Unenrolled.
func openTestEnrollingSidecar(t *testing.T, world *testSidecarWorld, coreUrl string) *Sidecar {
	t.Helper()
	local := openTestCoreSidecar(t, world, coreUrl)
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	return local
}

// A callback's one result.
func testSidecarResultCallback() (SidecarResultCallback, chan *SidecarResult) {
	results := make(chan *SidecarResult, 1)
	return connect.NewApiCallback[*SidecarResult](func(result *SidecarResult, err error) {
		if err != nil {
			result = &SidecarResult{Message: err.Error()}
		}
		results <- result
	}), results
}

func TestEnrollWithDeviceHandsTheSessionTheCoreSigner(t *testing.T) {
	world := newTestSidecarWorld(t)
	coreServer, coreUrl := newTestCoreServer(t)
	childClientId := sidecar.NewId()
	child := testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 0)
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": childClientId.String(), "admin_refused": true, "strict_exp": true}
		case "/auth/refresh":
			presented, _ := strings.CutPrefix(request.authorization, "Bearer ")
			refreshed, err := testRefreshedSidecarToken(presented)
			if err != nil {
				return http.StatusBadRequest, map[string]string{"error": "not a token"}
			}
			return http.StatusOK, map[string]string{"by_jwt": refreshed}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	ctx, deviceApi := newTestApi(t, deviceServer)
	deviceToken := dpopTestClientToken(t)
	deviceApi.SetByJwt(deviceToken)
	local := openTestEnrollingSidecar(t, world, coreUrl)

	callback, results := testSidecarResultCallback()
	go func() {
		value, err := local.enrollWithDeviceApi(ctx, deviceApi)
		callback.Result(sidecarResultOf(value, err), nil)
	}()
	var result *SidecarResult
	select {
	case result = <-results:
	case <-ctx.Done():
		t.Fatal("the enrollment did not answer")
	}
	if !result.Ok {
		t.Fatalf("the enrollment: %+v", result)
	}
	identity := local.Core().Identity()
	if identity == nil || result.Value != identity.SidecarId.String() || identity.ClientId != childClientId {
		t.Fatalf("the identity %+v for %s", identity, result.Value)
	}
	if got := local.GetSidecarId(); got == nil || got.String() != result.Value {
		t.Fatalf("GetSidecarId %v", got)
	}
	mint := deviceServer.seen()[0]
	if mint.path != "/network/auth-client" || mint.authorization != "Bearer "+deviceToken {
		t.Fatalf("the mint: %s on %q", mint.path, mint.authorization)
	}
	// the generation handed the core's signer to the session: its first refresh carries the ClientKey's proof over the
	// token the session took from bind, the child's re-minted with the sidecar_id
	reminted := coreServer.takeReminted(t)
	refresh := deviceServer.await(t, "/auth/refresh")
	if refresh.authorization != "Bearer "+reminted {
		t.Fatal("the first refresh did not present bind's re-mint")
	}
	requireDpopTestProof(t, refresh.proof, identity.ClientPublicKey, http.MethodGet, deviceApi.apiUrl+"/auth/refresh", reminted)
	if session := testHostSession(t, local); session.api.networkByJwt != "" {
		t.Fatal("the host's session holds a network credential")
	}
	if state := local.GetState(); state != string(sidecar.SidecarStateOffline) && state != string(sidecar.SidecarStateOnline) {
		t.Fatalf("the state after enrollment: %s %s", state, local.GetStateReason())
	}

	// the push token goes through the core, on the child's token: the re-mint, or its refresh, which keeps the claim
	pushed, pushResults := testSidecarResultCallback()
	local.SetPushToken(SidecarPushPlatformApns, "push-token-canary-sdk", pushed)
	if result := <-pushResults; !result.Ok {
		t.Fatalf("the push token: %+v", result)
	}
	push := coreServer.await(t, "/sidecar/push-token")
	pushBearer, _ := strings.CutPrefix(push.authorization, "Bearer ")
	requireTestSidecarClaim(t, pushBearer, childClientId.String(), identity.SidecarId)
	for _, request := range coreServer.seen() {
		if request.authorization == "Bearer "+deviceToken {
			t.Fatalf("the device's token reached the core's api at %s", request.path)
		}
	}
}

// A mint whose answer lacks admin_refused keeps nothing: the core refuses and removes the child, the session made
// for it is dropped and the host's is as it was; a remote backend and a device with no token are refused before
// anything is sent.
func TestEnrollWithDeviceKeepsNothingFromARefusedMint(t *testing.T) {
	world := newTestSidecarWorld(t)
	_, coreUrl := newTestCoreServer(t)
	childClientId := sidecar.NewId()
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		return http.StatusOK, map[string]any{
			"by_client_jwt": testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 0),
			"client_id":     childClientId.String(),
			"strict_exp":    true,
		}
	})
	ctx, deviceApi := newTestApi(t, deviceServer)
	local := openTestEnrollingSidecar(t, world, coreUrl)

	// no device token: nothing is sent
	if _, err := local.enrollWithDeviceApi(ctx, deviceApi); err == nil || len(deviceServer.seen()) != 0 {
		t.Fatalf("an enrollment without a device token: %v, %d requests", err, len(deviceServer.seen()))
	}
	deviceApi.SetByJwt(dpopTestClientToken(t))
	if _, err := local.enrollWithDeviceApi(ctx, deviceApi); err == nil {
		t.Fatal("a child without admin_refused was enrolled")
	}
	if local.host.TokenSource() != nil || local.Core().Identity() != nil || len(local.instance.Networks()) != 0 {
		t.Fatal("a refused enrollment kept something")
	}
	if state := local.GetState(); state != string(sidecar.SidecarStateUnenrolled) {
		t.Fatalf("the state after a refused enrollment: %s", state)
	}

}

// The host's child token session, which EnrollWithDevice made.
func testHostSession(t *testing.T, local *Sidecar) *sidecarApiSession {
	t.Helper()
	session := func() *sidecarApiSession {
		local.host.stateLock.Lock()
		defer local.host.stateLock.Unlock()
		return local.host.session
	}()
	if session == nil {
		t.Fatal("the host holds no session")
	}
	return session
}

// An app's local Sidecar and stand-ins for api, for enrollments through EnrollWithDevice: the device's api mints the
// child and refreshes each token as server's refresh does, holding each refresh until the test releases it, and the
// core's re-mints at bind (testCoreServer).
type testRemintWorld struct {
	world         *testSidecarWorld
	coreServer    *testCoreServer
	coreUrl       string
	ctx           context.Context
	deviceApi     *Api
	childClientId sidecar.Id
	// each refresh request as it arrives, before it is answered
	refreshes chan testSessionRequest
	// a value here lets one held refresh answer
	releases chan struct{}
}

func newTestRemintWorld(t *testing.T) *testRemintWorld {
	t.Helper()
	self := &testRemintWorld{
		world:         newTestSidecarWorld(t),
		childClientId: sidecar.NewId(),
		refreshes:     make(chan testSessionRequest, 16),
		releases:      make(chan struct{}),
	}
	self.coreServer, self.coreUrl = newTestCoreServer(t)
	child := testSidecarChildToken(t, credentialTestNetworkId, self.childClientId.String(), 0)
	ended := make(chan struct{})
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": self.childClientId.String(), "admin_refused": true, "strict_exp": true}
		case "/auth/refresh":
			select {
			case self.refreshes <- request:
			case <-ended:
			}
			select {
			case <-self.releases:
			case <-ended:
				return http.StatusServiceUnavailable, map[string]string{"error": "the test ended"}
			}
			presented, _ := strings.CutPrefix(request.authorization, "Bearer ")
			refreshed, err := testRefreshedSidecarToken(presented)
			if err != nil {
				return http.StatusBadRequest, map[string]string{"error": "not a token"}
			}
			return http.StatusOK, map[string]string{"by_jwt": refreshed}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	self.ctx, self.deviceApi = newTestApi(t, deviceServer)
	// added after the server's close, so it runs before it, and no held refresh keeps the server from closing
	t.Cleanup(func() {
		close(ended)
	})
	self.deviceApi.SetByJwt(dpopTestClientToken(t))
	return self
}

// Opens a Sidecar of the world and enrolls it through EnrollWithDevice, answering it with its identity.
func (self *testRemintWorld) enroll(t *testing.T) (*Sidecar, *sidecar.SidecarIdentity) {
	t.Helper()
	local := openTestEnrollingSidecar(t, self.world, self.coreUrl)
	sidecarId, err := local.enrollWithDeviceApi(self.ctx, self.deviceApi)
	if err != nil {
		t.Fatal(err)
	}
	identity := local.Core().Identity()
	if identity == nil || identity.SidecarId.String() != sidecarId || identity.ClientId != self.childClientId {
		t.Fatalf("the identity %+v for %s", identity, sidecarId)
	}
	return local, identity
}

// The next refresh request, which the device's api holds until releaseRefresh.
func (self *testRemintWorld) awaitRefresh(t *testing.T) testSessionRequest {
	t.Helper()
	select {
	case request := <-self.refreshes:
		return request
	case <-time.After(testSidecarTimeout):
		t.Fatal("no refresh arrived")
		return testSessionRequest{}
	}
}

// Lets the held refresh answer, and answers the token session takes from it. The core has followed session since
// awaitTestGenerationFollowsTheSession, and the session's Api calls its refresh listeners in the order they were
// added, so the core has re-sealed and installed the token by the time this one, added last, hears it.
func (self *testRemintWorld) releaseRefresh(t *testing.T, session *sidecarApiSession) string {
	t.Helper()
	refreshed := make(chan string, 1)
	sub := session.api.AddJwtRefreshListener(sidecarJwtRefreshListener(func(byJwt string) {
		select {
		case refreshed <- byJwt:
		default:
		}
	}))
	defer sub.Close()
	select {
	case self.releases <- struct{}{}:
	case <-time.After(testSidecarTimeout):
		t.Fatal("no refresh was held")
	}
	select {
	case byJwt := <-refreshed:
		return byJwt
	case <-time.After(testSidecarTimeout):
		t.Fatal("the session took no refreshed token")
		return ""
	}
}

// The token local's core presents now: the bearer its push token goes on.
func (self *testRemintWorld) pushBearer(t *testing.T, local *Sidecar) string {
	t.Helper()
	callback, results := testSidecarResultCallback()
	local.SetPushToken(SidecarPushPlatformApns, "push-token-canary-remint", callback)
	if result := <-results; !result.Ok {
		t.Fatalf("the push token: %+v", result)
	}
	push := self.coreServer.await(t, "/sidecar/push-token")
	bearer, _ := strings.CutPrefix(push.authorization, "Bearer ")
	return bearer
}

// Closes local and opens the world's Sidecar again, as an app's next launch does: the app's key store unlocks it and
// its host holds no session, so its core presents the token its keychain holds.
func (self *testRemintWorld) restart(t *testing.T, local *Sidecar) *Sidecar {
	t.Helper()
	self.world.close(t, local)
	reopened := openTestCoreSidecar(t, self.world, self.coreUrl)
	awaitTestSidecarState(t, reopened, sidecar.SidecarStateOffline, "")
	if reopened.host.TokenSource() != nil {
		t.Fatal("a restarted host offers a token source")
	}
	return reopened
}

// A component that reports the generation of each attach. A component attaches only to a published generation, and
// the core publishes one only once it follows the host's token source (followTokenSource), so an attach says the
// core's refresh callback is on the session's Api.
type testAttachProbe struct {
	attached chan int64
}

func (self *testAttachProbe) Name() string {
	return "remint-probe"
}

func (self *testAttachProbe) Attach(env *sidecar.SidecarEnv) error {
	select {
	case self.attached <- env.Identity.Generation:
	default:
	}
	return nil
}

func (self *testAttachProbe) Close() {
}

// Waits until local's generation follows the host's token source: until it attaches a probe.
func awaitTestGenerationFollowsTheSession(t *testing.T, local *Sidecar, generation int64) {
	t.Helper()
	probe := &testAttachProbe{attached: make(chan int64, 4)}
	if _, err := local.Core().AddComponent(probe); err != nil {
		t.Fatal(err)
	}
	select {
	case attached := <-probe.attached:
		if attached != generation {
			t.Fatalf("the probe attached to generation %d, not %d", attached, generation)
		}
	case <-time.After(testSidecarTimeout):
		t.Fatal("no generation attached the probe")
	}
}

// EnrollWithDevice ends with the session, the generation and the keychain all holding bind's re-mint, the child's
// token with the bound sidecar_id (QD-797, QD-805): the core hands it to the session before the generation starts, so
// the session's first refresh presents it with the ClientKey's proof, and the push token goes on it; after a restart,
// with no session in the host, the core presents the keychain's, the same re-mint. The first refresh is held
// throughout, so nothing else installs a token meanwhile.
func TestEnrollWithDeviceLeavesTheRemintInSessionGenerationAndKeychain(t *testing.T) {
	remint := newTestRemintWorld(t)
	local, identity := remint.enroll(t)
	reminted := remint.coreServer.takeReminted(t)
	requireTestSidecarClaim(t, reminted, remint.childClientId.String(), identity.SidecarId)

	session := testHostSession(t, local)
	if byJwt, err := session.ByJwt(); err != nil || byJwt != reminted || testSessionHeldByJwt(session) != reminted {
		t.Fatal("the session does not hold the re-mint")
	}
	refresh := remint.awaitRefresh(t)
	if refresh.authorization != "Bearer "+reminted {
		t.Fatal("the session's first refresh did not present the re-mint")
	}
	requireDpopTestProof(t, refresh.proof, identity.ClientPublicKey, http.MethodGet, remint.deviceApi.apiUrl+"/auth/refresh", reminted)
	if bearer := remint.pushBearer(t, local); bearer != reminted {
		t.Fatal("the generation does not hold the re-mint")
	}

	reopened := remint.restart(t, local)
	if bearer := remint.pushBearer(t, reopened); bearer != reminted {
		t.Fatal("the keychain does not hold the re-mint")
	}
}

// A refresh of the re-mint keeps its claim, and the session, the generation and the keychain all take the refreshed
// token (QD-797, QD-805). server's refresh keeps the claim of the token presented and adds none, so the claim lasts
// only because the session presents the re-mint.
func TestHostRefreshOfTheRemintKeepsTheSidecarId(t *testing.T) {
	remint := newTestRemintWorld(t)
	local, identity := remint.enroll(t)
	reminted := remint.coreServer.takeReminted(t)
	session := testHostSession(t, local)
	if refresh := remint.awaitRefresh(t); refresh.authorization != "Bearer "+reminted {
		t.Fatal("the session's first refresh did not present the re-mint")
	}
	awaitTestGenerationFollowsTheSession(t, local, identity.Generation)

	refreshed := remint.releaseRefresh(t, session)
	if refreshed == reminted {
		t.Fatal("the refresh answered the token it was presented")
	}
	requireTestSidecarClaim(t, refreshed, remint.childClientId.String(), identity.SidecarId)
	if byJwt, err := session.ByJwt(); err != nil || byJwt != refreshed || testSessionHeldByJwt(session) != refreshed {
		t.Fatal("the session does not hold the refreshed token")
	}
	if bearer := remint.pushBearer(t, local); bearer != refreshed {
		t.Fatal("the generation did not adopt the refreshed token")
	}

	reopened := remint.restart(t, local)
	if bearer := remint.pushBearer(t, reopened); bearer != refreshed {
		t.Fatal("the keychain does not hold the refreshed token")
	}
}

// A host's Reregister, restore_required's re-registration, hands the session the token bind re-mints then (QD-797,
// QD-805): bind is presented the token the generation holds, the session holds the re-mint at once, its running
// refresher refreshes it at once, as any new token, presenting the re-mint with the ClientKey's proof, and the refresh
// keeps the claim in the session and the generation.
func TestReregisterHandsTheSessionTheRemint(t *testing.T) {
	remint := newTestRemintWorld(t)
	local, identity := remint.enroll(t)
	remint.coreServer.takeReminted(t)
	session := testHostSession(t, local)
	remint.awaitRefresh(t)
	awaitTestGenerationFollowsTheSession(t, local, identity.Generation)
	first := remint.releaseRefresh(t, session)

	if err := local.Core().Reregister(remint.ctx); err != nil {
		t.Fatal(err)
	}
	binds := []testSessionRequest{}
	for _, request := range remint.coreServer.seen() {
		if request.path == "/sidecar/bind" {
			binds = append(binds, request)
		}
	}
	if len(binds) != 2 || binds[1].authorization != "Bearer "+first {
		t.Fatalf("Reregister's bind was not presented the generation's token: %d binds", len(binds))
	}
	reminted := remint.coreServer.takeReminted(t)
	if reminted == first {
		t.Fatal("bind answered the token it was presented")
	}
	requireTestSidecarClaim(t, reminted, remint.childClientId.String(), identity.SidecarId)
	if byJwt, err := session.ByJwt(); err != nil || byJwt != reminted || testSessionHeldByJwt(session) != reminted {
		t.Fatal("the session did not take Reregister's re-mint")
	}

	refresh := remint.awaitRefresh(t)
	if refresh.authorization != "Bearer "+reminted {
		t.Fatal("the session's refresh after Reregister did not present its re-mint")
	}
	requireDpopTestProof(t, refresh.proof, identity.ClientPublicKey, http.MethodGet, remint.deviceApi.apiUrl+"/auth/refresh", reminted)
	refreshed := remint.releaseRefresh(t, session)
	requireTestSidecarClaim(t, refreshed, remint.childClientId.String(), identity.SidecarId)
	if byJwt, err := session.ByJwt(); err != nil || byJwt != refreshed {
		t.Fatal("the session does not hold the refreshed re-mint")
	}
	if bearer := remint.pushBearer(t, local); bearer != refreshed {
		t.Fatal("the generation did not adopt the refreshed re-mint")
	}
}
