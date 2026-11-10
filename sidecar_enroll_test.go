//go:build !ios_extension && !js

package sdk

// EnrollWithDevice end to end on a local Sidecar (SDK-M8-01): the device's Api mints the child on its own token, the
// core binds it, and the generation that starts hands its ClientKey's signer to the child token's session, whose
// first refresh carries the proof; then GetSidecarId, GetState and SetPushToken read and post through the core. A
// refused mint keeps nothing and leaves the host's session as it was. Two local servers stand in for api: the device's
// and the core's; the platform is a closed loopback port, so nothing leaves the machine.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The core's api: the binding at generation 1, the push token and the removal answered, every other route a 404.
func newTestCoreServer(t *testing.T) (*testSessionServer, string) {
	t.Helper()
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/sidecar/bind":
			return http.StatusOK, map[string]int64{"generation": 1}
		case "/sidecar/push-token", "/network/remove-client":
			return http.StatusOK, map[string]any{}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return server, httpServer.URL
}

// A local Sidecar whose core reaches coreUrl, whose platform is a closed port, and whose host finds no readable
// network credential, on world.
func openTestEnrollingSidecar(t *testing.T, world *testSidecarWorld, coreUrl string) *Sidecar {
	t.Helper()
	local := world.openWith(t, DefaultSidecarSettings(), &sidecarLocalOptions{
		adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
			settings.ApiUrl = coreUrl
			settings.PlatformUrl = "wss://127.0.0.1:1"
		},
		hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
			return nil
		},
	})
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
	refreshed := testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 1)
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": childClientId.String(), "admin_refused": true, "strict_exp": true}
		case "/auth/refresh":
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
	// the generation handed the core's signer to the session: its first refresh carries the ClientKey's proof
	refresh := deviceServer.await(t, "/auth/refresh")
	requireDpopTestProof(t, refresh.proof, identity.ClientPublicKey, http.MethodGet, deviceApi.apiUrl+"/auth/refresh", child)
	if session := local.host.swapSession(nil); session == nil || session.api.networkByJwt != "" {
		t.Fatal("the host holds no session, or one with a network credential")
	} else {
		local.host.swapSession(session)
	}
	if state := local.GetState(); state != string(sidecar.SidecarStateOffline) && state != string(sidecar.SidecarStateOnline) {
		t.Fatalf("the state after enrollment: %s %s", state, local.GetStateReason())
	}

	// the push token goes through the core, on the child's token
	pushed, pushResults := testSidecarResultCallback()
	local.SetPushToken(SidecarPushPlatformApns, "push-token-canary-sdk", pushed)
	if result := <-pushResults; !result.Ok {
		t.Fatalf("the push token: %+v", result)
	}
	push := coreServer.await(t, "/sidecar/push-token")
	if push.authorization != "Bearer "+child && push.authorization != "Bearer "+refreshed {
		t.Fatal("the push token was not posted on the child's token")
	}
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

	remote := &Sidecar{ctx: context.Background(), remote: &testSidecarRemote{}}
	if _, err := remote.enrollWithDeviceApi(ctx, deviceApi); err != errSidecarRemoteEnroll {
		t.Fatalf("a remote backend enrolled: %v", err)
	}
}
