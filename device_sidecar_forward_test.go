//go:build !js

package sdk

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// Answers every request 503, so a test's Forwarders follow nothing and dial nothing real.
type testingForwardTransport struct{}

func (self testingForwardTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		request.Body.Close()
	}
	return &http.Response{
		StatusCode: 503,
		Status:     "503 Service Unavailable",
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    request,
	}, nil
}

// A device role with forwarding on: the VPN component's key store and its own directory.
type testingForwardDevice struct {
	role      *testingDeviceRole
	store     *testingForwardKeyStore
	stateDir  string
	identity  *testingForwardIdentity
	transport http.RoundTripper
}

func newTestingForwardDevice(t *testing.T) *testingForwardDevice {
	t.Helper()
	return &testingForwardDevice{
		role:      newTestingDeviceRole(t),
		store:     newTestingForwardKeyStore(),
		stateDir:  t.TempDir(),
		identity:  newTestingForwardIdentity(t),
		transport: testingForwardTransport{},
	}
}

// A device on the role's space with forwarding on, settings adjusted by adjust.
func (self *testingForwardDevice) device(t *testing.T, adjust func(settings *DeviceLocalSettings)) *DeviceLocal {
	t.Helper()
	return self.role.device(t, func(settings *DeviceLocalSettings) {
		settings.ForwardStateDir = self.stateDir
		settings.SetForwardKeyStore(self.store)
		settings.testingForwardingDeps = func(deps *deviceForwardingDeps) {
			deps.apiUrl = "https://api.forward.test"
			deps.platformUrl = "wss://platform.forward.test"
			deps.transport = self.transport
		}
		if adjust != nil {
			adjust(settings)
		}
	})
}

// The vectors' network's Pending, naming this test's identity.
func (self *testingForwardDevice) pending() *bridge.Pending {
	return self.identity.pending(self.role.vectors.networkId(), self.role.vectors.pending.GetGenesisSha256())
}

// keys then a hand-off for networkId through device's forwarding host, change altering the hand-off.
func (self *testingForwardDevice) setUp(t *testing.T, device *DeviceLocal, pending *bridge.Pending, change *testingForwardHandOffChange) error {
	t.Helper()
	networkId := connect.Id(pending.GetNetworkId())
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	return device.forwardingHost().handOff(self.identity.handOff(t, keys, pending, change))
}

// Waits until networkId's Forwarder has run its start, which anchors a legacy posture: a witness is answered only
// once its loop runs.
func testingForwardStarted(t *testing.T, device *DeviceLocal, networkId connect.Id) {
	t.Helper()
	if err := device.forwardingHost().witness(networkId, nil, false); err != nil {
		t.Fatalf("no forwarder answered: %v", err)
	}
}

// A hand-off that names other keys, another sidecar or another host, that another key signed, or that carries no token
// is refused, and nothing of it is kept: no token, no setup, no listing, and the posture stays legacy.
func TestForwardHandOffNamingOtherKeysRefused(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	otherReader := make([]byte, 1216)
	for name, change := range map[string]*testingForwardHandOffChange{
		"another forwarding key": {forwardPublicKey: otherKey.Public().(ed25519.PublicKey)},
		"another reader key":     {readerXwingPublicKey: otherReader},
		"forward names another":  {forwardSidecarId: connect.NewId().Bytes()},
		"another host client":    {hostClientId: connect.NewId().Bytes(), hostClientPublicKey: otherKey.Public().(ed25519.PublicKey)},
		"signed by another key":  {signer: otherKey},
		"signed by another id":   {signerId: connect.NewId().Bytes()},
	} {
		if err := forward.setUp(t, device, forward.pending(), change); !errors.Is(err, errDeviceForwardHandOffRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	otherSet := forward.identity.handOff(t, keys, forward.pending(), nil)
	otherSet.KeySetId = connect.NewId().Bytes()
	if err := device.forwardingHost().handOff(otherSet); !errors.Is(err, errDeviceForwardHandOffRefused) {
		t.Errorf("another key set: %v", err)
	}
	noToken := forward.identity.handOff(t, keys, forward.pending(), nil)
	noToken.ForwardClientJwt = ""
	if err := device.forwardingHost().handOff(noToken); !errors.Is(err, errDeviceForwardHandOffRefused) {
		t.Errorf("no token: %v", err)
	}
	role, _ := device.forwardingHost().role(networkId, false)
	if token, _ := role.keys.token(); token != "" {
		t.Error("a refused hand-off's token was kept")
	}
	if networkIds, _ := device.forwardingHost().networks.list(); len(networkIds) != 0 {
		t.Errorf("a refused hand-off listed %v", networkIds)
	}
	if state := device.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PostureLegacy {
		t.Errorf("a refused hand-off left the posture %v", state)
	}
}

// A hand-off accepted keeps F's token in the key store and the hand-off without it sealed, records the Delegation beside
// next, lists the network, and the Forwarder anchors the posture; a restart restores it pending from the sealed store
// before the first packet, and the key set is promoted only by a state that shows the Delegation live.
func TestForwardHandOffAnchorsThePostureAndSurvivesARestart(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	if state := device.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Fatalf("after the hand-off the posture is %v", state)
	}
	role, _ := device.forwardingHost().role(networkId, false)
	if token, _ := role.keys.token(); token == "" {
		t.Error("F's token was not kept")
	}
	setup, err := role.readSetup()
	if err != nil || setup == nil || setup.GetForwardClientJwt() != "" || len(setup.GetSignedDelegation()) == 0 {
		t.Errorf("the kept setup %v, %v", setup, err)
	}
	status := role.status()
	if status.KeySet != deviceForwardKeySetDelegated || !status.Hosting || status.Posture != sidecardevice.PosturePending {
		t.Errorf("status %+v", status)
	}
	if role.checkPromotion() {
		t.Error("the set was promoted with no state showing its Delegation")
	}
	if _, _, ok := role.keys.Delegation(); ok {
		t.Error("a set not yet promoted signs")
	}
	for _, path := range []string{"posture", "setup"} {
		matches, _ := filepath.Glob(filepath.Join(forward.stateDir, deviceForwardDirectoryName, "*", path+"*"))
		if len(matches) == 0 {
			t.Errorf("no %s record under the component's directory", path)
		}
	}
	if _, err := os.Stat(forward.role.postureDirectory()); !errors.Is(err, os.ErrNotExist) {
		t.Error("the posture went under the network space's storage, beside the app's")
	}
	testingJoinDeviceRole(t, device)

	restarted := forward.device(t, nil)
	if state := restarted.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Errorf("after a restart the posture is %v", state)
	}
	if restarted.sendRoute.Load().bridge == nil {
		t.Error("the restored posture is not in the first send route")
	}
}

// A sealed record with a flipped byte, or a key store that cannot answer, restores the posture pending, never legacy,
// and nothing is reset.
func TestForwardDamagedStoreRestoresPending(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	testingJoinDeviceRole(t, device)

	forward.store.setMode(testingForwardKeyStoreLocked)
	locked := forward.device(t, nil)
	if state := locked.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Errorf("with the key store locked the posture is %v", state)
	}
	testingJoinDeviceRole(t, locked)
	forward.store.setMode(testingForwardKeyStoreWorking)

	matches, _ := filepath.Glob(filepath.Join(forward.stateDir, deviceForwardDirectoryName, "*", "posture", "*"))
	if len(matches) == 0 {
		t.Fatal("CONTROL FAILED: no posture record to damage")
	}
	for _, path := range matches {
		onDisk, _ := os.ReadFile(path)
		onDisk[len(onDisk)-1] ^= 1
		os.WriteFile(path, onDisk, 0600)
	}
	damaged := forward.device(t, nil)
	if state := damaged.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PosturePending {
		t.Errorf("with a damaged record the posture is %v", state)
	}
}

// States move the posture through the sink and raise the floor; a copy of the store put back from below the floor is
// pending (store_rolled_back), never the older state.
func TestForwardRolledBackStoreRestoresPending(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	vectors := forward.role.vectors
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	role.stop()
	stateLog := func(from uint64, through uint64) *sidecar.BridgeStateLog {
		log := vectors.log(from, through)
		return &sidecar.BridgeStateLog{Entries: log.Entries, OpenRows: log.OpenRows}
	}
	if err := role.sink.SetState(vectors.state(t, 3, testingBridgeRules()), stateLog(1, 3)); err != nil {
		t.Fatalf("state 3: %v", err)
	}
	if decision := role.sink.Decision(); decision.State != sidecar.BridgeSinkEnforcing || decision.StatePin.GetVersion() != 3 {
		t.Fatalf("after state 3 the sink reads %+v", decision)
	}
	directory := filepath.Join(forward.stateDir, deviceForwardDirectoryName)
	copied := t.TempDir()
	if err := os.CopyFS(copied, os.DirFS(directory)); err != nil {
		t.Fatal(err)
	}
	if err := role.sink.SetState(vectors.state(t, 6, testingBridgeRules()), stateLog(1, 6)); err != nil {
		t.Fatalf("state 6: %v", err)
	}
	if n, _, _ := role.keys.floor().Load(); n != 6 {
		t.Errorf("the floor stands at %d after state 6", n)
	}
	testingJoinDeviceRole(t, device)
	os.RemoveAll(directory)
	if err := os.CopyFS(directory, os.DirFS(copied)); err != nil {
		t.Fatal(err)
	}
	restored := forward.device(t, nil)
	decision := restored.bridgeOwner.roleDecision(networkId)
	if decision.State != sidecardevice.PosturePending {
		t.Errorf("a store put back below the floor restored %v", decision.State)
	}
}

// A token past its exp at start holds the posture pending, a state notwithstanding, until a new hand-off.
func TestForwardLapsedClientHoldsPending(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	vectors := forward.role.vectors
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	handOff := forward.identity.handOff(t, keys, forward.pending(), nil)
	handOff.ForwardClientJwt = testingForwardJwt(t, 1000, 60)
	if err := device.forwardingHost().handOff(handOff); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	if !role.status().Lapsed {
		t.Error("a token past its exp did not lapse")
	}
	log := vectors.log(1, 3)
	role.sink.SetState(vectors.state(t, 3, testingBridgeRules()), &sidecar.BridgeStateLog{Entries: log.Entries, OpenRows: log.OpenRows})
	if decision := role.sink.Decision(); decision.State == sidecar.BridgeSinkEnforcing {
		t.Errorf("a lapsed forwarding client's posture enforces: %+v", decision)
	}
	// a witness that clears a fork does not clear the lapse
	role.sink.SetFork(false)
	if decision := role.sink.Decision(); decision.State == sidecar.BridgeSinkEnforcing {
		t.Error("a witness lifted the lapse's hold")
	}
}

// Two networks forward at once, each on its own posture: the other network's role has its own bridge, the send route
// holds a router whose hook is the device client's network's alone, so nothing is hosted outside it, and a packet goes
// to the role whose routes hold it.
func TestForwardTwoNetworksEachOnItsOwnPosture(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	vectors := forward.role.vectors
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	log := vectors.log(1, 3)
	if err := role.sink.SetState(vectors.state(t, 3, testingBridgeRules()), &sidecar.BridgeStateLog{Entries: log.Entries, OpenRows: log.OpenRows}); err != nil {
		t.Fatal(err)
	}

	otherNetworkId := connect.NewId()
	otherGenesis := make([]byte, 32)
	rand.Read(otherGenesis)
	otherPending := forward.identity.pending(otherNetworkId, otherGenesis)
	if err := forward.setUp(t, device, otherPending, nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, otherNetworkId)
	if state := device.bridgeOwner.roleDecision(otherNetworkId).State; state != sidecardevice.PosturePending {
		t.Errorf("the other network's posture is %v", state)
	}
	if state := device.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PostureEnforcingDocument {
		t.Errorf("the device client's network's posture moved to %v", state)
	}
	router, ok := device.sendRoute.Load().bridge.(*deviceBridgeRouter)
	if !ok {
		t.Fatalf("the send route holds %T, not a router over both roles", device.sendRoute.Load().bridge)
	}
	if router.primary == nil || router.EgressHook() != router.primary.EgressHook() {
		t.Error("the router's hook is not the device client's network's")
	}
	for _, role := range router.roles {
		if !role.primary && role.networkId != otherNetworkId {
			t.Errorf("a role of %s in the router", role.networkId)
		}
	}
	inBlock := testingDeviceRolePacket(t, testingDeviceRoleUnassigned, 17, 9, nil)
	if target := router.target(inBlock); target != router.primary {
		t.Error("a packet in the device client's block went elsewhere")
	}
	// with no local addresses set, the primary exports its per-peer /32s, its resolver among them (D-BRIDGE-12)
	if routes := router.Routes(); len(routes) == 0 || !routes[0].Contains(testingDeviceRoleResolver) {
		t.Errorf("the router routes %v", routes)
	}
	if target := router.target(testingDeviceRolePacket(t, testingDeviceRoleResolver, 17, 53, nil)); target != router.primary {
		t.Error("the resolver's packet went elsewhere")
	}
	statuses := device.forwardingHost().status()
	if len(statuses) != 2 || statuses[0].NetworkId != networkId || !statuses[0].Hosting || statuses[1].Hosting {
		t.Errorf("statuses %+v", statuses)
	}
}

// The sink reads a posture's decision as the Forwarder needs it.
func TestForwardSinkDecisionMapping(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if decision := deviceForwardSinkDecisionOf(sidecardevice.PostureDecision{State: sidecardevice.PostureLegacy}, time.Minute); decision.State != sidecar.BridgeSinkLegacy {
		t.Errorf("legacy read %v", decision.State)
	}
	enforcing := sidecardevice.PostureDecision{
		State:       sidecardevice.PostureEnforcingDocument,
		Pin:         &bridge.AuthPin{Version: 7},
		BridgeState: &bridge.BridgeState{Auth: &bridge.AuthPin{Version: 5}},
		Waiting:     []sidecardevice.PostureWaitingEntry{{N: 6, ThroughN: 6, FirstSeen: now}},
	}
	decision := deviceForwardSinkDecisionOf(enforcing, 10*time.Minute)
	if decision.State != sidecar.BridgeSinkEnforcing || decision.Pin.GetVersion() != 7 || decision.StatePin.GetVersion() != 5 ||
		decision.WaitingCount != 1 || !decision.NextAlarmAt.Equal(now.Add(10*time.Minute)) {
		t.Errorf("enforcing read %+v", decision)
	}
	forked := enforcing
	forked.Fork = true
	if decision := deviceForwardSinkDecisionOf(forked, time.Minute); decision.State != sidecar.BridgeSinkPending || !decision.Fork {
		t.Errorf("a fork read %+v", decision)
	}
	full := enforcing
	full.OverlayFullThroughN = 9
	if decision := deviceForwardSinkDecisionOf(full, time.Minute); decision.State != sidecar.BridgeSinkPending {
		t.Errorf("a full overlay read %v", decision.State)
	}
	overdue := enforcing
	overdue.Overdue = &sidecardevice.PostureOverdue{N: 6, Age: 11 * time.Minute, NextAlarmAt: now.Add(time.Hour)}
	overdue.Alarms = []sidecardevice.PostureAlarm{{Kind: sidecardevice.PostureAlarmCompileOverdue, N: 6}}
	if decision := deviceForwardSinkDecisionOf(overdue, time.Minute); decision.Overdue == nil || !decision.Overdue.Raised ||
		decision.Overdue.N != 6 || !decision.NextAlarmAt.Equal(now.Add(time.Hour)) {
		t.Errorf("overdue read %+v", decision)
	}
}

// F refreshes at a point between 40% and 50% of its lifetime, the same point for one token, and a token that does not
// read has none.
func TestForwardRefreshPointInWindow(t *testing.T) {
	settings := defaultDeviceForwardingSettings()
	lifetime := int64(7 * 24 * 60 * 60)
	for iat := int64(1000); iat < 1010; iat += 1 {
		byJwt := testingForwardJwt(t, iat, lifetime)
		at, ok := deviceForwardRefreshAt(byJwt, settings)
		if !ok {
			t.Fatal("a token's point did not read")
		}
		fraction := float64(at.Unix()-iat) / float64(lifetime)
		if fraction < 0.4 || 0.5 < fraction {
			t.Errorf("a refresh at %.3f of the lifetime", fraction)
		}
		if again, _ := deviceForwardRefreshAt(byJwt, settings); !again.Equal(at) {
			t.Error("one token took two points")
		}
	}
	if _, ok := deviceForwardRefreshAt("not.a.token", settings); ok {
		t.Error("a token that does not read has a point")
	}
	if expiry := deviceForwardTokenExpiry(testingForwardJwt(t, 10, 20)); expiry.Unix() != 30 {
		t.Errorf("exp read %v", expiry)
	}
	if deviceForwardBackoff(time.Second, time.Minute, 1) != time.Second || deviceForwardBackoff(time.Second, time.Minute, 10) != time.Minute {
		t.Error("the backoff does not double to its cap")
	}
}

// What a forwarding role answers out of the package is public: no hand-off's token reaches a status or an error.
func TestForwardStatusAndErrorsHoldNoToken(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	handOff := forward.identity.handOff(t, keys, forward.pending(), &testingForwardHandOffChange{signerId: connect.NewId().Bytes()})
	err = device.forwardingHost().handOff(handOff)
	if err == nil || strings.Contains(err.Error(), handOff.GetForwardClientJwt()) {
		t.Errorf("a refusal named the token: %v", err)
	}
	encoded, _ := proto.Marshal(keys)
	if bytes.Contains(encoded, []byte(handOff.GetForwardClientJwt())) {
		t.Error("the keys carry a token")
	}
}
