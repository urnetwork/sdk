package sdk

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
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
