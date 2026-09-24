package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The ownership of the extender network (EXTENDER.md E1, F1, F2, K6).
//
// The space owns three things that outlive a single call -- the refresh loop,
// the gossip node and the status watch -- and a settings change replaces the
// first two in place while a device and a view controller hold the space. Both
// halves fail silently when they are wrong: a close that does not join leaves
// a loop writing into a directory whose local state is gone, and a watch that
// does not re-subscribe stops publishing without ever saying so.

// Turns the member role's node on for one test. It lives here, untagged,
// because the tests that need it are not all on one build: the js build runs
// no node at all but still reads the switch.
func testEnableExtenderNode(t *testing.T) {
	t.Helper()
	extenderNodeEnabled = true
	t.Cleanup(func() {
		extenderNodeEnabled = false
	})
}

// Closing a space joins the refresh loop and the node it owns and leaves
// nothing behind that a later settings change could restart (F1, K6).
func TestNetworkSpaceCloseJoinsTheExtenderNetwork(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	testEnableExtenderNode(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{},
		t.TempDir(),
	)
	if networkSpace.getExtenderNetworkClient() == nil {
		t.Fatal("the space ran no extender network client")
	}
	if networkSpace.getExtenderNode() == nil {
		t.Fatal("the member role ran no node")
	}
	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space has no extender directory")
	}

	networkSpace.Close()
	if networkSpace.getExtenderNetworkClient() != nil {
		t.Fatal("the network client outlived the space")
	}
	if networkSpace.getExtenderNode() != nil {
		t.Fatal("the node outlived the space")
	}
	// closing twice joins once and does not panic on the fields it took
	networkSpace.Close()

	// a settings change that lands after the close installs nothing: the
	// replacement would be a loop nothing will ever join
	values := networkSpace.valuesCopy()
	values.ExtenderHosts = []string{"192.0.2.1"}
	networkSpace.applyExtenderValues(&values)
	if networkSpace.getExtenderNetworkClient() != nil {
		t.Fatal("a settings change installed a network client into a closed space")
	}
	if networkSpace.getExtenderNode() != nil {
		t.Fatal("a settings change installed a node into a closed space")
	}
	// the status is still answerable, because every app holds one across a
	// teardown it did not order
	if status := networkSpace.GetExtenderStatus(); status == nil || status.Extenders == nil {
		t.Fatal("a closed space reported no status at all")
	}
}

// Space and device teardown releases every goroutine it started, so an app
// that rebuilds its space -- a value change, a logout, an environment switch
// -- does not accumulate a refresh loop, a node and two status watches per
// cycle (F1, F2, F3).
func TestExtenderSpaceAndDeviceChurnLeaksNoGoroutines(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	testEnableExtenderNode(t)

	const cycles = 4
	// per goroutine-stack signature; a per-cycle leak shows as ~cycles
	const goroutineStackTolerance = 3

	cycle := func() {
		networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
		networkSpace := networkSpaceManager.updateNetworkSpace(
			NewNetworkSpaceKey("space.example", "main"),
			func(values *NetworkSpaceValues) {
				values.ExtenderHosts = []string{"192.0.2.1"}
			},
		)
		if networkSpace.getExtenderNode() == nil {
			t.Fatal("the member role ran no node")
		}
		deviceLocal, err := newDeviceLocalWithOverrides(
			networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
		)
		if err != nil {
			t.Fatal(err)
		}
		// a real settings change, which replaces the refresh loop and rebuilds
		// the node in place: the replaced pair has to be joined too
		networkSpaceManager.updateNetworkSpace(
			NewNetworkSpaceKey("space.example", "main"),
			func(values *NetworkSpaceValues) {
				values.ExtenderHosts = []string{"192.0.2.1", "198.51.100.7"}
			},
		)
		networkSpace.extenderDirectory.AddBootstrap(
			netip.MustParseAddr("203.0.113.42"),
			connect.ExtenderSourceDns,
		)
		deviceLocal.Close()
		networkSpaceManager.Close()
	}

	// one warm cycle so every lazily created process-global goroutine is in
	// the baseline rather than counted as a leak
	cycle()
	sampleStable()
	baseStacks := captureGoroutineStacks()

	for range cycles {
		cycle()
	}
	sampleStable()
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), goroutineStackTolerance)
}

// The status watch waits on the refresh loop the space runs right now, so a
// settings change that replaces it has to re-subscribe. A watch left on the
// closed client keeps publishing directory changes and never publishes another
// feed change again, which no app can tell from a quiet network (F2, K6).
func TestExtenderStatusListenerSurvivesASettingsRestart(t *testing.T) {
	recorder := testEnableExtenderManualHostsNetwork(t)

	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})

	statuses := make(chan *ExtenderStatus, 16)
	sub := networkSpace.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	t.Cleanup(sub.Close)

	// The watch subscribes to the directory on its own goroutine, so a change
	// published before it is armed is not an edge it ever sees. The change is
	// therefore re-published -- a fresh address each time, since a repeat of
	// one the directory already holds is no change at all -- until a callback
	// arrives, rather than waiting out a deadline on one edge that may have
	// predated the watch.
	serial := 0
	waitKnownCount := func(minimum int) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			select {
			case status := <-statuses:
				if minimum <= status.KnownCount {
					return
				}
			case <-time.After(1 * time.Second):
				if deadline.Before(time.Now()) {
					t.Fatalf(
						"the status never reported %d known addresses, last = %+v",
						minimum,
						networkSpace.GetExtenderStatus(),
					)
				}
				serial += 1
				networkSpace.extenderDirectory.AddBootstrap(
					netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", serial)),
					connect.ExtenderSourceDns,
				)
			}
		}
	}

	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.10"),
		connect.ExtenderSourceDns,
	)
	waitKnownCount(1)

	previousNetworkClient := networkSpace.getExtenderNetworkClient()
	previousBuilds := recorder.count()
	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"bootstrap.example"}
	})
	if networkSpace.getExtenderNetworkClient() == previousNetworkClient {
		t.Fatal("the settings change did not restart the network client")
	}
	if recorder.count() != previousBuilds+1 {
		t.Fatalf("client builds = %d, expected exactly one more", recorder.count())
	}

	// the same listener, on the same space, still carries what the directory
	// does after the swap
	known := networkSpace.GetExtenderStatus().KnownCount
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.11"),
		connect.ExtenderSourceDns,
	)
	waitKnownCount(known + 1)
}

// The controller subscribes on Start, releases on Stop, and Close leaves
// nothing subscribed however the app got there (K5).
func TestExtenderViewControllerStartStopAndClose(t *testing.T) {
	vc, networkSpace, _ := testExtenderViewController(t)
	// the subscription is lock-guarded state of the controller, so it is read
	// the way the controller reads it
	subscription := func() Sub {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		return vc.extenderStatusChangedSub
	}

	statuses := make(chan *ExtenderStatus, 16)
	vc.AddStatusListener(extenderViewControllerListenerFunc(func(status *ExtenderStatus) {
		select {
		case statuses <- status:
		default:
		}
	}))

	// nothing is subscribed before Start, so a directory change reaches no ui
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.10"),
		connect.ExtenderSourceDns,
	)

	vc.Start()
	// Start seeds the ui with the current state rather than waiting for the
	// next change, which is what an app renders on open
	select {
	case status := <-statuses:
		if status.KnownCount != 1 {
			t.Fatalf("seeded status known = %d, expected the current directory", status.KnownCount)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not seed the ui with the current status")
	}
	if subscription() == nil {
		t.Fatal("Start did not subscribe to the device")
	}
	// Start twice keeps the one subscription rather than stacking a second
	started := subscription()
	vc.Start()
	if subscription() != started {
		t.Fatal("a second Start replaced the subscription")
	}

	// re-published until a callback arrives, for the reason above: the space's
	// watch is armed on its own goroutine
	serial := 0
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.11"),
		connect.ExtenderSourceDns,
	)
	deadline := time.Now().Add(60 * time.Second)
	for reported := 0; reported < 2; {
		select {
		case status := <-statuses:
			reported = status.KnownCount
		case <-time.After(1 * time.Second):
			if deadline.Before(time.Now()) {
				t.Fatal("a directory change did not reach the controller's listeners")
			}
			serial += 1
			networkSpace.extenderDirectory.AddBootstrap(
				netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", serial)),
				connect.ExtenderSourceDns,
			)
		}
	}

	vc.Stop()
	if subscription() != nil {
		t.Fatal("Stop left the device subscription in place")
	}
	// Stop twice is the app closing a screen it already closed
	vc.Stop()

	// and Close after a Start releases it too
	vc.Start()
	if subscription() == nil {
		t.Fatal("Start after Stop did not subscribe again")
	}
	vc.Close()
	if subscription() != nil {
		t.Fatal("Close left the device subscription in place")
	}
}

type extenderViewControllerListenerFunc func(status *ExtenderStatus)

func (self extenderViewControllerListenerFunc) ExtenderStatusChanged(status *ExtenderStatus) {
	self(status)
}

// The controller is owned by the view controller manager like every other one,
// so an app that opens it and closes the device leaks nothing (K5).
func TestOpenExtenderViewControllerIsManaged(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	vc := device.OpenExtenderViewController()
	if vc == nil {
		t.Fatal("OpenExtenderViewController returned nil")
	}
	if count := openedViewControllerCount(device); count != 1 {
		t.Fatalf("opened view controllers = %d, want 1", count)
	}
	// the `test` space keys no extender network, so the controller answers the
	// empty surface rather than failing
	if status := vc.GetStatus(); status == nil || status.Extenders == nil {
		t.Fatal("the controller reported no status at all")
	}
	if settings := vc.GetSettings(); settings.NetworkHost != "" {
		t.Fatalf("network host = %q, expected none for a space that keys no network", settings.NetworkHost)
	}
	if share := vc.BuildShare(false); share.Text != "" || share.Count != 0 {
		t.Fatalf("share = %+v, expected nothing to share", share)
	}

	device.CloseViewController(vc)
	requireNoOwnedViewControllers(t, device)
}

// A space built directly, with no manager to persist through -- a headless
// embedder, the sn miner -- applies an extender settings change to itself
// rather than dropping it (K6).
func TestExtenderValuesApplyWithoutANetworkSpaceManager(t *testing.T) {
	recorder := testEnableExtenderManualHostsNetwork(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{},
		t.TempDir(),
	)
	t.Cleanup(networkSpace.Close)
	if networkSpace.getNetworkSpaceManager() != nil {
		t.Fatal("a directly built space carries a manager")
	}

	previousNetworkClient := networkSpace.getExtenderNetworkClient()
	if previousNetworkClient == nil {
		t.Fatal("the space ran no extender network client")
	}
	if !networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.1"}
	}) {
		t.Fatal("a real change reported none")
	}
	if networkSpace.getExtenderNetworkClient() == previousNetworkClient {
		t.Fatal("the network client was not restarted")
	}
	if manual := recorder.last(); len(manual) != 1 || manual[0] != "192.0.2.1" {
		t.Fatalf("manual hosts = %v, expected the configured one", manual)
	}
	if hosts := networkSpace.GetExtenderHosts(); hosts.Len() != 1 || hosts.Get(0) != "192.0.2.1" {
		t.Fatalf("hosts = %v, expected the applied value", hosts.getAll())
	}

	// and an edit that resolves to the same values restarts nothing
	previousNetworkClient = networkSpace.getExtenderNetworkClient()
	previousBuilds := recorder.count()
	if networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{" 192.0.2.1 ", ""}
	}) {
		t.Fatal("a whitespace-only edit reported a change")
	}
	if networkSpace.getExtenderNetworkClient() != previousNetworkClient {
		t.Fatal("a whitespace-only edit restarted the network client")
	}
	if recorder.count() != previousBuilds {
		t.Fatalf("client builds = %d, expected none", recorder.count())
	}
}

// The rpc mirror holds ONE device-side subscription however many remote
// listeners there are, and releases it when the last one goes (K5). A
// subscription that is never released keeps the device publishing an extender
// status into a session nothing reads; one released too early stops publishing
// while a panel is still open.
func TestExtenderStatusRpcListenerSubscriptionLifecycle(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	server, client := testingPreferenceRpc(t, deviceLocal)

	subscribed := func() bool {
		server.stateLock.Lock()
		defer server.stateLock.Unlock()
		return server.extenderStatusChangeListenerSub != nil
	}
	listenerCall := func(method string, listenerId connect.Id) {
		t.Helper()
		var void RpcVoid
		if err := testingPreferenceRpcCall(t, client, method, listenerId, &void); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	if subscribed() {
		t.Fatal("the mirror subscribed before any listener was added")
	}
	firstListenerId := connect.NewId()
	secondListenerId := connect.NewId()
	listenerCall("DeviceLocalRpc.AddExtenderStatusChangeListener", firstListenerId)
	if !subscribed() {
		t.Fatal("the mirror did not subscribe for the first listener")
	}
	listenerCall("DeviceLocalRpc.AddExtenderStatusChangeListener", secondListenerId)
	listenerCall("DeviceLocalRpc.RemoveExtenderStatusChangeListener", firstListenerId)
	if !subscribed() {
		t.Fatal("the mirror released the subscription while a listener was still attached")
	}
	listenerCall("DeviceLocalRpc.RemoveExtenderStatusChangeListener", secondListenerId)
	if subscribed() {
		t.Fatal("the mirror kept the subscription after the last listener went")
	}
	// removing one that was never added is what a torn down session replays
	listenerCall("DeviceLocalRpc.RemoveExtenderStatusChangeListener", firstListenerId)

	// and the getter answers over the same session, which is what a panel
	// opened before the first push reads
	var status *DeviceRemoteExtenderStatus
	if err := testingPreferenceRpcCall(
		t,
		client,
		"DeviceLocalRpc.GetExtenderStatus",
		RpcNoArg(0),
		&status,
	); err != nil {
		t.Fatal(err)
	}
	if status == nil || status.ExtenderStatus == nil {
		t.Fatal("the rpc getter answered no status")
	}
	connect.AssertEqual(t, status.ExtenderStatus.KnownCount, 1)
	connect.AssertEqual(t, status.ExtenderStatus.Extenders[0].Ip, "192.0.2.1")
}

// The attesting provider and the reporter a space keeps for its network
// client (GEOMAP §2.5), read under the lock the space writes them under.
func testNetworkSpaceProbeAttestor(
	networkSpace *NetworkSpace,
) (*connect.ExtenderProbeAttestor, *connect.ExtenderPingReporter) {
	networkSpace.stateLock.Lock()
	defer networkSpace.stateLock.Unlock()
	return networkSpace.extenderProbeAttestor, networkSpace.extenderProbeReporter
}

// Fails unless the network client carries exactly this attestor and reporter,
// nil for none. The client reports the pair it uses through ProbeAttestor, so
// the comparison is by identity against what the probe pass will sign with.
func testAssertExtenderNetworkClientProbeAttestor(
	t *testing.T,
	networkClient *connect.ExtenderNetworkClient,
	attestor *connect.ExtenderProbeAttestor,
	reporter *connect.ExtenderPingReporter,
) {
	t.Helper()
	installedAttestor, installedReporter := networkClient.ProbeAttestor()
	if installedAttestor != attestor {
		t.Fatalf("the network client carries attestor %p, expected %p", installedAttestor, attestor)
	}
	if installedReporter != reporter {
		t.Fatalf("the network client carries reporter %p, expected %p", installedReporter, reporter)
	}
}

// A providing device attests the probes of the space's network client and
// reports them through a reporter of its own (connect/DESIGNNOTES4.md §1,
// GEOMAP §2.5), from the moment its provide mode leaves none. The attestor
// names the provider and signs with its client key; the reporter posts to the
// space's api url, through the space's strategy, under whatever jwt the
// provider holds when it posts. A settings change that replaces the client in
// place hands the replacement the same pair (K6), and closing the device
// takes both off it.
func TestDeviceLocalProviderInstallsItsProbeAttestor(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	_, networkSpace := testExtenderStatusSpace(t)
	networkClient := networkSpace.getExtenderNetworkClient()
	if networkClient == nil {
		t.Fatal("the space ran no extender network client")
	}

	var reporterSettings *connect.ExtenderPingReporterSettings
	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = true
	settings.providerPingReporterSettings = func(settings *connect.ExtenderPingReporterSettings) {
		reporterSettings = settings
	}
	clientId := connect.NewId()
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deviceLocal.CloseAndWait(context.Background())
	})
	// built with a provider but providing in no mode: it attests nothing
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("a device providing in no mode installed an attestor")
	}
	deviceLocal.SetProvideMode(ProvideModePublic)

	attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace)
	if attestor == nil || reporter == nil {
		t.Fatalf("attestor = %v, reporter = %v, expected the provider's pair", attestor, reporter)
	}
	if attestor.Kind() != connect.ExtenderPingerKindProvider || attestor.ClientId != clientId {
		t.Fatalf(
			"the attestor names %q %s, expected the provider %s",
			attestor.Kind(),
			attestor.ClientId,
			clientId,
		)
	}
	deviceLocal.stateLock.Lock()
	provider := deviceLocal.provider
	deviceLocal.stateLock.Unlock()
	// the claim is signed with the provider's client key, which is the key
	// the operator verifies it under (GEOMAP §2.4)
	signingBytes := []byte(connect.ExtenderProbeSignatureDomain + " claim")
	if !connect.VerifyClientKeySignature(
		provider.Client().ClientKeyManager().PublicKey(),
		signingBytes,
		attestor.Sign(signingBytes),
	) {
		t.Fatal("the attestor does not sign with the provider's client key")
	}
	// the pair is the provider's own, and it is what the client carries
	func() {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		if provider.probeAttestor != attestor || provider.pingReporter != reporter {
			t.Fatal("the space carries a pair the provider did not install")
		}
	}()
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, attestor, reporter)

	// the report goes where every other api call of the provider goes, under
	// the jwt the provider holds at the time of the post
	if reporterSettings == nil {
		t.Fatal("the provider built its reporter without its settings")
	}
	connect.AssertEqual(t, reporterSettings.ApiUrl, networkSpace.apiUrl)
	if reporterSettings.ClientStrategy != networkSpace.clientStrategy {
		t.Fatal("the reporter does not post through the space's strategy")
	}
	provider.SetByJwt("refreshed-jwt")
	if byJwt := reporterSettings.ByJwt(); byJwt != "refreshed-jwt" {
		t.Fatalf("the reporter posts under %q, expected the refreshed jwt", byJwt)
	}

	// a settings change replaces the client in place, and the replacement
	// attests exactly as the one it replaced did (K6)
	if !networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.9"}
	}) {
		t.Fatal("the settings change changed nothing")
	}
	replacement := networkSpace.getExtenderNetworkClient()
	if replacement == nil || replacement == networkClient {
		t.Fatal("the settings change did not replace the network client")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, replacement, attestor, reporter)

	// nothing attests in the provider's name once it is gone
	if err := deviceLocal.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("the space kept the provider's pair after the device closed")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, replacement, nil, nil)
}

// Only a provider identifies itself to an extender (THREAT-MODEL.md §4): a
// device with no provider installs no attestor, and neither does a hosted
// device, which never provides and whose space is shared across unrelated
// customers. Of two providing devices in one space the later attests, and the
// earlier one closing -- a device replaced at a re-login, say -- leaves the
// later one's in place rather than leaving the space ranking only.
func TestDeviceLocalProbeAttestorBelongsToOneProvider(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	_, networkSpace := testExtenderStatusSpace(t)
	networkClient := networkSpace.getExtenderNetworkClient()
	if networkClient == nil {
		t.Fatal("the space ran no extender network client")
	}
	newDevice := func(configure func(settings *DeviceLocalSettings)) *DeviceLocal {
		t.Helper()
		settings := testExtenderStatusDeviceSettings()
		configure(settings)
		deviceLocal, err := newDeviceLocalWithOverrides(
			networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = deviceLocal.CloseAndWait(context.Background())
		})
		return deviceLocal
	}
	assertNoAttestor := func(name string) {
		t.Helper()
		if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
			t.Fatalf("%s left an attestor installed", name)
		}
		testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)
	}

	newDevice(func(settings *DeviceLocalSettings) {
		settings.AllowProvider = false
	})
	assertNoAttestor("a device with no provider")
	newDevice(func(settings *DeviceLocalSettings) {
		settings.AllowProvider = true
		settings.HostedIncompatible = true
	})
	assertNoAttestor("a hosted device")

	earlier := newDevice(func(settings *DeviceLocalSettings) {
		settings.AllowProvider = true
	})
	earlier.SetProvideMode(ProvideModeNetwork)
	earlierAttestor, _ := testNetworkSpaceProbeAttestor(networkSpace)
	later := newDevice(func(settings *DeviceLocalSettings) {
		settings.AllowProvider = true
	})
	later.SetProvideMode(ProvideModeNetwork)
	laterAttestor, laterReporter := testNetworkSpaceProbeAttestor(networkSpace)
	if earlierAttestor == nil || laterAttestor == nil || earlierAttestor == laterAttestor {
		t.Fatal("each provider did not install an attestor of its own")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, laterAttestor, laterReporter)

	if err := earlier.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != laterAttestor ||
		reporter != laterReporter {
		t.Fatal("the earlier provider's close took the later provider's attestor")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, laterAttestor, laterReporter)
	if err := later.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoAttestor("the last provider to close")
}

// The probes a space's network client made, in the order made: the address
// probed, and the attestor handed to the probe -- nil for a probe that ranks
// only and sends no claim.
type testProbeAttestorRecorder struct {
	stateLock sync.Mutex
	ips       []string
	attestors []*connect.ExtenderProbeAttestor
	// notified on every probe recorded
	probeMonitor *connect.Monitor
}

// Records one probe.
func (self *testProbeAttestorRecorder) record(ip string, attestor *connect.ExtenderProbeAttestor) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ips = append(self.ips, ip)
	self.attestors = append(self.attestors, attestor)
	self.probeMonitor.NotifyAll()
}

// The attestors of the probes of the ips made at or after probe `from`, and
// the number of probes made so far.
func (self *testProbeAttestorRecorder) attestorsOf(from int, ips ...string) ([]*connect.ExtenderProbeAttestor, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	attestors := []*connect.ExtenderProbeAttestor{}
	for i := from; i < len(self.ips); i += 1 {
		if slices.Contains(ips, self.ips[i]) {
			attestors = append(attestors, self.attestors[i])
		}
	}
	return attestors, len(self.ips)
}

// Waits until every one of the ips has a probe made at or after probe `from`
// that the predicate accepts, and returns the attestors of every probe of the
// ips made since then.
func (self *testProbeAttestorRecorder) waitForProbes(
	t *testing.T,
	from int,
	accept func(attestor *connect.ExtenderProbeAttestor) bool,
	ips ...string,
) []*connect.ExtenderProbeAttestor {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		probe := self.probeMonitor.NotifyChannel()
		probedIps := map[string]bool{}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for i := from; i < len(self.ips); i += 1 {
				if accept(self.attestors[i]) {
					probedIps[self.ips[i]] = true
				}
			}
		}()
		if !slices.ContainsFunc(ips, func(ip string) bool { return !probedIps[ip] }) {
			attestors, _ := self.attestorsOf(from, ips...)
			return attestors
		}
		select {
		case <-probe:
		case <-deadline:
			t.Fatalf("the network client never probed %v as expected, probed %v", ips, probedIps)
		}
	}
}

// Waits until every one of the ips has a latency sample in the directory,
// which the probe of it records: a probe pass has measured it.
func testWaitForProbeAttestorSamples(t *testing.T, networkSpace *NetworkSpace, ips ...string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		_, change := networkSpace.extenderDirectory.ChangeMonitor().Get()
		measuredIps := map[string]bool{}
		for _, candidate := range networkSpace.extenderDirectory.Candidates(4, 64) {
			if 0 < candidate.Latency {
				measuredIps[candidate.Ip.String()] = true
			}
		}
		if !slices.ContainsFunc(ips, func(ip string) bool { return !measuredIps[ip] }) {
			return
		}
		select {
		case <-change:
		case <-deadline:
			t.Fatalf("the network client never measured %v, measured %v", ips, measuredIps)
		}
	}
}

// A space on a synthetic host whose network client probes every v4 candidate
// it has no sample of through a recording probe, and the root key its records
// verify under.
func testProbeAttestorSpace(t *testing.T) (*NetworkSpace, ed25519.PrivateKey, *testProbeAttestorRecorder) {
	t.Helper()
	recorder := &testProbeAttestorRecorder{
		probeMonitor: connect.NewMonitor(),
	}
	extenderNetworkClientEnabled = true
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		testConfigureInProcessExtenderNetworkClient(settings)
		settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
		settings.ProbeWindowCount = 16
		settings.ProbeLatency = func(
			ctx context.Context,
			extenderConfig *connect.ExtenderConfig,
			attestor *connect.ExtenderProbeAttestor,
		) (*connect.ExtenderLatencyProbe, error) {
			recorder.record(extenderConfig.Ip.String(), attestor)
			return &connect.ExtenderLatencyProbe{
				Rtt:      20 * time.Millisecond,
				Response: &protocol.ExtenderResponse{PublicKey: slices.Clone(extenderConfig.PublicKey)},
			}, nil
		}
	}
	t.Cleanup(func() {
		extenderNetworkClientEnabled = false
		extenderNetworkClientConfigure = nil
	})

	_, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	storagePath, err := os.MkdirTemp("", "test_probe_attestor")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {
			values.ExtenderRootPublicKeys = []string{hex.EncodeToString(rootPrivateKey.Public().(ed25519.PublicKey))}
		},
	)
	if networkSpace.getExtenderNetworkClient() == nil {
		t.Fatal("the space ran no extender network client")
	}
	return networkSpace, rootPrivateKey, recorder
}

// Applies a signed record of a fresh extender at the documentation address.
func testApplyProbeAttestorRecord(t *testing.T, networkSpace *NetworkSpace, rootPrivateKey ed25519.PrivateKey, ip string) {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey: publicKey,
		Addresses: []*protocol.ExtenderAddress{
			{
				Ip:        ip,
				IpVersion: 4,
				Carriers:  []string{connect.ExtenderCarrierTcp},
			},
		},
		TcpPort:      443,
		IssueTimeMs:  uint64(now.UnixMilli()),
		ExpireTimeMs: uint64(now.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:  "space.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := networkSpace.extenderDirectory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
}

// Replaces the space's network client with a settings change (K6), whose
// first pass runs as it starts, and returns the replacement.
func testReplaceProbeAttestorClient(t *testing.T, networkSpace *NetworkSpace, manualHost string) *connect.ExtenderNetworkClient {
	t.Helper()
	previous := networkSpace.getExtenderNetworkClient()
	if !networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{manualHost}
	}) {
		t.Fatal("the settings change changed nothing")
	}
	replacement := networkSpace.getExtenderNetworkClient()
	if replacement == nil || replacement == previous {
		t.Fatal("the settings change did not replace the network client")
	}
	return replacement
}

// Only a providing device attests (connect/DESIGNNOTES4.md §1, GEOMAP §2.5): a
// device built with a provider but providing in no mode -- what every app
// build is -- probes to rank only, sending no claim; its mode leaving none
// installs the attestor and the next probe attests; its mode returning to none
// clears it and probes rank only again; and closing the device clears it.
func TestDeviceLocalProbeAttestorFollowsTheProvideMode(t *testing.T) {
	networkSpace, rootPrivateKey, recorder := testProbeAttestorSpace(t)
	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = true
	clientId := connect.NewId()
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deviceLocal.CloseAndWait(context.Background())
	})
	isNil := func(attestor *connect.ExtenderProbeAttestor) bool {
		return attestor == nil
	}

	// providing in no mode: a pass over fresh candidates ranks only
	testApplyProbeAttestorRecord(t, networkSpace, rootPrivateKey, "192.0.2.11")
	networkClient := testReplaceProbeAttestorClient(t, networkSpace, "192.0.2.91")
	testWaitForProbeAttestorSamples(t, networkSpace, "192.0.2.11", "192.0.2.91")
	attestors, probeCount := recorder.attestorsOf(0, "192.0.2.11", "192.0.2.91")
	if len(attestors) == 0 || slices.ContainsFunc(attestors, func(attestor *connect.ExtenderProbeAttestor) bool {
		return attestor != nil
	}) {
		t.Fatalf("a device providing in no mode made %d probes, with an attestor among them: %v", len(attestors), attestors)
	}
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("a device providing in no mode installed an attestor")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)

	// providing: the attestor is installed, and the pass it wakes attests
	// every candidate, which has no attested sample yet
	deviceLocal.SetProvideMode(ProvideModeNetwork)
	attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace)
	if attestor == nil || reporter == nil || attestor.ClientId != clientId {
		t.Fatalf("attestor = %v, reporter = %v, expected the provider's pair", attestor, reporter)
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, attestor, reporter)
	attestors = recorder.waitForProbes(t, probeCount, func(probeAttestor *connect.ExtenderProbeAttestor) bool {
		return probeAttestor == attestor
	}, "192.0.2.11", "192.0.2.91")
	if slices.ContainsFunc(attestors, func(probeAttestor *connect.ExtenderProbeAttestor) bool {
		return probeAttestor != attestor
	}) {
		t.Fatalf("a providing device made a probe without its attestor: %v", attestors)
	}

	// no longer providing: the attestor comes off, and a pass over fresh
	// candidates ranks only again
	deviceLocal.SetProvideMode(ProvideModeNone)
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("the attestor stayed installed after the device stopped providing")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)
	testApplyProbeAttestorRecord(t, networkSpace, rootPrivateKey, "192.0.2.12")
	networkClient = testReplaceProbeAttestorClient(t, networkSpace, "192.0.2.92")
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)
	attestors = recorder.waitForProbes(t, 0, isNil, "192.0.2.12", "192.0.2.92")
	if slices.ContainsFunc(attestors, func(attestor *connect.ExtenderProbeAttestor) bool {
		return attestor != nil
	}) {
		t.Fatalf("a device that stopped providing made a probe with an attestor: %v", attestors)
	}

	// providing again, then closed: nothing attests in its name once it is gone
	deviceLocal.SetProvideMode(ProvideModePublic)
	if attestor, _ := testNetworkSpaceProbeAttestor(networkSpace); attestor == nil {
		t.Fatal("providing again installed no attestor")
	}
	if err := deviceLocal.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("the space kept the provider's pair after the device closed")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)
}

// A hosted device never attests, even with its provider handed a providing
// mode directly: its device refuses the mode, and the provider was never let
// attest.
func TestDeviceLocalHostedProviderNeverAttests(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	_, networkSpace := testExtenderStatusSpace(t)
	networkClient := networkSpace.getExtenderNetworkClient()
	if networkClient == nil {
		t.Fatal("the space ran no extender network client")
	}
	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = true
	settings.HostedIncompatible = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deviceLocal.CloseAndWait(context.Background())
	})

	deviceLocal.SetProvideMode(ProvideModePublic)
	deviceLocal.stateLock.Lock()
	provider := deviceLocal.provider
	deviceLocal.stateLock.Unlock()
	if provider == nil {
		t.Fatal("the hosted device built no provider")
	}
	provider.setProvideMode(ProvideModePublic)
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("a hosted device installed an attestor")
	}
	testAssertExtenderNetworkClientProbeAttestor(t, networkClient, nil, nil)
}

// The provider is handed the provide mode as it is when the hand-over runs,
// not the mode of the change that prompted it, so a hand-over that lands after
// a later change cannot leave the attestor installed for a device that has
// stopped providing.
func TestDeviceLocalProvideModeHandOverIsNeverStale(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	_, networkSpace := testExtenderStatusSpace(t)
	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = deviceLocal.CloseAndWait(context.Background())
	})
	deviceLocal.SetProvideMode(ProvideModeNetwork)
	if attestor, _ := testNetworkSpaceProbeAttestor(networkSpace); attestor == nil {
		t.Fatal("providing installed no attestor")
	}

	// a later change to none has landed on the device, and the hand-over of
	// the earlier change to network runs only after it: it carries none
	func() {
		deviceLocal.stateLock.Lock()
		defer deviceLocal.stateLock.Unlock()
		deviceLocal.provideMode = ProvideModeNone
	}()
	deviceLocal.updateProviderProvideMode()
	deviceLocal.stateLock.Lock()
	provider := deviceLocal.provider
	deviceLocal.stateLock.Unlock()
	provider.stateLock.Lock()
	provideMode := provider.provideMode
	provider.stateLock.Unlock()
	if provideMode != ProvideModeNone {
		t.Fatalf("the provider was handed mode %d, expected the device's none", provideMode)
	}
	if attestor, reporter := testNetworkSpaceProbeAttestor(networkSpace); attestor != nil || reporter != nil {
		t.Fatal("a stale hand-over left the attestor installed")
	}
}
