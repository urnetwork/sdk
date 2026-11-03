//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/protocol"
	"github.com/urnetwork/sdk/sn/ss58"
)

// What an app keeps: the hotkey seed and, for a key on this device, the phrase.
type testingFleetClaimStore struct {
	seed      string
	phrase    string
	saves     int
	refuse    bool
	readsBack bool
	// what a failed read answers
	unreadable string
}

func (self *testingFleetClaimStore) LoadSnFleetHotkeySeed(string) string {
	if self.unreadable != "" {
		return self.unreadable
	}
	return self.seed
}

func (self *testingFleetClaimStore) SaveSnFleetHotkeySeed(_ string, seedHex string) bool {
	if self.refuse {
		return false
	}
	self.saves += 1
	self.seed = seedHex
	if self.readsBack {
		self.seed = "00" + seedHex[2:]
	}
	return true
}

func (self *testingFleetClaimStore) LoadSnFleetColdkeyPhrase(string) string {
	return self.phrase
}

// The phrase's two operations, with the phrase standing for the coldkey's seed.
type testingFleetClaimColdkey struct {
	t      *testing.T
	signed int
}

const testingFleetClaimPhrase = "the recovery phrase of the coldkey"

func (self *testingFleetClaimColdkey) Address(phrase string) (string, error) {
	if phrase != testingFleetClaimPhrase {
		return "", errors.New("not a phrase")
	}
	address, err := ss58.Encode(testingFleetPublic(self.t, testingFleetColdkeySeed), SnSs58Prefix)
	return address, err
}

func (self *testingFleetClaimColdkey) SignMessage(phrase string, message string) (string, error) {
	if phrase != testingFleetClaimPhrase {
		return "", errors.New("not a phrase")
	}
	self.signed += 1
	return testingFleetWalletSign(self.t, testingFleetColdkeySeed, message), nil
}

// The chain, as far as the flow can tell, with every write recorded.
type testingFleetClaimChain struct {
	block       uint64
	uid         int64
	registered  bool
	burn        uint64
	fee         uint64
	free        uint64
	convertible uint64
	published   *[32]byte
	readErr     error

	converts    []uint64
	registers   []uint64
	commitments [][32]byte
	writeErr    error
	seeds       [][32]byte
	phrases     []string
}

func (self *testingFleetClaimChain) Block(context.Context) (uint64, error) {
	return self.block, self.readErr
}

func (self *testingFleetClaimChain) HotkeyUid(context.Context, uint16, [32]byte) (int64, bool, error) {
	return self.uid, self.registered, self.readErr
}

func (self *testingFleetClaimChain) RegistrationBurnRao(context.Context, uint16) (uint64, error) {
	return self.burn, self.readErr
}

func (self *testingFleetClaimChain) RegistrationFeeRao(context.Context, uint16, [32]byte, [32]byte, uint64) (uint64, error) {
	return self.fee, self.readErr
}

func (self *testingFleetClaimChain) FreeBalanceRao(context.Context, [32]byte) (uint64, error) {
	return self.free, self.readErr
}

func (self *testingFleetClaimChain) ConvertibleRao(context.Context, uint16, [32]byte) (uint64, error) {
	return self.convertible, self.readErr
}

func (self *testingFleetClaimChain) FleetCommitment(context.Context, uint16, [32]byte) ([32]byte, bool, error) {
	if self.published == nil {
		return [32]byte{}, false, self.readErr
	}
	return *self.published, true, self.readErr
}

func (self *testingFleetClaimChain) ConvertAlpha(_ context.Context, phrase string, _ uint16, taoRao uint64) error {
	self.phrases = append(self.phrases, phrase)
	self.converts = append(self.converts, taoRao)
	return self.writeErr
}

func (self *testingFleetClaimChain) RegisterLimit(_ context.Context, phrase string, _ uint16, _ [32]byte, limitRao uint64) error {
	self.phrases = append(self.phrases, phrase)
	self.registers = append(self.registers, limitRao)
	return self.writeErr
}

func (self *testingFleetClaimChain) SetFleetCommitment(_ context.Context, seed [32]byte, _ uint16, hash [32]byte) error {
	self.seeds = append(self.seeds, seed)
	self.commitments = append(self.commitments, hash)
	return self.writeErr
}

func (self *testingFleetClaimChain) wrote() bool {
	return 0 < len(self.converts)+len(self.registers)+len(self.commitments)
}

type testingFleetClaim struct {
	claim   *SnFleetClaim
	server  *testingFleetServer
	store   *testingFleetClaimStore
	chain   *testingFleetClaimChain
	coldkey *testingFleetClaimColdkey
}

// A claim from the top: binding open, a consent, a phrase on this device and enough
// TAO, in the register window, with nothing done yet.
func newTestingFleetClaim(t *testing.T) *testingFleetClaim {
	t.Helper()
	coldkey, err := ss58.Encode(testingFleetPublic(t, testingFleetColdkeySeed), SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &testingFleetClaim{
		server: &testingFleetServer{status: &snFleetClaimStatus{
			BindingOpen:         true,
			ClaimPrompt:         true,
			ConsentColdkey:      coldkey,
			ChainId:             964,
			Netuid:              25,
			Coordinator:         testingFleetCoordinatorHex(),
			CurrentEpoch:        4,
			RegisterAfterBlock:  1000,
			RegisterBeforeBlock: 2000,
		}},
		store:   &testingFleetClaimStore{phrase: testingFleetClaimPhrase},
		chain:   &testingFleetClaimChain{block: 1500, burn: 500_000, fee: 2_000_000, free: 10_000_000},
		coldkey: &testingFleetClaimColdkey{t: t},
	}
	fixture.claim = newSnFleetClaim(fixture.server, fixture.store, fixture.chain, fixture.coldkey)
	fixture.claim.random = func(out []byte) error {
		copy(out, testingFleetHotkeySeed[:])
		return nil
	}
	return fixture
}

// As a device with its hotkey saved, authorized and registered.
func (self *testingFleetClaim) authorized(t *testing.T) {
	t.Helper()
	self.store.seed = hex.EncodeToString(testingFleetHotkeySeed[:])
	self.server.status.AuthorizationMessage = testingFleetAuthorizationMessage(t, testingFleetColdkeySeed, testingFleetPublic(t, testingFleetHotkeySeed), testingFleetNetworkId)
	self.chain.registered = true
	self.chain.uid = 42
}

func (self *testingFleetClaim) manifested(t *testing.T, generation uint64, mirrored bool, members ...protocol.FleetMember) *protocol.FleetManifest {
	t.Helper()
	manifest := testingFleetManifest(t, testingFleetPublic(t, testingFleetHotkeySeed), generation, members...)
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	self.server.status.Manifest = &snFleetManifestState{Generation: int64(generation), Manifest: string(canonical), Mirrored: mirrored}
	return manifest
}

func testingFleetExpectStage(t *testing.T, state *SnFleetClaimState, stage string) {
	t.Helper()
	if state.Stage != stage {
		t.Fatalf("stage %s (%s: %s), want %s", state.Stage, state.ErrorCode, state.ErrorMessage, stage)
	}
}

// While binding is closed the flow does nothing at all: no key, no signature, no read.
func TestSnFleetClaimStaysDarkWhileBindingIsClosed(t *testing.T) {
	for _, step := range []func(c *SnFleetClaim) *SnFleetClaimState{
		func(c *SnFleetClaim) *SnFleetClaimState { return c.TapSync(context.Background()) },
		func(c *SnFleetClaim) *SnFleetClaimState { return c.AdvanceSync(context.Background()) },
		func(c *SnFleetClaim) *SnFleetClaimState { return c.RefreshSync(context.Background()) },
	} {
		fixture := newTestingFleetClaim(t)
		fixture.server.status.BindingOpen = false
		fixture.chain.readErr = errors.New("the chain was read")
		testingFleetExpectStage(t, step(fixture.claim), SnFleetClaimStageClosed)
		if fixture.store.saves != 0 || len(fixture.server.authorizations) != 0 || fixture.chain.wrote() {
			t.Fatal("something happened while closed")
		}
	}
}

func TestSnFleetClaimNeedsAConsent(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.server.status.ConsentColdkey = ""
	testingFleetExpectStage(t, fixture.claim.TapSync(context.Background()), SnFleetClaimStageNoConsent)
	if fixture.store.saves != 0 {
		t.Fatal("it made a hotkey with no consent")
	}
}

// The one tap on a key on this device: makes the hotkey, signs and sends the
// authorization, registers within the live burn. Nothing else to do but wait.
func TestSnFleetClaimTapWithALocalKey(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	state := fixture.claim.TapSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageRegistering)

	if fixture.store.seed != hex.EncodeToString(testingFleetHotkeySeed[:]) || fixture.store.saves != 1 {
		t.Fatalf("hotkey seed %q, %d saves", fixture.store.seed, fixture.store.saves)
	}
	hotkey := testingFleetPublic(t, testingFleetHotkeySeed)
	if len(fixture.server.authorizations) != 1 || fixture.coldkey.signed != 1 {
		t.Fatalf("authorizations %+v", fixture.server.authorizations)
	}
	sent := fixture.server.authorizations[0]
	signature, err := snFleetParseHex(sent.Signature)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := protocol.VerifyFleetHotkeyAuthorization(sent.Message, signature)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Hotkey != hotkey || authorization.Coldkey != testingFleetPublic(t, testingFleetColdkeySeed) || authorization.NetworkID != [16]byte(testingFleetId(t, testingFleetNetworkId)) || authorization.ChainID != 964 || authorization.Netuid != 25 {
		t.Fatalf("authorization %+v", authorization)
	}
	if len(fixture.chain.registers) != 1 || fixture.chain.registers[0] != 500_000 {
		t.Fatalf("registers %v, want the live burn as the limit", fixture.chain.registers)
	}
	if len(fixture.chain.converts) != 0 || fixture.server.topUps != 0 {
		t.Fatal("it paid for what the balance covered")
	}
	if state.HotkeyAddress == "" || state.ColdkeyAddress == "" {
		t.Fatalf("state %+v", state)
	}
}

// A hotkey is stored and read back before anything is signed with it, and a store
// that can't do that stops the flow.
func TestSnFleetClaimStopsWhenTheHotkeyCannotBeKept(t *testing.T) {
	for name, store := range map[string]*testingFleetClaimStore{
		"a refused save":   {phrase: testingFleetClaimPhrase, refuse: true},
		"a bad read back":  {phrase: testingFleetClaimPhrase, readsBack: true},
		"a failed read":    {phrase: testingFleetClaimPhrase, unreadable: "unavailable"},
		"a malformed seed": {phrase: testingFleetClaimPhrase, seed: "abcd"},
	} {
		fixture := newTestingFleetClaim(t)
		fixture.store = store
		fixture.claim.store = store
		state := fixture.claim.TapSync(context.Background())
		testingFleetExpectStage(t, state, SnFleetClaimStageFailed)
		if state.ErrorCode != SnFleetClaimErrorSecretStore {
			t.Fatalf("%s: %+v", name, state)
		}
		if fixture.coldkey.signed != 0 || len(fixture.server.authorizations) != 0 || fixture.chain.wrote() {
			t.Fatalf("%s: signed or sent something", name)
		}
	}
}

// A wallet network stops at a message, and goes on once the wallet has signed it.
func TestSnFleetClaimWalletAuthorization(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.store.phrase = ""
	state := fixture.claim.TapSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageNeedsWalletAuthorization)
	if len(fixture.server.authorizations) != 0 {
		t.Fatal("sent without a signature")
	}
	parsed, err := protocol.ParseFleetHotkeyAuthorization(state.WalletMessage)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hotkey != testingFleetPublic(t, testingFleetHotkeySeed) {
		t.Fatal("another hotkey")
	}

	// a signature over something else is refused here, before a round trip
	wrong := fixture.claim.SubmitWalletAuthorizationSync(context.Background(), testingFleetWalletSign(t, testingFleetColdkeySeed, "something else"))
	testingFleetExpectStage(t, wrong, SnFleetClaimStageFailed)
	if wrong.ErrorCode != SnFleetClaimErrorWalletSignature || len(fixture.server.authorizations) != 0 {
		t.Fatalf("%+v", wrong)
	}
	stranger := fixture.claim.SubmitWalletAuthorizationSync(context.Background(), testingFleetWalletSign(t, testingFleetOtherColdkeySeed, state.WalletMessage))
	if stranger.ErrorCode != SnFleetClaimErrorWalletSignature || len(fixture.server.authorizations) != 0 {
		t.Fatalf("%+v", stranger)
	}

	// the same device on the next tap keeps its hotkey: the seed is in the store
	next := fixture.claim.SubmitWalletAuthorizationSync(context.Background(), testingFleetWalletSign(t, testingFleetColdkeySeed, state.WalletMessage))
	testingFleetExpectStage(t, next, SnFleetClaimStageNeedsWalletRegistration)
	if len(fixture.server.authorizations) != 1 || fixture.store.saves != 1 {
		t.Fatalf("authorizations %d, saves %d", len(fixture.server.authorizations), fixture.store.saves)
	}
	if next.BurnLimitRao != 500_000 || next.HotkeyAddress == "" {
		t.Fatalf("%+v", next)
	}
	if fixture.chain.wrote() {
		t.Fatal("wrote to the chain for a wallet network")
	}

	// once the wallet registered the hotkey, the rest needs no wallet
	fixture.authorized(t)
	fixture.server.status.Manifest = nil
	testingFleetExpectStage(t, fixture.claim.AdvanceSync(context.Background()), SnFleetClaimStageChoosingDevices)
}

// The server accepts it or it doesn't, and a refusal stops here.
func TestSnFleetClaimAuthorizationRefused(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.server.authorizeResult = &snFleetAuthorizationResult{Error: &SnError{Message: "no"}}
	state := fixture.claim.TapSync(context.Background())
	if state.ErrorCode != SnFleetClaimErrorAuthorizationRefused || fixture.chain.wrote() {
		t.Fatalf("%+v", state)
	}
}

// Registering waits for the window that keeps the new UID from being replaced before
// its binding starts, and for a burn spike to pass. The top-up has the same ceiling.
func TestSnFleetClaimRegisterWaits(t *testing.T) {
	early := newTestingFleetClaim(t)
	early.chain.block = 999
	state := early.claim.TapSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingRegisterWindow)
	if state.RegisterAfterBlock != 1000 || early.chain.wrote() {
		t.Fatalf("%+v", state)
	}
	if len(early.server.authorizations) != 1 {
		t.Fatal("the authorization should not wait")
	}

	late := newTestingFleetClaim(t)
	late.chain.block = 2001
	testingFleetExpectStage(t, late.claim.TapSync(context.Background()), SnFleetClaimStageWaitingRegisterWindow)

	spike := newTestingFleetClaim(t)
	spike.chain.burn = uint64(SnFleetClaimMaxBurnRao) + 1
	spike.server.status.TopUp.Available = true
	spike.chain.free = 0
	testingFleetExpectStage(t, spike.claim.TapSync(context.Background()), SnFleetClaimStageWaitingBurn)
	if spike.chain.wrote() || spike.server.topUps != 0 {
		t.Fatal("it paid into a burn spike")
	}
}

// Converting just what's short comes first, then the one top-up, and without either
// the flow says so. Each stops and resumes on the next pass.
func TestSnFleetClaimFundsTheBurn(t *testing.T) {
	converts := newTestingFleetClaim(t)
	converts.chain.free = 500_000
	converts.chain.convertible = 5_000_000
	converts.server.status.TopUp.Available = true
	testingFleetExpectStage(t, converts.claim.TapSync(context.Background()), SnFleetClaimStageConverting)
	if len(converts.chain.converts) != 1 || converts.chain.converts[0] != 2_000_000 || converts.server.topUps != 0 || len(converts.chain.registers) != 0 {
		t.Fatalf("converts %v, top-ups %d", converts.chain.converts, converts.server.topUps)
	}
	if converts.chain.phrases[0] != testingFleetClaimPhrase {
		t.Fatal("phrase")
	}

	tops := newTestingFleetClaim(t)
	tops.chain.free = 0
	tops.chain.convertible = 100
	tops.server.status.TopUp.Available = true
	state := tops.claim.TapSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingTopUp)
	if tops.server.topUps != 1 || !state.TopUpUsed || tops.chain.wrote() {
		t.Fatalf("%+v", state)
	}

	// sent and not landed: it waits, and doesn't ask again
	tops.server.status.TopUp = snFleetTopUp{Used: true, Landed: false}
	testingFleetExpectStage(t, tops.claim.TapSync(context.Background()), SnFleetClaimStageWaitingTopUp)
	if tops.server.topUps != 1 {
		t.Fatal("a second top-up")
	}

	// landed and the balance covers it now
	tops.server.status.TopUp = snFleetTopUp{Used: true, Landed: true}
	tops.chain.free = 2_500_000
	testingFleetExpectStage(t, tops.claim.TapSync(context.Background()), SnFleetClaimStageRegistering)

	none := newTestingFleetClaim(t)
	none.chain.free = 0
	testingFleetExpectStage(t, none.claim.TapSync(context.Background()), SnFleetClaimStageNeedsFunds)
	if none.server.topUps != 0 || none.chain.wrote() {
		t.Fatal("it spent something")
	}

	refused := newTestingFleetClaim(t)
	refused.chain.free = 0
	refused.server.status.TopUp.Available = true
	refused.server.topUpResult = &snFleetTopUpResult{Error: &SnError{Message: "no funded key"}}
	state = refused.claim.TapSync(context.Background())
	if state.ErrorCode != SnFleetClaimErrorTopUpRefused {
		t.Fatalf("%+v", state)
	}

	// a wallet network can have a top-up too
	wallet := newTestingFleetClaim(t)
	wallet.authorized(t)
	wallet.chain.registered = false
	wallet.store.phrase = ""
	wallet.chain.free = 0
	wallet.chain.convertible = 5_000_000
	wallet.server.status.TopUp.Available = true
	testingFleetExpectStage(t, wallet.claim.TapSync(context.Background()), SnFleetClaimStageWaitingTopUp)
	if len(wallet.chain.converts) != 0 {
		t.Fatal("a device with no phrase converted")
	}
}

// A failed read never leads to a registration.
func TestSnFleetClaimDoesNotSpendAfterAFailedRead(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.authorized(t)
	fixture.chain.registered = false
	fixture.chain.readErr = errors.New("the rpc is down")
	state := fixture.claim.TapSync(context.Background())
	if state.ErrorCode != SnFleetClaimErrorChain || fixture.chain.wrote() {
		t.Fatalf("%+v", state)
	}
}

// Refresh reads, and Advance spends only for a first claim someone already tapped.
// A UID lost later takes a new tap.
func TestSnFleetClaimSpendingTakesATap(t *testing.T) {
	fresh := newTestingFleetClaim(t)
	testingFleetExpectStage(t, fresh.claim.RefreshSync(context.Background()), SnFleetClaimStageNeedsTap)
	testingFleetExpectStage(t, fresh.claim.AdvanceSync(context.Background()), SnFleetClaimStageNeedsTap)
	if fresh.store.saves != 0 || len(fresh.server.authorizations) != 0 || fresh.chain.wrote() {
		t.Fatal("no tap, and something happened")
	}

	// tapped and authorized, waiting for the window: the same tap finishes it
	tapped := newTestingFleetClaim(t)
	tapped.authorized(t)
	tapped.chain.registered = false
	testingFleetExpectStage(t, tapped.claim.RefreshSync(context.Background()), SnFleetClaimStageNeedsTap)
	if tapped.chain.wrote() {
		t.Fatal("refresh wrote")
	}
	testingFleetExpectStage(t, tapped.claim.AdvanceSync(context.Background()), SnFleetClaimStageRegistering)
	if len(tapped.chain.registers) != 1 {
		t.Fatal("the tapped claim didn't finish")
	}

	// the UID was lost after devices were bound
	lost := newTestingFleetClaim(t)
	lost.authorized(t)
	lost.chain.registered = false
	lost.manifested(t, 3, true)
	state := lost.claim.AdvanceSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageNeedsTap)
	if lost.chain.wrote() || lost.server.topUps != 0 {
		t.Fatal("it spent again with no tap")
	}
	testingFleetExpectStage(t, lost.claim.TapSync(context.Background()), SnFleetClaimStageRegistering)
}

// Refresh on a running claim says where it stands and changes nothing.
func TestSnFleetClaimRefreshWritesNothing(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.authorized(t)
	manifest := fixture.manifested(t, 3, false)
	binding, err := manifest.Binding(manifest.Members[0], 5, 16)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
	state := fixture.claim.RefreshSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingMirror)
	if fixture.chain.wrote() || len(fixture.server.hotkeySigned) != 0 {
		t.Fatal("refresh wrote or signed")
	}
}

// With a manifest out, a pass with no tap publishes the commitment it computed itself
// and signs each binding, which is all renewing takes.
func TestSnFleetClaimAdvancePublishesAndSigns(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.authorized(t)
	manifest := fixture.manifested(t, 3, false)
	binding, err := manifest.Binding(manifest.Members[0], 5, 16)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}

	state := fixture.claim.AdvanceSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingMirror)

	hash, err := manifest.CommitmentHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.chain.commitments) != 1 || fixture.chain.commitments[0] != hash || fixture.chain.seeds[0] != testingFleetHotkeySeed {
		t.Fatalf("commitments %x", fixture.chain.commitments)
	}
	if len(fixture.chain.registers)+len(fixture.chain.converts) != 0 || fixture.coldkey.signed != 0 || fixture.server.topUps != 0 {
		t.Fatal("renewing used the coldkey or spent something")
	}
	if len(fixture.server.hotkeySigned) != 1 || len(fixture.server.hotkeySigned[0].Signatures) != 1 {
		t.Fatalf("signed %+v", fixture.server.hotkeySigned)
	}
	sent := fixture.server.hotkeySigned[0].Signatures[0]
	digest, err := binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := snFleetParseHex(sent.HotkeySignature)
	if err != nil || sent.Digest != snFleetHex(digest[:]) || !binding.VerifyHotkey(signature) {
		t.Fatalf("hotkey signature: %v", err)
	}

	// already published: not again
	fixture.chain.published = &hash
	fixture.chain.commitments = nil
	if state := fixture.claim.AdvanceSync(context.Background()); state.Stage != SnFleetClaimStageWaitingMirror || len(fixture.chain.commitments) != 0 {
		t.Fatalf("%+v %d", state, len(fixture.chain.commitments))
	}

	// mirrored: no chain read or publish, whatever it says
	fixture.server.status.Manifest.Mirrored = true
	fixture.server.status.Counts = snFleetDeviceCounts{Chosen: 1}
	fixture.chain.published = nil
	state = fixture.claim.AdvanceSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageWaitingDevices)
	if len(fixture.chain.commitments) != 0 || state.Chosen != 1 {
		t.Fatalf("%+v", state)
	}

	fixture.server.status.Counts = snFleetDeviceCounts{Chosen: 1, Ready: 1, Bound: 1, StillToUpdate: 4}
	fixture.server.status.HotkeyWork = nil
	state = fixture.claim.AdvanceSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageBound)
	if state.StillToUpdate != 4 || state.Ready != 1 || state.RetryAfterSeconds != 0 {
		t.Fatalf("%+v", state)
	}
}

// What the server sends is checked, and one bad binding means none get signed. The
// server can't get the hotkey to sign or publish anything else.
func TestSnFleetClaimRefusesWhatItCannotSign(t *testing.T) {
	hotkey := testingFleetPublic(t, testingFleetHotkeySeed)
	members := func(count int) []protocol.FleetMember {
		out := []protocol.FleetMember{}
		for i := 0; i < count; i += 1 {
			out = append(out, protocol.FleetMember{ClientID: [16]byte{1, byte(i)}, ClientKey: [32]byte{1, byte(i)}})
		}
		return out
	}
	cases := map[string]struct {
		code  string
		setup func(t *testing.T, fixture *testingFleetClaim)
	}{
		"a manifest of another hotkey": {SnFleetClaimErrorBadManifest, func(t *testing.T, fixture *testingFleetClaim) {
			canonical, err := testingFleetManifest(t, testingFleetPublic(t, testingFleetOtherHotkeySeed), 3).Canonical()
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.Manifest = &snFleetManifestState{Generation: 3, Manifest: string(canonical)}
		}},
		"a manifest of another deployment": {SnFleetClaimErrorBadManifest, func(t *testing.T, fixture *testingFleetClaim) {
			manifest := testingFleetManifest(t, hotkey, 3)
			manifest.ChainID = 945
			canonical, err := manifest.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.Manifest = &snFleetManifestState{Generation: 3, Manifest: string(canonical)}
		}},
		"a manifest over the limit": {SnFleetClaimErrorBadManifest, func(t *testing.T, fixture *testingFleetClaim) {
			fixture.manifested(t, 3, false, members(protocol.FleetRelayDeviceLimit+1)...)
		}},
		"manifest bytes that aren't canonical": {SnFleetClaimErrorBadManifest, func(t *testing.T, fixture *testingFleetClaim) {
			fixture.manifested(t, 3, false)
			fixture.server.status.Manifest.Manifest += " "
		}},
		"a binding of a device not in the manifest": {SnFleetClaimErrorBadBinding, func(t *testing.T, fixture *testingFleetClaim) {
			fixture.manifested(t, 3, false)
			outside := testingFleetManifest(t, hotkey, 3, members(1)...)
			binding, err := outside.Binding(outside.Members[0], 5, 16)
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
		}},
		"a binding of another generation": {SnFleetClaimErrorBadBinding, func(t *testing.T, fixture *testingFleetClaim) {
			fixture.manifested(t, 3, false)
			other := testingFleetManifest(t, hotkey, 4)
			binding, err := other.Binding(other.Members[0], 5, 16)
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
		}},
		"a binding that started already": {SnFleetClaimErrorBadBinding, func(t *testing.T, fixture *testingFleetClaim) {
			manifest := fixture.manifested(t, 3, false)
			binding, err := manifest.Binding(manifest.Members[0], 4, 15)
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
		}},
		"a binding longer than 12 epochs": {SnFleetClaimErrorBadBinding, func(t *testing.T, fixture *testingFleetClaim) {
			manifest := fixture.manifested(t, 3, false)
			binding, err := manifest.Binding(manifest.Members[0], 5, 17)
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, binding)}}
		}},
		"one bad binding among good ones": {SnFleetClaimErrorBadBinding, func(t *testing.T, fixture *testingFleetClaim) {
			manifest := fixture.manifested(t, 3, false)
			good, err := manifest.Binding(manifest.Members[0], 5, 16)
			if err != nil {
				t.Fatal(err)
			}
			fixture.server.status.HotkeyWork = []*snFleetHotkeyWork{{BindingJson: testingFleetBindingJson(t, good)}, {BindingJson: "{}"}}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newTestingFleetClaim(t)
			fixture.authorized(t)
			c.setup(t, fixture)
			state := fixture.claim.AdvanceSync(context.Background())
			testingFleetExpectStage(t, state, SnFleetClaimStageFailed)
			if state.ErrorCode != c.code {
				t.Fatalf("%+v", state)
			}
			if len(fixture.server.hotkeySigned) != 0 {
				t.Fatal("signatures went out")
			}
			if c.code == SnFleetClaimErrorBadManifest && len(fixture.chain.commitments) != 0 {
				t.Fatal("published a bad manifest")
			}
		})
	}
}

// The hotkey is on another device of the network: this one reads the state and can't
// act, and a tap doesn't replace it.
func TestSnFleetClaimHotkeyElsewhere(t *testing.T) {
	hotkeyAbsent := newTestingFleetClaim(t)
	hotkeyAbsent.authorized(t)
	hotkeyAbsent.store.seed = ""
	testingFleetExpectStage(t, hotkeyAbsent.claim.TapSync(context.Background()), SnFleetClaimStageHotkeyElsewhere)
	if hotkeyAbsent.store.saves != 0 {
		t.Fatal("a second hotkey")
	}

	other := newTestingFleetClaim(t)
	other.authorized(t)
	other.store.seed = hex.EncodeToString(testingFleetOtherHotkeySeed[:])
	testingFleetExpectStage(t, other.claim.TapSync(context.Background()), SnFleetClaimStageHotkeyElsewhere)
	if len(other.server.authorizations) != 0 || other.chain.wrote() {
		t.Fatal("it replaced the fleet's hotkey")
	}
}

// A payout wallet replaced after the authorization needs a new one from the new
// coldkey: the old one no longer speaks for this network.
func TestSnFleetClaimReauthorizesAfterTheColdkeyChanged(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.authorized(t)
	fixture.server.status.AuthorizationMessage = testingFleetAuthorizationMessage(t, testingFleetOtherColdkeySeed, testingFleetPublic(t, testingFleetHotkeySeed), testingFleetNetworkId)
	testingFleetExpectStage(t, fixture.claim.AdvanceSync(context.Background()), SnFleetClaimStageNeedsTap)
	if len(fixture.server.authorizations) != 0 {
		t.Fatal("it authorized with no tap")
	}
	fixture.claim.TapSync(context.Background())
	if len(fixture.server.authorizations) != 1 {
		t.Fatalf("authorizations %d", len(fixture.server.authorizations))
	}
}

// A phrase that isn't the payout wallet's can't sign for it.
func TestSnFleetClaimIgnoresAPhraseOfAnotherColdkey(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	coldkey, err := ss58.Encode(testingFleetPublic(t, testingFleetOtherColdkeySeed), SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server.status.ConsentColdkey = coldkey
	testingFleetExpectStage(t, fixture.claim.TapSync(context.Background()), SnFleetClaimStageNeedsWalletAuthorization)
	if fixture.coldkey.signed != 0 {
		t.Fatal("it signed with another key's phrase")
	}
}

// A secret never reaches what the flow reports.
func TestSnFleetClaimStateHoldsNoSecret(t *testing.T) {
	fixture := newTestingFleetClaim(t)
	fixture.chain.writeErr = errors.New("write failed: " + testingFleetClaimPhrase)
	state := fixture.claim.TapSync(context.Background())
	testingFleetExpectStage(t, state, SnFleetClaimStageFailed)
	if state.ErrorMessage == "" {
		t.Fatal("no message")
	}
	for _, secret := range []string{testingFleetClaimPhrase, fixture.store.seed} {
		if secret != "" && strings.Contains(state.ErrorMessage, secret) {
			t.Fatalf("a secret in %q", state.ErrorMessage)
		}
	}
}
