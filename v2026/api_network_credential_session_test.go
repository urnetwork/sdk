package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// How long the network credential lives beside a device's client token: what
// keeps it, what ends it, and where it comes from after a relaunch.

func requireAdminCallSends(t *testing.T, api *Api, requests chan recordedApiRequest, byJwt string) {
	t.Helper()
	_ = awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
	request, ok := takeRecordedRequest(t, requests)
	if !ok {
		t.Fatal("the admin call sent no request")
	}
	connect.AssertEqual(t, request.path, "/auth/network-delete")
	connect.AssertEqual(t, request.byJwt, byJwt)
}

func requireAdminCallRefused(t *testing.T, api *Api, requests chan recordedApiRequest) {
	t.Helper()
	err := awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
	if !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
	}
	if request, ok := takeRecordedRequest(t, requests); ok {
		t.Fatalf("the admin call was sent: %+v", request)
	}
}

func requireClientCallSends(t *testing.T, api *Api, requests chan recordedApiRequest, byJwt string) {
	t.Helper()
	_ = awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { api.SubscriptionBalance(cb) })
	request, ok := takeRecordedRequest(t, requests)
	if !ok {
		t.Fatal("the client call sent no request")
	}
	connect.AssertEqual(t, request.path, "/subscription/balance")
	connect.AssertEqual(t, request.byJwt, byJwt)
}

// A client token refresh replaces only the client token. The refresh carries
// the client token, never the network credential, and the network credential
// stays beside the refreshed token. /auth/refresh refreshes only a client
// token; the network credential renews separately, and only when a LocalState
// backs it (api_network_credential_renewal_test.go), which this API has none of.
func TestClientRefreshKeepsTheNetworkCredential(t *testing.T) {
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "initial")
	refreshedJwt := credentialTestClientJwt(t, credentialTestNetworkId, "refreshed")
	requests := make(chan recordedApiRequest, 64)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- recordedApiRequest{
			method: r.Method,
			path:   r.URL.Path,
			byJwt:  strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/refresh" {
			fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
	_, api := newTestApi(t, handler)
	refreshed := make(chan string, 1)
	api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)
	api.StartJwtRefresh()

	select {
	case got := <-refreshed:
		connect.AssertEqual(t, got, refreshedJwt)
	case <-time.After(10 * time.Second):
		t.Fatal("the client token was not refreshed")
	}
	refresh, ok := takeRecordedRequest(t, requests)
	if !ok {
		t.Fatal("no refresh request was recorded")
	}
	connect.AssertEqual(t, refresh.path, "/auth/refresh")
	connect.AssertEqual(t, refresh.byJwt, clientJwt)
	if extra, ok := takeRecordedRequest(t, requests); ok {
		t.Fatalf("unexpected request %+v (the network credential is not refreshable)", extra)
	}

	connect.AssertEqual(t, api.GetByJwt(), refreshedJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), true)
	requireAdminCallSends(t, api, requests, networkJwt)
	requireClientCallSends(t, api, requests, refreshedJwt)
}

// A relaunched app calls no SetByJwt: its device starts from LocalState, which
// pairs the client token with the network's sign-in token (the apps save both
// at sign-in). The device adopts that token for the admin routes, and the
// apps' sign-out (api.setByJwt(nil)) ends it.
func TestDeviceStartAdoptsTheNetworkCredentialFromLocalState(t *testing.T) {
	_, api, requests := newCredentialRecordingApi(t)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	localState := newLocalState(context.Background(), t.TempDir())
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	connect.AssertEqual(t, api.HasNetworkCredential(), false)

	installTestDeviceByJwt(t, api, localState, clientJwt, instanceId)
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), true)
	requireAdminCallSends(t, api, requests, networkJwt)
	requireClientCallSends(t, api, requests, clientJwt)
	// LocalState still keeps the two credentials apart
	connect.AssertEqual(t, localState.GetByJwt(), networkJwt)
	connect.AssertEqual(t, localState.GetByClientJwt(), clientJwt)

	api.SetByJwt("")
	connect.AssertEqual(t, api.HasNetworkCredential(), false)
	requireAdminCallRefused(t, api, requests)
}

// Shipped Apple extensions kept their client token in by_jwt. A device that
// starts from that LocalState never takes the client token as the network
// credential.
func TestDeviceStartNeverAdoptsAClientTokenAsTheNetworkCredential(t *testing.T) {
	_, api, requests := newCredentialRecordingApi(t)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "extension")
	localState := newLocalState(context.Background(), t.TempDir())
	instanceId := NewId()
	if err := localState.SetByJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetInstanceId(instanceId); err != nil {
		t.Fatal(err)
	}

	installTestDeviceByJwt(t, api, localState, clientJwt, instanceId)
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), false)
	requireAdminCallRefused(t, api, requests)
}

// A network credential is never sent for another network's device, whether
// the API holds it from a sign-in or LocalState pairs it with the client.
func TestNetworkCredentialOfAnotherNetworkIsDropped(t *testing.T) {
	otherNetworkJwt := credentialTestNetworkJwt(t, credentialTestOtherNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")

	t.Run("signed in", func(t *testing.T) {
		_, api, requests := newCredentialRecordingApi(t)
		api.SetByJwt(otherNetworkJwt)
		installTestDeviceByJwt(t, api, nil, clientJwt, nil)
		connect.AssertEqual(t, api.HasNetworkCredential(), false)
		requireAdminCallRefused(t, api, requests)
	})
	t.Run("local state", func(t *testing.T) {
		_, api, requests := newCredentialRecordingApi(t)
		localState := newLocalState(context.Background(), t.TempDir())
		if err := localState.SetByJwt(otherNetworkJwt); err != nil {
			t.Fatal(err)
		}
		if err := localState.SetByClientJwt(clientJwt); err != nil {
			t.Fatal(err)
		}
		installTestDeviceByJwt(t, api, localState, clientJwt, localState.GetInstanceId())
		connect.AssertEqual(t, api.HasNetworkCredential(), false)
		requireAdminCallRefused(t, api, requests)
	})
}

// The network credential ends with its session: a sign-out, a client token
// that replaces the session through SetByJwt, or the server's confirmed
// rejection of the device's client token (which signs the app out).
func TestTheNetworkCredentialEndsWithItsSession(t *testing.T) {
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	for _, end := range []struct {
		name string
		end  func(t *testing.T, api *Api)
	}{
		{"sign-out", func(t *testing.T, api *Api) {
			api.SetByJwt("")
		}},
		{"client token set through SetByJwt", func(t *testing.T, api *Api) {
			api.SetByJwt(clientJwt)
		}},
		{"client token rejected", func(t *testing.T, api *Api) {
			byJwt, generation := api.authCredentialSnapshot()
			if !api.rejectByJwt(byJwt, generation) {
				t.Fatal("the rejection was not applied")
			}
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			_, api, requests := newCredentialRecordingApi(t)
			api.SetByJwt(networkJwt)
			installTestDeviceByJwt(t, api, nil, clientJwt, nil)
			connect.AssertEqual(t, api.HasNetworkCredential(), true)

			end.end(t, api)
			connect.AssertEqual(t, api.HasNetworkCredential(), false)
			requireAdminCallRefused(t, api, requests)
		})
	}
}

// Closing a device ends its client token, not the sign-in: the network
// credential stays until the app signs out, and the next device keeps it.
func TestDeviceCloseKeepsTheSignInNetworkCredential(t *testing.T) {
	_, api, requests := newCredentialRecordingApi(t)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "first")
	api.SetByJwt(networkJwt)
	owner := installTestDeviceByJwt(t, api, nil, clientJwt, nil)

	api.closeDeviceOwner(owner)
	connect.AssertEqual(t, api.GetByJwt(), "")
	connect.AssertEqual(t, api.HasNetworkCredential(), true)
	requireAdminCallSends(t, api, requests, networkJwt)
	requireClientCallSends(t, api, requests, "")

	nextClientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "next")
	installTestDeviceByJwt(t, api, nil, nextClientJwt, nil)
	requireAdminCallSends(t, api, requests, networkJwt)
	requireClientCallSends(t, api, requests, nextClientJwt)
}

// A hosted device's session API copies the transports, never the parent's
// network credential.
func TestSessionApiDoesNotInheritTheNetworkCredential(t *testing.T) {
	ctx, api, requests := newCredentialRecordingApi(t)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "hosted")
	api.SetByJwt(networkJwt)
	session := api.newSession(ctx)
	t.Cleanup(func() {
		session.Close()
		_ = session.CloseAndWait(context.Background())
	})
	installTestDeviceByJwt(t, session, nil, clientJwt, nil)

	connect.AssertEqual(t, session.HasNetworkCredential(), false)
	requireAdminCallRefused(t, session, requests)
	requireClientCallSends(t, session, requests, clientJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), true)
}

// A DeviceRemote (the Apple app's device) forwards every request over device
// rpc with the credential the API selected for its route.
func TestRemoteDeviceTransportCarriesTheSelectedCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clientStrategy := newTestClientStrategy(ctx)
	api := newApi(ctx, clientStrategy, "https://api.credential.test")
	t.Cleanup(func() {
		api.Close()
		cancel()
		_ = api.CloseAndWait(context.Background())
		clientStrategy.Close()
	})
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "remote")

	type forwardedRequest struct {
		requestUrl string
		byJwt      string
	}
	forwarded := make(chan forwardedRequest, 8)
	httpPostRaw := func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		forwarded <- forwardedRequest{requestUrl: requestUrl, byJwt: byJwt}
		return []byte("{}"), nil
	}
	httpGetRaw := func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		forwarded <- forwardedRequest{requestUrl: requestUrl, byJwt: byJwt}
		return []byte("{}"), nil
	}

	api.SetByJwt(networkJwt)
	owner := newDeviceAuthPublicationGate()
	prepared, err := api.prepareDeviceAuth(nil, clientJwt, nil, time.Now(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.installDeviceRemote(prepared, owner, httpPostRaw, httpGetRaw, nil, connect.DefaultLogger()); err != nil {
		t.Fatal(err)
	}

	_ = awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
	connect.AssertEqual(t, <-forwarded, forwardedRequest{
		requestUrl: "https://api.credential.test/auth/network-delete",
		byJwt:      networkJwt,
	})
	_ = awaitApiCall(func(cb connect.ApiCallback[*GetAccountWalletsResult]) { api.GetAccountWallets(cb) })
	connect.AssertEqual(t, <-forwarded, forwardedRequest{
		requestUrl: "https://api.credential.test/account/wallets",
		byJwt:      networkJwt,
	})
	_ = awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { api.SubscriptionBalance(cb) })
	connect.AssertEqual(t, <-forwarded, forwardedRequest{
		requestUrl: "https://api.credential.test/subscription/balance",
		byJwt:      clientJwt,
	})
}

// The SDK matches a route the way the server's router does: by method and an
// anchored pattern over the path of a request to this API, without its query.
func TestApiRouteAccessMatchesTheServersRouting(t *testing.T) {
	for _, c := range []struct {
		method string
		path   string
		access apiRouteAccess
	}{
		{"POST", "/auth/network-delete", apiRouteAccessAppAdmin},
		{"GET", "/auth/network-delete", apiRouteAccessClient},
		{"POST", "/auth/network-delete/more", apiRouteAccessClient},
		{"POST", "/network/client-data-cap", apiRouteAccessNetwork},
		{"POST", "/auth/network-refresh", apiRouteAccessNetwork},
		{"GET", "/auth/refresh", apiRouteAccessClient},
		{"GET", "/network/client-data-cap", apiRouteAccessClient},
		{"GET", "/device/share-code/code-1/qr.png", apiRouteAccessNetwork},
		{"GET", "/device/share-code/a/b/qr.png", apiRouteAccessClient},
		{"GET", "/subscription/balance", apiRouteAccessClient},
	} {
		connect.AssertEqual(t, apiRouteAccessFor(c.method, c.path), c.access)
	}

	api := &Api{apiUrl: "https://api.credential.test/"}
	for _, c := range []struct {
		requestUrl string
		path       string
		ok         bool
	}{
		{"https://api.credential.test/auth/network-delete", "/auth/network-delete", true},
		{"https://api.credential.test/sn/pool/claim?epoch=7", "/sn/pool/claim", true},
		{"https://api.credential.test.other/auth/network-delete", "", false},
		{"https://elsewhere.test/auth/network-delete", "", false},
		{"https://api.credential.test", "", false},
	} {
		path, ok := api.apiRequestPath(c.requestUrl)
		connect.AssertEqual(t, path, c.path)
		connect.AssertEqual(t, ok, c.ok)
	}
}

// The network credential never leaves this API: a request to any other url
// carries exactly the credential its caller read, admin path or not.
func TestNetworkCredentialIsNeverSentOutsideTheApi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clientStrategy := newTestClientStrategy(ctx)
	api := newApi(ctx, clientStrategy, "https://api.credential.test")
	t.Cleanup(func() {
		api.Close()
		cancel()
		_ = api.CloseAndWait(context.Background())
		clientStrategy.Close()
	})
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)

	for _, byJwt := range []string{clientJwt, ""} {
		selected, err := api.requestByJwt(http.MethodPost, "https://elsewhere.test/auth/network-delete", byJwt)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, selected, byJwt)
	}
	selected, err := api.requestByJwt(http.MethodPost, "https://api.credential.test/auth/network-delete", clientJwt)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, selected, networkJwt)
}

// The client token test the server's gate applies (jwt.ByJwtNamesClientUnverified):
// a token names a client when its claims carry a client_id. A network token,
// an API key, and a bearer that is not a JWT do not.
func TestCredentialNamesClientReadsClaimsLikeTheServer(t *testing.T) {
	for _, c := range []struct {
		name   string
		byJwt  string
		client bool
	}{
		{"client token", credentialTestClientJwt(t, credentialTestNetworkId, "device"), true},
		{"network token", credentialTestNetworkJwt(t, credentialTestNetworkId), false},
		{"api key", apiKeyPrefix + "key", false},
		{"not a jwt", "not-a-jwt", false},
		{"empty", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, credentialNamesClient(c.byJwt), c.client)
			connect.AssertEqual(t, isNetworkCredential(c.byJwt), !c.client && c.byJwt != "")
		})
	}
}

// Every route an Api method requests has a case in apiCredentialCases, so that
// table is the SDK's complete call map. A new API call fails here until its
// route and credential are pinned there.
func TestApiCredentialCasesCoverEverySdkRoute(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range apiCredentialCases() {
		_, routePath, _ := strings.Cut(c.route, " ")
		covered[routePath] = true
	}
	routeLiteral := regexp.MustCompile(`"(?:%s)?(/[A-Za-z0-9_%./-]*[A-Za-z][A-Za-z0-9_%./-]*)(?:\?[^"]*)?"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	routeCount := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), "func (self *Api) ") {
			continue
		}
		for i, line := range strings.Split(string(source), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || !strings.Contains(line, "self.apiUrl") {
				continue
			}
			for _, match := range routeLiteral.FindAllStringSubmatch(line, -1) {
				routeCount += 1
				if !covered[match[1]] {
					t.Errorf("%s:%d: the route %s has no case in apiCredentialCases", file, i+1, match[1])
				}
			}
		}
	}
	if routeCount < 80 {
		t.Fatalf("found only %d API routes in the sdk source; the scan is broken", routeCount)
	}
}

// The SDK's admin route table is the server's App admin and Network only
// classes (server api/route_authz.go), and every other server class takes the
// current credential. A new server class fails here until the sdk decides the
// credential it sends. Runs where the server checkout is beside the sdk.
func TestApiAdminRoutesMatchTheServer(t *testing.T) {
	path := filepath.Join("..", "server", "api", "route_authz.go")
	source, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no server checkout beside the sdk (%s)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`(?m)^\s*"([A-Z]+ [^"]+)":\s*routeAccess(\w+)`)
	entries := entry.FindAllStringSubmatch(string(source), -1)
	if len(entries) < 200 {
		t.Fatalf("parsed only %d routes from %s", len(entries), path)
	}
	serverRoutes := map[string]apiRouteAccess{}
	for _, match := range entries {
		route := strings.ReplaceAll(match[1], `\\`, `\`)
		switch match[2] {
		case "AppAdmin":
			serverRoutes[route] = apiRouteAccessAppAdmin
		case "Network":
			serverRoutes[route] = apiRouteAccessNetwork
		case "Public", "Client", "OwnClient", "OwnClientPayout":
			// the current credential. OwnClientPayout refuses only an Embed
			// network's client token, which its backend never sends there.
			if _, ok := apiAdminRouteAccess[route]; ok {
				t.Errorf("%q is routeAccess%s on the server but an admin route in the sdk", route, match[2])
			}
		default:
			t.Errorf("%q: the server class routeAccess%s is new; decide the credential the sdk sends for it", route, match[2])
		}
	}
	for route, access := range serverRoutes {
		if sdkAccess, ok := apiAdminRouteAccess[route]; !ok {
			t.Errorf("the server's admin route %q is missing from apiAdminRouteAccess", route)
		} else if sdkAccess != access {
			t.Errorf("%q: the sdk class %d differs from the server's %d", route, sdkAccess, access)
		}
	}
	for route := range apiAdminRouteAccess {
		if _, ok := serverRoutes[route]; !ok {
			t.Errorf("apiAdminRouteAccess has %q, which is not an admin route on the server", route)
		}
	}
}
