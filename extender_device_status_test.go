package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"net/netip"
	"net/rpc"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The extender status on the device (EXTENDER.md K4, K5): the panel counts,
// the gossip state of each role, the event rate, and the same readout over a
// local device, over the rpc, and on a hosted device.

// testExtenderStatusSpace builds a space with a real extender network host, so
// it keeps a directory the status can describe. The suite's TestMain leaves the
// network client and the node off, so nothing resolves or dials.
func testExtenderStatusSpace(t *testing.T) (*NetworkSpaceManager, *NetworkSpace) {
	t.Helper()
	storagePath, err := os.MkdirTemp("", "test_extender_device_status")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {},
	)
	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space has no extender directory")
	}
	return networkSpaceManager, networkSpace
}

// The quiet device settings these tests build on: no rpc server, no logging.
func testExtenderStatusDeviceSettings() *DeviceLocalSettings {
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	return settings
}

// A local device and a remote over the real rpc transport, both against one
// caller-supplied space, so the status the remote reads through has a
// directory to describe. Mirrors testing_newSyncedDeviceLocalRemote, which
// builds its own storage-backed `test` space and therefore has none.
func testExtenderStatusSyncedDeviceLocalRemote(
	t *testing.T,
	networkSpace *NetworkSpace,
) (*DeviceLocal, *DeviceRemote) {
	t.Helper()
	return testExtenderStatusSyncedDeviceLocalRemoteWithSettings(t, networkSpace, nil)
}

// The same pair with the local device's settings adjusted first, which is how
// a test runs a provider behind the rpc.
func testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
	t *testing.T,
	networkSpace *NetworkSpace,
	configureLocal func(settings *DeviceLocalSettings),
) (*DeviceLocal, *DeviceRemote) {
	t.Helper()

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	localSettings := testExtenderStatusDeviceSettings()
	localSettings.EnableRpc = true
	if configureLocal != nil {
		configureLocal(localSettings)
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", instanceId, localSettings, clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, "", instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		deviceLocal.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deviceRemote.Close()
		deviceLocal.Close()
	})
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(30 * time.Second) {
		t.Fatal("device remote did not sync")
	}
	return deviceLocal, deviceRemote
}

// Seeds a root key on the space's directory and returns a signed record for
// one address of that space's network, so a test can apply a network event.
func testExtenderStatusRecord(
	t *testing.T,
	networkSpace *NetworkSpace,
	ip string,
) *protocol.ExtenderRecord {
	t.Helper()
	rootSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(rootSeed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	keySet, err := connect.NewExtenderRootKeySetFromHex(hex.EncodeToString(rootPublicKey))
	if err != nil {
		t.Fatal(err)
	}
	networkSpace.extenderDirectory.SetRootKeys(keySet)

	extenderSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	extenderPublicKey, err := connect.ExtenderPublicKeyFromSeed(extenderSeed)
	if err != nil {
		t.Fatal(err)
	}
	record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey: extenderPublicKey,
		Addresses: []*protocol.ExtenderAddress{
			{Ip: ip, IpVersion: 4, Carriers: []string{connect.ExtenderCarrierTcp}},
		},
		TcpPort:      443,
		IssueTimeMs:  uint64(time.Now().UnixMilli()),
		ExpireTimeMs: uint64(time.Now().Add(14 * 24 * time.Hour).UnixMilli()),
		NetworkHost:  "space.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// The status dot reads the evidence of the role it is in, and only that (K4,
// K5). A member reading the feed fields would draw red over a healthy mesh,
// which is the failure this pins.
func TestExtenderGossipStateByRole(t *testing.T) {
	connected := &connect.ExtenderNetworkClientStatus{FeedConnected: true}
	connecting := &connect.ExtenderNetworkClientStatus{Connecting: true}
	// a stream that is up wins over a dial still in flight
	both := &connect.ExtenderNetworkClientStatus{FeedConnected: true, Connecting: true}
	down := &connect.ExtenderNetworkClientStatus{}

	cases := []struct {
		name          string
		role          string
		networkStatus *connect.ExtenderNetworkClientStatus
		meshPeerCount int
		connecting    bool
		expect        string
	}{
		{name: "feed stream up", role: ExtenderRoleFeed, networkStatus: connected, expect: connect.ExtenderGossipStateConnected},
		{name: "feed dialing", role: ExtenderRoleFeed, networkStatus: connecting, expect: connect.ExtenderGossipStateConnecting},
		{name: "feed up while redialing", role: ExtenderRoleFeed, networkStatus: both, expect: connect.ExtenderGossipStateConnected},
		{name: "feed backoff", role: ExtenderRoleFeed, networkStatus: down, expect: connect.ExtenderGossipStateDisconnected},
		{name: "feed no client", role: ExtenderRoleFeed, expect: connect.ExtenderGossipStateDisconnected},
		// the member reads the mesh: a feed status that says connected must
		// not make a member with no peers look green
		{name: "member mesh", role: ExtenderRoleMember, networkStatus: connected, meshPeerCount: 1, expect: connect.ExtenderGossipStateConnected},
		{name: "member peering", role: ExtenderRoleMember, networkStatus: connected, connecting: true, expect: connect.ExtenderGossipStateConnecting},
		{name: "member alone", role: ExtenderRoleMember, networkStatus: connected, expect: connect.ExtenderGossipStateDisconnected},
		{name: "member mesh while peering", role: ExtenderRoleMember, meshPeerCount: 2, connecting: true, expect: connect.ExtenderGossipStateConnected},
	}
	for _, c := range cases {
		state := extenderGossipState(c.role, c.networkStatus, c.meshPeerCount, c.connecting)
		if state != c.expect {
			t.Errorf("%s: state = %q, expected %q", c.name, state, c.expect)
		}
	}
}

// The panel's counts and rate (K4): N is the addresses carrying a live
// connection, M every usable one, and the rate is the records and revocations
// the network delivered in the trailing minute. Every row carries its color.
func TestExtenderStatusCountsAndEventRate(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	directory := networkSpace.extenderDirectory

	// three usable addresses, one of them carrying a connection
	for _, ip := range []string{"192.0.2.1", "198.51.100.7", "203.0.113.42"} {
		directory.AddBootstrap(netip.MustParseAddr(ip), connect.ExtenderSourceDns)
	}
	directory.SetInUse(netip.MustParseAddr("192.0.2.1"), 1)

	status := networkSpace.GetExtenderStatus()
	connect.AssertEqual(t, status.KnownCount, 3)
	connect.AssertEqual(t, status.ActiveCount, 1)
	connect.AssertEqual(t, status.ReserveCount, 3)
	connect.AssertEqual(t, status.HoldCount, 0)
	// a bootstrap address is not a network event
	connect.AssertEqual(t, status.EventCountLastMinute, 0)
	// the suite runs no client and no node, so the dot is red
	connect.AssertEqual(t, status.GossipState, connect.ExtenderGossipStateDisconnected)

	for i := range status.Extenders.Len() {
		extenderInfo := status.Extenders.Get(i)
		if extenderInfo.ColorHex != GetExtenderColorHex(extenderInfo.Ip) {
			t.Errorf("%s color = %q", extenderInfo.Ip, extenderInfo.ColorHex)
		}
	}
	if colorHex := status.Extenders.Get(0).ColorHex; colorHex != testExtenderColorHexes["192.0.2.1"] {
		t.Errorf("first row color = %q, expected the pinned color", colorHex)
	}

	// a held address leaves the reserve but stays known
	directory.RecordFailure(netip.MustParseAddr("203.0.113.42"), connect.ExtenderConnectModeTcpTls)
	status = networkSpace.GetExtenderStatus()
	connect.AssertEqual(t, status.KnownCount, 3)
	connect.AssertEqual(t, status.HoldCount, 1)
	connect.AssertEqual(t, status.ReserveCount, 2)
	connect.AssertEqual(t, status.ActiveCount, 1)

	// a record applied from the feed is a network event; one loaded from the
	// store or added by hand is not
	record := testExtenderStatusRecord(t, networkSpace, "192.0.2.9")
	if changed, err := directory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil || !changed {
		t.Fatalf("apply record changed = %v, err = %v", changed, err)
	}
	connect.AssertEqual(t, networkSpace.GetExtenderStatus().EventCountLastMinute, 1)

	revocationRecord := testExtenderStatusRecord(t, networkSpace, "192.0.2.10")
	if changed, err := directory.ApplyRecord(revocationRecord, connect.ExtenderSourceGossip); err != nil || !changed {
		t.Fatalf("apply mesh record changed = %v, err = %v", changed, err)
	}
	connect.AssertEqual(t, networkSpace.GetExtenderStatus().EventCountLastMinute, 2)

	// a manual address is configuration, not a network event
	directory.AddManual(netip.MustParseAddr("198.51.100.99"))
	connect.AssertEqual(t, networkSpace.GetExtenderStatus().EventCountLastMinute, 2)
}

// A local device reads its own space, and a hosted one reports nothing: its
// space is shared across unrelated customers (K5, G1).
func TestDeviceLocalExtenderStatus(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)
	networkSpace.extenderDirectory.SetInUse(netip.MustParseAddr("192.0.2.1"), 1)

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	status := deviceLocal.GetExtenderStatus()
	connect.AssertEqual(t, status.KnownCount, 1)
	connect.AssertEqual(t, status.ActiveCount, 1)
	connect.AssertEqual(t, status.Extenders.Get(0).ColorHex, testExtenderColorHexes["192.0.2.1"])

	// the listener is the space's, so a directory change reaches it
	statuses := make(chan *ExtenderStatus, 4)
	sub := deviceLocal.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	defer sub.Close()
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.7"),
		connect.ExtenderSourceDns,
	)
	select {
	case changed := <-statuses:
		connect.AssertEqual(t, changed.KnownCount, 2)
	case <-time.After(30 * time.Second):
		t.Fatal("the device extender status listener was never called")
	}
}

// A hosted device describes no extender network at all (K5).
func TestDeviceLocalHostedExtenderStatusIsEmpty(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)

	settings := testExtenderStatusDeviceSettings()
	settings.HostedIncompatible = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	status := deviceLocal.GetExtenderStatus()
	if status == nil || status.Extenders == nil {
		t.Fatal("a hosted device reported no status at all")
	}
	connect.AssertEqual(t, status.KnownCount, 0)
	connect.AssertEqual(t, status.Extenders.Len(), 0)
	connect.AssertEqual(t, status.GossipState, connect.ExtenderGossipStateDisconnected)
	// and the listener is inert rather than nil
	sub := deviceLocal.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			t.Error("a hosted device published an extender status")
		}),
	)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.7"),
		connect.ExtenderSourceDns,
	)
	sub.Close()
}

// The same readout over the rpc: the remote reads through to the device
// process, and the device's coalesced callback is mirrored to it (K5).
func TestDeviceRemoteExtenderStatus(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("192.0.2.1"),
		connect.ExtenderSourceDns,
	)
	networkSpace.extenderDirectory.SetInUse(netip.MustParseAddr("192.0.2.1"), 1)

	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	status := deviceRemote.GetExtenderStatus()
	connect.AssertEqual(t, status.KnownCount, 1)
	connect.AssertEqual(t, status.ActiveCount, 1)
	connect.AssertEqual(t, status.ReserveCount, 1)
	connect.AssertEqual(t, status.Extenders.Len(), 1)
	connect.AssertEqual(t, status.Extenders.Get(0).Ip, "192.0.2.1")
	connect.AssertEqual(t, status.Extenders.Get(0).ColorHex, testExtenderColorHexes["192.0.2.1"])
	// the local device answers the same thing, which is the point of the
	// read-through
	connect.AssertEqual(t, deviceLocal.GetExtenderStatus().KnownCount, 1)

	statuses := make(chan *ExtenderStatus, 8)
	sub := deviceRemote.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	defer sub.Close()

	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.7"),
		connect.ExtenderSourceDns,
	)
	deadline := time.After(60 * time.Second)
	for {
		select {
		case changed := <-statuses:
			if changed.KnownCount == 2 {
				return
			}
		case <-deadline:
			t.Fatal("the extender status never crossed the rpc to the remote")
		}
	}
}

// With the device process down the remote answers the last readout it saw, and
// the empty status when it has never seen one (K5).
func TestDeviceRemoteExtenderStatusCachesTheLastValue(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)

	// never observed: the empty status, never nil
	status := deviceRemote.GetExtenderStatus()
	if status == nil || status.Extenders == nil {
		t.Fatal("a remote with no service reported no status")
	}
	connect.AssertEqual(t, status.KnownCount, 0)
	connect.AssertEqual(t, status.GossipState, connect.ExtenderGossipStateDisconnected)

	// the device process publishes one before it goes away
	published := &ExtenderStatus{
		Role:                 ExtenderRoleMember,
		GossipState:          connect.ExtenderGossipStateConnected,
		GossipConnected:      true,
		GossipPeerCount:      3,
		EventCountLastMinute: 5,
		KnownCount:           2,
		ActiveCount:          1,
		ReserveCount:         2,
		Extenders:            NewExtenderInfoList(),
	}
	published.Extenders.Add(&ExtenderInfo{
		Ip:       "192.0.2.1",
		ColorHex: GetExtenderColorHex("192.0.2.1"),
		State:    connect.ExtenderStateActive,
		InUse:    1,
	})
	deviceRemote.extenderStatusChanged(newExtenderStatusRpc(published))

	// the service is still down, and the getter answers what was published
	cached := deviceRemote.GetExtenderStatus()
	connect.AssertEqual(t, cached.KnownCount, 2)
	connect.AssertEqual(t, cached.ActiveCount, 1)
	connect.AssertEqual(t, cached.GossipState, connect.ExtenderGossipStateConnected)
	connect.AssertEqual(t, cached.GossipPeerCount, 3)
	connect.AssertEqual(t, cached.EventCountLastMinute, 5)
	connect.AssertEqual(t, cached.Extenders.Len(), 1)
	connect.AssertEqual(t, cached.Extenders.Get(0).Ip, "192.0.2.1")
	// the cache hands back a copy, so a caller that mutates its status does
	// not rewrite what the next reader sees
	cached.Extenders.Get(0).Ip = "203.0.113.1"
	connect.AssertEqual(t, deviceRemote.GetExtenderStatus().Extenders.Get(0).Ip, "192.0.2.1")
}

// A new app can briefly talk to the previous extension process after an app
// update. The extender status is a read-only panel, so a device process that
// does not answer it yet leaves the session alive and the app reads the cached
// or empty status -- losing rpc control of the tunnel over a panel would be a
// much worse trade (K5).
func TestExtenderStatusMissingMethodKeepsTheRpcSessionAlive(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// a device process that has the rest of the surface but not this method
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", &testingOptionalMethodRpc{}); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)

	settings := defaultDeviceRpcSettings()
	service := &rpcClientWithTimeout{
		ctx:         context.Background(),
		log:         settings.logger(),
		timeout:     time.Second,
		closeClient: clientConn.Close,
		client:      rpc.NewClient(clientConn),
	}
	defer service.Close()

	cleanupCalled := false
	status, err := rpcCallNoArgAllowMissingMethod[*DeviceRemoteExtenderStatus](
		service,
		"DeviceLocalRpc.GetExtenderStatus",
		func() {
			cleanupCalled = true
			clientConn.Close()
		},
	)
	if err == nil || !rpcMissingMethodError(err) {
		t.Fatalf("missing method error = %v", err)
	}
	if cleanupCalled {
		t.Fatal("the missing extender status method closed the rpc session")
	}
	if status != nil {
		t.Fatalf("status = %+v, expected none", status)
	}

	var reply bool
	if err := service.Call("DeviceLocalRpc.Ping", true, &reply); err != nil || !reply {
		t.Fatalf("the rpc session did not survive: reply = %t err = %v", reply, err)
	}
}

// The rpc mirror carries every field of the status. A mirror that forgets one
// drops it silently, which is the defect device_rpc_mirror_test.go exists for.
func TestRpcMirrorExtenderStatusComplete(t *testing.T) {
	seed := 0
	status := &ExtenderStatus{}
	fillNonZero(t, reflect.ValueOf(status), &seed)
	// the bound list's backing slice is unexported, so fillNonZero cannot fill
	// it; the rows are filled here instead
	status.Extenders = NewExtenderInfoList()
	extenderInfo := &ExtenderInfo{}
	fillNonZero(t, reflect.ValueOf(extenderInfo), &seed)
	status.Extenders.Add(extenderInfo)

	wired := gobRoundTrip(t, &DeviceRemoteExtenderStatus{
		ExtenderStatus: newExtenderStatusRpc(status),
	})
	connect.AssertEqual(t, wired.ExtenderStatus.toExtenderStatus(), status)
}
