package sdk

// device_rpc_provider_state_test.go -- a browser-state remote reads the
// provider state of the device it controls (device_rpc_provider_state.go).
//
// The end to end tests run a real device and a browser-state remote over the
// real rpc transport. They order their checks with a barrier rather than a
// wait for a value: testingProviderStateBarrier opens a fresh contract on the
// device and waits until the remote reports its row, which a browser-state
// remote does only from a pushed state that read the device after every
// change made before the barrier. A remote without the push still reports the
// row (the device sends it row by row), so a getter that is not served from
// the pushed state fails right after the barrier rather than at a timeout.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Hands each contract row a remote reports to the test, in order.
type testingProviderContractDetailsListener struct {
	contractDetails chan *ContractDetails
}

func newTestingProviderContractDetailsListener() *testingProviderContractDetailsListener {
	return &testingProviderContractDetailsListener{
		contractDetails: make(chan *ContractDetails, 1024),
	}
}

// Hands the row over. The remote's callback must never wait on the test, so a
// full buffer drops the row; the tests drain it as they go and the buffer
// outlasts any one of them.
func (self *testingProviderContractDetailsListener) ContractDetailsChanged(contractDetails *ContractDetails) {
	select {
	case self.contractDetails <- contractDetails:
	default:
	}
}

// Waits for a row of contractId, or fails.
func (self *testingProviderContractDetailsListener) waitForContract(t *testing.T, contractId connect.Id) *ContractDetails {
	t.Helper()
	timeout := time.After(60 * time.Second)
	for {
		select {
		case contractDetails := <-self.contractDetails:
			if contractDetails != nil && contractDetails.ContractId != nil &&
				contractDetails.ContractId.Cmp(newId(contractId)) == 0 {
				return contractDetails
			}
		case <-timeout:
			t.Fatalf("the remote never reported contract %s", contractId)
			return nil
		}
	}
}

// A space whose api is an unresolvable .example host and whose platform is an
// in-process server on the v4 loopback, so a provider in it dials nothing
// outside the test.
func testingProviderStateNetworkSpace(t *testing.T) (*testingClientLimitPlatform, *NetworkSpace) {
	t.Helper()
	platform := newTestingClientLimitPlatform(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	networkSpace := NewNetworkSpaceWithUrls(ctx, "https://api.provider-state.example", platform.url, strategySettings)
	t.Cleanup(networkSpace.Close)
	return platform, networkSpace
}

// The provider settings of the tests' local devices: no extender role, which
// would bind its fixed ports, and a dns pump that must never leave the machine.
func testingProviderStateDeviceSettings(settings *DeviceLocalSettings) {
	settings.AllowProvider = true
	settings.ProvideExtenderEnabled = false
	settings.DnsPumpHost = "pump.provider-state.example"
}

// A local device with a provider and a browser-state remote over the real rpc
// transport in such a space, synced.
func testingNewBrowserStateProviderPair(t *testing.T) (*testingClientLimitPlatform, *DeviceLocal, *DeviceRemote) {
	t.Helper()
	return testingNewBrowserStateProviderPairWithSettings(t, nil)
}

// The same pair with the local device's settings adjusted first.
func testingNewBrowserStateProviderPairWithSettings(
	t *testing.T,
	configureLocal func(settings *DeviceLocalSettings),
) (*testingClientLimitPlatform, *DeviceLocal, *DeviceRemote) {
	t.Helper()
	platform, networkSpace := testingProviderStateNetworkSpace(t)

	clientId := connect.NewId()
	instanceId := NewId()

	localSettings := testExtenderStatusDeviceSettings()
	localSettings.EnableRpc = true
	testingProviderStateDeviceSettings(localSettings)
	if configureLocal != nil {
		configureLocal(localSettings)
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", instanceId, localSettings, clientId,
	)
	if err != nil {
		t.Fatal(err)
	}

	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	settings.BrowserStateOnly = true
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
		t.Fatal("the browser-state remote did not sync")
	}
	return platform, deviceLocal, deviceRemote
}

// Opens a fresh ingress contract on the device and waits for listener, added
// to the remote, to report it (see the file header).
func testingProviderStateBarrier(
	t *testing.T,
	deviceLocal *DeviceLocal,
	listener *testingProviderContractDetailsListener,
) {
	t.Helper()
	contractId := connect.NewId()
	deviceLocal.updateProviderContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId: contractId,
			Receive:    true,
			Path: connect.TransferPath{
				SourceId:      connect.NewId(),
				DestinationId: connect.NewId(),
			},
			TransferByteCount: 1024,
			Open:              true,
		},
	})
	listener.waitForContract(t, contractId)
}

// The row of contractId in a list, or nil.
func testingContractDetailsRow(contractDetailsList *ContractDetailsList, contractId connect.Id) *ContractDetails {
	if contractDetailsList == nil {
		return nil
	}
	for _, contractDetails := range contractDetailsList.getAll() {
		if contractDetails.ContractId != nil && contractDetails.ContractId.Cmp(newId(contractId)) == 0 {
			return contractDetails
		}
	}
	return nil
}

// Defect 1: a browser-state remote of a providing device reads the provider
// packet stats -- the data a provider app shows as provided -- from the getter
// and through the contract view controller, which polls that getter. Before,
// the getter answered nil whenever no synchronous rpc service was published,
// which for a browser remote is always.
func TestBrowserStateRemoteReadsProviderPacketStats(t *testing.T) {
	_, deviceLocal, deviceRemote := testingNewBrowserStateProviderPair(t)
	listener := newTestingProviderContractDetailsListener()
	sub := deviceRemote.AddProviderIngressContractDetailsChangeListener(listener)
	defer sub.Close()

	// the traffic of an earlier provider generation, which the device's
	// provider packet stats carry for the life of the device
	func() {
		deviceLocal.stateLock.Lock()
		defer deviceLocal.stateLock.Unlock()
		deviceLocal.providerPacketStatsBase = connect.PacketStats{
			RemoteEgressPacketCount:  3,
			RemoteEgressByteCount:    3 * 1024,
			RemoteIngressPacketCount: 7,
			RemoteIngressByteCount:   7 * 1024,
		}
	}()
	want := deviceLocal.GetProviderPacketStats()
	testingProviderStateBarrier(t, deviceLocal, listener)

	got := deviceRemote.GetProviderPacketStats()
	if got == nil {
		t.Fatal("the browser-state remote read no provider packet stats")
	}
	connect.AssertEqual(t, got.RemoteEgressByteCount, want.RemoteEgressByteCount)
	connect.AssertEqual(t, got.RemoteIngressByteCount, want.RemoteIngressByteCount)
	connect.AssertEqual(t, got.RemoteEgressPacketCount, want.RemoteEgressPacketCount)
	connect.AssertEqual(t, got.RemoteIngressPacketCount, want.RemoteIngressPacketCount)

	vc := deviceRemote.OpenContractViewController()
	defer deviceRemote.CloseContractViewController(vc)
	// a sample made now polls the getter above
	vc.sample()
	vcPacketStats := vc.GetProviderPacketStats()
	if vcPacketStats == nil {
		t.Fatal("the contract view controller read no provider packet stats")
	}
	connect.AssertEqual(t, vcPacketStats.RemoteEgressByteCount, want.RemoteEgressByteCount)
	connect.AssertEqual(t, vcPacketStats.RemoteIngressByteCount, want.RemoteIngressByteCount)
}

// Defect 2: a browser-state remote of a device serving a client reads the
// provider contract rows and stats of both directions, and the provider
// contract details view controller built on it counts the client. Before, the
// getters answered nil, so the controller -- which reads the rows back from
// the getters whenever a listener fires -- showed no client served.
func TestBrowserStateRemoteReadsProviderContracts(t *testing.T) {
	_, deviceLocal, deviceRemote := testingNewBrowserStateProviderPair(t)
	ingressListener := newTestingProviderContractDetailsListener()
	ingressSub := deviceRemote.AddProviderIngressContractDetailsChangeListener(ingressListener)
	defer ingressSub.Close()
	egressListener := newTestingProviderContractDetailsListener()
	egressSub := deviceRemote.AddProviderEgressContractDetailsChangeListener(egressListener)
	defer egressSub.Close()

	vc := deviceRemote.OpenProviderContractDetailsViewController()
	defer deviceRemote.CloseContractDetailsViewController(vc)
	rowsChanged := make(chan struct{}, 1)
	rowsSub := vc.AddContractRowsListener(&testing_contractRowsListener{
		onChange: func() {
			select {
			case rowsChanged <- struct{}{}:
			default:
			}
		},
	})
	defer rowsSub.Close()
	vc.Start()

	provider := connect.NewId()
	client := connect.NewId()
	stream := connect.NewId()
	ingressContractId := connect.NewId()
	egressContractId := connect.NewId()
	deviceLocal.updateProviderContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId:         ingressContractId,
			Receive:            true,
			Path:               connect.TransferPath{SourceId: client, DestinationId: provider, StreamId: stream},
			TransferByteCount:  64 * 1024,
			UsedByteCount:      4 * 1024,
			UsedByteCountDelta: 4 * 1024,
			Open:               true,
		},
		{
			ContractId:         egressContractId,
			Receive:            false,
			Companion:          true,
			Path:               connect.TransferPath{SourceId: provider, DestinationId: client, StreamId: stream},
			TransferByteCount:  64 * 1024,
			UsedByteCount:      2 * 1024,
			UsedByteCountDelta: 2 * 1024,
			Open:               true,
		},
	})
	ingressListener.waitForContract(t, ingressContractId)
	egressListener.waitForContract(t, egressContractId)

	ingressRow := testingContractDetailsRow(deviceRemote.GetProviderIngressContractDetails(), ingressContractId)
	if ingressRow == nil {
		t.Fatalf("the browser-state remote read no ingress row, read %v", deviceRemote.GetProviderIngressContractDetails())
	}
	egressRow := testingContractDetailsRow(deviceRemote.GetProviderEgressContractDetails(), egressContractId)
	if egressRow == nil {
		t.Fatalf("the browser-state remote read no egress row, read %v", deviceRemote.GetProviderEgressContractDetails())
	}
	connect.AssertEqual(t, ingressRow, testingContractDetailsRow(deviceLocal.GetProviderIngressContractDetails(), ingressContractId))
	connect.AssertEqual(t, egressRow, testingContractDetailsRow(deviceLocal.GetProviderEgressContractDetails(), egressContractId))
	// the path that names the client, and the stream the contract rides
	if path := ingressRow.ContractTransferPath; path == nil || path.SourceId.Cmp(newId(client)) != 0 || path.StreamId.Cmp(newId(stream)) != 0 {
		t.Fatalf("the ingress row lost its path: %+v", path)
	}
	connect.AssertEqual(t, deviceRemote.GetProviderIngressContractStats(), deviceLocal.GetProviderIngressContractStats())
	connect.AssertEqual(t, deviceRemote.GetProviderEgressContractStats(), deviceLocal.GetProviderEgressContractStats())

	// the controller's rows follow; the client is one row with one contract in
	// each direction
	timeout := time.After(60 * time.Second)
	for {
		row := testing_rowForClient(vc.GetContractRows(), client.String())
		if row != nil && row.ReceiveContracts.Len() == 1 && row.SendContracts.Len() == 1 {
			break
		}
		select {
		case <-rowsChanged:
		case <-timeout:
			t.Fatalf("the provider contract details controller never showed the client, rows %d", vc.GetContractRows().Len())
		}
	}
}

// Defect 3: provide changes made after the first sync reach a browser-state
// remote that holds no listener for them. Before, its getters read the values
// of the first sync for good.
func TestBrowserStateRemoteReadsLaterProvideChanges(t *testing.T) {
	_, deviceLocal, deviceRemote := testingNewBrowserStateProviderPair(t)
	listener := newTestingProviderContractDetailsListener()
	sub := deviceRemote.AddProviderIngressContractDetailsChangeListener(listener)
	defer sub.Close()
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), false)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModeNone)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)

	deviceLocal.SetProvidePaused(true)
	connect.AssertEqual(t, deviceLocal.GetProvidePaused(), true)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), true)

	deviceLocal.SetProvidePaused(false)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), false)

	deviceLocal.SetProvideMode(ProvideModePublic)
	connect.AssertEqual(t, deviceLocal.GetProvideEnabled(), true)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModePublic)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), true)

	deviceLocal.SetProvideMode(ProvideModeNone)
	connect.AssertEqual(t, deviceLocal.GetProvideEnabled(), false)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModeNone)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), false)
}

// Beside defect 3: a browser-state remote of a device in the auto control mode
// reads the provide mode and enabled state the device holds as a connection
// comes and goes. Before, it derived them from the control mode and the connect
// state of the first sync, so it read network for a device that was not
// providing, and kept reading network after the device turned public.
func TestBrowserStateRemoteReadsTheProvideStateOfAnAutoDevice(t *testing.T) {
	_, deviceLocal, deviceRemote := testingNewBrowserStateProviderPairWithSettings(t, func(settings *DeviceLocalSettings) {
		settings.DefaultProvideControlMode = ProvideControlModeAuto
	})
	listener := newTestingProviderContractDetailsListener()
	sub := deviceRemote.AddProviderIngressContractDetailsChangeListener(listener)
	defer sub.Close()
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), deviceLocal.GetProvideMode())
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), deviceLocal.GetProvideEnabled())

	// a connection turns an auto device public
	deviceLocal.SetConnectLocation(testingSpecificPreferenceLocation())
	connect.AssertEqual(t, deviceLocal.GetProvideMode(), ProvideModePublic)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModePublic)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), deviceLocal.GetProvideEnabled())

	// and its end leaves the device providing to its network only
	deviceLocal.RemoveDestination()
	connect.AssertEqual(t, deviceLocal.GetProvideMode(), ProvideModeNetwork)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModeNetwork)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), deviceLocal.GetProvideEnabled())
}

// Defect 5, the client limit status: a browser-state remote without a client
// limit listener reads a hold that starts after the first sync. Before, it read
// the none of the first sync for good. A second browser-state remote with a
// listener hears the hold too.
func TestBrowserStateRemoteReadsClientLimitStatus(t *testing.T) {
	platform, deviceLocal, deviceRemote := testingNewBrowserStateProviderPair(t)
	listener := newTestingProviderContractDetailsListener()
	sub := deviceRemote.AddProviderIngressContractDetailsChangeListener(listener)
	defer sub.Close()
	clientLimitListener := newTestingClientLimitListener()
	clientLimitSub := deviceLocal.AddClientLimitStatusChangeListener(clientLimitListener)
	defer clientLimitSub.Close()

	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	settings.BrowserStateOnly = true
	listeningRemote, err := newDeviceRemoteWithOverrides(
		deviceLocal.networkSpace, "", deviceLocal.GetInstanceId(), settings, deviceLocal.clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer listeningRemote.Close()
	remoteClientLimitListener := newTestingClientLimitListener()
	remoteClientLimitSub := listeningRemote.AddClientLimitStatusChangeListener(remoteClientLimitListener)
	defer remoteClientLimitSub.Close()

	deviceLocal.SetProvideMode(ProvideModePublic)
	declared := platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	waitProviderSettled(t, deviceLocal)
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), noneClientLimitStatus())

	declared.closeForClientLimit()
	var status *ClientLimitStatus
	for status == nil || status.Status == ClientLimitStatusNone {
		status = clientLimitListener.next(t, "the client limit close reached no device listener")
	}
	testingProviderStateBarrier(t, deviceLocal, listener)
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus(), deviceLocal.GetClientLimitStatus())
	connect.AssertEqual(t, deviceRemote.GetClientLimitStatus().Status, ClientLimitStatusExceeded)

	// the sync hands the listener the current status when it attaches, so
	// skip any none until the hold arrives
	var remoteStatus *ClientLimitStatus
	for remoteStatus == nil || remoteStatus.Status == ClientLimitStatusNone {
		remoteStatus = remoteClientLimitListener.next(t, "the client limit close reached no browser-state remote listener")
	}
	connect.AssertEqual(t, remoteStatus, deviceLocal.GetClientLimitStatus())
}

// Defect 5, the provider's connected state: a browser-state remote reads it
// as the device's provider connects to an in-process platform and as a client
// limit hold disconnects it. Before, a remote had no connected getter at all.
func TestBrowserStateRemoteReadsProviderConnected(t *testing.T) {
	platform, deviceLocal, deviceRemote := testingNewBrowserStateProviderPair(t)
	listener := newTestingProviderContractDetailsListener()
	sub := deviceRemote.AddProviderIngressContractDetailsChangeListener(listener)
	defer sub.Close()

	deviceLocal.SetProvideMode(ProvideModePublic)
	declared := platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	waitProviderSettled(t, deviceLocal)
	testingProviderStateBarrier(t, deviceLocal, listener)
	if !deviceRemote.GetProviderConnected() {
		t.Fatal("the browser-state remote read the connected provider as not connected")
	}

	declared.closeForClientLimit()
	if !waitProviderCondition(60*time.Second, func() bool {
		return !deviceLocal.GetProviderConnected()
	}) {
		t.Fatal("the held provider stayed connected")
	}
	testingProviderStateBarrier(t, deviceLocal, listener)
	if deviceRemote.GetProviderConnected() {
		t.Fatal("the browser-state remote read the held provider as connected")
	}
}

// A native remote reads the provider's connected state through to the device,
// and never asks for or holds the pushed provider state: its provider getters
// read through as they always did.
func TestDeviceRemoteReadsProviderConnectedThrough(t *testing.T) {
	platform, networkSpace := testingProviderStateNetworkSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		testingProviderStateDeviceSettings,
	)
	connect.AssertEqual(t, deviceRemote.settings.BrowserStateOnly, false)

	deviceLocal.SetProvideMode(ProvideModePublic)
	platform.nextConnectionDeclaring(t, connect.ProvideIntentDeclared)
	waitProviderSettled(t, deviceLocal)
	connect.AssertEqual(t, deviceRemote.GetProviderConnected(), true)

	deviceRemote.stateLock.Lock()
	lastProviderState := deviceRemote.lastProviderState
	deviceRemote.stateLock.Unlock()
	if lastProviderState != nil {
		t.Fatalf("the native remote holds a pushed provider state: %+v", lastProviderState)
	}
	packetStats := deviceRemote.GetProviderPacketStats()
	if packetStats == nil {
		t.Fatal("the native remote read no provider packet stats through")
	}

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	// the last value read stays while the device process is gone
	connect.AssertEqual(t, deviceRemote.GetProviderConnected(), true)
}

// The device answers a remote that asks for the provider state with the state
// in the sync reply, and does not register that remote's provider contract
// details listeners row by row: the rows reach them from the state, and a row
// sent alone could reach a listener before the state that holds it. A remote
// that does not ask gets the rows one by one, as before, and no state.
func TestDeviceLocalRpcSyncAnswersTheProviderState(t *testing.T) {
	_, networkSpace := testingProviderStateNetworkSpace(t)
	settings := testExtenderStatusDeviceSettings()
	testingProviderStateDeviceSettings(settings)
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, "", "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)

	for _, providerStateListener := range []bool{true, false} {
		server, client := testingPreferenceRpc(t, deviceLocal)
		request := &DeviceRemoteSyncRequest{
			InstanceId: deviceLocal.instanceId,
			RpcVersion: DeviceRpcVersion,
			ProviderEgressContractStatsChangeListenerIds:    []connect.Id{connect.NewId()},
			ProviderEgressContractDetailsChangeListenerIds:  []connect.Id{connect.NewId()},
			ProviderIngressContractDetailsChangeListenerIds: []connect.Id{connect.NewId()},
			ProviderStateListener:                           providerStateListener,
		}
		var response *DeviceRemoteSyncResponse
		if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error != "" {
			t.Fatalf("the sync was refused: %s", response.Error)
		}
		server.stateLock.Lock()
		providerStateSubs := server.providerStateSubs
		egressStatsSub := server.providerEgressContractStatsChangeListenerSub
		egressDetailsSub := server.providerEgressContractDetailsChangeListenerSub
		ingressDetailsSub := server.providerIngressContractDetailsChangeListenerSub
		server.stateLock.Unlock()

		if egressStatsSub == nil {
			t.Fatalf("asking for the state %t: the provider contract stats listener was not registered", providerStateListener)
		}
		if providerStateListener {
			if response.ProviderState == nil || !response.ProviderState.Provider {
				t.Fatalf("the reply carried no provider state: %+v", response.ProviderState)
			}
			if providerStateSubs == nil {
				t.Fatal("the device did not subscribe to the provider state")
			}
			if egressDetailsSub != nil || ingressDetailsSub != nil {
				t.Fatal("the device registered the contract rows one by one beside the state")
			}
		} else {
			if response.ProviderState != nil {
				t.Fatalf("the reply carried a provider state nobody asked for: %+v", response.ProviderState)
			}
			if providerStateSubs != nil {
				t.Fatal("the device subscribed to a provider state nobody asked for")
			}
			if egressDetailsSub == nil || ingressDetailsSub == nil {
				t.Fatal("the device did not register the contract rows one by one")
			}
		}
	}
}

// A push read the device after the sync reply of its connection did, and the
// two cross on different streams, so a reply taken after a push must not roll
// the remote's copy back. A device that answers no state drops the copy.
func TestDeviceRemoteSyncReplyNeverReplacesANewerPush(t *testing.T) {
	deviceRemote := &DeviceRemote{
		providerEgressContractDetailsChangeListeners:  map[connect.Id]ContractDetailsChangeListener{},
		providerIngressContractDetailsChangeListeners: map[connect.Id]ContractDetailsChangeListener{},
	}
	sync := func(lastKnownState DeviceRemoteState, providerState *DeviceRemoteProviderState) {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.lastKnownState = lastKnownState
		deviceRemote.syncProviderStateWithLock(providerState)
	}
	reply := &DeviceRemoteProviderState{
		Provider:          true,
		ProvidePaused:     false,
		ProviderConnected: false,
		PacketStats:       &PacketStatsRpc{RemoteEgressByteCount: 1},
	}
	replyState := DeviceRemoteState{}
	replyState.ProvidePaused.Set(false)

	// the push lands first
	deviceRemote.providerStateChanged(&DeviceRemoteProviderState{
		Provider:          true,
		ProvidePaused:     true,
		ProviderConnected: true,
		PacketStats:       &PacketStatsRpc{RemoteEgressByteCount: 2},
	})
	sync(replyState, reply)
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), true)
	connect.AssertEqual(t, deviceRemote.GetProviderConnected(), true)
	connect.AssertEqual(t, deviceRemote.GetProviderPacketStats().RemoteEgressByteCount, ByteCount(2))

	// a new connection: with no push yet its reply is the newest state
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.providerStatePushed = false
	}()
	sync(replyState, reply)
	connect.AssertEqual(t, deviceRemote.GetProvidePaused(), false)
	connect.AssertEqual(t, deviceRemote.GetProviderConnected(), false)
	connect.AssertEqual(t, deviceRemote.GetProviderPacketStats().RemoteEgressByteCount, ByteCount(1))

	// a device that pushes none: nothing would refresh the copy
	sync(replyState, nil)
	if packetStats := deviceRemote.GetProviderPacketStats(); packetStats != nil {
		t.Fatalf("the remote read a dropped state: %+v", packetStats)
	}
	if contractDetails := deviceRemote.GetProviderIngressContractDetails(); contractDetails != nil {
		t.Fatalf("the remote read dropped rows: %+v", contractDetails)
	}
}

// A push fans its contract rows out to the provider contract details
// listeners as the device fans out an emit: every row of a direction whose
// rows changed, and every row on the first push of a connection, which is the
// state a listener gets on a sync. A push that changed no row hands none.
func TestDeviceRemoteProviderStateFansOutChangedRows(t *testing.T) {
	deviceRemote := &DeviceRemote{
		providerEgressContractDetailsChangeListeners:  map[connect.Id]ContractDetailsChangeListener{},
		providerIngressContractDetailsChangeListeners: map[connect.Id]ContractDetailsChangeListener{},
	}
	listener := newTestingProviderContractDetailsListener()
	deviceRemote.providerIngressContractDetailsChangeListeners[connect.NewId()] = listener
	contractId := connect.NewId()
	push := func(usedByteCount ByteCount, packetByteCount ByteCount) {
		deviceRemote.providerStateChanged(&DeviceRemoteProviderState{
			Provider:    true,
			PacketStats: &PacketStatsRpc{RemoteIngressByteCount: packetByteCount},
			IngressContractDetails: []*ContractDetailsRpc{
				newContractDetailsRpc(&ContractDetails{
					ContractId:            newId(contractId),
					ContractUsedByteCount: usedByteCount,
					ContractByteCount:     1024 * 1024,
					ContractTransferPath:  fromConnect(connect.TransferPath{SourceId: connect.Id{1}, DestinationId: connect.Id{2}}),
					Status:                ContractStatusOpen,
				}),
			},
		})
	}
	// the fan out runs in the push, so each check reads what that push handed
	handed := func() []ByteCount {
		usedByteCounts := []ByteCount{}
		for {
			select {
			case contractDetails := <-listener.contractDetails:
				usedByteCounts = append(usedByteCounts, contractDetails.ContractUsedByteCount)
			default:
				return usedByteCounts
			}
		}
	}

	push(100, 1)
	connect.AssertEqual(t, handed(), []ByteCount{100})
	push(100, 2)
	connect.AssertEqual(t, handed(), []ByteCount{})
	push(200, 2)
	connect.AssertEqual(t, handed(), []ByteCount{200})
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.providerStatePushed = false
	}()
	push(200, 2)
	connect.AssertEqual(t, handed(), []ByteCount{200})
}

// The pushed state crosses as gob whole: every field, filled, survives the
// wire. Every field must be filled here, so a field added later fails until
// the test fills it too.
func TestRpcGobProviderStateComplete(t *testing.T) {
	seed := 0
	filled := func(value any) {
		fillNonZero(t, reflect.ValueOf(value), &seed)
	}
	clientLimitStatus := &ClientLimitStatus{}
	filled(clientLimitStatus)
	egressContractStats := &ContractStats{}
	filled(egressContractStats)
	ingressContractStats := &ContractStats{}
	filled(ingressContractStats)
	egressContractDetails := &ContractDetails{}
	filled(egressContractDetails)
	ingressContractDetails := &ContractDetails{}
	filled(ingressContractDetails)
	// the packet stats nest themselves per carrier, which fillNonZero would
	// follow without end, so the counts are filled flat and one carrier is added
	packetStats := &PacketStatsRpc{}
	packetStatsValue := reflect.ValueOf(packetStats).Elem()
	for i := range packetStatsValue.NumField() {
		if field := packetStatsValue.Field(i); field.Kind() == reflect.Int64 {
			seed += 1
			field.SetInt(int64(seed))
		}
	}
	packetStats.TransportStats = []*TransportPacketStatsRpc{
		{
			TransportType:              TransportTypeH1,
			Stats:                      &PacketStatsRpc{RemoteEgressByteCount: 1024},
			H1WebSocketConnectionCount: 1,
			H1PlusConnectionCount:      2,
		},
	}

	providerState := &DeviceRemoteProviderState{
		Provider:               true,
		ProvideEnabled:         true,
		ProvidePaused:          true,
		ProvideMode:            ProvideModePublic,
		ProviderConnected:      true,
		ClientLimitStatus:      clientLimitStatus,
		PacketStats:            packetStats,
		EgressContractStats:    egressContractStats,
		IngressContractStats:   ingressContractStats,
		EgressContractDetails:  []*ContractDetailsRpc{newContractDetailsRpc(egressContractDetails)},
		IngressContractDetails: []*ContractDetailsRpc{newContractDetailsRpc(ingressContractDetails)},
	}
	providerStateValue := reflect.ValueOf(providerState).Elem()
	for i := range providerStateValue.NumField() {
		if providerStateValue.Field(i).IsZero() {
			t.Fatalf("DeviceRemoteProviderState.%s is not filled", providerStateValue.Type().Field(i).Name)
		}
	}
	connect.AssertEqual(t, gobRoundTrip(t, providerState), providerState)
}
