//go:build !ios_extension && !js

package sdk

// T4's ios32 and Android surrogate cells (SDK-M8-15; §22.4's T4, §22.9's Memory, size row; §18.4's budgets): a cell
// boots the app's sidecar role under its profile, as the app does, and holds its peak and its steady heap with the role
// open to §18.4's budget for the class; the Android cell runs no background sidecar, whatever the VPN service does
// (owner decision 197), and only an OS window or a wake lets it run with the app closed. A cell also enrolls the role,
// registers its wake token, takes a wake with the app closed and reads in the wake's window, each step held to the
// class's budget (SDK-M8-15's Tests). The cells run on this host under the profiles' Go memory limits, and build for an
// Android surrogate as the bind builds the sdk (cmd/t4cell); the surrogate devices' runs of the same cells are external.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// Boots the app's sidecar role on cell's profile under class: its peak while it opens and its steady heap once open,
// each held to the profile's sidecar budget.
func testT4BootCell(t *testing.T, cell *t4cell.Cell, class string) *Sidecar {
	t.Helper()
	world := newTestSidecarWorld(t)
	budget := cell.Profile().SidecarBudgetByteCount
	var local *Sidecar
	if _, err := cell.Measure("sidecar role boots ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		settings := DefaultSidecarSettings()
		settings.HostClass = class
		settings.StateDir = world.stateDir
		settings.Components.Add(SidecarComponentEvs)
		opened, err := newSidecarLocal(context.Background(), world.space, world.keyStore, nil, settings, &sidecarLocalOptions{
			componentFactories:  map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent},
			searchFreeByteCount: func(dir string) (uint64, error) { return 8 * 1024 * 1024 * 1024, nil },
		})
		local = opened
		return opened, err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		world.close(t, local)
	})
	if _, err := cell.Measure("sidecar role open, steady ("+class+")", budget, t4cell.HoldsSteady, func() (any, error) {
		if local.GetSearch() == nil {
			t.Error("the booted role has no search")
		}
		return local, nil
	}); err != nil {
		t.Fatal(err)
	}
	return local
}

// The ios32 cell boots the iOS app's sidecar role within §18.4's 32 MiB, its peak and its steady heap.
func TestIos32CellBootsWithinItsBudget(t *testing.T) {
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	local := testT4BootCell(t, cell, SidecarHostClassIos)
	if local.hostClass.name != SidecarHostClassIos || local.hostClass.memoryBudgetByteCount != cell.Profile().SidecarBudgetByteCount {
		t.Errorf("the role's class %+v", local.hostClass)
	}
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// The Android cell boots the sidecar role within its 16 MiB added, and runs no background sidecar: with the app
// closed and the VPN service running it may not run, and only an OS-granted window or a wake lets it (owner decision
// 197).
func TestAndroidCellRunsNoBackgroundSidecar(t *testing.T) {
	cell := t4cell.NewCell(t4cell.AndroidProfile())
	local := testT4BootCell(t, cell, SidecarHostClassAndroid)
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
	host := local.searchHost
	search := local.GetSearch()
	search.SetForeground(true)
	if !host.mayRun(false) {
		t.Fatal("the app in the foreground may not run its sidecar")
	}
	search.SetForeground(false)
	if host.mayRun(false) {
		t.Error("the Android cell runs a background sidecar with the app closed")
	}
	if local.hostClass.mayRun(false, false, false, true) {
		t.Error("the VPN service running lets the Android sidecar run")
	}
	if !host.mayRun(true) {
		t.Error("a wake does not let the sidecar run")
	}
	closeWindow := host.openWindow()
	if !host.mayRun(false) {
		t.Error("an OS window does not let the sidecar run")
	}
	closeWindow()
	if host.mayRun(false) {
		t.Error("the window outlived its close")
	}
}

// A cell's steps past boot, each held to the profile's sidecar budget: the role enrolls as EnrollWithDevice does,
// against loopback servers standing for the device client's api and the core's; it registers its wake token on its own
// client, as the app does at launch; with the app closed it may not run until a wake, whose window lets it, and it
// reads a file of §18.5's largest write there; the window closed, it may not run again, and its steady heap stays
// within the budget. The read runs on the evs seam over a fake backend, which holds a copy of the file in this
// process, so each figure from that step on is over the role's own by about a mebibyte; the core's own evs reads run
// on sidecartest's world, which the sdk's module does not take on.
func testT4EnrollWakeReadCell(t *testing.T, cell *t4cell.Cell, class string, platform string, token string) {
	t.Helper()
	world := newTestSidecarWorld(t)
	budget := cell.Profile().SidecarBudgetByteCount
	childClientId := sidecar.NewId()
	coreServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
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
			// the generation alone, as server's bind answers since QD-1029
			if binding.ClientId != childClientId {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			return http.StatusOK, map[string]any{"generation": 1}
		case "/sidecar/push-token":
			return http.StatusOK, map[string]any{}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	httpServer := httptest.NewServer(coreServer)
	t.Cleanup(httpServer.Close)
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

	var local *Sidecar
	if _, err := cell.Measure("sidecar role boots ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		settings := DefaultSidecarSettings()
		settings.HostClass = class
		settings.StateDir = world.stateDir
		settings.Components.Add(SidecarComponentEvs)
		opened, err := newSidecarLocal(context.Background(), world.space, world.keyStore, nil, settings, &sidecarLocalOptions{
			adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
				settings.ApiUrl = httpServer.URL
				settings.PlatformUrl = "wss://127.0.0.1:1"
			},
			hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
				return nil
			},
			componentFactories:  map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent},
			searchFreeByteCount: func(dir string) (uint64, error) { return 8 * 1024 * 1024 * 1024, nil },
		})
		local = opened
		return opened, err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		world.close(t, local)
	})
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")

	if _, err := cell.Measure("sidecar role enrolls ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		_, err := local.enrollWithDeviceApi(ctx, deviceApi)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if local.Core().Identity() == nil {
		t.Fatal("the enrollment left no identity")
	}

	// the app open, at launch: its wake token goes to the core's client, and only there
	search := local.GetSearch()
	search.SetForeground(true)
	registered := newTestEvsCallback[*SidecarResult]()
	local.SetPushToken(platform, token, registered)
	if result, err := registered.await(t); err != nil || result == nil || !result.Ok || result.Message != "" && bytes.Contains([]byte(result.Message), []byte(token)) {
		t.Fatalf("the wake token's registration: %v %+v", err, result)
	}
	posted := []sidecar.SidecarPushTokenArgs{}
	for _, request := range coreServer.seen() {
		if request.path != "/sidecar/push-token" {
			continue
		}
		args := sidecar.SidecarPushTokenArgs{}
		if err := json.Unmarshal(request.body, &args); err != nil || request.method != http.MethodPost || request.authorization == "" {
			t.Fatalf("the push token's request: %v, %s, bearer %v", err, request.method, request.authorization != "")
		}
		posted = append(posted, args)
	}
	if len(posted) != 1 || posted[0].Platform != platform || posted[0].Token != token {
		t.Fatalf("the core's api took %d wake tokens: %v", len(posted), posted)
	}

	// the app closed: no run until a wake, whose window lets the role read
	search.SetForeground(false)
	host := local.searchHost
	if host.mayRun(false) {
		t.Fatal("the role runs with the app closed and no wake")
	}
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(4)
	file := bytes.Repeat([]byte{0x5a}, 1024*1024)
	if result := testEvsWrite(t, evs, repoId, "inbox/wake.bin", file, ""); !result.Ok {
		t.Fatalf("the file to read: %+v", result)
	}
	var closeWindow func()
	if _, err := cell.Measure("sidecar role takes a wake ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		closeWindow = host.openWindow()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !host.mayRun(true) || !host.mayRun(false) {
		t.Error("the wake's window does not let the role run")
	}
	if _, err := cell.Measure("sidecar role reads in the wake's window ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		read := testEvsRead(t, evs, repoId, "inbox/wake.bin")
		if !read.Ok || !bytes.Equal(read.Data, file) {
			t.Errorf("the read: ok %v, %d bytes", read.Ok, len(read.Data))
		}
		return read, nil
	}); err != nil {
		t.Fatal(err)
	}
	closeWindow()
	if host.mayRun(false) {
		t.Error("the wake's window outlived its close")
	}
	if _, err := cell.Measure("sidecar role enrolled, steady ("+class+")", budget, t4cell.HoldsSteady, func() (any, error) {
		return local, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The ios32 cell enrolls the iOS app's sidecar role, registers its APNs token, takes a wake with the app closed and
// reads in the wake's window, within §18.4's 32 MiB at each step.
func TestIos32CellEnrollsTakesAWakeAndReads(t *testing.T) {
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	// a device token's 64 hex digits, synthetic
	testT4EnrollWakeReadCell(t, cell, SidecarHostClassIos, SidecarPushPlatformApns, "5ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec05ec0")
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// The Android cell does the same with its FCM token, within the sidecar role's 16 MiB added.
func TestAndroidCellEnrollsTakesAWakeAndReads(t *testing.T) {
	cell := t4cell.NewCell(t4cell.AndroidProfile())
	testT4EnrollWakeReadCell(t, cell, SidecarHostClassAndroid, SidecarPushPlatformFcm, "synthetic-fcm-registration-token:t4cell")
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}
