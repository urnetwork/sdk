//go:build !ios_extension && !js

package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A sidecar role's core a test scripts: what is due, and the setups it ran.
type testingSetupCore struct {
	stateLock  sync.Mutex
	networkId  sidecar.Id
	due        sidecar.ForwardingSetupReason
	confirmed  int
	setUpKeys  []*bridge.ForwardingKeys
	witnessPin *bridge.AuthPin
	fork       bool
}

func (self *testingSetupCore) NetworkId() *sidecar.Id {
	networkId := self.networkId
	return &networkId
}

func (self *testingSetupCore) ForwardingSetupDue(networkId sidecar.Id) (sidecar.ForwardingSetupReason, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.due, nil
}

func (self *testingSetupCore) ConfirmForwardingSetup(networkId sidecar.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.confirmed += 1
	if self.due == sidecar.ForwardingSetupForeignRevocation {
		self.due = sidecar.ForwardingSetupNoneLive
	}
	return nil
}

func (self *testingSetupCore) SetUpForwarding(ctx context.Context, networkId sidecar.Id, keys *bridge.ForwardingKeys) (*bridge.ForwardingHandOff, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.due == sidecar.ForwardingSetupForeignRevocation {
		return nil, sidecar.ErrForwardingSetupNeedsConfirm
	}
	self.setUpKeys = append(self.setUpKeys, keys)
	self.due = sidecar.ForwardingSetupNotDue
	return &bridge.ForwardingHandOff{KeySetId: keys.GetKeySetId(), ForwardClientJwt: "synthetic-forward-jwt",
		StrictExpMs: time.Now().Add(time.Hour).UnixMilli()}, nil
}

func (self *testingSetupCore) ForwardingWitness(networkId sidecar.Id) (*sidecar.ForwardingWitness, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &sidecar.ForwardingWitness{Pin: self.witnessPin, Fork: self.fork, SetupDue: self.due}, nil
}

// The VPN component's side of the channel, recording what reached it.
type testingForwardingChannel struct {
	stateLock   sync.Mutex
	keys        *bridge.ForwardingKeys
	handOffs    []*bridge.ForwardingHandOff
	witnesses   []*bridge.AuthPin
	forks       []bool
	unreachable bool
	// signaled at each witness, when set
	witnessed chan struct{}
}

// A VPN device that survives reopening the app; the status and key set belong to the component.
type testingAttachedForwardingDevice struct {
	Device
	*testingForwardingChannel
	status *BridgeStatus
	handed chan struct{}
}

func (self *testingAttachedForwardingDevice) GetBridgeStatus() *BridgeStatus {
	return self.status
}

func (self *testingAttachedForwardingDevice) forwardingHandOff(handOff *bridge.ForwardingHandOff) error {
	if err := self.testingForwardingChannel.forwardingHandOff(handOff); err != nil {
		return err
	}
	self.handed <- struct{}{}
	return nil
}

// A reopened app reattaches without enrollment. If the previous app exited after minting and before the
// component received its hand-off, the generated set is resumed even though rotation is not due.
func TestSetForwardingDeviceResumesAnUndeliveredSetAfterReopen(t *testing.T) {
	testingForwardingReattach(t, &BridgeNetworkStatus{KeySet: deviceForwardKeySetGenerated})
}

func TestSetForwardingDeviceRenewsALapsedClientAfterReopen(t *testing.T) {
	testingForwardingReattach(t, &BridgeNetworkStatus{KeySet: deviceForwardKeySetActive, Lapsed: true})
}

func testingForwardingReattach(t *testing.T, networkStatus *BridgeNetworkStatus) {
	t.Helper()
	world := newTestSidecarWorld(t)
	first := world.open(t)
	world.close(t, first)
	reopened := world.open(t)
	t.Cleanup(func() { world.close(t, reopened) })
	core := &testingSetupCore{networkId: sidecar.NewId(), witnessPin: &bridge.AuthPin{Version: 4}}
	reopened.forwardingState().core = core
	status := &BridgeStatus{Networks: NewBridgeNetworkStatusList()}
	networkStatus.NetworkId = newId([16]byte(core.networkId))
	status.Networks.Add(networkStatus)
	channel := &testingAttachedForwardingDevice{
		testingForwardingChannel: &testingForwardingChannel{
			keys: &bridge.ForwardingKeys{KeySetId: connect.NewId().Bytes()}, witnessed: make(chan struct{}, 1),
		},
		status: status,
		handed: make(chan struct{}, 1),
	}
	reopened.SetForwardingDevice(channel)
	select {
	case <-channel.handed:
	case <-time.After(testSidecarTimeout):
		t.Fatal("reopening the app left the VPN component without its hand-off")
	}
	if len(core.setUpKeys) != 1 || !proto.Equal(core.setUpKeys[0], channel.keys) || core.confirmed != 0 {
		t.Fatal("reattachment changed the component's keys or confirmed a withdrawal")
	}
}

func (self *testingForwardingChannel) forwardingKeys(networkId connect.Id) (*bridge.ForwardingKeys, error) {
	return self.keys, nil
}

func (self *testingForwardingChannel) forwardingHandOff(handOff *bridge.ForwardingHandOff) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.unreachable {
		return errDeviceForwardingUnreachable
	}
	self.handOffs = append(self.handOffs, handOff)
	return nil
}

func (self *testingForwardingChannel) forwardingWitness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.witnesses = append(self.witnesses, pin)
	self.forks = append(self.forks, fork)
	if self.witnessed != nil {
		select {
		case self.witnessed <- struct{}{}:
		default:
		}
	}
	return nil
}

func (self *testingForwardingChannel) handOffCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.handOffs)
}

// A local Sidecar whose forwarding setup runs on core, with the device channel known.
func testingSetupSidecar(t *testing.T, core *testingSetupCore, channel *testingForwardingChannel) *Sidecar {
	t.Helper()
	world := newTestSidecarWorld(t)
	local := world.open(t)
	t.Cleanup(func() { world.close(t, local) })
	forwarding := local.forwardingState()
	forwarding.core = core
	forwarding.stateLock.Lock()
	forwarding.device = channel
	forwarding.stateLock.Unlock()
	return local
}

// After an enrollment the setup runs: the component's keys go to the core's setup, and its hand-off to the component.
func TestSetupRunsAfterEnrollment(t *testing.T) {
	core := &testingSetupCore{networkId: sidecar.NewId(), due: sidecar.ForwardingSetupNewClientGeneration}
	channel := &testingForwardingChannel{keys: &bridge.ForwardingKeys{KeySetId: connect.NewId().Bytes()}}
	local := testingSetupSidecar(t, core, channel)
	if err := local.runForwardingSetup(false); err != nil {
		t.Fatal(err)
	}
	if len(core.setUpKeys) != 1 || !proto.Equal(core.setUpKeys[0], channel.keys) {
		t.Error("the core's setup did not take the component's keys")
	}
	if channel.handOffCount() != 1 || !bytes.Equal(channel.handOffs[0].GetKeySetId(), channel.keys.GetKeySetId()) {
		t.Error("the hand-off did not reach the component")
	}
	// nothing due: no second setup
	if err := local.runForwardingSetup(false); err != nil || len(core.setUpKeys) != 1 {
		t.Errorf("a setup ran with nothing due: %v", err)
	}
}

// After another signer's revocation the setup waits for the person: bridge_alarm forwarding_revoked is raised and no
// setup runs, until ConfirmForwardingSetup records the confirmation and runs it.
func TestForeignRevocationWaitsForConfirm(t *testing.T) {
	core := &testingSetupCore{networkId: sidecar.NewId(), due: sidecar.ForwardingSetupForeignRevocation}
	channel := &testingForwardingChannel{keys: &bridge.ForwardingKeys{KeySetId: connect.NewId().Bytes()}}
	local := testingSetupSidecar(t, core, channel)
	received := make(chan *SidecarEvent, 16)
	local.AddEventListener(&testSidecarListener{events: received})
	if err := local.runForwardingSetup(false); !errors.Is(err, sidecar.ErrForwardingSetupNeedsConfirm) {
		t.Errorf("a foreign revocation's setup answered %v", err)
	}
	if len(core.setUpKeys) != 0 || channel.handOffCount() != 0 {
		t.Fatal("a setup ran before the person confirmed")
	}
	for alarmed := false; !alarmed; {
		select {
		case event := <-received:
			if event.Kind != "bridge_alarm" {
				continue
			}
			if event.BridgeAlarm != SidecarBridgeAlarmForwardingRevoked {
				t.Errorf("event %+v", event)
			}
			alarmed = true
		case <-time.After(testSidecarTimeout):
			t.Fatal("no forwarding_revoked alarm")
		}
	}
	confirm := newTestEvsCallback[*SidecarResult]()
	local.ConfirmForwardingSetup(newId([16]byte(core.networkId)), confirm)
	if result, err := confirm.await(t); err != nil || !result.Ok {
		t.Fatalf("the confirmation answered %+v, %v", result, err)
	}
	if core.confirmed != 1 || len(core.setUpKeys) != 1 || channel.handOffCount() != 1 {
		t.Errorf("after the confirmation: %d confirmations, %d setups, %d hand-offs", core.confirmed, len(core.setUpKeys), channel.handOffCount())
	}
	other := newTestEvsCallback[*SidecarResult]()
	local.ConfirmForwardingSetup(NewId(), other)
	if result, _ := other.await(t); result.Ok {
		t.Error("a confirmation for another network was taken")
	}
}

// A hand-off the component cannot take while its process is down is kept, in memory alone, and handed at the next
// witness.
func TestHandOffQueuedWhileDisconnected(t *testing.T) {
	core := &testingSetupCore{networkId: sidecar.NewId(), due: sidecar.ForwardingSetupNewClientGeneration, witnessPin: &bridge.AuthPin{Version: 3}}
	channel := &testingForwardingChannel{keys: &bridge.ForwardingKeys{KeySetId: connect.NewId().Bytes()}, unreachable: true}
	local := testingSetupSidecar(t, core, channel)
	if err := local.runForwardingSetup(false); !errors.Is(err, errDeviceForwardingUnreachable) {
		t.Errorf("an unreachable component answered %v", err)
	}
	if local.forwardingState().pendingHandOff == nil {
		t.Fatal("the undelivered hand-off was not kept")
	}
	channel.stateLock.Lock()
	channel.unreachable = false
	channel.stateLock.Unlock()
	if !local.sendForwardingWitness(context.Background()) {
		t.Error("the witness did not go")
	}
	if channel.handOffCount() != 1 || local.forwardingState().pendingHandOff != nil {
		t.Error("the kept hand-off was not handed at the next witness")
	}
	if len(core.setUpKeys) != 1 {
		t.Errorf("%d setups for one hand-off", len(core.setUpKeys))
	}
	// the witness goes only when the pin or the fork moved
	if local.sendForwardingWitness(context.Background()) {
		t.Error("a witness went with nothing moved")
	}
	core.stateLock.Lock()
	core.fork = true
	core.stateLock.Unlock()
	if !local.sendForwardingWitness(context.Background()) || !channel.forks[len(channel.forks)-1] {
		t.Error("a fork did not go to the component")
	}
}

// What the channel carries out of the component holds no private key: the RPC's keys answer is public halves alone.
func TestForwardingChannelCarriesNoPrivateKey(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	rpc := &DeviceLocalRpc{deviceLocal: device}
	var reply *DeviceRemoteForwardingKeys
	if err := rpc.ForwardingKeys(&DeviceRemoteForwardingKeysArgs{NetworkId: networkId}, &reply); err != nil {
		t.Fatal(err)
	}
	for _, name := range forward.store.names() {
		value := forward.store.value(t, name)
		if 16 < len(value) && bytes.Contains(reply.Keys, value) {
			t.Errorf("the keys answer carries %s", name)
		}
	}
	keys := &bridge.ForwardingKeys{}
	if err := proto.Unmarshal(reply.Keys, keys); err != nil || len(keys.GetForwardPublicKey()) != 32 {
		t.Errorf("the keys answer decodes as %v, %v", keys, err)
	}
}

// An unsigned or forged hand-off over the RPC is refused by the component and lowers nothing, and a witness for a
// network with no forwarder changes nothing (DP1).
func TestUnsignedResponseOverRpcIgnored(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	rpc := &DeviceLocalRpc{deviceLocal: device}
	var reply *DeviceRemoteForwardingKeys
	if err := rpc.ForwardingKeys(&DeviceRemoteForwardingKeysArgs{NetworkId: networkId}, &reply); err != nil {
		t.Fatal(err)
	}
	keys := &bridge.ForwardingKeys{}
	proto.Unmarshal(reply.Keys, keys)
	unsigned := forward.identity.handOff(t, keys, forward.pending(), nil)
	unsigned.SignedDelegation = nil
	unsignedBytes, _ := proto.Marshal(unsigned)
	if err := rpc.ForwardingHandOff(&DeviceRemoteForwardingHandOff{HandOff: unsignedBytes}, nil); err == nil {
		t.Error("an unsigned hand-off was taken")
	}
	if err := rpc.ForwardingHandOff(&DeviceRemoteForwardingHandOff{HandOff: []byte{0xff, 0xff}}, nil); err == nil {
		t.Error("bytes that do not decode were taken")
	}
	pinBytes, _ := proto.Marshal(&bridge.AuthPin{Version: 9})
	if err := rpc.ForwardingWitness(&DeviceRemoteForwardingWitness{NetworkId: networkId, Pin: pinBytes}, nil); err == nil {
		t.Error("a witness for a network with no forwarder was taken")
	}
	if state := device.bridgeOwner.roleDecision(networkId).State; state != 1 {
		t.Errorf("the posture moved to %v", state)
	}
	status := device.GetBridgeStatus()
	if status.State != "legacy" || status.AclEnforcingMint() {
		t.Errorf("status %+v", status)
	}
}

// The status mirror carries each forwarding role's fields, and the device client is minted acl_enforcing only once its
// role left legacy.
func TestBridgeStatusCarriesTheForwardingFields(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	changed := make(chan *BridgeStatus, 16)
	sub := device.AddBridgeStatusChangeListener(&testingBridgeStatusListener{changed: changed})
	defer sub.Close()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	status := device.GetBridgeStatus()
	if status.Networks.Len() != 1 {
		t.Fatalf("%d forwarding roles in the status", status.Networks.Len())
	}
	network := status.Networks.Get(0)
	if network.NetworkId.id != [16]byte(networkId) || !network.Hosting || network.Posture != "pending" || network.KeySet != deviceForwardKeySetDelegated || network.ForwardClientExpiresMillis == 0 {
		t.Errorf("the role's fields %+v", network)
	}
	if !status.AclEnforcingMint() {
		t.Error("a role past legacy does not mint acl_enforcing")
	}
	select {
	case moved := <-changed:
		if moved.State == "" {
			t.Error("a status with no state")
		}
	case <-time.After(testSidecarTimeout):
		t.Error("no status change was heard")
	}
	rpc := &DeviceLocalRpc{deviceLocal: device}
	var mirrored *DeviceRemoteBridgeStatus
	if err := rpc.GetBridgeStatus(0, &mirrored); err != nil || len(mirrored.Networks) != 1 || mirrored.toBridgeStatus().Networks.Len() != 1 {
		t.Errorf("the RPC's status %+v, %v", mirrored, err)
	}
	encoded, _ := json.Marshal(mirrored)
	if bytes.Contains(encoded, []byte("synthetic")) {
		t.Error("the status carries a token")
	}
	if DeviceRpcVersion != 3 {
		t.Errorf("DeviceRpcVersion is %d: forwarding's calls are additive", DeviceRpcVersion)
	}
}

// A fork the core found reaches the component at the head_alarm the core raises for it, not at the next interval, so
// the component holds pending on it promptly.
func TestForkWitnessedAtTheHeadAlarm(t *testing.T) {
	core := &testingSetupCore{networkId: sidecar.Id(connect.NewId()), witnessPin: &bridge.AuthPin{Version: 5}}
	channel := &testingForwardingChannel{witnessed: make(chan struct{}, 1)}
	local := testingSetupSidecar(t, core, channel)
	forwarding := local.forwardingState()
	forwarding.settings.WitnessInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go local.runForwardingWitness(ctx)
	waitWitness := func(when string) {
		t.Helper()
		select {
		case <-channel.witnessed:
		case <-time.After(testSidecarTimeout):
			t.Fatalf("no witness reached the component %s", when)
		}
	}
	waitWitness("at the loop's start")
	core.stateLock.Lock()
	core.fork = true
	core.stateLock.Unlock()
	local.events.offerFresh(sidecar.SidecarEvent{Kind: sidecar.SidecarEventHeadAlarm, HeadAlarm: sidecar.SidecarHeadAlarmFork})
	waitWitness("at the head_alarm")
	channel.stateLock.Lock()
	defer channel.stateLock.Unlock()
	if len(channel.forks) != 2 || channel.forks[0] || !channel.forks[1] || channel.witnesses[1].GetVersion() != 5 {
		t.Errorf("the component was told forks %v", channel.forks)
	}
}
