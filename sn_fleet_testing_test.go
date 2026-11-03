//go:build !ios_extension && !js

package sdk

// What the Top 200 claim tests share: real keys and real messages, with a fake
// server, chain and stores, so nothing here depends on the network or the clock.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/protocol"
)

const (
	testingFleetNetworkId = "019b2e10-0000-4d3e-8f01-000000000001"
	testingFleetClientId  = "019b2e10-0000-4d3e-8f01-0000000000c1"
)

var (
	testingFleetColdkeySeed      = [32]byte{0xc0, 1}
	testingFleetOtherColdkeySeed = [32]byte{0xc0, 2}
	testingFleetHotkeySeed       = [32]byte{0xb0, 1}
	testingFleetOtherHotkeySeed  = [32]byte{0xb0, 2}
	testingFleetClientSeed       = func() []byte {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = 0xa0
		return seed
	}()
)

func testingFleetId(t testing.TB, value string) connect.Id {
	t.Helper()
	id, err := connect.ParseId(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testingFleetPublic(t testing.TB, seed [32]byte) [32]byte {
	t.Helper()
	public, err := protocol.FleetHotkeyPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

// What a wallet signs: the text wrapped in <Bytes>, sr25519, the substrate context.
func testingFleetWalletSign(t testing.TB, seed [32]byte, message string) string {
	t.Helper()
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := mini.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte("<Bytes>"+message+"</Bytes>")))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	return snFleetHex(encoded[:])
}

// A signed consent of the network, as the server hands it over.
func testingFleetConsent(t testing.TB, coldkeySeed [32]byte, networkId string, generation uint64) *snFleetSignedMessage {
	t.Helper()
	statement := snNetworkConsentStatement{
		Schema:     snNetworkConsentSchema,
		Scope:      snNetworkConsentScope,
		Domain:     json.RawMessage(`{"d":1}`),
		UserId:     [16]byte{1},
		NetworkId:  [16]byte(testingFleetId(t, networkId)),
		Coldkey:    testingFleetPublic(t, coldkeySeed),
		Generation: generation,
		Nonce:      [32]byte{1},
		IssuedAt:   1000,
		ExpiresAt:  1300,
		FromEpoch:  5, ThroughEpoch: 65540,
		Prospective: json.RawMessage(`{"p":1}`),
	}
	raw, err := json.Marshal(&statement)
	if err != nil {
		t.Fatal(err)
	}
	message := snNetworkConsentMessagePrefix + string(raw)
	return &snFleetSignedMessage{Message: message, Signature: testingFleetWalletSign(t, coldkeySeed, message)}
}

func testingFleetCoordinator() [20]byte {
	var coordinator [20]byte
	copy(coordinator[:], []byte{0xc1, 0x89, 0x25, 0x92, 0x5e, 0x2b, 0x7b, 0xb9, 0x05, 0x9b, 0x7d, 0x69, 0x6b, 0x8c, 0x92, 0x76, 0x2a, 0xe8, 0x64, 0x06})
	return coordinator
}

func testingFleetCoordinatorHex() string {
	coordinator := testingFleetCoordinator()
	return snFleetHex(coordinator[:])
}

// The coldkey's authorization of a hotkey.
func testingFleetAuthorizationMessage(t testing.TB, coldkeySeed [32]byte, hotkey [32]byte, networkId string) string {
	t.Helper()
	message, err := protocol.FleetHotkeyAuthorization{
		ChainID:     964,
		Netuid:      25,
		Coordinator: testingFleetCoordinator(),
		NetworkID:   [16]byte(testingFleetId(t, networkId)),
		Coldkey:     testingFleetPublic(t, coldkeySeed),
		Hotkey:      hotkey,
	}.Message()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func testingFleetAuthorization(t testing.TB, coldkeySeed [32]byte, hotkey [32]byte, networkId string) *snFleetSignedMessage {
	t.Helper()
	message := testingFleetAuthorizationMessage(t, coldkeySeed, hotkey, networkId)
	return &snFleetSignedMessage{Message: message, Signature: testingFleetWalletSign(t, coldkeySeed, message)}
}

func testingFleetClientKey() [32]byte {
	return [32]byte(ed25519.NewKeyFromSeed(testingFleetClientSeed).Public().(ed25519.PublicKey))
}

// A manifest of this device, in a generation.
func testingFleetManifest(t testing.TB, hotkey [32]byte, generation uint64, members ...protocol.FleetMember) *protocol.FleetManifest {
	t.Helper()
	if len(members) == 0 {
		members = []protocol.FleetMember{{ClientID: [16]byte(testingFleetId(t, testingFleetClientId)), ClientKey: testingFleetClientKey()}}
	}
	return &protocol.FleetManifest{
		Schema:      protocol.FleetManifestSchema,
		ChainID:     964,
		Netuid:      25,
		Coordinator: testingFleetCoordinator(),
		FleetID:     [32]byte{0xf1},
		Hotkey:      hotkey,
		Generation:  generation,
		Members:     members,
	}
}

// The wire form of a binding, as the server writes it.
func testingFleetBindingJson(t testing.TB, binding protocol.FleetBinding) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"chain_id":         binding.ChainID,
		"netuid":           binding.Netuid,
		"coordinator":      "0x" + hex.EncodeToString(binding.Coordinator[:]),
		"fleet_id":         "0x" + hex.EncodeToString(binding.FleetID[:]),
		"hotkey":           "0x" + hex.EncodeToString(binding.Hotkey[:]),
		"client_id":        connect.Id(binding.ClientID).String(),
		"client_key":       "0x" + hex.EncodeToString(binding.ClientKey[:]),
		"generation":       binding.Generation,
		"valid_from_epoch": binding.ValidFromEpoch,
		"valid_to_epoch":   binding.ValidToEpoch,
		"commitment_hash":  "0x" + hex.EncodeToString(binding.CommitmentHash[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Work offering this device a binding of a hotkey in a generation.
func testingFleetWork(t testing.TB, coldkeySeed [32]byte, hotkeySeed [32]byte, generation uint64) *snFleetDeviceWork {
	t.Helper()
	hotkey := testingFleetPublic(t, hotkeySeed)
	manifest := testingFleetManifest(t, hotkey, generation)
	binding, err := manifest.Binding(manifest.Members[0], 5*generation, 5*generation+11)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := protocol.SignFleetBindingWithHotkey(hotkeySeed, binding)
	if err != nil {
		t.Fatal(err)
	}
	return &snFleetDeviceWork{
		BindingOpen:      true,
		NextCheckSeconds: 3600,
		Authorization:    testingFleetAuthorization(t, coldkeySeed, hotkey, testingFleetNetworkId),
		BindingJson:      testingFleetBindingJson(t, binding),
		HotkeySignature:  snFleetHex(signature),
	}
}

// A store that remembers one text, and can be told to fail.
type testingFleetMemoryStore struct {
	lock   sync.Mutex
	saved  string
	saves  int
	refuse bool
	// what a failed read answers
	unreadable string
}

func (self *testingFleetMemoryStore) LoadSnFleetDeviceState() string {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.unreadable != "" {
		return self.unreadable
	}
	return self.saved
}

func (self *testingFleetMemoryStore) SaveSnFleetDeviceState(stateJson string) bool {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.refuse {
		return false
	}
	self.saved = stateJson
	self.saves += 1
	return true
}

func (self *testingFleetMemoryStore) state(t testing.TB) *snFleetDeviceState {
	t.Helper()
	self.lock.Lock()
	defer self.lock.Unlock()
	state := &snFleetDeviceState{}
	if self.saved != "" {
		if err := json.Unmarshal([]byte(self.saved), state); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// The server, as far as the flows can tell.
type testingFleetServer struct {
	lock sync.Mutex

	network string

	work       *snFleetDeviceWork
	workErr    error
	workAsked  []bool
	signatures []*snFleetDeviceSignatureArgs
	signErr    error
	// the order of everything that happened, for ordering checks
	events *[]string
	store  *testingFleetMemoryStore

	binding *snFleetDeviceBinding
	leaves  []*snFleetDeviceLeaveArgs
	leave   func(args *snFleetDeviceLeaveArgs) *snFleetDeviceLeaveResult

	status          *snFleetClaimStatus
	statusErr       error
	authorizations  []*snFleetAuthorizationArgs
	authorizeResult *snFleetAuthorizationResult
	hotkeySigned    []*snFleetHotkeySignaturesArgs
	topUps          int
	topUpResult     *snFleetTopUpResult
}

func (self *testingFleetServer) snFleetByJwt() string {
	network := self.network
	if network == "" {
		network = testingFleetNetworkId
	}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{"network_id": network, "client_id": testingFleetClientId}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		panic(err)
	}
	return token
}

func (self *testingFleetServer) snFleetDeviceWork(_ context.Context, needConsent bool) (*snFleetDeviceWork, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.workAsked = append(self.workAsked, needConsent)
	if self.workErr != nil {
		return nil, self.workErr
	}
	if self.work == nil {
		return &snFleetDeviceWork{NextCheckSeconds: 21600}, nil
	}
	work := *self.work
	return &work, nil
}

func (self *testingFleetServer) snFleetDeviceSignature(_ context.Context, args *snFleetDeviceSignatureArgs) (*snFleetDeviceSignatureResult, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.store != nil && args.ClientSignature != "" {
		// a device saves its signature before it leaves
		if state := self.store.state(testingFleetPanicT{}); len(state.Signed) == 0 || state.Signed[len(state.Signed)-1].ClientSignature != args.ClientSignature {
			panic("the signature left before it was saved")
		}
	}
	self.signatures = append(self.signatures, args)
	if self.signErr != nil {
		return nil, self.signErr
	}
	return &snFleetDeviceSignatureResult{Accepted: true}, nil
}

func (self *testingFleetServer) snFleetDeviceBinding(context.Context) (*snFleetDeviceBinding, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.binding == nil {
		return &snFleetDeviceBinding{}, nil
	}
	binding := *self.binding
	return &binding, nil
}

func (self *testingFleetServer) snFleetDeviceLeave(_ context.Context, args *snFleetDeviceLeaveArgs) (*snFleetDeviceLeaveResult, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.leaves = append(self.leaves, args)
	if self.leave != nil {
		return self.leave(args), nil
	}
	return &snFleetDeviceLeaveResult{Left: true, EffectiveEpoch: args.EffectiveEpoch}, nil
}

func (self *testingFleetServer) snFleetClaimStatus(context.Context) (*snFleetClaimStatus, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.statusErr != nil {
		return nil, self.statusErr
	}
	if self.status == nil {
		return nil, errors.New("no status")
	}
	status := *self.status
	return &status, nil
}

func (self *testingFleetServer) snFleetAuthorize(_ context.Context, args *snFleetAuthorizationArgs) (*snFleetAuthorizationResult, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.authorizations = append(self.authorizations, args)
	if self.authorizeResult != nil {
		return self.authorizeResult, nil
	}
	return &snFleetAuthorizationResult{Accepted: true}, nil
}

func (self *testingFleetServer) snFleetHotkeySignatures(_ context.Context, args *snFleetHotkeySignaturesArgs) (*snFleetHotkeySignaturesResult, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.hotkeySigned = append(self.hotkeySigned, args)
	return &snFleetHotkeySignaturesResult{Accepted: int64(len(args.Signatures))}, nil
}

func (self *testingFleetServer) snFleetTopUp(context.Context) (*snFleetTopUpResult, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.topUps += 1
	if self.topUpResult != nil {
		return self.topUpResult, nil
	}
	return &snFleetTopUpResult{AmountRao: 2_000_000}, nil
}

// A testing.TB for a check on a path that has no t: any failure is a panic.
type testingFleetPanicT struct {
	testing.TB
}

func (testingFleetPanicT) Helper() {}

func (testingFleetPanicT) Fatal(args ...any) {
	panic(args)
}

// A key holder that counts every signature, with the real checks of a device
// around it.
type testingFleetSigner struct {
	inner    *snFleetSeedSigner
	bindings int
	revokes  int
	failSign error
}

func newTestingFleetSigner(t testing.TB) *testingFleetSigner {
	t.Helper()
	seed := testingFleetClientSeed
	return &testingFleetSigner{inner: &snFleetSeedSigner{snDevice: &snDevice{clientId: testingFleetId(t, testingFleetClientId), clientKeySeed: func() []byte { return seed }}}}
}

func (self *testingFleetSigner) GetSnClientKey() string {
	return self.inner.GetSnClientKey()
}

func (self *testingFleetSigner) SignSnFleetBinding(bindingJson string) (string, error) {
	if self.failSign != nil {
		return "", self.failSign
	}
	self.bindings += 1
	return self.inner.SignSnFleetBinding(bindingJson)
}

func (self *testingFleetSigner) SignSnFleetRevoke(revokeJson string) (string, error) {
	self.revokes += 1
	return self.inner.SignSnFleetRevoke(revokeJson)
}

func newTestingFleetDevice(t testing.TB, server *testingFleetServer, signer *testingFleetSigner, store *testingFleetMemoryStore) *SnFleetDevice {
	t.Helper()
	device := newSnFleetDevice(server, signer, testingFleetId(t, testingFleetClientId), store)
	device.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	// the middle of every jitter window, so delays are exact
	device.random = func(n int64) int64 { return n / 2 }
	return device
}
