package sdk

import (
	"context"
	"net"
	"net/rpc"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The provider extender status and setting across the device rpc (EXTENDER.md
// N2, N6): the remote reports what the device process derived, the last value
// stands while the device is out of contact, a device process too old to
// answer reports the role unsupported, and the setting round trips.

// A device process that answers the provider extender surface, for the cases
// that need the remote pointed at something other than a real DeviceLocalRpc.
type testingExtenderProvideRpc struct {
	stateLock       sync.Mutex
	status          *ExtenderProvideStatus
	provideExtender bool
	setCount        int
}

func (self *testingExtenderProvideRpc) Ping(_ bool, reply *bool) error {
	*reply = true
	return nil
}

func (self *testingExtenderProvideRpc) GetExtenderProvideStatus(
	_ RpcNoArg,
	status **ExtenderProvideStatus,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	*status = cloneExtenderProvideStatus(self.status)
	return nil
}

func (self *testingExtenderProvideRpc) GetProvideExtender(
	_ RpcNoArg,
	provideExtender *bool,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	*provideExtender = self.provideExtender
	return nil
}

func (self *testingExtenderProvideRpc) SetProvideExtender(
	provideExtender bool,
	_ RpcVoid,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideExtender = provideExtender
	self.setCount += 1
	return nil
}

func (self *testingExtenderProvideRpc) sets() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.setCount
}

// Points a device remote at an in-process rpc server serving deviceProcess as
// DeviceLocalRpc, the way the ios app process reaches the tunnel extension.
// Mirrors TestDeviceRemoteControlIpFamilyStatusIsTheDeviceProcessAnswer.
func testExtenderProvideDeviceProcess(
	t *testing.T,
	deviceRemote *DeviceRemote,
	deviceProcess any,
) *rpcClientWithTimeout {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", deviceProcess); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)

	settings := defaultDeviceRpcSettings()
	service := &rpcClientWithTimeout{
		ctx:         context.Background(),
		log:         settings.logger(),
		timeout:     settings.RpcCallTimeout,
		closeClient: clientConn.Close,
		client:      rpc.NewClient(clientConn),
	}
	t.Cleanup(func() { service.Close() })

	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	deviceRemote.service = service
	deviceRemote.remoteConnected = true
	return service
}

// A remote built before its device process exists -- an app started while the
// daemon is down -- and the function that brings the device up on the
// remote's address and waits for the sync that follows.
func testExtenderProvideRemoteBeforeLocal(
	t *testing.T,
	networkSpace *NetworkSpace,
) (*DeviceRemote, func() *DeviceLocal) {
	t.Helper()

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, "", instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	if deviceRemote.GetRemoteConnected() {
		t.Fatal("the remote found a device process before one was started")
	}

	bringUp := func() *DeviceLocal {
		t.Helper()
		localSettings := testExtenderStatusDeviceSettings()
		localSettings.EnableRpc = true
		deviceLocal, err := newDeviceLocalWithOverrides(
			networkSpace, "", "", "", "", instanceId, localSettings, clientId,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(deviceLocal.Close)
		deviceRemote.Sync()
		if !deviceRemote.waitForSync(30 * time.Second) {
			t.Fatal("the device remote did not sync after the device came up")
		}
		return deviceLocal
	}
	return deviceRemote, bringUp
}

func testQueuedProvideExtender(deviceRemote *DeviceRemote) deviceRemoteValue[bool] {
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	return deviceRemote.state.ProvideExtender
}

// The status a remote reads is the one the device process derived, State and
// Reason included: the rule of N3 runs once, where the role is (N2).
func TestDeviceRemoteExtenderProvideStatus(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	local := deviceLocal.GetExtenderProvideStatus()
	// the device derived a state rather than leaving the field for the app
	if local.State == "" {
		t.Fatal("the local device reported no provider extender state")
	}
	connect.AssertEqual(t, local.Supported, extenderProvideSupported)
	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus(), local)

	// the setting is the half of N3 the user owns, and it crosses as the
	// derived state, not as the raw fields: off whatever else is true
	deviceLocal.SetProvideExtender(false)
	connect.AssertEqual(
		t,
		deviceLocal.GetExtenderProvideStatus().State,
		ExtenderProvideStateOff,
	)
	connect.AssertEqual(
		t,
		deviceRemote.GetExtenderProvideStatus().State,
		ExtenderProvideStateOff,
	)
	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus(), deviceLocal.GetExtenderProvideStatus())
}

// The setting reads and writes through the rpc, in both directions (N2, N4).
func TestDeviceRemoteProvideExtenderSetting(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	// default on, for desktop and the miner alike (N4)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)

	deviceRemote.SetProvideExtender(false)
	// the device process persisted it, which is what makes it survive a
	// restart of the app process
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), false)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)

	// and a change made on the device is what the remote reads back
	deviceLocal.SetProvideExtender(true)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)
}

// Turned off while the daemon is down, the setting reads back off at once and
// reaches the device at the next sync, as the provide mode does (N2).
func TestDeviceRemoteQueuesTheProvideExtenderSettingWhileUnreachable(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceRemote, bringUp := testExtenderProvideRemoteBeforeLocal(t, networkSpace)

	deviceRemote.SetProvideExtender(false)
	// the toggle does not snap back while the daemon is down
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)
	if queued := testQueuedProvideExtender(deviceRemote); !queued.IsSet || queued.Value {
		t.Fatalf("queued = %+v, expected off queued for replay", queued)
	}

	deviceLocal := bringUp()

	// the remote never writes the setting itself, so an off on the device can
	// only have come across the sync
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), false)
	if queued := testQueuedProvideExtender(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v after the sync, expected nothing left to replay", queued)
	}
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)
}

// With the device process gone the remote reads the last value it read or
// wrote, not the default, so a setting the user turned off stays off in the
// ui while the daemon restarts (N2).
func TestDeviceRemoteProvideExtenderLastKnownWhileUnreachable(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	deviceRemote.SetProvideExtender(false)
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), false)
	if queued := testQueuedProvideExtender(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v, expected a write the device took to leave nothing queued", queued)
	}

	deviceLocal.Close()

	// the default is on, so reading off here is the last known value
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)
}

// A change on the device reaches the remote's listener, through the local
// device's coalesced callback and the rpc listener registry (N2, N6).
func TestDeviceRemoteExtenderProvideStatusListener(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	statuses := make(chan *ExtenderProvideStatus, 8)
	sub := deviceRemote.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	defer sub.Close()

	deviceLocal.SetProvideExtender(false)

	deadline := time.After(60 * time.Second)
	for {
		select {
		case changed := <-statuses:
			if changed == nil {
				t.Fatal("the remote was handed no status")
			}
			if changed.State == ExtenderProvideStateOff {
				return
			}
		case <-deadline:
			t.Fatal("the provider extender status never crossed the rpc to the remote")
		}
	}
}

// A listener added while the daemon is down is carried by the next sync, and
// the device replays the current status to it, so a row opened before the
// daemon came up fills in without waiting for a change (N2). Closing its sub
// removes the device's subscription over the rpc.
func TestDeviceRemoteExtenderProvideStatusListenerReplaysAtSync(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceRemote, bringUp := testExtenderProvideRemoteBeforeLocal(t, networkSpace)

	statuses := make(chan *ExtenderProvideStatus, 8)
	sub := deviceRemote.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)

	deviceLocal := bringUp()

	select {
	case status := <-statuses:
		// nothing changes on the device, so the level it replayed is what it
		// reports now
		connect.AssertEqual(t, status, deviceLocal.GetExtenderProvideStatus())
	case <-time.After(60 * time.Second):
		t.Fatal("the device never replayed the status to a listener added before it came up")
	}
	// the listener id crossed in the sync: the device holds one subscription
	// for the rpc
	connect.AssertEqual(t, len(deviceLocal.extenderProvideStatusChangeListeners.Get()), 1)

	sub.Close()
	connect.AssertEqual(t, len(deviceLocal.extenderProvideStatusChangeListeners.Get()), 0)
}

// The last status the remote read, not only the last one pushed, stands while
// the device process is gone (N2, K5).
func TestDeviceRemoteExtenderProvideStatusReadCacheSurvivesServiceLoss(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	// no listener is registered, so nothing is pushed: the read is the only
	// writer of the cache here
	read := deviceRemote.GetExtenderProvideStatus()
	if !read.Supported || read.State == "" {
		t.Fatalf("status = %+v, expected the device's derived status", read)
	}

	deviceLocal.Close()

	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus(), read)
}

// A pushed status carries its error case and reason intact over the real
// transport (N2, N3). The device provides with the role on and an identity it
// cannot activate under, so it reports the start error without running a role.
func TestDeviceRemoteExtenderProvideStatusPushCarriesTheErrorCase(t *testing.T) {
	testEnableExtenderProvideRole(t)
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		func(settings *DeviceLocalSettings) {
			settings.AllowProvider = true
			settings.providerExtenderSettings = func(extenderSettings *deviceLocalExtenderSettings) {
				extenderSettings.IdentityKeySeed = []byte("not an extender key seed")
			}
		},
	)

	statuses := make(chan *ExtenderProvideStatus, 16)
	sub := deviceRemote.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	defer sub.Close()

	// providing is what asks for the role
	deviceLocal.SetProvideMode(ProvideModePublic)

	deadline := time.After(60 * time.Second)
	for {
		select {
		case status := <-statuses:
			if status.ErrorCase != ExtenderProvideErrorStart {
				continue
			}
			local := deviceLocal.GetExtenderProvideStatus()
			if status.State != ExtenderProvideStateError ||
				local.StartError == "" ||
				status.StartError != local.StartError ||
				status.Reason != local.StartError {
				t.Fatalf("pushed = %+v, local = %+v, expected the start error intact", status, local)
			}
			return
		case <-deadline:
			t.Fatalf(
				"the start error never crossed the rpc, local = %+v",
				deviceLocal.GetExtenderProvideStatus(),
			)
		}
	}
}

// With the device process down the remote answers the last readout it saw, and
// the unsupported status when it has never seen one (N2).
func TestDeviceRemoteExtenderProvideStatusCachesTheLastValue(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)

	// never observed: unsupported, never nil, so the row is hidden rather
	// than drawn dead (N1)
	status := deviceRemote.GetExtenderProvideStatus()
	if status == nil {
		t.Fatal("a remote with no service reported no status")
	}
	connect.AssertEqual(t, status.Supported, false)
	connect.AssertEqual(t, status.State, ExtenderProvideStateOff)
	// the setting reads the local default, which is on (N4)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)

	// the device process publishes one before it goes away
	published := &ExtenderProvideStatus{
		Supported:           true,
		State:               ExtenderProvideStateActive,
		Reason:              "the operator refused the activation",
		Enabled:             true,
		Listening:           true,
		ActivatedV4:         true,
		Ipv4:                "192.0.2.10",
		LastActivationTime:  1757000000000,
		LastActivationError: "the operator refused the activation",
		DnsPorts:            "53,4053",
		ConnectionCount:     3,
	}
	deviceRemote.extenderProvideStatusChanged(published)

	// the service is still down, and the getter answers what was published
	cached := deviceRemote.GetExtenderProvideStatus()
	connect.AssertEqual(t, cached, published)
	// the cache hands back a copy, so a caller that mutates its status does
	// not rewrite what the next reader sees
	cached.State = ExtenderProvideStateOff
	cached.Ipv4 = "203.0.113.1"
	connect.AssertEqual(
		t,
		deviceRemote.GetExtenderProvideStatus().State,
		ExtenderProvideStateActive,
	)
	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus().Ipv4, "192.0.2.10")
}

// An app updated ahead of the device process it is attached to reads the
// unsupported status and the default setting, and keeps its rpc session: the
// row is hidden, and losing rpc control of the tunnel over a settings row
// would be a much worse trade (N1, N2).
func TestDeviceRemoteExtenderProvideWithoutTheMethods(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	// a device process that has the rest of the surface but none of these
	// methods
	service := testExtenderProvideDeviceProcess(t, deviceRemote, &testingOptionalMethodRpc{})

	status := deviceRemote.GetExtenderProvideStatus()
	connect.AssertEqual(t, status.Supported, false)
	connect.AssertEqual(t, status.State, ExtenderProvideStateOff)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)
	// the setter is a write to a method that is not there, and is dropped
	// rather than queued, or it would replay on every reconnect
	deviceRemote.SetProvideExtender(false)
	if queued := testQueuedProvideExtender(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v, expected a write the device process cannot take to be dropped", queued)
	}
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)
	// the listener add goes the same way
	sub := deviceRemote.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {}),
	)
	defer sub.Close()

	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the missing provider extender methods tore the rpc session down")
	}
	var reply bool
	if err := service.Call("DeviceLocalRpc.Ping", true, &reply); err != nil || !reply {
		t.Fatalf("the rpc session did not survive: reply = %t err = %v", reply, err)
	}
}

// A daemon downgraded under a running app answers the missing method, and the
// remote reports the role unsupported even though it holds a status from the
// newer process, so the row is hidden rather than left with a toggle that
// reaches nothing (N1, N2).
func TestDeviceRemoteExtenderProvideStatusMissingMethodDropsTheCache(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	deviceRemote.extenderProvideStatusChanged(&ExtenderProvideStatus{
		Supported:   true,
		State:       ExtenderProvideStateActive,
		Enabled:     true,
		Listening:   true,
		ActivatedV4: true,
		Ipv4:        "192.0.2.10",
	})
	// with no service the cached status stands
	connect.AssertEqual(
		t,
		deviceRemote.GetExtenderProvideStatus().State,
		ExtenderProvideStateActive,
	)

	testExtenderProvideDeviceProcess(t, deviceRemote, &testingOptionalMethodRpc{})

	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus(), unsupportedExtenderProvideStatus())
	deviceRemote.stateLock.Lock()
	cached := deviceRemote.lastExtenderProvideStatus
	deviceRemote.stateLock.Unlock()
	if cached != nil {
		t.Fatalf("cached = %+v, expected the newer process's status to be dropped", cached)
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the missing method tore the rpc session down")
	}
}

// The status crosses whole, with the state the device derived (N2). A device
// process that reports a state the app cannot compute for itself -- an
// activated family, a bind failure -- must arrive with it intact.
func TestDeviceRemoteExtenderProvideStatusReadsThroughUnchanged(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	deviceProcess := &testingExtenderProvideRpc{
		provideExtender: true,
		status: &ExtenderProvideStatus{
			Supported:          true,
			State:              ExtenderProvideStateError,
			ErrorCase:          ExtenderProvideErrorListen,
			Reason:             "tcp: bind refused; quic: bind refused; dns: bind refused",
			Enabled:            true,
			ListenError:        "tcp: bind refused; quic: bind refused; dns: bind refused",
			LastActivationTime: 1757000000000,
			RevokedTime:        1757000001000,
			ConnectionCount:    7,
		},
	}
	testExtenderProvideDeviceProcess(t, deviceRemote, deviceProcess)

	connect.AssertEqual(t, deviceRemote.GetExtenderProvideStatus(), deviceProcess.status)

	// and the setting round trips to the device process
	deviceRemote.SetProvideExtender(false)
	connect.AssertEqual(t, deviceProcess.sets(), 1)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)
}

// A hosted device never runs the role (G1), so the setter never reaches it,
// on either side of the rpc.
func TestProvideExtenderHostedGuard(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	deviceProcess := &testingExtenderProvideRpc{provideExtender: true}
	testExtenderProvideDeviceProcess(t, deviceRemote, deviceProcess)
	deviceRemote.settings.DisableHostedIncompatible = true

	deviceRemote.SetProvideExtender(false)
	connect.AssertEqual(t, deviceProcess.sets(), 0)
	if queued := testQueuedProvideExtender(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v, expected the hosted guard to queue nothing", queued)
	}

	// the rpc layer blocks it independently, for a local object that was not
	// constructed with the DeviceLocal guard
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	localRpc := &DeviceLocalRpc{
		deviceLocal: deviceLocal,
		settings:    &deviceRpcSettings{DisableHostedIncompatible: true},
	}
	connect.AssertEqual(t, localRpc.SetProvideExtender(false, nil), nil)
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), true)

	// and the device itself refuses it when it is hosted, for a caller in the
	// same process that never crosses the rpc
	_, hostedSpace := testExtenderStatusSpace(t)
	hostedSettings := testExtenderStatusDeviceSettings()
	hostedSettings.HostedIncompatible = true
	hostedDevice, err := newDeviceLocalWithOverrides(
		hostedSpace, "", "", "", "", NewId(), hostedSettings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer hostedDevice.Close()
	hostedDevice.SetProvideExtender(false)
	connect.AssertEqual(t, hostedDevice.GetProvideExtender(), true)
	// a hosted device cannot provide, so what it reports is not providing
	connect.AssertEqual(
		t,
		hostedDevice.GetExtenderProvideStatus().State,
		ExtenderProvideStateNotProviding,
	)
}

// Every field of the status survives the rpc wire. It crosses as it stands
// rather than through a hand-written mirror, so this guards a future field
// gob cannot carry -- the defect device_rpc_mirror_test.go exists for.
func TestRpcGobExtenderProvideStatusComplete(t *testing.T) {
	seed := 0
	status := &ExtenderProvideStatus{}
	fillNonZero(t, reflect.ValueOf(status), &seed)

	wired := gobRoundTrip(t, status)
	connect.AssertEqual(t, wired, status)
}
