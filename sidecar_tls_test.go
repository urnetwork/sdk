//go:build !ios_extension && !js

package sdk

// The tls component on sdk hosts and an app's tls/ listens (TLS-M8-01): each host class's args, foreground classes
// running no CT watch; the component waiting for the core's identity, built at the enrollment and again once a
// restart's unlock gives it, with the device key in the keychain the app's key store wraps; tls/ names routed to the
// tls component's front and every other name to a plain listen; certs' audience taken from the space; and the deps
// check over sdk's legs, which keeps acmez and miekg/dns off every app leg, the tls component on each app leg and off
// the iOS extension's, whose make size row it therefore leaves as it was. Every host is a .example name or a local
// test server, and nothing dials out.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"
)

// Each host class asks for its own args: ios and android a foreground host's, with no CT watch, desktop a host's that
// stays up. An args refusal is the component waiting for the enrollment; an unknown class is refused outright.
func TestTlsComponentTakesItsHostClassesArgs(t *testing.T) {
	refusal := errors.New("the core has no network yet")
	for _, c := range []struct {
		hostClass  string
		foreground bool
	}{
		{hostClass: SidecarHostClassIos, foreground: true},
		{hostClass: SidecarHostClassAndroid, foreground: true},
		{hostClass: SidecarHostClassDesktop, foreground: false},
	} {
		asked := []bool{}
		component, err := newSidecarTlsComponentWith(t.Context(), &SidecarSettings{HostClass: c.hostClass}, func(foreground bool) (*sidecar.TlsComponentArgs, error) {
			asked = append(asked, foreground)
			return nil, refusal
		})
		if component != nil || !errors.Is(err, errSidecarComponentNotEnrolled) || !errors.Is(err, refusal) {
			t.Errorf("%s: %v, %v", c.hostClass, component, err)
		}
		if !slices.Equal(asked, []bool{c.foreground}) {
			t.Errorf("%s asked for %v", c.hostClass, asked)
		}
	}
	_, err := newSidecarTlsComponentWith(t.Context(), &SidecarSettings{HostClass: "tv"}, func(foreground bool) (*sidecar.TlsComponentArgs, error) {
		t.Error("an unknown class asked for args")
		return nil, refusal
	})
	if err == nil || errors.Is(err, errSidecarComponentNotEnrolled) {
		t.Errorf("an unknown host class: %v", err)
	}
}

// A tls/ name is the front's, through the tls component: an unenrolled Sidecar's tls/ Listen answers that it runs no
// tls component yet, while any other name reaches the plain listen, which answers the sockets component's unavailable.
func TestSidecarSocketsListenRoutesTlsNamesToTheFront(t *testing.T) {
	for address, tlsName := range map[string]bool{
		"tls/443/www":            true,
		"tls/8443/@":             true,
		"tls/5671/queue":         true,
		"abcdefghij:tls/443/www": true,
		"db/primary":             false,
		"smtp/25/mx":             false,
		"tls/80/www":             false,
		"share/x":                false,
	} {
		if sidecarTlsAddress(address) != tlsName {
			t.Errorf("%s: a tls name %v", address, !tlsName)
		}
	}

	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentSockets)
	settings.Components.Add(SidecarComponentTls)
	local := newTestSidecarWorld(t).openWith(t, settings, &sidecarLocalOptions{})
	if local.component(SidecarComponentTls) != nil {
		t.Fatal("the tls component was built before the enrollment")
	}
	sockets := local.GetSockets()
	if _, err := sockets.Listen("tls/443/www", 0); !errors.Is(err, sidecar.ErrTlsComponentAbsent) {
		t.Errorf("a tls/ Listen before the tls component: %v", err)
	}
	if _, err := sockets.ListenContext(t.Context(), "tls/443/www"); !errors.Is(err, sidecar.ErrTlsComponentAbsent) {
		t.Errorf("a tls/ ListenContext before the tls component: %v", err)
	}
	if _, err := sockets.Listen("db/primary", 0); !errors.Is(err, sidecar.ErrSocketUnavailable) || errors.Is(err, sidecar.ErrTlsComponentAbsent) {
		t.Errorf("a plain Listen: %v", err)
	}
}

// certs' audience follows the space as the services do, an env's prefix and port kept, as an origin with no path.
func TestSidecarCertsResourceFollowsTheSpace(t *testing.T) {
	for _, c := range []struct {
		apiUrl string
		want   string
	}{
		{apiUrl: "https://api.sdktest.example", want: "https://certs.sdktest.example"},
		{apiUrl: "https://test-api.sdktest.example:8443/v1", want: "https://test-certs.sdktest.example:8443"},
	} {
		settings := sidecar.DefaultSidecarSettings()
		if err := applySidecarServiceUrls(newTestSidecarSpace(c.apiUrl, "wss://connect.sdktest.example"), settings); err != nil {
			t.Fatal(err)
		}
		if settings.Tls.CertsResource != c.want {
			t.Errorf("%s gave %q", c.apiUrl, settings.Tls.CertsResource)
		}
	}
	if _, err := sidecarCertsResourceOf("https://sdktest.example", "api"); err == nil {
		t.Error("an api url with no api label")
	}
}

// The core's api for an enrollment of the child clientId: the binding at generation 1, answered with the child's token
// re-minted under the sidecar the binding names (QD-797), and every other route a 404.
func newTestTlsCoreServer(t *testing.T, clientId string) string {
	t.Helper()
	server := newTestSessionServer(func(request testSessionRequest) (int, any) {
		if request.path != "/sidecar/bind" {
			return http.StatusNotFound, map[string]string{"error": "not found"}
		}
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
		reminted := credentialTestJwt(t, gojwt.MapClaims{
			"network_id": credentialTestNetworkId,
			"user_id":    credentialTestUserId,
			"client_id":  clientId,
			"device_id":  credentialTestDeviceId,
			"strict_exp": true,
			"sidecar_id": binding.SidecarId.String(),
		})
		return http.StatusOK, map[string]any{"generation": 1, "by_client_jwt": reminted}
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

// The component on an app host: it waits through the first open, is built at the enrollment with no CT watch, its
// device key sealed in the keychain the app's key store wraps, and after a restart is built again once the unlock
// gives the core its identity, with no enrollment to build it.
func TestTlsComponentBuildsAtEnrollmentAndAgainOnceARestartUnlocks(t *testing.T) {
	world := newTestSidecarWorld(t)
	childClientId := sidecar.NewId()
	coreUrl := newTestTlsCoreServer(t, childClientId.String())
	child := testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 0)
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": childClientId.String(), "admin_refused": true, "strict_exp": true}
		case "/auth/refresh":
			return http.StatusOK, map[string]string{"by_jwt": child}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	ctx, deviceApi := newTestApi(t, deviceServer)
	deviceApi.SetByJwt(dpopTestClientToken(t))
	settings := DefaultSidecarSettings()
	settings.HostClass = SidecarHostClassIos
	settings.Components.Add(SidecarComponentTls)
	options := func(built chan struct{}) *sidecarLocalOptions {
		return &sidecarLocalOptions{
			adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
				settings.ApiUrl = coreUrl
				settings.PlatformUrl = "wss://127.0.0.1:1"
			},
			hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
				return nil
			},
			deferredComponentsBuilt: func() {
				if built != nil {
					built <- struct{}{}
				}
			},
		}
	}

	local := world.openWith(t, settings, options(nil))
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	if local.component(SidecarComponentTls) != nil {
		t.Fatal("the tls component was built before the enrollment")
	}
	if _, err := local.enrollWithDeviceApi(ctx, deviceApi); err != nil {
		t.Fatal(err)
	}
	component, ok := local.component(SidecarComponentTls).(*sidecar.TlsComponent)
	if !ok {
		t.Fatalf("the enrollment built %v", local.component(SidecarComponentTls))
	}
	if status := component.CtWatchStatus(); status.Degraded || !status.PassTime.IsZero() || len(status.Alarms) != 0 {
		t.Errorf("an app host's component reports a CT watch: %+v", status)
	}
	if status := local.Core().Status(); status.CtWatchDegraded {
		t.Error("an app host's status shows a CT watch")
	}
	key, err := local.Core().TlsDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	spkiSha256 := key.SpkiSha256()
	world.close(t, local)

	built := make(chan struct{}, 1)
	reopened := world.openWith(t, settings, options(built))
	select {
	case <-built:
	case <-ctx.Done():
		t.Fatal("the restart never built its waiting components")
	}
	if _, ok := reopened.component(SidecarComponentTls).(*sidecar.TlsComponent); !ok {
		t.Fatalf("the restart built %v", reopened.component(SidecarComponentTls))
	}
	// the same key, read back through the key store's wrap
	key, err = reopened.Core().TlsDeviceKey()
	if err != nil || key.SpkiSha256() != spkiSha256 {
		t.Errorf("the device key did not survive the restart: %v", err)
	}
	world.close(t, reopened)

	// another app's key store opens nothing: no identity, so the component waits and no key is there to read
	stranger := &testSidecarWorld{stateDir: world.stateDir, keyStore: newTestSidecarKeyStore(), space: world.space}
	locked := stranger.openWith(t, settings, options(nil))
	if locked.Core().Identity() != nil || locked.component(SidecarComponentTls) != nil {
		t.Error("another key store opened the instance")
	}
	if _, err := locked.Core().TlsDeviceKey(); err == nil {
		t.Error("the device key read without the app's key store")
	}
}

// The packages each sdk leg resolves, with each one's compiled files: go list -deps on the leg's environment and
// tags, GOFLAGS cleared and GOWORK off as the messaging boundary's legs run.
func sidecarTlsLegFiles(t *testing.T, leg messagingBoundaryLeg) map[string][]string {
	t.Helper()
	arguments := []string{"list", "-deps", "-f", "{{.ImportPath}}{{range .GoFiles}} {{.}}{{end}}"}
	if leg.tags != "" {
		arguments = append(arguments, "-tags", leg.tags)
	}
	arguments = append(arguments, leg.patterns...)
	command := exec.Command("go", arguments...)
	command.Dir = filepath.Join(".", filepath.FromSlash(leg.moduleDirectory))
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	command.Env = append(command.Env, leg.environment...)
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("%s: go %s: %v\n%s", leg.name, strings.Join(arguments, " "), err, stderr)
	}
	files := map[string][]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if 0 < len(fields) {
			files[fields[0]] = fields[1:]
		}
	}
	return files
}

// D-TLS-7 and TLS-M8-01's trap, over sdk's mobile legs: no app leg links an ACME client or miekg/dns, while each
// links the tls component and this file's wrapper, so the check reads a closure that holds them; the iOS extension's
// leg links neither, so §18.10's make size row, which measures that leg, is as it was.
func TestSidecarTlsLegsLinkNoAcmeClientOrDnsLibrary(t *testing.T) {
	refusedRoots := []string{"github.com/mholt/acmez", "github.com/miekg/dns"}
	legCount := 0
	for _, leg := range messagingBoundaryLegs {
		if !strings.HasPrefix(leg.name, "gomobile ") {
			continue
		}
		legCount += 1
		files := sidecarTlsLegFiles(t, leg)
		if _, ok := files["github.com/urnetwork/sdk"]; !ok {
			t.Errorf("CONTROL FAILED: %s resolved %d packages and not the sdk", leg.name, len(files))
			continue
		}
		for packagePath := range files {
			for _, root := range refusedRoots {
				if packagePath == root || strings.HasPrefix(packagePath, root+"/") {
					t.Errorf("%s links %s", leg.name, packagePath)
				}
			}
		}
		rootTlsFiles := slices.DeleteFunc(slices.Clone(files["github.com/urnetwork/sidecar"]), func(file string) bool {
			return !strings.HasPrefix(file, "tls_")
		})
		sdkTls := slices.Contains(files["github.com/urnetwork/sdk"], "sidecar_tls.go")
		extension := strings.Contains(leg.tags, "ios_extension")
		switch {
		case extension && (0 < len(rootTlsFiles) || sdkTls):
			t.Errorf("%s links the tls component: %v, sidecar_tls.go %v", leg.name, rootTlsFiles, sdkTls)
		case !extension && (!slices.Contains(rootTlsFiles, "tls_component.go") || !slices.Contains(rootTlsFiles, "tls_cert_manager.go") || !sdkTls):
			t.Errorf("CONTROL FAILED: %s does not link the tls component: %v, sidecar_tls.go %v", leg.name, rootTlsFiles, sdkTls)
		}
		t.Logf("%s: %d packages, %d of the root's tls files", leg.name, len(files), len(rootTlsFiles))
	}
	if legCount < 4 {
		t.Errorf("%d mobile legs read", legCount)
	}
}
