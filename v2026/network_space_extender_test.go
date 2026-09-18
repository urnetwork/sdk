package sdk

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The extender network of a url-only space (EXTENDER.md F1, E1, E3).
//
// A headless embedder names its endpoints outright rather than carrying a host
// name, so everything the extender network is keyed by is derived from the api
// url instead: the space host under it, the extender dns name beside it, and
// the bundled root keys of that derived host. A url with nothing to derive
// from -- a loopback test server, a single label -- keeps no directory at all,
// which is what makes the provider extender role skip it.

// The api url these tests derive from, and the names that derive from it.
const (
	testUrlSpaceApiUrl      = "https://api.space.example"
	testUrlSpacePlatformUrl = "wss://connect.space.example"
	testUrlSpaceHost        = "space.example"
)

// Installs an in-process resolver and hello on every network client this test
// builds, so a live client never leaves the machine, and turns the client and
// the node on for the test.
func testEnableUrlSpaceExtenderNetwork(t *testing.T) {
	t.Helper()
	testEnableExtenderNode(t)
	extenderNetworkClientEnabled = true
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, nil
		}
		settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
			return &connect.ExtenderHelloResult{}, nil
		}
		// no probe pass: nothing here measures latency, and a probe would
		// dial whatever the directory holds
		settings.ProbeWindowCount = 0
	}
	t.Cleanup(func() {
		extenderNetworkClientEnabled = false
		extenderNetworkClientConfigure = nil
	})
}

// A url-only space whose api url names a real host discovers like any other
// space: it keeps a directory, runs the refresh loop, and joins the mesh as a
// member, all under the host below the api host.
func TestUrlNetworkSpaceRunsTheExtenderNetworkOfTheApiHost(t *testing.T) {
	testEnableUrlSpaceExtenderNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, nil)
	defer networkSpace.close()

	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space kept no extender directory")
	}
	if networkSpace.extenderNetworkClient == nil {
		t.Fatal("the space ran no extender network client")
	}
	extenderNode := networkSpace.getExtenderNode()
	if extenderNode == nil {
		t.Fatal("the space ran no member node")
	}
	if role := networkSpace.GetExtenderStatus().Role; role != ExtenderRoleMember {
		t.Fatalf("role = %s, expected member", role)
	}

	// the names every part is keyed by are the derived ones, not the space's
	// own `custom` placeholder
	if extenderDnsName := networkSpace.GetExtenderDnsName(); extenderDnsName != "extender."+testUrlSpaceHost {
		t.Errorf("extender dns name = %q", extenderDnsName)
	}
	if gossipUrl := networkSpace.GetGossipUrl(); gossipUrl != "wss://gossip."+testUrlSpaceHost {
		t.Errorf("gossip url = %q", gossipUrl)
	}
	if networkHostName := extenderNetworkHostName(&networkSpace.key, &networkSpace.values); networkHostName != testUrlSpaceHost {
		t.Errorf("extender network host = %q", networkHostName)
	}
	// what the relay may forward to is the space under the api url (A5)
	allowedHosts := networkSpace.extenderAllowedHosts()
	for _, allowedHost := range []string{testUrlSpaceHost, "*." + testUrlSpaceHost} {
		if !slices.Contains(allowedHosts, allowedHost) {
			t.Errorf("allowed hosts = %v, expected %q among them", allowedHosts, allowedHost)
		}
	}
	// the space's own host name is unchanged: a url-only space stays `custom`
	// to everything that is not the extender network
	if hostName := networkSpace.GetHostName(); hostName != "custom" {
		t.Errorf("host name = %q, expected the url-only space to keep its own", hostName)
	}
}

// The trust anchor of a url-only space is the bundled table entry of the host
// derived from its api url, so a headless embedder verifies the first records
// it hears exactly as an app does (B4, F1).
func TestUrlNetworkSpaceTakesTheBundledRootKeysOfTheApiHost(t *testing.T) {
	key := NetworkSpaceKey{HostName: "custom", EnvName: "custom"}
	for hostName, rootPublicKeyHexes := range bundledExtenderRootPublicKeyHexes {
		values := NetworkSpaceValues{ApiUrl: "https://api." + hostName}
		if got := ExtenderRootPublicKeys(&key, &values); !slices.Equal(got, rootPublicKeyHexes) {
			t.Errorf("the api.%s space resolves %v, expected %v", hostName, got, rootPublicKeyHexes)
		}
	}
	// a host the table does not name accepts no record until its first hello
	values := NetworkSpaceValues{ApiUrl: testUrlSpaceApiUrl}
	if rootPublicKeys := ExtenderRootPublicKeys(&key, &values); len(rootPublicKeys) != 0 {
		t.Errorf("the %s space resolves %v, expected none", testUrlSpaceApiUrl, rootPublicKeys)
	}
}

// A url-only space whose api url has no host to derive from keeps no extender
// network at all: there is nothing to resolve, nothing to key a record by, and
// the provider extender role skips it (F1, G2).
func TestUrlNetworkSpaceWithoutADerivableHostRunsNothing(t *testing.T) {
	testEnableUrlSpaceExtenderNetwork(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	apiUrls := []string{
		"http://127.0.0.1:8080",
		"http://[2001:db8::1]:8080",
		"http://localhost:8080",
		// a bare space host has no service label, so no extender dns name
		// derives; there is nothing to bootstrap from
		"https://example",
	}
	for _, apiUrl := range apiUrls {
		networkSpace := NewNetworkSpaceWithUrls(ctx, apiUrl, "ws://127.0.0.1:8081", nil)
		if networkSpace.extenderDirectory != nil {
			t.Errorf("%s kept an extender directory", apiUrl)
		}
		if networkSpace.extenderNetworkClient != nil {
			t.Errorf("%s ran an extender network client", apiUrl)
		}
		if networkSpace.getExtenderNode() != nil {
			t.Errorf("%s ran a member node", apiUrl)
		}
		if extenderDnsName := networkSpace.GetExtenderDnsName(); extenderDnsName != "" {
			t.Errorf("%s derived the extender dns name %q", apiUrl, extenderDnsName)
		}
		networkSpace.close()
	}
}

// The extender identity of a space with no local state is stable for the life
// of the space, and an embedder's seed replaces the generated one (B1, G2).
func TestUrlNetworkSpaceExtenderIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, nil)
	defer networkSpace.close()

	generatedKeySeed := networkSpace.extenderIdentityKeySeed()
	if len(generatedKeySeed) == 0 {
		t.Fatal("the space created no extender identity")
	}
	if !slices.Equal(networkSpace.extenderIdentityKeySeed(), generatedKeySeed) {
		t.Fatal("the space created a second extender identity")
	}

	embedderKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	networkSpace.setExtenderKeySeed(embedderKeySeed)
	if !slices.Equal(networkSpace.extenderIdentityKeySeed(), embedderKeySeed) {
		t.Fatal("the embedder's extender identity was not installed")
	}
}

// A space with local state keeps its own `.extender_key`, which wins over
// anything an embedder supplies (B1).
func TestStoredNetworkSpaceKeepsItsOwnExtenderIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer networkSpace.close()

	storedKeySeed := networkSpace.extenderIdentityKeySeed()
	if len(storedKeySeed) == 0 {
		t.Fatal("the space created no extender identity")
	}
	embedderKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	networkSpace.setExtenderKeySeed(embedderKeySeed)
	if !slices.Equal(networkSpace.extenderIdentityKeySeed(), storedKeySeed) {
		t.Fatal("an embedder replaced the persisted extender identity")
	}
}

// A change that lands before a space's extender status watch runs is still
// pushed (F2, K5): every channel the watch waits on first is armed before its
// goroutine starts. The barrier holds the watch goroutine until the change has
// landed.
func TestNetworkSpaceExtenderStatusWatchKeepsAChangeBeforeItRuns(t *testing.T) {
	const apiUrl = "https://api.watch.example"
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWatch := func() {
		releaseOnce.Do(func() { close(release) })
	}
	hook := func(networkSpace *NetworkSpace) {
		// only this test's space waits
		if networkSpace.apiUrl != apiUrl && networkSpace.values.ApiUrl != apiUrl {
			return
		}
		close(entered)
		<-release
	}
	testingBeforeExtenderStatusWatch.Store(&hook)
	t.Cleanup(func() {
		testingBeforeExtenderStatusWatch.Store(nil)
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	networkSpace := NewNetworkSpaceWithUrls(ctx, apiUrl, "wss://connect.watch.example", strategySettings)
	t.Cleanup(networkSpace.Close)
	// released before the space closes
	t.Cleanup(releaseWatch)
	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space keeps no extender directory, so it runs no watch")
	}

	statuses := make(chan *ExtenderStatus, 4)
	sub := networkSpace.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	defer sub.Close()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the extender status watch never started")
	}
	// the change lands while the watch goroutine exists and has not waited on
	// anything yet
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)
	releaseWatch()

	select {
	case status := <-statuses:
		connect.AssertEqual(t, status.KnownCount, 1)
	case <-time.After(10 * time.Second):
		t.Fatal("a change that landed before the watch ran was never pushed")
	}
}
