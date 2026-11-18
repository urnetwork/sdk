//go:build !js

package sdk

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// the role's clock, nil for real time
	clock sidecar.Clock
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
			deps.clock = self.clock
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

// The sink reports a posture's overdue raise once: a later read finding the same raise among the alarms is no new
// raise, and a raise at the next point, or naming a further entry, is (QD-696; BRIDGE-M8-07's app host schedules
// found the Forwarder raising one point's bridge_alarm at every later read).
func TestForwardSinkReportsEachOverdueRaiseOnce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	decisionAt := func(n uint64, age time.Duration) sidecardevice.PostureDecision {
		return sidecardevice.PostureDecision{
			State:       sidecardevice.PostureEnforcingDocument,
			BridgeState: &bridge.BridgeState{Auth: &bridge.AuthPin{Version: 5}},
			Waiting:     []sidecardevice.PostureWaitingEntry{{N: 6, ThroughN: 6, FirstSeen: now}, {N: 7, ThroughN: 7, FirstSeen: now.Add(time.Minute)}},
			Overdue:     &sidecardevice.PostureOverdue{N: 6, Age: age, NextAlarmAt: now.Add(time.Hour)},
			Alarms:      []sidecardevice.PostureAlarm{{Kind: sidecardevice.PostureAlarmCompileOverdue, N: n, Age: age}},
		}
	}
	raises := &deviceForwardRaises{}
	for i, step := range []struct {
		decision sidecardevice.PostureDecision
		raised   bool
	}{
		{decisionAt(6, 10*time.Minute), true},
		{decisionAt(6, 10*time.Minute), false},
		{decisionAt(6, 10*time.Minute), false},
		{decisionAt(7, 10*time.Minute), true},
		{decisionAt(6, 30*time.Minute), true},
		{decisionAt(6, 30*time.Minute), false},
	} {
		sink := deviceForwardSinkDecisionOf(step.decision, 10*time.Minute)
		raises.once(step.decision, &sink)
		if sink.Overdue == nil || sink.Overdue.Raised != step.raised {
			t.Fatalf("read %d: overdue %+v, want raised %t", i, sink.Overdue, step.raised)
		}
	}
	// a decision with no raise among its alarms reports none, and leaves the last raise as it was
	quiet := decisionAt(6, 30*time.Minute)
	quiet.Alarms = nil
	sink := deviceForwardSinkDecisionOf(quiet, 10*time.Minute)
	raises.once(quiet, &sink)
	if sink.Overdue == nil || sink.Overdue.Raised {
		t.Fatalf("a read with no raise: %+v", sink.Overdue)
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

// Deltas a legacy posture cannot take wipe nothing: a role whose anchor never took keeps its keys and store.
func TestForwardLegacyDeltasWipeNothing(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	if _, err := device.forwardingHost().keys(networkId); err != nil {
		t.Fatal(err)
	}
	role, _ := device.forwardingHost().role(networkId, false)
	log := forward.role.vectors.log(1, 1)
	if err := role.sink.ApplyDeltas(&sidecar.BridgeStateLog{Entries: log.Entries, OpenRows: log.OpenRows}); !errors.Is(err, errDeviceBridgeLegacy) {
		t.Errorf("a legacy posture's deltas answered %v", err)
	}
	if _, status := role.keys.loadSet(deviceForwardSlotNext); status != deviceForwardSecretPresent {
		t.Error("the generated key set went with a legacy posture's deltas")
	}
	if rootKey, err := role.keys.rootKey(false); err != nil || len(rootKey) != 32 {
		t.Errorf("the root key went: %v", err)
	}
}

// A role's Forwarder reads each base's config head through F's head reader, whose keys are the generations that
// Forwarder keeps (SDK-M8-16; QD-814; QD-839, QD-973): on every leg, the Apple extensions' among them, the role holds
// one, an anchoring reader the Forwarder reads at the state at each base.
func TestForwardRoleReadsHeadsThroughF(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	role.stateLock.Lock()
	heads := role.heads
	role.stateLock.Unlock()
	if _, anchors := heads.(sidecar.ConfigHeadAnchorReader); heads == nil || !anchors {
		t.Fatalf("the role's Forwarder reads heads through %T", heads)
	}
}

// A head_alarm the role's Forwarder raises reaches the status mirror, the newest with its two pins' positions, and
// tells the status listeners (§18.6's witness row): a witness past what the component holds is withheld.
func TestForwardHeadAlarmReachesTheStatusMirror(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	changed := make(chan *BridgeStatus, 64)
	sub := device.AddBridgeStatusChangeListener(&testingBridgeStatusListener{changed: changed})
	defer sub.Close()
	if err := device.forwardingHost().witness(networkId, &bridge.AuthPin{Version: 9, Chain: make([]byte, 32)}, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(testSidecarTimeout)
	for {
		select {
		case status := <-changed:
			if status.Networks == nil || status.Networks.Len() != 1 || status.Networks.Get(0).HeadAlarm == "" {
				continue
			}
			network := status.Networks.Get(0)
			if network.HeadAlarm != string(sidecar.SidecarHeadAlarmWithheld) || network.HeadAlarmOtherN != 9 || network.HeadAlarmN != 0 || network.HeadAlarmMillis == 0 {
				t.Errorf("the status's head alarm %+v", network)
			}
			mirrored := newDeviceRemoteBridgeStatus(status).toBridgeStatus().Networks.Get(0)
			if mirrored.HeadAlarm != network.HeadAlarm || mirrored.HeadAlarmOtherN != network.HeadAlarmOtherN {
				t.Errorf("the RPC's copy carries %+v", mirrored)
			}
			return
		case <-deadline:
			t.Fatal("no status carried the head alarm")
		}
	}
}

// A clock a test moves: each After and AfterFunc fires once Advance reaches its time, never on its own.
type testingForwardClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*testingForwardTimer
}

type testingForwardTimer struct {
	at       time.Time
	fired    chan time.Time
	function func()
}

func newTestingForwardClock(now time.Time) *testingForwardClock {
	return &testingForwardClock{now: now}
}

func (self *testingForwardClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testingForwardClock) After(timeout time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if timeout <= 0 {
		fired <- self.now
		return fired
	}
	self.timers = append(self.timers, &testingForwardTimer{at: self.now.Add(timeout), fired: fired})
	return fired
}

func (self *testingForwardClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	timer := &testingForwardTimer{at: self.now.Add(timeout), function: function}
	self.timers = append(self.timers, timer)
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		held := slices.Contains(self.timers, timer)
		self.timers = slices.DeleteFunc(self.timers, func(other *testingForwardTimer) bool { return other == timer })
		return held
	}
}

// Moves the clock by by and fires every timer it reaches.
func (self *testingForwardClock) Advance(by time.Duration) {
	self.stateLock.Lock()
	self.now = self.now.Add(by)
	now := self.now
	due := []*testingForwardTimer{}
	self.timers = slices.DeleteFunc(self.timers, func(timer *testingForwardTimer) bool {
		if timer.at.After(now) {
			return false
		}
		due = append(due, timer)
		return true
	})
	self.stateLock.Unlock()
	for _, timer := range due {
		if timer.fired != nil {
			timer.fired <- now
		} else {
			go timer.function()
		}
	}
}

// api as F's refresh reaches it: each GET /auth/refresh is handed to the test, which answers it with a token, an
// empty one answering 503; every other request is 503.
type testingForwardRefreshTransport struct {
	requests chan *testingForwardRefresh
}

type testingForwardRefresh struct {
	request *http.Request
	answer  chan string
}

func (self *testingForwardRefreshTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/auth/refresh") {
		return testingForwardTransport{}.RoundTrip(request)
	}
	refresh := &testingForwardRefresh{request: request, answer: make(chan string, 1)}
	select {
	case self.requests <- refresh:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	var byJwt string
	select {
	case byJwt = <-refresh.answer:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	if byJwt == "" {
		return testingForwardTransport{}.RoundTrip(request)
	}
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"by_jwt":"` + byJwt + `"}`)),
		Request:    request,
	}, nil
}

// The next refresh F asks, within the test's bound.
func (self *testingForwardRefreshTransport) next(t *testing.T) *testingForwardRefresh {
	t.Helper()
	select {
	case refresh := <-self.requests:
		return refresh
	case <-time.After(testSidecarTimeout):
		t.Fatal("F asked for no refresh")
		return nil
	}
}

// The jwk x of a DPoP proof's header, empty for one that does not read.
func testingForwardProofKey(proof string) string {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return ""
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	header := struct {
		Jwk struct {
			X string `json:"x"`
		} `json:"jwk"`
	}{}
	if json.Unmarshal(headerBytes, &header) != nil {
		return ""
	}
	return header.Jwk.X
}

// F refreshes itself at its window's point by proofs of its own ClientKey, which the component generated, keeps the
// new token before it presents it, and presents it at the next refresh, all on the role's clock with no app running.
func TestForwardClientRefreshesByItsOwnProofs(t *testing.T) {
	forward := newTestingForwardDevice(t)
	start := time.Unix(1800000000, 0)
	clock := newTestingForwardClock(start)
	transport := &testingForwardRefreshTransport{requests: make(chan *testingForwardRefresh)}
	forward.clock, forward.transport = clock, transport
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	lifetime := int64(7 * 24 * 60 * 60)
	first := testingForwardJwt(t, start.Unix(), lifetime)
	handOff := forward.identity.handOff(t, keys, forward.pending(), nil)
	handOff.ForwardClientJwt = first
	if err := device.forwardingHost().handOff(handOff); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)

	// nothing before 40% of the lifetime; by 50% the refresh is asked
	clock.Advance(time.Duration(lifetime) * time.Second / 2)
	refresh := transport.next(t)
	if bearer := refresh.request.Header.Get("Authorization"); bearer != "Bearer "+first {
		t.Errorf("the refresh presented %q", bearer)
	}
	if proofKey := testingForwardProofKey(refresh.request.Header.Get("DPoP")); proofKey != base64.RawURLEncoding.EncodeToString(keys.GetClientPublicKey()) {
		t.Errorf("the refresh's proof is by %q, not F's own ClientKey", proofKey)
	}
	second := testingForwardJwt(t, start.Unix()+lifetime/2, lifetime)
	refresh.answer <- second

	// the next refresh, at the new token's point, presents the new token, which was kept first
	clock.Advance(time.Duration(lifetime) * time.Second / 2)
	refresh = transport.next(t)
	if bearer := refresh.request.Header.Get("Authorization"); bearer != "Bearer "+second {
		t.Errorf("the next refresh presented %q", bearer)
	}
	if kept, _ := role.keys.token(); kept != second {
		t.Error("the new token was presented before it was kept")
	}
	if status := role.status(); status.Lapsed || status.ForwardClientExpiresAt.Unix() != start.Unix()+lifetime/2+lifetime {
		t.Errorf("status %+v", status)
	}
	refresh.answer <- testingForwardJwt(t, start.Unix()+lifetime, lifetime)
}

// F left unrefreshed past its exp lapses with no grace: its refreshes failing, the role holds its posture pending.
func TestForwardClientLapsesUnrefreshed(t *testing.T) {
	forward := newTestingForwardDevice(t)
	start := time.Unix(1800000000, 0)
	clock := newTestingForwardClock(start)
	transport := &testingForwardRefreshTransport{requests: make(chan *testingForwardRefresh)}
	forward.clock, forward.transport = clock, transport
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	lifetime := int64(7 * 24 * 60 * 60)
	handOff := forward.identity.handOff(t, keys, forward.pending(), nil)
	handOff.ForwardClientJwt = testingForwardJwt(t, start.Unix(), lifetime)
	if err := device.forwardingHost().handOff(handOff); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	role, _ := device.forwardingHost().role(networkId, false)
	changed := make(chan *BridgeStatus, 64)
	sub := device.AddBridgeStatusChangeListener(&testingBridgeStatusListener{changed: changed})
	defer sub.Close()
	clock.Advance(time.Duration(lifetime) * time.Second / 2)
	refresh := transport.next(t)
	// the clock passes the exp while the refresh is in flight, which then fails: the role lapses, and the status says
	// so
	clock.Advance(time.Duration(lifetime) * time.Second)
	if role.status().Lapsed {
		t.Fatal("the role lapsed before a refresh failed")
	}
	refresh.answer <- ""
	deadline := time.After(testSidecarTimeout)
	for lapsed := false; !lapsed; {
		select {
		case status := <-changed:
			lapsed = status.Networks != nil && status.Networks.Len() == 1 && status.Networks.Get(0).Lapsed
		case <-deadline:
			t.Fatal("no status carried the lapse")
		}
	}
	if decision := role.sink.Decision(); decision.State == sidecar.BridgeSinkEnforcing || !decision.Fork {
		t.Errorf("a lapsed role's decision %+v", decision)
	}
}

// The sidecar's own sign-out, a SidecarRevoke Pending's identity key verifies, moves the role's posture from past legacy
// back to legacy, and the role is wiped (4.2.6's wiped): its Forwarder stops, its key store items, sealed files and
// listing go, and the host holds the role no more.
func TestForwardSignOutWipesTheRole(t *testing.T) {
	forward := newTestingForwardDevice(t)
	device := forward.device(t, nil)
	networkId := forward.role.vectors.networkId()
	vectors := forward.role.vectors
	if err := forward.setUp(t, device, forward.pending(), nil); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	host := device.forwardingHost()
	role, _ := host.role(networkId, false)
	// the vectors' sidecar's Pending, on the same network and anchor, replaces the hand-off's id and key
	if err := role.sink.SetPending(vectors.pending); err != nil {
		t.Fatal(err)
	}
	log := vectors.log(1, 3)
	if err := role.sink.SetState(vectors.state(t, 3, testingBridgeRules()), &sidecar.BridgeStateLog{Entries: log.Entries, OpenRows: log.OpenRows}); err != nil {
		t.Fatal(err)
	}
	if state := device.bridgeOwner.roleDecision(networkId).State; !state.Enforcing() {
		t.Fatalf("the role's posture is %v before the sign-out", state)
	}
	forward.store.stateLock.Lock()
	heldBefore := len(forward.store.items)
	forward.store.stateLock.Unlock()
	if heldBefore == 0 {
		t.Fatal("the key store holds nothing of the role before the sign-out")
	}
	deltas := vectors.log(4, 7)
	if err := role.sink.ApplyDeltas(&sidecar.BridgeStateLog{Entries: deltas.Entries, OpenRows: deltas.OpenRows}); err != nil {
		t.Fatalf("the deltas to the sign-out: %v", err)
	}
	host.wipes.Wait()
	if state := device.bridgeOwner.roleDecision(networkId).State; state != sidecardevice.PostureLegacy {
		t.Errorf("the role's posture is %v after its sidecar's sign-out", state)
	}
	if role.currentForwarder() != nil {
		t.Error("the wiped role's Forwarder still runs")
	}
	forward.store.stateLock.Lock()
	items := slices.Collect(maps.Keys(forward.store.items))
	forward.store.stateLock.Unlock()
	if len(items) != 0 {
		t.Errorf("the key store holds %v after the wipe", items)
	}
	if _, err := os.Stat(role.files.directoryPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the role's sealed files remain: %v", err)
	}
	if networkIds, _ := host.networks.list(); slices.Contains(networkIds, networkId) {
		t.Error("the wiped network is still listed")
	}
	if held, _ := host.role(networkId, false); held != nil {
		t.Error("the host still holds the wiped role")
	}
}
