//go:build !ios_extension

package sdk

// pro_features_view_controller_test.go -- the network's Pro features in use
// read from its proxies list (pro_features_view_controller.go), and the same
// status in the device list (manage_devices_view_controller.go).
//
// The api runs against an in-process stand-in for GET /network/proxies. Waits
// are on the controllers' own listener calls; the refresh interval's timer is
// the test's.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
)

// testingProFeaturesServer answers GET /network/proxies with its proxies and
// plan, and records the credential of each request.
type testingProFeaturesServer struct {
	stateLock   sync.Mutex
	proxies     []*NetworkClientInfo
	plan        *NetworkProxiesPlan
	credentials []string
	gets        int
	// a status to answer with, 0 for 200
	status int
}

func (self *testingProFeaturesServer) install(t *testing.T, api *Api) {
	api.setHttpGetRaw(func(_ context.Context, requestUrl string, byJwt string) ([]byte, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !strings.HasSuffix(requestUrl, "/network/proxies") {
			t.Errorf("unexpected get %s", requestUrl)
		}
		self.credentials = append(self.credentials, byJwt)
		self.gets += 1
		if self.status != 0 {
			return nil, &connect.HttpStatusError{StatusCode: self.status, Status: http.StatusText(self.status)}
		}
		list := NewNetworkClientInfoList()
		list.addAll(self.proxies...)
		return json.Marshal(&NetworkClientsResult{Clients: list, Plan: self.plan})
	})
}

func (self *testingProFeaturesServer) set(proxies []*NetworkClientInfo, plan *NetworkProxiesPlan, status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.proxies = proxies
	self.plan = plan
	self.status = status
}

func (self *testingProFeaturesServer) getCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.gets
}

func (self *testingProFeaturesServer) sentCredentials() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.credentials...)
}

// a hosted proxy that holds the given Pro-only features
func testingProFeaturesProxy(features ...string) *NetworkClientInfo {
	proFeatures := NewStringList()
	for _, feature := range features {
		proFeatures.Add(feature)
	}
	return &NetworkClientInfo{
		ClientId:    NewId(),
		ProxyClient: &ProxyClient{SocksProxyUrl: "socks5h://proxy.example:1080"},
		ProFeatures: proFeatures,
		Connections: NewNetworkClientConnectionList(),
	}
}

type testingProFeaturesListener struct {
	snapshots chan *ProFeaturesSnapshot
}

func newTestingProFeaturesListener() *testingProFeaturesListener {
	return &testingProFeaturesListener{snapshots: make(chan *ProFeaturesSnapshot, 64)}
}

func (self *testingProFeaturesListener) ProFeaturesChanged(snapshot *ProFeaturesSnapshot) {
	self.snapshots <- snapshot
}

// until waits for a snapshot that satisfies done, or fails.
func (self *testingProFeaturesListener) until(t *testing.T, message string, done func(*ProFeaturesSnapshot) bool) *ProFeaturesSnapshot {
	t.Helper()
	for {
		select {
		case snapshot := <-self.snapshots:
			if done(snapshot) {
				return snapshot
			}
		case <-time.After(30 * time.Second):
			t.Fatal(message)
			return nil
		}
	}
}

// settings whose refresh interval fires only when the test sends on ticks
func testingProFeaturesSettings(ticks chan time.Time) *proFeaturesSettings {
	settings := defaultProFeaturesSettings()
	settings.after = func(time.Duration) <-chan time.Time {
		return ticks
	}
	return settings
}

// A free network's proxies holding SOCKS and WireGuard, before enforcement:
// the list and the main screen both show the upgrade, and the read carries
// the network credential the api keeps.
func TestProFeaturesViewControllerShowsTheUpgradeBeforeEnforcement(t *testing.T) {
	server := &testingProFeaturesServer{}
	server.set(
		[]*NetworkClientInfo{
			testingProFeaturesProxy(ProFeatureSocksProxy, ProFeatureWireguardProxy),
			testingProFeaturesProxy(ProFeatureSocksProxy),
		},
		&NetworkProxiesPlan{},
		0,
	)
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(make(chan time.Time)))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	before := vc.GetSnapshot()
	connect.AssertEqual(t, before.Status.Prompt, ProFeaturesPromptNone)
	connect.AssertEqual(t, before.MainPrompt, ProFeaturesPromptNone)

	vc.Start()
	snapshot := listener.until(t, "the status never loaded", func(s *ProFeaturesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, snapshot.ErrorId, ProFeaturesErrorNone)
	connect.AssertEqual(t, snapshot.Status.Prompt, ProFeaturesPromptUpgrade)
	connect.AssertEqual(t, snapshot.Status.SocksProxyCount, 2)
	connect.AssertEqual(t, snapshot.Status.WireguardProxyCount, 1)
	connect.AssertEqual(t, snapshot.MainPrompt, ProFeaturesPromptUpgrade)
	for _, credential := range server.sentCredentials() {
		connect.AssertEqual(t, credential, testingManageDevicesNetworkJwt)
	}
}

// A dismissal withdraws the main screen's notice and leaves the list's prompt.
// It holds through refreshes of the same prompt, and enforcement turning on
// shows the notice again with what stopped. An upgrade ends both.
func TestProFeaturesViewControllerDismissHoldsUntilThePromptChanges(t *testing.T) {
	server := &testingProFeaturesServer{}
	proxies := []*NetworkClientInfo{testingProFeaturesProxy(ProFeatureSocksProxy)}
	server.set(proxies, &NetworkProxiesPlan{}, 0)
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(make(chan time.Time)))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	vc.Start()
	listener.until(t, "the upgrade never showed", func(s *ProFeaturesSnapshot) bool {
		return s.MainPrompt == ProFeaturesPromptUpgrade
	})

	vc.DismissMainNotice()
	dismissed := listener.until(t, "the dismissal never published", func(s *ProFeaturesSnapshot) bool {
		return s.MainPrompt == ProFeaturesPromptNone
	})
	connect.AssertEqual(t, dismissed.Status.Prompt, ProFeaturesPromptUpgrade)

	vc.Refresh()
	refreshed := listener.until(t, "the refresh never loaded", func(s *ProFeaturesSnapshot) bool { return !s.Loading })
	connect.AssertEqual(t, refreshed.MainPrompt, ProFeaturesPromptNone)
	connect.AssertEqual(t, refreshed.Status.Prompt, ProFeaturesPromptUpgrade)

	server.set(proxies, &NetworkProxiesPlan{ProFeaturesEnforced: true}, 0)
	vc.Refresh()
	stopped := listener.until(t, "what stopped never showed", func(s *ProFeaturesSnapshot) bool {
		return s.MainPrompt == ProFeaturesPromptStopped
	})
	connect.AssertEqual(t, stopped.Status.Prompt, ProFeaturesPromptStopped)

	server.set(proxies, &NetworkProxiesPlan{Pro: true, ProFeaturesEnforced: true}, 0)
	vc.Refresh()
	upgraded := listener.until(t, "the upgrade never ended the notice", func(s *ProFeaturesSnapshot) bool {
		return !s.Loading && s.Status.Pro
	})
	connect.AssertEqual(t, upgraded.Status.Prompt, ProFeaturesPromptNone)
	connect.AssertEqual(t, upgraded.MainPrompt, ProFeaturesPromptNone)
}

// A failed read keeps the last status and says so; the next good read clears
// the error.
func TestProFeaturesViewControllerKeepsTheLastStatusOnAFailedRead(t *testing.T) {
	server := &testingProFeaturesServer{}
	proxies := []*NetworkClientInfo{testingProFeaturesProxy(ProFeatureWireguardProxy)}
	server.set(proxies, &NetworkProxiesPlan{}, 0)
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(make(chan time.Time)))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	vc.Start()
	listener.until(t, "the status never loaded", func(s *ProFeaturesSnapshot) bool { return s.Loaded && !s.Loading })

	server.set(proxies, &NetworkProxiesPlan{}, http.StatusInternalServerError)
	vc.Refresh()
	failed := listener.until(t, "the failure never published", func(s *ProFeaturesSnapshot) bool {
		return s.ErrorId == ProFeaturesErrorLoadFailed
	})
	connect.AssertEqual(t, failed.Status.Prompt, ProFeaturesPromptUpgrade)
	connect.AssertEqual(t, failed.MainPrompt, ProFeaturesPromptUpgrade)

	server.set(proxies, &NetworkProxiesPlan{}, 0)
	vc.Refresh()
	listener.until(t, "the next read never cleared the error", func(s *ProFeaturesSnapshot) bool {
		return !s.Loading && s.ErrorId == ProFeaturesErrorNone
	})

	server.set(proxies, &NetworkProxiesPlan{}, http.StatusForbidden)
	vc.Refresh()
	listener.until(t, "a refused credential never asked for a sign-in", func(s *ProFeaturesSnapshot) bool {
		return s.ErrorId == ProFeaturesErrorSignInRequired
	})
}

// A server before the plan reports none, and the notice stays off.
func TestProFeaturesViewControllerOlderServerHasNoNotice(t *testing.T) {
	server := &testingProFeaturesServer{}
	server.set([]*NetworkClientInfo{testingProFeaturesProxy(ProFeatureSocksProxy)}, nil, 0)
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(make(chan time.Time)))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	vc.Start()
	snapshot := listener.until(t, "the status never loaded", func(s *ProFeaturesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, snapshot.Status.PlanKnown, false)
	connect.AssertEqual(t, snapshot.Status.Prompt, ProFeaturesPromptNone)
	connect.AssertEqual(t, snapshot.MainPrompt, ProFeaturesPromptNone)
}

// While open it reads the list again every refresh interval, so an app that
// stays open learns that enforcement turned on. Start twice runs one loop.
func TestProFeaturesViewControllerRefreshesEveryInterval(t *testing.T) {
	server := &testingProFeaturesServer{}
	proxies := []*NetworkClientInfo{testingProFeaturesProxy(ProFeatureSocksProxy)}
	server.set(proxies, &NetworkProxiesPlan{}, 0)
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	ticks := make(chan time.Time)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(ticks))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	vc.Start()
	vc.Start()
	listener.until(t, "the status never loaded", func(s *ProFeaturesSnapshot) bool {
		return s.Loaded && s.Status.Prompt == ProFeaturesPromptUpgrade
	})
	connect.AssertEqual(t, server.getCount(), 1)

	server.set(proxies, &NetworkProxiesPlan{ProFeaturesEnforced: true}, 0)
	select {
	case ticks <- time.Now():
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh loop never waited on the interval")
	}
	listener.until(t, "the interval never read the list again", func(s *ProFeaturesSnapshot) bool {
		return s.Status.Prompt == ProFeaturesPromptStopped
	})
	connect.AssertEqual(t, server.getCount(), 2)
}

// An api holding only a client token can't read the network's proxies:
// nothing is sent, and the controller asks for a new sign-in.
func TestProFeaturesViewControllerWithoutTheNetworkCredentialSendsNothing(t *testing.T) {
	clientToken, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"network_id": NewId().String(),
		"client_id":  NewId().String(),
	}).SignedString([]byte("synthetic-pro-features-key"))
	if err != nil {
		t.Fatal(err)
	}
	server := &testingProFeaturesServer{}
	api := newTestingManageDevicesApi(t, clientToken)
	server.install(t, api)
	vc := newProFeaturesViewController(t.Context(), api, testingProFeaturesSettings(make(chan time.Time)))
	defer vc.Close()
	listener := newTestingProFeaturesListener()
	defer vc.AddProFeaturesListener(listener).Close()

	vc.Start()
	snapshot := listener.until(t, "the controller never asked for a sign-in", func(s *ProFeaturesSnapshot) bool {
		return s.ErrorId == ProFeaturesErrorSignInRequired
	})
	connect.AssertEqual(t, snapshot.Loaded, false)
	connect.AssertEqual(t, server.getCount(), 0)
}

// The device list, where the apps list the network's proxies, says the same
// status from the proxies it reads.
func TestManageDevicesSaysTheProFeaturesInUse(t *testing.T) {
	proxy := testingProFeaturesProxy(ProFeatureSocksProxy, ProFeatureWireguardProxy)
	server := &testingManageDevicesServer{
		proxies:     []*NetworkClientInfo{proxy},
		proxiesPlan: &NetworkProxiesPlan{ProFeaturesEnforced: true},
	}
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	vc := newManageDevicesViewControllerWithApi(t.Context(), api, defaultManageDevicesSettings())
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()

	connect.AssertEqual(t, vc.GetSnapshot().ProFeatures.Prompt, ProFeaturesPromptNone)
	vc.Start()
	snapshot := listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, snapshot.ProFeatures.Prompt, ProFeaturesPromptStopped)
	connect.AssertEqual(t, snapshot.ProFeatures.SocksProxyCount, 1)
	connect.AssertEqual(t, snapshot.ProFeatures.WireguardProxyCount, 1)
}
